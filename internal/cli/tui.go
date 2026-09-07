package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/tui"
	"golang.org/x/term"
)

func terminalPair(stdin *os.File, stdout io.Writer) bool {
	output, ok := stdout.(*os.File)
	return ok && stdin != nil && term.IsTerminal(int(stdin.Fd())) && term.IsTerminal(int(output.Fd()))
}

func runTUI(settings options, stdin *os.File, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := tui.New(ctx, func(ctx context.Context, request tui.Request, prompt tui.Prompt) tui.Result {
		return terminalAction(ctx, settings, request, prompt)
	})
	if _, err := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithAltScreen(), tea.WithContext(ctx)).Run(); err != nil {
		fmt.Fprintln(stderr, "Nie można uruchomić interfejsu terminalowego.")
		return 4
	}
	return 0
}

// terminalAction adapts typed UI requests to the existing command use cases.
// Command output stays in memory. Only safe fields and Polish messages reach
// the display model; raw errors, secret references and session data do not.
func terminalAction(ctx context.Context, settings options, request tui.Request, prompt tui.Prompt) tui.Result {
	ctx = contextOrBackground(ctx)
	settings.output, settings.nonInteractive = "json", true
	if strings.HasPrefix(request.Action, "profile-") {
		applyProfileRequest(&settings, request)
	} else {
		settings.accountID, settings.username, settings.passwordFile = request.ID, request.Username, request.PasswordFile
	}
	var stdout, stderr bytes.Buffer
	code := 0
	sessionCleanupFailed := false
	checkResultMessage := ""
	switch request.Action {
	case "refresh":
	case "create", "edit":
		if request.Action == "create" && request.PasswordFile == "" {
			settings.noStoredPassword = true
		}
		code = runAccountWithContext(ctx, "account "+request.Action, settings, nil, &stdout, &stderr)
	case "delete":
		// Session cleanup is best effort here. The database account and its
		// dependent records must still be deleted when Secret Service is down.
		logoutCode := accountLogoutWithContext(ctx, "account logout", settings, &stdout, &stderr, true)
		code = accountDeleteWithContext(ctx, "account delete", settings, &stdout, &stderr, true)
		sessionCleanupFailed = logoutCode != 0 && code == 0
	case "logout":
		code = accountLogoutWithContext(ctx, "account logout", settings, &stdout, &stderr, true)
	case "login":
		code = accountLoginWithPrompt(ctx, "account login", settings, nil, &stdout, &stderr, true, prompt)
	case "profile-create":
		code = runProfile("profile create", settings, &stdout, &stderr)
	case "profile-edit":
		code = runProfile("profile edit", settings, &stdout, &stderr)
	case "profile-enable":
		code = runProfile("profile enable", settings, &stdout, &stderr)
	case "profile-disable":
		code = runProfile("profile disable", settings, &stdout, &stderr)
	case "profile-delete":
		code = runProfile("profile delete", settings, &stdout, &stderr)
	case "profile-dry-check":
		settings.dry = true
		code = runCheckWithContext(ctx, "check", settings, nil, &stdout, &stderr)
		checkResultMessage = dryCheckMessage(request.ID, stdout.Bytes())
	case "profile-check":
		code = runCheckWithContext(ctx, "check", settings, nil, &stdout, &stderr)
		checkResultMessage = "Wykonano trwałą kontrolę profilu."
	default:
		return tui.Result{Failed: true, Message: "Nieznana operacja."}
	}
	if ctx.Err() != nil {
		return tui.Result{Failed: true, Message: "Anulowano operację. Odśwież stan."}
	}
	if code != 0 {
		var envelope struct {
			Error errorBody `json:"error"`
		}
		_ = json.Unmarshal(stderr.Bytes(), &envelope)
		message := terminalError(envelope.Error.Code)
		if strings.HasPrefix(request.Action, "profile-") {
			message = terminalErrorDetails(envelope.Error.Code, envelope.Error.Message)
		}
		return tui.Result{Failed: true, Message: message}
	}
	snapshot, err := terminalSnapshot(settings)
	if err != nil {
		return tui.Result{Failed: true, Message: "Nie można odczytać stanu. Sprawdź dostęp do bazy. R: ponów."}
	}
	if ctx.Err() != nil {
		return tui.Result{Failed: true, Message: "Anulowano operację. Odśwież stan."}
	}
	message := "Stan odświeżony."
	switch request.Action {
	case "create", "edit":
		message = "Zapisano konto."
	case "delete":
		message = "Usunięto konto, jego profile i historię."
		if sessionCleanupFailed {
			message += " Nie udało się usunąć zapisanej sesji; sprawdź magazyn sesji."
		}
	case "logout":
		message = "Wylogowano konto."
	case "login":
		message = "Zalogowano konto. Sesja została zapisana."
	case "profile-create", "profile-edit":
		message = "Zapisano profil."
	case "profile-enable":
		message = "Włączono profil."
	case "profile-disable":
		message = "Wstrzymano profil."
	case "profile-delete":
		message = "Usunięto profil i jego historię."
	case "profile-dry-check", "profile-check":
		if checkResultMessage != "" {
			message = checkResultMessage
		}
	}
	return tui.Result{Snapshot: snapshot, Message: message}
}

func applyProfileRequest(settings *options, request tui.Request) {
	settings.profileID = request.ID
	settings.accountID = request.Profile.AccountID
	settings.region = request.Profile.RegionIDs
	settings.specialty = request.Profile.SpecialtyIDs
	settings.clinic = request.Profile.ClinicIDs
	settings.doctor = request.Profile.DoctorIDs
	settings.language = request.Profile.LanguageIDs
	settings.visitType = request.Profile.VisitType
	settings.searchType = request.Profile.SearchType
	settings.startDate = request.Profile.StartDate
	settings.endDate = request.Profile.EndDate
	settings.checkIntervalRaw = request.Profile.CheckIntervalMinutes
	for _, field := range request.ProfileClear {
		switch field {
		case "clinic_ids":
			settings.clearClinic = true
		case "doctor_ids":
			settings.clearDoctor = true
		case "language_ids":
			settings.clearLanguage = true
		case "visit_type":
			settings.clearVisitType = true
		case "search_type":
			settings.clearSearchType = true
		case "start_date":
			settings.clearStartDate = true
		case "end_date":
			settings.clearEndDate = true
		}
	}
}

func dryCheckMessage(profileID string, raw []byte) string {
	var envelope struct {
		Data struct {
			SlotCount int `json:"slot_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		return fmt.Sprintf("Sucha kontrola profilu %s znalazła %d terminów. Historia i dostarczanie nie zostały zmienione.", profileID, envelope.Data.SlotCount)
	}
	return "Wykonano suchą kontrolę profilu. Historia i dostarczanie nie zostały zmienione."
}

func terminalSnapshot(settings options) (tui.Snapshot, error) {
	snapshot := tui.Snapshot{ProfileValues: map[string]tui.ProfileValues{}}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return snapshot, err
	}
	defer storage.Close()
	pruneHistoryBestEffort(storage)
	status, err := collectHistoryStatusForIssuer(storage, sessionStoreFor(settings), settings.medicoverBaseURL)
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
		snapshot.Accounts = append(snapshot.Accounts, tui.Row{ID: account.ID, Label: account.Username, Detail: state})
		if !account.AuthRequired && !account.Authenticated {
			snapshot.Actions = append(snapshot.Actions, tui.Row{ID: account.ID, Label: "! Sprawdź magazyn sesji: " + account.ID, Target: 1})
		}
	}
	for _, action := range status.RequiredActions {
		label := map[string]string{
			"authentication_required": "Zaloguj konto", "profile_disabled": "Profil wyłączony", "destination_disabled": "Telegram wyłączony",
			"active_incident": "Sprawdź aktywny problem", "permanent_failure": "Sprawdź błąd dostarczenia", "invalid_retention": "Sprawdź okres historii",
		}[action.Code]
		if label == "" {
			label = "Sprawdź stan"
		}
		snapshot.Actions = append(snapshot.Actions, tui.Row{ID: action.ID, Label: "! " + label + ": " + action.ID, Detail: label, Target: terminalActionTarget(action)})
		if action.Code == "invalid_retention" {
			snapshot.History = append(snapshot.History, tui.Row{
				ID:     action.ID,
				Label:  "Nieprawidłowy okres historii",
				Detail: "Ustaw prawidłowy okres w poleceniu history retention.",
			})
		}
	}
	if len(status.Accounts) == 0 {
		snapshot.Actions = append(snapshot.Actions, tui.Row{Label: "! Dodaj konto w obszarze Konta (2, A).", Target: 1})
	}
	profiles, err := storage.ListProfiles("")
	if err != nil {
		return snapshot, err
	}
	for _, profile := range profiles {
		state := "OK — włączony"
		if !profile.Enabled {
			state = "— wyłączony"
		}
		snapshot.Profiles = append(snapshot.Profiles, tui.Row{ID: profile.ID, Label: profile.ID + " — " + state, Detail: "Konto: " + profile.AccountID})
		snapshot.ProfileValues[profile.ID] = tui.ProfileValues{
			AccountID:            profile.AccountID,
			RegionIDs:            profile.RegionIDs,
			SpecialtyIDs:         profile.SpecialtyIDs,
			ClinicIDs:            profile.ClinicIDs,
			DoctorIDs:            profile.DoctorIDs,
			LanguageIDs:          profile.LanguageIDs,
			VisitType:            profile.VisitType,
			SearchType:           profile.SearchType,
			StartDate:            profile.StartDate,
			EndDate:              profile.EndDate,
			CheckIntervalMinutes: fmt.Sprintf("%d", profile.CheckIntervalMinutes),
			Enabled:              profile.Enabled,
		}
		monitoringState := "aktywna praca"
		nextRun := profileNextRun(storage, profile, authRequired[profile.AccountID], time.Now().UTC())
		if !profile.Enabled {
			monitoringState = "wstrzymane: profil wyłączony"
		} else if authRequired[profile.AccountID] {
			monitoringState = "wstrzymane: konto wymaga logowania"
		}
		snapshot.Monitoring = append(snapshot.Monitoring, tui.Row{
			ID:     profile.ID,
			Label:  profile.ID + " — " + monitoringState,
			Detail: fmt.Sprintf("Konto: %s · Następny przebieg: %s", profile.AccountID, nextRun),
		})
	}
	for _, account := range status.Accounts {
		if account.AuthRequired {
			snapshot.Monitoring = append(snapshot.Monitoring, tui.Row{
				ID:     "account:" + account.ID,
				Label:  "Konto " + account.ID + " — wstrzymane",
				Detail: "Wymaga logowania; profile tego konta czekają.",
			})
		}
	}
	for _, incident := range status.ActiveIncidents {
		snapshot.Monitoring = append(snapshot.Monitoring, tui.Row{
			ID:     "incident:" + incident.ID,
			Label:  fmt.Sprintf("! Aktywny problem: %s %s", incident.ScopeType, incident.ScopeID),
			Detail: fmt.Sprintf("%s · Kolejne błędy: %d", incident.FailureCode, incident.ConsecutiveFailures),
		})
	}
	destinations, err := storage.ListDestinations()
	if err != nil {
		return snapshot, err
	}
	for _, destination := range destinations {
		state := "OK — włączony"
		if !destination.Enabled {
			state = "— wyłączony"
		}
		snapshot.Destinations = append(snapshot.Destinations, tui.Row{ID: destination.ID, Label: destination.Name + " — " + state, Detail: "Dane tokenu są ukryte."})
	}
	runs, err := storage.ListRecentObservationRuns(100)
	if err != nil {
		return snapshot, err
	}
	for _, run := range runs {
		state := map[string]string{"running": "w toku", "complete": "ukończony", "failed": "błąd", "partial": "częściowy", "cancelled": "anulowany", "conflicting": "konflikt", "stale": "nieaktualny"}[run.Status]
		row := tui.Row{ID: run.ID, Label: run.ProfileID + " — " + state, Detail: run.StartedAt}
		snapshot.Runs = append(snapshot.Runs, row)
		snapshot.History = append(snapshot.History, row)
	}
	for _, delivery := range status.PermanentFailures {
		snapshot.History = append(snapshot.History, tui.Row{
			ID:    delivery.ID,
			Label: delivery.ProfileID + " — trwały błąd dostarczenia",
			Detail: fmt.Sprintf("Telegram: %s · Próby: %d · Utworzono: %s",
				delivery.DestinationID, delivery.Attempts, delivery.CreatedAt),
		})
	}
	for _, delivery := range status.OperationalDeliveries {
		snapshot.History = append(snapshot.History, tui.Row{
			ID:    delivery.ID,
			Label: "Operacyjne dostarczenie — trwały błąd",
			Detail: fmt.Sprintf("Incydent: %s · Telegram: %s · Typ: %s · Próby: %d · Utworzono: %s",
				delivery.IncidentID, delivery.DestinationID, delivery.Kind, delivery.Attempts, delivery.CreatedAt),
		})
	}
	paused := len(status.DisabledProfiles)
	for _, profile := range profiles {
		if profile.Enabled && authRequired[profile.AccountID] {
			paused++
		}
	}
	snapshot.Summary = fmt.Sprintf("Konta: %d · Profile: %d · Wstrzymane: %d · Problemy: %d", len(snapshot.Accounts), len(snapshot.Profiles), paused, len(status.ActiveIncidents))
	return snapshot, nil
}

func terminalActionTarget(action historyAction) int {
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

func profileNextRun(storage *store.Store, profile store.Profile, authRequired bool, now time.Time) string {
	if !profile.Enabled || authRequired {
		return "wstrzymany"
	}
	last, ok := latestRunStart(storage, profile.ID)
	if !ok {
		return "oczekuje na pierwszy przebieg"
	}
	next := last.Add(time.Duration(profile.CheckIntervalMinutes) * time.Minute)
	if !next.After(now) {
		return "teraz"
	}
	return next.Format("2006-01-02 15:04")
}

func terminalError(code string) string {
	return terminalErrorDetails(code, "")
}

func terminalErrorDetails(code, detail string) string {
	switch code {
	case "account_exists":
		return "Konto już istnieje. Użyj innego identyfikatora."
	case "account_not_found":
		return "Konto nie istnieje. R: odśwież listę."
	case "profile_exists":
		return "Profil już istnieje. Użyj innego identyfikatora."
	case "profile_not_found":
		return "Profil nie istnieje. R: odśwież listę."
	case "profile_disabled":
		return "Profil jest wyłączony. Włącz go klawiszem P."
	case "run_active":
		return "Kontrola tego profilu już trwa. Spróbuj ponownie później."
	case "invalid_arguments":
		if message := polishProfileValidation(detail); message != "" {
			return message
		}
		return "Nieprawidłowe dane. Sprawdź pola lub uprawnienia pliku."
	case "invalid_credentials":
		return "Nieprawidłowe dane logowania lub kod MFA. L: ponów."
	case "authentication_required", "mfa_required":
		return "Wymagane logowanie. L: ponów."
	case "protocol_changed":
		return "Medicover zmienił sposób logowania. Sprawdź aktualizację programu."
	case "secret_error", "missing_input":
		return "Brak dostępu do hasła. Sprawdź plik lub magazyn haseł."
	case "timeout":
		return "Logowanie przekroczyło limit czasu. L: ponów."
	default:
		return "Operacja nie powiodła się. Sprawdź połączenie i magazyn sesji. R: odśwież."
	}
}

func polishProfileValidation(detail string) string {
	lower := strings.ToLower(strings.TrimSpace(detail))
	switch {
	case strings.Contains(lower, "end date must not be before start date"):
		return "Nieprawidłowy zakres dat: Data końcowa nie może być wcześniejsza niż Data początkowa."
	case strings.Contains(lower, "start date"):
		return "Nieprawidłowa data początkowa. Użyj formatu YYYY-MM-DD."
	case strings.Contains(lower, "end date"):
		return "Nieprawidłowa data końcowa. Użyj formatu YYYY-MM-DD."
	case strings.Contains(lower, "check interval"):
		return "Nieprawidłowy interwał. Podaj liczbę od 1 do 43200 minut."
	case strings.Contains(lower, "search type"):
		return "Nieprawidłowy typ wyszukiwania. Użyj Standard albo DiagnosticProcedure."
	case strings.Contains(lower, "visit type"):
		return "Nieprawidłowy typ wizyty. Użyj liter, cyfr, - albo _."
	case strings.Contains(lower, "region"):
		return "Nieprawidłowe regiony. Podaj dodatnie identyfikatory rozdzielone przecinkami."
	case strings.Contains(lower, "specialty"):
		return "Nieprawidłowe specjalności. Podaj dodatnie identyfikatory rozdzielone przecinkami."
	case strings.Contains(lower, "clinic"):
		return "Nieprawidłowe placówki. Podaj dodatnie identyfikatory rozdzielone przecinkami."
	case strings.Contains(lower, "doctor"):
		return "Nieprawidłowi lekarze. Podaj dodatnie identyfikatory rozdzielone przecinkami."
	case strings.Contains(lower, "language"):
		return "Nieprawidłowe języki. Podaj dodatnie identyfikatory rozdzielone przecinkami."
	case strings.Contains(lower, "profile id"):
		return "Nieprawidłowy identyfikator profilu. Użyj liter, cyfr, - albo _."
	case strings.Contains(lower, "account id"):
		return "Nieprawidłowy identyfikator konta. Wybierz istniejące konto."
	default:
		return ""
	}
}
