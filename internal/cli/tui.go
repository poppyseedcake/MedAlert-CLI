package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/poppyseedcake/MedAlert/internal/application"
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
		if err := terminalProfileApplication(ctx, settings, request, "create"); err != nil {
			return terminalApplicationFailure(ctx, err)
		}
	case "profile-edit":
		if err := terminalProfileApplication(ctx, settings, request, "edit"); err != nil {
			return terminalApplicationFailure(ctx, err)
		}
	case "profile-enable":
		if err := terminalProfileApplication(ctx, settings, request, "enable"); err != nil {
			return terminalApplicationFailure(ctx, err)
		}
	case "profile-disable":
		if err := terminalProfileApplication(ctx, settings, request, "disable"); err != nil {
			return terminalApplicationFailure(ctx, err)
		}
	case "profile-delete":
		if err := terminalProfileApplication(ctx, settings, request, "delete"); err != nil {
			return terminalApplicationFailure(ctx, err)
		}
	case "profile-dry-check":
		settings.dry = true
		checked, err := terminalCheckApplication(ctx, settings, request, prompt)
		if err != nil {
			return terminalApplicationFailure(ctx, err)
		}
		checkResultMessage = fmt.Sprintf("Sucha kontrola profilu %s znalazła %d terminów. Historia i dostarczanie nie zostały zmienione.", request.ID, len(checked.Search.Slots))
	case "profile-check":
		if _, err := terminalCheckApplication(ctx, settings, request, prompt); err != nil {
			return terminalApplicationFailure(ctx, err)
		}
		checkResultMessage = "Wykonano trwałą kontrolę profilu."
	case "telegram-create", "telegram-edit", "telegram-enable", "telegram-disable", "telegram-test", "telegram-token", "telegram-link", "telegram-delete":
		if err := terminalTelegramApplication(ctx, settings, request, prompt); err != nil {
			return terminalRequestFailure(ctx, request, err)
		}
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
	snapshot, err := terminalSnapshotWithContext(ctx, settings, request.Action != "profile-dry-check")
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
	case "telegram-create":
		message = "Zapisano cel Telegram."
	case "telegram-edit":
		message = "Zapisano zmiany celu Telegram."
	case "telegram-enable":
		message = "Włączono cel Telegram."
	case "telegram-disable":
		message = "Wstrzymano cel Telegram."
	case "telegram-test":
		message = "Wysłano wiadomość testową Telegram."
	case "telegram-token":
		message = "Zapisano nowy token celu Telegram."
	case "telegram-link":
		message = "Zaktualizowano powiązania celu Telegram."
	case "telegram-delete":
		message = "Usunięto cel Telegram i jego powiązania."
	}
	return tui.Result{Snapshot: snapshot, Message: message}
}

func terminalTelegramApplication(ctx context.Context, settings options, request tui.Request, prompt tui.Prompt) error {
	profileIDs := []string{}
	if raw := strings.TrimSpace(request.Destination.LinkedProfiles); raw != "" {
		profileIDs = strings.Split(raw, ",")
	}
	values := application.DestinationValues{
		Name:      request.Destination.Name,
		ChatID:    request.Destination.ChatID,
		TokenFile: request.Destination.TokenFile,
		Enabled:   request.Destination.Enabled,
	}
	_, err := application.New(application.Config{
		Database:         settings.database,
		MedicoverBaseURL: settings.medicoverBaseURL,
		TelegramBaseURL:  settings.telegramBaseURL,
	}).Telegram(ctx, application.TelegramRequest{
		Action:     strings.TrimPrefix(request.Action, "telegram-"),
		ID:         request.ID,
		Values:     values,
		ProfileIDs: profileIDs,
	}, prompt)
	return err
}

func terminalProfileApplication(ctx context.Context, settings options, request tui.Request, action string) error {
	values := application.ProfileValues{
		AccountID: request.Profile.AccountID, RegionIDs: request.Profile.RegionIDs,
		SpecialtyIDs: request.Profile.SpecialtyIDs, ClinicIDs: request.Profile.ClinicIDs,
		DoctorIDs: request.Profile.DoctorIDs, LanguageIDs: request.Profile.LanguageIDs,
		VisitType: request.Profile.VisitType, SearchType: request.Profile.SearchType,
		StartDate: request.Profile.StartDate, EndDate: request.Profile.EndDate,
		CheckIntervalMinutes: request.Profile.CheckIntervalMinutes,
		Enabled:              request.Profile.Enabled,
	}
	if action == "create" {
		values.Enabled = true
	}
	_, err := application.New(application.Config{
		Database:         settings.database,
		SessionDir:       settings.sessionDir,
		MedicoverBaseURL: settings.medicoverBaseURL,
	}).Profile(ctx, application.ProfileRequest{
		Action: action, ID: request.ID, Values: values, Clear: request.ProfileClear,
	})
	return err
}

func terminalApplicationFailure(ctx context.Context, err error) tui.Result {
	if ctx.Err() != nil {
		return tui.Result{Failed: true, Message: "Anulowano operację. Odśwież stan."}
	}
	code, detail := application.ErrorInfo(err)
	return tui.Result{Failed: true, Message: terminalErrorDetails(code, detail)}
}

func terminalRequestFailure(ctx context.Context, request tui.Request, err error) tui.Result {
	if ctx.Err() != nil {
		return tui.Result{Failed: true, Message: "Anulowano operację. Odśwież stan."}
	}
	code, detail := application.ErrorInfo(err)
	message := terminalErrorDetails(code, detail)
	if strings.HasPrefix(request.Action, "telegram-") {
		message = terminalTelegramErrorDetails(code, detail)
	}
	if code == "permanent_failure" && strings.TrimSpace(request.ID) != "" {
		message = fmt.Sprintf("Trwały błąd Telegram dla celu %s.", request.ID)
		if strings.TrimSpace(detail) != "" {
			message += " " + strings.TrimSpace(detail)
		}
	}
	return tui.Result{Failed: true, Message: message}
}

func terminalTelegramErrorDetails(code, detail string) string {
	switch code {
	case "secret_error":
		return "Brak dostępu do tokenu bota Telegram. Sprawdź plik lub magazyn sekretów."
	case "missing_input":
		return "Token bota Telegram jest wymagany. Wpisz go ponownie."
	case "timeout":
		return "Operacja Telegram przekroczyła limit czasu. Spróbuj ponownie."
	default:
		return terminalErrorDetails(code, detail)
	}
}

func terminalCheckApplication(ctx context.Context, settings options, request tui.Request, prompt tui.Prompt) (application.CheckResult, error) {
	return application.New(application.Config{
		Database:         settings.database,
		SessionDir:       settings.sessionDir,
		MedicoverBaseURL: settings.medicoverBaseURL,
		TelegramBaseURL:  settings.telegramBaseURL,
	}).Check(ctx, application.CheckRequest{ProfileID: request.ID, Dry: settings.dry, Prompt: prompt})
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

func terminalSnapshot(settings options) (tui.Snapshot, error) {
	return terminalSnapshotWithContext(context.Background(), settings, true)
}

func terminalSnapshotWithPrune(settings options, prune bool) (tui.Snapshot, error) {
	return terminalSnapshotWithContext(context.Background(), settings, prune)
}

func terminalSnapshotWithContext(ctx context.Context, settings options, prune bool) (tui.Snapshot, error) {
	state, err := application.New(application.Config{
		Database:         settings.database,
		SessionDir:       settings.sessionDir,
		MedicoverBaseURL: settings.medicoverBaseURL,
		TelegramBaseURL:  settings.telegramBaseURL,
	}).Snapshot(ctx, prune)
	if err != nil {
		return tui.Snapshot{}, err
	}
	return tui.Snapshot{
		Accounts:          tuiRows(state.Accounts),
		Profiles:          tuiRows(state.Profiles),
		Destinations:      tuiRows(state.Destinations),
		Monitoring:        tuiRows(state.Monitoring),
		Runs:              tuiRows(state.Runs),
		History:           tuiRows(state.History),
		Actions:           tuiRows(state.Actions),
		ProfileValues:     tuiProfileValues(state.ProfileValues),
		DestinationValues: tuiDestinationValues(state.DestinationValues),
		Summary:           state.Summary,
	}, nil
}

func tuiRows(rows []application.Row) []tui.Row {
	converted := make([]tui.Row, 0, len(rows))
	for _, row := range rows {
		converted = append(converted, tui.Row{ID: row.ID, Label: row.Label, Detail: row.Detail, Target: row.Target})
	}
	return converted
}

func tuiProfileValues(values map[string]application.ProfileValues) map[string]tui.ProfileValues {
	converted := make(map[string]tui.ProfileValues, len(values))
	for id, value := range values {
		converted[id] = tui.ProfileValues{
			AccountID: value.AccountID, RegionIDs: value.RegionIDs, SpecialtyIDs: value.SpecialtyIDs,
			ClinicIDs: value.ClinicIDs, DoctorIDs: value.DoctorIDs, LanguageIDs: value.LanguageIDs,
			VisitType: value.VisitType, SearchType: value.SearchType, StartDate: value.StartDate,
			EndDate: value.EndDate, CheckIntervalMinutes: value.CheckIntervalMinutes, Enabled: value.Enabled,
		}
	}
	return converted
}

func tuiDestinationValues(values map[string]application.DestinationValues) map[string]tui.DestinationValues {
	converted := make(map[string]tui.DestinationValues, len(values))
	for id, value := range values {
		converted[id] = tui.DestinationValues{
			Name:           value.Name,
			ChatID:         value.ChatID,
			TokenFile:      value.TokenFile,
			TokenSource:    value.TokenSource,
			LinkedProfiles: strings.Join(value.LinkedProfiles, ","),
			LastTestAt:     value.LastTestAt,
			LastTestStatus: value.LastTestStatus,
			LastTestError:  value.LastTestError,
			Enabled:        value.Enabled,
		}
	}
	return converted
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
	case "destination_exists":
		return "Cel Telegram już istnieje. Użyj innego identyfikatora."
	case "destination_not_found":
		return "Cel Telegram nie istnieje. R: odśwież listę."
	case "profile_disabled":
		return "Profil jest wyłączony. Włącz go klawiszem P."
	case "run_active":
		return "Kontrola tego profilu już trwa. Spróbuj ponownie później."
	case "permanent_failure":
		if detail != "" {
			return "Trwały błąd dostarczenia Telegram: " + detail
		}
		return "Trwały błąd dostarczenia Telegram. Sprawdź cel."
	case "temporary_failure":
		return "Tymczasowy błąd Telegram. Spróbuj ponownie później."
	case "rate_limited":
		return "Telegram zgłosił limit zapytań. Spróbuj później."
	case "unknown_delivery":
		return "Nieznany wynik dostarczenia Telegram. Sprawdź historię."
	case "cancelled":
		return "Anulowano operację."
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
