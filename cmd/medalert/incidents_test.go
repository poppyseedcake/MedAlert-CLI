package main_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func incidentQuery(t *testing.T, database, query string) string {
	t.Helper()
	return watchQueryCount(t, database, query)
}

func TestIncidentAuthPauseStartsImmediatelyAndRecovers(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-auth"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 2)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "alice-morning", "alice", "204", "phone,backup")
	createIncidentProfile(t, environment, "bob-morning", "bob", "204", "phone,backup")

	// Break bob's password: authentication must pause only bob.
	bobPasswordFile := filepath.Join(root, "password-bob")
	wrongMarker := "WRONG-PASS-MARKER-" + t.Name() + "-unique"
	if err := os.WriteFile(bobPasswordFile, []byte(wrongMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bobFailed := run(t, environment, "check", "--profile", "bob-morning", "--output", "json", "--non-interactive")
	if bobFailed.exitCode != 3 || !strings.Contains(bobFailed.stderr, "authentication_required") && !strings.Contains(bobFailed.stderr, "invalid_credentials") {
		t.Fatalf("bob check with bad password = %#v, want auth failure exit 3", bobFailed)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'bob' AND status = 'active'"); got != "1" {
		t.Fatalf("bob account incidents = %s, want 1 immediate", got)
	}
	if got := incidentQuery(t, database, "SELECT consecutive_failures FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'bob'"); got != "1" {
		t.Fatalf("bob consecutive = %s, want 1", got)
	}
	// One failure notification per eligible destination (phone, backup).
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'failure' AND status = 'delivered'"); got != "2" {
		t.Fatalf("failure deliveries = %s, want 2 (one per destination)", got)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("telegram requests after bob auth failure = %d, want 2", got)
	}
	_, texts := telegramFake.snapshot()
	for _, text := range texts {
		for _, want := range []string{"Problem", "Konto", "bob"} {
			if !strings.Contains(text, want) {
				t.Fatalf("failure text missing %q: %q", want, text)
			}
		}
	}
	// Secrets never reach outputs, logs, JSON, or SQLite.
	for _, output := range []string{bobFailed.stdout, bobFailed.stderr} {
		if strings.Contains(output, wrongMarker) || strings.Contains(output, "TOKEN-INCIDENT-ONE-unique") || strings.Contains(output, "TOKEN-INCIDENT-TWO-unique") {
			t.Fatalf("output leaks secret: %q", output)
		}
	}

	// Repeated failures update the incident without new notifications.
	secondBob := run(t, environment, "check", "--profile", "bob-morning", "--output", "json", "--non-interactive")
	if secondBob.exitCode != 3 {
		t.Fatalf("second bob check = %#v", secondBob)
	}
	if got := incidentQuery(t, database, "SELECT consecutive_failures FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'bob'"); got != "2" {
		t.Fatalf("bob consecutive after repeat = %s, want 2", got)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("telegram after repeated bob failure = %d, want still 2 (no noise)", got)
	}

	// Independent work continues: alice with good credentials succeeds.
	aliceOk := run(t, environment, "check", "--profile", "alice-morning", "--output", "json", "--non-interactive")
	if aliceOk.exitCode != 0 {
		t.Fatalf("alice check during bob incident = %#v, want success (isolation)", aliceOk)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'alice' AND status = 'active'"); got != "0" {
		t.Fatalf("alice incidents = %s, want 0", got)
	}

	// Fix bob and recover: one successful run ends the incident with one
	// recovery per destination that delivered the failure.
	if err := os.WriteFile(bobPasswordFile, []byte("bob-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	medicoverFake.setSlots(incidentSlot("booking-auth"))
	bobRecovered := run(t, environment, "check", "--profile", "bob-morning", "--output", "json", "--non-interactive")
	if bobRecovered.exitCode != 0 {
		t.Fatalf("bob recovery check = %#v", bobRecovered)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'bob' ORDER BY first_seen_at DESC LIMIT 1"); got != "resolved" {
		t.Fatalf("bob incident status = %s, want resolved", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'recovery' AND status = 'delivered'"); got != "2" {
		t.Fatalf("recovery deliveries = %s, want 2", got)
	}
	_, afterTexts := telegramFake.snapshot()
	foundRecovery := false
	for _, text := range afterTexts {
		if strings.Contains(text, "wznowione") {
			foundRecovery = true
			break
		}
	}
	if !foundRecovery {
		t.Fatalf("no Polish recovery message (wznowione) in %q", afterTexts)
	}
	// Recovery must not leak secrets either.
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), wrongMarker) || strings.Contains(string(raw), "TOKEN-INCIDENT-ONE-unique") || strings.Contains(string(raw), "TOKEN-INCIDENT-TWO-unique") {
		t.Fatal("database file contains secret")
	}
	for _, output := range []string{bobRecovered.stdout, bobRecovered.stderr} {
		if strings.Contains(output, "TOKEN-INCIDENT-ONE-unique") || strings.Contains(output, "TOKEN-INCIDENT-TWO-unique") {
			t.Fatalf("recovery output leaks token: %q", output)
		}
	}
}

func TestIncidentProtocolChangeStartsImmediatelyAndRecovers(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	// Single account alice for a focused protocol test.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = database
	_ = secretDir
	// Reuse alice from fixture (first account). Create destinations + profile.
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "proto", "alice", "204", "phone")

	medicoverFake.setMode("protocol")
	failed := run(t, environment, "check", "--profile", "proto", "--output", "json", "--non-interactive")
	if failed.exitCode != 5 || !strings.Contains(failed.stderr, "protocol_changed") {
		t.Fatalf("protocol check = %#v, want exit 5 protocol_changed", failed)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'proto' AND status = 'active'"); got != "1" {
		t.Fatalf("profile incidents = %s, want 1 immediate", got)
	}
	if got := incidentQuery(t, database, "SELECT kind FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'proto'"); got != "protocol_changed" {
		t.Fatalf("incident kind = %s, want protocol_changed", got)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after protocol failure = %d, want 1", got)
	}

	// Repeated protocol failures update without new notifications.
	again := run(t, environment, "check", "--profile", "proto", "--output", "json", "--non-interactive")
	if again.exitCode != 5 {
		t.Fatalf("second protocol check = %#v", again)
	}
	if got := incidentQuery(t, database, "SELECT consecutive_failures FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'proto'"); got != "2" {
		t.Fatalf("consecutive = %s, want 2", got)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after repeat protocol = %d, want still 1", got)
	}

	// Recovery with empty slots (no availability noise, only recovery).
	medicoverFake.setSlots()
	recovered := run(t, environment, "check", "--profile", "proto", "--output", "json", "--non-interactive")
	if recovered.exitCode != 0 {
		t.Fatalf("protocol recovery = %#v", recovered)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'proto'"); got != "resolved" {
		t.Fatalf("incident status = %s, want resolved", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'recovery' AND status = 'delivered'"); got != "1" {
		t.Fatalf("recoveries = %s, want 1", got)
	}
}

func TestIncidentTemporaryThresholdNeedsThreeConsecutive(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	// Single destination keeps counts focused.
	singleToken := writeProcessSecretFile(t, secretDir, "single-token", "TOKEN-SINGLE-unique")
	created := run(t, environment, "telegram", "create", "--telegram", "single", "--name", "Solo", "--chat-id", "999999", "--token-file", singleToken, "--non-interactive")
	if created.exitCode != 0 {
		t.Fatalf("create single = %#v", created)
	}
	solo := run(t, environment, "profile", "create", "--profile", "solo", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "single", "--non-interactive")
	if solo.exitCode != 0 {
		t.Fatalf("create solo = %#v", solo)
	}

	medicoverFake.setMode("temporary")
	for attempt := 1; attempt <= 2; attempt++ {
		failed := run(t, environment, "check", "--profile", "solo", "--output", "json", "--non-interactive")
		if failed.exitCode != 4 || !strings.Contains(failed.stderr, "temporary_failure") {
			t.Fatalf("attempt %d = %#v, want temporary_failure", attempt, failed)
		}
		if got := incidentQuery(t, database, "SELECT consecutive_failures FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'solo'"); got != string(rune('0'+attempt)) {
			t.Fatalf("consecutive after %d = %s", attempt, got)
		}
		if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE incident_id IN (SELECT id FROM operational_incidents WHERE scope_id = 'solo')"); got != "0" {
			t.Fatalf("deliveries after %d failures = %s, want 0 (not yet notifiable)", attempt, got)
		}
		if got := telegramFake.requestCount(); got != 0 {
			t.Fatalf("telegram after %d temporary = %d, want 0", attempt, got)
		}
	}

	third := run(t, environment, "check", "--profile", "solo", "--output", "json", "--non-interactive")
	if third.exitCode != 4 {
		t.Fatalf("third = %#v", third)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'failure' AND status = 'delivered'"); got != "1" {
		t.Fatalf("failure deliveries after 3rd = %s, want 1", got)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after 3rd = %d, want 1", got)
	}

	fourth := run(t, environment, "check", "--profile", "solo", "--output", "json", "--non-interactive")
	if fourth.exitCode != 4 {
		t.Fatalf("fourth = %#v", fourth)
	}
	if got := incidentQuery(t, database, "SELECT consecutive_failures FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'solo'"); got != "4" {
		t.Fatalf("consecutive after 4th = %s, want 4", got)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after 4th = %d, want still 1 (no noise)", got)
	}

	// Recovery ends the incident with one recovery notification.
	medicoverFake.setSlots()
	recovered := run(t, environment, "check", "--profile", "solo", "--output", "json", "--non-interactive")
	if recovered.exitCode != 0 {
		t.Fatalf("recovery = %#v", recovered)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'solo'"); got != "resolved" {
		t.Fatalf("status = %s, want resolved", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'recovery' AND status = 'delivered'"); got != "1" {
		t.Fatalf("recoveries = %s, want 1", got)
	}
}

func TestIncidentPermanentDeliveryFailureVisible(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-perm"))
	telegramFake := newIncidentTelegramFake()
	telegramFake.failChat("654321", "permanent")
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "perm", "alice", "204", "phone,backup")

	first := run(t, environment, "check", "--profile", "perm", "--output", "json", "--non-interactive")
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v (observation success wins despite permanent delivery)", first)
	}
	if got := incidentQuery(t, database, "SELECT status FROM telegram_deliveries WHERE destination_id = 'backup'"); got != "permanent_failure" {
		t.Fatalf("backup delivery = %s, want permanent_failure durable", got)
	}
	if got := incidentQuery(t, database, "SELECT status FROM telegram_deliveries WHERE destination_id = 'phone'"); got != "delivered" {
		t.Fatalf("phone delivery = %s, want delivered (isolation)", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'destination' AND status = 'active'"); got != "1" {
		t.Fatalf("destination incidents = %s, want 1 visible incident state", got)
	}
	// Failure reported through the other route (phone), never the failed route.
	if got := incidentQuery(t, database, "SELECT destination_id FROM operational_deliveries WHERE kind = 'failure' AND status = 'delivered' LIMIT 1"); got != "phone" {
		t.Fatalf("incident failure notifier = %s, want phone (not failed backup)", got)
	}
	_, texts := telegramFake.snapshot()
	foundAvailability := false
	foundFailure := false
	for _, text := range texts {
		if strings.Contains(text, "Dostępny termin") {
			foundAvailability = true
		}
		if strings.Contains(text, "Problem") && strings.Contains(text, "backup") {
			foundFailure = true
		}
	}
	if !foundAvailability || !foundFailure {
		t.Fatalf("texts missing availability (%v) or destination failure (%v): %q", foundAvailability, foundFailure, texts)
	}

	// Fix the route and create a new episode: the destination incident
	// resolves with a recovery through the route that delivered the failure.
	telegramFake.fixChat("654321")
	medicoverFake.setSlots()
	disappeared := run(t, environment, "check", "--profile", "perm", "--output", "json", "--non-interactive")
	if disappeared.exitCode != 0 {
		t.Fatalf("disappearance = %#v", disappeared)
	}
	medicoverFake.setSlots(incidentSlot("booking-perm-2"))
	returned := run(t, environment, "check", "--profile", "perm", "--output", "json", "--non-interactive")
	if returned.exitCode != 0 {
		t.Fatalf("return = %#v", returned)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'destination' ORDER BY first_seen_at DESC LIMIT 1"); got != "resolved" {
		t.Fatalf("destination incident = %s, want resolved after route recovers", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'recovery' AND status = 'delivered'"); got != "1" {
		t.Fatalf("destination recoveries = %s, want 1", got)
	}
}

func TestIncidentFailedSearchKeepsEpisodes(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-keep"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "keep", "alice", "204", "phone")

	first := run(t, environment, "check", "--profile", "keep", "--output", "json", "--non-interactive")
	if first.exitCode != 0 {
		t.Fatalf("first = %#v", first)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM availability_episodes WHERE active = 1"); got != "1" {
		t.Fatalf("active episodes = %s, want 1", got)
	}
	// Failed and partial searches never end episodes.
	medicoverFake.setMode("temporary")
	failed := run(t, environment, "check", "--profile", "keep", "--output", "json", "--non-interactive")
	if failed.exitCode != 4 {
		t.Fatalf("failed = %#v", failed)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM availability_episodes WHERE active = 1"); got != "1" {
		t.Fatalf("active after failed = %s, want still 1", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM observation_runs WHERE status = 'failed'"); got != "1" {
		t.Fatalf("failed runs = %s, want 1", got)
	}
}

func TestIncidentProfileIsolationInWatch(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-iso"))
	// Region 999 always triggers a protocol failure; other regions succeed.
	medicoverFake.setFailRegion("999")
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "good", "alice", "204", "phone")
	createIncidentProfile(t, environment, "bad", "alice", "999", "phone")

	result := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if result.exitCode != 0 {
		t.Fatalf("watch --once = %#v (one profile failure must not stop the other)", result)
	}
	if !strings.Contains(result.stdout, `"profile":"good"`) || !strings.Contains(result.stdout, `"profile":"bad"`) {
		t.Fatalf("watch events missing isolation profiles: %q", result.stdout)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'bad' AND status = 'active'"); got != "1" {
		t.Fatalf("bad incidents = %s, want 1", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'good' AND status = 'active'"); got != "0" {
		t.Fatalf("good incidents = %s, want 0 (isolation)", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'good' AND status = 'complete'"); got != "1" {
		t.Fatalf("good runs = %s, want 1 complete", got)
	}
}

func TestIncidentRetrySentWithoutProfileRun(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	telegramFake := newIncidentTelegramFake()
	telegramFake.setGlobal("temporary")
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	singleToken := writeProcessSecretFile(t, secretDir, "single-token", "TOKEN-RETRY-UNIQUE")
	if created := run(t, environment, "telegram", "create", "--telegram", "single", "--name", "Solo", "--chat-id", "999999", "--token-file", singleToken, "--non-interactive"); created.exitCode != 0 {
		t.Fatalf("create single = %#v", created)
	}
	if created := run(t, environment, "profile", "create", "--profile", "solo", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "single", "--non-interactive"); created.exitCode != 0 {
		t.Fatalf("create solo = %#v", created)
	}

	// Protocol failure is immediately notifiable, but every Telegram send
	// fails temporarily, leaving a retry delivery that is due immediately.
	medicoverFake.setMode("protocol")
	failed := run(t, environment, "check", "--profile", "solo", "--output", "json", "--non-interactive")
	if failed.exitCode != 5 {
		t.Fatalf("protocol check = %#v, want exit 5", failed)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_deliveries WHERE kind = 'failure' LIMIT 1"); got != "retry" {
		t.Fatalf("failure delivery = %s, want retry", got)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after failed check = %d, want 1", got)
	}

	// The profile just ran inside its 30-minute interval, so no observation
	// is due. The retry must still go out on the next watch iteration
	// without waiting for another profile check.
	telegramFake.setGlobal("success")
	watched := run(t, environment, "watch", "--once", "--poll-interval", "100ms", "--output", "json", "--non-interactive")
	if watched.exitCode != 0 {
		t.Fatalf("watch --once = %#v", watched)
	}
	if got := telegramFake.requestCount(); got != 2 {
		t.Fatalf("telegram after watch = %d, want 2 (retry sent without a profile run)", got)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_deliveries WHERE kind = 'failure' LIMIT 1"); got != "delivered" {
		t.Fatalf("failure delivery after retry = %s, want delivered", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM observation_runs WHERE profile_id = 'solo'"); got != "1" {
		t.Fatalf("observation runs = %s, want 1 (no new run scheduled the retry)", got)
	}
}

func TestIncidentSearchAuthCreatesOnlyAccountIncident(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-search-auth"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "searchauth", "alice", "204", "phone")

	// Login succeeds but the search endpoint rejects the token: one
	// account-level problem, not one problem per profile.
	medicoverFake.setMode("auth")
	failed := run(t, environment, "check", "--profile", "searchauth", "--output", "json", "--non-interactive")
	if failed.exitCode != 3 || !strings.Contains(failed.stderr, "authentication_required") {
		t.Fatalf("search-auth check = %#v, want exit 3 authentication_required", failed)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'alice' AND status = 'active'"); got != "1" {
		t.Fatalf("account incidents = %s, want 1", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_incidents WHERE scope_type = 'profile' AND scope_id = 'searchauth'"); got != "0" {
		t.Fatalf("profile incidents = %s, want 0 (no duplicate alert)", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'failure'"); got != "1" {
		t.Fatalf("failure deliveries = %s, want 1 (not duplicated)", got)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after search-auth failure = %d, want 1", got)
	}

	// A repeated failure updates the same account incident without noise.
	again := run(t, environment, "check", "--profile", "searchauth", "--output", "json", "--non-interactive")
	if again.exitCode != 3 {
		t.Fatalf("second search-auth check = %#v", again)
	}
	if got := telegramFake.requestCount(); got != 1 {
		t.Fatalf("telegram after repeat = %d, want still 1", got)
	}

	// One successful run ends the single incident with one recovery.
	medicoverFake.setSlots(incidentSlot("booking-search-auth"))
	recovered := run(t, environment, "check", "--profile", "searchauth", "--output", "json", "--non-interactive")
	if recovered.exitCode != 0 {
		t.Fatalf("recovery check = %#v", recovered)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'account' AND scope_id = 'alice'"); got != "resolved" {
		t.Fatalf("account incident = %s, want resolved", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'recovery' AND status = 'delivered'"); got != "1" {
		t.Fatalf("recoveries = %s, want 1 (not duplicated)", got)
	}
}

func TestIncidentMixedPassKeepsDestinationFailure(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-mixed-a"))
	telegramFake := newIncidentTelegramFake()
	// The first phone send fails temporarily, leaving episode A with a
	// retryable delivery for both destinations.
	telegramFake.failChat("123456", "temporary")
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "mixed", "alice", "204", "phone,backup")

	first := run(t, environment, "check", "--profile", "mixed", "--output", "json", "--non-interactive")
	if first.exitCode != 0 {
		t.Fatalf("first check = %#v", first)
	}

	// The next phone send fails permanently while a new episode delivers
	// through the same route in the same pass.
	telegramFake.fixChat("123456")
	telegramFake.failChatOnce("123456")
	medicoverFake.setSlots(incidentSlot("booking-mixed-a"), incidentSlot("booking-mixed-b"))
	second := run(t, environment, "check", "--profile", "mixed", "--output", "json", "--non-interactive")
	if second.exitCode != 0 {
		t.Fatalf("second check = %#v", second)
	}
	// The permanent phone failure must stay reported even though the same
	// destination delivered the new episode in the same pass.
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'destination' AND scope_id = 'phone' ORDER BY first_seen_at DESC LIMIT 1"); got != "active" {
		t.Fatalf("destination incident = %s, want active (failure wins over same-pass success)", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'failure' AND status = 'delivered'"); got != "1" {
		t.Fatalf("failure deliveries = %s, want 1 reported through backup", got)
	}

	// A later all-success pass resolves the incident with a recovery.
	medicoverFake.setSlots(incidentSlot("booking-mixed-a"), incidentSlot("booking-mixed-b"), incidentSlot("booking-mixed-c"))
	third := run(t, environment, "check", "--profile", "mixed", "--output", "json", "--non-interactive")
	if third.exitCode != 0 {
		t.Fatalf("third check = %#v", third)
	}
	if got := incidentQuery(t, database, "SELECT status FROM operational_incidents WHERE scope_type = 'destination' AND scope_id = 'phone' ORDER BY first_seen_at DESC LIMIT 1"); got != "resolved" {
		t.Fatalf("destination incident = %s, want resolved", got)
	}
	if got := incidentQuery(t, database, "SELECT count(*) FROM operational_deliveries WHERE kind = 'recovery' AND status = 'delivered'"); got != "1" {
		t.Fatalf("recoveries = %s, want 1", got)
	}
}
