package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/application"
	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/operatorstatus"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"
)

func historyTestEnv(root string) func(string) string {
	return func(key string) string {
		switch key {
		case "XDG_DATA_HOME", "MEDALERT_OUTPUT", "MEDALERT_DATABASE", "MEDALERT_NON_INTERACTIVE", "HOME", "MEDALERT_SESSION_DIR", "MEDALERT_MEDICOVER_BASE_URL", "MEDALERT_TELEGRAM_BASE_URL":
			return ""
		default:
			return ""
		}
	}
}

func setupHistoryFixture(t *testing.T) (string, func(string) string) {
	t.Helper()
	keyring.MockInit()
	root := t.TempDir()
	database := privateDB(t, root, "medalert.db")
	getenv := historyTestEnv(root)
	sessionDir := filepath.Join(root, "sessions")
	getenvWithSession := func(key string) string {
		if key == "MEDALERT_SESSION_DIR" {
			return sessionDir
		}
		return getenv(key)
	}
	// Create one account (prompt source so no secret file is needed), one
	// destination, and one profile. The account has no session, so status
	// must report authentication required.
	if code, _, stderr := runCLI(t, getenvWithSession, "account", "create", "--database", database, "--non-interactive", "--account", "alice", "--username", "alice@example.com", "--no-stored-password"); code != 0 {
		t.Fatalf("create account: code=%d stderr=%q", code, stderr)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(store.Destination{ID: "dest", Name: "Test", ChatID: "123456", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	storage.Close()
	if code, _, stderr := runCLI(t, getenvWithSession, "profile", "create", "--database", database, "--non-interactive", "--profile", "prof", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "dest"); code != 0 {
		t.Fatalf("create profile: code=%d stderr=%q", code, stderr)
	}
	return database, getenvWithSession
}

func decodeEnvelope(t *testing.T, stdout, wantCommand string) map[string]any {
	t.Helper()
	var envelope struct {
		SchemaVersion int            `json:"schema_version"`
		Command       string         `json:"command"`
		Data          map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, stdout)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != wantCommand {
		t.Fatalf("envelope = %+v, want command %q", envelope, wantCommand)
	}
	return envelope.Data
}

func TestHistoryRunsEmptyAndJSONEnvelope(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	code, stdout, stderr := runCLI(t, getenv, "history", "runs", "--database", database, "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("runs json: code=%d stderr=%q", code, stderr)
	}
	data := decodeEnvelope(t, stdout, "history runs")
	if _, ok := data["runs"]; !ok {
		t.Fatalf("data missing runs: %v", data)
	}
	code, stdout, _ = runCLI(t, getenv, "history", "runs", "--database", database, "--profile", "prof")
	if code != 0 || !strings.Contains(stdout, "No observation runs found.") {
		t.Fatalf("runs text: code=%d stdout=%q", code, stdout)
	}
}

func TestHistoryRunsStatusFilterAppliesBeforeLimit(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	oldRun, err := storage.BeginObservationRun("prof", now)
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.FailObservationRun(oldRun.ID, store.ObservationRunFailed, "temporary_failure", "old failure", now); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	newRun, err := storage.BeginObservationRun("prof", now.Add(time.Minute))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(newRun.ID, nil, now.Add(time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	storage.Close()

	code, stdout, stderr := runCLI(t, getenv, "history", "runs", "--database", database, "--status", "failed", "--limit", "1", "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("history runs: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	data := decodeEnvelope(t, stdout, "history runs")
	runs, ok := data["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("runs = %#v, want one failed run", data["runs"])
	}
	if run, ok := runs[0].(map[string]any); !ok || run["id"] != oldRun.ID || run["status"] != store.ObservationRunFailed {
		t.Fatalf("run = %#v, want old failed run %s", runs[0], oldRun.ID)
	}
}

func TestHistoryEpisodesEndedFilterAppliesBeforeLimit(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	firstRun, err := storage.BeginObservationRun("prof", now)
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	first, err := storage.ReconcileObservationRun(firstRun.ID, []store.ObservationSlot{{Identity: "old-slot", StableIdentity: "old-stable", BookingString: "old-booking", Time: "2099-01-10T10:00:00Z"}}, now)
	if err != nil || len(first.NewEpisodes) != 1 {
		storage.Close()
		t.Fatalf("first reconciliation = %#v, err=%v", first, err)
	}
	endRun, err := storage.BeginObservationRun("prof", now.Add(time.Minute))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(endRun.ID, nil, now.Add(time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	activeRun, err := storage.BeginObservationRun("prof", now.Add(2*time.Minute))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(activeRun.ID, []store.ObservationSlot{{Identity: "new-slot", StableIdentity: "new-stable", BookingString: "new-booking", Time: "2099-01-10T10:00:00Z"}}, now.Add(2*time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	storage.Close()

	code, stdout, stderr := runCLI(t, getenv, "history", "episodes", "--database", database, "--status", "ended", "--limit", "1", "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("history episodes: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	data := decodeEnvelope(t, stdout, "history episodes")
	episodes, ok := data["episodes"].([]any)
	if !ok || len(episodes) != 1 {
		t.Fatalf("episodes = %#v, want one ended episode", data["episodes"])
	}
	if episode, ok := episodes[0].(map[string]any); !ok || episode["active"] != false || episode["id"] != first.NewEpisodes[0].ID {
		t.Fatalf("episode = %#v, want ended episode %s", episodes[0], first.NewEpisodes[0].ID)
	}
}

func TestHistoryStatusIdentifiesAuthAndDisabled(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	// Disable the profile and destination so status must list them.
	if code, _, stderr := runCLI(t, getenv, "profile", "disable", "--database", database, "--profile", "prof"); code != 0 {
		t.Fatalf("disable profile: %d %q", code, stderr)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SetDestinationEnabled("dest", false); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	storage.Close()

	code, stdout, stderr := runCLI(t, getenv, "history", "status", "--database", database, "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("status json: code=%d stderr=%q", code, stderr)
	}
	data := decodeEnvelope(t, stdout, "history status")
	actions, _ := data["required_actions"].([]any)
	codes := map[string]bool{}
	for _, raw := range actions {
		if action, ok := raw.(map[string]any); ok {
			if code, ok := action["code"].(string); ok {
				codes[code] = true
			}
		}
	}
	for _, want := range []string{"authentication_required", "profile_disabled", "destination_disabled"} {
		if !codes[want] {
			t.Fatalf("required actions missing %q: %v", want, codes)
		}
	}
	// Text stays human-readable and lists the same actions.
	code, stdout, _ = runCLI(t, getenv, "history", "status", "--database", database)
	if code != 0 || !strings.Contains(stdout, "authentication_required") || !strings.Contains(stdout, "profile_disabled") {
		t.Fatalf("status text: code=%d stdout=%q", code, stdout)
	}
}

func TestHistoryStatusIncludesPermanentOperationalDelivery(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	incident, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, "prof", "alice", "prof", "", "authentication_required", "login required", now, []string{"dest"})
	if err != nil || !newly || len(created) != 1 {
		storage.Close()
		t.Fatalf("record incident = %#v deliveries=%d newly=%v err=%v", incident, len(created), newly, err)
	}
	claimed, err := storage.BeginIncidentDeliveryAttempt(created[0].ID, now)
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 1}, now); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	_, recoveries, cancelled, err := storage.ResolveIncident(store.IncidentScopeProfile, "prof", "prof", "", now.Add(time.Minute))
	if err != nil || len(recoveries) != 1 || len(cancelled) != 0 {
		storage.Close()
		t.Fatalf("resolve recoveries=%d cancelled=%d err=%v", len(recoveries), len(cancelled), err)
	}
	recoveryClaim, err := storage.BeginIncidentDeliveryAttempt(recoveries[0].ID, now.Add(time.Minute))
	if err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if _, err := storage.RecordIncidentDeliveryResult(recoveryClaim.ID, recoveryClaim, store.DeliveryResult{Status: store.DeliveryPermanentFailure, LastError: "permanent test failure"}, now.Add(2*time.Minute)); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	storage.Close()

	code, stdout, stderr := runCLI(t, getenv, "history", "status", "--database", database, "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("history status: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	data := decodeEnvelope(t, stdout, "history status")
	operational, ok := data["operational_deliveries"].([]any)
	if !ok || len(operational) != 1 {
		t.Fatalf("operational_deliveries = %#v, want one permanent delivery", data["operational_deliveries"])
	}
	summary, ok := data["summary"].(map[string]any)
	if !ok || summary["permanent_failures"] != float64(1) {
		t.Fatalf("summary = %#v, want one permanent failure", data["summary"])
	}
	actions, _ := data["required_actions"].([]any)
	for _, raw := range actions {
		if action, ok := raw.(map[string]any); ok && action["code"] == "permanent_failure" && action["scope"] == "operational_delivery" {
			return
		}
	}
	t.Fatalf("required_actions = %#v, want operational permanent failure", actions)
}

func TestHistoryActionMessagePreservesIncidentAndDeliveryDetails(t *testing.T) {
	status := operatorstatus.Status{
		ActiveIncidents: []store.Incident{{
			ID: "incident-1", ScopeType: "profile", ScopeID: "profile-1", FailureCode: "authentication_required",
		}},
		PermanentFailures: []store.Delivery{{
			ID: "delivery-1", ProfileID: "profile-1", DestinationID: "destination-1",
		}},
		OperationalDeliveries: []store.IncidentDelivery{{
			ID: "operational-1", IncidentID: "incident-1", DestinationID: "destination-1", Kind: "recovery",
		}},
	}
	tests := []struct {
		name   string
		action operatorstatus.Action
		want   string
	}{
		{"active incident", operatorstatus.Action{Code: "active_incident", Scope: "profile", ID: "profile-1"}, "profile incident incident-1: authentication_required"},
		{"delivery", operatorstatus.Action{Code: "permanent_failure", Scope: "delivery", ID: "delivery-1"}, "profile profile-1 destination destination-1 delivery failed permanently"},
		{"operational delivery", operatorstatus.Action{Code: "permanent_failure", Scope: "operational_delivery", ID: "operational-1"}, "incident incident-1 destination destination-1 recovery delivery failed permanently"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := historyActionMessage(test.action, status); got != test.want {
				t.Fatalf("historyActionMessage() = %q, want %q", got, test.want)
			}
		})
	}
}

type historyStatusSessionStore struct {
	state *medicover.SessionState
}

func (s historyStatusSessionStore) Load(string) (*medicover.SessionState, error) {
	if s.state == nil {
		return nil, session.ErrNotFound
	}
	return s.state, nil
}

func (historyStatusSessionStore) Save(string, *medicover.SessionState) error { return nil }

func (historyStatusSessionStore) Delete(string) error { return nil }

func TestHistoryStatusFlagsExpiredSession(t *testing.T) {
	database, _ := setupHistoryFixture(t)
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	backend := historyStatusSessionStore{state: &medicover.SessionState{
		DeviceID: "device",
		Cookies:  []medicover.StoredCookie{{Name: "MedicoverTrusted", Value: "expired", Expires: "2000-01-01T00:00:00Z"}},
	}}
	data, err := collectHistoryStatus(storage, backend)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Accounts) != 1 || data.Accounts[0].Authenticated || !data.Accounts[0].AuthRequired {
		t.Fatalf("account status = %#v, want authentication required", data.Accounts)
	}
	for _, action := range data.RequiredActions {
		if action.Code == "authentication_required" && action.ID == "alice" {
			return
		}
	}
	t.Fatalf("required actions = %#v, want authentication_required", data.RequiredActions)
}

func TestHistoryStatusReturnsRetentionReadError(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	databaseHandle, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := databaseHandle.Exec("ALTER TABLE application_metadata RENAME COLUMN value TO broken_value"); err != nil {
		databaseHandle.Close()
		t.Fatal(err)
	}
	if err := databaseHandle.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, getenv, "history", "status", "--database", database, "--output", "json")
	if code == 0 {
		t.Fatalf("history status succeeded for retention read failure: stdout=%q stderr=%q", stdout, stderr)
	}
	if stdout != "" || !strings.Contains(stderr, "database_error") || strings.Contains(stderr, "invalid_retention") {
		t.Fatalf("history status error: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestHistoryRetentionShowSetAndPrune(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	code, stdout, _ := runCLI(t, getenv, "history", "retention", "--database", database, "--output", "json")
	if code != 0 {
		t.Fatalf("retention show: %d %q", code, stdout)
	}
	data := decodeEnvelope(t, stdout, "history retention")
	if days, _ := data["retention_days"].(float64); days != 90 {
		t.Fatalf("default retention = %v, want 90", data)
	}
	if code, _, stderr := runCLI(t, getenv, "history", "retention", "--database", database, "--retention-days", "30"); code != 0 {
		t.Fatalf("retention set: %d %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, getenv, "history", "retention", "--database", database, "--retention-days", "0"); code != 2 || !strings.Contains(stderr, "retention") {
		t.Fatalf("retention invalid: %d %q", code, stderr)
	}
	code, stdout, stderr := runCLI(t, getenv, "history", "prune", "--database", database, "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("prune: %d %q", code, stderr)
	}
	decodeEnvelope(t, stdout, "history prune")
}

func TestHistoryRejectsIrrelevantFlags(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	for _, args := range [][]string{
		{"history", "runs", "--database", database, "--region", "204"},
		{"history", "episodes", "--database", database, "--account", "alice"},
		{"history", "incidents", "--database", database, "--profile", "prof"},
		{"history", "deliveries", "--database", database, "--scope", "account"},
		{"history", "status", "--database", database, "--limit", "10"},
		{"history", "retention", "--database", database, "--limit", "10"},
		{"history", "prune", "--database", database, "--retention-days", "30"},
		{"history", "runs", "--database", database, "--telegram", "dest"},
		{"doctor", "--database", database, "--limit", "10"},
		{"completion", "--limit", "10"},
	} {
		if code, _, stderr := runCLI(t, getenv, args...); code != 2 || !strings.Contains(stderr, "not supported") {
			t.Fatalf("args %v: code=%d stderr=%q", args, code, stderr)
		}
	}
}

func TestCompletionScriptsAndJSON(t *testing.T) {
	getenv := historyTestEnv(t.TempDir())
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		code, stdout, stderr := runCLI(t, getenv, "completion", shell)
		if code != 0 || stderr != "" || !strings.Contains(stdout, "medalert") {
			t.Fatalf("completion %s: code=%d stderr=%q", shell, code, stderr)
		}
	}
	code, stdout, _ := runCLI(t, getenv, "completion", "bash", "--output", "json")
	if code != 0 {
		t.Fatalf("completion json: %d", code)
	}
	data := decodeEnvelope(t, stdout, "completion")
	if data["shell"] != "bash" {
		t.Fatalf("shell = %v", data)
	}
	if code, _, stderr := runCLI(t, getenv, "completion", "tcsh"); code != 2 || !strings.Contains(stderr, "shell must be") {
		t.Fatalf("completion invalid shell: %d %q", code, stderr)
	}
}

func TestDoctorReportsDiagnosticsInTextAndJSON(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	code, stdout, stderr := runCLI(t, getenv, "doctor", "--database", database, "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor json: %d %q", code, stderr)
	}
	data := decodeEnvelope(t, stdout, "doctor")
	if _, ok := data["retention_days"]; !ok {
		t.Fatalf("doctor data missing retention_days: %v", data)
	}
	if _, ok := data["required_actions"]; !ok {
		t.Fatalf("doctor data missing required_actions: %v", data)
	}
	code, stdout, _ = runCLI(t, getenv, "doctor", "--database", database)
	if code != 0 || !strings.Contains(stdout, "Database schema") {
		t.Fatalf("doctor text: %d %q", code, stdout)
	}
}

func TestHistoryNeverLeaksSecrets(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := privateDB(t, root, "medalert.db")
	getenv := historyTestEnv(root)
	sessionDir := filepath.Join(root, "sessions")
	getenvWithSession := func(key string) string {
		if key == "MEDALERT_SESSION_DIR" {
			return sessionDir
		}
		return getenv(key)
	}
	marker := "MARKER-HISTORY-SECRET-" + t.Name()
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(secretDir, "pw")
	tokenFile := filepath.Join(secretDir, "tok")
	if err := os.WriteFile(passwordFile, []byte(marker+"-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(marker+"-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputs := []string{}
	collect := func(code int, stdout, stderr string) {
		outputs = append(outputs, stdout, stderr)
		if strings.Contains(stdout, marker) || strings.Contains(stderr, marker) {
			t.Fatalf("output leaks secret: stdout=%q stderr=%q", stdout, stderr)
		}
		_ = code
	}
	code, stdout, stderr := runCLI(t, getenvWithSession, "account", "create", "--database", database, "--non-interactive", "--account", "alice", "--username", "alice@example.com", "--password-file", passwordFile)
	collect(code, stdout, stderr)
	code, stdout, stderr = runCLI(t, getenvWithSession, "telegram", "create", "--database", database, "--non-interactive", "--telegram", "dest", "--name", "T", "--chat-id", "123", "--token-file", tokenFile)
	collect(code, stdout, stderr)
	code, stdout, stderr = runCLI(t, getenvWithSession, "profile", "create", "--database", database, "--non-interactive", "--profile", "prof", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "dest")
	collect(code, stdout, stderr)
	for _, args := range [][]string{
		{"history", "runs", "--database", database, "--output", "json"},
		{"history", "episodes", "--database", database, "--output", "json"},
		{"history", "incidents", "--database", database, "--output", "json"},
		{"history", "deliveries", "--database", database, "--output", "json"},
		{"history", "status", "--database", database, "--output", "json"},
		{"history", "runs", "--database", database},
		{"history", "status", "--database", database},
		{"doctor", "--database", database, "--output", "json"},
		{"doctor", "--database", database},
	} {
		code, stdout, stderr := runCLI(t, getenvWithSession, args...)
		collect(code, stdout, stderr)
	}
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatal("database file contains secret marker")
	}
}

func TestStatusKeepsCancelledDeliveriesInHistoryWithoutRequiredActions(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	now := time.Now().UTC().Add(-time.Minute)
	run, err := storage.BeginObservationRun("prof", now)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{{
		Identity: "slot", StableIdentity: "slot", Time: "2099-01-10T10:00:00Z",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := storage.EnsureEpisodeDeliveries("prof", observed.NewEpisodes[0].ID, now)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %v, err = %v", deliveries, err)
	}
	if _, err := storage.CancelDeliveriesForEndedEpisodes("prof", []string{observed.NewEpisodes[0].ID}, now); err != nil {
		t.Fatal(err)
	}
	incident, pending, _, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, "prof", "alice", "prof", "", "authentication_required", "login required", now, []string{"dest"})
	if err != nil || len(pending) != 1 {
		t.Fatalf("incident = %v, pending = %v, err = %v", incident, pending, err)
	}
	if _, _, _, err := storage.ResolveIncident(store.IncidentScopeProfile, "prof", "prof", "", now); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"history", "status"}, {"doctor"}} {
		code, stdout, stderr := runCLI(t, getenv, append(command, "--database", database, "--output", "json")...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", command, code, stderr)
		}
		data := decodeEnvelope(t, stdout, strings.Join(command, " "))
		for _, field := range []string{"permanent_failures", "operational_deliveries"} {
			if command[0] == "doctor" {
				continue
			}
			if rows, ok := data[field].([]any); !ok || len(rows) != 1 {
				t.Fatalf("%s = %#v, want one historical cancellation", field, data[field])
			}
		}
		for _, raw := range data["required_actions"].([]any) {
			action := raw.(map[string]any)
			if action["code"] == "permanent_failure" || action["code"] == "destination_delivery_failure" {
				t.Errorf("%v reports cancelled delivery as required action: %v", command, action)
			}
		}
	}
	snapshot, err := application.New(application.Config{Database: database, SessionDir: getenv("MEDALERT_SESSION_DIR")}).Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := 0
	for _, row := range snapshot.History {
		if (row.ID == deliveries[0].ID || row.ID == pending[0].ID) && strings.Contains(row.Label, "anulowane") {
			cancelled++
		}
	}
	if cancelled != 2 {
		t.Fatalf("TUI history = %+v, want two cancelled deliveries", snapshot.History)
	}
	for _, action := range snapshot.Actions {
		if action.ID == deliveries[0].ID || action.ID == pending[0].ID || action.ID == "dest" {
			t.Fatalf("TUI cancellation action = %+v", action)
		}
	}

}

func TestStatusSharesAccountPauseBetweenCLIAndTUI(t *testing.T) {
	database, getenv := setupHistoryFixture(t)
	backend := session.FileStore{Dir: getenv("MEDALERT_SESSION_DIR")}
	if err := backend.Save("alice", &medicover.SessionState{DeviceID: "device", Cookies: []medicover.StoredCookie{{Name: "MedicoverTrusted", Value: "test-cookie", Domain: "login.example.test", Path: "/", Secure: true, Expires: "2099-01-10T10:00:00Z"}}}); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, _, _, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, "alice", "alice", "", "", "authentication_required", "login required", time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, getenv, "history", "status", "--database", database, "--medicover-base-url", "https://login.example.test", "--output", "json")
	if code != 0 || stderr != "" {
		t.Fatalf("status: code=%d stderr=%q", code, stderr)
	}
	data := decodeEnvelope(t, stdout, "history status")
	accounts, ok := data["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %#v", data["accounts"])
	}
	account := accounts[0].(map[string]any)
	if account["authenticated"] != false || account["auth_required"] != true {
		t.Fatalf("account = %#v", account)
	}
	snapshot, err := application.New(application.Config{Database: database, SessionDir: backend.Dir, MedicoverBaseURL: "https://login.example.test"}).Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Detail != "! Wymaga logowania" {
		t.Fatalf("TUI accounts = %+v", snapshot.Accounts)
	}
	logins := 0
	for _, action := range snapshot.Actions {
		if action.ID == "alice" && action.Detail == "Zaloguj konto" && action.Target == 1 {
			logins++
		}
	}
	if logins != 1 {
		t.Fatalf("TUI actions = %+v, want one login action", snapshot.Actions)
	}
}
