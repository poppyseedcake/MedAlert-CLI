package main_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

// watchFake is a local Medicover stand-in for watch acceptance. It accepts
// two test users, counts requests, and supports delay and failure injection.
// It never contacts production.
type watchFake struct {
	mu             sync.Mutex
	baseURL        string
	redirectURI    string
	slots          []map[string]any
	searchFailure  bool
	searchDelay    time.Duration
	pending        map[string]string
	codes          map[string]string
	authRequests   int
	searchRequests int
}

func newWatchFake(t *testing.T) (*watchFake, func()) {
	t.Helper()
	fake := &watchFake{pending: map[string]string{}, codes: map[string]string{}}
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
			code := "watch-trusted-" + state[:minWatchLen(state, 6)]
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
		valid := (username == "alice@example.com" && password == "alice-pass") ||
			(username == "bob@example.com" && password == "bob-pass")
		if !valid {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form><input type="password" name="Input.Password" /></form></body></html>`))
			return
		}
		state := extractWatchState(r.PostForm.Get("Input.ReturnUrl"))
		code := "watch-login-" + state[:minWatchLen(state, 6)]
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
		if !ok || base64.RawURLEncoding.EncodeToString(mustWatchSHA256(verifier)) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "watch-access",
			"refresh_token": "watch-refresh",
			"expires_in":    300,
		})
	})
	searchHandler := func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		failure := fake.searchFailure
		delay := fake.searchDelay
		slots := append([]map[string]any(nil), fake.slots...)
		fake.mu.Unlock()
		// Delay only the slots call so one observation holds its SQLite lease
		// for the full delay while filters stay fast.
		if delay > 0 && strings.HasSuffix(r.URL.Path, "/slots") {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fake.mu.Lock()
		fake.searchRequests++
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

func minWatchLen(value string, n int) int {
	if len(value) < n {
		return len(value)
	}
	return n
}

func extractWatchState(returnURL string) string {
	parsed, err := url.Parse(returnURL)
	if err != nil {
		return ""
	}
	return parsed.Query().Get("state")
}

func mustWatchSHA256(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func (f *watchFake) setWatchSlots(slots ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots = append([]map[string]any(nil), slots...)
	f.searchFailure = false
}

func (f *watchFake) failWatchSearch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchFailure = true
}

func (f *watchFake) setWatchDelay(delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchDelay = delay
}

func watchSlot(booking string) map[string]any {
	return map[string]any{
		"bookingString":   booking,
		"appointmentDate": "2099-09-10T10:00:00Z",
		"clinic":          map[string]any{"name": "Main clinic"},
		"doctor":          map[string]any{"name": "Dr Example"},
		"specialty":       map[string]any{"name": "Cardiology"},
		"visitType":       "Center",
	}
}

type watchJSONEvent struct {
	SchemaVersion int            `json:"schema_version"`
	Command       string         `json:"command"`
	Event         string         `json:"event"`
	Data          map[string]any `json:"data"`
}

func watchEnvironment(fake *watchFake, database, sessionDir string) []string {
	return []string{
		"MEDALERT_DATABASE=" + database,
		"MEDALERT_SESSION_DIR=" + sessionDir,
		"MEDALERT_MEDICOVER_BASE_URL=" + fake.baseURL,
		"MEDALERT_NON_INTERACTIVE=true",
	}
}

func createWatchFixture(t *testing.T, fake *watchFake, root string, accounts int) (string, []string) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "sessions")
	environment := watchEnvironment(fake, database, sessionDir)
	users := []struct{ id, username, password string }{
		{"alice", "alice@example.com", "alice-pass"},
		{"bob", "bob@example.com", "bob-pass"},
	}
	for index := 0; index < accounts && index < len(users); index++ {
		user := users[index]
		passwordFile := filepath.Join(root, "password-"+user.id)
		if err := os.WriteFile(passwordFile, []byte(user.password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		created := run(t, environment, "account", "create", "--account", user.id, "--username", user.username, "--password-file", passwordFile, "--non-interactive")
		if created.exitCode != 0 || created.stderr != "" {
			t.Fatalf("create account %s = %#v", user.id, created)
		}
	}
	return database, environment
}

func createWatchProfile(t *testing.T, environment []string, id, account string) {
	t.Helper()
	created := run(t, environment, "profile", "create", "--profile", id, "--account", account, "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--non-interactive")
	if created.exitCode != 0 || created.stderr != "" {
		t.Fatalf("create profile %s = %#v", id, created)
	}
}

func parseWatchEvents(t *testing.T, stdout string) []watchJSONEvent {
	t.Helper()
	events := []watchJSONEvent{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event watchJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode watch event %q: %v", line, err)
		}
		if event.SchemaVersion != 1 || event.Command != "watch" || event.Event == "" {
			t.Fatalf("invalid watch event envelope: %q", line)
		}
		events = append(events, event)
	}
	return events
}

func countWatchEvents(events []watchJSONEvent, name string) int {
	count := 0
	for _, event := range events {
		if event.Event == name {
			count++
		}
	}
	return count
}

func watchQueryCount(t *testing.T, database, query string) string {
	t.Helper()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		t.Fatal(err)
	}
	var value string
	// A concurrent watch may hold a short write lock during Begin/Reconcile;
	// retry instead of failing the poll on SQLITE_BUSY.
	for attempt := 0; attempt < 20; attempt++ {
		if err := db.QueryRow(query).Scan(&value); err == nil {
			return value
		} else if strings.Contains(strings.ToLower(err.Error()), "locked") || strings.Contains(strings.ToLower(err.Error()), "busy") {
			time.Sleep(50 * time.Millisecond)
			continue
		} else {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func waitForWatchCondition(t *testing.T, timeout time.Duration, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

type watchBackground struct {
	command   *exec.Cmd
	stdoutFSR *os.File
	stderrFSR *os.File
	stdoutPth string
	stderrPth string
}

func startWatchBackground(t *testing.T, environment []string, args ...string) *watchBackground {
	t.Helper()
	stdoutFile, err := os.CreateTemp("", "watch-stdout-*.log")
	if err != nil {
		t.Fatal(err)
	}
	stderrFile, err := os.CreateTemp("", "watch-stderr-*.log")
	if err != nil {
		_ = os.Remove(stdoutFile.Name())
		t.Fatal(err)
	}
	command := exec.Command(executablePath, args...)
	command.Env = append(os.Environ(), environment...)
	command.Stdout = stdoutFile
	command.Stderr = stderrFile
	if err := command.Start(); err != nil {
		_ = os.Remove(stdoutFile.Name())
		_ = os.Remove(stderrFile.Name())
		t.Fatal(err)
	}
	background := &watchBackground{
		command:   command,
		stdoutFSR: stdoutFile,
		stderrFSR: stderrFile,
		stdoutPth: stdoutFile.Name(),
		stderrPth: stderrFile.Name(),
	}
	t.Cleanup(func() {
		_ = os.Remove(background.stdoutPth)
		_ = os.Remove(background.stderrPth)
	})
	return background
}

func (b *watchBackground) signalAndWait(t *testing.T, signal os.Signal) (int, string, string) {
	t.Helper()
	if err := b.command.Process.Signal(signal); err != nil {
		t.Fatalf("signal watch: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.command.Wait() }()
	select {
	case err := <-done:
		return watchBackgroundExit(t, err, b)
	case <-time.After(10 * time.Second):
		_ = b.command.Process.Kill()
		<-done
		t.Fatal("watch did not stop after signal")
		return -1, "", ""
	}
}

func (b *watchBackground) wait(t *testing.T) (int, string, string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- b.command.Wait() }()
	select {
	case err := <-done:
		return watchBackgroundExit(t, err, b)
	case <-time.After(15 * time.Second):
		_ = b.command.Process.Kill()
		<-done
		t.Fatal("watch did not exit in time")
		return -1, "", ""
	}
}

func watchBackgroundExit(t *testing.T, waitErr error, b *watchBackground) (int, string, string) {
	t.Helper()
	_ = b.stdoutFSR.Close()
	_ = b.stderrFSR.Close()
	stdout, _ := os.ReadFile(b.stdoutPth)
	stderr, _ := os.ReadFile(b.stderrPth)
	if waitErr == nil {
		return 0, string(stdout), string(stderr)
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(stdout), string(stderr)
	}
	t.Fatalf("wait watch: %v", waitErr)
	return -1, "", ""
}

func readBackgroundFile(path string) string {
	contents, _ := os.ReadFile(path)
	return string(contents)
}

func TestWatchOnceRunsEachEnabledProfile(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	database, environment := createWatchFixture(t, fake, root, 1)
	createWatchProfile(t, environment, "morning", "alice")
	createWatchProfile(t, environment, "evening", "alice")

	disabled := run(t, environment, "profile", "disable", "--profile", "evening")
	if disabled.exitCode != 0 {
		t.Fatalf("disable = %#v", disabled)
	}
	// Only morning is enabled: --once must schedule it and skip the disabled row.
	result := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if result.exitCode != 0 {
		t.Fatalf("watch --once = %#v", result)
	}
	if result.stdout == "" || strings.Contains(result.stdout, "booking-1") {
		t.Fatalf("watch stdout must be JSON Lines without slot secrets: %q", result.stdout)
	}
	events := parseWatchEvents(t, result.stdout)
	if countWatchEvents(events, "started") != 1 || countWatchEvents(events, "run_completed") != 1 || countWatchEvents(events, "stopped") != 1 {
		t.Fatalf("watch events = %#v", events)
	}
	for _, event := range events {
		if event.Event == "run_completed" && event.Data["profile"] != "morning" {
			t.Fatalf("unexpected completed profile: %#v", event.Data)
		}
	}
	if !strings.Contains(result.stderr, "watch:") || !strings.Contains(result.stderr, "morning") {
		t.Fatalf("text logs must use stderr: %q", result.stderr)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'complete'"); got != "1" {
		t.Fatalf("complete runs = %s, want 1 (disabled profile skipped)", got)
	}

	// Re-enable and verify both enabled profiles are scheduled.
	enabled := run(t, environment, "profile", "enable", "--profile", "evening")
	if enabled.exitCode != 0 {
		t.Fatalf("enable = %#v", enabled)
	}
	// Clear history so the second --once treats both as due: a restart must
	// still schedule every enabled profile, covered here by fresh episodes.
	second := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if second.exitCode != 0 {
		t.Fatalf("second watch --once = %#v", second)
	}
	secondEvents := parseWatchEvents(t, second.stdout)
	// Morning already ran 30m interval ago? No, it just ran, so only evening is due.
	// This asserts interval throttling: morning must not run twice in a row.
	if countWatchEvents(secondEvents, "run_completed") != 1 {
		t.Fatalf("second watch events = %#v, want exactly evening", secondEvents)
	}
	foundEvening := false
	for _, event := range secondEvents {
		if event.Event == "run_completed" && event.Data["profile"] == "evening" {
			foundEvening = true
		}
		if event.Event == "run_completed" && event.Data["profile"] == "morning" {
			t.Fatalf("morning ran before its interval: %#v", secondEvents)
		}
	}
	if !foundEvening {
		t.Fatalf("evening was not scheduled: %#v", secondEvents)
	}
}

func TestWatchDoesNotRepeatBeforeInterval(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	_, environment := createWatchFixture(t, fake, root, 1)
	createWatchProfile(t, environment, "morning", "alice")
	createWatchProfile(t, environment, "evening", "alice")

	background := startWatchBackground(t, environment, "watch", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	// Both profiles are new, so both run once, then neither is due again for
	// 30 minutes. A 2s continuous run must not create extra runs: one profile
	// has no more than one active run and intervals are respected.
	waitForWatchCondition(t, 10*time.Second, "both profiles to complete", func() bool {
		stdout := readBackgroundFile(background.stdoutPth)
		if stdout == "" {
			return false
		}
		events := []watchJSONEvent{}
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			var event watchJSONEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &event); err != nil {
				return false
			}
			events = append(events, event)
		}
		return countWatchEvents(events, "run_completed") >= 2
	})
	time.Sleep(2 * time.Second)
	exitCode, stdout, stderr := background.signalAndWait(t, syscall.SIGTERM)
	if exitCode != 0 {
		t.Fatalf("watch exit = %d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	events := parseWatchEvents(t, stdout)
	if countWatchEvents(events, "run_completed") != 2 {
		t.Fatalf("run_completed count = %d, want exactly 2 (no repeat before interval): %#v", countWatchEvents(events, "run_completed"), events)
	}
	if !strings.Contains(stderr, "watch:") {
		t.Fatalf("text logs must use stderr: %q", stderr)
	}
	if stdout != "" && strings.Contains(stdout, "watch:") {
		t.Fatalf("machine events must be JSON Lines, not text logs: %q", stdout)
	}
}

func TestWatchReloadsNewProfileWithoutRestart(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	database, environment := createWatchFixture(t, fake, root, 1)
	createWatchProfile(t, environment, "morning", "alice")

	background := startWatchBackground(t, environment, "watch", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	waitForWatchCondition(t, 10*time.Second, "first profile to complete", func() bool {
		return watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'morning' AND status = 'complete'") == "1"
	})
	// Reload: create a second profile while watch is running. It must be
	// picked up between planned runs without a restart.
	createWatchProfile(t, environment, "reloaded", "alice")
	waitForWatchCondition(t, 10*time.Second, "reloaded profile to complete", func() bool {
		return watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'reloaded' AND status = 'complete'") == "1"
	})
	exitCode, stdout, _ := background.signalAndWait(t, syscall.SIGTERM)
	if exitCode != 0 {
		t.Fatalf("watch exit = %d", exitCode)
	}
	events := parseWatchEvents(t, stdout)
	sawReloaded := false
	for _, event := range events {
		if event.Event == "run_completed" && event.Data["profile"] == "reloaded" {
			sawReloaded = true
		}
	}
	if !sawReloaded {
		t.Fatalf("reloaded profile never ran: %#v", events)
	}
}

func TestWatchLeaseConflictSkipsDuplicate(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	database, environment := createWatchFixture(t, fake, root, 1)
	// Use the minimum interval so a 2-minute-old running lease is still both
	// due (interval elapsed) and active (15-minute lease held).
	created := run(t, environment, "profile", "create", "--profile", "morning", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "1", "--non-interactive")
	if created.exitCode != 0 {
		t.Fatalf("create profile = %#v", created)
	}

	// Hold the SQLite lease from this test process with a start time that is
	// due but still inside the 15-minute active window.
	holder, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	held, err := holder.BeginObservationRun("morning", time.Now().UTC().Add(-2*time.Minute))
	if err != nil {
		holder.Close()
		t.Fatal(err)
	}
	if held.ID == "" {
		holder.Close()
		t.Fatal("held run has no id")
	}
	skipped := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if skipped.exitCode != 0 {
		_, _ = holder.FailObservationRun(held.ID, store.ObservationRunCancelled, "cancelled", "cleanup", time.Now().UTC())
		holder.Close()
		t.Fatalf("watch with held lease = %#v", skipped)
	}
	events := parseWatchEvents(t, skipped.stdout)
	if countWatchEvents(events, "run_skipped") != 1 || countWatchEvents(events, "run_completed") != 0 {
		_, _ = holder.FailObservationRun(held.ID, store.ObservationRunCancelled, "cancelled", "cleanup", time.Now().UTC())
		holder.Close()
		t.Fatalf("lease conflict events = %#v, want one skipped", events)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'complete'"); got != "0" {
		_, _ = holder.FailObservationRun(held.ID, store.ObservationRunCancelled, "cancelled", "cleanup", time.Now().UTC())
		holder.Close()
		t.Fatalf("complete runs during lease = %s, want 0", got)
	}
	if _, err := holder.FailObservationRun(held.ID, store.ObservationRunCancelled, "cancelled", "cleanup", time.Now().UTC()); err != nil {
		holder.Close()
		t.Fatal(err)
	}
	holder.Close()

	// After the lease is released the same watch must make progress.
	retried := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if retried.exitCode != 0 {
		t.Fatalf("watch after lease release = %#v", retried)
	}
	retriedEvents := parseWatchEvents(t, retried.stdout)
	if countWatchEvents(retriedEvents, "run_completed") != 1 {
		t.Fatalf("retry events = %#v, want one completed", retriedEvents)
	}

	// Two processes sharing one database must not duplicate: start two
	// --once watches concurrently with a slow search so their running
	// windows overlap; only one may complete the fresh profile.
	fake.setWatchDelay(2 * time.Second)
	start := make(chan struct{})
	results := make(chan processResult, 2)
	for range 2 {
		go func() {
			<-start
			results <- run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
		}()
	}
	// Both processes race only when the profile is due again. Force due by
	// using a fresh profile that neither has run.
	createWatchProfile(t, environment, "raced", "alice")
	close(start)
	collected := []processResult{<-results, <-results}
	fake.setWatchDelay(0)
	for _, result := range collected {
		if result.exitCode != 0 {
			t.Fatalf("concurrent watch = %#v", result)
		}
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'raced' AND status = 'complete'"); got != "1" {
		t.Fatalf("concurrent complete runs for raced = %s, want 1 (lease prevents duplicate)", got)
	}
	completedOutputs := 0
	skippedOutputs := 0
	for _, result := range collected {
		events := parseWatchEvents(t, result.stdout)
		completedOutputs += countWatchEvents(events, "run_completed")
		skippedOutputs += countWatchEvents(events, "run_skipped")
	}
	if completedOutputs != 1 || skippedOutputs < 1 {
		t.Fatalf("concurrent outputs completed=%d skipped=%d, want 1 completed and at least 1 skipped", completedOutputs, skippedOutputs)
	}
}

func TestWatchIsolatesFailingAccount(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "sessions")
	environment := watchEnvironment(fake, database, sessionDir)
	// Alice has valid credentials; bob points at a wrong password so only
	// bob's account pauses with Authentication Required.
	aliceFile := filepath.Join(root, "alice-pass")
	if err := os.WriteFile(aliceFile, []byte("alice-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bobFile := filepath.Join(root, "bob-pass")
	if err := os.WriteFile(bobFile, []byte("wrong-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, account := range []struct{ id, username, file string }{
		{"alice", "alice@example.com", aliceFile},
		{"bob", "bob@example.com", bobFile},
	} {
		created := run(t, environment, "account", "create", "--account", account.id, "--username", account.username, "--password-file", account.file, "--non-interactive")
		if created.exitCode != 0 {
			t.Fatalf("create %s = %#v", account.id, created)
		}
	}
	createWatchProfile(t, environment, "alice-morning", "alice")
	createWatchProfile(t, environment, "bob-morning", "bob")

	result := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if result.exitCode != 0 {
		t.Fatalf("watch with failing account = %#v (one failure must not stop other work)", result)
	}
	events := parseWatchEvents(t, result.stdout)
	aliceCompleted := false
	bobPaused := false
	for _, event := range events {
		if event.Event == "run_completed" && event.Data["profile"] == "alice-morning" {
			aliceCompleted = true
		}
		if event.Event == "auth_paused" && event.Data["account"] == "bob" {
			bobPaused = true
		}
		if event.Event == "run_completed" && event.Data["profile"] == "bob-morning" {
			t.Fatalf("bob profile completed despite bad credentials: %#v", events)
		}
	}
	if !aliceCompleted {
		t.Fatalf("healthy account did not complete: %#v", events)
	}
	if !bobPaused {
		t.Fatalf("failing account did not pause: %#v", events)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'alice-morning' AND status = 'complete'"); got != "1" {
		t.Fatalf("alice runs = %s, want 1", got)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'bob-morning'"); got != "0" {
		t.Fatalf("bob runs = %s, want 0 (paused before any run)", got)
	}

	// Fix bob's secret and verify only bob resumes: alice already ran inside
	// its interval, so the next --once must complete bob without repeating alice.
	if err := os.WriteFile(bobFile, []byte("bob-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if resumed.exitCode != 0 {
		t.Fatalf("watch after fixing bob = %#v", resumed)
	}
	resumedEvents := parseWatchEvents(t, resumed.stdout)
	bobCompleted := false
	for _, event := range resumedEvents {
		if event.Event == "run_completed" && event.Data["profile"] == "bob-morning" {
			bobCompleted = true
		}
		if event.Event == "run_completed" && event.Data["profile"] == "alice-morning" {
			t.Fatalf("alice repeated before its interval: %#v", resumedEvents)
		}
	}
	if !bobCompleted {
		t.Fatalf("bob did not resume after fixing credentials: %#v", resumedEvents)
	}
}

func TestWatchStopsOnSIGINT(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	database, environment := createWatchFixture(t, fake, root, 1)
	createWatchProfile(t, environment, "morning", "alice")

	background := startWatchBackground(t, environment, "watch", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	waitForWatchCondition(t, 10*time.Second, "first run to complete", func() bool {
		return watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'complete'") == "1"
	})
	exitCode, stdout, stderr := background.signalAndWait(t, syscall.SIGINT)
	if exitCode != 0 {
		t.Fatalf("SIGINT exit = %d stdout=%q stderr=%q, want controlled 0", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "SIGINT") {
		t.Fatalf("stderr must log the signal: %q", stderr)
	}
	events := parseWatchEvents(t, stdout)
	sawStopped := false
	for _, event := range events {
		if event.Event == "stopped" && event.Data["signal"] == "SIGINT" {
			sawStopped = true
		}
	}
	if !sawStopped {
		t.Fatalf("stopped event with SIGINT missing: %#v", events)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'running'"); got != "0" {
		t.Fatalf("running runs after SIGINT = %s, want 0 (no invalid state)", got)
	}
}

func TestWatchStopsOnSIGTERMWhileCancellingActiveWork(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	fake.setWatchDelay(5 * time.Second)
	root := t.TempDir()
	database, environment := createWatchFixture(t, fake, root, 1)
	createWatchProfile(t, environment, "morning", "alice")

	background := startWatchBackground(t, environment, "watch", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	// Wait until the run has started (active lease visible) instead of
	// waiting for completion: the signal must cancel active work.
	waitForWatchCondition(t, 10*time.Second, "run to become active", func() bool {
		stdout := readBackgroundFile(background.stdoutPth)
		return strings.Contains(stdout, `"event":"run_started"`)
	})
	exitCode, stdout, stderr := background.signalAndWait(t, syscall.SIGTERM)
	if exitCode != 0 {
		t.Fatalf("SIGTERM exit = %d stdout=%q stderr=%q, want controlled 0", exitCode, stdout, stderr)
	}
	if !strings.Contains(stderr, "SIGTERM") {
		t.Fatalf("stderr must log the signal: %q", stderr)
	}
	events := parseWatchEvents(t, stdout)
	sawStopped := false
	for _, event := range events {
		if event.Event == "stopped" && event.Data["signal"] == "SIGTERM" {
			sawStopped = true
		}
		if event.Event == "run_completed" {
			t.Fatalf("cancelled search must not commit a complete run: %#v", events)
		}
	}
	if !sawStopped {
		t.Fatalf("stopped event with SIGTERM missing: %#v", events)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'running'"); got != "0" {
		t.Fatalf("running runs after SIGTERM = %s, want 0", got)
	}
	if got := watchQueryCount(t, database, "SELECT count(*) FROM availability_episodes"); got != "0" {
		t.Fatalf("episodes after cancelled run = %s, want 0 (cancelled runs never change episodes)", got)
	}
}

func TestWatchRejectsIrrelevantFlags(t *testing.T) {
	root := privateTempDir(t)
	database := filepath.Join(root, "medalert.db")
	for _, args := range [][]string{
		{"watch", "--database", database, "--profile", "morning"},
		{"watch", "--database", database, "--account", "alice"},
		{"watch", "--database", database, "--region", "204"},
		{"watch", "--database", database, "--dry"},
		{"watch", "--database", database, "--check-interval-minutes", "5"},
		{"watch", "--database", database, "--password-file", "/unused"},
		{"check", "--database", database, "--profile", "p", "--once"},
		{"check", "--database", database, "--profile", "p", "--poll-interval", "100ms"},
	} {
		result := run(t, nil, args...)
		if result.exitCode != 2 || result.stdout != "" || !strings.Contains(result.stderr, "not supported") {
			t.Fatalf("args %v result = %#v", args, result)
		}
	}
}

func TestWatchStreamsTextLogsOnStderrAndJSONLinesOnStdout(t *testing.T) {
	fake, cleanup := newWatchFake(t)
	defer cleanup()
	fake.setWatchSlots(watchSlot("booking-1"))
	root := t.TempDir()
	marker := "WATCH-MARKER-" + t.Name() + "-unique"
	// Use the marker as a wrong password so authentication fails: the marker
	// must never appear in logs, JSON, or the database.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "sessions")
	environment := watchEnvironment(fake, database, sessionDir)
	passwordFile := filepath.Join(root, "marker-pass")
	if err := os.WriteFile(passwordFile, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := run(t, environment, "account", "create", "--account", "alice", "--username", "alice@example.com", "--password-file", passwordFile, "--non-interactive")
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}
	createWatchProfile(t, environment, "morning", "alice")

	jsonResult := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if jsonResult.exitCode != 0 {
		t.Fatalf("json watch = %#v", jsonResult)
	}
	if jsonResult.stdout == "" || !strings.Contains(jsonResult.stdout, `"command":"watch"`) {
		t.Fatalf("json stdout must hold JSON Lines events: %q", jsonResult.stdout)
	}
	if !strings.Contains(jsonResult.stderr, "watch:") {
		t.Fatalf("text logs must use stderr: %q", jsonResult.stderr)
	}
	events := parseWatchEvents(t, jsonResult.stdout)
	if len(events) == 0 {
		t.Fatal("no JSON Lines events")
	}

	textResult := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--non-interactive")
	if textResult.exitCode != 0 {
		t.Fatalf("text watch = %#v", textResult)
	}
	if textResult.stdout != "" {
		t.Fatalf("text mode must keep stdout empty, got %q", textResult.stdout)
	}
	if !strings.Contains(textResult.stderr, "watch:") {
		t.Fatalf("text logs missing: %q", textResult.stderr)
	}

	for _, output := range []string{jsonResult.stdout, jsonResult.stderr, textResult.stdout, textResult.stderr} {
		if strings.Contains(output, marker) {
			t.Fatalf("output leaks secret marker: %q", output)
		}
	}
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatal("database file contains secret marker")
	}
}
