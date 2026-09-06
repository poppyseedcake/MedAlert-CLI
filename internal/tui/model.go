// Package tui presents the Polish terminal interface. All operations pass to
// the same use cases as the command interface; this package owns only UI state.
package tui

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const MinWidth, MinHeight = 80, 24

var areas = []string{"Stan", "Konta", "Profile", "Telegram", "Monitoring", "Historia"}
var titles = []string{"Stan systemu", "Konta Medicover", "Profile obserwacji", "Telegram", "Monitoring", "Historia"}

// Row contains display data only. It must never contain secret values.
type Row struct {
	ID, Label, Detail string
	Target            int
}
type Snapshot struct {
	Accounts                     []Row
	Actions                      []Row
	Profiles, Destinations, Runs []Row
	History                      []Row
	Summary                      string
}
type Request struct{ Action, ID, Username, PasswordFile string }
type Result struct {
	Snapshot Snapshot
	Message  string
	Failed   bool
}
type Prompt func(context.Context, string) (string, error)
type Execute func(context.Context, Request, Prompt) Result

type Model struct {
	ctx                           context.Context
	execute                       Execute
	width, height, area, selected int
	snapshot                      Snapshot
	notice                        string
	form                          *form
	pendingForm                   *form
	loaded                        bool
	confirmation                  string
	confirmYes                    bool
	help, detail, busy            bool
	cancel                        context.CancelFunc
	events                        chan tea.Msg
	generation                    int
	activeGeneration              int
}

type form struct {
	title, action, id string
	labels, original  []string
	fields            []textinput.Model
	focus             int
	secret            bool
	reply             chan string
}
type promptMsg struct {
	kind       string
	reply      chan string
	generation int
}
type finishedMsg struct {
	result     Result
	generation int
}

func New(ctx context.Context, execute Execute) *Model {
	return &Model{ctx: ctx, execute: execute, notice: "Wczytywanie stanu...", events: make(chan tea.Msg)}
}
func (m *Model) Init() tea.Cmd { return tea.Batch(m.start(Request{Action: "refresh"}), m.waitPrompt()) }
func (m *Model) waitPrompt() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-m.events:
			return msg
		case <-m.ctx.Done():
			return nil
		}
	}
}
func (m *Model) start(request Request) tea.Cmd {
	if m.execute == nil {
		return nil
	}
	m.busy = true
	m.notice = "Trwa operacja. Esc: anuluj oczekiwanie."
	m.generation++
	generation := m.generation
	m.activeGeneration = generation
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	return func() tea.Msg {
		defer cancel()
		prompt := func(ctx context.Context, kind string) (string, error) {
			reply := make(chan string, 1)
			select {
			case m.events <- promptMsg{kind, reply, generation}:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			select {
			case value := <-reply:
				return value, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return finishedMsg{m.execute(ctx, request, prompt), generation}
	}
}
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case promptMsg:
		if msg.generation == m.generation && m.busy {
			title := "Podaj hasło"
			if msg.kind == "mfa" {
				title = "Podaj kod MFA"
			}
			m.openForm(title, "secret", "", []string{title}, []string{""})
			m.form.secret, m.form.reply = true, msg.reply
			m.form.fields[0].EchoMode = textinput.EchoNone
			m.notice = "Wpisz dane. Enter: wyślij. Esc: anuluj logowanie."
		}
		return m, m.waitPrompt()
	case finishedMsg:
		if msg.generation != m.activeGeneration {
			return m, nil
		}
		cancelled := msg.generation != m.generation
		m.busy, m.cancel = false, nil
		m.activeGeneration = 0
		if cancelled {
			// A cancelled operation may have completed after Esc. Its result
			// must not replace the current snapshot or restore an old form.
			return m, nil
		}
		if !msg.result.Failed {
			selectedID := m.selectedID()
			m.snapshot = msg.result.Snapshot
			m.loaded = true
			m.selected = 0
			for i, row := range m.rows() {
				if row.ID == selectedID {
					m.selected = i
				}
			}
			m.form = nil
		} else {
			m.form = m.pendingForm
		}
		m.pendingForm = nil
		m.notice = msg.result.Message
		return m, nil
	case tea.KeyMsg:
		key := strings.ToLower(msg.String())
		if msg.Type == tea.KeyCtrlC {
			m.stop()
			return m, tea.Quit
		}
		if m.width < MinWidth || m.height < MinHeight {
			return m, nil
		}
		if m.help {
			if key == "esc" || key == "?" || key == "enter" || key == "f1" {
				m.help = false
			}
			return m, nil
		}
		if msg.Type == tea.KeyF1 {
			m.help = true
			return m, nil
		}
		if m.confirmation != "" {
			return m, m.confirm(key)
		}
		if m.form != nil {
			return m, m.updateForm(msg)
		}
		if key == "?" {
			m.help = true
			return m, nil
		}
		if m.busy {
			if key == "esc" {
				m.stop()
				m.notice = "Anulowano oczekiwanie. R: odśwież stan."
			}
			return m, nil
		}
		switch key {
		case "q":
			return m, tea.Quit
		case "esc":
			if m.detail {
				m.detail = false
			} else {
				m.area, m.selected = 0, 0
			}
		case "up":
			m.selected = max(0, m.selected-1)
		case "down":
			m.selected = min(max(0, len(m.rows())-1), m.selected+1)
		case "pgup":
			m.selected = max(0, m.selected-10)
		case "pgdown":
			m.selected = min(max(0, len(m.rows())-1), m.selected+10)
		case "r":
			return m, m.start(Request{Action: "refresh"})
		case "enter":
			if m.area == 0 && len(m.rows()) > 0 {
				target := m.rows()[m.selected]
				m.area, m.selected = target.Target, 0
				for i, row := range m.rows() {
					if row.ID == target.ID {
						m.selected = i
					}
				}
			} else {
				m.detail = true
			}
		case "a":
			if m.area == 1 {
				m.openForm("Dodaj konto", "create", "", []string{"Identyfikator (litery, cyfry, - lub _)", "Login Medicover", "Plik hasła (pusty: pytaj przy logowaniu)"}, []string{"", "", ""})
			}
		case "e":
			if m.area == 1 && m.selectedID() != "" {
				m.openForm("Edytuj konto", "edit", m.selectedID(), []string{"Login Medicover", "Plik hasła (pusty: zachowaj źródło)"}, []string{m.rows()[m.selected].Label, ""})
			}
		case "l":
			if m.area == 1 && m.selectedID() != "" {
				return m, m.start(Request{Action: "login", ID: m.selectedID()})
			}
		case "d", "o":
			if m.area == 1 && m.selectedID() != "" {
				m.confirmation = key
				m.confirmYes = false
			}
		default:
			for i := range areas {
				if key == fmt.Sprint(i+1) {
					m.area, m.selected, m.detail = i, 0, false
				}
			}
		}
	}
	return m, nil
}
func (m *Model) stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.generation++
	// Keep busy until the cancelled command returns. The command may be
	// waiting in SQLite or Secret Service and can still finish a side effect.
	m.form = nil
	m.pendingForm = nil
	m.cancel = nil
}
func (m *Model) selectedID() string {
	rows := m.rows()
	if m.selected < len(rows) {
		return rows[m.selected].ID
	}
	return ""
}
func (m *Model) rows() []Row {
	switch m.area {
	case 0:
		return m.snapshot.Actions
	case 1:
		return m.snapshot.Accounts
	case 2, 4:
		return m.snapshot.Profiles
	case 3:
		return m.snapshot.Destinations
	case 5:
		if m.snapshot.History != nil {
			return m.snapshot.History
		}
		return m.snapshot.Runs
	}
	return nil
}
func (m *Model) openForm(title, action, id string, labels, values []string) {
	f := &form{title: title, action: action, id: id, labels: labels, original: values}
	for i, value := range values {
		input := textinput.New()
		input.CharLimit = 512
		input.Width = 40
		input.Prompt = ""
		input.SetValue(value)
		if i == 0 {
			input.Focus()
		}
		f.fields = append(f.fields, input)
	}
	m.form = f
}
func (f *form) dirty() bool {
	for i, field := range f.fields {
		if field.Value() != f.original[i] {
			return true
		}
	}
	return false
}
func (m *Model) updateForm(msg tea.KeyMsg) tea.Cmd {
	f := m.form
	// Bubble Tea can group typed runes. Keep ordinary text such as "home"
	// separate from the Home key binding in the input component.
	if msg.Type == tea.KeyRunes && f.focus < len(f.fields) {
		for _, r := range msg.Runes {
			f.fields[f.focus], _ = f.fields[f.focus].Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		return nil
	}
	switch msg.String() {
	case "esc":
		if f.secret {
			m.stop()
			m.notice = "Anulowano logowanie."
		} else if f.dirty() {
			m.confirmation = "discard"
			m.confirmYes = false
		} else {
			m.form = nil
		}
	case "tab", "shift+tab":
		if f.focus < len(f.fields) {
			f.fields[f.focus].Blur()
		}
		delta := 1
		if msg.String() == "shift+tab" {
			delta = -1
		}
		f.focus = (f.focus + delta + len(f.fields) + 2) % (len(f.fields) + 2)
		if f.focus < len(f.fields) {
			f.fields[f.focus].Focus()
		}
	case "enter":
		if f.focus == len(f.fields)+1 {
			return m.updateForm(tea.KeyMsg{Type: tea.KeyEsc})
		}
		if f.secret {
			if strings.TrimSpace(f.fields[0].Value()) == "" {
				m.notice = "Wpisz wymaganą wartość."
				return nil
			}
			f.reply <- f.fields[0].Value()
			f.fields[0].Reset()
			m.form = nil
			m.notice = "Trwa logowanie. Esc: anuluj."
			return nil
		}
		if f.focus < len(f.fields) {
			return m.updateForm(tea.KeyMsg{Type: tea.KeyTab})
		}
		request := Request{Action: f.action, ID: f.id}
		if f.action == "create" {
			request.ID, request.Username, request.PasswordFile = f.fields[0].Value(), f.fields[1].Value(), f.fields[2].Value()
		} else {
			request.Username, request.PasswordFile = f.fields[0].Value(), f.fields[1].Value()
		}
		if strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.Username) == "" {
			m.notice = "Uzupełnij identyfikator i login."
			return nil
		}
		cmd := m.start(request)
		m.pendingForm = m.form
		m.form = nil
		return cmd
	default:
		if f.focus < len(f.fields) {
			f.fields[f.focus], _ = f.fields[f.focus].Update(msg)
		}
	}
	return nil
}
func (m *Model) confirm(key string) tea.Cmd {
	switch key {
	case "esc":
		m.confirmation = ""
		return nil
	case "left", "right", "tab", "shift+tab":
		m.confirmYes = !m.confirmYes
	case "enter":
		action := m.confirmation
		m.confirmation = ""
		if !m.confirmYes {
			return nil
		}
		if action == "discard" {
			m.form = nil
			m.notice = "Odrzucono zmiany."
			return nil
		}
		request := Request{ID: m.selectedID(), Action: "delete"}
		if action == "o" {
			request.Action = "logout"
		}
		return m.start(request)
	}
	return nil
}
func (m *Model) View() string {
	if m.width < MinWidth || m.height < MinHeight {
		return "Za mały terminal.\nWymagane: co najmniej 80 kolumn i 24 wiersze.\nPowiększ okno. Ctrl+C: zakończ.\n"
	}
	content := []string{titles[m.area], ""}
	footer := "1-6: obszar  ↑↓: wybór  Enter: szczegóły  R: odśwież  ?: pomoc  q: koniec"
	switch {
	case m.help:
		content = []string{"Pomoc — klawiatura", "", "1-6: otwórz obszar poza formularzem.", "Strzałki ↑↓: wybierz pozycję. PgUp/PgDn: przewiń.", "Enter: otwórz szczegóły lub wykonaj działanie.", "Konta: A dodaj, E edytuj, L zaloguj, O wyloguj, D usuń.", "R: odśwież dane i stan sesji.", "Tab / Shift+Tab: pola i przyciski formularza.", "Esc: wróć; w formularzu anuluj zmiany.", "Hasło i kod MFA są ukryte. F1: pomoc w formularzu.", "Q: zakończ poza formularzem. Ctrl+C: zakończ zawsze.", "", "Esc / Enter: zamknij pomoc."}
	case m.confirmation != "":
		title := "Usunąć konto " + m.selectedID() + "?"
		detail := "Usunięcie obejmuje profile i ich historię."
		if m.confirmation == "o" {
			title = "Wylogować konto " + m.selectedID() + "?"
			detail = "Zapisana sesja tego konta zostanie usunięta."
		}
		if m.confirmation == "discard" {
			title = "Odrzucić zmiany?"
			detail = "Niezapisane dane formularza zostaną usunięte."
		}
		buttons := "> [Nie]    [Tak]"
		if m.confirmYes {
			buttons = "  [Nie]  > [Tak]"
		}
		content = append(strings.Split(ansi.Hardwrap(safe(title), m.bodyWidth(), true), "\n"), "", detail, "", buttons)
		footer = "Tab / ←→: wybór  Enter: zatwierdź  Esc: wróć"
	case m.form != nil:
		f := m.form
		content = []string{f.title, ""}
		for i, field := range f.fields {
			prefix := "  "
			if f.focus == i {
				prefix = "> "
			}
			value := field.Value()
			if f.focus == i && !f.secret {
				value = inputWindow(field, m.bodyWidth()-2)
			}
			if f.secret {
				value = "[wartość ukryta]"
			}
			content = append(content, prefix+f.labels[i], "  "+value, "")
		}
		save, cancel := "  [Zapisz]", "  [Anuluj]"
		if f.secret {
			save = "  [Wyślij]"
		}
		if f.focus == len(f.fields) {
			save = ">" + save[1:]
		}
		if f.focus == len(f.fields)+1 {
			cancel = ">" + cancel[1:]
		}
		content = append(content, save+"    "+cancel)
		footer = "Tab: pole lub przycisk  Enter: dalej  Esc: anuluj  F1: pomoc"
	default:
		if m.area == 0 {
			content = append(content, m.snapshot.Summary, "", "Do zrobienia")
		}
		rows := m.rows()
		if len(rows) == 0 {
			empty := "Brak zapisanych pozycji."
			if m.area == 1 {
				empty = "Brak kont. Naciśnij A, aby dodać konto."
			}
			if m.area == 0 {
				empty = "Brak wymaganych działań."
				if !m.loaded {
					empty = "Stan nie jest jeszcze dostępny. R: odśwież."
				}
			}
			content = append(content, empty)
		}
		if m.detail && m.selected < len(rows) {
			row := rows[m.selected]
			content = append(content, row.ID, row.Label, row.Detail, "", "Esc: wróć do listy.")
		} else {
			capacity := max(1, m.height-6-len(content)-2)
			start := max(0, m.selected-capacity+1)
			for i := start; i < min(len(rows), start+capacity); i++ {
				prefix := "  "
				if i == m.selected {
					prefix = "> "
				}
				row := rows[i]
				label := row.Label
				if m.area == 1 {
					label = row.ID + " — " + row.Detail
				}
				content = append(content, prefix+label)
			}
			if len(rows) > capacity {
				content = append(content, fmt.Sprintf("Pozycja %d z %d · PgUp/PgDn", m.selected+1, len(rows)))
			}
		}
		if m.area == 1 {
			footer = "A: dodaj  E: edytuj  L: loguj  O: wyloguj  D: usuń  ?: pomoc  Esc: stan"
		}
		if m.area == 4 {
			content = append(content, "", "Stan zapisanych profili. R: odśwież.", "Stałe monitorowanie: polecenie medalert watch.", "Stan usługi systemd: nie jest sprawdzany.")
		}
	}
	return m.frame(content, footer)
}

func (m *Model) frame(content []string, footer string) string {
	// Use the terminal's own foreground and background. Selection is visible
	// without color. At 120 columns the required actions get a third pane.
	width := m.bodyWidth()
	side := m.width >= 120
	lines := []string{"MedAlert  /  " + titles[m.area], strings.Repeat("─", m.width)}
	for i := 0; i < m.height-6; i++ {
		nav := ""
		if i < len(areas) {
			prefix := "  "
			if i == m.area {
				prefix = "> "
			}
			nav = fmt.Sprintf("%s%d %s", prefix, i+1, areas[i])
		}
		body := ""
		if i < len(content) {
			body = content[i]
		}
		line := pad(nav, 18) + " │ " + pad(body, width)
		if side {
			detail := ""
			if i == 0 {
				detail = "Do zrobienia"
			} else if i <= len(m.snapshot.Actions) {
				detail = m.snapshot.Actions[i-1].Label
			}
			line += " │ " + pad(detail, 29)
		}
		lines = append(lines, line)
	}
	lines = append(lines, strings.Repeat("─", m.width), ansi.Truncate(safe(m.notice), m.width, "…"), ansi.Truncate(footer, m.width, "…"))
	return strings.Join(lines, "\n")
}
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}
func pad(s string, width int) string {
	s = ansi.Truncate(safe(s), max(0, width), "…")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func (m *Model) bodyWidth() int {
	width := m.width - 21
	if m.width >= 120 {
		width -= 32
	}
	return max(1, width)
}

// inputWindow keeps the cursor and nearby text visible in terminal cells.
func inputWindow(field textinput.Model, width int) string {
	runes := []rune(field.Value())
	position := field.Position()
	before, after := safe(string(runes[:position])), safe(string(runes[position:]))
	beforeLimit := width - 1
	if after != "" {
		beforeLimit--
	}
	if cells := ansi.StringWidth(before); cells > beforeLimit {
		before = "…" + ansi.TruncateLeft(before, cells-beforeLimit+1, "")
	}
	after = ansi.Truncate(after, max(0, width-ansi.StringWidth(before)-1), "…")
	return before + "▏" + after
}
