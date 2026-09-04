package main_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// incidentMedicoverFake is a Medicover stand-in for operational-incident
// acceptance. It validates test credentials, serves slots, and injects
// temporary or protocol failures, including per-region protocol failures for
// profile isolation.
type incidentMedicoverFake struct {
	mu          sync.Mutex
	baseURL     string
	redirectURI string
	slots       []map[string]any
	mode        string // success, temporary, protocol
	failRegion  string
	pending     map[string]string
	codes       map[string]string
}

func newIncidentMedicoverFake(t *testing.T) (*incidentMedicoverFake, func()) {
	t.Helper()
	fake := &incidentMedicoverFake{pending: map[string]string{}, codes: map[string]string{}, mode: "success"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 fake.baseURL,
			"authorization_endpoint": fake.baseURL + "/connect/authorize",
			"token_endpoint":         fake.baseURL + "/connect/token",
		})
	})
	mux.HandleFunc("/connect/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		state := query.Get("state")
		fake.mu.Lock()
		fake.pending[state] = query.Get("code_challenge")
		fake.mu.Unlock()
		if _, err := r.Cookie("MedicoverTrusted"); err == nil {
			code := "incident-trusted-" + state[:minIncidentLen(state, 6)]
			fake.mu.Lock()
			fake.codes[code] = state
			fake.mu.Unlock()
			http.Redirect(w, r, query.Get("redirect_uri")+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusFound)
			return
		}
		http.Redirect(w, r, "/Account/Login?ReturnUrl="+url.QueryEscape("/connect/authorize?"+r.URL.RawQuery), http.StatusFound)
	})
	mux.HandleFunc("/Account/Login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form method="post" action="/Account/Login">` +
				`<input type="hidden" name="__RequestVerificationToken" value="token" />` +
				`<input type="hidden" name="Input.ReturnUrl" value="` + r.URL.Query().Get("ReturnUrl") + `" />` +
				`<input type="text" name="Input.Username" /><input type="password" name="Input.Password" />` +
				`</form></body></html>`))
			return
		}
		_ = r.ParseForm()
		username := r.PostForm.Get("Input.Username")
		password := r.PostForm.Get("Input.Password")
		valid := (username == "patient@example.com" && password == "durable-pass") ||
			(username == "alice@example.com" && password == "alice-pass") ||
			(username == "bob@example.com" && password == "bob-pass")
		if !valid {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form><input type="password" name="Input.Password" /></form></body></html>`))
			return
		}
		state := ""
		if parsed, err := url.Parse(r.PostForm.Get("Input.ReturnUrl")); err == nil {
			state = parsed.Query().Get("state")
		}
		code := "incident-login-" + state[:minIncidentLen(state, 6)]
		fake.mu.Lock()
		fake.codes[code] = state
		fake.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted", Path: "/"})
		http.Redirect(w, r, fake.redirectURI+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusFound)
	})
	mux.HandleFunc("/connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.PostForm.Get("code")
		verifier := r.PostForm.Get("code_verifier")
		fake.mu.Lock()
		state, ok := fake.codes[code]
		challenge := fake.pending[state]
		fake.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		// Verify PKCE without importing medicover internals: recompute locally.
		if !validIncidentPKCE(verifier, challenge) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "incident-access",
			"refresh_token": "incident-refresh",
			"expires_in":    300,
		})
	})
	searchHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fake.mu.Lock()
		mode := fake.mode
		failRegion := fake.failRegion
		slots := append([]map[string]any(nil), fake.slots...)
		fake.mu.Unlock()
		if mode == "temporary" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if mode == "protocol" {
			// Invalid filter shape triggers protocol_changed.
			_, _ = w.Write([]byte(`{"unexpected":true}`))
			return
		}
		if failRegion != "" && strings.Contains(r.URL.RawQuery, failRegion) {
			_, _ = w.Write([]byte(`{"unexpected":true}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/slots") {
			if slots == nil {
				slots = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": slots, "totalPages": 1})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"regions": []any{}})
	}
	mux.HandleFunc("/appointments/api/v2/search-appointments/filters/initial-filters", searchHandler)
	mux.HandleFunc("/appointments/api/v2/search-appointments/filters", searchHandler)
	mux.HandleFunc("/appointments/api/v2/search-appointments/slots", searchHandler)
	server := httptest.NewServer(mux)
	fake.baseURL = server.URL
	fake.redirectURI = server.URL + "/signin-oidc"
	return fake, server.Close
}

func minIncidentLen(value string, n int) int {
	if len(value) < n {
		return len(value)
	}
	return n
}

func validIncidentPKCE(verifier, challenge string) bool {
	// Recompute S256 without importing medicover: standard library only.
	// Must match medicover.NewPKCE: base64url(sha256(verifier)) == challenge.
	if verifier == "" || challenge == "" {
		return false
	}
	// Use the same logic as the other fakes via manual sha256.
	return incidentPKCEChallenge(verifier) == challenge
}

func incidentPKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (f *incidentMedicoverFake) setSlots(slots ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots = append([]map[string]any(nil), slots...)
	f.mode = "success"
}

func (f *incidentMedicoverFake) setMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

func (f *incidentMedicoverFake) setFailRegion(region string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failRegion = region
}

func incidentSlot(booking string) map[string]any {
	return map[string]any{
		"bookingString":   booking,
		"appointmentDate": "2099-09-10T10:00:00Z",
		"clinic":          map[string]any{"name": "Main clinic"},
		"doctor":          map[string]any{"name": "Dr Example"},
		"specialty":       map[string]any{"name": "Cardiology"},
		"visitType":       "Center",
	}
}

// incidentTelegramFake records Telegram sends and replays per-chat outcomes.
type incidentTelegramFake struct {
	mu        sync.Mutex
	failChats map[string]string // chat -> mode: permanent, temporary
	global    string            // success, temporary, permanent
	messageID int64
	requests  int
	chats     []string
	texts     []string
}

func newIncidentTelegramFake() *incidentTelegramFake {
	return &incidentTelegramFake{failChats: map[string]string{}, global: "success", messageID: 200}
}

func (f *incidentTelegramFake) setGlobal(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.global = mode
}

func (f *incidentTelegramFake) failChat(chat, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failChats[chat] = mode
}

func (f *incidentTelegramFake) fixChat(chat string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failChats, chat)
}

func (f *incidentTelegramFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *incidentTelegramFake) snapshot() (chats []string, texts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.chats...), append([]string(nil), f.texts...)
}

func (f *incidentTelegramFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		chat := ""
		text := ""
		if value, ok := payload["chat_id"].(string); ok {
			chat = value
		}
		if value, ok := payload["text"].(string); ok {
			text = value
		}
		f.mu.Lock()
		f.requests++
		f.chats = append(f.chats, chat)
		f.texts = append(f.texts, text)
		mode := f.global
		if perChat, ok := f.failChats[chat]; ok {
			mode = perChat
		}
		f.messageID++
		messageID := f.messageID
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "temporary":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"ok":false}`))
		case "permanent":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + itoa(int(messageID)) + `}}`))
		}
	})
}

func createIncidentFixture(t *testing.T, medicoverBase, telegramBase, root string, accounts int) (string, []string, string) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "sessions")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := []string{
		"MEDALERT_DATABASE=" + database,
		"MEDALERT_SESSION_DIR=" + sessionDir,
		"MEDALERT_MEDICOVER_BASE_URL=" + medicoverBase,
		"MEDALERT_TELEGRAM_BASE_URL=" + telegramBase,
		"MEDALERT_NON_INTERACTIVE=true",
	}
	users := []struct{ id, username, password string }{
		{"alice", "alice@example.com", "alice-pass"},
		{"bob", "bob@example.com", "bob-pass"},
		{"patient", "patient@example.com", "durable-pass"},
	}
	for index := 0; index < accounts && index < len(users); index++ {
		user := users[index]
		passwordFile := filepath.Join(root, "password-"+user.id)
		if err := os.WriteFile(passwordFile, []byte(user.password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		created := run(t, environment, "account", "create", "--account", user.id, "--username", user.username, "--password-file", passwordFile, "--non-interactive")
		if created.exitCode != 0 {
			t.Fatalf("create account %s = %#v", user.id, created)
		}
	}
	return database, environment, secretDir
}

func createIncidentDestinations(t *testing.T, environment []string, secretDir string) {
	t.Helper()
	tokenOne := writeProcessSecretFile(t, secretDir, "token-one", "TOKEN-INCIDENT-ONE-unique")
	tokenTwo := writeProcessSecretFile(t, secretDir, "token-two", "TOKEN-INCIDENT-TWO-unique")
	for _, destination := range []struct{ id, name, chat, token string }{
		{"phone", "Telefon", "123456", tokenOne},
		{"backup", "Zapas", "654321", tokenTwo},
	} {
		created := run(t, environment, "telegram", "create", "--telegram", destination.id, "--name", destination.name, "--chat-id", destination.chat, "--token-file", destination.token, "--non-interactive")
		if created.exitCode != 0 {
			t.Fatalf("create destination %s = %#v", destination.id, created)
		}
	}
}

func createIncidentProfile(t *testing.T, environment []string, profileID, accountID, region string, destinations string) {
	t.Helper()
	created := run(t, environment, "profile", "create", "--profile", profileID, "--account", accountID, "--region", region, "--specialty", "132", "--check-interval-minutes", "30", "--telegram", destinations, "--non-interactive")
	if created.exitCode != 0 {
		t.Fatalf("create profile %s = %#v", profileID, created)
	}
}
