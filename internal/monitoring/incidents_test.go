package monitoring

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

// openClosedIncidentStore creates an account, a profile, and a destination,
// then closes the handle so every subsequent database read fails. It returns
// the closed store, the database path for reopening, and the profile.
func openClosedIncidentStore(t *testing.T) (*store.Store, string, store.Profile) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := storage.CreateAccount(store.Account{ID: "alice", Username: "alice@example.com", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		storage.Close()
		t.Fatalf("create account: %v", err)
	}
	profile, err := storage.CreateProfile(store.Profile{ID: "morning", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, Enabled: true, SearchType: store.SearchTypeStandard})
	if err != nil {
		storage.Close()
		t.Fatalf("create profile: %v", err)
	}
	if _, err := storage.CreateDestination(store.Destination{ID: "phone", Name: "Telefon", ChatID: "123456", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		storage.Close()
		t.Fatalf("create destination: %v", err)
	}
	if err := storage.SetProfileDestinations("morning", []string{"phone"}); err != nil {
		storage.Close()
		t.Fatalf("link: %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return storage, databasePath, profile
}

func assertNoIncidentsStored(t *testing.T, databasePath string) {
	t.Helper()
	reopened, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	incidents, err := reopened.ListIncidents("", "", 0)
	if err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	if len(incidents) != 0 {
		t.Fatalf("incidents = %#v, want none after a destination-list failure", incidents)
	}
}

// A temporary database read error while resolving eligible destinations must
// fail the recording instead of creating an incident without failure
// deliveries. Later updates could never add the missing routes, and the
// incident would stay silent.
func TestRecordProfileFailurePropagatesDestinationListError(t *testing.T) {
	storage, databasePath, profile := openClosedIncidentStore(t)
	if _, _, err := RecordProfileFailure(storage, profile, "protocol_changed", "changed", time.Now().UTC()); err == nil {
		t.Fatal("RecordProfileFailure succeeded, want destination-list error")
	}
	assertNoIncidentsStored(t, databasePath)
}

func TestRecordAccountFailurePropagatesDestinationListError(t *testing.T) {
	storage, databasePath, _ := openClosedIncidentStore(t)
	if _, _, err := RecordAccountFailure(storage, "alice", "authentication_required", "auth needed", time.Now().UTC()); err == nil {
		t.Fatal("RecordAccountFailure succeeded, want destination-list error")
	}
	assertNoIncidentsStored(t, databasePath)
}

func TestRecordDestinationFailurePropagatesDestinationListError(t *testing.T) {
	storage, databasePath, profile := openClosedIncidentStore(t)
	if _, _, err := RecordDestinationFailure(storage, profile, "phone", "permanent_failure", "chat not found", time.Now().UTC()); err == nil {
		t.Fatal("RecordDestinationFailure succeeded, want destination-list error")
	}
	assertNoIncidentsStored(t, databasePath)
}
