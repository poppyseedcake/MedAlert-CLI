package cli

import (
	"bytes"
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

	"github.com/poppyseedcake/MedAlert/internal/store"
)

// minimalFake is a small local Medicover stand-in for CLI tests. It accepts
// one password user and one MFA user and never contacts production.
type minimalFake struct {
	mu            sync.Mutex
	baseURL       string
	redirectURI   string
	pending       map[string]string
	lastState     string
	codes         map[string]string
	refreshTokens map[string]bool
	lastLogin     url.Values
	lastMFA       url.Values
}

func newMinimalFake(t *testing.T) (*minimalFake, func()) {
	t.Helper()
	fake := &minimalFake{pending: map[string]string{}, codes: map[string]string{}, refreshTokens: map[string]bool{}}
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
		fake.lastState = state
		fake.mu.Unlock()
		if hasTestCookie(r, "MedicoverTrusted") {
			code := "code-" + state[:6]
			fake.mu.Lock()
			fake.codes[code] = state
			fake.mu.Unlock()
			http.Redirect(w, r, query.Get("redirect_uri")+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusFound)
			http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/"})
			return
		}
		http.Redirect(w, r, "/Account/Login?ReturnUrl="+url.QueryEscape("/connect/authorize?"+r.URL.RawQuery), http.StatusFound)
	})
	mux.HandleFunc("/Account/Login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form method="post" action="/Account/Login">` +
				`<input type="hidden" name="__RequestVerificationToken" value="t123" />` +
				`<input type="hidden" name="Input.ReturnUrl" value="` + r.URL.Query().Get("ReturnUrl") + `" />` +
				`<input type="hidden" name="X-Unknown-Preserved" value="keep-me" />` +
				`<input type="text" name="Input.Username" /><input type="password" name="Input.Password" />` +
				`</form></body></html>`))
			return
		}
		_ = r.ParseForm()
		fake.mu.Lock()
		fake.lastLogin = r.PostForm
		fake.mu.Unlock()
		username := r.PostForm.Get("Input.Username")
		password := r.PostForm.Get("Input.Password")
		if username == "mfa-user@example.com" && password == "mfa-pass" {
			http.SetCookie(w, &http.Cookie{Name: "MFA-Pending", Value: "1", Path: "/"})
			http.Redirect(w, r, "/Mfa", http.StatusFound)
			return
		}
		if username == "plain-user@example.com" && password == "plain-pass" {
			state := extractState(r.PostForm.Get("Input.ReturnUrl"))
			if state == "" {
				fake.mu.Lock()
				state = fake.lastState
				fake.mu.Unlock()
			}
			code := "code-login"
			fake.mu.Lock()
			fake.codes[code] = state
			fake.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/"})
			http.Redirect(w, r, fake.redirectURI+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><form method="post" action="/Account/Login">` +
			`<input type="hidden" name="__RequestVerificationToken" value="t123" />` +
			`<input type="text" name="Input.Username" /><input type="password" name="Input.Password" />` +
			`</form></body></html>`))
	})
	mux.HandleFunc("/Mfa", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><form method="post" action="/MFA/Post">` +
			`<input type="hidden" name="__RequestVerificationToken" value="mfa-t" />` +
			`<input type="hidden" name="X-Unknown-Preserved" value="keep-me-mfa" />` +
			`<input type="text" name="Input.MfaCode" />` +
			`</form></body></html>`))
	})
	mux.HandleFunc("/MFA/Post", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fake.mu.Lock()
		fake.lastMFA = r.PostForm
		fake.mu.Unlock()
		if r.PostForm.Get("Input.MfaCode") != "123456" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form method="post" action="/MFA/Post"><input type="text" name="Input.MfaCode" /></form></body></html>`))
			return
		}
		fake.mu.Lock()
		state := fake.lastState
		fake.mu.Unlock()
		code := "code-mfa"
		fake.mu.Lock()
		fake.codes[code] = state
		fake.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/"})
		http.Redirect(w, r, fake.redirectURI+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusFound)
	})
	mux.HandleFunc("/connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			refresh := r.PostForm.Get("refresh_token")
			fake.mu.Lock()
			ok := fake.refreshTokens[refresh]
			fake.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-refreshed", "refresh_token": "refresh-new", "expires_in": 300})
			return
		}
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
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		refresh := "refresh-1"
		fake.mu.Lock()
		fake.refreshTokens[refresh] = true
		delete(fake.codes, code)
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": refresh, "expires_in": 300})
	})
	server := httptest.NewServer(mux)
	fake.baseURL = server.URL
	fake.redirectURI = server.URL + "/signin-oidc"
	return fake, server.Close
}

func hasTestCookie(r *http.Request, name string) bool {
	cookie, err := r.Cookie(name)
	return err == nil && cookie.Value != ""
}

func extractState(returnURL string) string {
	if parsed, err := url.Parse(returnURL); err == nil {
		return parsed.Query().Get("state")
	}
	return ""
}

func testAuthGetenv(root, database, sessionDir, baseURL string) func(string) string {
	return func(key string) string {
		switch key {
		case "XDG_DATA_HOME":
			return root
		case "MEDALERT_DATABASE":
			return database
		case "MEDALERT_SESSION_DIR":
			return sessionDir
		case "MEDALERT_MEDICOVER_BASE_URL":
			return baseURL
		case "MEDALERT_OUTPUT", "MEDALERT_NON_INTERACTIVE", "HOME":
			return ""
		default:
			return ""
		}
	}
}

func writeSecretFile(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func createFileAccount(t *testing.T, getenv func(string) string, database, id, username, passwordFile string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"account", "create", "--database", database, "--non-interactive", "--account", id, "--username", username, "--password-file", passwordFile}, os.Stdin, &stdout, &stderr, getenv)
	if code != 0 {
		t.Fatalf("create %s: code=%d stderr=%q", id, code, stderr.String())
	}
}

func TestLoginLogoutStatusWithFileSessions(t *testing.T) {
	fake, cleanup := newMinimalFake(t)
	defer cleanup()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := testAuthGetenv(root, database, sessionDir, fake.baseURL)
	passwordFile := writeSecretFile(t, secretDir, "pw", "plain-pass")
	createFileAccount(t, getenv, database, "alice", "plain-user@example.com", passwordFile)

	// Login
	var loginOut, loginErr bytes.Buffer
	loginCode := RunWithIO([]string{"account", "login", "--database", database, "--session-dir", sessionDir, "--medicover-base-url", fake.baseURL, "--account", "alice", "--non-interactive"}, os.Stdin, &loginOut, &loginErr, getenv)
	if loginCode != 0 {
		t.Fatalf("login: code=%d stdout=%q stderr=%q", loginCode, loginOut.String(), loginErr.String())
	}
	// Status is authenticated.
	var statusOut, statusErr bytes.Buffer
	statusCode := RunWithIO([]string{"account", "status", "--database", database, "--session-dir", sessionDir, "--account", "alice"}, os.Stdin, &statusOut, &statusErr, getenv)
	if statusCode != 0 {
		t.Fatalf("status: code=%d stdout=%q stderr=%q", statusCode, statusOut.String(), statusErr.String())
	}
	// Logout removes only this account.
	var logoutOut, logoutErr bytes.Buffer
	logoutCode := RunWithIO([]string{"account", "logout", "--database", database, "--session-dir", sessionDir, "--account", "alice"}, os.Stdin, &logoutOut, &logoutErr, getenv)
	if logoutCode != 0 {
		t.Fatalf("logout: code=%d stderr=%q", logoutCode, logoutErr.String())
	}
	var statusAfterOut, statusAfterErr bytes.Buffer
	statusAfter := RunWithIO([]string{"account", "status", "--database", database, "--session-dir", sessionDir, "--account", "alice", "--output", "json"}, os.Stdin, &statusAfterOut, &statusAfterErr, getenv)
	if statusAfter != 3 {
		t.Fatalf("status after logout = %d, want 3", statusAfter)
	}
	// Hidden fields were preserved.
	fake.mu.Lock()
	loginForm := fake.lastLogin
	fake.mu.Unlock()
	if loginForm.Get("X-Unknown-Preserved") != "keep-me" {
		t.Fatalf("hidden fields were not preserved: %v", loginForm)
	}
}

func TestLoginRequiresMFAFileWhenNonInteractive(t *testing.T) {
	fake, cleanup := newMinimalFake(t)
	defer cleanup()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := testAuthGetenv(root, database, sessionDir, fake.baseURL)
	passwordFile := writeSecretFile(t, secretDir, "pw", "mfa-pass")
	createFileAccount(t, getenv, database, "mfa", "mfa-user@example.com", passwordFile)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"account", "login", "--database", database, "--session-dir", sessionDir, "--medicover-base-url", fake.baseURL, "--account", "mfa", "--non-interactive"}, os.Stdin, &stdout, &stderr, getenv)
	if code != 3 {
		t.Fatalf("login without MFA = %d, want 3", code)
	}
	mfaFile := writeSecretFile(t, secretDir, "mfa", "123456")
	stdout.Reset()
	stderr.Reset()
	code = RunWithIO([]string{"account", "login", "--database", database, "--session-dir", sessionDir, "--medicover-base-url", fake.baseURL, "--account", "mfa", "--non-interactive", "--mfa-code-file", mfaFile}, os.Stdin, &stdout, &stderr, getenv)
	if code != 0 {
		t.Fatalf("MFA login: code=%d stderr=%q", code, stderr.String())
	}
}

func TestLogoutIsPerAccount(t *testing.T) {
	fake, cleanup := newMinimalFake(t)
	defer cleanup()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := testAuthGetenv(root, database, sessionDir, fake.baseURL)
	passwordFile := writeSecretFile(t, secretDir, "pw", "plain-pass")
	createFileAccount(t, getenv, database, "alice", "plain-user@example.com", passwordFile)
	createFileAccount(t, getenv, database, "bob", "plain-user@example.com", passwordFile)

	for _, id := range []string{"alice", "bob"} {
		var stdout, stderr bytes.Buffer
		if code := RunWithIO([]string{"account", "login", "--database", database, "--session-dir", sessionDir, "--medicover-base-url", fake.baseURL, "--account", id, "--non-interactive"}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
			t.Fatalf("login %s: %d %q", id, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := RunWithIO([]string{"account", "logout", "--database", database, "--session-dir", sessionDir, "--account", "alice"}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("logout alice: %d", code)
	}
	var bobOut, bobErr bytes.Buffer
	if code := RunWithIO([]string{"account", "status", "--database", database, "--session-dir", sessionDir, "--account", "bob"}, os.Stdin, &bobOut, &bobErr, getenv); code != 0 {
		t.Fatalf("bob status after alice logout = %d, want 0 (independent)", code)
	}
	var aliceOut, aliceErr bytes.Buffer
	if code := RunWithIO([]string{"account", "status", "--database", database, "--session-dir", sessionDir, "--account", "alice"}, os.Stdin, &aliceOut, &aliceErr, getenv); code != 3 {
		t.Fatalf("alice status after logout = %d, want 3", code)
	}
}

func TestLoginRedactsSecrets(t *testing.T) {
	fake, cleanup := newMinimalFake(t)
	defer cleanup()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "SECRET-MARKER-" + t.Name() + "-unique"
	getenv := testAuthGetenv(root, database, sessionDir, fake.baseURL)
	passwordFile := writeSecretFile(t, secretDir, "pw", marker)
	storage, err := store.Open(mustInitDB(t, database))
	if err == nil {
		storage.Close()
	}
	createFileAccount(t, getenv, database, "alice", "plain-user@example.com", passwordFile)

	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"account", "login", "--database", database, "--session-dir", sessionDir, "--medicover-base-url", fake.baseURL, "--account", "alice", "--non-interactive", "--output", "json"}, os.Stdin, &stdout, &stderr, getenv)
	// Wrong password (marker) must fail without leaking the marker.
	if code == 0 {
		t.Fatal("login succeeded with a wrong password marker")
	}
	for _, output := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(output, marker) {
			t.Fatalf("output leaks secret: %q", output)
		}
	}
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatal("database contains secret")
	}
}

func mustInitDB(t *testing.T, database string) string {
	t.Helper()
	if err := os.Chmod(filepath.Dir(database), 0o700); err != nil {
		// t.TempDir is 0700 by default on some systems; ensure parent exists.
		if mkErr := os.MkdirAll(filepath.Dir(database), 0o700); mkErr != nil {
			t.Fatal(mkErr)
		}
	}
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestAuthRejectsIrrelevantFlags(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	getenv := testAuthGetenv(root, database, "", "")
	for _, args := range [][]string{
		{"account", "login", "--database", database, "--account", "a", "--username", "u"},
		{"account", "logout", "--database", database, "--account", "a", "--mfa-code-file", "f"},
		{"account", "status", "--database", database, "--account", "a", "--forget-secret"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunWithIO(args, os.Stdin, &stdout, &stderr, getenv); code != 2 || !strings.Contains(stderr.String(), "not supported") {
			t.Fatalf("args %v: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}
