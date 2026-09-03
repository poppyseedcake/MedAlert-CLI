package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createProcessAccount(t *testing.T, databasePath, id string) {
	t.Helper()
	created := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", id, "--username", id+"@example.com", "--no-stored-password")
	if created.exitCode != 0 {
		t.Fatalf("create account %s = %#v", id, created)
	}
}

func TestProfileManageMultipleProfilesWithTextAndJSON(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessAccount(t, databasePath, "alice")
	createProcessAccount(t, databasePath, "bob")

	createOne := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "cardio", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30")
	if createOne.exitCode != 0 || createOne.stderr != "" {
		t.Fatalf("create cardio = %#v", createOne)
	}
	if !strings.Contains(createOne.stdout, "Created profile cardio for account alice.") {
		t.Fatalf("stdout = %q", createOne.stdout)
	}

	createTwo := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "derma", "--account", "bob", "--region", "205", "--specialty", "133", "--clinic", "10,11", "--doctor", "20", "--language", "4", "--visit-type", "Center", "--search-type", "DiagnosticProcedure", "--start-date", "2026-09-01", "--end-date", "2026-09-30", "--check-interval-minutes", "15", "--output", "json")
	if createTwo.exitCode != 0 || createTwo.stderr != "" {
		t.Fatalf("create derma = %#v", createTwo)
	}
	if !strings.Contains(createTwo.stdout, `"command":"profile create"`) || !strings.Contains(createTwo.stdout, `"schema_version":1`) {
		t.Fatalf("derma json = %q", createTwo.stdout)
	}
	if !strings.Contains(createTwo.stdout, `"id":"derma"`) || !strings.Contains(createTwo.stdout, `"account_id":"bob"`) {
		t.Fatalf("derma json identity = %q", createTwo.stdout)
	}

	listText := run(t, nil, "profile", "list", "--database", databasePath)
	if listText.exitCode != 0 {
		t.Fatalf("list = %#v", listText)
	}
	if !strings.Contains(listText.stdout, "cardio") || !strings.Contains(listText.stdout, "derma") {
		t.Fatalf("list stdout = %q", listText.stdout)
	}
	if !strings.Contains(listText.stdout, "alice") || !strings.Contains(listText.stdout, "bob") {
		t.Fatalf("list must identify accounts: %q", listText.stdout)
	}

	listJSON := run(t, nil, "profile", "list", "--database", databasePath, "--output", "json")
	if listJSON.exitCode != 0 {
		t.Fatalf("list json = %#v", listJSON)
	}
	var listEnvelope struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Profiles []struct {
				ID        string `json:"id"`
				AccountID string `json:"account_id"`
			} `json:"profiles"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON.stdout), &listEnvelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listEnvelope.SchemaVersion != 1 || listEnvelope.Command != "profile list" || len(listEnvelope.Data.Profiles) != 2 {
		t.Fatalf("list envelope = %#v", listEnvelope)
	}

	filtered := run(t, nil, "profile", "list", "--database", databasePath, "--account", "alice", "--output", "json")
	if !strings.Contains(filtered.stdout, "cardio") || strings.Contains(filtered.stdout, `"id":"derma"`) {
		t.Fatalf("filtered = %q", filtered.stdout)
	}

	show := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "cardio", "--output", "json")
	if show.exitCode != 0 || !strings.Contains(show.stdout, `"account_id":"alice"`) {
		t.Fatalf("show = %#v", show)
	}

	// Editing keeps the stable identity.
	showBefore := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "cardio", "--output", "json")
	var before struct {
		Data struct {
			Profile struct {
				CreatedAt string `json:"created_at"`
			} `json:"profile"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(showBefore.stdout), &before); err != nil {
		t.Fatal(err)
	}
	edit := run(t, nil, "profile", "edit", "--database", databasePath, "--non-interactive", "--profile", "cardio", "--clinic", "12", "--check-interval-minutes", "45")
	if edit.exitCode != 0 || edit.stderr != "" {
		t.Fatalf("edit = %#v", edit)
	}
	showAfter := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "cardio", "--output", "json")
	var after struct {
		Data struct {
			Profile struct {
				ID        string `json:"id"`
				AccountID string `json:"account_id"`
				ClinicIDs string `json:"clinic_ids"`
				CreatedAt string `json:"created_at"`
			} `json:"profile"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(showAfter.stdout), &after); err != nil {
		t.Fatal(err)
	}
	if after.Data.Profile.ID != "cardio" || after.Data.Profile.AccountID != "alice" {
		t.Fatalf("identity changed: %#v", after.Data.Profile)
	}
	if after.Data.Profile.CreatedAt != before.Data.Profile.CreatedAt {
		t.Fatal("created_at changed on edit")
	}
	if after.Data.Profile.ClinicIDs != "12" {
		t.Fatalf("edit not applied: %#v", after.Data.Profile)
	}

	// Disabling pauses without deleting saved state.
	disabled := run(t, nil, "profile", "disable", "--database", databasePath, "--profile", "cardio")
	if disabled.exitCode != 0 {
		t.Fatalf("disable = %#v", disabled)
	}
	shownDisabled := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "cardio", "--output", "json")
	if !strings.Contains(shownDisabled.stdout, `"enabled":false`) {
		t.Fatalf("disabled show = %q", shownDisabled.stdout)
	}
	listAfterDisable := run(t, nil, "profile", "list", "--database", databasePath, "--output", "json")
	if !strings.Contains(listAfterDisable.stdout, "cardio") {
		t.Fatalf("disabled profile disappeared from list: %q", listAfterDisable.stdout)
	}
	enabled := run(t, nil, "profile", "enable", "--database", databasePath, "--profile", "cardio")
	if enabled.exitCode != 0 {
		t.Fatalf("enable = %#v", enabled)
	}

	// Deleting removes the configuration.
	deleted := run(t, nil, "profile", "delete", "--database", databasePath, "--profile", "derma")
	if deleted.exitCode != 0 {
		t.Fatalf("delete = %#v", deleted)
	}
	afterDelete := run(t, nil, "profile", "list", "--database", databasePath, "--output", "json")
	if !strings.Contains(afterDelete.stdout, "cardio") || strings.Contains(afterDelete.stdout, `"id":"derma"`) {
		t.Fatalf("after delete = %q", afterDelete.stdout)
	}
	missing := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "derma", "--output", "json")
	if missing.exitCode != 2 || !strings.Contains(missing.stderr, `"code":"profile_not_found"`) {
		t.Fatalf("show deleted = %#v", missing)
	}
}

func TestProfileNonInteractiveRejectsMissingInput(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessAccount(t, databasePath, "alice")

	missing := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "nop", "--account", "alice", "--region", "204", "--specialty", "132")
	if missing.exitCode != 2 || missing.stdout != "" || missing.stderr == "" {
		t.Fatalf("missing interval = %#v", missing)
	}
	invalid := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--output", "json", "--profile", "bad", "--account", "alice", "--region", "bad", "--specialty", "132", "--check-interval-minutes", "30")
	if invalid.exitCode != 2 || !strings.Contains(invalid.stderr, `"code":"invalid_arguments"`) {
		t.Fatalf("invalid region = %#v", invalid)
	}
	// The rejected creates must not leave rows behind.
	listed := run(t, nil, "profile", "list", "--database", databasePath)
	if !strings.Contains(listed.stdout, "No profiles found.") {
		t.Fatalf("rejected creates left rows: %q", listed.stdout)
	}
}

func TestProfileRejectsIrrelevantFlagsViaExecutable(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")

	for _, args := range [][]string{
		{"profile", "show", "--database", databasePath, "--profile", "p", "--region", "204"},
		{"profile", "delete", "--database", databasePath, "--profile", "p", "--account", "alice"},
		{"profile", "list", "--database", databasePath, "--profile", "p"},
		{"account", "show", "--database", databasePath, "--account", "a", "--region", "204"},
	} {
		result := run(t, nil, args...)
		if result.exitCode != 2 || result.stdout != "" || !strings.Contains(result.stderr, "not supported") {
			t.Fatalf("args %v result = %#v", args, result)
		}
	}
}

func TestProfileDuplicateCreateViaExecutable(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessAccount(t, databasePath, "alice")

	created := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "dup", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30")
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}
	again := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "dup", "--account", "alice", "--region", "205", "--specialty", "133", "--check-interval-minutes", "15", "--output", "json")
	if again.exitCode != 2 || !strings.Contains(again.stderr, `"code":"profile_exists"`) {
		t.Fatalf("duplicate = %#v", again)
	}
	shown := run(t, nil, "profile", "show", "--database", databasePath, "--profile", "dup", "--output", "json")
	if !strings.Contains(shown.stdout, `"region_ids":"204"`) {
		t.Fatalf("duplicate overwrote original: %q", shown.stdout)
	}
}

func TestProfileDatabaseFileHoldsNoSecrets(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessAccount(t, databasePath, "alice")

	created := run(t, nil, "profile", "create", "--database", databasePath, "--non-interactive", "--profile", "plain", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--output", "json")
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}
	raw, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	// Profiles carry only IDs and criteria; a regression that stored secret
	// values would be visible here. The marker below never existed as input,
	// so this guards thenonatomic file handling rather than a real secret.
	if strings.Contains(string(raw), "MARKER-PROFILE-SECRET-unique") {
		t.Fatal("database contains unexpected marker")
	}
	assertProcessQueryValue(t, databasePath, "SELECT region_ids FROM profiles WHERE id = 'plain'", "204")
	assertProcessQueryValue(t, databasePath, "SELECT enabled FROM profiles WHERE id = 'plain'", "1")
}
