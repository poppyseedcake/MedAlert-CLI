package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/zalando/go-keyring"
)

func testGetenv(root string) func(string) string {
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

func runCLI(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO(args, os.Stdin, &stdout, &stderr, getenv)
	return code, stdout.String(), stderr.String()
}

func privateDB(t *testing.T, root, name string) string {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, name)
}

// Invalid input must be rejected before any secret is prompted for or saved:
// a nonexistent password file together with an invalid id has to report
// invalid_arguments, not secret_error.
func TestCreateValidatesBeforeTouchingSecrets(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := privateDB(t, root, "medalert.db")
	getenv := testGetenv(root)

	code, _, stderr := runCLI(t, getenv, "account", "create",
		"--database", database, "--non-interactive",
		"--account", "bad id!", "--username", "u@example.com",
		"--password-file", filepath.Join(root, "missing"))
	if code != 2 || !strings.Contains(stderr, "account id") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if _, err := secrets.GetPassword("bad id!"); err == nil {
		t.Fatal("secret was saved for a rejected account")
	}
}

// An edit rejected by validation must leave the stored secret alone, even
// when password-change flags are present.
func TestEditValidatesBeforeTouchingSecrets(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := privateDB(t, root, "medalert.db")
	getenv := testGetenv(root)

	if _, err := store.Initialize(database); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(store.Account{ID: "alice", Username: "a@example.com", PasswordSource: store.PasswordSourceSecretService}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	storage.Close()
	if err := secrets.SetPassword("alice", "old-secret"); err != nil {
		t.Fatal(err)
	}

	tooLong := strings.Repeat("u", 300) + "@example.com"
	code, _, _ := runCLI(t, getenv, "account", "edit",
		"--database", database, "--non-interactive",
		"--account", "alice", "--username", tooLong,
		"--password-file", filepath.Join(root, "missing"))
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	value, err := secrets.GetPassword("alice")
	if err != nil || value != "old-secret" {
		t.Fatalf("stored secret = %q, %v; want untouched old-secret", value, err)
	}
}

func TestRestoreSecretAfterFailedUpdate(t *testing.T) {
	keyring.MockInit()

	// A failed service-to-service edit restores the stashed password.
	if err := secrets.SetPassword("a", "new-secret"); err != nil {
		t.Fatal(err)
	}
	restoreSecretAfterFailedUpdate("a", store.PasswordSourceSecretService, store.PasswordSourceSecretService, "old-secret", true, true)
	if value, err := secrets.GetPassword("a"); err != nil || value != "old-secret" {
		t.Fatalf("restored = %q, %v", value, err)
	}

	// A failed edit that newly saved a service password removes the orphan
	// when no previous service password is known to exist.
	if err := secrets.SetPassword("b", "new-secret"); err != nil {
		t.Fatal(err)
	}
	restoreSecretAfterFailedUpdate("b", store.PasswordSourceFile, store.PasswordSourceSecretService, "", false, true)
	if _, err := secrets.GetPassword("b"); err == nil {
		t.Fatal("orphaned secret was not removed")
	}

	// When the prior state is unknown, Secret Service is left untouched.
	if err := secrets.SetPassword("c", "new-secret"); err != nil {
		t.Fatal(err)
	}
	restoreSecretAfterFailedUpdate("c", store.PasswordSourceSecretService, store.PasswordSourceSecretService, "", false, false)
	if value, err := secrets.GetPassword("c"); err != nil || value != "new-secret" {
		t.Fatalf("secret = %q, %v; want untouched new-secret", value, err)
	}

	// Non-service edits never wrote anything: nothing happens.
	restoreSecretAfterFailedUpdate("d", store.PasswordSourceSecretService, store.PasswordSourceFile, "", false, true)
	restoreSecretAfterFailedUpdate("d", store.PasswordSourcePrompt, store.PasswordSourcePrompt, "", false, true)
}

// --version takes precedence over any command instead of becoming a
// positional account id.
func TestVersionFlagOverridesCommand(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	database := privateDB(t, root, "medalert.db")
	getenv := testGetenv(root)

	code, stdout, _ := runCLI(t, getenv, "account", "create",
		"--database", database, "--username", "u@example.com",
		"--no-stored-password", "--version")
	if code != 0 || !strings.HasPrefix(stdout, "medalert ") {
		t.Fatalf("code = %d, stdout = %q", code, stdout)
	}
	listCode, listOut, _ := runCLI(t, getenv, "account", "list", "--database", database)
	if listCode != 0 || !strings.Contains(listOut, "No accounts found.") {
		t.Fatalf("list = %d %q; the --version invocation must not create anything", listCode, listOut)
	}
}

func TestVersionAndDoctorRejectExtraArguments(t *testing.T) {
	root := t.TempDir()
	database := privateDB(t, root, "medalert.db")
	getenv := testGetenv(root)

	for _, args := range [][]string{
		{"version", "extra"},
		{"doctor", "--database", database, "extra"},
	} {
		if code, _, stderr := runCLI(t, getenv, args...); code != 2 || !strings.Contains(stderr, "too many arguments") {
			t.Fatalf("args %v: code = %d, stderr = %q", args, code, stderr)
		}
	}
}
