package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
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
	settings.output, settings.nonInteractive = "json", true
	settings.accountID, settings.username, settings.passwordFile = request.ID, request.Username, request.PasswordFile
	var stdout, stderr bytes.Buffer
	code := 0
	switch request.Action {
	case "refresh":
	case "create", "edit":
		if request.Action == "create" && request.PasswordFile == "" {
			settings.noStoredPassword = true
		}
		code = runAccount("account "+request.Action, settings, nil, &stdout, &stderr)
	case "delete":
		code = accountLogout("account logout", settings, &stdout, &stderr, true)
		if code == 0 {
			code = accountDelete("account delete", settings, &stdout, &stderr, true)
		}
	case "logout":
		code = accountLogout("account logout", settings, &stdout, &stderr, true)
	case "login":
		code = accountLoginWithPrompt(ctx, "account login", settings, nil, &stdout, &stderr, true, prompt)
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
		return tui.Result{Failed: true, Message: terminalError(envelope.Error.Code)}
	}
	snapshot, err := terminalSnapshot(settings)
	if err != nil {
		return tui.Result{Failed: true, Message: "Nie można odczytać stanu. Sprawdź dostęp do bazy. R: ponów."}
	}
	message := "Stan odświeżony."
	switch request.Action {
	case "create", "edit":
		message = "Zapisano konto."
	case "delete":
		message = "Usunięto konto, jego profile i historię."
	case "logout":
		message = "Wylogowano konto."
	case "login":
		message = "Zalogowano konto. Sesja została zapisana."
	}
	return tui.Result{Snapshot: snapshot, Message: message}
}

func terminalSnapshot(settings options) (tui.Snapshot, error) {
	var snapshot tui.Snapshot
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
	for _, account := range status.Accounts {
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
		snapshot.Actions = append(snapshot.Actions, tui.Row{ID: action.ID, Label: "! " + label + ": " + action.ID, Detail: label, Target: map[string]int{"account": 1, "profile": 2, "destination": 3, "delivery": 5, "operational_delivery": 5, "history": 5}[action.Scope]})
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
		snapshot.Runs = append(snapshot.Runs, tui.Row{ID: run.ID, Label: run.ProfileID + " — " + state, Detail: run.StartedAt})
	}
	snapshot.Summary = fmt.Sprintf("Konta: %d · Profile: %d · Problemy: %d", len(snapshot.Accounts), len(snapshot.Profiles), len(status.ActiveIncidents))
	return snapshot, nil
}

func terminalError(code string) string {
	switch code {
	case "account_exists":
		return "Konto już istnieje. Użyj innego identyfikatora."
	case "account_not_found":
		return "Konto nie istnieje. R: odśwież listę."
	case "invalid_arguments":
		return "Nieprawidłowe dane. Sprawdź pola lub uprawnienia pliku."
	case "invalid_credentials":
		return "Nieprawidłowe dane logowania lub kod MFA. L: ponów."
	case "authentication_required", "mfa_required":
		return "Wymagane logowanie. L: ponów."
	case "protocol_changed":
		return "Medicover zmienił sposób logowania. Sprawdź aktualizację programu."
	case "secret_error", "missing_input":
		return "Brak dostępu do hasła. Sprawdź plik lub magazyn haseł."
	default:
		return "Operacja nie powiodła się. Sprawdź połączenie i magazyn sesji. R: odśwież."
	}
}
