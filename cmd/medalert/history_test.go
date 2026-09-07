package main_test

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func historyEnvelope(t *testing.T, stdout, wantCommand string) map[string]any {
	t.Helper()
	var envelope struct {
		SchemaVersion int            `json:"schema_version"`
		Command       string         `json:"command"`
		Data          map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %s JSON: %v\n%s", wantCommand, err, stdout)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != wantCommand {
		t.Fatalf("envelope = %+v, want command %q", envelope, wantCommand)
	}
	return envelope.Data
}

func TestHistoryShowsRunsFailuresEpisodesIncidentsDeliveries(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-history"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	_, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "history-prof", "alice", "204", "phone")

	// One successful check creates a run, an episode, and a delivery.
	ok := run(t, environment, "check", "--profile", "history-prof", "--output", "json", "--non-interactive")
	if ok.exitCode != 0 {
		t.Fatalf("successful check = %#v", ok)
	}
	runs := run(t, environment, "history", "runs", "--profile", "history-prof", "--output", "json", "--non-interactive")
	if runs.exitCode != 0 {
		t.Fatalf("history runs = %#v", runs)
	}
	runsData := historyEnvelope(t, runs.stdout, "history runs")
	if list, _ := runsData["runs"].([]any); len(list) == 0 {
		t.Fatalf("history runs is empty after a successful check")
	}
	episodes := run(t, environment, "history", "episodes", "--profile", "history-prof", "--output", "json", "--non-interactive")
	if episodes.exitCode != 0 {
		t.Fatalf("history episodes = %#v", episodes)
	}
	episodesData := historyEnvelope(t, episodes.stdout, "history episodes")
	if list, _ := episodesData["episodes"].([]any); len(list) == 0 {
		t.Fatalf("history episodes is empty after a successful check")
	}
	deliveries := run(t, environment, "history", "deliveries", "--profile", "history-prof", "--output", "json", "--non-interactive")
	if deliveries.exitCode != 0 {
		t.Fatalf("history deliveries = %#v", deliveries)
	}
	deliveriesData := historyEnvelope(t, deliveries.stdout, "history deliveries")
	if list, _ := deliveriesData["deliveries"].([]any); len(list) == 0 {
		t.Fatalf("history deliveries is empty after a successful check")
	}

	// Three temporary failures make the profile incident notifiable.
	medicoverFake.setMode("temporary")
	for range 3 {
		failed := run(t, environment, "check", "--profile", "history-prof", "--output", "json", "--non-interactive")
		if failed.exitCode != 4 {
			t.Fatalf("temporary check = %#v, want exit 4", failed)
		}
	}
	incidents := run(t, environment, "history", "incidents", "--output", "json", "--non-interactive")
	if incidents.exitCode != 0 {
		t.Fatalf("history incidents = %#v", incidents)
	}
	incidentsData := historyEnvelope(t, incidents.stdout, "history incidents")
	if list, _ := incidentsData["incidents"].([]any); len(list) == 0 {
		t.Fatalf("history incidents is empty after three temporary failures")
	}
	// Failed runs are history too.
	failedRuns := run(t, environment, "history", "runs", "--profile", "history-prof", "--status", "failed", "--output", "json", "--non-interactive")
	if failedRuns.exitCode != 0 {
		t.Fatalf("history failed runs = %#v", failedRuns)
	}
	failedData := historyEnvelope(t, failedRuns.stdout, "history runs")
	if list, _ := failedData["runs"].([]any); len(list) == 0 {
		t.Fatalf("no failed runs in history after temporary failures")
	}
	// Incident deliveries are history too.
	incidentDeliveries := run(t, environment, "history", "incident-deliveries", "--output", "json", "--non-interactive")
	if incidentDeliveries.exitCode != 0 {
		t.Fatalf("history incident-deliveries = %#v", incidentDeliveries)
	}
	incidentData := historyEnvelope(t, incidentDeliveries.stdout, "history incident-deliveries")
	if list, _ := incidentData["deliveries"].([]any); len(list) == 0 {
		t.Fatalf("no incident deliveries after notifiable incident")
	}
}

func TestHistoryStatusIdentifiesRequiredActions(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-status"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	_ = database
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "status-prof", "alice", "204", "phone")

	// Disabled configuration is a required action.
	disabled := run(t, environment, "profile", "disable", "--profile", "status-prof", "--non-interactive")
	if disabled.exitCode != 0 {
		t.Fatalf("disable = %#v", disabled)
	}
	telegramFake.failChat("123456", "permanent")
	medicoverFake.setSlots(incidentSlot("booking-status-permanent"))
	// Re-enable for the permanent-failure check, then disable again so both
	// disabled and permanent states appear in status.
	enabled := run(t, environment, "profile", "enable", "--profile", "status-prof", "--non-interactive")
	if enabled.exitCode != 0 {
		t.Fatalf("enable = %#v", enabled)
	}
	permanent := run(t, environment, "check", "--profile", "status-prof", "--output", "json", "--non-interactive")
	if permanent.exitCode != 0 {
		t.Fatalf("permanent check = %#v", permanent)
	}
	redisable := run(t, environment, "profile", "disable", "--profile", "status-prof", "--non-interactive")
	if redisable.exitCode != 0 {
		t.Fatalf("redisable = %#v", redisable)
	}
	// Authentication required: remove the session state.
	sessions := filepath.Join(root, "sessions")
	_ = os.RemoveAll(sessions)

	status := run(t, environment, "history", "status", "--output", "json", "--non-interactive")
	if status.exitCode != 0 {
		t.Fatalf("history status = %#v", status)
	}
	data := historyEnvelope(t, status.stdout, "history status")
	actions, _ := data["required_actions"].([]any)
	codes := map[string]bool{}
	for _, raw := range actions {
		if action, ok := raw.(map[string]any); ok {
			if code, ok := action["code"].(string); ok {
				codes[code] = true
			}
		}
	}
	for _, want := range []string{"authentication_required", "profile_disabled", "permanent_failure"} {
		if !codes[want] {
			t.Fatalf("status missing %q, got %v\n%s", want, codes, status.stdout)
		}
	}
	// Active incidents appear when a protocol failure starts one immediately.
	medicoverFake.setMode("protocol")
	protocolProfile := run(t, environment, "profile", "enable", "--profile", "status-prof", "--non-interactive")
	_ = protocolProfile
	protocolCheck := run(t, environment, "check", "--profile", "status-prof", "--output", "json", "--non-interactive")
	if protocolCheck.exitCode != 5 {
		t.Fatalf("protocol check = %#v, want exit 5", protocolCheck)
	}
	statusAfter := run(t, environment, "history", "status", "--output", "json", "--non-interactive")
	dataAfter := historyEnvelope(t, statusAfter.stdout, "history status")
	actionsAfter, _ := dataAfter["required_actions"].([]any)
	foundIncident := false
	for _, raw := range actionsAfter {
		if action, ok := raw.(map[string]any); ok && action["code"] == "active_incident" {
			foundIncident = true
		}
	}
	if !foundIncident {
		t.Fatalf("status missing active_incident after protocol failure: %s", statusAfter.stdout)
	}
	// Doctor shares the same diagnostics contract.
	doctor := run(t, environment, "doctor", "--output", "json", "--non-interactive")
	if doctor.exitCode != 0 {
		t.Fatalf("doctor = %#v", doctor)
	}
	doctorData := historyEnvelope(t, doctor.stdout, "doctor")
	if _, ok := doctorData["required_actions"]; !ok {
		t.Fatalf("doctor missing required_actions: %s", doctor.stdout)
	}
	if _, ok := doctorData["retention_days"]; !ok {
		t.Fatalf("doctor missing retention_days: %s", doctor.stdout)
	}
}

func TestHistoryRetentionRemovesOldAutomatically(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-retention"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	database, environment, secretDir := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 1)
	createIncidentDestinations(t, environment, secretDir)
	createIncidentProfile(t, environment, "retention-prof", "alice", "204", "phone")

	ok := run(t, environment, "check", "--profile", "retention-prof", "--output", "json", "--non-interactive")
	if ok.exitCode != 0 {
		t.Fatalf("check = %#v", ok)
	}
	// Backdate the run so it is older than a 1-day policy.
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	old := "2020-01-01T00:00:00Z"
	if _, err := db.Exec(`UPDATE observation_runs SET started_at = ? WHERE profile_id = ?`, old, "retention-prof"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	setRetention := run(t, environment, "history", "retention", "--retention-days", "1", "--non-interactive")
	if setRetention.exitCode != 0 {
		t.Fatalf("set retention = %#v", setRetention)
	}
	// Any history read prunes automatically according to the saved policy.
	runs := run(t, environment, "history", "runs", "--profile", "retention-prof", "--output", "json", "--non-interactive")
	if runs.exitCode != 0 {
		t.Fatalf("history runs after retention = %#v", runs)
	}
	runsData := historyEnvelope(t, runs.stdout, "history runs")
	if list, _ := runsData["runs"].([]any); len(list) != 0 {
		t.Fatalf("old run was not pruned automatically: %v", list)
	}
	// Manual prune reports the same saved policy behavior.
	prune := run(t, environment, "history", "prune", "--output", "json", "--non-interactive")
	if prune.exitCode != 0 {
		t.Fatalf("prune = %#v", prune)
	}
	historyEnvelope(t, prune.stdout, "history prune")
}

func TestAutomationCommandGroupsAndContracts(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := t.TempDir()
	_, environment, _ := createIncidentFixture(t, medicoverFake.baseURL, telegramServer.URL, root, 0)

	// Every documented English command group responds (exit 0 or a usage
	// error that still names the group, never "a supported command").
	groups := [][]string{
		{"account", "list", "--non-interactive"},
		{"profile", "list", "--non-interactive"},
		{"telegram", "list", "--non-interactive"},
		{"history", "status", "--non-interactive"},
		{"doctor", "--non-interactive"},
		{"version"},
		{"completion", "bash"},
	}
	for _, args := range groups {
		result := run(t, environment, args...)
		if result.exitCode != 0 {
			t.Fatalf("group %v = %#v", args, result)
		}
	}
	// check and watch need state; their usage errors still prove the group
	// exists and follows the JSON envelope contract.
	checkMissing := run(t, environment, "check", "--output", "json", "--non-interactive")
	if checkMissing.exitCode != 2 || !strings.Contains(checkMissing.stderr, `"schema_version":1`) {
		t.Fatalf("check missing profile = %#v", checkMissing)
	}
	watchOnce := run(t, environment, "watch", "--once", "--poll-interval", "50ms", "--output", "json", "--non-interactive")
	if watchOnce.exitCode != 0 || !strings.Contains(watchOnce.stdout, `"command":"watch"`) {
		t.Fatalf("watch --once = %#v", watchOnce)
	}
	// Finishing commands use the versioned JSON envelope.
	for _, args := range [][]string{
		{"account", "list", "--output", "json", "--non-interactive"},
		{"history", "status", "--output", "json", "--non-interactive"},
		{"doctor", "--output", "json", "--non-interactive"},
		{"completion", "bash", "--output", "json"},
	} {
		result := run(t, environment, args...)
		if result.exitCode != 0 {
			t.Fatalf("json %v = %#v", args, result)
		}
		var envelope struct {
			SchemaVersion int    `json:"schema_version"`
			Command       string `json:"command"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &envelope); err != nil {
			t.Fatalf("decode %v: %v", args, err)
		}
		if envelope.SchemaVersion != 1 || envelope.Command == "" {
			t.Fatalf("envelope %v = %+v", args, envelope)
		}
	}
	// Non-interactive execution never prompts: missing required input is a
	// usage error, not a block.
	missing := run(t, environment, "account", "create", "--account", "nopass", "--username", "u@example.com", "--non-interactive", "--output", "json")
	if missing.exitCode != 2 || !strings.Contains(missing.stderr, `"code":"missing_input"`) {
		t.Fatalf("non-interactive missing input = %#v", missing)
	}
}

func TestFinalAcceptanceSecretMarkersStayOutOfOutputsAndHistory(t *testing.T) {
	medicoverFake, medicoverCleanup := newIncidentMedicoverFake(t)
	defer medicoverCleanup()
	medicoverFake.setSlots(incidentSlot("booking-secret"))
	telegramFake := newIncidentTelegramFake()
	telegramServer := httptest.NewServer(telegramFake.handler())
	defer telegramServer.Close()

	root := privateTempDir(t)
	database := filepath.Join(root, "medalert.db")
	sessionDir := filepath.Join(root, "sessions")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "MARKER-HISTORY-" + t.Name() + "-unique"
	passwordFile := writeProcessSecretFile(t, secretDir, "pw", marker+"-pass")
	tokenFile := writeProcessSecretFile(t, secretDir, "tok", marker+"-token")
	environment := []string{
		"MEDALERT_DATABASE=" + database,
		"MEDALERT_SESSION_DIR=" + sessionDir,
		"MEDALERT_MEDICOVER_BASE_URL=" + medicoverFake.baseURL,
		"MEDALERT_TELEGRAM_BASE_URL=" + telegramServer.URL,
		"MEDALERT_NON_INTERACTIVE=true",
	}
	created := run(t, environment, "account", "create", "--account", "alice", "--username", "alice@example.com", "--password-file", passwordFile, "--non-interactive")
	if created.exitCode != 0 {
		t.Fatalf("create account = %#v", created)
	}
	// Overwrite the secret files with the marker after creation so the stored
	// references stay valid but the values are unique markers.
	if err := os.WriteFile(passwordFile, []byte(marker+"-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point the account at the marker password file.
	edited := run(t, environment, "account", "edit", "--account", "alice", "--password-file", passwordFile, "--non-interactive")
	if edited.exitCode != 0 {
		t.Fatalf("edit account = %#v", edited)
	}
	createdDest := run(t, environment, "telegram", "create", "--telegram", "phone", "--name", "T", "--chat-id", "123456", "--token-file", tokenFile, "--non-interactive")
	if createdDest.exitCode != 0 {
		t.Fatalf("create dest = %#v", createdDest)
	}
	createdProfile := run(t, environment, "profile", "create", "--profile", "secret-prof", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "phone", "--non-interactive")
	if createdProfile.exitCode != 0 {
		t.Fatalf("create profile = %#v", createdProfile)
	}
	// Use the real password the fake accepts for the check, then restore the
	// marker so history and logs are exercised with marker secrets present.
	if err := os.WriteFile(passwordFile, []byte("durable-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fixture account uses patient credentials in the fake; create a
	// matching patient account for the durable check.
	_ = run(t, environment, "account", "create", "--account", "patient", "--username", "patient@example.com", "--password-file", passwordFile, "--non-interactive")
	_ = run(t, environment, "profile", "create", "--profile", "patient-prof", "--account", "patient", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "phone", "--non-interactive")
	checkResult := run(t, environment, "check", "--profile", "patient-prof", "--output", "json", "--non-interactive")
	if checkResult.exitCode != 0 {
		t.Fatalf("check = %#v", checkResult)
	}
	if err := os.WriteFile(passwordFile, []byte(marker+"-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(marker+"-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputs := []string{checkResult.stdout, checkResult.stderr}
	for _, args := range [][]string{
		{"history", "runs", "--profile", "patient-prof", "--output", "json", "--non-interactive"},
		{"history", "episodes", "--profile", "patient-prof", "--output", "json", "--non-interactive"},
		{"history", "incidents", "--output", "json", "--non-interactive"},
		{"history", "deliveries", "--profile", "patient-prof", "--output", "json", "--non-interactive"},
		{"history", "status", "--output", "json", "--non-interactive"},
		{"history", "runs", "--profile", "patient-prof", "--non-interactive"},
		{"history", "status", "--non-interactive"},
		{"doctor", "--output", "json", "--non-interactive"},
		{"doctor", "--non-interactive"},
	} {
		result := run(t, environment, args...)
		outputs = append(outputs, result.stdout, result.stderr)
	}
	for _, output := range outputs {
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
