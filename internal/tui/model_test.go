package tui_test

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/poppyseedcake/MedAlert/internal/tui"
)

func TestStartScreenAndKeyboardNavigation(t *testing.T) {
	m := tui.New(context.Background(), nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := m.View()
	for _, label := range []string{"Stan systemu", "Do zrobienia", "Konta", "Profile", "Telegram", "Monitoring", "Historia"} {
		if !strings.Contains(view, label) {
			t.Fatalf("missing %q in %s", label, view)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !strings.Contains(m.View(), "Konta Medicover") {
		t.Fatal(m.View())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !strings.Contains(m.View(), "Stan systemu") {
		t.Fatal(m.View())
	}
	m.Update(tea.WindowSizeMsg{Width: 79, Height: 24})
	if !strings.Contains(m.View(), "80 kolumn i 24 wiersze") || strings.Contains(m.View(), "Konta") {
		t.Fatal(m.View())
	}
}

func key(m *tui.Model, value string) tea.Cmd {
	var msg tea.KeyMsg
	switch value {
	case "enter":
		msg.Type = tea.KeyEnter
	case "esc":
		msg.Type = tea.KeyEsc
	case "tab":
		msg.Type = tea.KeyTab
	case "down":
		msg.Type = tea.KeyDown
	default:
		for _, r := range value {
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		return nil
	}
	_, cmd := m.Update(msg)
	return cmd
}

func TestAccountFormRequiresSaveAndConfirmsDiscard(t *testing.T) {
	var requests []tui.Request
	m := tui.New(context.Background(), func(_ context.Context, r tui.Request, _ tui.Prompt) tui.Result {
		requests = append(requests, r)
		return tui.Result{Message: "Zapisano konto.", Snapshot: tui.Snapshot{Accounts: []tui.Row{{ID: r.ID, Label: r.ID}}}}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	key(m, "2")
	key(m, "a")
	key(m, "home")
	key(m, "tab")
	key(m, "patient")
	key(m, "esc")
	if !strings.Contains(m.View(), "Odrzucić zmiany?") {
		t.Fatal(m.View())
	}
	key(m, "esc")
	if !strings.Contains(m.View(), "patient") {
		t.Fatal("discard cancel lost input")
	}
	key(m, "tab")
	key(m, "tab")
	cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("save did not start")
	}
	m.Update(cmd())
	if len(requests) != 1 || requests[0].Action != "create" || requests[0].ID != "home" || requests[0].Username != "patient" {
		t.Fatalf("requests = %+v", requests)
	}
	if !strings.Contains(m.View(), "Zapisano konto.") {
		t.Fatal(m.View())
	}
	key(m, "d")
	if !strings.Contains(m.View(), "Usunąć konto") {
		t.Fatal(m.View())
	}
	key(m, "esc")
	if len(requests) != 1 {
		t.Fatal("delete ran before confirmation")
	}
}

func TestEscReleasesBusyBeforeCancelledOperationReturns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	m := tui.New(context.Background(), func(ctx context.Context, _ tui.Request, _ tui.Prompt) tui.Result {
		calls++
		close(started)
		<-ctx.Done()
		<-release
		return tui.Result{Failed: true, Message: "Anulowano operację."}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	key(m, "2")
	key(m, "a")
	key(m, "home")
	key(m, "tab")
	key(m, "patient")
	key(m, "tab")
	key(m, "tab")
	cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("save did not start")
	}

	finished := make(chan tea.Msg, 1)
	go func() { finished <- cmd() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}

	key(m, "esc")
	key(m, "a")
	if !strings.Contains(m.View(), "Dodaj konto") {
		t.Fatal("TUI stayed busy while the cancelled operation was blocked")
	}
	if calls != 1 {
		t.Fatalf("execute calls = %d, want 1", calls)
	}
	key(m, "esc")

	close(release)
	select {
	case msg := <-finished:
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("cancelled operation did not return")
	}
	key(m, "a")
	if !strings.Contains(m.View(), "Dodaj konto") {
		t.Fatal("TUI stayed busy after the operation returned")
	}
}

func TestCancelledOperationSerializesTheSameAccount(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	loginCalls := 0
	snapshot := tui.Snapshot{Accounts: []tui.Row{{ID: "home", Label: "patient", Detail: "Wymaga logowania"}}}
	m := tui.New(context.Background(), func(ctx context.Context, request tui.Request, _ tui.Prompt) tui.Result {
		switch request.Action {
		case "refresh":
			return tui.Result{Snapshot: snapshot, Message: "Stan odświeżony."}
		case "login":
			loginCalls++
			if loginCalls == 1 {
				close(started)
				<-ctx.Done()
				<-release
				return tui.Result{Failed: true, Message: "Anulowano operację."}
			}
			return tui.Result{Snapshot: snapshot, Message: "Zalogowano konto."}
		default:
			t.Fatalf("unexpected request: %+v", request)
			return tui.Result{}
		}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("refresh did not start")
	}
	m.Update(cmd())
	key(m, "2")
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if cmd == nil {
		t.Fatal("login did not start")
	}

	finished := make(chan tea.Msg, 1)
	go func() { finished <- cmd() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("login did not start")
	}

	key(m, "esc")
	_, retry := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if retry != nil || loginCalls != 1 || !strings.Contains(m.View(), "nadal się kończy") {
		t.Fatalf("same-account operation was not serialized: calls=%d view=%s", loginCalls, m.View())
	}

	close(release)
	select {
	case msg := <-finished:
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("cancelled login did not return")
	}
	_, retry = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if retry == nil {
		t.Fatal("same-account operation was not released")
	}
	m.Update(retry())
	if loginCalls != 2 || !strings.Contains(m.View(), "Zalogowano konto.") {
		t.Fatalf("retry did not run after the cancelled operation returned: calls=%d view=%s", loginCalls, m.View())
	}
}

func TestGroupedTextDoesNotActivateGlobalKeys(t *testing.T) {
	for _, text := range []string{"ctrl+c", "f1"} {
		t.Run(text, func(t *testing.T) {
			m := tui.New(context.Background(), nil)
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			key(m, "2")
			key(m, "a")
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
			if cmd != nil || !strings.Contains(m.View(), text) || !strings.Contains(m.View(), "Dodaj konto") {
				t.Fatalf("text activated a key: %s", m.View())
			}
		})
	}
}

func TestLongInputKeepsCursorAndEditedTailVisible(t *testing.T) {
	m := tui.New(context.Background(), nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	key(m, "2")
	key(m, "a")
	key(m, "home")
	key(m, "tab")
	key(m, "patient")
	key(m, "tab")
	path := "/" + strings.Repeat("long-directory/", 10) + "password-file"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(path)})
	if !strings.Contains(m.View(), "password-file▏") {
		t.Fatalf("tail or cursor hidden: %s", m.View())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	if !strings.Contains(m.View(), "▏/long-directory") {
		t.Fatalf("start or cursor hidden: %s", m.View())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if !strings.Contains(m.View(), "password-fil▏") {
		t.Fatalf("edited tail hidden: %s", m.View())
	}
}

func TestRequiredHistoryActionSelectsMatchingHistoryRow(t *testing.T) {
	snapshot := tui.Snapshot{
		Actions: []tui.Row{{ID: "delivery-1", Label: "! Sprawdź błąd dostarczenia: delivery-1", Target: 5}},
		History: []tui.Row{
			{ID: "run-1", Label: "niezwiązany przebieg", Detail: "run"},
			{ID: "delivery-1", Label: "wymagane dostarczenie", Detail: "delivery details"},
		},
	}
	m := tui.New(context.Background(), func(_ context.Context, r tui.Request, _ tui.Prompt) tui.Result {
		if r.Action != "refresh" {
			t.Fatalf("request = %+v, want refresh", r)
		}
		return tui.Result{Snapshot: snapshot, Message: "Stan odświeżony."}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("refresh did not start")
	}
	m.Update(cmd())

	key(m, "enter")
	if !strings.Contains(m.View(), "> wymagane dostarczenie") {
		t.Fatalf("required action selected the wrong history row: %s", m.View())
	}
	key(m, "enter")
	view := m.View()
	if !strings.Contains(view, "delivery details") || strings.Contains(view, "niezwiązany przebieg") {
		t.Fatalf("history detail is unrelated: %s", view)
	}
}
