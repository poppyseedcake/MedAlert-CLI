package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/tui"
	"github.com/zalando/go-keyring"
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
		Enabled: false,
	}
	result := terminalAction(context.Background(), settings, tui.Request{Action: "profile-create", ID: "morning", Profile: profile}, nil)
	if result.Failed || len(result.Snapshot.Profiles) != 1 || result.Snapshot.ProfileValues["morning"].AccountID != "home" || !result.Snapshot.ProfileValues["morning"].Enabled {
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

func TestTerminalProfileChecksUseTerminalPrompt(t *testing.T) {
	fake, closeServer := newMinimalFake(t)
	defer closeServer()
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
		ID: "home", Username: "plain-user@example.com", PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(store.Profile{
		ID: "morning", AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132",
		SearchType: store.SearchTypeStandard, CheckIntervalMinutes: 30, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	oldRun, err := storage.BeginObservationRun("morning", time.Now().UTC().Add(-48*time.Hour))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.FailObservationRun(oldRun.ID, store.ObservationRunFailed, "temporary_failure", "old test run", time.Now().UTC().Add(-48*time.Hour)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.SetHistoryRetentionDays(1); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	settings := options{
		database:         database,
		sessionDir:       filepath.Join(root, "sessions"),
		medicoverBaseURL: fake.baseURL,
	}
	prompted := []string{}
	prompt := func(_ context.Context, kind string) (string, error) {
		prompted = append(prompted, kind)
		if kind != "password" {
			t.Fatalf("unexpected prompt kind %q", kind)
		}
		return "plain-pass", nil
	}

	result := terminalAction(context.Background(), settings, tui.Request{Action: "profile-dry-check", ID: "morning"}, prompt)
	if result.Failed || !strings.Contains(result.Message, "Historia i dostarczanie nie zostały zmienione") {
		t.Fatalf("dry check result = %+v", result)
	}
	if len(prompted) != 1 || prompted[0] != "password" {
		t.Fatalf("dry check prompts = %#v", prompted)
	}
	if _, err := os.Stat(filepath.Join(settings.sessionDir, "sessions", "home.json")); !os.IsNotExist(err) {
		t.Fatalf("dry check saved session: %v", err)
	}
	storage, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := storage.ListObservationRuns("morning")
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("dry check pruned history: %d runs remain, want 1", len(runs))
	}

	prompted = nil
	result = terminalAction(context.Background(), settings, tui.Request{Action: "profile-check", ID: "morning"}, prompt)
	if result.Failed || result.Message != "Wykonano trwałą kontrolę profilu." {
		t.Fatalf("durable check result = %+v", result)
	}
	if len(prompted) != 1 || prompted[0] != "password" {
		t.Fatalf("durable check prompts = %#v", prompted)
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
		"! Aktywny problem: profil morning",
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

func TestTerminalSnapshotShowsCompletePolishHistory(t *testing.T) {
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
	if _, err := storage.CreateDestination(store.Destination{
		ID: "phone", Name: "Telefon", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(store.Profile{
		ID: "morning", AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132",
		SearchType: store.SearchTypeStandard, CheckIntervalMinutes: 30, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("morning", []string{"phone"}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	runAt := time.Now().UTC().Add(-2 * time.Hour)
	run, err := storage.BeginObservationRun("morning", runAt)
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{{
		Identity: "slot-1", StableIdentity: "slot-1", Time: time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
		Doctor: "Dr Kowalski", Specialty: "Kardiologia", Clinic: "Centrum", VisitType: "Standard",
	}}, runAt.Add(time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	episodes, err := storage.ListAvailabilityEpisodes("morning")
	if err != nil || len(episodes) != 1 {
		storage.Close()
		t.Fatalf("episodes = %#v, err=%v", episodes, err)
	}
	if _, err := storage.EnsureEpisodeDeliveries("morning", episodes[0].ID, runAt.Add(time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	deliveries, err := storage.ListDeliveriesForProfile("morning")
	if err != nil || len(deliveries) != 1 {
		storage.Close()
		t.Fatalf("deliveries = %#v, err=%v", deliveries, err)
	}
	claimedDelivery, err := storage.BeginDeliveryAttempt(deliveries[0].ID, runAt.Add(2*time.Minute))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.RecordDeliveryResult(claimedDelivery.ID, claimedDelivery, store.DeliveryResult{
		Status: store.DeliveryPermanentFailure, LastError: "permanent_failure: chat not found",
	}, runAt.Add(3*time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, _, _, err := storage.RecordIncidentFailure(
		store.IncidentScopeProfile, "morning", "home", "morning", "", "protocol_changed", "portal changed", runAt, []string{"phone"},
	); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	incidents, err := storage.ListIncidents(store.IncidentStatusActive, "", 10)
	if err != nil || len(incidents) != 1 {
		storage.Close()
		t.Fatalf("incidents = %#v, err=%v", incidents, err)
	}
	incidentDeliveries, err := storage.ListIncidentDeliveries(incidents[0].ID)
	if err != nil || len(incidentDeliveries) != 1 {
		storage.Close()
		t.Fatalf("incident deliveries = %#v, err=%v", incidentDeliveries, err)
	}
	claimed, err := storage.BeginIncidentDeliveryAttempt(incidentDeliveries[0].ID, runAt.Add(2*time.Minute))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 7}, runAt.Add(3*time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, _, _, err := storage.ResolveIncident(store.IncidentScopeProfile, "morning", "morning", "", runAt.Add(4*time.Minute)); err != nil {
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
	var history strings.Builder
	for _, row := range snapshot.History {
		history.WriteString(row.Label)
		history.WriteByte(' ')
		history.WriteString(row.Detail)
		history.WriteByte('\n')
	}
	view := history.String()
	for _, want := range []string{"Przebieg", "Dostępność", "Incydent", "Dostarczenie Telegram", "Powiadomienie incydentu", "Kardiologia", "protocol_changed", "recovery"} {
		if !strings.Contains(view, want) {
			t.Fatalf("history misses %q: %s", want, view)
		}
	}
	foundDestinationAction := false
	for _, action := range snapshot.Actions {
		if action.ID == "phone" && action.Target == 3 {
			foundDestinationAction = true
		}
	}
	if !foundDestinationAction {
		t.Fatalf("permanent delivery did not target Telegram destination: %#v", snapshot.Actions)
	}
}

func TestTerminalTelegramActionsUseSafeApplicationFlow(t *testing.T) {
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
	if _, err := storage.CreateAccount(store.Account{ID: "home", Username: "patient@example.com", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(store.Profile{
		ID: "morning", AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132",
		SearchType: store.SearchTypeStandard, CheckIntervalMinutes: 30, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	tokenMarker := "TUI_TELEGRAM_TOKEN_MARKER_34"
	tokenFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokenFile, []byte(tokenMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 34}})
	}))
	defer server.Close()
	settings := options{database: database, sessionDir: filepath.Join(root, "sessions"), telegramBaseURL: server.URL}

	result := terminalAction(context.Background(), settings, tui.Request{
		Action: "telegram-create", ID: "phone",
		Destination: tui.DestinationValues{Name: "Telefon", ChatID: "123", TokenFile: tokenFile},
	}, nil)
	if result.Failed {
		t.Fatalf("create result = %+v", result)
	}
	if len(result.Snapshot.Destinations) != 1 || !strings.Contains(result.Snapshot.Destinations[0].Label, "Telefon") {
		t.Fatalf("created destinations = %#v", result.Snapshot.Destinations)
	}

	result = terminalAction(context.Background(), settings, tui.Request{
		Action: "telegram-link", ID: "phone",
		Destination: tui.DestinationValues{LinkedProfiles: "morning"},
	}, nil)
	if result.Failed {
		t.Fatalf("link result = %+v", result)
	}
	if got := result.Snapshot.DestinationValues["phone"].LinkedProfiles; got != "morning" {
		t.Fatalf("linked profiles = %q, want morning", got)
	}

	result = terminalAction(context.Background(), settings, tui.Request{Action: "telegram-test", ID: "phone"}, nil)
	if result.Failed || !strings.Contains(result.Message, "test") && !strings.Contains(result.Message, "testową") {
		t.Fatalf("test result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "telegram-disable", ID: "phone"}, nil)
	if result.Failed || result.Snapshot.DestinationValues["phone"].Enabled {
		t.Fatalf("disable result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "telegram-enable", ID: "phone"}, nil)
	if result.Failed || !result.Snapshot.DestinationValues["phone"].Enabled {
		t.Fatalf("enable result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{
		Action: "telegram-edit", ID: "phone",
		Destination: tui.DestinationValues{Name: "Telefon domowy", ChatID: "456"},
	}, nil)
	if result.Failed || !strings.Contains(result.Snapshot.Destinations[0].Label, "Telefon domowy") {
		t.Fatalf("edit result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "telegram-delete", ID: "phone"}, nil)
	if result.Failed || len(result.Snapshot.Destinations) != 0 {
		t.Fatalf("delete result = %+v", result)
	}

	if strings.Contains(result.Message, tokenMarker) {
		t.Fatal("telegram token leaked in TUI result")
	}
}

func TestTerminalTelegramPromptKeepsTokenHidden(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	d := newTerminalDriver(t, options{database: database, sessionDir: filepath.Join(root, "sessions")})
	d.text("4")
	d.text("a")
	d.text("phone")
	d.press(tea.KeyTab)
	d.text("Telefon")
	d.press(tea.KeyTab)
	d.text("123")
	d.press(tea.KeyTab)
	d.press(tea.KeyTab)
	d.press(tea.KeyEnter)
	d.until("Podaj token bota Telegram")
	tokenMarker := "TUI_HIDDEN_TELEGRAM_TOKEN_MARKER_34"
	d.text(tokenMarker)
	if strings.Contains(d.model.View(), tokenMarker) {
		t.Fatal("telegram token visible while entering it")
	}
	d.press(tea.KeyEnter)
	d.until("Zapisano cel Telegram.")
	if strings.Contains(d.model.View(), tokenMarker) {
		t.Fatal("telegram token leaked after it was saved")
	}
	stored, err := secrets.GetTelegramToken("phone")
	if err != nil {
		t.Fatal(err)
	}
	if stored != tokenMarker {
		t.Fatalf("stored telegram token = %q, want marker", stored)
	}
}

func TestTerminalTelegramPermanentFailurePointsToDestination(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	tokenMarker := "TUI_PERMANENT_TELEGRAM_TOKEN_MARKER_34"
	tokenFile := filepath.Join(root, "telegram-token")
	if err := os.WriteFile(tokenFile, []byte(tokenMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer server.Close()
	settings := options{database: database, sessionDir: filepath.Join(root, "sessions"), telegramBaseURL: server.URL}
	result := terminalAction(context.Background(), settings, tui.Request{
		Action: "telegram-create", ID: "phone",
		Destination: tui.DestinationValues{Name: "Telefon", ChatID: "123", TokenFile: tokenFile},
	}, nil)
	if result.Failed {
		t.Fatalf("create result = %+v", result)
	}
	result = terminalAction(context.Background(), settings, tui.Request{Action: "telegram-test", ID: "phone"}, nil)
	if !result.Failed || !strings.Contains(result.Message, "phone") || !strings.Contains(result.Message, "Trwały błąd") {
		t.Fatalf("permanent failure result = %+v", result)
	}
	if strings.Contains(result.Message, tokenMarker) {
		t.Fatal("telegram token leaked in permanent failure")
	}
	snapshot, err := terminalSnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.DestinationValues["phone"].LastTestStatus; got != store.DeliveryPermanentFailure {
		t.Fatalf("last test status = %q, want %q", got, store.DeliveryPermanentFailure)
	}
	foundAction := false
	for _, row := range snapshot.Actions {
		if row.ID == "phone" && row.Target == 3 {
			foundAction = true
		}
	}
	if !foundAction {
		t.Fatalf("missing destination action: %#v", snapshot.Actions)
	}
	if strings.Contains(snapshot.Destinations[0].Detail, tokenMarker) {
		t.Fatal("telegram token leaked in destination snapshot")
	}
}
