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

// authFake is a sanitized local Medicover stand-in for executable acceptance.
// It uses only test credentials and never contacts production.
type authFake struct {
	mu            sync.Mutex
	baseURL       string
	redirectURI   string
	pending       map[string]string
	lastState     string
	codes         map[string]string
	refreshTokens map[string]bool
}

func newAuthFake(t *testing.T) (*authFake, func()) {
	t.Helper()
	fake := &authFake{pending: map[string]string{}, codes: map[string]string{}, refreshTokens: map[string]bool{}}
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
		if hasAuthCookie(r) {
			code := "code-reuse-" + state[:6]
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
		username := r.PostForm.Get("Input.Username")
		password := r.PostForm.Get("Input.Password")
		if r.PostForm.Get("X-Unknown-Preserved") != "keep-me" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>hidden dropped</body></html>`))
			return
		}
		if username == "mfa-user@example.com" && password == "mfa-pass" {
			http.SetCookie(w, &http.Cookie{Name: "MFA-Pending", Value: "1", Path: "/"})
			http.Redirect(w, r, "/Mfa", http.StatusFound)
			return
		}
		if username == "plain-user@example.com" && password == "plain-pass" {
			state := ""
			if parsed, err := url.Parse(r.PostForm.Get("Input.ReturnUrl")); err == nil {
				state = parsed.Query().Get("state")
			}
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
		if r.PostForm.Get("Input.MfaCode") != "123456" || r.PostForm.Get("X-Unknown-Preserved") != "keep-me-mfa" {
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
	mux.HandleFunc("/appointments/api/v2/search-appointments/filters/initial-filters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"regions":[]}`))
	})
	mux.HandleFunc("/appointments/api/v2/search-appointments/filters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clinics":[]}`))
	})
	mux.HandleFunc("/appointments/api/v2/search-appointments/slots", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"bookingString":"safe-booking","appointmentDate":"2026-09-12T09:00:00","doctor":null,"clinic":{"name":"Clinic A"},"specialty":{"name":"Cardiology"},"visitType":"Center"}],"totalPages":1}`))
	})
	server := httptest.NewServer(mux)
	fake.baseURL = server.URL
	fake.redirectURI = server.URL + "/signin-oidc"
	return fake, server.Close
}

func TestDryCheckViaExecutableDoesNotChangeDatabase(t *testing.T) {
	fake, cleanup := newAuthFake(t)
	defer cleanup()
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := writeProcessSecretFile(t, secretDir, "password", "plain-pass")
	env := []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}
	if got := run(t, env, "account", "create", "--database", databasePath, "--non-interactive", "--account", "alice", "--username", "plain-user@example.com", "--password-file", passwordFile); got.exitCode != 0 {
		t.Fatal(got)
	}
	if got := run(t, env, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "cardio", "--account", "alice", "--region", "204", "--specialty", "132", "--clinic", "10", "--doctor", "20", "--language", "4", "--visit-type", "Center", "--search-type", "Standard", "--start-date", "2026-09-01", "--end-date", "2026-09-30", "--check-interval-minutes", "30"); got.exitCode != 0 {
		t.Fatal(got)
	}
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, env, "check", "--dry", "--database", databasePath, "--non-interactive", "--profile", "cardio", "--output", "json")
	if got.exitCode != 0 || got.stderr != "" || !strings.Contains(got.stdout, `"complete":true`) || !strings.Contains(got.stdout, `"booking_string":"safe-booking"`) {
		t.Fatalf("result=%#v", got)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry check changed the database")
	}
}

func hasAuthCookie(r *http.Request) bool {
	cookie, err := r.Cookie("MedicoverTrusted")
	return err == nil && cookie.Value == "trusted-1"
}

func TestAuthLoginLogoutStatusViaExecutable(t *testing.T) {
	fake, cleanup := newAuthFake(t)
	defer cleanup()
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := writeProcessSecretFile(t, secretDir, "pw", "plain-pass")

	created := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "alice", "--username", "plain-user@example.com", "--password-file", passwordFile)
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}

	login := run(t, []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}, "account", "login", "--database", databasePath, "--non-interactive", "--account", "alice")
	if login.exitCode != 0 {
		t.Fatalf("login = %#v", login)
	}
	if !strings.Contains(login.stdout, "Logged in account alice") && !strings.Contains(login.stdout, "Reused trusted session") {
		t.Fatalf("login stdout = %q", login.stdout)
	}
	assertProcessFileMode(t, filepath.Join(sessionDir, "sessions", "alice.json"), 0o600)
	assertProcessFileMode(t, filepath.Join(sessionDir, "sessions"), 0o700)

	// Second login reuses the trusted session.
	second := run(t, []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}, "account", "login", "--database", databasePath, "--non-interactive", "--account", "alice")
	if second.exitCode != 0 || !strings.Contains(second.stdout, "Reused trusted session") {
		t.Fatalf("second login = %#v", second)
	}

	status := run(t, []string{"MEDALERT_SESSION_DIR=" + sessionDir}, "account", "status", "--database", databasePath, "--account", "alice", "--output", "json")
	if status.exitCode != 0 || !strings.Contains(status.stdout, `"authenticated":true`) {
		t.Fatalf("status = %#v", status)
	}

	logout := run(t, []string{"MEDALERT_SESSION_DIR=" + sessionDir}, "account", "logout", "--database", databasePath, "--account", "alice")
	if logout.exitCode != 0 {
		t.Fatalf("logout = %#v", logout)
	}
	after := run(t, []string{"MEDALERT_SESSION_DIR=" + sessionDir}, "account", "status", "--database", databasePath, "--account", "alice", "--output", "json")
	if after.exitCode != 3 || !strings.Contains(after.stdout, `"authenticated":false`) {
		t.Fatalf("status after logout = %#v", after)
	}
}

func TestAuthMFAViaExecutable(t *testing.T) {
	fake, cleanup := newAuthFake(t)
	defer cleanup()
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := writeProcessSecretFile(t, secretDir, "pw", "mfa-pass")
	created := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "mfa", "--username", "mfa-user@example.com", "--password-file", passwordFile)
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}

	withoutCode := run(t, []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}, "account", "login", "--database", databasePath, "--non-interactive", "--account", "mfa")
	if withoutCode.exitCode != 3 {
		t.Fatalf("login without MFA = %#v, want exit 3", withoutCode)
	}
	mfaFile := writeProcessSecretFile(t, secretDir, "mfa", "123456")
	withCode := run(t, []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}, "account", "login", "--database", databasePath, "--non-interactive", "--account", "mfa", "--mfa-code-file", mfaFile)
	if withCode.exitCode != 0 {
		t.Fatalf("MFA login = %#v", withCode)
	}
}

func TestAuthFailuresAndRedactionViaExecutable(t *testing.T) {
	fake, cleanup := newAuthFake(t)
	defer cleanup()
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "MARKER-SECRET-" + t.Name() + "-unique-999"
	passwordFile := writeProcessSecretFile(t, secretDir, "pw", marker)
	created := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "alice", "--username", "plain-user@example.com", "--password-file", passwordFile)
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}
	// Wrong password (marker) must fail with exit 3 and never leak the marker.
	failed := run(t, []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}, "account", "login", "--database", databasePath, "--non-interactive", "--account", "alice", "--output", "json")
	if failed.exitCode != 3 {
		t.Fatalf("wrong password login = %#v, want exit 3", failed)
	}
	if strings.Contains(failed.stdout, marker) || strings.Contains(failed.stderr, marker) {
		t.Fatalf("output leaks secret: stdout=%q stderr=%q", failed.stdout, failed.stderr)
	}
	if !strings.Contains(failed.stderr, "invalid_credentials") && !strings.Contains(failed.stderr, "authentication_required") {
		t.Fatalf("stderr = %q, want auth failure (not protocol_changed)", failed.stderr)
	}
	if strings.Contains(failed.stderr, "protocol_changed") {
		t.Fatalf("wrong password reported as protocol_changed: %q", failed.stderr)
	}
	raw, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatal("database contains secret")
	}
	sessionFiles, _ := filepath.Glob(filepath.Join(sessionDir, "sessions", "*.json"))
	for _, path := range sessionFiles {
		contents, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(contents), marker) {
			t.Fatalf("session file %s contains password marker", path)
		}
	}
}

func TestAuthPerAccountIndependenceViaExecutable(t *testing.T) {
	fake, cleanup := newAuthFake(t)
	defer cleanup()
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := writeProcessSecretFile(t, secretDir, "pw", "plain-pass")
	for _, id := range []string{"alice", "bob"} {
		created := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", id, "--username", "plain-user@example.com", "--password-file", passwordFile)
		if created.exitCode != 0 {
			t.Fatalf("create %s = %#v", id, created)
		}
		logged := run(t, []string{"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}, "account", "login", "--database", databasePath, "--non-interactive", "--account", id)
		if logged.exitCode != 0 {
			t.Fatalf("login %s = %#v", id, logged)
		}
	}
	// Logging out alice must not affect bob.
	logout := run(t, []string{"MEDALERT_SESSION_DIR=" + sessionDir}, "account", "logout", "--database", databasePath, "--account", "alice")
	if logout.exitCode != 0 {
		t.Fatalf("logout alice = %#v", logout)
	}
	bobStatus := run(t, []string{"MEDALERT_SESSION_DIR=" + sessionDir}, "account", "status", "--database", databasePath, "--account", "bob")
	if bobStatus.exitCode != 0 {
		t.Fatalf("bob status after alice logout = %#v, want independent success", bobStatus)
	}
	aliceStatus := run(t, []string{"MEDALERT_SESSION_DIR=" + sessionDir}, "account", "status", "--database", databasePath, "--account", "alice")
	if aliceStatus.exitCode != 3 {
		t.Fatalf("alice status after logout = %#v, want exit 3", aliceStatus)
	}
}

func TestAuthDoesNotTouchMediCzuwaczFiles(t *testing.T) {
	fake, cleanup := newAuthFake(t)
	defer cleanup()
	root := privateTempDir(t)
	home := t.TempDir()
	databasePath := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "data")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant fake MediCzuwacz files that the Go port must never touch.
	mediczuwaczCookie := filepath.Join(home, ".mediczuwacz-cookies.txt")
	mediczuwaczDevice := filepath.Join(home, ".mediczuwacz-device.txt")
	if err := os.WriteFile(mediczuwaczCookie, []byte("cookie"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediczuwaczDevice, []byte("device"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeCookie, _ := os.ReadFile(mediczuwaczCookie)
	beforeDevice, _ := os.ReadFile(mediczuwaczDevice)

	passwordFile := writeProcessSecretFile(t, secretDir, "pw", "plain-pass")
	env := []string{"HOME=" + home, "MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL, "MEDALERT_SESSION_DIR=" + sessionDir}
	created := run(t, env, "account", "create", "--database", databasePath, "--non-interactive", "--account", "alice", "--username", "plain-user@example.com", "--password-file", passwordFile)
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}
	logged := run(t, env, "account", "login", "--database", databasePath, "--non-interactive", "--account", "alice")
	if logged.exitCode != 0 {
		t.Fatalf("login = %#v", logged)
	}
	afterCookie, _ := os.ReadFile(mediczuwaczCookie)
	afterDevice, _ := os.ReadFile(mediczuwaczDevice)
	if string(beforeCookie) != string(afterCookie) || string(beforeDevice) != string(afterDevice) {
		t.Fatal("MediCzuwacz files were modified")
	}
}
