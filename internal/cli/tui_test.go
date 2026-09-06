package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
func (d *terminalDriver) until(text string) {
	d.t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for !strings.Contains(d.model.View(), text) {
		select {
		case msg := <-d.messages:
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, cmd := range batch {
					d.schedule(cmd)
				}
			} else {
				_, cmd := d.model.Update(msg)
				d.schedule(cmd)
			}
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
