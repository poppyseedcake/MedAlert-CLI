package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func openTelegramStore(t *testing.T) *store.Store {
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
	t.Cleanup(func() { storage.Close() })
	return storage
}

func validDestination(id string) store.Destination {
	return store.Destination{
		ID:          id,
		Name:        "Phone " + id,
		ChatID:      "123456",
		TokenSource: store.TokenSourcePrompt,
		Enabled:     true,
	}
}

func TestDestinationsCRUDKeepsStableIdentity(t *testing.T) {
	storage := openTelegramStore(t)
	created, err := storage.CreateDestination(validDestination("phone"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "Phone phone" || !created.Enabled {
		t.Fatalf("created = %#v", created)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatal("timestamps are empty")
	}
	shown, err := storage.GetDestination("phone")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if shown.ChatID != "123456" {
		t.Fatalf("shown = %#v", shown)
	}
	name := "Renamed"
	updated, err := storage.UpdateDestination("phone", store.DestinationUpdate{Name: &name})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if updated.ID != "phone" || updated.Name != "Renamed" {
		t.Fatalf("identity changed: %#v", updated)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Fatalf("created_at changed: %q -> %q", created.CreatedAt, updated.CreatedAt)
	}
	disabled, err := storage.SetDestinationEnabled("phone", false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.Enabled {
		t.Fatal("still enabled after disable")
	}
	enabled, err := storage.SetDestinationEnabled("phone", true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !enabled.Enabled {
		t.Fatal("still disabled after enable")
	}
	if err := storage.DeleteDestination("phone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := storage.GetDestination("phone"); err == nil {
		t.Fatal("deleted destination still readable")
	}
}

func TestDestinationValidationRejectsBadInput(t *testing.T) {
	for _, destination := range []store.Destination{
		{ID: "", Name: "n", ChatID: "1", TokenSource: store.TokenSourcePrompt, Enabled: true},
		{ID: "bad id!", Name: "n", ChatID: "1", TokenSource: store.TokenSourcePrompt, Enabled: true},
		{ID: "ok", Name: "", ChatID: "1", TokenSource: store.TokenSourcePrompt, Enabled: true},
		{ID: "ok", Name: "n", ChatID: "", TokenSource: store.TokenSourcePrompt, Enabled: true},
		{ID: "ok", Name: "n", ChatID: "abc", TokenSource: store.TokenSourcePrompt, Enabled: true},
		{ID: "ok", Name: "n", ChatID: "0", TokenSource: store.TokenSourcePrompt, Enabled: true},
		{ID: "ok", Name: "n", ChatID: "1", TokenSource: "env", Enabled: true},
		{ID: "ok", Name: "n", ChatID: "1", TokenSource: store.TokenSourceFile, TokenRef: "", Enabled: true},
		{ID: "ok", Name: "n", ChatID: "1", TokenSource: store.TokenSourcePrompt, TokenRef: "/should/be/empty", Enabled: true},
	} {
		if err := store.ValidateDestination(destination); err == nil {
			t.Errorf("validation passed for %#v", destination)
		}
	}
}

func TestCreateDuplicateDestinationFails(t *testing.T) {
	storage := openTelegramStore(t)
	if _, err := storage.CreateDestination(validDestination("dup")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("dup")); err == nil {
		t.Fatal("duplicate create succeeded")
	}
}

func TestProfileLinksToMultipleDestinations(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	if _, err := storage.CreateProfile(validProfile("linked", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("two")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("linked", []string{"two", "one", "one"}); err != nil {
		t.Fatalf("link: %v", err)
	}
	ids, err := storage.ListProfileDestinationIDs("linked")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "one" || ids[1] != "two" {
		t.Fatalf("ids = %#v, want [one two]", ids)
	}
	linked, err := storage.ListProfileDestinations("linked")
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) != 2 {
		t.Fatalf("linked = %#v", linked)
	}
	// Clearing unlinks all.
	if err := storage.SetProfileDestinations("linked", []string{}); err != nil {
		t.Fatal(err)
	}
	after, err := storage.ListProfileDestinations("linked")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("after clear = %#v", after)
	}
	// Unknown destination is rejected.
	if err := storage.SetProfileDestinations("linked", []string{"missing"}); err == nil {
		t.Fatal("link to missing destination succeeded")
	}
	// Unknown profile is rejected.
	if err := storage.SetProfileDestinations("nobody", []string{"one"}); err == nil {
		t.Fatal("link to missing profile succeeded")
	}
}

func TestDestinationLinksToMultipleProfiles(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	for _, profileID := range []string{"morning", "evening"} {
		if _, err := storage.CreateProfile(validProfile(profileID, "alice")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.CreateDestination(validDestination("phone")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("tablet")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("morning", []string{"tablet"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetDestinationProfiles("phone", []string{"evening", "morning", "evening"}); err != nil {
		t.Fatalf("link destination: %v", err)
	}
	profiles, err := storage.ListDestinationProfileIDs("phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0] != "evening" || profiles[1] != "morning" {
		t.Fatalf("destination profiles = %#v, want [evening morning]", profiles)
	}
	morningDestinations, err := storage.ListProfileDestinationIDs("morning")
	if err != nil {
		t.Fatal(err)
	}
	if len(morningDestinations) != 2 || morningDestinations[0] != "phone" || morningDestinations[1] != "tablet" {
		t.Fatalf("morning destinations = %#v, want [phone tablet]", morningDestinations)
	}
	if err := storage.SetDestinationProfiles("phone", []string{"evening"}); err != nil {
		t.Fatal(err)
	}
	profiles, err = storage.ListDestinationProfileIDs("phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0] != "evening" {
		t.Fatalf("destination profiles after replace = %#v, want [evening]", profiles)
	}
	morningDestinations, err = storage.ListProfileDestinationIDs("morning")
	if err != nil {
		t.Fatal(err)
	}
	if len(morningDestinations) != 1 || morningDestinations[0] != "tablet" {
		t.Fatalf("morning destinations after replace = %#v, want [tablet]", morningDestinations)
	}
	if err := storage.SetDestinationProfiles("phone", []string{"missing"}); err == nil {
		t.Fatal("link to missing profile succeeded")
	}
	profiles, err = storage.ListDestinationProfileIDs("phone")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0] != "evening" {
		t.Fatalf("links changed after rejected update = %#v", profiles)
	}
}

func TestDeleteDestinationRemovesLinks(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	if _, err := storage.CreateProfile(validProfile("p1", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("gone")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("p1", []string{"gone"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteDestination("gone"); err != nil {
		t.Fatal(err)
	}
	links, err := storage.ListProfileDestinations("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("links after delete = %#v", links)
	}
}

func TestDeleteProfileRemovesLinks(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	if _, err := storage.CreateProfile(validProfile("temp", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("keep")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("temp", []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteProfile("temp"); err != nil {
		t.Fatal(err)
	}
	// Destination survives, links are gone with the profile.
	if _, err := storage.GetDestination("keep"); err != nil {
		t.Fatalf("destination should survive profile delete: %v", err)
	}
}

func TestRecordDestinationTestStoresResult(t *testing.T) {
	storage := openTelegramStore(t)
	if _, err := storage.CreateDestination(validDestination("tested")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	updated, err := storage.RecordDestinationTest("tested", "delivered", "test delivered", now)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if updated.LastTestStatus != "delivered" || updated.LastTestAt == "" {
		t.Fatalf("updated = %#v", updated)
	}
	shown, err := storage.GetDestination("tested")
	if err != nil {
		t.Fatal(err)
	}
	if shown.LastTestStatus != "delivered" {
		t.Fatalf("shown = %#v", shown)
	}
}

func TestMigrateV4ToV5KeepsProfiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, 4, `
		CREATE TABLE application_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
		CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (2, '2026-01-02T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (3, '2026-01-03T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (4, '2026-01-04T00:00:00Z');
		CREATE TABLE accounts (id TEXT PRIMARY KEY, username TEXT NOT NULL, password_source TEXT NOT NULL CHECK(password_source IN ('secret-service','file','prompt')), password_ref TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
		INSERT INTO accounts (id, username, password_source, password_ref, created_at, updated_at) VALUES ('alice', 'a@example.com', 'prompt', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
		CREATE TABLE profiles (id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, region_ids TEXT NOT NULL, specialty_ids TEXT NOT NULL, clinic_ids TEXT NOT NULL DEFAULT '', doctor_ids TEXT NOT NULL DEFAULT '', language_ids TEXT NOT NULL DEFAULT '', visit_type TEXT NOT NULL DEFAULT '', search_type TEXT NOT NULL DEFAULT 'Standard', start_date TEXT NOT NULL DEFAULT '', end_date TEXT NOT NULL DEFAULT '', check_interval_minutes INTEGER NOT NULL CHECK(check_interval_minutes BETWEEN 1 AND 43200), enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)), created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
		INSERT INTO profiles (id, account_id, region_ids, specialty_ids, check_interval_minutes, enabled, created_at, updated_at) VALUES ('p1', 'alice', '204', '132', 30, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
	`)
	status, err := store.Initialize(databasePath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if status.SchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("schema = %d, want %d", status.SchemaVersion, store.CurrentSchemaVersion)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer storage.Close()
	if _, err := storage.GetProfile("p1"); err != nil {
		t.Fatalf("profile after migrate: %v", err)
	}
	if _, err := storage.CreateDestination(validDestination("after-migrate")); err != nil {
		t.Fatalf("create destination after migrate: %v", err)
	}
}

func TestCreateProfileWithDestinationsIsAtomic(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	// Missing destination leaves no orphan profile behind, so a retry with a
	// valid list succeeds.
	_, err := storage.CreateProfileWithDestinations(validProfile("atomic", "alice"), []string{"one", "missing"})
	if err == nil {
		t.Fatal("create with missing destination succeeded")
	}
	if _, err := storage.GetProfile("atomic"); err == nil {
		t.Fatal("partial profile left behind after failed atomic create")
	}
	created, err := storage.CreateProfileWithDestinations(validProfile("atomic", "alice"), []string{"one"})
	if err != nil {
		t.Fatalf("retry after failed atomic create: %v", err)
	}
	ids, err := storage.ListProfileDestinationIDs(created.ID)
	if err != nil || len(ids) != 1 || ids[0] != "one" {
		t.Fatalf("links after retry = %#v, %v", ids, err)
	}
}

func TestUpdateProfileWithDestinationsIsAtomic(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(validProfile("edit-atomic", "alice")); err != nil {
		t.Fatal(err)
	}
	clinic := "10"
	_, err := storage.UpdateProfileWithDestinations("edit-atomic", store.ProfileUpdate{ClinicIDs: &clinic}, true, []string{"missing"}, true)
	if err == nil {
		t.Fatal("update with missing destination succeeded")
	}
	shown, err := storage.GetProfile("edit-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if shown.ClinicIDs == "10" {
		t.Fatalf("criteria changed despite failed atomic update: %#v", shown)
	}
	links, err := storage.ListProfileDestinationIDs("edit-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("links changed despite failed atomic update: %#v", links)
	}
}

func TestUpdateProfileDestinationsOnlyBumpsUpdatedAt(t *testing.T) {
	storage := openTelegramStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("two")); err != nil {
		t.Fatal(err)
	}
	before, err := storage.CreateProfileWithDestinations(validProfile("bump", "alice"), []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	// Ensure the clock advances past the create timestamp.
	time.Sleep(2 * time.Millisecond)
	after, err := storage.UpdateProfileWithDestinations("bump", store.ProfileUpdate{}, false, []string{"one", "two"}, true)
	if err != nil {
		t.Fatalf("destinations-only update: %v", err)
	}
	if after.UpdatedAt == "" || after.UpdatedAt == before.UpdatedAt {
		t.Fatalf("updated_at not bumped: before=%q after=%q", before.UpdatedAt, after.UpdatedAt)
	}
	shown, err := storage.GetProfile("bump")
	if err != nil {
		t.Fatal(err)
	}
	if shown.UpdatedAt != after.UpdatedAt {
		t.Fatalf("persisted updated_at = %q, want %q", shown.UpdatedAt, after.UpdatedAt)
	}
}
