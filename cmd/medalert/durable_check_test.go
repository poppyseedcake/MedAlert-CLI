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

	"github.com/poppyseedcake/MedAlert/internal/store"
)

type durableCheckFake struct {
	mu             sync.Mutex
	baseURL        string
	redirectURI    string
	slots          []map[string]any
	searchFailure  bool
	pending        map[string]string
	codes          map[string]string
	authRequests   int
	searchRequests int
	afterToken     func() error
	afterTokenErr  error
}

func newDurableCheckFake(t *testing.T) (*durableCheckFake, func()) {
	t.Helper()
	fake := &durableCheckFake{
		pending: map[string]string{},
		codes:   map[string]string{},
	}
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
		fake.authRequests++
		fake.pending[state] = query.Get("code_challenge")
		fake.mu.Unlock()
		if _, err := r.Cookie("MedicoverTrusted"); err == nil {
			code := "trusted-code"
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
		if r.PostForm.Get("Input.Username") != "patient@example.com" || r.PostForm.Get("Input.Password") != "durable-pass" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form><input type="password" name="Input.Password" /></form></body></html>`))
			return
		}
		state := extractDurableState(r.PostForm.Get("Input.ReturnUrl"))
		code := "login-code"
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
		afterToken := fake.afterToken
		fake.mu.Unlock()
		if !ok || base64.RawURLEncoding.EncodeToString(mustSHA256(verifier)) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if afterToken != nil {
			if callbackErr := afterToken(); callbackErr != nil {
				fake.mu.Lock()
				fake.afterTokenErr = callbackErr
				fake.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "durable-access",
			"refresh_token": "durable-refresh",
			"expires_in":    300,
		})
	})
	searchHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fake.mu.Lock()
		fake.searchRequests++
		failure := fake.searchFailure
		slots := append([]map[string]any(nil), fake.slots...)
		fake.mu.Unlock()
		if failure {
			w.WriteHeader(http.StatusBadGateway)
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

func extractDurableState(returnURL string) string {
	parsed, err := url.Parse(returnURL)
	if err != nil {
		return ""
	}
	return parsed.Query().Get("state")
}

func mustSHA256(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func (f *durableCheckFake) setSlots(slots ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots = append([]map[string]any(nil), slots...)
	f.searchFailure = false
}

func (f *durableCheckFake) failSearch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchFailure = true
}

func (f *durableCheckFake) authRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authRequests
}

func (f *durableCheckFake) searchRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchRequests
}

func (f *durableCheckFake) setAfterToken(callback func() error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterToken = callback
}

func durableSlot(booking string) map[string]any {
	return map[string]any{
		"bookingString":   booking,
		"appointmentDate": "2099-09-10T10:00:00Z",
		"clinic":          map[string]any{"name": "Main clinic"},
		"doctor":          map[string]any{"name": "Dr Example"},
		"specialty":       map[string]any{"name": "Cardiology"},
		"visitType":       "Center",
	}
}

type durableCheckData struct {
	Dry            bool `json:"dry"`
	Complete       bool `json:"complete"`
	NewlyAvailable int  `json:"newly_available"`
	Ended          int  `json:"ended"`
	ActiveEpisodes int  `json:"active_episode_count"`
}

func runDurableCheck(t *testing.T, environment []string) durableCheckData {
	t.Helper()
	result := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 0 || result.stderr != "" {
		t.Fatalf("check = %#v", result)
	}
	var envelope struct {
		Data durableCheckData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("decode check result: %v", err)
	}
	return envelope.Data
}

func durableCheckEnvironment(fake *durableCheckFake, database, sessionDir string) []string {
	return []string{
		"MEDALERT_DATABASE=" + database,
		"MEDALERT_SESSION_DIR=" + sessionDir,
		"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL,
		"MEDALERT_NON_INTERACTIVE=true",
	}
}

func createDurableCheckFixture(t *testing.T, fake *durableCheckFake, root string) (string, []string) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	passwordFile := filepath.Join(root, "password")
	if err := os.WriteFile(passwordFile, []byte("durable-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := durableCheckEnvironment(fake, database, filepath.Join(root, "sessions"))
	account := run(t, environment, "account", "create", "--account", "patient", "--username", "patient@example.com", "--password-file", passwordFile, "--non-interactive")
	if account.exitCode != 0 || account.stderr != "" {
		t.Fatalf("create account = %#v", account)
	}
	profile := run(t, environment, "profile", "create", "--profile", "morning", "--account", "patient", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--non-interactive")
	if profile.exitCode != 0 || profile.stderr != "" {
		t.Fatalf("create profile = %#v", profile)
	}
	return database, environment
}

func TestDurableCheckStartsEpisodeForNewSlot(t *testing.T) {
	fake, cleanup := newDurableCheckFake(t)
	defer cleanup()
	fake.setSlots(map[string]any{
		"bookingString":   "booking-1",
		"appointmentDate": "2099-09-10T10:00:00Z",
		"clinic":          map[string]any{"name": "Main clinic"},
		"doctor":          map[string]any{"name": "Dr Example"},
		"specialty":       map[string]any{"name": "Cardiology"},
		"visitType":       "Center",
	})
	root := t.TempDir()
	database, environment := createDurableCheckFixture(t, fake, root)

	result := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 0 || result.stderr != "" {
		t.Fatalf("check = %#v", result)
	}
	var envelope struct {
		Data struct {
			Dry            bool `json:"dry"`
			Complete       bool `json:"complete"`
			NewlyAvailable int  `json:"newly_available"`
			ActiveEpisodes int  `json:"active_episode_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
		t.Fatalf("decode check result: %v", err)
	}
	if envelope.Data.Dry || !envelope.Data.Complete || envelope.Data.NewlyAvailable != 1 || envelope.Data.ActiveEpisodes != 1 {
		t.Fatalf("check data = %#v", envelope.Data)
	}
	assertProcessQueryValue(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'complete'", "1")
	assertProcessQueryValue(t, database, "SELECT count(*) FROM availability_episodes WHERE active = 1", "1")
}

func TestDryCheckDoesNotSaveObservationState(t *testing.T) {
	fake, cleanup := newDurableCheckFake(t)
	defer cleanup()
	fake.setSlots(durableSlot("booking-1"))
	root := t.TempDir()
	database, environment := createDurableCheckFixture(t, fake, root)

	result := run(t, environment, "check", "--dry", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 0 || result.stderr != "" || !strings.Contains(result.stdout, `"dry":true`) {
		t.Fatalf("dry check = %#v", result)
	}
	assertProcessQueryValue(t, database, "SELECT count(*) FROM observation_runs", "0")
	assertProcessQueryValue(t, database, "SELECT count(*) FROM availability_episodes", "0")
}

func TestDurableCheckRejectsDisabledProfileBeforeAuthentication(t *testing.T) {
	fake, cleanup := newDurableCheckFake(t)
	defer cleanup()
	root := t.TempDir()
	_, environment := createDurableCheckFixture(t, fake, root)

	disabled := run(t, environment, "profile", "disable", "--profile", "morning")
	if disabled.exitCode != 0 || disabled.stderr != "" {
		t.Fatalf("disable profile = %#v", disabled)
	}

	result := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 2 || result.stdout != "" || !strings.Contains(result.stderr, `"code":"profile_disabled"`) {
		t.Fatalf("disabled check = %#v", result)
	}
	if count := fake.authRequestCount(); count != 0 {
		t.Fatalf("authentication requests = %d, want 0", count)
	}
}

func TestDurableCheckRejectsProfileRecreatedForAnotherAccount(t *testing.T) {
	fake, cleanup := newDurableCheckFake(t)
	defer cleanup()
	root := t.TempDir()
	database, environment := createDurableCheckFixture(t, fake, root)
	otherAccount := run(t, environment, "account", "create", "--account", "other", "--username", "other@example.com", "--no-stored-password", "--non-interactive")
	if otherAccount.exitCode != 0 || otherAccount.stderr != "" {
		t.Fatalf("create other account = %#v", otherAccount)
	}

	fake.setAfterToken(func() error {
		storage, err := store.Open(database)
		if err != nil {
			return err
		}
		defer storage.Close()
		if err := storage.DeleteProfile("morning"); err != nil {
			return err
		}
		_, err = storage.CreateProfile(store.Profile{
			ID:                   "morning",
			AccountID:            "other",
			RegionIDs:            "205",
			SpecialtyIDs:         "133",
			CheckIntervalMinutes: 30,
			Enabled:              true,
			SearchType:           store.SearchTypeStandard,
		})
		return err
	})

	result := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 6 || result.stdout != "" || !strings.Contains(result.stderr, `"code":"stale_result"`) {
		t.Fatalf("recreated-profile check = %#v", result)
	}
	if fake.searchRequestCount() != 0 {
		t.Fatalf("search requests = %d, want 0", fake.searchRequestCount())
	}
	fake.mu.Lock()
	callbackErr := fake.afterTokenErr
	fake.mu.Unlock()
	if callbackErr != nil {
		t.Fatalf("recreate profile: %v", callbackErr)
	}
}

func TestDurableCheckRejectsAccountChangedBeforeSearch(t *testing.T) {
	fake, cleanup := newDurableCheckFake(t)
	defer cleanup()
	root := t.TempDir()
	database, environment := createDurableCheckFixture(t, fake, root)

	fake.setAfterToken(func() error {
		storage, err := store.Open(database)
		if err != nil {
			return err
		}
		defer storage.Close()
		username := "changed@example.com"
		_, err = storage.UpdateAccount("patient", store.AccountUpdate{Username: &username})
		return err
	})

	result := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 6 || result.stdout != "" || !strings.Contains(result.stderr, `"code":"stale_result"`) {
		t.Fatalf("changed-account check = %#v", result)
	}
	if fake.searchRequestCount() != 0 {
		t.Fatalf("search requests = %d, want 0", fake.searchRequestCount())
	}
	fake.mu.Lock()
	callbackErr := fake.afterTokenErr
	fake.mu.Unlock()
	if callbackErr != nil {
		t.Fatalf("change account: %v", callbackErr)
	}
}

func TestDurableCheckReconcilesAcrossProcessesAndKeepsStateOnFailure(t *testing.T) {
	fake, cleanup := newDurableCheckFake(t)
	defer cleanup()
	root := t.TempDir()
	database, environment := createDurableCheckFixture(t, fake, root)

	fake.setSlots(durableSlot("booking-1"))
	first := runDurableCheck(t, environment)
	if first.Dry || !first.Complete || first.NewlyAvailable != 1 || first.Ended != 0 || first.ActiveEpisodes != 1 {
		t.Fatalf("first check = %#v", first)
	}

	// A new process with the same database must see the active episode and
	// must not report the slot again.
	repeated := runDurableCheck(t, environment)
	if repeated.NewlyAvailable != 0 || repeated.Ended != 0 || repeated.ActiveEpisodes != 1 {
		t.Fatalf("repeated check = %#v", repeated)
	}

	fake.setSlots()
	disappeared := runDurableCheck(t, environment)
	if disappeared.NewlyAvailable != 0 || disappeared.Ended != 1 || disappeared.ActiveEpisodes != 0 {
		t.Fatalf("disappearance check = %#v", disappeared)
	}

	fake.setSlots(durableSlot("booking-1"))
	returned := runDurableCheck(t, environment)
	if returned.NewlyAvailable != 1 || returned.Ended != 0 || returned.ActiveEpisodes != 1 {
		t.Fatalf("return check = %#v", returned)
	}

	// A failed search records the failed run, but leaves the returned episode
	// active. This also exercises the process restart boundary once more.
	fake.failSearch()
	failure := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if failure.exitCode != 4 || failure.stdout != "" || !strings.Contains(failure.stderr, `"code":"temporary_failure"`) {
		t.Fatalf("failed check = %#v", failure)
	}
	assertProcessQueryValue(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'complete'", "4")
	assertProcessQueryValue(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'failed'", "1")
	assertProcessQueryValue(t, database, "SELECT count(*) FROM availability_episodes", "2")
	assertProcessQueryValue(t, database, "SELECT count(*) FROM availability_episodes WHERE active = 1", "1")
}
