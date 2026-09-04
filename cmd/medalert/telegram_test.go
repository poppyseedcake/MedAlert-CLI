package main_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createTelegramDestination(t *testing.T, databasePath, id, name, chatID, tokenFile string, extra ...string) {
	t.Helper()
	args := []string{"telegram", "create", "--database", databasePath, "--non-interactive", "--telegram", id, "--name", name, "--chat-id", chatID, "--token-file", tokenFile}
	args = append(args, extra...)
	created := run(t, nil, args...)
	if created.exitCode != 0 {
		t.Fatalf("create telegram %s = %#v", id, created)
	}
}

func TestTelegramManageDestinationsWithTextAndJSON(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	markerOne := "MARKER-TELEGRAM-ONE-" + t.Name() + "-unique"
	markerTwo := "MARKER-TELEGRAM-TWO-unique"
	fileOne := writeProcessSecretFile(t, secretDir, "one", markerOne)
	fileTwo := writeProcessSecretFile(t, secretDir, "two", markerTwo)

	createOne := run(t, nil, "telegram", "create", "--database", databasePath, "--non-interactive", "--telegram", "phone", "--name", "Telefon", "--chat-id", "123456", "--token-file", fileOne)
	if createOne.exitCode != 0 || createOne.stderr != "" {
		t.Fatalf("create phone = %#v", createOne)
	}
	if !strings.Contains(createOne.stdout, "Created telegram destination phone.") {
		t.Fatalf("stdout = %q", createOne.stdout)
	}

	createTwo := run(t, nil, "telegram", "create", "--database", databasePath, "--non-interactive", "--telegram", "backup", "--name", "Zapas", "--chat-id", "-100999", "--token-file", fileTwo, "--output", "json")
	if createTwo.exitCode != 0 || createTwo.stderr != "" {
		t.Fatalf("create backup = %#v", createTwo)
	}
	if !strings.Contains(createTwo.stdout, `"command":"telegram create"`) || !strings.Contains(createTwo.stdout, `"schema_version":1`) {
		t.Fatalf("backup json = %q", createTwo.stdout)
	}
	if !strings.Contains(createTwo.stdout, `"id":"backup"`) || !strings.Contains(createTwo.stdout, `"chat_id":"-100999"`) {
		t.Fatalf("backup identity = %q", createTwo.stdout)
	}

	listText := run(t, nil, "telegram", "list", "--database", databasePath)
	if listText.exitCode != 0 {
		t.Fatalf("list = %#v", listText)
	}
	if !strings.Contains(listText.stdout, "phone") || !strings.Contains(listText.stdout, "backup") {
		t.Fatalf("list stdout = %q", listText.stdout)
	}

	listJSON := run(t, nil, "telegram", "list", "--database", databasePath, "--output", "json")
	if listJSON.exitCode != 0 {
		t.Fatalf("list json = %#v", listJSON)
	}
	var listEnvelope struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Destinations []struct {
				ID       string `json:"id"`
				ChatID   string `json:"chat_id"`
				TokRef   string `json:"token_ref"`
				TokSrc   string `json:"token_source"`
			} `json:"destinations"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON.stdout), &listEnvelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listEnvelope.SchemaVersion != 1 || listEnvelope.Command != "telegram list" || len(listEnvelope.Data.Destinations) != 2 {
		t.Fatalf("list envelope = %#v", listEnvelope)
	}

	show := run(t, nil, "telegram", "show", "--database", databasePath, "--telegram", "phone", "--output", "json")
	if show.exitCode != 0 || !strings.Contains(show.stdout, `"id":"phone"`) {
		t.Fatalf("show = %#v", show)
	}

	edit := run(t, nil, "telegram", "edit", "--database", databasePath, "--non-interactive", "--telegram", "phone", "--name", "Telefon Nowy")
	if edit.exitCode != 0 || edit.stderr != "" {
		t.Fatalf("edit = %#v", edit)
	}
	shownAfter := run(t, nil, "telegram", "show", "--database", databasePath, "--telegram", "phone", "--output", "json")
	if !strings.Contains(shownAfter.stdout, "Telefon Nowy") {
		t.Fatalf("edit not applied: %q", shownAfter.stdout)
	}

	disabled := run(t, nil, "telegram", "disable", "--database", databasePath, "--telegram", "phone")
	if disabled.exitCode != 0 {
		t.Fatalf("disable = %#v", disabled)
	}
	shownDisabled := run(t, nil, "telegram", "show", "--database", databasePath, "--telegram", "phone", "--output", "json")
	if !strings.Contains(shownDisabled.stdout, `"enabled":false`) {
		t.Fatalf("disabled show = %q", shownDisabled.stdout)
	}
	enabled := run(t, nil, "telegram", "enable", "--database", databasePath, "--telegram", "phone")
	if enabled.exitCode != 0 {
		t.Fatalf("enable = %#v", enabled)
	}

	deleted := run(t, nil, "telegram", "delete", "--database", databasePath, "--telegram", "backup")
	if deleted.exitCode != 0 {
		t.Fatalf("delete = %#v", deleted)
	}
	afterDelete := run(t, nil, "telegram", "list", "--database", databasePath, "--output", "json")
	if !strings.Contains(afterDelete.stdout, `"id":"phone"`) || strings.Contains(afterDelete.stdout, `"id":"backup"`) {
		t.Fatalf("after delete = %q", afterDelete.stdout)
	}
	missing := run(t, nil, "telegram", "show", "--database", databasePath, "--telegram", "backup", "--output", "json")
	if missing.exitCode != 2 || !strings.Contains(missing.stderr, `"code":"destination_not_found"`) {
		t.Fatalf("show deleted = %#v", missing)
	}

	for _, output := range []string{createOne.stdout, createOne.stderr, createTwo.stdout, createTwo.stderr, listText.stdout, listText.stderr, listJSON.stdout, show.stdout, edit.stdout, afterDelete.stdout} {
		if strings.Contains(output, markerOne) || strings.Contains(output, markerTwo) {
			t.Fatalf("output leaks secret: %q", output)
		}
	}
	raw, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), markerOne) || strings.Contains(string(raw), markerTwo) {
		t.Fatal("database file contains secret value")
	}
	assertProcessQueryValue(t, databasePath, "SELECT token_source FROM telegram_destinations WHERE id = 'phone'", "file")
	assertProcessQueryValue(t, databasePath, "SELECT token_ref FROM telegram_destinations WHERE id = 'phone'", fileOne)
}

func TestProfileLinksToMultipleTelegramDestinations(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fileOne := writeProcessSecretFile(t, secretDir, "one", "token-one")
	fileTwo := writeProcessSecretFile(t, secretDir, "two", "token-two")
	createTelegramDestination(t, databasePath, "one", "Jeden", "111", fileOne)
	createTelegramDestination(t, databasePath, "two", "Dwa", "222", fileTwo)
	createProcessAccount(t, databasePath, "alice")

	created := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "linked", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "one,two", "--output", "json")
	if created.exitCode != 0 {
		t.Fatalf("create profile with telegram = %#v", created)
	}
	if !strings.Contains(created.stdout, `"telegram_destinations"`) || !strings.Contains(created.stdout, `"one"`) || !strings.Contains(created.stdout, `"two"`) {
		t.Fatalf("create output missing links: %q", created.stdout)
	}
	shown := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "linked", "--output", "json")
	if !strings.Contains(shown.stdout, `"one"`) || !strings.Contains(shown.stdout, `"two"`) {
		t.Fatalf("show missing links: %q", shown.stdout)
	}
	shownText := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "linked")
	if !strings.Contains(shownText.stdout, "Telegram:") || !strings.Contains(shownText.stdout, "one") {
		t.Fatalf("text show missing telegram: %q", shownText.stdout)
	}

	// Editing to a single destination keeps the stable identity.
	edited := run(t, nil, "profile", "edit", "--database", databasePath, "--non-interactive", "--profile", "linked", "--telegram", "one")
	if edited.exitCode != 0 {
		t.Fatalf("edit telegram = %#v", edited)
	}
	reshown := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "linked", "--output", "json")
	if !strings.Contains(reshown.stdout, `"one"`) || strings.Contains(reshown.stdout, `"two"`) {
		t.Fatalf("edit links = %q", reshown.stdout)
	}

	// Clearing unlinks all.
	cleared := run(t, nil, "profile", "edit", "--database", databasePath, "--non-interactive", "--profile", "linked", "--clear-telegram")
	if cleared.exitCode != 0 {
		t.Fatalf("clear telegram = %#v", cleared)
	}
	afterClear := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "linked", "--output", "json")
	if strings.Contains(afterClear.stdout, `"one"`) {
		t.Fatalf("clear did not unlink: %q", afterClear.stdout)
	}

	// Unknown destination is rejected and the profile keeps its links.
	bad := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "badlinks", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "missing", "--output", "json")
	if bad.exitCode != 2 || !strings.Contains(bad.stderr, `"code":"destination_not_found"`) {
		t.Fatalf("bad links = %#v", bad)
	}
}

func TestTelegramTestSendsPolishMessageAndRecordsResult(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenMarker := "MARKER-TELEGRAM-TEST-TOKEN-unique-98765"
	tokenFile := writeProcessSecretFile(t, secretDir, "token", tokenMarker)
	createTelegramDestination(t, databasePath, "phone", "Telefon", "123456", tokenFile)

	var gotPath, gotText, gotChat string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			if text, ok := payload["text"].(string); ok {
				gotText = text
			}
			if chat, ok := payload["chat_id"].(string); ok {
				gotChat = chat
			} else if payload["chat_id"] != nil {
				if encoded, err := json.Marshal(payload["chat_id"]); err == nil {
					gotChat = strings.Trim(string(encoded), `"`)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer server.Close()
	env := []string{"MEDALERT_TELEGRAM_BASE_URL=" + server.URL}

	tested := run(t, env, "telegram", "test", "--database", databasePath, "--telegram", "phone", "--output", "json")
	if tested.exitCode != 0 {
		t.Fatalf("test = %#v", tested)
	}
	if !strings.Contains(tested.stdout, `"delivered":true`) || !strings.Contains(tested.stdout, `"message_id":77`) {
		t.Fatalf("test stdout = %q", tested.stdout)
	}
	// The safe Polish test message must mention Telegram and the destination.
	if !strings.Contains(gotText, "Test") || !strings.Contains(gotText, "Telefon") {
		t.Fatalf("telegram text = %q, want Polish test with destination name", gotText)
	}
	if gotChat != "123456" {
		t.Fatalf("chat = %q, want 123456", gotChat)
	}
	// The request itself must carry the token in the /bot<token>/ path;
	// redaction applies to errors and diagnostics, which the unit tests and
	// the failure acceptance test below verify never contain the marker.
	if !strings.Contains(gotPath, "/bot") || !strings.Contains(gotPath, "/sendMessage") {
		t.Fatalf("request path = %q, want Telegram sendMessage endpoint", gotPath)
	}
	assertProcessQueryValue(t, databasePath, "SELECT last_test_status FROM telegram_destinations WHERE id = 'phone'", "delivered")
	shown := run(t, nil, "telegram", "show", "--database", databasePath, "--telegram", "phone", "--output", "json")
	if !strings.Contains(shown.stdout, `"last_test_status":"delivered"`) {
		t.Fatalf("show after test = %q", shown.stdout)
	}
	if strings.Contains(tested.stdout, tokenMarker) || strings.Contains(tested.stderr, tokenMarker) || strings.Contains(shown.stdout, tokenMarker) {
		t.Fatal("output leaks token")
	}
	raw, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tokenMarker) {
		t.Fatal("database contains token")
	}
}

func TestTelegramTestClassifiesPermanentFailure(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenMarker := "MARKER-PERM-TOKEN-unique-123"
	tokenFile := writeProcessSecretFile(t, secretDir, "token", tokenMarker)
	createTelegramDestination(t, databasePath, "bad", "Zly", "999", tokenFile)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer server.Close()
	env := []string{"MEDALERT_TELEGRAM_BASE_URL=" + server.URL}

	failed := run(t, env, "telegram", "test", "--database", databasePath, "--telegram", "bad", "--output", "json")
	if failed.exitCode != 2 || !strings.Contains(failed.stderr, `"code":"permanent_failure"`) {
		t.Fatalf("permanent = %#v", failed)
	}
	if strings.Contains(failed.stdout, tokenMarker) || strings.Contains(failed.stderr, tokenMarker) {
		t.Fatal("failure leaks token")
	}
	assertProcessQueryValue(t, databasePath, "SELECT last_test_status FROM telegram_destinations WHERE id = 'bad'", "permanent_failure")
}

func TestTelegramPolishAvailabilityFormatting(t *testing.T) {
	// Availability formatting is exercised through the telegram module unit
	// tests, but the CLI must never leak tokens when showing linked profiles.
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := writeProcessSecretFile(t, secretDir, "token", "safe-token-value")
	createTelegramDestination(t, databasePath, "one", "Jeden", "111", tokenFile)
	createProcessAccount(t, databasePath, "alice")
	created := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "one")
	if created.exitCode != 0 {
		t.Fatalf("create profile = %#v", created)
	}
	shown := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "p1")
	if !strings.Contains(shown.stdout, "Telegram:") || strings.Contains(shown.stdout, "safe-token-value") {
		t.Fatalf("show = %q", shown.stdout)
	}
}

func TestTelegramNonInteractiveRejectsMissingInput(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	missing := run(t, nil, "telegram", "create", "--database", databasePath, "--non-interactive", "--telegram", "nop", "--name", "N", "--chat-id", "1")
	if missing.exitCode != 2 || missing.stdout != "" || missing.stderr == "" {
		t.Fatalf("missing token = %#v", missing)
	}
	invalid := run(t, nil, "telegram", "create", "--database", databasePath, "--non-interactive", "--output", "json", "--telegram", "bad", "--name", "N", "--chat-id", "not-a-chat", "--no-stored-token")
	if invalid.exitCode != 2 || !strings.Contains(invalid.stderr, `"code":"invalid_arguments"`) {
		t.Fatalf("invalid chat = %#v", invalid)
	}
	listed := run(t, nil, "telegram", "list", "--database", databasePath)
	if !strings.Contains(listed.stdout, "No telegram destinations found.") {
		t.Fatalf("rejected creates left rows: %q", listed.stdout)
	}
}

func TestTelegramRejectsIrrelevantFlags(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	for _, args := range [][]string{
		{"telegram", "show", "--database", databasePath, "--telegram", "p", "--region", "204"},
		{"telegram", "delete", "--database", databasePath, "--telegram", "p", "--account", "alice"},
		{"telegram", "list", "--database", databasePath, "--telegram", "p"},
		{"telegram", "test", "--database", databasePath, "--telegram", "p", "--name", "N"},
		{"profile", "show", "--database", databasePath, "--profile", "p", "--chat-id", "1"},
		{"account", "show", "--database", databasePath, "--account", "a", "--telegram", "p"},
	} {
		result := run(t, nil, args...)
		if result.exitCode != 2 || result.stdout != "" || !strings.Contains(result.stderr, "not supported") {
			t.Fatalf("args %v result = %#v", args, result)
		}
	}
}

func TestTelegramDuplicateCreateViaExecutable(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := writeProcessSecretFile(t, secretDir, "valid", "valid-token")
	createTelegramDestination(t, databasePath, "dup", "Dup", "123", valid)
	again := run(t, nil, "telegram", "create", "--database", databasePath, "--non-interactive", "--telegram", "dup", "--name", "Other", "--chat-id", "456", "--token-file", valid, "--output", "json")
	if again.exitCode != 2 || !strings.Contains(again.stderr, `"code":"destination_exists"`) {
		t.Fatalf("duplicate = %#v", again)
	}
	shown := run(t, nil, "telegram", "show", "--database", databasePath, "--telegram", "dup", "--output", "json")
	if !strings.Contains(shown.stdout, `"chat_id":"123"`) {
		t.Fatalf("duplicate overwrote original: %q", shown.stdout)
	}
}
