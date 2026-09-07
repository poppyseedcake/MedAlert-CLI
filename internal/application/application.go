// Package application coordinates profile actions and safe monitoring queries
// used by the terminal adapter. It returns display-safe values only:
// passwords, tokens, session state, and transport details never cross this
// boundary.
package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/monitoring"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

// Config selects the durable application resources used by the application
// service.
type Config struct {
	Database         string
	SessionDir       string
	MedicoverBaseURL string
	TelegramBaseURL  string
}

// Application is the concrete application service. The project has one
// implementation, so callers use this service directly instead of adding a
// general provider interface.
type Application struct {
	config Config
}

// New creates an application service.
func New(config Config) *Application {
	return &Application{config: config}
}

// Row is a safe row for an adapter. Target identifies an application area for
// required-action navigation; it does not expose any storage detail.
type Row struct {
	ID     string
	Label  string
	Detail string
	Target int
}

// ProfileValues contains profile criteria that are safe to display or edit.
// It never contains a password, session, token, or secret reference.
type ProfileValues struct {
	AccountID            string
	RegionIDs            string
	SpecialtyIDs         string
	ClinicIDs            string
	DoctorIDs            string
	LanguageIDs          string
	VisitType            string
	SearchType           string
	StartDate            string
	EndDate              string
	CheckIntervalMinutes string
	Enabled              bool
}

// ProfileRequest describes one profile configuration action from an adapter.
// Clear contains optional field names that the user intentionally emptied.
type ProfileRequest struct {
	Action string
	ID     string
	Values ProfileValues
	Clear  []string
}

// OperationError is a safe application error. Code is stable for adapters;
// Message contains validation detail but never contains a secret.
type OperationError struct {
	Code    string
	Message string
	Cause   error
}

func (e *OperationError) Error() string { return e.Message }

func (e *OperationError) Unwrap() error { return e.Cause }

// ErrorInfo returns the stable code and safe detail for an application error.
func ErrorInfo(err error) (string, string) {
	var operationErr *OperationError
	if errors.As(err, &operationErr) {
		return operationErr.Code, operationErr.Message
	}
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		return medicoverErr.Code, medicoverErr.Message
	}
	var telegramErr *telegram.Error
	if errors.As(err, &telegramErr) {
		return telegramErr.Code, telegramErr.Message
	}
	switch {
	case errors.Is(err, store.ErrProfileNotFound):
		return "profile_not_found", err.Error()
	case errors.Is(err, store.ErrAccountNotFound):
		return "account_not_found", err.Error()
	case errors.Is(err, store.ErrDestinationExists):
		return "destination_exists", err.Error()
	case errors.Is(err, store.ErrDestinationNotFound):
		return "destination_not_found", err.Error()
	case errors.Is(err, store.ErrDestinationInvalid), errors.Is(err, store.ErrProfileInvalid):
		return "invalid_arguments", err.Error()
	case errors.Is(err, store.ErrProfileDisabled):
		return "profile_disabled", err.Error()
	case errors.Is(err, store.ErrObservationRunActive):
		return "run_active", err.Error()
	case errors.Is(err, store.ErrObservationRunConflicting):
		return "conflicting_result", err.Error()
	case errors.Is(err, store.ErrObservationRunStale):
		return "stale_result", err.Error()
	case errors.Is(err, store.ErrObservationRunInvalid), errors.Is(err, store.ErrObservationRunNotFound):
		return "invalid_arguments", err.Error()
	case errors.Is(err, secrets.ErrMissingInput):
		return "missing_input", err.Error()
	case errors.Is(err, secrets.ErrSecretNotFound), errors.Is(err, secrets.ErrSecretUnsafe):
		return "secret_error", "cannot access the required secret"
	case errors.Is(err, context.Canceled):
		return "cancelled", "operation was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "operation timed out"
	}
	return "database_error", err.Error()
}

// Profile applies a profile configuration action through the application
// boundary. Telegram links are managed by Telegram actions and are not part
// of the profile form in the Polish TUI.
func (a *Application) Profile(ctx context.Context, request ProfileRequest) (store.Profile, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return store.Profile{}, &OperationError{Code: "cancelled", Message: err.Error()}
	}
	storage, err := openStore(a.config.Database)
	if err != nil {
		return store.Profile{}, &OperationError{Code: "database_error", Message: err.Error()}
	}
	defer storage.Close()
	id := strings.TrimSpace(request.ID)
	switch request.Action {
	case "create":
		profile, buildErr := profileFromValues(id, request.Values)
		if buildErr != nil {
			return store.Profile{}, &OperationError{Code: "invalid_arguments", Message: buildErr.Error()}
		}
		profile.Enabled = request.Values.Enabled
		created, createErr := storage.CreateProfileContext(ctx, profile)
		if createErr != nil {
			return store.Profile{}, profileOperationError(createErr)
		}
		return created, nil
	case "edit":
		update, hasChange, updateErr := profileUpdate(request.Values, request.Clear)
		if updateErr != nil {
			return store.Profile{}, &OperationError{Code: "invalid_arguments", Message: updateErr.Error()}
		}
		if !hasChange {
			return store.Profile{}, &OperationError{Code: "invalid_arguments", Message: "no profile changes requested"}
		}
		updated, updateErr := storage.UpdateProfileContext(ctx, id, update)
		if updateErr != nil {
			return store.Profile{}, profileOperationError(updateErr)
		}
		return updated, nil
	case "enable", "disable":
		updated, setErr := storage.SetProfileEnabledContext(ctx, id, request.Action == "enable")
		if setErr != nil {
			return store.Profile{}, profileOperationError(setErr)
		}
		return updated, nil
	case "delete":
		current, getErr := storage.GetProfileContext(ctx, id)
		if getErr != nil {
			return store.Profile{}, profileOperationError(getErr)
		}
		if deleteErr := storage.DeleteProfileContext(ctx, id); deleteErr != nil {
			return store.Profile{}, profileOperationError(deleteErr)
		}
		return current, nil
	default:
		return store.Profile{}, &OperationError{Code: "invalid_arguments", Message: "unsupported profile action"}
	}
}

// Snapshot is the display-safe application state used by the Polish TUI.
type Snapshot struct {
	Accounts          []Row
	Profiles          []Row
	Destinations      []Row
	Monitoring        []Row
	Runs              []Row
	History           []Row
	Actions           []Row
	ProfileValues     map[string]ProfileValues
	DestinationValues map[string]DestinationValues
	Summary           string
}

// DestinationValues contains safe Telegram values used by the Polish TUI.
// It never contains a bot token. TokenFile is a path to an approved secret
// file, not the file contents.
type DestinationValues struct {
	Name           string
	ChatID         string
	TokenFile      string
	TokenSource    string
	LinkedProfiles []string
	LastTestAt     string
	LastTestStatus string
	LastTestError  string
	Enabled        bool
}

type accountStatus struct {
	ID            string
	Username      string
	Authenticated bool
	AuthRequired  bool
}

type requiredAction struct {
	Code  string
	Scope string
	ID    string
}

type statusData struct {
	Accounts              []accountStatus
	Profiles              []store.Profile
	Destinations          []store.Destination
	DestinationProfiles   map[string][]string
	ActiveIncidents       []store.Incident
	PermanentFailures     []store.Delivery
	OperationalDeliveries []store.IncidentDelivery
	RequiredActions       []requiredAction
}

// Snapshot reads the operator state through the application and store
// modules. prune is false after a dry check because a dry check must not
// change observation history.
func (a *Application) Snapshot(ctx context.Context, prune bool) (Snapshot, error) {
	var snapshot Snapshot
	ctx = contextOrBackground(ctx)
	snapshot.ProfileValues = map[string]ProfileValues{}
	snapshot.DestinationValues = map[string]DestinationValues{}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	storage, err := openStore(a.config.Database)
	if err != nil {
		return snapshot, err
	}
	defer storage.Close()
	if prune {
		_, _ = storage.PruneHistory(time.Now().UTC())
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	status, err := a.readStatus(storage)
	if err != nil {
		return snapshot, err
	}

	authRequired := make(map[string]bool, len(status.Accounts))
	for _, account := range status.Accounts {
		authRequired[account.ID] = account.AuthRequired
		state := "? Brak dostępu do magazynu sesji"
		if account.AuthRequired {
			state = "! Wymaga logowania"
		} else if account.Authenticated {
			state = "OK — sesja zapisana"
		}
		snapshot.Accounts = append(snapshot.Accounts, Row{ID: account.ID, Label: account.Username, Detail: state})
		if !account.AuthRequired && !account.Authenticated {
			snapshot.Actions = append(snapshot.Actions, Row{ID: account.ID, Label: "! Sprawdź magazyn sesji: " + account.ID, Target: 1})
		}
	}

	for _, action := range status.RequiredActions {
		label := map[string]string{
			"authentication_required":      "Zaloguj konto",
			"profile_disabled":             "Profil wyłączony",
			"destination_disabled":         "Telegram wyłączony",
			"destination_test_failure":     "Sprawdź Telegram",
			"destination_delivery_failure": "Sprawdź Telegram",
			"active_incident":              "Sprawdź aktywny problem",
			"permanent_failure":            "Sprawdź błąd dostarczenia",
			"invalid_retention":            "Sprawdź okres historii",
		}[action.Code]
		if label == "" {
			label = "Sprawdź stan"
		}
		snapshot.Actions = append(snapshot.Actions, Row{
			ID: action.ID, Label: "! " + label + ": " + action.ID,
			Detail: label, Target: actionTarget(action),
		})
		if action.Code == "invalid_retention" {
			snapshot.History = append(snapshot.History, Row{
				ID:     action.ID,
				Label:  "Nieprawidłowy okres historii",
				Detail: "Ustaw prawidłowy okres w poleceniu history retention.",
			})
		}
	}
	if len(status.Accounts) == 0 {
		snapshot.Actions = append(snapshot.Actions, Row{Label: "! Dodaj konto w obszarze Konta (2, A).", Target: 1})
	}

	for _, profile := range status.Profiles {
		state := "OK — włączony"
		if !profile.Enabled {
			state = "— wyłączony"
		}
		snapshot.Profiles = append(snapshot.Profiles, Row{
			ID: profile.ID, Label: profile.ID + " — " + state,
			Detail: "Konto: " + profile.AccountID,
		})
		snapshot.ProfileValues[profile.ID] = profileValues(profile)

		monitoringState := "aktywna praca"
		nextRun := a.profileNextRun(storage, profile, authRequired[profile.AccountID], time.Now().UTC())
		if !profile.Enabled {
			monitoringState = "wstrzymane: profil wyłączony"
		} else if authRequired[profile.AccountID] {
			monitoringState = "wstrzymane: konto wymaga logowania"
		}
		snapshot.Monitoring = append(snapshot.Monitoring, Row{
			ID:     profile.ID,
			Label:  profile.ID + " — " + monitoringState,
			Detail: fmt.Sprintf("Konto: %s · Następny przebieg: %s", profile.AccountID, nextRun),
		})
	}
	for _, account := range status.Accounts {
		if account.AuthRequired {
			snapshot.Monitoring = append(snapshot.Monitoring, Row{
				ID:     "account:" + account.ID,
				Label:  "Konto " + account.ID + " — wstrzymane",
				Detail: "Wymaga logowania; profile tego konta czekają.",
			})
		}
	}
	for _, incident := range status.ActiveIncidents {
		snapshot.Monitoring = append(snapshot.Monitoring, Row{
			ID:     "incident:" + incident.ID,
			Label:  fmt.Sprintf("! Aktywny problem: %s %s", incidentScopeLabel(incident.ScopeType), incident.ScopeID),
			Detail: fmt.Sprintf("%s · Kolejne błędy: %d", incidentFailureLabel(incident.FailureCode), incident.ConsecutiveFailures),
		})
	}

	for _, destination := range status.Destinations {
		state := "OK — włączony"
		if !destination.Enabled {
			state = "— wyłączony"
		}
		linkedProfiles := append([]string(nil), status.DestinationProfiles[destination.ID]...)
		values := destinationValues(destination, linkedProfiles)
		snapshot.DestinationValues[destination.ID] = values
		snapshot.Destinations = append(snapshot.Destinations, Row{
			ID:     destination.ID,
			Label:  destination.Name + " — " + state,
			Detail: destinationDetail(values),
		})
	}

	runs, err := storage.ListRecentObservationRuns(100)
	if err != nil {
		return snapshot, err
	}
	episodes, err := storage.ListRecentEpisodes("", false, 100)
	if err != nil {
		return snapshot, err
	}
	incidents, err := storage.ListIncidents("", "", 100)
	if err != nil {
		return snapshot, err
	}
	deliveries, err := storage.ListRecentDeliveries("", "", 100)
	if err != nil {
		return snapshot, err
	}
	incidentDeliveries, err := storage.ListRecentIncidentDeliveries("", 100)
	if err != nil {
		return snapshot, err
	}

	type historyItem struct {
		at  time.Time
		row Row
	}
	items := make([]historyItem, 0, len(runs)+len(episodes)+len(incidents)+len(deliveries)+len(incidentDeliveries)+len(status.Destinations))
	for _, run := range runs {
		row := historyRunRow(run)
		snapshot.Runs = append(snapshot.Runs, row)
		items = append(items, historyItem{at: parseHistoryTime(run.StartedAt), row: row})
	}
	for _, episode := range episodes {
		row := historyEpisodeRow(episode)
		items = append(items, historyItem{at: parseHistoryTime(episode.StartedAt), row: row})
	}
	for _, incident := range incidents {
		row := historyIncidentRow(incident)
		items = append(items, historyItem{at: parseHistoryTime(incident.LastSeenAt), row: row})
	}
	for _, delivery := range deliveries {
		row := historyDeliveryRow(delivery)
		items = append(items, historyItem{at: parseHistoryTime(delivery.CreatedAt), row: row})
	}
	for _, delivery := range incidentDeliveries {
		row := historyIncidentDeliveryRow(delivery)
		items = append(items, historyItem{at: parseHistoryTime(delivery.CreatedAt), row: row})
	}
	for _, destination := range status.Destinations {
		if strings.TrimSpace(destination.LastTestAt) == "" || strings.TrimSpace(destination.LastTestStatus) == "" {
			continue
		}
		row := historyDestinationTestRow(destination)
		items = append(items, historyItem{at: parseHistoryTime(destination.LastTestAt), row: row})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].at.Equal(items[right].at) {
			return items[left].row.ID > items[right].row.ID
		}
		return items[left].at.After(items[right].at)
	})
	for _, item := range items {
		snapshot.History = append(snapshot.History, item.row)
	}
	paused := 0
	for _, profile := range status.Profiles {
		if !profile.Enabled || (profile.Enabled && authRequired[profile.AccountID]) {
			paused++
		}
	}
	snapshot.Summary = fmt.Sprintf("Konta: %d · Profile: %d · Wstrzymane: %d · Problemy: %d", len(snapshot.Accounts), len(snapshot.Profiles), paused, len(status.ActiveIncidents))
	return snapshot, nil
}

func destinationValues(destination store.Destination, linkedProfiles []string) DestinationValues {
	values := DestinationValues{
		Name:           destination.Name,
		ChatID:         destination.ChatID,
		TokenSource:    destination.TokenSource,
		LinkedProfiles: append([]string(nil), linkedProfiles...),
		LastTestAt:     destination.LastTestAt,
		LastTestStatus: destination.LastTestStatus,
		LastTestError:  destination.LastTestError,
		Enabled:        destination.Enabled,
	}
	if destination.TokenSource == store.TokenSourceFile {
		values.TokenFile = destination.TokenRef
	}
	return values
}

func destinationDetail(values DestinationValues) string {
	token := "Token: magazyn sekretów"
	switch values.TokenSource {
	case store.TokenSourceFile:
		token = "Token: plik " + values.TokenFile
	case store.TokenSourcePrompt:
		token = "Token: pytaj przy wysyłce"
	}
	profiles := "Profile: brak"
	if len(values.LinkedProfiles) > 0 {
		profiles = "Profile: " + strings.Join(values.LinkedProfiles, ", ")
	}
	detail := fmt.Sprintf("Chat: %s · %s · %s", values.ChatID, token, profiles)
	if values.LastTestStatus != "" {
		detail += fmt.Sprintf(" · Ostatni test: %s", historyDeliveryStatus(values.LastTestStatus))
		if values.LastTestError != "" {
			detail += " — " + values.LastTestError
		}
	}
	return detail
}

func historyRunRow(run store.ObservationRun) Row {
	state := map[string]string{
		store.ObservationRunRunning:     "w toku",
		store.ObservationRunComplete:    "ukończony",
		store.ObservationRunFailed:      "błąd",
		store.ObservationRunPartial:     "częściowy",
		store.ObservationRunCancelled:   "anulowany",
		store.ObservationRunConflicting: "konflikt",
		store.ObservationRunStale:       "nieaktualny",
	}[run.Status]
	if state == "" {
		state = run.Status
	}
	detail := fmt.Sprintf("Profil: %s · Start: %s", run.ProfileID, run.StartedAt)
	if run.CompletedAt != "" {
		detail += " · Koniec: " + run.CompletedAt
	}
	if run.Status == store.ObservationRunComplete {
		detail += fmt.Sprintf(" · Terminów: %d", run.SlotCount)
	}
	if run.ErrorCode != "" {
		detail += " · Błąd: " + run.ErrorCode
	}
	if run.ErrorMessage != "" {
		detail += " — " + run.ErrorMessage
	}
	return Row{ID: run.ID, Label: fmt.Sprintf("Przebieg %s — %s (%s)", run.ProfileID, state, run.Status), Detail: detail}
}

func historyEpisodeRow(episode store.AvailabilityEpisode) Row {
	state := "zakończona"
	status := "ended"
	if episode.Active {
		state = "aktywna"
		status = "active"
	}
	detail := fmt.Sprintf("Profil: %s · Czas: %s · Lekarz: %s · Specjalizacja: %s · Placówka: %s · Typ wizyty: %s · Start: %s",
		episode.ProfileID, displayValue(episode.Time), displayValue(episode.Doctor), displayValue(episode.Specialty), displayValue(episode.Clinic), displayValue(episode.VisitType), episode.StartedAt)
	if episode.EndedAt != "" {
		detail += " · Koniec: " + episode.EndedAt
	}
	return Row{ID: episode.ID, Label: fmt.Sprintf("Dostępność %s — %s (%s)", episode.ProfileID, state, status), Detail: detail}
}

func historyIncidentRow(incident store.Incident) Row {
	state := "aktywny"
	if incident.Status == store.IncidentStatusResolved {
		state = "rozwiązany"
	}
	detail := fmt.Sprintf("Zakres: %s %s · Błąd: %s · Kolejne błędy: %d · Start: %s · Ostatnio: %s",
		incidentScopeLabel(incident.ScopeType), incident.ScopeID, incident.FailureCode, incident.ConsecutiveFailures, incident.FirstSeenAt, incident.LastSeenAt)
	if incident.AccountID != "" {
		detail += " · Konto: " + incident.AccountID
	}
	if incident.ProfileID != "" {
		detail += " · Profil: " + incident.ProfileID
	}
	if incident.DestinationID != "" {
		detail += " · Telegram: " + incident.DestinationID
	}
	if incident.FailureMessage != "" {
		detail += " — " + incident.FailureMessage
	}
	return Row{ID: incident.ID, Label: fmt.Sprintf("Incydent %s %s — %s (%s)", incidentScopeLabel(incident.ScopeType), incident.ScopeID, state, incident.Status), Detail: detail}
}

func historyDeliveryRow(delivery store.Delivery) Row {
	status := historyDeliveryRowStatus(delivery.Status, delivery.LastError)
	detail := fmt.Sprintf("Profil: %s · Epizod: %s · Telegram: %s · Próby: %d · Utworzono: %s · Zmieniono: %s",
		delivery.ProfileID, delivery.EpisodeID, delivery.DestinationID, delivery.Attempts, delivery.CreatedAt, delivery.UpdatedAt)
	if delivery.NextAttemptAt != "" {
		detail += " · Następna próba: " + delivery.NextAttemptAt
	}
	if delivery.DeliveredAt != "" {
		detail += " · Dostarczono: " + delivery.DeliveredAt
	}
	if delivery.LastError != "" {
		detail += " · Błąd: " + delivery.LastError
	}
	return Row{ID: delivery.ID, Label: fmt.Sprintf("Dostarczenie Telegram — %s — %s (%s)", delivery.ProfileID, status, delivery.Status), Detail: detail}
}

func historyIncidentDeliveryRow(delivery store.IncidentDelivery) Row {
	status := historyDeliveryRowStatus(delivery.Status, delivery.LastError)
	detail := fmt.Sprintf("Incydent: %s · Telegram: %s · Typ: %s · Próby: %d · Utworzono: %s · Zmieniono: %s",
		delivery.IncidentID, delivery.DestinationID, delivery.Kind, delivery.Attempts, delivery.CreatedAt, delivery.UpdatedAt)
	if delivery.NextAttemptAt != "" {
		detail += " · Następna próba: " + delivery.NextAttemptAt
	}
	if delivery.DeliveredAt != "" {
		detail += " · Dostarczono: " + delivery.DeliveredAt
	}
	if delivery.LastError != "" {
		detail += " · Błąd: " + delivery.LastError
	}
	return Row{ID: delivery.ID, Label: fmt.Sprintf("Powiadomienie incydentu — %s — %s (%s)", delivery.Kind, status, delivery.Status), Detail: detail}
}

func historyDestinationTestRow(destination store.Destination) Row {
	detail := fmt.Sprintf("Cel: %s · Chat: %s · Próba: %s", destination.ID, destination.ChatID, destination.LastTestAt)
	if destination.LastTestError != "" {
		detail += " · " + destination.LastTestError
	}
	return Row{ID: "test:" + destination.ID, Label: fmt.Sprintf("Test Telegram — %s — %s (%s)", destination.Name, historyDeliveryStatus(destination.LastTestStatus), destination.LastTestStatus), Detail: detail}
}

func historyDeliveryStatus(status string) string {
	labels := map[string]string{
		store.DeliveryPending:          "oczekuje",
		store.DeliveryDelivered:        "dostarczone",
		store.DeliveryRetry:            "ponowienie",
		store.DeliveryPermanentFailure: "trwały błąd",
		"unknown_delivery":             "nieznany wynik",
		"temporary_failure":            "błąd tymczasowy",
		"rate_limited":                 "limit zapytań",
		"cancelled":                    "anulowane",
		"timeout":                      "przekroczony czas",
	}
	if label := labels[status]; label != "" {
		return label
	}
	return status
}

func historyDeliveryRowStatus(status, message string) string {
	if status == store.DeliveryPermanentFailure && isNonDeliveryPermanentFailure(message) {
		return "anulowane"
	}
	return historyDeliveryStatus(status)
}

func isNonDeliveryPermanentFailure(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "slot is no longer available") ||
		strings.Contains(message, "incident ended before failure was delivered")
}

func displayValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func parseHistoryTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (a *Application) readStatus(storage *store.Store) (statusData, error) {
	data := statusData{}
	backend := sessionStore(a.config.SessionDir)
	accounts, err := storage.ListAccounts()
	if err != nil {
		return data, err
	}
	for _, account := range accounts {
		state := accountStatus{ID: account.ID, Username: account.Username, Authenticated: true}
		if saved, loadErr := backend.Load(account.ID); loadErr != nil {
			if errors.Is(loadErr, session.ErrNotFound) || errors.Is(loadErr, session.ErrCorrupt) {
				state.Authenticated = false
				state.AuthRequired = true
			} else {
				state.Authenticated = false
			}
		} else if !medicover.HasUsableSessionCookiesForIssuer(saved, strings.TrimSpace(a.config.MedicoverBaseURL), time.Now().UTC()) {
			state.Authenticated = false
			state.AuthRequired = true
		}
		data.Accounts = append(data.Accounts, state)
		if state.AuthRequired {
			data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "authentication_required", Scope: "account", ID: account.ID})
		}
	}
	data.Profiles, err = storage.ListProfiles("")
	if err != nil {
		return data, err
	}
	data.DestinationProfiles = map[string][]string{}
	for _, profile := range data.Profiles {
		if !profile.Enabled {
			data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "profile_disabled", Scope: "profile", ID: profile.ID})
		}
		linked, linkErr := storage.ListProfileDestinationIDs(profile.ID)
		if linkErr != nil {
			return data, linkErr
		}
		for _, destinationID := range linked {
			data.DestinationProfiles[destinationID] = append(data.DestinationProfiles[destinationID], profile.ID)
		}
	}
	data.Destinations, err = storage.ListDestinations()
	if err != nil {
		return data, err
	}
	for _, destination := range data.Destinations {
		if !destination.Enabled {
			data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "destination_disabled", Scope: "destination", ID: destination.ID})
		}
		if destination.LastTestStatus == store.DeliveryPermanentFailure {
			data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "destination_test_failure", Scope: "destination", ID: destination.ID})
		}
	}
	data.ActiveIncidents, err = storage.ListIncidents(store.IncidentStatusActive, "", 1000)
	if err != nil {
		return data, err
	}
	for _, incident := range data.ActiveIncidents {
		id := incident.ScopeID
		if id == "" {
			id = incident.ID
		}
		data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "active_incident", Scope: incident.ScopeType, ID: id})
		if incident.ScopeType == store.IncidentScopeAccount && accountPauseIncident(incident) {
			for index := range data.Accounts {
				if data.Accounts[index].ID == incident.ScopeID {
					data.Accounts[index].AuthRequired = true
					data.Accounts[index].Authenticated = false
				}
			}
		}
	}
	data.PermanentFailures, err = storage.ListRecentDeliveries("", store.DeliveryPermanentFailure, 1000)
	if err != nil {
		return data, err
	}
	destinationFailures := map[string]struct{}{}
	for _, delivery := range data.PermanentFailures {
		if isNonDeliveryPermanentFailure(delivery.LastError) {
			continue
		}
		data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "permanent_failure", Scope: "delivery", ID: delivery.ID})
		if destinationID := strings.TrimSpace(delivery.DestinationID); destinationID != "" {
			if _, seen := destinationFailures[destinationID]; !seen {
				data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "destination_delivery_failure", Scope: "destination", ID: destinationID})
				destinationFailures[destinationID] = struct{}{}
			}
		}
	}
	data.OperationalDeliveries, err = storage.ListRecentIncidentDeliveriesByStatus("", store.DeliveryPermanentFailure, 1000)
	if err != nil {
		return data, err
	}
	for _, delivery := range data.OperationalDeliveries {
		if isNonDeliveryPermanentFailure(delivery.LastError) {
			continue
		}
		data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "permanent_failure", Scope: "operational_delivery", ID: delivery.ID})
		if destinationID := strings.TrimSpace(delivery.DestinationID); destinationID != "" {
			if _, seen := destinationFailures[destinationID]; !seen {
				data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "destination_delivery_failure", Scope: "destination", ID: destinationID})
				destinationFailures[destinationID] = struct{}{}
			}
		}
	}
	if _, err := storage.GetHistoryRetentionDays(); err != nil && errors.Is(err, store.ErrHistoryInvalid) {
		data.RequiredActions = append(data.RequiredActions, requiredAction{Code: "invalid_retention", Scope: "history", ID: "retention"})
	} else if err != nil {
		return data, err
	}
	return data, nil
}

func (a *Application) profileNextRun(storage *store.Store, profile store.Profile, authRequired bool, now time.Time) string {
	if !profile.Enabled || authRequired {
		return "wstrzymany"
	}
	last, ok := latestRunStart(storage, profile.ID)
	if !ok {
		return "oczekuje na pierwszy przebieg"
	}
	next := monitoring.NextRunAfter(profile, last)
	if !next.After(now) {
		return "teraz"
	}
	return next.Format("2006-01-02 15:04")
}

func latestRunStart(storage *store.Store, profileID string) (time.Time, bool) {
	runs, err := storage.ListObservationRuns(profileID)
	if err != nil {
		return time.Time{}, false
	}
	return monitoring.LatestRunStart(runs)
}

func profileValues(profile store.Profile) ProfileValues {
	return ProfileValues{
		AccountID: profile.AccountID, RegionIDs: profile.RegionIDs, SpecialtyIDs: profile.SpecialtyIDs,
		ClinicIDs: profile.ClinicIDs, DoctorIDs: profile.DoctorIDs, LanguageIDs: profile.LanguageIDs,
		VisitType: profile.VisitType, SearchType: profile.SearchType, StartDate: profile.StartDate,
		EndDate: profile.EndDate, CheckIntervalMinutes: fmt.Sprintf("%d", profile.CheckIntervalMinutes), Enabled: profile.Enabled,
	}
}

func incidentScopeLabel(scope string) string {
	switch scope {
	case store.IncidentScopeAccount:
		return "konto"
	case store.IncidentScopeProfile:
		return "profil"
	case store.IncidentScopeDestination:
		return "Telegram"
	default:
		return "obszar"
	}
}

func incidentFailureLabel(code string) string {
	switch code {
	case "authentication_required", "mfa_required":
		return "Wymagane logowanie"
	case "invalid_credentials":
		return "Nieprawidłowe dane logowania"
	case "protocol_changed":
		return "Zmiana protokołu"
	case "temporary_failure":
		return "Błąd tymczasowy"
	case "rate_limited":
		return "Limit zapytań"
	case "permanent_failure":
		return "Trwały błąd"
	default:
		return "Aktywny problem"
	}
}

func accountPauseIncident(incident store.Incident) bool {
	if incident.Kind == store.IncidentKindAuth {
		return true
	}
	switch strings.TrimSpace(incident.FailureCode) {
	case "authentication_required", "mfa_required", "invalid_credentials":
		return true
	default:
		return false
	}
}

func actionTarget(action requiredAction) int {
	if action.Code == "active_incident" {
		switch action.Scope {
		case "account":
			return 1
		case "profile":
			return 2
		case "destination":
			return 3
		default:
			return 4
		}
	}
	return map[string]int{"account": 1, "profile": 2, "destination": 3, "delivery": 5, "operational_delivery": 5, "history": 5}[action.Scope]
}

func profileFromValues(id string, values ProfileValues) (store.Profile, error) {
	region, err := store.NormalizeIDList(values.RegionIDs)
	if err != nil || region == "" {
		return store.Profile{}, fmt.Errorf("region must be comma-separated Medicover IDs")
	}
	specialty, err := store.NormalizeIDList(values.SpecialtyIDs)
	if err != nil || specialty == "" {
		return store.Profile{}, fmt.Errorf("specialty must be comma-separated Medicover IDs")
	}
	clinic, err := store.NormalizeIDList(values.ClinicIDs)
	if err != nil {
		return store.Profile{}, fmt.Errorf("clinic IDs must be comma-separated positive integers")
	}
	doctor, err := store.NormalizeIDList(values.DoctorIDs)
	if err != nil {
		return store.Profile{}, fmt.Errorf("doctor IDs must be comma-separated positive integers")
	}
	language, err := store.NormalizeIDList(values.LanguageIDs)
	if err != nil {
		return store.Profile{}, fmt.Errorf("language IDs must be comma-separated positive integers")
	}
	searchType, err := store.NormalizeSearchType(values.SearchType)
	if err != nil {
		return store.Profile{}, fmt.Errorf("search type must be Standard or DiagnosticProcedure")
	}
	startDate, err := store.NormalizeDate(values.StartDate, true)
	if err != nil {
		return store.Profile{}, fmt.Errorf("start date must be YYYY-MM-DD")
	}
	endDate, err := store.NormalizeDate(values.EndDate, true)
	if err != nil {
		return store.Profile{}, fmt.Errorf("end date must be YYYY-MM-DD")
	}
	interval, err := strconv.Atoi(strings.TrimSpace(values.CheckIntervalMinutes))
	if err != nil || interval < 1 || interval > 43200 {
		return store.Profile{}, fmt.Errorf("check interval must be 1..43200 minutes")
	}
	return store.Profile{
		ID: id, AccountID: strings.TrimSpace(values.AccountID), RegionIDs: region, SpecialtyIDs: specialty,
		ClinicIDs: clinic, DoctorIDs: doctor, LanguageIDs: language, VisitType: strings.TrimSpace(values.VisitType),
		SearchType: searchType, StartDate: startDate, EndDate: endDate, CheckIntervalMinutes: interval,
		Enabled: true,
	}, nil
}

func profileUpdate(values ProfileValues, clear []string) (store.ProfileUpdate, bool, error) {
	var update store.ProfileUpdate
	hasChange := false
	if strings.TrimSpace(values.RegionIDs) != "" {
		normalized, err := store.NormalizeIDList(values.RegionIDs)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("region must be comma-separated Medicover IDs")
		}
		update.RegionIDs = &normalized
		hasChange = true
	}
	if strings.TrimSpace(values.SpecialtyIDs) != "" {
		normalized, err := store.NormalizeIDList(values.SpecialtyIDs)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("specialty must be comma-separated Medicover IDs")
		}
		update.SpecialtyIDs = &normalized
		hasChange = true
	}
	if fieldIn(clear, "clinic_ids") {
		empty := ""
		update.ClinicIDs = &empty
		hasChange = true
	} else if strings.TrimSpace(values.ClinicIDs) != "" {
		normalized, err := store.NormalizeIDList(values.ClinicIDs)
		if err != nil {
			return update, false, fmt.Errorf("clinic IDs must be comma-separated positive integers")
		}
		update.ClinicIDs = &normalized
		hasChange = true
	}
	if fieldIn(clear, "doctor_ids") {
		empty := ""
		update.DoctorIDs = &empty
		hasChange = true
	} else if strings.TrimSpace(values.DoctorIDs) != "" {
		normalized, err := store.NormalizeIDList(values.DoctorIDs)
		if err != nil {
			return update, false, fmt.Errorf("doctor IDs must be comma-separated positive integers")
		}
		update.DoctorIDs = &normalized
		hasChange = true
	}
	if fieldIn(clear, "language_ids") {
		empty := ""
		update.LanguageIDs = &empty
		hasChange = true
	} else if strings.TrimSpace(values.LanguageIDs) != "" {
		normalized, err := store.NormalizeIDList(values.LanguageIDs)
		if err != nil {
			return update, false, fmt.Errorf("language IDs must be comma-separated positive integers")
		}
		update.LanguageIDs = &normalized
		hasChange = true
	}
	if fieldIn(clear, "visit_type") {
		empty := ""
		update.VisitType = &empty
		hasChange = true
	} else if strings.TrimSpace(values.VisitType) != "" {
		value := strings.TrimSpace(values.VisitType)
		update.VisitType = &value
		hasChange = true
	}
	if fieldIn(clear, "search_type") {
		value := store.SearchTypeStandard
		update.SearchType = &value
		hasChange = true
	} else if strings.TrimSpace(values.SearchType) != "" {
		normalized, err := store.NormalizeSearchType(values.SearchType)
		if err != nil {
			return update, false, fmt.Errorf("search type must be Standard or DiagnosticProcedure")
		}
		update.SearchType = &normalized
		hasChange = true
	}
	if fieldIn(clear, "start_date") {
		empty := ""
		update.StartDate = &empty
		hasChange = true
	} else if strings.TrimSpace(values.StartDate) != "" {
		normalized, err := store.NormalizeDate(values.StartDate, true)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("start date must be YYYY-MM-DD")
		}
		update.StartDate = &normalized
		hasChange = true
	}
	if fieldIn(clear, "end_date") {
		empty := ""
		update.EndDate = &empty
		hasChange = true
	} else if strings.TrimSpace(values.EndDate) != "" {
		normalized, err := store.NormalizeDate(values.EndDate, true)
		if err != nil || normalized == "" {
			return update, false, fmt.Errorf("end date must be YYYY-MM-DD")
		}
		update.EndDate = &normalized
		hasChange = true
	}
	if strings.TrimSpace(values.CheckIntervalMinutes) != "" {
		interval, err := strconv.Atoi(strings.TrimSpace(values.CheckIntervalMinutes))
		if err != nil || interval < 1 || interval > 43200 {
			return update, false, fmt.Errorf("check interval must be 1..43200 minutes")
		}
		update.CheckIntervalMinutes = &interval
		hasChange = true
	}
	return update, hasChange, nil
}

func fieldIn(fields []string, wanted string) bool {
	for _, field := range fields {
		if field == wanted {
			return true
		}
	}
	return false
}

func profileOperationError(err error) error {
	code := "database_error"
	switch {
	case errors.Is(err, store.ErrProfileExists):
		code = "profile_exists"
	case errors.Is(err, store.ErrProfileNotFound):
		code = "profile_not_found"
	case errors.Is(err, store.ErrAccountNotFound):
		code = "account_not_found"
	case errors.Is(err, store.ErrProfileInvalid):
		code = "invalid_arguments"
	}
	return &OperationError{Code: code, Message: err.Error(), Cause: err}
}

func openStore(database string) (*store.Store, error) {
	if _, err := store.Initialize(database); err != nil {
		return nil, err
	}
	return store.Open(database)
}

func sessionStore(directory string) session.Store {
	if strings.TrimSpace(directory) != "" {
		return session.FileStore{Dir: directory}
	}
	return session.SecretServiceStore{}
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
