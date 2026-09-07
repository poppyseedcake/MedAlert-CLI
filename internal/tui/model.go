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

// ProfileValues contains safe values used to fill a profile form. It does
// not contain passwords, sessions, or notification tokens.
type ProfileValues struct {
	AccountID            string
	RegionIDs            string
	SpecialtyIDs         string
	ClinicIDs            string
	DoctorIDs            string
	LanguageIDs          string
	VisitType            string
	SearchType           string
	StartDate            string
	EndDate              string
	CheckIntervalMinutes string
	Enabled              bool
}

// DestinationValues contains safe values used to fill Telegram forms. It
// never contains a bot token; TokenFile is only a path to an approved secret
// file.
type DestinationValues struct {
	Name           string
	ChatID         string
	TokenFile      string
	TokenSource    string
	LinkedProfiles string
	LastTestAt     string
	LastTestStatus string
	LastTestError  string
	Enabled        bool
}

type Snapshot struct {
	Accounts                     []Row
	Actions                      []Row
	Profiles, Destinations, Runs []Row
	Monitoring                   []Row
	ProfileValues                map[string]ProfileValues
	DestinationValues            map[string]DestinationValues
	History                      []Row
	Summary                      string
}
type Request struct {
	Action, ID, Username, PasswordFile string
	Profile                            ProfileValues
	Destination                        DestinationValues
	ProfileClear                       []string
}
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
	operations                    map[int]string
	runningKeys                   map[string]struct{}
}

type form struct {
	title, action, id      string
	keys, labels, original []string
	fields                 []textinput.Model
	focus                  int
	secret                 bool
	reply                  chan string
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
	key := operationKey(request)
	if _, running := m.runningKeys[key]; running {
		m.notice = "Poprzednia operacja nadal się kończy. Spróbuj ponownie później."
		return nil
	}
	if m.operations == nil {
		m.operations = make(map[int]string)
		m.runningKeys = make(map[string]struct{})
	}
	m.busy = true
	m.notice = "Trwa operacja. Esc: anuluj oczekiwanie."
	m.generation++
	generation := m.generation
	m.activeGeneration = generation
	m.operations[generation] = key
	m.runningKeys[key] = struct{}{}
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
		if msg.generation == m.activeGeneration && m.busy {
			title := "Podaj hasło"
			if msg.kind == "mfa" {
				title = "Podaj kod MFA"
			} else if msg.kind == "telegram-token" {
				title = "Podaj token bota Telegram"
			}
			m.openForm(title, "secret", "", []string{title}, []string{""})
			m.form.secret, m.form.reply = true, msg.reply
			m.form.fields[0].EchoMode = textinput.EchoNone
			m.notice = "Wpisz dane. Enter: wyślij. Esc: anuluj logowanie."
			if msg.kind == "telegram-token" {
				m.notice = "Wpisz dane. Enter: wyślij. Esc: anuluj operację."
			}
		}
		return m, m.waitPrompt()
	case finishedMsg:
		key, running := m.operations[msg.generation]
		if !running {
			return m, nil
		}
		delete(m.operations, msg.generation)
		delete(m.runningKeys, key)
		if msg.generation != m.activeGeneration {
			// A cancelled operation can finish after a new operation starts.
			// Its result must not replace the newer state.
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
			} else if m.area == 2 {
				m.openProfileCreateForm()
			} else if m.area == 3 {
				m.openDestinationCreateForm()
			}
		case "e":
			if m.area == 1 && m.selectedID() != "" {
				m.openForm("Edytuj konto", "edit", m.selectedID(), []string{"Login Medicover", "Plik hasła (pusty: zachowaj źródło)"}, []string{m.rows()[m.selected].Label, ""})
			} else if m.area == 2 && m.selectedID() != "" {
				m.openProfileEditForm(m.selectedID())
			} else if m.area == 3 && m.selectedID() != "" {
				m.openDestinationEditForm(m.selectedID())
			}
		case "l":
			if m.area == 1 && m.selectedID() != "" {
				return m, m.start(Request{Action: "login", ID: m.selectedID()})
			} else if m.area == 3 && m.selectedID() != "" {
				m.openDestinationLinkForm(m.selectedID())
			}
		case "p":
			if m.area == 2 && m.selectedID() != "" {
				action := "profile-enable"
				if m.profileEnabled(m.selectedID()) {
					action = "profile-disable"
				}
				return m, m.start(Request{Action: action, ID: m.selectedID()})
			} else if m.area == 3 && m.selectedID() != "" {
				action := "telegram-enable"
				if m.destinationEnabled(m.selectedID()) {
					action = "telegram-disable"
				}
				return m, m.start(Request{Action: action, ID: m.selectedID()})
			}
		case "t":
			if m.area == 3 && m.selectedID() != "" {
				return m, m.start(Request{Action: "telegram-test", ID: m.selectedID()})
			}
		case "s":
			if m.area == 3 && m.selectedID() != "" {
				return m, m.start(Request{Action: "telegram-token", ID: m.selectedID()})
			}
		case "y":
			if m.area == 2 && m.selectedID() != "" {
				return m, m.start(Request{Action: "profile-dry-check", ID: m.selectedID()})
			}
		case "k":
			if m.area == 2 && m.selectedID() != "" {
				return m, m.start(Request{Action: "profile-check", ID: m.selectedID()})
			}
		case "d":
			if (m.area == 1 || m.area == 2) && m.selectedID() != "" {
				m.confirmation = key
				m.confirmYes = false
			} else if m.area == 3 && m.selectedID() != "" {
				m.confirmation = key
				m.confirmYes = false
			}
		case "o":
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
	// Release the foreground UI immediately. The worker remains in operations
	// until it returns, so another operation for the same account cannot race
	// with its late side effect.
	m.busy = false
	m.activeGeneration = 0
	m.form = nil
	m.pendingForm = nil
	m.cancel = nil
}

func operationKey(request Request) string {
	if id := strings.TrimSpace(request.ID); id != "" {
		if strings.HasPrefix(request.Action, "telegram-") {
			return "destination:" + id
		}
		if strings.HasPrefix(request.Action, "profile-") {
			return "profile:" + id
		}
		return "account:" + id
	}
	return "action:" + request.Action
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
	case 2:
		return m.snapshot.Profiles
	case 4:
		return m.snapshot.Monitoring
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

func (m *Model) profileEnabled(id string) bool {
	if values, ok := m.snapshot.ProfileValues[id]; ok {
		return values.Enabled
	}
	rows := m.snapshot.Profiles
	for _, row := range rows {
		if row.ID == id {
			return !strings.Contains(strings.ToLower(row.Label), "wyłączony")
		}
	}
	return false
}

func (m *Model) destinationEnabled(id string) bool {
	if values, ok := m.snapshot.DestinationValues[id]; ok {
		return values.Enabled
	}
	for _, row := range m.snapshot.Destinations {
		if row.ID == id {
			return !strings.Contains(strings.ToLower(row.Label), "wyłączony")
		}
	}
	return false
}

func (m *Model) openForm(title, action, id string, labels, values []string) {
	m.openFormFields(title, action, id, defaultFormKeys(action, len(labels)), labels, values)
}

func (m *Model) openFormFields(title, action, id string, keys, labels, values []string) {
	keys = append([]string(nil), keys...)
	labels = append([]string(nil), labels...)
	values = append([]string(nil), values...)
	f := &form{title: title, action: action, id: id, keys: keys, labels: labels, original: values}
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

func defaultFormKeys(action string, fieldCount int) []string {
	var keys []string
	switch action {
	case "create":
		keys = []string{"id", "username", "password_file"}
	case "edit":
		keys = []string{"username", "password_file"}
	case "secret":
		keys = []string{"secret"}
	case "telegram-create":
		keys = []string{"destination_id", "destination_name", "chat_id", "token_file"}
	case "telegram-edit":
		keys = []string{"destination_name", "chat_id", "token_file"}
	case "telegram-link":
		keys = []string{"profile_ids"}
	}
	if len(keys) != fieldCount {
		keys = make([]string, fieldCount)
	}
	return keys
}

var profileCreateKeys = []string{
	"profile_id", "account_id", "region_ids", "specialty_ids", "clinic_ids", "doctor_ids",
	"language_ids", "visit_type", "search_type", "start_date", "end_date", "check_interval_minutes",
}

var profileEditKeys = []string{
	"region_ids", "specialty_ids", "clinic_ids", "doctor_ids", "language_ids", "visit_type",
	"search_type", "start_date", "end_date", "check_interval_minutes",
}

var profileCreateLabels = []string{
	"Identyfikator profilu",
	"Konto Medicover (identyfikator)",
	"Regiony (ID lub lista)",
	"Specjalności (ID lub lista)",
	"Placówki (ID lub lista, puste: dowolna)",
	"Lekarze (ID lub lista, puste: dowolny)",
	"Języki (ID lub lista, puste: dowolny)",
	"Typ wizyty (opcjonalnie)",
	"Typ wyszukiwania (Standard lub DiagnosticProcedure)",
	"Data od (YYYY-MM-DD, opcjonalnie)",
	"Data do (YYYY-MM-DD, opcjonalnie)",
	"Interwał sprawdzania (minuty)",
}

var profileEditLabels = []string{
	"Regiony (ID lub lista)",
	"Specjalności (ID lub lista)",
	"Placówki (ID lub lista, puste: dowolna)",
	"Lekarze (ID lub lista, puste: dowolny)",
	"Języki (ID lub lista, puste: dowolny)",
	"Typ wizyty (opcjonalnie)",
	"Typ wyszukiwania (Standard lub DiagnosticProcedure)",
	"Data od (YYYY-MM-DD, opcjonalnie)",
	"Data do (YYYY-MM-DD, opcjonalnie)",
	"Interwał sprawdzania (minuty)",
}

func (m *Model) openProfileCreateForm() {
	values := []string{"", "", "", "", "", "", "", "", "Standard", "", "", "30"}
	m.openFormFields("Dodaj profil obserwacji", "profile-create", "", profileCreateKeys, profileCreateLabels, values)
}

func (m *Model) openProfileEditForm(id string) {
	values := m.snapshot.ProfileValues[id]
	formValues := []string{
		values.RegionIDs, values.SpecialtyIDs, values.ClinicIDs, values.DoctorIDs, values.LanguageIDs,
		values.VisitType, values.SearchType, values.StartDate, values.EndDate, values.CheckIntervalMinutes,
	}
	if strings.TrimSpace(formValues[6]) == "" {
		formValues[6] = "Standard"
	}
	m.openFormFields("Edytuj profil obserwacji", "profile-edit", id, profileEditKeys, profileEditLabels, formValues)
}

var destinationCreateLabels = []string{
	"Identyfikator celu (litery, cyfry, - lub _)",
	"Nazwa celu Telegram",
	"Identyfikator czatu Telegram",
	"Plik tokenu (pusty: zapytaj ukrycie)",
}

var destinationEditLabels = []string{
	"Nazwa celu Telegram",
	"Identyfikator czatu Telegram",
	"Plik tokenu (pusty: zachowaj źródło)",
}

func (m *Model) openDestinationCreateForm() {
	m.openFormFields("Dodaj cel Telegram", "telegram-create", "", defaultFormKeys("telegram-create", 4), destinationCreateLabels, []string{"", "", "", ""})
}

func (m *Model) openDestinationEditForm(id string) {
	values := m.snapshot.DestinationValues[id]
	m.openFormFields("Edytuj cel Telegram", "telegram-edit", id, defaultFormKeys("telegram-edit", 3), destinationEditLabels, []string{values.Name, values.ChatID, values.TokenFile})
}

func (m *Model) openDestinationLinkForm(id string) {
	values := m.snapshot.DestinationValues[id]
	m.openFormFields("Powiąż cel Telegram", "telegram-link", id, defaultFormKeys("telegram-link", 1), []string{"Profile powiązane (ID lub lista, puste: odłącz)"}, []string{values.LinkedProfiles})
}

func (f *form) dirty() bool {
	for i, field := range f.fields {
		if field.Value() != f.original[i] {
			return true
		}
	}
	return false
}

func (f *form) request() Request {
	request := Request{Action: f.action, ID: f.id}
	for i, key := range f.keys {
		if i >= len(f.fields) {
			continue
		}
		value := f.fields[i].Value()
		switch key {
		case "id":
			request.ID = value
		case "username":
			request.Username = value
		case "password_file":
			request.PasswordFile = value
		case "profile_id":
			request.ID = value
		case "account_id":
			request.Profile.AccountID = value
		case "region_ids":
			request.Profile.RegionIDs = value
		case "specialty_ids":
			request.Profile.SpecialtyIDs = value
		case "clinic_ids":
			request.Profile.ClinicIDs = value
		case "doctor_ids":
			request.Profile.DoctorIDs = value
		case "language_ids":
			request.Profile.LanguageIDs = value
		case "visit_type":
			request.Profile.VisitType = value
		case "search_type":
			request.Profile.SearchType = value
		case "start_date":
			request.Profile.StartDate = value
		case "end_date":
			request.Profile.EndDate = value
		case "check_interval_minutes":
			request.Profile.CheckIntervalMinutes = value
		case "destination_id":
			request.ID = value
		case "destination_name":
			request.Destination.Name = value
		case "chat_id":
			request.Destination.ChatID = value
		case "token_file":
			request.Destination.TokenFile = value
		case "profile_ids":
			request.Destination.LinkedProfiles = value
		}
		if f.action == "profile-edit" && value == "" && i < len(f.original) && f.original[i] != "" {
			request.ProfileClear = append(request.ProfileClear, key)
		}
	}
	return request
}

func validateFormRequest(request Request) (string, bool) {
	switch request.Action {
	case "create", "edit":
		if strings.TrimSpace(request.ID) == "" || strings.TrimSpace(request.Username) == "" {
			return "Uzupełnij identyfikator i login.", false
		}
	case "profile-create":
		if strings.TrimSpace(request.ID) == "" {
			return "Wpisz identyfikator profilu.", false
		}
		if strings.TrimSpace(request.Profile.AccountID) == "" {
			return "Wpisz identyfikator konta Medicover.", false
		}
		fallthrough
	case "profile-edit":
		if strings.TrimSpace(request.Profile.RegionIDs) == "" {
			return "Wpisz co najmniej jeden region.", false
		}
		if strings.TrimSpace(request.Profile.SpecialtyIDs) == "" {
			return "Wpisz co najmniej jedną specjalność.", false
		}
		if strings.TrimSpace(request.Profile.CheckIntervalMinutes) == "" {
			return "Wpisz interwał sprawdzania w minutach.", false
		}
	case "telegram-create":
		if strings.TrimSpace(request.ID) == "" {
			return "Wpisz identyfikator celu Telegram.", false
		}
		if strings.TrimSpace(request.Destination.Name) == "" {
			return "Wpisz nazwę celu Telegram.", false
		}
		if strings.TrimSpace(request.Destination.ChatID) == "" {
			return "Wpisz identyfikator czatu Telegram.", false
		}
	case "telegram-edit":
		if strings.TrimSpace(request.Destination.Name) == "" {
			return "Wpisz nazwę celu Telegram.", false
		}
		if strings.TrimSpace(request.Destination.ChatID) == "" {
			return "Wpisz identyfikator czatu Telegram.", false
		}
	}
	return "", true
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
			operation := m.operations[m.activeGeneration]
			telegramPrompt := strings.HasPrefix(operation, "destination:") || strings.HasPrefix(operation, "action:telegram-")
			m.stop()
			if telegramPrompt {
				m.notice = "Anulowano operację."
			} else {
				m.notice = "Anulowano logowanie."
			}
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
		request := f.request()
		if notice, valid := validateFormRequest(request); !valid {
			m.notice = notice
			return nil
		}
		cmd := m.start(request)
		if cmd == nil {
			return nil
		}
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
		} else if m.area == 2 {
			request.Action = "profile-delete"
		} else if m.area == 3 {
			request.Action = "telegram-delete"
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
		content = []string{"Pomoc — klawiatura", "", "1-6: otwórz obszar poza formularzem.", "Strzałki ↑↓: wybierz pozycję. PgUp/PgDn: przewiń.", "Enter: otwórz szczegóły lub wykonaj działanie.", "Konta: A dodaj, E edytuj, L zaloguj, O wyloguj, D usuń.", "Profile: A dodaj, E edytuj, P włącz/wyłącz, Y sucha kontrola, K kontrola, D usuń.", "Telegram: A dodaj, E edytuj, P włącz/wyłącz, T test, S nowy token, L profile, D usuń.", "R: odśwież dane i stan sesji.", "Tab / Shift+Tab: pola i przyciski formularza.", "Esc: wróć; w formularzu anuluj zmiany.", "Hasło, kod MFA i token Telegram są ukryte. F1: pomoc w formularzu.", "Q: zakończ poza formularzem. Ctrl+C: zakończ zawsze.", "", "Esc / Enter: zamknij pomoc."}
	case m.confirmation != "":
		title := "Usunąć konto " + m.selectedID() + "?"
		detail := "Usunięcie obejmuje profile i ich historię."
		if m.area == 2 {
			title = "Usunąć profil " + m.selectedID() + "?"
			detail = "Usunięcie obejmuje konfigurację i historię profilu."
		}
		if m.area == 3 {
			title = "Usunąć cel Telegram " + m.selectedID() + "?"
			detail = "Usunięcie obejmuje powiązania i historię dostarczeń."
		}
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
		content = m.formContent()
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
			if m.area == 3 {
				empty = "Brak celów Telegram. Naciśnij A, aby dodać cel."
			}
			if m.area == 0 {
				empty = "Brak wymaganych działań."
				if !m.loaded {
					empty = "Stan nie jest jeszcze dostępny. R: odśwież."
				}
			}
			if m.area == 4 {
				empty = "Brak profili do monitorowania."
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
				if m.area == 4 && row.Detail != "" {
					label += " · " + row.Detail
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
		if m.area == 2 {
			footer = "A: dodaj  E: edytuj  P: włącz/wyłącz  Y: sucha kontrola  K: kontrola  D: usuń  Esc: stan"
		}
		if m.area == 3 {
			footer = "A: dodaj  E: edytuj  P: włącz/wyłącz  T: test  S: token  L: profile  D: usuń  Esc: stan"
		}
		if m.area == 4 {
			footer = "↑↓: wybór  Enter: szczegóły  R: odśwież  Esc: stan"
		}
	}
	return m.frame(content, footer)
}

func (m *Model) formContent() []string {
	f := m.form
	content := []string{f.title, ""}
	// A profile form has many fields. Keep the focused field and its nearby
	// fields visible at the minimum terminal size, so Tab never moves the
	// cursor to an invisible control.
	capacity := max(1, (m.height-6-2)/3)
	start := 0
	if f.focus < len(f.fields) {
		start = max(0, f.focus-capacity+1)
	} else if len(f.fields) > capacity {
		start = len(f.fields) - capacity
	}
	end := min(len(f.fields), start+capacity)
	for i := start; i < end; i++ {
		field := f.fields[i]
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
	return content
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
