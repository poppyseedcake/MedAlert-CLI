package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/tui"
)

// This driver sends keyboard events through the public TUI state boundary.
// It runs the actual account use cases with SQLite and a local protocol server.
type terminalDriver struct {
	t        *testing.T
	model    *tui.Model
	messages chan tea.Msg
	ctx      context.Context
}

func newTerminalDriver(t *testing.T, settings options) *terminalDriver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &terminalDriver{t: t, ctx: ctx, messages: make(chan tea.Msg, 32)}
	d.model = tui.New(ctx, func(ctx context.Context, r tui.Request, p tui.Prompt) tui.Result {
		return terminalAction(ctx, settings, r, p)
	})
	d.model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	d.schedule(d.model.Init())
	d.until("Stan odświeżony.")
	return d
}
func (d *terminalDriver) schedule(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		select {
		case d.messages <- msg:
		case <-d.ctx.Done():
		}
	}()
}
func (d *terminalDriver) update(msg tea.Msg) {
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, cmd := range batch {
			d.schedule(cmd)
		}
		return
	}
	_, cmd := d.model.Update(msg)
	d.schedule(cmd)
}
func (d *terminalDriver) next() {
	d.t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	select {
	case msg := <-d.messages:
		d.update(msg)
	case <-timeout.C:
		d.t.Fatal("timed out waiting for TUI operation")
	}
}
func (d *terminalDriver) until(text string) {
	d.t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for !strings.Contains(d.model.View(), text) {
		select {
		case msg := <-d.messages:
			d.update(msg)
		case <-timeout.C:
			d.t.Fatalf("missing %q: %s", text, d.model.View())
		}
	}
}
func (d *terminalDriver) key(keys ...tea.KeyMsg) {
	d.t.Helper()
	for _, msg := range keys {
		_, cmd := d.model.Update(msg)
		d.schedule(cmd)
	}
}
func (d *terminalDriver) text(text string) {
	d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}
func (d *terminalDriver) press(key tea.KeyType) { d.key(tea.KeyMsg{Type: key}) }

func TestTerminalStateAccountLifecycleAndSecretRedaction(t *testing.T) {
	fake, closeServer := newMinimalFake(t)
	defer closeServer()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	settings := options{database: filepath.Join(root, "medalert.db"), sessionDir: filepath.Join(root, "sessions"), medicoverBaseURL: fake.baseURL}
	d := newTerminalDriver(t, settings)
	d.text("2")
	d.text("a")
	d.text("home")
	d.press(tea.KeyTab)
	d.text("mfa-user@example.com")
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Zapisano konto.")
	d.text("l")
	d.until("Podaj hasło")
	d.text("mfa-pass")
	if strings.Contains(d.model.View(), "mfa-pass") {
		t.Fatal("password visible")
	}
	d.press(tea.KeyEnter)
	d.until("Podaj kod MFA")
	d.text("123456")
	if strings.Contains(d.model.View(), "123456") {
		t.Fatal("MFA visible")
	}
	d.press(tea.KeyEnter)
	d.until("Zalogowano konto.")
	d.until("OK — sesja zapisana")
	// Authentication and logout must affect only the selected account.
	d.text("a")
	d.text("other")
	d.press(tea.KeyTab)
	d.text("plain-user@example.com")
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Zapisano konto.")
	d.press(tea.KeyDown)
	d.text("l")
	d.until("Podaj hasło")
	d.text("plain-pass")
	d.press(tea.KeyEnter)
	d.until("Zalogowano konto.")
	if strings.Count(d.model.View(), "OK — sesja zapisana") != 2 {
		t.Fatal("accounts did not keep independent sessions")
	}
	d.text("o")
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Wylogowano konto.")
	if strings.Count(d.model.View(), "OK — sesja zapisana") != 1 {
		t.Fatal("logout changed another account")
	}
	d.text("d")
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Usunięto konto")
	// A saved session must be reused without another password prompt.
	d.text("l")
	d.until("Zalogowano konto.")
	d.text("o")
	d.until("Wylogować konto")
	d.press(tea.KeyEnter) // No is the default.
	if !strings.Contains(d.model.View(), "OK — sesja zapisana") {
		t.Fatal("logout ran without consent")
	}
	d.text("o")
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Wylogowano konto.")
	d.text("l")
	d.until("Podaj hasło")
	d.text("INJECTED_PASSWORD_MARKER_32")
	if strings.Contains(d.model.View(), "INJECTED_PASSWORD_MARKER_32") {
		t.Fatal("secret visible")
	}
	d.press(tea.KeyEsc)
	d.until("Anulowano logowanie.")
	d.next()
	d.text("e")
	d.until("Edytuj konto")
	d.press(tea.KeyCtrlA)
	d.press(tea.KeyCtrlK)
	d.text("plain-user@example.com")
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Zapisano konto.")
	d.press(tea.KeyEnter)
	d.until("plain-user@example.com")
	d.press(tea.KeyEsc)
	d.text("d")
	d.until("Usunąć konto")
	d.press(tea.KeyEsc)
	if strings.Contains(d.model.View(), "Brak kont") {
		t.Fatal("delete cancel removed account")
	}
	d.text("d")
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Usunięto konto")
	d.until("Brak kont")
}

func TestTerminalStateKeepsFailedFormAndHidesProviderErrors(t *testing.T) {
	fake, closeServer := newMinimalFake(t)
	defer closeServer()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	settings := options{database: filepath.Join(root, "db"), sessionDir: filepath.Join(root, "sessions"), medicoverBaseURL: fake.baseURL}
	d := newTerminalDriver(t, settings)
	d.text("2")
	d.text("a")
	d.text("invalid/id")
	d.press(tea.KeyTab)
	d.text("plain-user@example.com")
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Nieprawidłowe dane.")
	if !strings.Contains(d.model.View(), "plain-user@example.com") {
		t.Fatal("failed save lost form")
	}
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyCtrlA)
	d.press(tea.KeyCtrlK)
	d.text("valid")
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Zapisano konto.")
	d.text("l")
	d.until("Podaj hasło")
	d.text("PROVIDER_SECRET_MARKER_32")
	d.press(tea.KeyEnter)
	d.until("Nieprawidłowe dane logowania")
	if strings.Contains(d.model.View(), "PROVIDER_SECRET_MARKER_32") {
		t.Fatal("provider error leaked password")
	}
}

func TestTerminalDeleteRemovesAccountWhenSessionCleanupFails(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(store.Account{
		ID: "alice", Username: "alice@example.com", PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(store.Profile{
		ID: "morning", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132",
		SearchType: store.SearchTypeStandard, CheckIntervalMinutes: 30, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	// A regular file cannot act as the session directory. This models a
	// session backend failure without requiring a live Secret Service.
	sessionPath := filepath.Join(root, "session-store")
	if err := os.WriteFile(sessionPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := terminalAction(context.Background(), options{
		database: database, sessionDir: sessionPath,
	}, tui.Request{Action: "delete", ID: "alice"}, nil)
	if result.Failed {
		t.Fatalf("delete failed: %+v", result)
	}
	if !strings.Contains(result.Message, "Nie udało się usunąć zapisanej sesji") {
		t.Fatalf("delete message = %q, want session warning", result.Message)
	}

	storage, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.GetAccount("alice"); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("account after delete = %v, want not found", err)
	}
	if _, err := storage.GetProfile("morning"); !errors.Is(err, store.ErrProfileNotFound) {
		t.Fatalf("profile after delete = %v, want not found", err)
	}
}

func TestTerminalActionCancellationDoesNotMutateAccounts(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(store.Account{
		ID:             "alice",
		Username:       "alice@example.com",
		PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := terminalAction(ctx, options{database: database}, tui.Request{
		Action:   "create",
		ID:       "bob",
		Username: "bob@example.com",
	}, nil); !result.Failed {
		t.Fatal("cancelled create succeeded")
	}
	if result := terminalAction(ctx, options{database: database}, tui.Request{
		Action: "delete",
		ID:     "alice",
	}, nil); !result.Failed {
		t.Fatal("cancelled delete succeeded")
	}

	storage, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.GetAccount("alice"); err != nil {
		t.Fatalf("account after cancelled delete = %v, want present", err)
	}
	if _, err := storage.GetAccount("bob"); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("account after cancelled create = %v, want not found", err)
	}
}

func TestTerminalTimeoutMessage(t *testing.T) {
	if message := terminalError("timeout"); !strings.Contains(message, "przekroczyło limit czasu") {
		t.Fatalf("timeout message = %q", message)
	}
}

func TestTerminalProfileActionsUseSharedProfileUseCases(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(store.Account{
		ID: "home", Username: "patient@example.com", PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	settings := options{database: database, sessionDir: filepath.Join(root, "sessions")}
	profile := tui.ProfileValues{
		AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132", ClinicIDs: "12", SearchType: "Standard", CheckIntervalMinutes: "30",
		Enabled: true,
	}
	result := terminalAction(context.Background(), settings, tui.Request{Action: "profile-create", ID: "morning", Profile: profile}, nil)
	if result.Failed || len(result.Snapshot.Profiles) != 1 || result.Snapshot.ProfileValues["morning"].AccountID != "home" {
		t.Fatalf("profile create result = %+v", result)
	}

	profile.RegionIDs = "204,205"
	profile.ClinicIDs = ""
	result = terminalAction(context.Background(), settings, tui.Request{Action: "profile-edit", ID: "morning", Profile: profile, ProfileClear: []string{"clinic_ids"}}, nil)
	if result.Failed || result.Snapshot.ProfileValues["morning"].RegionIDs != "204,205" || result.Snapshot.ProfileValues["morning"].ClinicIDs != "" {
		t.Fatalf("profile edit result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "profile-disable", ID: "morning"}, nil)
	if result.Failed || result.Snapshot.ProfileValues["morning"].Enabled {
		t.Fatalf("profile disable result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "profile-enable", ID: "morning"}, nil)
	if result.Failed || !result.Snapshot.ProfileValues["morning"].Enabled {
		t.Fatalf("profile enable result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "profile-delete", ID: "morning"}, nil)
	if result.Failed || len(result.Snapshot.Profiles) != 0 {
		t.Fatalf("profile delete result = %+v", result)
	}
}

func TestTerminalProfileValidationMessageIsPolish(t *testing.T) {
	message := terminalErrorDetails("invalid_arguments", "end date must not be before start date")
	if !strings.Contains(message, "Data końcowa") || !strings.Contains(message, "Data początkowa") {
		t.Fatalf("validation message = %q", message)
	}
}

func TestTerminalSnapshotShowsMonitoringPausesAndProblems(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(store.Account{
		ID: "home", Username: "patient@example.com", PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	profile, err := storage.CreateProfile(store.Profile{
		ID: "morning", AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132",
		SearchType: store.SearchTypeStandard, CheckIntervalMinutes: 30, Enabled: true,
	})
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, _, _, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, profile.ID, profile.AccountID, profile.ID, "", "temporary_failure", "safe failure", time.Now().UTC(), nil); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := terminalSnapshot(options{database: database, sessionDir: filepath.Join(root, "sessions")})
	if err != nil {
		t.Fatal(err)
	}
	monitoringView := ""
	for _, row := range snapshot.Monitoring {
		monitoringView += row.Label + " " + row.Detail + "\n"
	}
	for _, want := range []string{
		"morning — wstrzymane: konto wymaga logowania",
		"Następny przebieg: wstrzymany",
		"Konto home — wstrzymane",
		"! Aktywny problem: profile morning",
	} {
		if !strings.Contains(monitoringView, want) {
			t.Fatalf("monitoring view misses %q: %s", want, monitoringView)
		}
	}
	for _, action := range snapshot.Actions {
		if action.ID == "morning" && action.Target == 2 {
			return
		}
	}
	t.Fatalf("profile required action did not target Profiles: %+v", snapshot.Actions)
}
