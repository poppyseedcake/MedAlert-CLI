package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/zalando/go-keyring"
)

func profileGetenv(root string) func(string) string {
	return func(key string) string {
		switch key {
		case "XDG_DATA_HOME":
			return root
		case "MEDALERT_OUTPUT", "MEDALERT_DATABASE", "MEDALERT_NON_INTERACTIVE", "HOME":
			return ""
		default:
			return ""
		}
	}
}

func createTestAccount(t *testing.T, getenv func(string) string, database, id string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO([]string{"account", "create", "--database", database, "--non-interactive", "--account", id, "--username", id + "@example.com", "--no-stored-password"}, os.Stdin, &stdout, &stderr, getenv)
	if code != 0 {
		t.Fatalf("create account %s: code=%d stderr=%q", id, code, stderr.String())
	}
}

func runProfileCLI(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO(args, os.Stdin, &stdout, &stderr, getenv)
	return code, stdout.String(), stderr.String()
}

func TestProfileCreateListShowEditEnableDisableDelete(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := profileGetenv(root)
	createTestAccount(t, getenv, database, "alice")

	// Create with full criteria, JSON envelope identifies account + profile.
	code, stdout, stderr := runProfileCLI(t, getenv, "profile", "create", "--database", database,
		"--profile", "myprof", "--account", "alice",
		"--region", "204", "--specialty", "132", "--clinic", "10,11",
		"--doctor", "20", "--language", "4", "--visit-type", "Center",
		"--search-type", "Standard", "--start-date", "2026-09-01", "--end-date", "2026-09-30",
		"--check-interval-minutes", "30", "--output", "json")
	if code != 0 {
		t.Fatalf("create: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var created struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Profile store.Profile `json:"profile"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.SchemaVersion != 1 || created.Command != "profile create" {
		t.Fatalf("envelope = %#v", created)
	}
	if created.Data.Profile.ID != "myprof" || created.Data.Profile.AccountID != "alice" {
		t.Fatalf("profile = %#v", created.Data.Profile)
	}
	createdAt := created.Data.Profile.CreatedAt

	// List in text identifies account + profile.
	code, stdout, _ = runProfileCLI(t, getenv, "profile", "list", "--database", database)
	if code != 0 || !strings.Contains(stdout, "myprof") || !strings.Contains(stdout, "alice") {
		t.Fatalf("list = %d %q", code, stdout)
	}

	// Show via positional id.
	code, stdout, _ = runProfileCLI(t, getenv, "profile", "show", "--database", database, "myprof")
	if code != 0 || !strings.Contains(stdout, "ID: myprof") || !strings.Contains(stdout, "Account: alice") {
		t.Fatalf("show = %d %q", code, stdout)
	}

	// Edit keeps the stable identity and created_at.
	code, stdout, stderr = runProfileCLI(t, getenv, "profile", "edit", "--database", database,
		"--profile", "myprof", "--clinic", "12", "--check-interval-minutes", "15")
	if code != 0 {
		t.Fatalf("edit: code=%d stderr=%q", code, stderr)
	}
	code, stdout, _ = runProfileCLI(t, getenv, "profile", "show", "--database", database, "--profile", "myprof", "--output", "json")
	var shown struct {
		Data struct {
			Profile store.Profile `json:"profile"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Data.Profile.ID != "myprof" || shown.Data.Profile.CreatedAt != createdAt {
		t.Fatalf("identity changed: %#v", shown.Data.Profile)
	}
	if shown.Data.Profile.ClinicIDs != "12" || shown.Data.Profile.CheckIntervalMinutes != 15 {
		t.Fatalf("edit not applied: %#v", shown.Data.Profile)
	}

	// Disable pauses without deleting: the row stays readable as disabled.
	code, _, _ = runProfileCLI(t, getenv, "profile", "disable", "--database", database, "myprof")
	if code != 0 {
		t.Fatalf("disable = %d", code)
	}
	code, stdout, _ = runProfileCLI(t, getenv, "profile", "show", "--database", database, "myprof")
	if code != 0 || !strings.Contains(stdout, "disabled") {
		t.Fatalf("show after disable = %d %q", code, stdout)
	}
	code, _, _ = runProfileCLI(t, getenv, "profile", "enable", "--database", database, "myprof")
	if code != 0 {
		t.Fatalf("enable = %d", code)
	}

	// Delete removes the configuration.
	code, stdout, _ = runProfileCLI(t, getenv, "profile", "delete", "--database", database, "myprof", "--output", "json")
	if code != 0 || !strings.Contains(stdout, `"deleted":"myprof"`) || !strings.Contains(stdout, `"account":"alice"`) {
		t.Fatalf("delete = %d %q", code, stdout)
	}
	code, _, _ = runProfileCLI(t, getenv, "profile", "show", "--database", database, "myprof")
	if code == 0 {
		t.Fatal("show succeeded after delete")
	}
}

func TestProfileCreateRejectsMissingOrInvalidCriteria(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := profileGetenv(root)
	createTestAccount(t, getenv, database, "alice")

	for _, args := range [][]string{
		// Missing profile id.
		{"profile", "create", "--database", database, "--non-interactive", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30"},
		// Missing account.
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30"},
		// Missing region / specialty / interval.
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--specialty", "132", "--check-interval-minutes", "30"},
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--check-interval-minutes", "30"},
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--specialty", "132"},
		// Invalid values.
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "bad", "--specialty", "132", "--check-interval-minutes", "30"},
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "0"},
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--start-date", "not-a-date"},
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--start-date", "2026-09-10", "--end-date", "2026-09-01"},
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--search-type", "Nope"},
		// Unknown account.
		{"profile", "create", "--database", database, "--non-interactive", "--profile", "p1", "--account", "nobody", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30"},
		// Duplicate.
	} {
		code, _, stderr := runProfileCLI(t, getenv, args...)
		if code != 2 || stderr == "" {
			t.Fatalf("args %v: code=%d stderr=%q, want rejection without prompting", args, code, stderr)
		}
	}

	// Duplicate reports profile_exists.
	if code, _, _ := runProfileCLI(t, getenv, "profile", "create", "--database", database, "--non-interactive", "--profile", "dup", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30"); code != 0 {
		t.Fatalf("first create = %d", code)
	}
	code, _, stderr := runProfileCLI(t, getenv, "profile", "create", "--database", database, "--non-interactive", "--profile", "dup", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--output", "json")
	if code != 2 || !strings.Contains(stderr, "profile_exists") {
		t.Fatalf("duplicate = %d %q", code, stderr)
	}

	// Edit with no changes is rejected.
	code, _, stderr = runProfileCLI(t, getenv, "profile", "edit", "--database", database, "--non-interactive", "--profile", "dup")
	if code != 2 {
		t.Fatalf("empty edit = %d", code)
	}
}

func TestProfileRejectsIrrelevantFlags(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := profileGetenv(root)
	for _, args := range [][]string{
		{"profile", "show", "--database", database, "--profile", "p", "--region", "204"},
		{"profile", "delete", "--database", database, "--profile", "p", "--account", "alice"},
		{"profile", "enable", "--database", database, "--profile", "p", "--clinic", "1"},
		{"profile", "list", "--database", database, "--region", "204"},
		{"profile", "list", "--database", database, "--profile", "p"},
		{"profile", "create", "--database", database, "--profile", "p", "--account", "a", "--region", "1", "--specialty", "1", "--check-interval-minutes", "5", "--username", "u"},
		{"account", "list", "--database", database, "--region", "204"},
		{"account", "show", "--database", database, "--account", "a", "--profile", "p"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunWithIO(args, os.Stdin, &stdout, &stderr, getenv); code != 2 || !strings.Contains(stderr.String(), "not supported") {
			t.Fatalf("args %v: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestProfilePositionalAndFlagConflict(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := profileGetenv(root)
	database := filepath.Join(root, "medalert.db")
	code, _, stderr := runProfileCLI(t, getenv, "profile", "show", "--database", database, "--profile", "a", "b")
	if code != 2 || !strings.Contains(stderr, "not both") {
		t.Fatalf("conflict = %d %q", code, stderr)
	}
}
