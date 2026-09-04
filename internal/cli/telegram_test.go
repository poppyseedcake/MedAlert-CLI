package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/zalando/go-keyring"
)

func telegramGetenv(root string) func(string) string {
	return func(key string) string {
		switch key {
		case "XDG_DATA_HOME":
			return root
		case "MEDALERT_OUTPUT", "MEDALERT_DATABASE", "MEDALERT_NON_INTERACTIVE", "HOME", "MEDALERT_TELEGRAM_BASE_URL":
			return ""
		default:
			return ""
		}
	}
}

func writeTokenFile(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTelegramCreateListShowEditEnableDisableDelete(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := telegramGetenv(root)
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := writeTokenFile(t, secretDir, "token", "bot-token-value")

	var stdout, stderr bytes.Buffer
	if code := RunWithIO([]string{"telegram", "create", "--database", database, "--non-interactive", "--telegram", "phone", "--name", "Telefon", "--chat-id", "123", "--token-file", tokenFile}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Created telegram destination phone.") {
		t.Fatalf("create stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "list", "--database", database}, os.Stdin, &stdout, &stderr, getenv); code != 0 || !strings.Contains(stdout.String(), "phone") {
		t.Fatalf("list = %d %q %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "show", "--database", database, "phone"}, os.Stdin, &stdout, &stderr, getenv); code != 0 || !strings.Contains(stdout.String(), "ID: phone") {
		t.Fatalf("show = %d %q", code, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "edit", "--database", database, "--non-interactive", "--telegram", "phone", "--name", "Nowy"}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("edit = %d %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "disable", "--database", database, "phone"}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("disable = %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "show", "--database", database, "phone"}, os.Stdin, &stdout, &stderr, getenv); code != 0 || !strings.Contains(stdout.String(), "disabled") {
		t.Fatalf("show disabled = %d %q", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "enable", "--database", database, "phone"}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("enable = %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"telegram", "delete", "--database", database, "phone", "--output", "json"}, os.Stdin, &stdout, &stderr, getenv); code != 0 || !strings.Contains(stdout.String(), `"deleted":"phone"`) {
		t.Fatalf("delete = %d %q", code, stdout.String())
	}
}

func TestTelegramCreateRejectsMissingOrInvalid(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := telegramGetenv(root)
	for _, args := range [][]string{
		{"telegram", "create", "--database", database, "--non-interactive", "--name", "N", "--chat-id", "1", "--no-stored-token"},
		{"telegram", "create", "--database", database, "--non-interactive", "--telegram", "p", "--chat-id", "1", "--no-stored-token"},
		{"telegram", "create", "--database", database, "--non-interactive", "--telegram", "p", "--name", "N", "--no-stored-token"},
		{"telegram", "create", "--database", database, "--non-interactive", "--telegram", "p", "--name", "N", "--chat-id", "not-a-chat", "--no-stored-token"},
		{"telegram", "create", "--database", database, "--non-interactive", "--telegram", "p", "--name", "N", "--chat-id", "1", "--token-file", "/missing", "--no-stored-token"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunWithIO(args, os.Stdin, &stdout, &stderr, getenv); code != 2 || stderr.String() == "" {
			t.Fatalf("args %v: code=%d stderr=%q, want rejection", args, code, stderr.String())
		}
	}
}

func TestTelegramRejectsIrrelevantFlags(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := telegramGetenv(root)
	for _, args := range [][]string{
		{"telegram", "show", "--database", database, "--telegram", "p", "--region", "204"},
		{"telegram", "list", "--database", database, "--telegram", "p"},
		{"telegram", "test", "--database", database, "--telegram", "p", "--name", "N"},
		{"profile", "show", "--database", database, "--profile", "p", "--chat-id", "1"},
		{"account", "show", "--database", database, "--account", "a", "--telegram", "p"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunWithIO(args, os.Stdin, &stdout, &stderr, getenv); code != 2 || !strings.Contains(stderr.String(), "not supported") {
			t.Fatalf("args %v: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestTelegramProfileLinkingValidation(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := filepath.Join(root, "medalert.db")
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	getenv := telegramGetenv(root)
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := writeTokenFile(t, secretDir, "token", "tok")
	var stdout, stderr bytes.Buffer
	if code := RunWithIO([]string{"telegram", "create", "--database", database, "--non-interactive", "--telegram", "one", "--name", "One", "--chat-id", "1", "--token-file", tokenFile}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("create dest: %d %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"account", "create", "--database", database, "--non-interactive", "--account", "alice", "--username", "a@example.com", "--no-stored-password"}, os.Stdin, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("create account: %d %q", code, stderr.String())
	}
	// Unknown destination is rejected before creating the profile.
	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"profile", "create", "--database", database, "--non-interactive", "--profile", "bad", "--account", "alice", "--region", "204", "--specialty", "132", "--check-interval-minutes", "30", "--telegram", "missing"}, os.Stdin, &stdout, &stderr, getenv); code != 2 {
		t.Fatalf("bad link = %d %q", code, stderr.String())
	}
	// Both --telegram and --clear-telegram is rejected.
	stdout.Reset()
	stderr.Reset()
	if code := RunWithIO([]string{"profile", "edit", "--database", database, "--non-interactive", "--profile", "bad", "--telegram", "one", "--clear-telegram"}, os.Stdin, &stdout, &stderr, getenv); code != 2 {
		t.Fatalf("conflicting link flags = %d %q", code, stderr.String())
	}
}

func TestWriteProfileWithDestinationsSurfacesReadErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(store.Account{ID: "alice", Username: "a@example.com", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		t.Fatal(err)
	}
	profile, err := storage.CreateProfile(store.Profile{ID: "p1", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, Enabled: true, SearchType: store.SearchTypeStandard})
	if err != nil {
		t.Fatal(err)
	}
	// Closing the handle forces the destination lookup to fail. The writer
	// must return the failure instead of silently reporting no links.
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := writeProfileWithDestinations(&stdout, "profile show", storage, profile, "", true); err == nil {
		t.Fatal("write succeeded with closed store, want destination read error")
	} else if stdout.String() != "" {
		t.Fatalf("write produced output despite read failure: %q", stdout.String())
	}
}
