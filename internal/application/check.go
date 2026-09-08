package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/monitoring"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

// CheckRequest describes one manual profile check. Prompt is optional: the
// command adapter leaves it nil, while the Polish TUI supplies its hidden
// password and MFA input callback.
type CheckRequest struct {
	ProfileID string
	Dry       bool
	Stdin     *os.File
	Prompt    func(context.Context, string) (string, error)
}

// CheckResult contains safe result data for command and terminal adapters.
type CheckResult struct {
	Profile            store.Profile
	Account            store.Account
	Search             medicover.SearchResult
	Reconciliation     store.ObservationReconciliation
	Deliveries         monitoring.DeliverySummary
	Incidents          monitoring.IncidentSummary
	ActiveEpisodeCount int
	Dry                bool
}

// Check runs one dry or durable profile check through the application
// boundary. Dry checks do not prune history, save session state, record
// incidents, create observation runs, or deliver Telegram messages.
func (a *Application) Check(ctx context.Context, request CheckRequest) (CheckResult, error) {
	ctx = contextOrBackground(ctx)
	result := CheckResult{Dry: request.Dry}
	profileID := strings.TrimSpace(request.ProfileID)
	if profileID == "" {
		return result, &OperationError{Code: "invalid_arguments", Message: "profile id is required"}
	}
	if err := ctx.Err(); err != nil {
		return result, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	storage, err := openStore(a.config.Database)
	if err != nil {
		return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	defer storage.Close()
	if !request.Dry {
		_, _ = storage.PruneHistory(time.Now().UTC())
	}
	profile, err := storage.GetProfile(profileID)
	if err != nil {
		return result, profileOperationError(err)
	}
	result.Profile = profile
	if !request.Dry {
		if _, err := storage.ReapExpiredMaxAttemptClaims(profile.ID, time.Now().UTC()); err != nil {
			return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
		}
		if !profile.Enabled {
			return result, &OperationError{Code: "profile_disabled", Message: fmt.Sprintf("%s: %s", store.ErrProfileDisabled, profile.ID), Cause: store.ErrProfileDisabled}
		}
	}
	account, err := storage.GetAccount(profile.AccountID)
	if err != nil {
		return result, &OperationError{Code: "account_not_found", Message: err.Error(), Cause: err}
	}
	result.Account = account
	backend := sessionStore(a.config.SessionDir)
	var saved *medicover.SessionState
	if state, loadErr := backend.Load(account.ID); loadErr == nil {
		saved = state
	} else if !errors.Is(loadErr, session.ErrNotFound) && !errors.Is(loadErr, session.ErrCorrupt) {
		return result, &OperationError{Code: "temporary_failure", Message: "session storage is temporarily unavailable", Cause: loadErr}
	}
	client := medicoverClient(a.config.MedicoverBaseURL)
	checkContext, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	auth, authErr := client.Authenticate(checkContext, medicover.AuthRequest{Session: saved})
	if authErr != nil && medicover.IsAuthRequired(authErr) {
		password, resolveErr := resolvePassword(checkContext, request, account)
		if resolveErr != nil {
			return result, resolveErr
		}
		passwordSecret := medicover.Secret(password)
		password = ""
		var requestMFA func(context.Context) (medicover.Secret, error)
		if request.Prompt != nil {
			requestMFA = func(promptContext context.Context) (medicover.Secret, error) {
				value, promptErr := request.Prompt(promptContext, "mfa")
				return medicover.Secret(value), promptErr
			}
		}
		auth, authErr = client.Authenticate(checkContext, medicover.AuthRequest{
			Username: medicover.Secret(account.Username), Password: passwordSecret,
			RequestMFA: requestMFA, Session: saved,
		})
	}
	if authErr != nil {
		if !request.Dry {
			if _, recordErr := monitoring.HandleAuthenticationFailure(storage, account.ID, authErr, time.Now().UTC()); recordErr != nil {
				return result, &OperationError{Code: "database_error", Message: recordErr.Error(), Cause: recordErr}
			}
			if _, processErr := a.processIncidentDeliveries(checkContext, storage, request.Stdin); processErr != nil {
				return result, &OperationError{Code: "database_error", Message: processErr.Error(), Cause: processErr}
			}
		}
		return result, authErr
	}
	if !request.Dry {
		if err := backend.Save(account.ID, auth.Session); err != nil {
			return result, &OperationError{Code: "temporary_failure", Message: "cannot save session state", Cause: err}
		}
	}
	if request.Dry {
		search, searchErr := client.Search(checkContext, auth.AccessToken, searchCriteria(profile))
		if searchErr != nil {
			return result, searchErr
		}
		result.Search = search
		return result, nil
	}
	checked, err := monitoring.Check(checkContext, storage, profile, account, client, auth.AccessToken, time.Now().UTC())
	if err != nil {
		now := time.Now().UTC()
		if _, recordErr := monitoring.HandleCheckFailure(storage, profile, err, now); recordErr != nil {
			return result, &OperationError{Code: "database_error", Message: recordErr.Error(), Cause: recordErr}
		}
		if _, processErr := a.processIncidentDeliveries(checkContext, storage, request.Stdin); processErr != nil {
			return result, &OperationError{Code: "database_error", Message: processErr.Error(), Cause: processErr}
		}
		return result, err
	}
	if _, err := monitoring.ResolveCheckIncidents(storage, profile, time.Now().UTC()); err != nil {
		return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	deliveries, err := a.deliverAfterCheck(checkContext, storage, profile, checked, request.Stdin)
	if err != nil {
		return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	if _, err := monitoring.ReconcileDestinationIncidents(storage, profile, deliveries, time.Now().UTC()); err != nil {
		return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	incidents, err := a.processIncidentDeliveries(checkContext, storage, request.Stdin)
	if err != nil {
		return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	episodes, err := storage.ListAvailabilityEpisodes(profile.ID)
	if err != nil {
		return result, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	for _, episode := range episodes {
		if episode.Active {
			result.ActiveEpisodeCount++
		}
	}
	result.Search = checked.Search
	result.Reconciliation = checked.Reconciliation
	result.Deliveries = deliveries
	result.Incidents = incidents
	return result, nil
}

func resolvePassword(ctx context.Context, request CheckRequest, account store.Account) (string, error) {
	var password string
	var err error
	if request.Prompt != nil && account.PasswordSource == store.PasswordSourcePrompt {
		password, err = request.Prompt(ctx, "password")
	} else {
		password, err = secrets.Resolve(account.PasswordSource, account.PasswordRef, account.ID, request.Stdin, io.Discard, true)
	}
	if err == nil {
		return password, nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", &OperationError{Code: "timeout", Message: "authentication timed out", Cause: err}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: ctxErr}
	}
	code := "secret_error"
	if errors.Is(err, secrets.ErrMissingInput) {
		code = "missing_input"
	}
	return "", &OperationError{Code: code, Message: err.Error(), Cause: err}
}

func medicoverClient(baseURL string) *medicover.Client {
	config := medicover.Config{}
	if trimmed := strings.TrimSpace(baseURL); trimmed != "" {
		config.Issuer = trimmed
		config.RedirectURI = strings.TrimSuffix(trimmed, "/") + "/signin-oidc"
		config.APIBaseURL = trimmed
	}
	return medicover.NewClient(config)
}

func searchCriteria(profile store.Profile) medicover.SearchCriteria {
	return medicover.SearchCriteria{
		RegionIDs: profile.RegionIDs, SpecialtyIDs: profile.SpecialtyIDs, ClinicIDs: profile.ClinicIDs,
		DoctorIDs: profile.DoctorIDs, LanguageIDs: profile.LanguageIDs, VisitType: profile.VisitType,
		SearchType: profile.SearchType, StartDate: profile.StartDate, EndDate: profile.EndDate,
	}
}

func (a *Application) telegramSender() *telegram.Client {
	return telegram.NewClient(telegram.Config{BaseURL: strings.TrimSpace(a.config.TelegramBaseURL)})
}

func (a *Application) resolveTelegramToken(destination store.Destination, stdin *os.File) (telegram.Secret, error) {
	token, err := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, stdin, io.Discard, true)
	if err != nil {
		return "", err
	}
	secret := telegram.Secret(token)
	token = ""
	return secret, nil
}

func (a *Application) processIncidentDeliveries(ctx context.Context, storage *store.Store, stdin *os.File) (monitoring.IncidentSummary, error) {
	resolve := func(destination store.Destination) (telegram.Secret, error) {
		return a.resolveTelegramToken(destination, stdin)
	}
	deliveryContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return monitoring.ProcessIncidentDeliveries(deliveryContext, storage, a.telegramSender(), resolve, time.Now().UTC())
}

func (a *Application) deliverAfterCheck(ctx context.Context, storage *store.Store, profile store.Profile, result monitoring.CheckResult, stdin *os.File) (monitoring.DeliverySummary, error) {
	resolve := func(destination store.Destination) (telegram.Secret, error) {
		return a.resolveTelegramToken(destination, stdin)
	}
	deliveryContext, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return monitoring.ProcessAvailabilityDeliveries(deliveryContext, storage, profile, result.Reconciliation, a.telegramSender(), resolve, time.Now().UTC())
}
