package cli

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
)

func runCheck(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	return runCheckWithContext(context.Background(), command, settings, stdin, stdout, stderr)
}

func runCheckWithContext(ctx context.Context, command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	return runCheckWithPrompt(ctx, command, settings, stdin, stdout, stderr, nil)
}

// runCheckWithPrompt is the shared check use case with an optional prompt
// callback for an interactive adapter. Command-line checks remain
// non-interactive; the Polish terminal can provide the same password and MFA
// flow as account login without exposing secret input to this package.
func runCheckWithPrompt(ctx context.Context, command string, settings options, stdin *os.File, stdout, stderr io.Writer, prompt func(context.Context, string) (string, error)) int {
	jsonOutput := settings.output == "json"
	if len(settings.positionals) > 1 || (settings.profileID != "" && len(settings.positionals) > 0 && settings.positionals[0] != settings.profileID) {
		writeError(stderr, command, "invalid_arguments", "use either --profile or one positional profile id", jsonOutput)
		return 2
	}
	id := settings.profileID
	if id == "" && len(settings.positionals) == 1 {
		id = settings.positionals[0]
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	// Automatic history retention: old terminal records are removed on every
	// durable check according to the saved policy. Dry runs never touch
	// durable state, so they skip pruning as well.
	if !settings.dry {
		pruneHistoryBestEffort(storage)
	}
	profile, err := storage.GetProfile(id)
	if err != nil {
		return reportProfileError(stderr, command, err, jsonOutput)
	}
	// Maintenance: finalize rows stuck after an interrupted final claim.
	// Stuck rows cannot schedule work by themselves (see watch
	// launchIteration), so a manual check finalizes them too — including
	// for disabled profiles, whose exhausted budget is terminal regardless
	// of enabled state. Dry runs never touch durable state.
	if !settings.dry {
		if _, err := storage.ReapExpiredMaxAttemptClaims(profile.ID, time.Now().UTC()); err != nil {
			return reportStoreError(stderr, command, err, jsonOutput)
		}
	}
	if !settings.dry && !profile.Enabled {
		return reportCheckError(stderr, command, fmt.Errorf("%w: %s", store.ErrProfileDisabled, profile.ID), jsonOutput)
	}
	account, err := storage.GetAccount(profile.AccountID)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	backend := sessionStoreFor(settings)
	var saved *medicover.SessionState
	if state, loadErr := backend.Load(account.ID); loadErr == nil {
		saved = state
	} else if !errors.Is(loadErr, session.ErrNotFound) && !isSessionCorrupt(loadErr) {
		writeError(stderr, command, "temporary_failure", "session storage is temporarily unavailable", jsonOutput)
		return 4
	}
	client := medicoverClientFor(settings)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	auth, authErr := client.Authenticate(ctx, medicover.AuthRequest{Session: saved})
	if authErr != nil && medicover.IsAuthRequired(authErr) {
		var password string
		var resolveErr error
		if prompt != nil && account.PasswordSource == store.PasswordSourcePrompt {
			password, resolveErr = prompt(ctx, "password")
		} else {
			password, resolveErr = secrets.Resolve(account.PasswordSource, account.PasswordRef, account.ID, stdin, stderr, true)
		}
		if resolveErr != nil {
			if errors.Is(resolveErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeError(stderr, command, "timeout", "authentication timed out", jsonOutput)
				return 4
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return reportContextError(stderr, command, ctxErr, jsonOutput)
			}
			return reportSecretError(stderr, command, resolveErr, jsonOutput)
		}
		passwordSecret := medicover.Secret(password)
		password = ""
		var requestMFA func(context.Context) (medicover.Secret, error)
		if prompt != nil {
			requestMFA = func(promptContext context.Context) (medicover.Secret, error) {
				value, promptErr := prompt(promptContext, "mfa")
				return medicover.Secret(value), promptErr
			}
		}
		auth, authErr = client.Authenticate(ctx, medicover.AuthRequest{
			Username:   medicover.Secret(account.Username),
			Password:   passwordSecret,
			RequestMFA: requestMFA,
			Session:    saved,
		})
	}
	if authErr != nil {
		if !settings.dry {
			now := time.Now().UTC()
			if recordErr := recordAccountFailure(storage, account, authErr, now); recordErr != nil {
				return reportStoreError(stderr, command, recordErr, jsonOutput)
			}
			if _, processErr := processIncidentDeliveries(ctx, storage, settings, stdin, stderr); processErr != nil {
				return reportStoreError(stderr, command, processErr, jsonOutput)
			}
		}
		return reportMedicoverError(stderr, command, authErr, jsonOutput)
	}
	if !settings.dry {
		if err := backend.Save(account.ID, auth.Session); err != nil {
			writeError(stderr, command, "temporary_failure", "cannot save session state", jsonOutput)
			return 4
		}
	}
	if settings.dry {
		result, err := client.Search(ctx, auth.AccessToken, searchCriteriaForProfile(profile))
		if err != nil {
			return reportMedicoverError(stderr, command, err, jsonOutput)
		}
		data := map[string]any{"account": account.ID, "profile": profile.ID, "dry": true, "complete": true, "slots": result.Slots, "slot_count": len(result.Slots), "pages": result.Pages}
		if jsonOutput {
			writeResult(stdout, command, data)
			return 0
		}
		fmt.Fprintf(stdout, "Dry check for profile %s (account %s) found %d available slots.\n", profile.ID, account.ID, len(result.Slots))
		for _, slot := range result.Slots {
			fmt.Fprintf(stdout, "%s  %s  %s  %s\n", slot.Time, slot.Doctor, slot.Clinic, slot.Identity)
		}
		return 0
	}
	result, err := monitoring.Check(ctx, storage, profile, account, client, auth.AccessToken, time.Now().UTC())
	if err != nil {
		now := time.Now().UTC()
		if medicover.IsAuthRequired(err) {
			// A search-phase authentication failure means the account
			// session is bad. It creates one account-level problem, not
			// one problem per profile, so only the account incident is
			// recorded here.
			if recordErr := recordAccountFailure(storage, account, err, now); recordErr != nil {
				return reportStoreError(stderr, command, recordErr, jsonOutput)
			}
		} else if recordErr := recordProfileFailure(storage, profile, err, now); recordErr != nil {
			return reportStoreError(stderr, command, recordErr, jsonOutput)
		}
		if _, processErr := processIncidentDeliveries(ctx, storage, settings, stdin, stderr); processErr != nil {
			return reportStoreError(stderr, command, processErr, jsonOutput)
		}
		return reportCheckError(stderr, command, err, jsonOutput)
	}
	// One complete successful run ends operational incidents. Failed or
	// partial searches never reach here, so they never end episodes (only
	// ReconcileObservationRun changes episodes) and never resolve incidents.
	if err := resolveIncidentsOnSuccess(storage, profile, time.Now().UTC()); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	// Durable Telegram notifications run after the complete observation run.
	// Observation success wins: retryable Telegram failures stay pending for
	// later check and watch cycles and never change the check exit code.
	deliveries, err := deliverAfterCheck(ctx, storage, profile, result, settings, stdin, stderr)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	if err := trackDestinationIncidents(storage, profile, deliveries, time.Now().UTC()); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	incidents, err := processIncidentDeliveries(ctx, storage, settings, stdin, stderr)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	episodes, err := storage.ListAvailabilityEpisodes(profile.ID)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	activeEpisodeCount := 0
	for _, episode := range episodes {
		if episode.Active {
			activeEpisodeCount++
		}
	}
	data := map[string]any{
		"account":              account.ID,
		"profile":              profile.ID,
		"dry":                  false,
		"complete":             true,
		"run":                  result.Reconciliation.Run,
		"slots":                result.Search.Slots,
		"slot_count":           len(result.Search.Slots),
		"pages":                result.Search.Pages,
		"new_episodes":         result.Reconciliation.NewEpisodes,
		"ended_episodes":       result.Reconciliation.EndedEpisodes,
		"newly_available":      len(result.Reconciliation.NewEpisodes),
		"ended":                len(result.Reconciliation.EndedEpisodes),
		"active_episode_count": activeEpisodeCount,
		"deliveries":           deliveries.Deliveries,
		"delivered":            deliveries.Delivered,
		"delivery_failed":      deliveries.Failed,
		"delivery_pending":     deliveries.StillRetry,
		"delivery_cancelled":   deliveries.Cancelled,
		"incident_deliveries":  incidents.Deliveries,
		"incident_delivered":   incidents.Delivered,
		"incident_failed":      incidents.Failed,
		"incident_pending":     incidents.StillRetry,
	}
	if jsonOutput {
		writeResult(stdout, command, data)
		return 0
	}
	fmt.Fprintf(stdout, "Check for profile %s (account %s) found %d available slots; %d newly available, %d ended.\n", profile.ID, account.ID, len(result.Search.Slots), len(result.Reconciliation.NewEpisodes), len(result.Reconciliation.EndedEpisodes))
	for _, slot := range result.Search.Slots {
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", slot.Time, slot.Doctor, slot.Clinic, slot.Identity)
	}
	if deliveries.Attempted > 0 || deliveries.Cancelled > 0 {
		fmt.Fprintf(stdout, "Telegram: %d delivered, %d pending, %d failed, %d cancelled.\n", deliveries.Delivered, deliveries.StillRetry, deliveries.Failed, deliveries.Cancelled)
	}
	if incidents.Attempted > 0 {
		fmt.Fprintf(stdout, "Incidents: %d delivered, %d pending, %d failed.\n", incidents.Delivered, incidents.StillRetry, incidents.Failed)
	}
	return 0
}

func searchCriteriaForProfile(profile store.Profile) medicover.SearchCriteria {
	return medicover.SearchCriteria{
		RegionIDs:    profile.RegionIDs,
		SpecialtyIDs: profile.SpecialtyIDs,
		ClinicIDs:    profile.ClinicIDs,
		DoctorIDs:    profile.DoctorIDs,
		LanguageIDs:  profile.LanguageIDs,
		VisitType:    profile.VisitType,
		SearchType:   profile.SearchType,
		StartDate:    profile.StartDate,
		EndDate:      profile.EndDate,
	}
}

func reportCheckError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	switch {
	case errors.Is(err, store.ErrProfileNotFound):
		return reportProfileError(stderr, command, err, jsonOutput)
	case errors.Is(err, store.ErrProfileDisabled):
		writeError(stderr, command, "profile_disabled", err.Error(), jsonOutput)
		return 2
	case errors.Is(err, store.ErrObservationRunActive):
		writeError(stderr, command, "run_active", err.Error(), jsonOutput)
		return 4
	case errors.Is(err, store.ErrObservationRunConflicting):
		writeError(stderr, command, "conflicting_result", err.Error(), jsonOutput)
		return 6
	case errors.Is(err, store.ErrObservationRunStale):
		writeError(stderr, command, "stale_result", err.Error(), jsonOutput)
		return 6
	case errors.Is(err, store.ErrObservationRunInvalid), errors.Is(err, store.ErrObservationRunNotFound):
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	default:
		return reportMedicoverError(stderr, command, err, jsonOutput)
	}
}
