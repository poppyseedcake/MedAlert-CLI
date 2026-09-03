package medicover_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
)

// fakeMedicover is a sanitized local stand-in for the Medicover OIDC,
// password, MFA, and token endpoints. It uses only test credentials and
// never contacts production.
type fakeMedicover struct {
	t *testing.T

	server      *httptest.Server
	baseURL     string
	redirectURI string

	mu sync.Mutex

	users    map[string]string
	mfaUsers map[string]bool
	mfaCode  string

	// pendingStates tracks authorize state -> challenge for PKCE checks.
	pendingStates map[string]string
	// lastState tracks the most recent authorize state for single-flight
	// login flows. Map iteration is random, so code issuance must use the
	// newest state, not an arbitrary entry.
	lastState     string
	lastChallenge string
	// codes tracks issued codes -> state/challenge/username.
	codes map[string]fakeCode
	// refreshTokens tracks refresh -> username.
	refreshTokens map[string]string

	lastAuthorizeQuery url.Values
	lastLoginForm      url.Values
	lastMFAForm        url.Values

	tokenHits   int
	refreshHits int

	// Failure injection.
	wrongState       bool
	wrongIssuer      bool
	evilRedirect     bool
	redirectLoop     bool
	missingLoginForm bool
	rateLimitToken   bool
	tokenServerError bool
	changedAction    bool
}

type fakeCode struct {
	state       string
	challenge   string
	username    string
	redirectURI string
}

func newFake(t *testing.T) *fakeMedicover {
	t.Helper()
	fake := &fakeMedicover{
		t:             t,
		users:         map[string]string{"user-no-mfa@example.com": "pass-no-mfa-123", "user-mfa@example.com": "pass-mfa-123"},
		mfaUsers:      map[string]bool{"user-mfa@example.com": true},
		mfaCode:       "123456",
		pendingStates: map[string]string{},
		codes:         map[string]fakeCode{},
		refreshTokens: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", fake.handleDiscovery)
	mux.HandleFunc("/connect/authorize", fake.handleAuthorize)
	mux.HandleFunc("/Account/Login", fake.handleLogin)
	mux.HandleFunc("/Account/LoginChanged", fake.handleLogin)
	mux.HandleFunc("/Mfa", fake.handleMFAGet)
	mux.HandleFunc("/mfa-changed", fake.handleMFAGet)
	mux.HandleFunc("/MFA/Post", fake.handleMFAPost)
	mux.HandleFunc("/connect/token", fake.handleToken)
	server := httptest.NewServer(mux)
	fake.server = server
	fake.baseURL = server.URL
	fake.redirectURI = server.URL + "/signin-oidc"
	t.Cleanup(server.Close)
	return fake
}

func (f *fakeMedicover) clientConfig() medicover.Config {
	return medicover.Config{
		Issuer:      f.baseURL,
		ClientID:    "web",
		RedirectURI: f.redirectURI,
	}
}

func (f *fakeMedicover) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	document := map[string]string{
		"issuer":                 f.baseURL,
		"authorization_endpoint": f.baseURL + "/connect/authorize",
		"token_endpoint":         f.baseURL + "/connect/token",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(document)
}

func (f *fakeMedicover) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	f.mu.Lock()
	f.lastAuthorizeQuery = query
	f.mu.Unlock()

	if f.redirectLoop {
		http.Redirect(w, r, "/connect/authorize?loop=1", http.StatusFound)
		return
	}
	if f.evilRedirect {
		http.Redirect(w, r, "https://evil.example/callback?code=fake", http.StatusFound)
		return
	}
	// Validate required OIDC parameters. A real server would redirect to an
	// error page; the fake returns 400 so the client treats it as a protocol
	// change instead of a silent reuse miss.
	required := []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method", "response_mode"}
	for _, key := range required {
		if strings.TrimSpace(query.Get(key)) == "" {
			http.Error(w, "missing "+key, http.StatusBadRequest)
			return
		}
	}
	if query.Get("code_challenge_method") != "S256" {
		http.Error(w, "PKCE must be S256", http.StatusBadRequest)
		return
	}
	state := query.Get("state")
	challenge := query.Get("code_challenge")
	f.mu.Lock()
	f.pendingStates[state] = challenge
	f.lastState = state
	f.lastChallenge = challenge
	f.mu.Unlock()

	// Trusted reuse: a valid trusted cookie returns a code immediately.
	if hasCookie(r, "MedicoverTrusted", "trusted-1") {
		code := "code-reuse-" + state[:8]
		f.mu.Lock()
		f.codes[code] = fakeCode{state: state, challenge: challenge, username: "reused", redirectURI: query.Get("redirect_uri")}
		f.mu.Unlock()
		returnedState := state
		if f.wrongState {
			returnedState = "wrong-state"
		}
		issuer := f.baseURL
		if f.wrongIssuer {
			issuer = "https://evil.example"
		}
		location := query.Get("redirect_uri") + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(returnedState) + "&iss=" + url.QueryEscape(issuer)
		http.Redirect(w, r, location, http.StatusFound)
		// Refresh the trusted cookie.
		http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/", HttpOnly: true})
		return
	}
	// Otherwise redirect to the login page.
	loginPath := "/Account/Login"
	if f.changedAction {
		loginPath = "/Account/LoginChanged"
	}
	returnURL := "/connect/authorize?" + r.URL.RawQuery
	http.Redirect(w, r, loginPath+"?ReturnUrl="+url.QueryEscape(returnURL), http.StatusFound)
}

func (f *fakeMedicover) loginHTML(returnURL string) string {
	action := "/Account/Login"
	if f.changedAction {
		action = "/Account/LoginChanged"
	}
	if f.missingLoginForm {
		return "<html><body>No form here</body></html>"
	}
	return `<html><body><form method="post" action="` + action + `">` +
		`<input type="hidden" name="__RequestVerificationToken" value="token-123" />` +
		`<input type="hidden" name="Input.ReturnUrl" value="` + htmlEscape(returnURL) + `" />` +
		`<input type="hidden" name="X-Unknown-Preserved" value="keep-me" />` +
		`<input type="hidden" name="X-Another-Field" value="also-kept" />` +
		`<input type="text" name="Input.Username" value="" />` +
		`<input type="password" name="Input.Password" />` +
		`<button type="submit" name="Input.Button" value="login">Login</button>` +
		`</form></body></html>`
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}

func (f *fakeMedicover) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		returnURL := r.URL.Query().Get("ReturnUrl")
		if returnURL == "" {
			returnURL = "/connect/authorize"
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, f.loginHTML(returnURL))
		return
	}
	// POST
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.lastLoginForm = r.PostForm
	f.mu.Unlock()

	// Hidden preservation is required: the server rejects posts that drop
	// required or unknown hidden fields.
	if r.PostForm.Get("__RequestVerificationToken") == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("X-Unknown-Preserved") != "keep-me" {
		// Return a page without a form so the client reports a protocol
		// change. Tests assert preservation by inspecting lastLoginForm.
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>hidden field was dropped</body></html>")
		return
	}
	username := r.PostForm.Get("Input.Username")
	password := r.PostForm.Get("Input.Password")
	if username == "" {
		username = r.PostForm.Get("Input.UserName")
	}
	expected, ok := f.users[username]
	if !ok || password != expected {
		// Wrong credentials return the same login form (200), never a
		// protocol error.
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, f.loginHTML(r.PostForm.Get("Input.ReturnUrl")))
		return
	}
	// Extract the original OIDC state from ReturnUrl for the code redirect.
	state := ""
	challenge := ""
	redirectURI := f.redirectURI
	if returnURL := r.PostForm.Get("Input.ReturnUrl"); returnURL != "" {
		if parsed, err := url.Parse(returnURL); err == nil {
			state = parsed.Query().Get("state")
			redirectURIParam := parsed.Query().Get("redirect_uri")
			if redirectURIParam != "" {
				redirectURI = redirectURIParam
			}
		} else if strings.Contains(returnURL, "state=") {
			state = "fallback"
		}
	}
	if state == "" {
		// Fall back to the most recent pending state (single-flight tests).
		f.mu.Lock()
		state = f.lastState
		f.mu.Unlock()
	}
	f.mu.Lock()
	challenge = f.pendingStates[state]
	f.mu.Unlock()

	if f.mfaUsers[username] {
		http.SetCookie(w, &http.Cookie{Name: "MFA-Pending", Value: username, Path: "/", HttpOnly: true})
		mfaPath := "/Mfa"
		if f.changedAction {
			mfaPath = "/mfa-changed"
		}
		http.Redirect(w, r, mfaPath, http.StatusFound)
		return
	}
	code := "code-login-" + randomSuffix()
	f.mu.Lock()
	f.codes[code] = fakeCode{state: state, challenge: challenge, username: username, redirectURI: redirectURI}
	f.mu.Unlock()
	returnedState := state
	if f.wrongState {
		returnedState = "wrong-state"
	}
	issuer := f.baseURL
	if f.wrongIssuer {
		issuer = "https://evil.example"
	}
	http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/", HttpOnly: true})
	location := redirectURI + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(returnedState) + "&iss=" + url.QueryEscape(issuer)
	http.Redirect(w, r, location, http.StatusFound)
}

func (f *fakeMedicover) mfaHTML() string {
	return `<html><body><form method="post" action="/MFA/Post">` +
		`<input type="hidden" name="__RequestVerificationToken" value="mfa-token-456" />` +
		`<input type="hidden" name="X-Unknown-Preserved" value="keep-me-mfa" />` +
		`<input type="text" name="Input.MfaCode" value="" />` +
		`</form></body></html>`
}

func (f *fakeMedicover) handleMFAGet(w http.ResponseWriter, r *http.Request) {
	// Trusted skip: a trusted cookie returns a code without showing a form.
	if hasCookie(r, "MedicoverTrusted", "trusted-1") && !hasCookie(r, "MFA-Pending", "") {
		// Use the most recent pending state for the code.
		f.mu.Lock()
		state := f.lastState
		challenge := f.lastChallenge
		f.mu.Unlock()
		if state != "" {
			code := "code-mfa-trusted-" + randomSuffix()
			f.mu.Lock()
			f.codes[code] = fakeCode{state: state, challenge: challenge, username: "trusted", redirectURI: f.redirectURI}
			f.mu.Unlock()
			location := f.redirectURI + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
			http.Redirect(w, r, location, http.StatusFound)
			return
		}
	}
	// If MFA was just requested via password, the pending cookie exists.
	// Serve the MFA form either way for test simplicity.
	w.Header().Set("Content-Type", "text/html")
	_, _ = io.WriteString(w, f.mfaHTML())
}

func (f *fakeMedicover) handleMFAPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.lastMFAForm = r.PostForm
	f.mu.Unlock()

	if r.PostForm.Get("__RequestVerificationToken") == "" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>missing MFA token</body></html>")
		return
	}
	if r.PostForm.Get("X-Unknown-Preserved") != "keep-me-mfa" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>hidden MFA field was dropped</body></html>")
		return
	}
	// Trusted-device markers are required.
	if r.PostForm.Get("Input.IsTrustedDevice") != "true" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body>missing trusted marker</body></html>")
		return
	}
	codeValue := r.PostForm.Get("Input.MfaCode")
	if codeValue != f.mfaCode {
		// Wrong code returns the same MFA form (200) so the client reports
		// invalid credentials, not a protocol change.
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, f.mfaHTML())
		return
	}
	pendingUser := cookieValue(r, "MFA-Pending")
	if pendingUser == "" {
		pendingUser = "mfa-user"
	}
	f.mu.Lock()
	state := f.lastState
	challenge := f.lastChallenge
	f.mu.Unlock()
	code := "code-mfa-" + randomSuffix()
	f.mu.Lock()
	f.codes[code] = fakeCode{state: state, challenge: challenge, username: pendingUser, redirectURI: f.redirectURI}
	f.mu.Unlock()
	returnedState := state
	if f.wrongState {
		returnedState = "wrong-state"
	}
	http.SetCookie(w, &http.Cookie{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/", HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: "MFA-Pending", Value: "", Path: "/", MaxAge: -1})
	location := f.redirectURI + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(returnedState)
	http.Redirect(w, r, location, http.StatusFound)
}

func (f *fakeMedicover) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenHits++
	f.mu.Unlock()
	if f.rateLimitToken {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if f.tokenServerError {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"server_error"}`)
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_request"}`)
		return
	}
	grant := r.PostForm.Get("grant_type")
	switch grant {
	case "authorization_code":
		code := r.PostForm.Get("code")
		verifier := r.PostForm.Get("code_verifier")
		f.mu.Lock()
		stored, ok := f.codes[code]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"unknown code"}`)
			return
		}
		// Validate PKCE S256.
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if challenge != stored.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"PKCE mismatch"}`)
			return
		}
		if r.PostForm.Get("redirect_uri") == "" || r.PostForm.Get("client_id") != "web" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_request"}`)
			return
		}
		// One-time codes.
		f.mu.Lock()
		delete(f.codes, code)
		access := "access-" + randomSuffix()
		refresh := "refresh-" + randomSuffix()
		f.refreshTokens[refresh] = stored.username
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"expires_in":    300,
			"token_type":    "Bearer",
		})
	case "refresh_token":
		refresh := r.PostForm.Get("refresh_token")
		f.mu.Lock()
		username, ok := f.refreshTokens[refresh]
		f.refreshHits++
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"unknown refresh"}`)
			return
		}
		f.mu.Lock()
		access := "access-refreshed-" + randomSuffix()
		newRefresh := "refresh-rotated-" + randomSuffix()
		delete(f.refreshTokens, refresh)
		f.refreshTokens[newRefresh] = username
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": newRefresh,
			"expires_in":    300,
			"token_type":    "Bearer",
		})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unsupported_grant_type"}`)
	}
}

func hasCookie(r *http.Request, name, value string) bool {
	cookie, err := r.Cookie(name)
	if err != nil {
		return false
	}
	if value == "" {
		return true
	}
	return cookie.Value == value
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func TestDiscoverySupportsCodeFlowAndS256(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	authorize, token, err := client.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !strings.Contains(authorize, "/connect/authorize") || !strings.Contains(token, "/connect/token") {
		t.Fatalf("endpoints = %q %q", authorize, token)
	}
}

func TestPasswordLoginWithoutMFA(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	result, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if result.AccessToken == "" || result.Session == nil || result.Reused || result.MFAUsed {
		t.Fatalf("result = %+v", result)
	}
	if strings.TrimSpace(result.Session.RefreshToken) == "" || strings.TrimSpace(result.Session.DeviceID) == "" {
		t.Fatalf("session = %+v", result.Session)
	}
	// Hidden fields must be preserved without exposing values.
	fake.mu.Lock()
	loginForm := fake.lastLoginForm
	fake.mu.Unlock()
	if loginForm.Get("X-Unknown-Preserved") != "keep-me" || loginForm.Get("__RequestVerificationToken") == "" {
		t.Fatalf("hidden fields were not preserved: %v", loginForm)
	}
	// OIDC state and PKCE must be validated: the authorize query must carry
	// S256 and a device id.
	fake.mu.Lock()
	authorizeQuery := fake.lastAuthorizeQuery
	fake.mu.Unlock()
	if authorizeQuery.Get("code_challenge_method") != "S256" || authorizeQuery.Get("device_id") == "" || authorizeQuery.Get("state") == "" {
		t.Fatalf("authorize query = %v", authorizeQuery)
	}
}

func TestMFALogin(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	// Without a code the client must report MFA required, not success.
	_, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-mfa@example.com",
		Password: "pass-mfa-123",
	})
	if err == nil || !isMFARequired(err) {
		t.Fatalf("err = %v, want MFA required", err)
	}
	result, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-mfa@example.com",
		Password: "pass-mfa-123",
		MFACode:  "123456",
	})
	if err != nil {
		t.Fatalf("MFA login: %v", err)
	}
	if !result.MFAUsed || result.AccessToken == "" {
		t.Fatalf("result = %+v", result)
	}
	fake.mu.Lock()
	mfaForm := fake.lastMFAForm
	fake.mu.Unlock()
	if mfaForm.Get("X-Unknown-Preserved") != "keep-me-mfa" || mfaForm.Get("Input.IsTrustedDevice") != "true" {
		t.Fatalf("MFA hidden fields were not preserved: %v", mfaForm)
	}
}

func isMFARequired(err error) bool {
	var medicoverErr *medicover.Error
	if ok := errorAs(err, &medicoverErr); !ok {
		return false
	}
	return medicoverErr.Code == medicover.CodeMFARequired
}

func errorAs(err error, target any) bool {
	type causer interface{ As(any) bool }
	// Use a minimal errors.As without importing errors twice.
	for err != nil {
		if ok := tryAssign(err, target); ok {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func tryAssign(err error, target any) bool {
	if pointer, ok := target.(**medicover.Error); ok {
		if value, ok := err.(*medicover.Error); ok {
			*pointer = value
			return true
		}
	}
	return false
}

func TestTrustedReuse(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	first, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	second, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Session: first.Session,
	})
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if !second.Reused || second.AccessToken == "" {
		t.Fatalf("reuse result = %+v", second)
	}
}

func TestRenewalBeforeExpiry(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	login, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	refreshed, newSession, err := client.Refresh(context.Background(), login.Session)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.AccessToken == "" || refreshed.AccessToken == login.AccessToken {
		t.Fatalf("refresh did not rotate the access token")
	}
	if newSession.RefreshToken == login.Session.RefreshToken {
		t.Fatalf("refresh token was not rotated")
	}
}

func TestInvalidGrantBecomesAuthRequired(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	_, _, err := client.Refresh(context.Background(), &medicover.SessionState{DeviceID: "device-1", RefreshToken: "bad-refresh"})
	if err == nil {
		t.Fatal("refresh succeeded with a bad token")
	}
	var medicoverErr *medicover.Error
	if !errorAs(err, &medicoverErr) || medicoverErr.Code != medicover.CodeAuthRequired {
		t.Fatalf("err = %v, want auth required", err)
	}
}

func TestWrongStateFails(t *testing.T) {
	fake := newFake(t)
	fake.wrongState = true
	client := medicover.NewClient(fake.clientConfig())
	_, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err == nil {
		t.Fatal("login succeeded with a wrong state")
	}
}

func TestRateLimitIncludesRetryAfter(t *testing.T) {
	fake := newFake(t)
	fake.rateLimitToken = true
	client := medicover.NewClient(fake.clientConfig())
	_, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err == nil {
		t.Fatal("login succeeded despite rate limiting")
	}
	var medicoverErr *medicover.Error
	if !errorAs(err, &medicoverErr) || medicoverErr.Code != medicover.CodeRateLimited {
		t.Fatalf("err = %v, want rate limited", err)
	}
	if medicoverErr.RetryAfter <= 0 {
		t.Fatalf("retry after = %v, want positive", medicoverErr.RetryAfter)
	}
}

func TestInvalidCredentialsAreNotProtocolErrors(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	_, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "wrong-password",
	})
	if err == nil {
		t.Fatal("login succeeded with a wrong password")
	}
	var medicoverErr *medicover.Error
	if !errorAs(err, &medicoverErr) || medicoverErr.Code != medicover.CodeInvalidCredentials {
		t.Fatalf("err = %v, want invalid credentials", err)
	}
}

func TestChangedFormActionIsAccepted(t *testing.T) {
	fake := newFake(t)
	fake.changedAction = true
	client := medicover.NewClient(fake.clientConfig())
	result, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("login with changed action: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatalf("empty access token")
	}
}

func TestTokenErrorsDoNotExposeSecrets(t *testing.T) {
	fake := newFake(t)
	passwordMarker := "PASSWORD-MARKER-" + t.Name() + "-secret"
	mfaMarker := "654321"
	client := medicover.NewClient(fake.clientConfig())
	_, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: medicover.Secret(passwordMarker),
		MFACode:  medicover.Secret(mfaMarker),
	})
	if err == nil {
		t.Fatal("login succeeded with a wrong password")
	}
	text := err.Error()
	if strings.Contains(text, passwordMarker) || strings.Contains(text, mfaMarker) {
		t.Fatalf("error leaks secrets: %q", text)
	}
}

func TestConcurrentRefreshSharesOneOperation(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	login, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	coordinator := medicover.NewCoordinator()
	start := make(chan struct{})
	results := make(chan error, 5)
	for range 5 {
		go func() {
			<-start
			_, _, err := coordinator.RefreshOne(context.Background(), client, "alice", login.Session)
			results <- err
		}()
	}
	close(start)
	for range 5 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent refresh: %v", err)
		}
	}
	fake.mu.Lock()
	refreshHits := fake.refreshHits
	fake.mu.Unlock()
	// Five concurrent callers share refresh calls, but the fake rotates the
	// refresh token on each use, so only the first batch can share. The key
	// property is that concurrent callers do not issue five independent
	// token calls for the same original session: the coordinator dedups the
	// in-flight operation. Because rotation invalidates the original token,
	// later batches would fail; here all five share the first call.
	if refreshHits != 1 {
		t.Fatalf("refresh hits = %d, want 1 (shared operation)", refreshHits)
	}
}

func TestEnsureValidRefreshesExpiringToken(t *testing.T) {
	fake := newFake(t)
	now := time.Now()
	clock := func() time.Time { return now }
	cfg := fake.clientConfig()
	cfg.Clock = clock
	client := medicover.NewClient(cfg)
	login, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	current := medicover.Tokens{AccessToken: login.AccessToken, ExpiresAt: login.ExpiresAt, RefreshToken: ""}
	// A token valid for 5 minutes does not need renewal.
	if kept, _, err := client.EnsureValid(context.Background(), login.Session, current); err != nil || kept.AccessToken != current.AccessToken {
		t.Fatalf("valid token was refreshed: %+v %v", kept, err)
	}
	// An expiring token (10s left) is renewed before expiry.
	expiring := medicover.Tokens{AccessToken: login.AccessToken, ExpiresAt: now.Add(10 * time.Second)}
	refreshed, newSession, err := client.EnsureValid(context.Background(), login.Session, expiring)
	if err != nil {
		t.Fatalf("ensure valid: %v", err)
	}
	if refreshed.AccessToken == "" || refreshed.AccessToken == login.AccessToken {
		t.Fatalf("expiring token was not renewed")
	}
	if newSession.RefreshToken == login.Session.RefreshToken {
		t.Fatalf("refresh token was not rotated")
	}
	if !medicover.NeedsRefresh(now.Add(10*time.Second), now) {
		t.Fatal("NeedsRefresh missed an expiring token")
	}
	if medicover.NeedsRefresh(now.Add(5*time.Minute), now) {
		t.Fatal("NeedsRefresh flagged a valid token")
	}
}

func TestFirstVersionNeverTouchesMediCzuwaczFiles(t *testing.T) {
	// The Go port must not import, edit, or delete MediCzuwacz cookie or
	// device files. Plain mentions of the project name in comments and docs
	// are allowed for attribution; direct file access is not.
	for _, path := range []string{
		"internal/medicover/medicover.go",
		"internal/medicover/http.go",
		"internal/medicover/form.go",
		"internal/medicover/cookies.go",
		"internal/medicover/coordinator.go",
		"internal/session/session.go",
		"internal/cli/auth.go",
	} {
		contents, err := readTestFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lowered := strings.ToLower(string(contents))
		for _, forbidden := range []string{"/data/cookies", "mozilla", ".mozilla", "cookies.txt", "/data/keyring", "mediczuwacz/*.json", "mediczuwacz/cookies"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("%s references forbidden MediCzuwacz location %q", path, forbidden)
			}
		}
		// No production source may open a MediCzuwacz path.
		if strings.Contains(lowered, "os.open") && strings.Contains(lowered, "mediczuwacz") {
			t.Fatalf("%s opens a MediCzuwacz path", path)
		}
	}
}

func readTestFile(path string) ([]byte, error) {
	// Tests run from the package directory; resolve the repo root.
	for _, prefix := range []string{"../../", "../../../", "./"} {
		candidate := prefix + path
		if contents, err := os.ReadFile(candidate); err == nil {
			return contents, nil
		}
	}
	return nil, fmt.Errorf("cannot find %s", path)
}

func TestAuthorizeTSUsesMilliseconds(t *testing.T) {
	fake := newFake(t)
	client := medicover.NewClient(fake.clientConfig())
	if _, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	fake.mu.Lock()
	ts := fake.lastAuthorizeQuery.Get("ts")
	fake.mu.Unlock()
	if len(ts) < 13 {
		t.Fatalf("ts = %q, want Unix milliseconds (13+ digits)", ts)
	}
}

func TestIntermediateAuthorizeCallbackIsFollowed(t *testing.T) {
	// A login POST that intermediates through /connect/authorize/callback
	// must still yield a code instead of protocol_changed.
	var baseURL, redirectURI string
	pending := map[string]string{}
	codes := map[string]string{}
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 baseURL,
			"authorization_endpoint": baseURL + "/connect/authorize",
			"token_endpoint":         baseURL + "/connect/token",
		})
	})
	mux.HandleFunc("/connect/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		pending[query.Get("state")] = query.Get("code_challenge")
		mu.Unlock()
		http.Redirect(w, r, "/Account/Login?ReturnUrl="+url.QueryEscape("/connect/authorize?"+r.URL.RawQuery), http.StatusFound)
	})
	mux.HandleFunc("/Account/Login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><body><form method="post" action="/Account/Login">`+
				`<input type="hidden" name="__RequestVerificationToken" value="t" />`+
				`<input type="hidden" name="Input.ReturnUrl" value="`+r.URL.Query().Get("ReturnUrl")+`" />`+
				`<input type="text" name="Input.Username" /><input type="password" name="Input.Password" /></form></body></html>`)
			return
		}
		_ = r.ParseForm()
		state := ""
		if parsed, err := url.Parse(r.PostForm.Get("Input.ReturnUrl")); err == nil {
			state = parsed.Query().Get("state")
		}
		// Intermediate step instead of a direct callback redirect.
		http.Redirect(w, r, "/connect/authorize/callback?state="+url.QueryEscape(state), http.StatusFound)
	})
	mux.HandleFunc("/connect/authorize/callback", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		mu.Lock()
		_ = pending[state]
		code := "code-intermediate"
		codes[code] = state
		mu.Unlock()
		http.Redirect(w, r, redirectURI+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusFound)
	})
	mux.HandleFunc("/connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.PostForm.Get("code")
		verifier := r.PostForm.Get("code_verifier")
		mu.Lock()
		state, ok := codes[code]
		challenge := pending[state]
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 300})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL = server.URL
	redirectURI = server.URL + "/signin-oidc"
	client := medicover.NewClient(medicover.Config{Issuer: baseURL, ClientID: "web", RedirectURI: redirectURI})
	result, err := client.Authenticate(context.Background(), medicover.AuthRequest{
		Username: "user-no-mfa@example.com",
		Password: "pass-no-mfa-123",
	})
	if err != nil {
		t.Fatalf("login via intermediate callback: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("empty access token")
	}
}
