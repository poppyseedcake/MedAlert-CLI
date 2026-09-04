package main_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// deliveryTelegramFake is a local Telegram Bot API stand-in. It records every
// sendMessage call and replays a configured outcome. It never contacts
// production and never sees real bot tokens except test markers.
type deliveryTelegramFake struct {
	mu         sync.Mutex
	mode       string
	retryAfter int
	messageID  int64
	requests   int
	chats      []string
	texts      []string
}

func newDeliveryTelegramFake() *deliveryTelegramFake {
	return &deliveryTelegramFake{mode: "success", messageID: 100}
}

func (f *deliveryTelegramFake) setMode(mode string, retryAfter int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
	f.retryAfter = retryAfter
}

func (f *deliveryTelegramFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *deliveryTelegramFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		chat := ""
		text := ""
		if value, ok := payload["chat_id"].(string); ok {
			chat = value
		} else if payload["chat_id"] != nil {
			if encoded, err := json.Marshal(payload["chat_id"]); err == nil {
				chat = strings.Trim(string(encoded), `"`)
			}
		}
		if value, ok := payload["text"].(string); ok {
			text = value
		}
		f.mu.Lock()
		f.requests++
		f.chats = append(f.chats, chat)
		f.texts = append(f.texts, text)
		mode := f.mode
		retryAfter := f.retryAfter
		f.messageID++
		messageID := f.messageID
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "temporary":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"ok":false}`))
		case "rate_limited":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":` + itoa(retryAfter) + `}}`))
		case "permanent":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
		case "unknown":
			_, _ = w.Write([]byte(`not json`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + itoa(int(messageID)) + `}}`))
		}
	})
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func createDeliveryFixture(t *testing.T, medicoverBase, telegramBase, root string, profileInterval int) (string, []string, string) {
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
	passwordFile := filepath.Join(root, "password")
	if err := os.WriteFile(passwordFile, []byte("durable-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	account := run(t, environment, "account", "create", "--account", "patient", "--username", "patient@example.com", "--password-file", passwordFile, "--non-interactive")
	if account.exitCode != 0 || account.stderr != "" {
		t.Fatalf("create account = %#v", account)
	}
	tokenOne := writeProcessSecretFile(t, secretDir, "token-one", "TOKEN-ONE-MARKER-unique-111")
	tokenTwo := writeProcessSecretFile(t, secretDir, "token-two", "TOKEN-TWO-MARKER-unique-222")
	for _, destination := range []struct{ id, name, chat, token string }{
		{"phone", "Telefon", "123456", tokenOne},
		{"backup", "Zapas", "654321", tokenTwo},
	} {
		created := run(t, environment, "telegram", "create", "--telegram", destination.id, "--name", destination.name, "--chat-id", destination.chat, "--token-file", destination.token, "--non-interactive")
		if created.exitCode != 0 || created.stderr != "" {
			t.Fatalf("create destination %s = %#v", destination.id, created)
		}
	}
	interval := itoa(profileInterval)
	if interval == "0" {
		interval = "30"
	}
	profile := run(t, environment, "profile", "create", "--profile", "morning", "--account", "patient", "--region", "204", "--specialty", "132", "--check-interval-minutes", interval, "--telegram", "phone,backup", "--non-interactive")
	if profile.exitCode != 0 || profile.stderr != "" {
		t.Fatalf("create profile = %#v", profile)
	}
	return database, environment, secretDir
}

func runDeliveryCheck(t *testing.T, environment []string) processResult {
	t.Helper()
	return run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
}

func deliveryQuery(t *testing.T, database, query string) string {
	t.Helper()
	return watchQueryCount(t, database, query)
}

func TestDeliverySendsOncePerDestinationAndSurvivesRestart(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-1"))
	telegramFake := newDeliveryTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	first := runDeliveryCheck(t, environment)
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v", first)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("telegram requests after first check = %d, want 2 (one per destination)", got)
	}
	// Polish availability message must carry profile, time, doctor,
	// specialty, clinic, and visit type.
	telegramFake.mu.Lock()
	texts := append([]string(nil), telegramFake.texts...)
	chats := append([]string(nil), telegramFake.chats...)
	telegramFake.mu.Unlock()
	if len(chats) != 2 {
		t.Fatalf("chats = %#v, want two destinations", chats)
	}
	seen := map[string]bool{}
	for _, chat := range chats {
		seen[chat] = true
	}
	if !seen["123456"] || !seen["654321"] {
		t.Fatalf("chats = %#v, want both destination chats", chats)
	}
	for _, text := range texts {
		for _, want := range []string{"morning", "2099-09-10", "Dr Example", "Cardiology", "Main clinic", "Center", "Profil", "Czas", "Lekarz", "Specjalizacja", "Placówka", "Typ wizyty"} {
			if !strings.Contains(text, want) {
				t.Fatalf("telegram text missing %q: %q", want, text)
			}
		}
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "2" {
		t.Fatalf("delivered = %s, want 2", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'pending'"); got != "0" {
		t.Fatalf("pending = %s, want 0", got)
	}

	// Restart: a new process with the same database and the same slots must
	// not send again for the continuous episode.
	second := runDeliveryCheck(t, environment)
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("telegram requests after restart check = %d, want still 2 (no duplicate)", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries"); got != "2" {
		t.Fatalf("deliveries after restart = %s, want 2", got)
	}

	// New episode after disappearance is eligible for a new notification.
	medicoverFake.setSlots()
	disappeared := runDeliveryCheck(t, environment)
	if disappeared.exitCode != 0 {
		t.Fatalf("disappearance check = %#v", disappeared)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after disappearance = %d, want still 2 (no send on end)", got)
	}
	medicoverFake.setSlots(durableSlot("booking-1"))
	returned := runDeliveryCheck(t, environment)
	if returned.exitCode != 0 {
		t.Fatalf("return check = %#v", returned)
	}
	if got := telegramFake.requestCount(); got != 4 {
		t.Fatalf("requests after return = %d, want 4 (new episode notifies both again)", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "4" {
		t.Fatalf("delivered after return = %s, want 4", got)
	}

	// Secrets never reach SQLite, logs, or JSON.
	for _, output := range []string{first.stdout, first.stderr, second.stdout, second.stderr, disappeared.stdout, returned.stdout} {
		if strings.Contains(output, "TOKEN-ONE-MARKER-unique-111") || strings.Contains(output, "TOKEN-TWO-MARKER-unique-222") {
			t.Fatalf("output leaks token: %q", output)
		}
	}
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "TOKEN-ONE-MARKER-unique-111") || strings.Contains(string(raw), "TOKEN-TWO-MARKER-unique-222") {
		t.Fatal("database file contains bot token")
	}
}

func TestDeliveryRetriesTemporaryThenSends(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-9"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("temporary", 0)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	first := runDeliveryCheck(t, environment)
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v", first)
	}
	// Two destinations, each attempted once and left retryable.
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after temporary failure = %d, want 2", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'retry'"); got != "2" {
		t.Fatalf("retry = %s, want 2", got)
	}
	if got := deliveryQuery(t, database, "SELECT attempts FROM telegram_deliveries LIMIT 1"); got != "1" {
		t.Fatalf("attempts after first = %s, want 1", got)
	}

	// A later monitoring cycle completes a new observation run and retries
	// while the slot remains available.
	telegramFake.setMode("success", 0)
	second := runDeliveryCheck(t, environment)
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	if got := telegramFake.requestCount(); got != 4 {
		t.Fatalf("requests after retry = %d, want 4", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "2" {
		t.Fatalf("delivered after retry = %s, want 2", got)
	}
	if got := deliveryQuery(t, database, "SELECT attempts FROM telegram_deliveries LIMIT 1"); got != "2" {
		t.Fatalf("attempts after retry = %s, want 2", got)
	}
}

func TestDeliveryRespectsRetryAfter(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-7"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("rate_limited", 2)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	first := runDeliveryCheck(t, environment)
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v", first)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after rate limit = %d, want 2", got)
	}
	// Immediate retry is gated by retry_after: no new Telegram calls.
	second := runDeliveryCheck(t, environment)
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests before retry_after = %d, want still 2", got)
	}
	time.Sleep(3 * time.Second)
	telegramFake.setMode("success", 0)
	third := runDeliveryCheck(t, environment)
	if third.exitCode != 0 {
		t.Fatalf("third check = %#v", third)
	}
	if got := telegramFake.requestCount(); got != 4 {
		t.Fatalf("requests after retry_after = %d, want 4", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "2" {
		t.Fatalf("delivered = %s, want 2", got)
	}
}

func TestDeliveryStopsWhenSlotDisappears(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-stale"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("temporary", 0)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	first := runDeliveryCheck(t, environment)
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v", first)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests = %d, want 2 pending", got)
	}
	// Slot disappears before the retry: the pending delivery stops without
	// sending stale availability.
	medicoverFake.setSlots()
	telegramFake.setMode("success", 0)
	second := runDeliveryCheck(t, environment)
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after disappearance = %d, want still 2 (stale stops)", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'permanent_failure'"); got != "2" {
		t.Fatalf("stale permanent = %s, want 2", got)
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reason string
	if err := db.QueryRow("SELECT last_error FROM telegram_deliveries LIMIT 1").Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "no longer available") {
		t.Fatalf("stale reason = %q, want slot no longer available", reason)
	}
}

func TestDeliveryGivesUpAfterFiveAttempts(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-max"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("temporary", 0)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	for attempt := 1; attempt <= 5; attempt++ {
		result := runDeliveryCheck(t, environment)
		if result.exitCode != 0 {
			t.Fatalf("check %d = %#v", attempt, result)
		}
	}
	if got := telegramFake.requestCount(); got != 10 {
		t.Fatalf("requests after 5 cycles = %d, want 10 (2 destinations x 5 attempts)", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'permanent_failure'"); got != "2" {
		t.Fatalf("permanent after budget = %s, want 2", got)
	}
	sixth := runDeliveryCheck(t, environment)
	if sixth.exitCode != 0 {
		t.Fatalf("sixth check = %#v", sixth)
	}
	if got := telegramFake.requestCount(); got != 10 {
		t.Fatalf("requests after budget exhausted = %d, want still 10 (no more than five attempts)", got)
	}
}

func TestDeliveryDuplicateControlAcrossCheckAndWatch(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-x"))
	telegramFake := newDeliveryTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	// One-minute interval so we can force the watch due with a small clock
	// shift for the check->watch direction.
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)

	// Watch creates the first notification.
	watched := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if watched.exitCode != 0 {
		t.Fatalf("watch --once = %#v", watched)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after watch = %d, want 2", got)
	}
	// A manual check with the same slots must not duplicate.
	checked := runDeliveryCheck(t, environment)
	if checked.exitCode != 0 {
		t.Fatalf("check after watch = %#v", checked)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after check following watch = %d, want still 2", got)
	}

	// Force the profile due again with a durable clock shift, then watch
	// must run a fresh observation yet still not duplicate the continuous
	// episode.
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	shifted := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := db.Exec("UPDATE observation_runs SET started_at = ?", shifted); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	rewatched := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if rewatched.exitCode != 0 {
		t.Fatalf("second watch = %#v", rewatched)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after second watch = %d, want still 2 (continuous episode)", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "2" {
		t.Fatalf("delivered = %s, want 2", got)
	}
}

func TestDeliveryPermanentFailureIsVisible(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-bad"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("permanent", 0)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	// Single destination keeps the permanent assertion focused.
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
		"MEDALERT_MEDICOVER_BASE_URL=" + medicoverFake.baseURL,
		"MEDALERT_TELEGRAM_BASE_URL=" + telegramServer.URL,
		"MEDALERT_NON_INTERACTIVE=true",
	}
	passwordFile := filepath.Join(root, "password")
	if err := os.WriteFile(passwordFile, []byte("durable-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created := run(t, environment, "account", "create", "--account", "patient", "--username", "patient@example.com", "--password-file", passwordFile, "--non-interactive"); created.exitCode != 0 {
		t.Fatalf("create account = %#v", created)
	}
	tokenFile := writeProcessSecretFile(t, secretDir, "token", "TOKEN-PERM-MARKER-unique")
	if created := run(t, environment, "telegram", "create", "--telegram", "phone", "--name", "Telefon", "--chat-id", "123456", "--token-file", tokenFile, "--non-interactive"); created.exitCode != 0 {
		t.Fatalf("create destination = %#v", created)
	}
	if created := run(t, environment, "profile", "create", "--profile", "morning", "--account", "patient", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "phone", "--non-interactive"); created.exitCode != 0 {
		t.Fatalf("create profile = %#v", created)
	}

	result := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if result.exitCode != 0 {
		t.Fatalf("check = %#v", result)
	}
	if got := deliveryQuery(t, database, "SELECT status FROM telegram_deliveries LIMIT 1"); got != "permanent_failure" {
		t.Fatalf("status = %s, want permanent_failure", got)
	}
	// Permanent failures never retry on the next cycle.
	second := run(t, environment, "check", "--profile", "morning", "--output", "json", "--non-interactive")
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("requests = %d, want 1 (permanent never retries)", got)
	}
}

func TestDeliveryRetriesUnknownThenSends(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-unknown"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("unknown", 0)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	first := runDeliveryCheck(t, environment)
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v", first)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after unknown = %d, want 2", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'retry'"); got != "2" {
		t.Fatalf("retry after unknown = %s, want 2 (unknown stays retryable)", got)
	}
	telegramFake.setMode("success", 0)
	second := runDeliveryCheck(t, environment)
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "2" {
		t.Fatalf("delivered after unknown retry = %s, want 2", got)
	}
}

func TestDeliveryRetryAcrossCheckAndWatch(t *testing.T) {
	medicoverFake, medicoverCleanup := newDurableCheckFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(durableSlot("booking-cross"))
	telegramFake := newDeliveryTelegramFake()
	telegramFake.setMode("temporary", 0)
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, _ := createDeliveryFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 30)

	// Manual check leaves retryable work behind.
	first := runDeliveryCheck(t, environment)
	if first.exitCode != 0 {
		t.Fatalf("check = %#v", first)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("requests after check = %d, want 2 pending", got)
	}
	// A later watch cycle completes a new observation run and retries while
	// the slot remains available, without any clock manipulation: pending
	// plus a complete latest run makes the profile retry-due.
	telegramFake.setMode("success", 0)
	watched := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if watched.exitCode != 0 {
		t.Fatalf("watch --once = %#v", watched)
	}
	if got := telegramFake.requestCount(); got != 4 {
		t.Fatalf("requests after watch retry = %d, want 4 (check->watch retry)", got)
	}
	if got := deliveryQuery(t, database, "SELECT count(*) FROM telegram_deliveries WHERE status = 'delivered'"); got != "2" {
		t.Fatalf("delivered after cross retry = %s, want 2", got)
	}
}
