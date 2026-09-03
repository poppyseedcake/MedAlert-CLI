package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/zalando/go-keyring"
)

func testState(device string) *medicover.SessionState {
	return &medicover.SessionState{
		DeviceID:     device,
		Cookies:      []medicover.StoredCookie{{Name: "MedicoverTrusted", Value: "trusted-1", Path: "/"}},
		RefreshToken: "refresh-" + device,
		UpdatedAt:    "2026-01-01T00:00:00Z",
	}
}

func TestSecretServiceIsolatesAccounts(t *testing.T) {
	keyring.MockInit()
	store := session.SecretServiceStore{}
	if err := store.Save("alice", testState("device-alice")); err != nil {
		t.Fatalf("save alice: %v", err)
	}
	if err := store.Save("bob", testState("device-bob")); err != nil {
		t.Fatalf("save bob: %v", err)
	}
	// Logging out one account removes only that account's state.
	if err := store.Delete("alice"); err != nil {
		t.Fatalf("delete alice: %v", err)
	}
	if _, err := store.Load("alice"); err == nil {
		t.Fatal("alice session still present after logout")
	}
	bob, err := store.Load("bob")
	if err != nil || bob.DeviceID != "device-bob" {
		t.Fatalf("bob session = %+v, %v", bob, err)
	}
	_ = store.Delete("bob")
}

func TestSecretServiceCorruptBecomesCorruptError(t *testing.T) {
	keyring.MockInit()
	store := session.SecretServiceStore{}
	if err := keyring.Set("medalert", session.ServiceKey("corrupt"), "not-json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("corrupt"); err == nil {
		t.Fatal("corrupt session loaded")
	}
	_ = store.Delete("corrupt")
}

func TestFileStoreRoundTripAndIsolation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := session.FileStore{Dir: root}
	if err := store.Save("alice", testState("device-alice")); err != nil {
		t.Fatalf("save alice: %v", err)
	}
	if err := store.Save("bob", testState("device-bob")); err != nil {
		t.Fatalf("save bob: %v", err)
	}
	assertFileMode(t, filepath.Join(root, "sessions", "alice.json"), 0o600)
	assertFileMode(t, filepath.Join(root, "sessions"), 0o700)

	if err := store.Delete("alice"); err != nil {
		t.Fatalf("delete alice: %v", err)
	}
	if _, err := store.Load("alice"); err == nil {
		t.Fatal("alice session still present")
	}
	bob, err := store.Load("bob")
	if err != nil || bob.DeviceID != "device-bob" {
		t.Fatalf("bob = %+v, %v", bob, err)
	}
}

func TestFileStoreRejectsSymlinkAndBadPermissions(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := session.FileStore{Dir: root}
	if err := store.Save("alice", testState("device-alice")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "alice.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("alice"); err == nil {
		t.Fatal("open permissions accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "sessions", "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sessions", "linked.json")
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("linked"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestFileStoreCorruptFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "bad.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := session.FileStore{Dir: root}
	if _, err := store.Load("bad"); err == nil {
		t.Fatal("corrupt file loaded")
	}
}

func TestFileStoreMissingIsNotFound(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := session.FileStore{Dir: root}
	if _, err := store.Load("missing"); err == nil {
		t.Fatal("missing session loaded")
	} else if !strings.Contains(err.Error(), "no saved session") {
		t.Fatalf("err = %v, want not found", err)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
