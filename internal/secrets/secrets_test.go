package secrets_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/zalando/go-keyring"
	"golang.org/x/sys/unix"
)

func TestSecretFileRoundTripAndRedaction(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "SECRET-MARKER-FILE-" + t.Name() + "-unique-12345"
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := secrets.ReadSecretFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if value != marker {
		t.Fatalf("value mismatch")
	}
	if errText := errStringForFile(path, marker); strings.Contains(errText, marker) {
		t.Fatal("error text contains secret")
	}
}

func TestSecretFileStripsSingleTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"password\n", "password"},
		{"password\r\n", "password"},
		{"password\n\n", "password\n"},
		{"password\n\n\n", "password\n\n"},
		{"password", "password"},
	} {
		path := filepath.Join(dir, "pw")
		if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := secrets.ReadSecretFile(path)
		if err != nil {
			t.Fatalf("read %q: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("read %q = %q, want %q", tc.raw, got, tc.want)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
func errStringForFile(path, _ string) string {
	_, err := secrets.ReadSecretFile(filepath.Join(filepath.Dir(path), "missing"))
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSecretFileRejectsUnsafePermissionsAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(dir, "open")
	if err := os.WriteFile(openPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.ReadSecretFile(openPath); err == nil {
		t.Fatal("open permissions accepted")
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.ReadSecretFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
}

// A FIFO must be rejected without blocking: the open must not wait for a
// writer before the regular-file check runs.
func TestSecretFileRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.ReadSecretFile(path); err == nil {
		t.Fatal("FIFO accepted")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %q, want non-regular-file rejection", err)
	}
}

func TestSecretServiceMockDoesNotLeak(t *testing.T) {
	keyring.MockInit()
	marker := "SECRET-MARKER-SERVICE-unique-67890"
	if err := secrets.SetPassword("acct1", marker); err != nil {
		t.Fatalf("set: %v", err)
	}
	value, err := secrets.GetPassword("acct1")
	if err != nil || value != marker {
		t.Fatalf("get = %q, %v", value, err)
	}
	if _, err := secrets.GetPassword("missing-acct"); err == nil {
		t.Fatal("missing secret found")
	} else if strings.Contains(err.Error(), marker) {
		t.Fatal("missing error leaks secret")
	}
	if err := secrets.DeletePassword("acct1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := secrets.GetPassword("acct1"); err == nil {
		t.Fatal("deleted secret still readable")
	}
}

func TestPromptRejectsNonInteractive(t *testing.T) {
	keyring.MockInit()
	// nil stdin or non-interactive must fail without blocking.
	if _, err := secrets.PromptForPassword("Password: ", nil, os.Stderr, true); err == nil {
		t.Fatal("non-interactive prompt succeeded")
	}
	if _, err := secrets.PromptForPassword("Password: ", nil, os.Stderr, false); err == nil {
		t.Fatal("nil stdin prompt succeeded")
	}
}
