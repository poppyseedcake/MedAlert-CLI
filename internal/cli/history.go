package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/operatorstatus"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

// runHistory dispatches the history automation group. History never touches
// secrets: it only reads configuration, observation state, incidents, and
// delivery history, so it never prompts even in interactive terminals.
func runHistory(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	switch command {
	case "history runs":
		return historyRuns(command, settings, stdout, stderr, jsonOutput)
	case "history episodes":
		return historyEpisodes(command, settings, stdout, stderr, jsonOutput)
	case "history incidents":
		return historyIncidents(command, settings, stdout, stderr, jsonOutput)
	case "history deliveries":
		return historyDeliveries(command, settings, stdout, stderr, jsonOutput)
	case "history incident-deliveries":
		return historyIncidentDeliveries(command, settings, stdout, stderr, jsonOutput)
	case "history status":
		return historyStatus(command, settings, stdout, stderr, jsonOutput)
	case "history retention":
		return historyRetention(command, settings, stdout, stderr, jsonOutput)
	case "history prune":
		return historyPrune(command, settings, stdout, stderr, jsonOutput)
	default:
		writeError(stderr, command, "invalid_arguments", "a supported history command is required", jsonOutput)
		return 2
	}
}

func historyRuns(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	limit, ok := checkHistoryLimit(command, settings, stderr, jsonOutput)
	if !ok {
		return 2
	}
	profileFilter := strings.TrimSpace(settings.profileID)
	statusFilter := strings.TrimSpace(settings.historyStatus)
	if statusFilter != "" && !validRunStatus(statusFilter) {
		writeError(stderr, command, "invalid_arguments", "status must be running, complete, failed, partial, cancelled, conflicting, or stale", jsonOutput)
		return 2
	}
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history runs (use --profile and --limit)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	if profileFilter != "" {
		if _, err := storage.GetProfile(profileFilter); err != nil {
			return reportHistoryError(stderr, command, err, jsonOutput)
		}
	}
	var runs []store.ObservationRun
	if profileFilter != "" {
		runs, err = storage.ListRecentObservationRunsForProfile(profileFilter, statusFilter, limit)
		if err != nil {
			return reportHistoryError(stderr, command, err, jsonOutput)
		}
	} else {
		if statusFilter != "" {
			runs, err = storage.ListRecentObservationRunsByStatus(statusFilter, limit)
		} else {
			runs, err = storage.ListRecentObservationRuns(limit)
		}
		if err != nil {
			return reportHistoryError(stderr, command, err, jsonOutput)
		}
	}
	filtered := []store.ObservationRun{}
	for _, run := range runs {
		if statusFilter != "" && run.Status != statusFilter {
			continue
		}
		filtered = append(filtered, run)
		if profileFilter == "" && len(filtered) >= limit {
			break
		}
	}
	if profileFilter != "" && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"runs": filtered})
		return 0
	}
	if len(filtered) == 0 {
		fmt.Fprintln(stdout, "No observation runs found.")
		return 0
	}
	for _, run := range filtered {
		fmt.Fprintln(stdout, formatHistoryRun(run))
	}
	return 0
}

func historyEpisodes(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	limit, ok := checkHistoryLimit(command, settings, stderr, jsonOutput)
	if !ok {
		return 2
	}
	statusFilter := strings.TrimSpace(settings.historyStatus)
	if statusFilter != "" && statusFilter != "active" && statusFilter != "ended" && statusFilter != "all" {
		writeError(stderr, command, "invalid_arguments", "status must be active, ended, or all", jsonOutput)
		return 2
	}
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history episodes (use --profile and --limit)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	profileFilter := strings.TrimSpace(settings.profileID)
	if profileFilter != "" {
		if _, err := storage.GetProfile(profileFilter); err != nil {
			return reportHistoryError(stderr, command, err, jsonOutput)
		}
	}
	activeOnly := statusFilter == "active"
	var episodes []store.AvailabilityEpisode
	if statusFilter == "ended" {
		episodes, err = storage.ListRecentEpisodesByStatus(profileFilter, "ended", limit)
	} else {
		episodes, err = storage.ListRecentEpisodes(profileFilter, activeOnly, limit)
	}
	if err != nil {
		return reportHistoryError(stderr, command, err, jsonOutput)
	}
	filtered := episodes
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"episodes": filtered})
		return 0
	}
	if len(filtered) == 0 {
		fmt.Fprintln(stdout, "No availability episodes found.")
		return 0
	}
	for _, episode := range filtered {
		fmt.Fprintln(stdout, formatHistoryEpisode(episode))
	}
	return 0
}

func historyIncidents(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	limit, ok := checkHistoryLimit(command, settings, stderr, jsonOutput)
	if !ok {
		return 2
	}
	statusFilter := strings.TrimSpace(settings.historyStatus)
	if statusFilter == "" {
		statusFilter = "active"
	}
	if statusFilter != "active" && statusFilter != "resolved" && statusFilter != "all" {
		writeError(stderr, command, "invalid_arguments", "status must be active, resolved, or all", jsonOutput)
		return 2
	}
	scopeFilter := strings.TrimSpace(settings.historyScope)
	if scopeFilter != "" && scopeFilter != store.IncidentScopeAccount && scopeFilter != store.IncidentScopeProfile && scopeFilter != store.IncidentScopeDestination {
		writeError(stderr, command, "invalid_arguments", "scope must be account, profile, or destination", jsonOutput)
		return 2
	}
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history incidents (use --status, --scope, and --limit)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	queryStatus := statusFilter
	if statusFilter == "all" {
		queryStatus = ""
	}
	incidents, err := storage.ListIncidents(queryStatus, scopeFilter, limit)
	if err != nil {
		return reportHistoryError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"incidents": incidents})
		return 0
	}
	if len(incidents) == 0 {
		fmt.Fprintln(stdout, "No operational incidents found.")
		return 0
	}
	for _, incident := range incidents {
		fmt.Fprintln(stdout, formatHistoryIncident(incident))
	}
	return 0
}

func historyDeliveries(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	limit, ok := checkHistoryLimit(command, settings, stderr, jsonOutput)
	if !ok {
		return 2
	}
	statusFilter := strings.TrimSpace(settings.historyStatus)
	if statusFilter != "" && statusFilter != store.DeliveryPending && statusFilter != store.DeliveryDelivered && statusFilter != store.DeliveryRetry && statusFilter != store.DeliveryPermanentFailure {
		writeError(stderr, command, "invalid_arguments", "status must be pending, delivered, retry, or permanent_failure", jsonOutput)
		return 2
	}
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history deliveries (use --profile, --status, and --limit)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	if profileFilter := strings.TrimSpace(settings.profileID); profileFilter != "" {
		if _, err := storage.GetProfile(profileFilter); err != nil {
			return reportHistoryError(stderr, command, err, jsonOutput)
		}
	}
	deliveries, err := storage.ListRecentDeliveries(strings.TrimSpace(settings.profileID), statusFilter, limit)
	if err != nil {
		return reportHistoryError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"deliveries": deliveries})
		return 0
	}
	if len(deliveries) == 0 {
		fmt.Fprintln(stdout, "No telegram deliveries found.")
		return 0
	}
	for _, delivery := range deliveries {
		fmt.Fprintln(stdout, formatHistoryDelivery(delivery))
	}
	return 0
}

func historyIncidentDeliveries(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	limit, ok := checkHistoryLimit(command, settings, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history incident-deliveries (use --incident and --limit)", jsonOutput)
		return 2
	}
	incidentID := strings.TrimSpace(settings.historyIncidentID)
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	deliveries, err := storage.ListRecentIncidentDeliveries(incidentID, limit)
	if err != nil {
		return reportHistoryError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"deliveries": deliveries})
		return 0
	}
	if len(deliveries) == 0 {
		fmt.Fprintln(stdout, "No incident deliveries found.")
		return 0
	}
	for _, delivery := range deliveries {
		fmt.Fprintf(stdout, "%s %s %s %s attempts:%d\n", delivery.ID, delivery.IncidentID, delivery.Kind, delivery.Status, delivery.Attempts)
	}
	return 0
}

// historyStatusData is the stable machine contract for history status and
// doctor diagnostics. Text may change; these fields and meanings do not.
type historyStatusData struct {
	Accounts              []historyAccountStatus   `json:"accounts"`
	DisabledProfiles      []historyProfileRef      `json:"disabled_profiles"`
	DisabledDestinations  []historyDestRef         `json:"disabled_destinations"`
	ActiveIncidents       []store.Incident         `json:"active_incidents"`
	PermanentFailures     []store.Delivery         `json:"permanent_failures"`
	OperationalDeliveries []store.IncidentDelivery `json:"operational_deliveries"`
	RequiredActions       []historyAction          `json:"required_actions"`
	RetentionDays         int                      `json:"retention_days"`
	Summary               map[string]int           `json:"summary"`
}

type historyAccountStatus struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Authenticated bool   `json:"authenticated"`
	AuthRequired  bool   `json:"auth_required"`
}

type historyProfileRef struct {
	ID      string `json:"id"`
	Account string `json:"account"`
}

type historyDestRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type historyAction struct {
	Code    string `json:"code"`
	Scope   string `json:"scope"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

func historyStatus(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history status", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	if settings.sessionDir == "" {
		// Session directory defaults from the environment were already
		// resolved in parse, but callers that build options directly (tests)
		// may leave it empty.
		settings.sessionDir = strings.TrimSpace(os.Getenv("MEDALERT_SESSION_DIR"))
	}
	data, err := collectHistoryStatusForIssuer(storage, sessionStoreFor(settings), settings.medicoverBaseURL)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	// Optional narrowing filters keep automation stable: --account and
	// --profile only filter the already-collected view, never change codes.
	if accountFilter := strings.TrimSpace(settings.accountID); accountFilter != "" {
		filtered := []historyAccountStatus{}
		for _, account := range data.Accounts {
			if account.ID == accountFilter {
				filtered = append(filtered, account)
			}
		}
		data.Accounts = filtered
	}
	if profileFilter := strings.TrimSpace(settings.profileID); profileFilter != "" {
		filteredProfiles := []historyProfileRef{}
		for _, profile := range data.DisabledProfiles {
			if profile.ID == profileFilter {
				filteredProfiles = append(filteredProfiles, profile)
			}
		}
		data.DisabledProfiles = filteredProfiles
		filteredActions := []historyAction{}
		for _, action := range data.RequiredActions {
			if action.Scope != "profile" || action.ID == profileFilter {
				// Keep account/destination/incident actions: narrowing to a
				// profile must not hide an account that needs login.
				if action.Scope == "profile" && action.ID != profileFilter {
					continue
				}
				filteredActions = append(filteredActions, action)
			}
		}
		data.RequiredActions = filteredActions
	}
	if jsonOutput {
		writeResult(stdout, command, data)
		return 0
	}
	if len(data.RequiredActions) == 0 {
		fmt.Fprintln(stdout, "Status: ok, no actions required.")
	} else {
		fmt.Fprintf(stdout, "Status: %d action(s) required.\n", len(data.RequiredActions))
		for _, action := range data.RequiredActions {
			fmt.Fprintf(stdout, "- %s %s %s: %s\n", action.Code, action.Scope, action.ID, action.Message)
		}
	}
	fmt.Fprintf(stdout, "Accounts: %d, profiles: %d (%d disabled), destinations: %d (%d disabled), active incidents: %d, permanent failures: %d.\n",
		data.Summary["accounts"], data.Summary["profiles"], data.Summary["disabled_profiles"],
		data.Summary["destinations"], data.Summary["disabled_destinations"],
		data.Summary["active_incidents"], data.Summary["permanent_failures"])
	return 0
}

// collectHistoryStatus builds the operator status view: authentication state,
// disabled configuration, active incidents, and permanent delivery failures.
// It never prompts and never returns secret values.
func collectHistoryStatus(storage *store.Store, backend session.Store) (historyStatusData, error) {
	return collectHistoryStatusForIssuer(storage, backend, "")
}

func collectHistoryStatusForIssuer(storage *store.Store, backend session.Store, medicoverIssuer string) (historyStatusData, error) {
	status, err := operatorstatus.Read(storage, backend, medicoverIssuer, time.Now().UTC())
	if err != nil {
		return historyStatusData{}, err
	}
	data := historyStatusData{
		Accounts: []historyAccountStatus{}, DisabledProfiles: []historyProfileRef{},
		DisabledDestinations: []historyDestRef{}, RequiredActions: []historyAction{},
		ActiveIncidents: status.ActiveIncidents, PermanentFailures: status.PermanentFailures,
		OperationalDeliveries: status.OperationalDeliveries, RetentionDays: status.RetentionDays,
	}
	for _, account := range status.Accounts {
		data.Accounts = append(data.Accounts, historyAccountStatus{
			ID: account.ID, Username: account.Username, Authenticated: account.Authenticated, AuthRequired: account.AuthRequired,
		})
	}
	for _, profile := range status.Profiles {
		if !profile.Enabled {
			data.DisabledProfiles = append(data.DisabledProfiles, historyProfileRef{ID: profile.ID, Account: profile.AccountID})
		}
	}
	for _, destination := range status.Destinations {
		if !destination.Enabled {
			data.DisabledDestinations = append(data.DisabledDestinations, historyDestRef{ID: destination.ID, Name: destination.Name})
		}
	}
	for _, action := range status.RequiredActions {
		data.RequiredActions = append(data.RequiredActions, historyAction{
			Code: action.Code, Scope: action.Scope, ID: action.ID, Message: historyActionMessage(action),
		})
	}
	data.Summary = map[string]int{
		"accounts": len(data.Accounts), "profiles": len(status.Profiles), "disabled_profiles": len(data.DisabledProfiles),
		"destinations": len(status.Destinations), "disabled_destinations": len(data.DisabledDestinations),
		"active_incidents": len(data.ActiveIncidents), "permanent_failures": len(data.PermanentFailures) + len(data.OperationalDeliveries),
		"operational_deliveries": len(data.OperationalDeliveries), "required_actions": len(data.RequiredActions),
	}
	return data, nil
}

func historyActionMessage(action operatorstatus.Action) string {
	switch action.Code {
	case "authentication_required":
		return fmt.Sprintf("account %s requires authentication", action.ID)
	case "session_unavailable":
		return fmt.Sprintf("check session storage for account %s", action.ID)
	case "profile_disabled":
		return fmt.Sprintf("profile %s is disabled", action.ID)
	case "destination_disabled":
		return fmt.Sprintf("telegram destination %s is disabled", action.ID)
	case "destination_test_failure":
		return fmt.Sprintf("check the failed test for telegram destination %s", action.ID)
	case "destination_delivery_failure":
		return fmt.Sprintf("check delivery to telegram destination %s", action.ID)
	case "active_incident":
		return fmt.Sprintf("check the active incident for %s %s", action.Scope, action.ID)
	case "permanent_failure":
		return fmt.Sprintf("%s %s failed permanently", action.Scope, action.ID)
	case "invalid_retention":
		return "set a valid history retention period"
	default:
		return fmt.Sprintf("check %s %s", action.Scope, action.ID)
	}
}

func historyRetention(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history retention (use --retention-days to change the policy)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	raw := strings.TrimSpace(settings.retentionDaysRaw)
	if raw == "" {
		days, err := storage.GetHistoryRetentionDays()
		if err != nil {
			writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
			return 2
		}
		if jsonOutput {
			writeResult(stdout, command, map[string]any{"retention_days": days})
			return 0
		}
		fmt.Fprintf(stdout, "History retention: %d days.\n", days)
		return 0
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		writeError(stderr, command, "invalid_arguments", "retention days must be an integer", jsonOutput)
		return 2
	}
	saved, err := storage.SetHistoryRetentionDays(days)
	if err != nil {
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"retention_days": saved})
		return 0
	}
	fmt.Fprintf(stdout, "History retention set to %d days.\n", saved)
	return 0
}

func historyPrune(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for history prune", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.retentionDaysRaw) != "" {
		writeError(stderr, command, "invalid_arguments", "--retention-days is not supported for history prune (use history retention to change the policy)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	result, err := storage.PruneHistory(time.Now().UTC())
	if err != nil {
		return reportHistoryError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"pruned": result})
		return 0
	}
	fmt.Fprintf(stdout, "Pruned history: %d runs, %d episodes, %d deliveries, %d incidents, %d incident deliveries.\n",
		result.ObservationRuns, result.AvailabilityEpisodes, result.TelegramDeliveries, result.OperationalIncidents, result.OperationalDeliveries)
	return 0
}

// pruneHistoryBestEffort removes old history according to the saved policy
// without failing the caller's command. Check, watch, and history reads call
// this so retention is automatic; only durable store unavailability is
// ignored here and surfaced by the caller's own queries.
func pruneHistoryBestEffort(storage *store.Store) {
	if storage == nil {
		return
	}
	_, _ = storage.PruneHistory(time.Now().UTC())
}

func checkHistoryLimit(command string, settings options, stderr io.Writer, jsonOutput bool) (int, bool) {
	raw := strings.TrimSpace(settings.historyLimitRaw)
	if raw == "" {
		return 100, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 1000 {
		writeError(stderr, command, "invalid_arguments", "limit must be 1..1000", jsonOutput)
		return 0, false
	}
	return limit, true
}

func validRunStatus(status string) bool {
	switch status {
	case store.ObservationRunRunning, store.ObservationRunComplete, store.ObservationRunFailed, store.ObservationRunPartial, store.ObservationRunCancelled, store.ObservationRunConflicting, store.ObservationRunStale:
		return true
	default:
		return false
	}
}

func reportHistoryError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	switch {
	case errors.Is(err, store.ErrHistoryInvalid),
		errors.Is(err, store.ErrObservationRunInvalid),
		errors.Is(err, store.ErrDeliveryInvalid),
		errors.Is(err, store.ErrDeliveryNotFound),
		errors.Is(err, store.ErrIncidentInvalid),
		errors.Is(err, store.ErrIncidentNotFound),
		errors.Is(err, store.ErrProfileInvalid),
		errors.Is(err, store.ErrProfileNotFound),
		errors.Is(err, store.ErrAccountInvalid),
		errors.Is(err, store.ErrAccountNotFound),
		errors.Is(err, store.ErrDestinationInvalid),
		errors.Is(err, store.ErrDestinationNotFound):
		writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
		return 2
	default:
		return reportStoreError(stderr, command, err, jsonOutput)
	}
}

func formatHistoryRun(run store.ObservationRun) string {
	if run.Status == store.ObservationRunComplete {
		return fmt.Sprintf("%s %s %s slots:%d", run.StartedAt, run.ProfileID, run.Status, run.SlotCount)
	}
	if run.ErrorCode != "" {
		return fmt.Sprintf("%s %s %s %s", run.StartedAt, run.ProfileID, run.Status, run.ErrorCode)
	}
	return fmt.Sprintf("%s %s %s", run.StartedAt, run.ProfileID, run.Status)
}

func formatHistoryEpisode(episode store.AvailabilityEpisode) string {
	state := "ended"
	if episode.Active {
		state = "active"
	}
	return fmt.Sprintf("%s %s %s %s %s %s", episode.StartedAt, episode.ProfileID, state, episode.Time, episode.Doctor, episode.SlotIdentity)
}

func formatHistoryIncident(incident store.Incident) string {
	return fmt.Sprintf("%s %s %s %s failures:%d %s", incident.FirstSeenAt, incident.ScopeType, incident.ScopeID, incident.Status, incident.ConsecutiveFailures, incident.FailureCode)
}

func formatHistoryDelivery(delivery store.Delivery) string {
	return fmt.Sprintf("%s %s %s %s attempts:%d", delivery.CreatedAt, delivery.ProfileID, delivery.DestinationID, delivery.Status, delivery.Attempts)
}

// checkHistoryFlags rejects flags that a history subcommand does not consume
// so typos fail instead of being silently ignored. --database, --output,
// --non-interactive, and --session-dir remain global.
func checkHistoryFlags(command string, settings options) error {
	if command == "history" {
		return nil
	}
	// Domain flags that history never consumes.
	switch {
	case settings.username != "":
		return fmt.Errorf("--username is not supported for %s", command)
	case settings.passwordFile != "":
		return fmt.Errorf("--password-file is not supported for %s", command)
	case settings.passwordPrompt:
		return fmt.Errorf("--password-prompt is not supported for %s", command)
	case settings.noStoredPassword:
		return fmt.Errorf("--no-stored-password is not supported for %s", command)
	case settings.mfaCodeFile != "":
		return fmt.Errorf("--mfa-code-file is not supported for %s", command)
	case settings.forgetSecret:
		return fmt.Errorf("--forget-secret is not supported for %s", command)
	case settings.region != "":
		return fmt.Errorf("--region is not supported for %s", command)
	case settings.specialty != "":
		return fmt.Errorf("--specialty is not supported for %s", command)
	case settings.clinic != "":
		return fmt.Errorf("--clinic is not supported for %s", command)
	case settings.doctor != "":
		return fmt.Errorf("--doctor is not supported for %s", command)
	case settings.language != "":
		return fmt.Errorf("--language is not supported for %s", command)
	case settings.visitType != "":
		return fmt.Errorf("--visit-type is not supported for %s", command)
	case settings.searchType != "":
		return fmt.Errorf("--search-type is not supported for %s", command)
	case settings.startDate != "":
		return fmt.Errorf("--start-date is not supported for %s", command)
	case settings.endDate != "":
		return fmt.Errorf("--end-date is not supported for %s", command)
	case settings.checkIntervalRaw != "":
		return fmt.Errorf("--check-interval-minutes is not supported for %s", command)
	case settings.profileDisabled:
		return fmt.Errorf("--disabled is not supported for %s", command)
	case settings.clearClinic:
		return fmt.Errorf("--clear-clinic is not supported for %s", command)
	case settings.clearDoctor:
		return fmt.Errorf("--clear-doctor is not supported for %s", command)
	case settings.clearLanguage:
		return fmt.Errorf("--clear-language is not supported for %s", command)
	case settings.clearVisitType:
		return fmt.Errorf("--clear-visit-type is not supported for %s", command)
	case settings.clearSearchType:
		return fmt.Errorf("--clear-search-type is not supported for %s", command)
	case settings.clearStartDate:
		return fmt.Errorf("--clear-start-date is not supported for %s", command)
	case settings.clearEndDate:
		return fmt.Errorf("--clear-end-date is not supported for %s", command)
	case settings.telegramIDsRaw != "" && command != "history incident-deliveries":
		// history incident-deliveries uses --incident, never --telegram.
		// Other history commands use --profile/--account, never --telegram.
		return fmt.Errorf("--telegram is not supported for %s", command)
	case settings.telegramName != "":
		return fmt.Errorf("--name is not supported for %s", command)
	case settings.chatID != "":
		return fmt.Errorf("--chat-id is not supported for %s", command)
	case settings.tokenFile != "":
		return fmt.Errorf("--token-file is not supported for %s", command)
	case settings.tokenPrompt:
		return fmt.Errorf("--token-prompt is not supported for %s", command)
	case settings.noStoredToken:
		return fmt.Errorf("--no-stored-token is not supported for %s", command)
	case settings.clearTelegram:
		return fmt.Errorf("--clear-telegram is not supported for %s", command)
	case settings.watchOnce:
		return fmt.Errorf("--once is not supported for %s", command)
	case settings.maxIterationsRaw != "":
		return fmt.Errorf("--max-iterations is not supported for %s", command)
	case settings.pollIntervalRaw != "":
		return fmt.Errorf("--poll-interval is not supported for %s", command)
	case settings.dry:
		return fmt.Errorf("--dry is not supported for %s", command)
	}
	switch command {
	case "history runs":
		switch {
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s", command)
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s (use --profile)", command)
		}
	case "history episodes":
		switch {
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s", command)
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s (use --profile)", command)
		}
	case "history incidents":
		switch {
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s (use --scope and --status)", command)
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s (use --scope and --status)", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s", command)
		case settings.telegramIDsRaw != "":
			return fmt.Errorf("--telegram is not supported for %s", command)
		}
	case "history deliveries":
		switch {
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s", command)
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s (use --profile)", command)
		}
	case "history incident-deliveries":
		switch {
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s (use --incident)", command)
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s (use --incident)", command)
		case settings.historyStatus != "":
			return fmt.Errorf("--status is not supported for %s", command)
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s", command)
		case settings.historyLimitRaw != "" && false:
			// Limit is allowed; placeholder keeps the switch exhaustive.
		}
		if strings.TrimSpace(settings.telegramIDsRaw) != "" {
			return fmt.Errorf("--telegram is not supported for %s", command)
		}
	case "history status":
		switch {
		case settings.historyLimitRaw != "":
			return fmt.Errorf("--limit is not supported for %s", command)
		case settings.historyStatus != "":
			return fmt.Errorf("--status is not supported for %s", command)
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s", command)
		}
	case "history retention":
		switch {
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s", command)
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		case settings.historyLimitRaw != "":
			return fmt.Errorf("--limit is not supported for %s", command)
		case settings.historyStatus != "":
			return fmt.Errorf("--status is not supported for %s", command)
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		}
	case "history prune":
		switch {
		case settings.accountID != "":
			return fmt.Errorf("--account is not supported for %s", command)
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		case settings.historyLimitRaw != "":
			return fmt.Errorf("--limit is not supported for %s", command)
		case settings.historyStatus != "":
			return fmt.Errorf("--status is not supported for %s", command)
		case settings.historyScope != "":
			return fmt.Errorf("--scope is not supported for %s", command)
		case settings.historyIncidentID != "":
			return fmt.Errorf("--incident is not supported for %s", command)
		case settings.retentionDaysRaw != "":
			return fmt.Errorf("--retention-days is not supported for %s (use history retention to change the policy)", command)
		}
	}
	return nil
}

// checkHistoryFlagsForNonHistoryCommands rejects history-only flags for
// commands that do not consume them.
func checkHistoryFlagsForNonHistoryCommands(command string, settings options) error {
	switch {
	case settings.historyLimitRaw != "":
		return fmt.Errorf("--limit is not supported for %s", command)
	case settings.historyStatus != "":
		return fmt.Errorf("--status is not supported for %s", command)
	case settings.historyScope != "":
		return fmt.Errorf("--scope is not supported for %s", command)
	case settings.historyIncidentID != "":
		return fmt.Errorf("--incident is not supported for %s", command)
	case settings.retentionDaysRaw != "":
		return fmt.Errorf("--retention-days is not supported for %s", command)
	}
	return nil
}

// checkDoctorFlags rejects history and domain flags that doctor does not
// consume. Doctor is a read-only diagnostic: it never prompts and never
// changes history.
func checkDoctorFlags(command string, settings options) error {
	return checkHistoryFlagsForNonHistoryCommands(command, settings)
}

// runDoctor reports database state plus operator diagnostics. It shares the
// history status contract so automation parses one shape: database fields at
// the top level plus retention, summary, and required actions. It never
// prompts and always exits 0 on a successful inspection, even when actions
// are required.
func runDoctor(command string, settings options, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	if len(settings.positionals) > 0 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for doctor", jsonOutput)
		return 2
	}
	status, inspectErr := store.Inspect(settings.database)
	if inspectErr != nil {
		return reportStoreError(stderr, command, inspectErr, jsonOutput)
	}
	if status.MigrationRequired || !status.Exists {
		if jsonOutput {
			writeResult(stdout, command, status)
			return 0
		}
		if status.MigrationRequired {
			fmt.Fprintf(stdout, "Database migration to schema %d is required.\n", status.RequiredVersion)
		} else {
			fmt.Fprintf(stdout, "Database schema %d is supported.\n", status.SchemaVersion)
		}
		return 0
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	diagnostics, err := collectHistoryStatusForIssuer(storage, sessionStoreFor(settings), settings.medicoverBaseURL)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{
			"exists":                  status.Exists,
			"schema_version":          status.SchemaVersion,
			"required_schema_version": status.RequiredVersion,
			"migration_required":      status.MigrationRequired,
			"retention_days":          diagnostics.RetentionDays,
			"summary":                 diagnostics.Summary,
			"required_actions":        diagnostics.RequiredActions,
		})
		return 0
	}
	if status.MigrationRequired {
		fmt.Fprintf(stdout, "Database migration to schema %d is required.\n", status.RequiredVersion)
	} else {
		fmt.Fprintf(stdout, "Database schema %d is supported.\n", status.SchemaVersion)
	}
	if len(diagnostics.RequiredActions) == 0 {
		fmt.Fprintln(stdout, "No actions required.")
	} else {
		fmt.Fprintf(stdout, "%d action(s) required:\n", len(diagnostics.RequiredActions))
		for _, action := range diagnostics.RequiredActions {
			fmt.Fprintf(stdout, "- %s %s %s: %s\n", action.Code, action.Scope, action.ID, action.Message)
		}
	}
	return 0
}
