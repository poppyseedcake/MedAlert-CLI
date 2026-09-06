package tui_test

import (
	"context"
	"strings"
	"testing"

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
