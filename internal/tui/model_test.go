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
	case "shift+tab":
		msg.Type = tea.KeyShiftTab
	case "ctrl+a":
		msg.Type = tea.KeyCtrlA
	case "ctrl+k":
		msg.Type = tea.KeyCtrlK
	case "down":
		msg.Type = tea.KeyDown
	default:
		var cmd tea.Cmd
		for _, r := range value {
			_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		return cmd
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

func TestProfileFormCoversCriteriaAndRunsChecks(t *testing.T) {
	snapshot := tui.Snapshot{
		Accounts: []tui.Row{{ID: "home", Label: "patient", Detail: "OK — sesja zapisana"}},
		Profiles: []tui.Row{{ID: "morning", Label: "morning — OK — włączony", Detail: "Konto: home"}},
		ProfileValues: map[string]tui.ProfileValues{
			"morning": {
				AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132", ClinicIDs: "",
				DoctorIDs: "", LanguageIDs: "", VisitType: "", SearchType: "Standard",
				StartDate: "", EndDate: "", CheckIntervalMinutes: "30", Enabled: true,
			},
		},
	}
	var requests []tui.Request
	m := tui.New(context.Background(), func(_ context.Context, request tui.Request, _ tui.Prompt) tui.Result {
		requests = append(requests, request)
		result := snapshot
		if request.Action == "profile-create" {
			result.Profiles = append([]tui.Row(nil), snapshot.Profiles...)
			result.Profiles = append(result.Profiles, tui.Row{ID: request.ID, Label: request.ID + " — OK — włączony", Detail: "Konto: " + request.Profile.AccountID})
			result.ProfileValues = map[string]tui.ProfileValues{}
			for id, values := range snapshot.ProfileValues {
				result.ProfileValues[id] = values
			}
			request.Profile.Enabled = true
			result.ProfileValues[request.ID] = request.Profile
		}
		if request.ID == "new-profile" && request.Action != "profile-create" {
			result.Profiles = append([]tui.Row(nil), snapshot.Profiles...)
			result.Profiles = append(result.Profiles, tui.Row{ID: request.ID, Label: request.ID + " — OK — włączony", Detail: "Konto: home"})
			result.ProfileValues = map[string]tui.ProfileValues{
				request.ID: {AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132", SearchType: "Standard", CheckIntervalMinutes: "15", Enabled: true},
			}
		}
		return tui.Result{Snapshot: result, Message: "Operacja zakończona."}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("refresh did not start")
	}
	m.Update(cmd())

	key(m, "3")
	key(m, "a")
	labels := []string{
		"Identyfikator profilu", "Konto Medicover", "Regiony", "Specjalności", "Placówki",
		"Lekarze", "Języki", "Typ wizyty", "Typ wyszukiwania", "Data od", "Data do",
		"Interwał sprawdzania",
	}
	for i, label := range labels {
		if !strings.Contains(m.View(), label) {
			t.Fatalf("profile form misses %q: %s", label, m.View())
		}
		if i < len(labels)-1 {
			key(m, "tab")
		}
	}
	for i := 0; i < len(labels)-1; i++ {
		key(m, "shift+tab")
	}
	for i, value := range []string{"new-profile", "home", "204", "132", "", "", "", "", "Standard", "", "", "15"} {
		if value != "" {
			if i == 8 || i == 11 {
				key(m, "ctrl+a")
				key(m, "ctrl+k")
			}
			key(m, value)
		}
		key(m, "tab")
	}
	cmd = key(m, "enter")
	if cmd == nil {
		t.Fatal("profile save did not start")
	}
	m.Update(cmd())
	if len(requests) != 2 || requests[1].Action != "profile-create" {
		t.Fatalf("profile create requests = %+v", requests)
	}
	created := requests[1]
	if created.ID != "new-profile" || created.Profile.AccountID != "home" || created.Profile.RegionIDs != "204" || created.Profile.SpecialtyIDs != "132" || created.Profile.CheckIntervalMinutes != "15" {
		t.Fatalf("profile create request = %+v", created)
	}

	key(m, "down")
	cmd = key(m, "y")
	if cmd == nil {
		t.Fatal("dry check did not start")
	}
	m.Update(cmd())
	if len(requests) != 3 || requests[2].Action != "profile-dry-check" || requests[2].ID != "new-profile" {
		t.Fatalf("dry check request = %+v", requests)
	}
	cmd = key(m, "k")
	if cmd == nil {
		t.Fatal("durable check did not start")
	}
	m.Update(cmd())
	if len(requests) != 4 || requests[3].Action != "profile-check" || requests[3].ID != "new-profile" {
		t.Fatalf("durable check request = %+v", requests)
	}
}

func TestProfileEditPauseAndDeleteUseSelectedProfile(t *testing.T) {
	snapshot := tui.Snapshot{
		Profiles: []tui.Row{{ID: "morning", Label: "morning — OK — włączony", Detail: "Konto: home"}},
		ProfileValues: map[string]tui.ProfileValues{
			"morning": {
				AccountID: "home", RegionIDs: "204", SpecialtyIDs: "132", ClinicIDs: "12", SearchType: "Standard",
				CheckIntervalMinutes: "30", Enabled: true,
			},
		},
	}
	var requests []tui.Request
	m := tui.New(context.Background(), func(_ context.Context, request tui.Request, _ tui.Prompt) tui.Result {
		requests = append(requests, request)
		return tui.Result{Snapshot: snapshot, Message: "Profil zapisany."}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m.Update(cmd())
	key(m, "3")
	key(m, "e")
	if !strings.Contains(m.View(), "204") || !strings.Contains(m.View(), "132") {
		t.Fatalf("edit form did not load profile values: %s", m.View())
	}
	// Region is the first edit field. Append a second valid Medicover ID and
	// leave the other fields unchanged.
	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	key(m, ",205")
	for i := 0; i < 9; i++ {
		if i == 2 {
			key(m, "ctrl+a")
			key(m, "ctrl+k")
		}
		key(m, "tab")
	}
	if !strings.Contains(m.View(), "30") {
		t.Fatalf("edit form did not keep the interval visible: %s", m.View())
	}
	key(m, "tab")
	cmd = key(m, "enter")
	if cmd == nil {
		t.Fatal("profile edit did not start")
	}
	m.Update(cmd())
	if len(requests) != 2 || requests[1].Action != "profile-edit" || requests[1].Profile.RegionIDs != "204,205" || len(requests[1].ProfileClear) != 1 || requests[1].ProfileClear[0] != "clinic_ids" {
		t.Fatalf("profile edit request = %+v", requests)
	}

	cmd = key(m, "p")
	if cmd == nil {
		t.Fatal("profile pause did not start")
	}
	m.Update(cmd())
	if len(requests) != 3 || requests[2].Action != "profile-disable" || requests[2].ID != "morning" {
		t.Fatalf("profile pause request = %+v", requests)
	}
	key(m, "d")
	if !strings.Contains(m.View(), "Usunąć profil morning") {
		t.Fatalf("profile delete did not ask for confirmation: %s", m.View())
	}
	key(m, "tab")
	cmd = key(m, "enter")
	if cmd == nil {
		t.Fatal("profile delete did not start")
	}
	m.Update(cmd())
	if len(requests) != 4 || requests[3].Action != "profile-delete" {
		t.Fatalf("profile delete request = %+v", requests)
	}
}

func TestMonitoringAndRequiredActionsNavigateToProfileWork(t *testing.T) {
	snapshot := tui.Snapshot{
		Actions:  []tui.Row{{ID: "morning", Label: "! Profil wyłączony: morning", Target: 2}},
		Profiles: []tui.Row{{ID: "morning", Label: "morning — — wyłączony", Detail: "Konto: home"}},
		Monitoring: []tui.Row{
			{ID: "work:morning", Label: "morning — aktywna praca", Detail: "Następny przebieg: za 5 min"},
			{ID: "pause:home", Label: "Konto home — wstrzymane", Detail: "Wymaga logowania"},
			{ID: "incident:1", Label: "! Aktywny problem: morning", Detail: "temporary_failure"},
		},
	}
	m := tui.New(context.Background(), func(_ context.Context, request tui.Request, _ tui.Prompt) tui.Result {
		if request.Action != "refresh" {
			t.Fatalf("request = %+v, want refresh", request)
		}
		return tui.Result{Snapshot: snapshot, Message: "Stan odświeżony."}
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m.Update(cmd())
	key(m, "5")
	for _, text := range []string{"aktywna praca", "Następny przebieg", "wstrzymane", "Aktywny problem"} {
		if !strings.Contains(m.View(), text) {
			t.Fatalf("monitoring misses %q: %s", text, m.View())
		}
	}
	key(m, "1")
	key(m, "enter")
	if !strings.Contains(m.View(), "> morning") {
		t.Fatalf("required action did not select profile: %s", m.View())
	}
}
