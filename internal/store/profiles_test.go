package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func openProfileStore(t *testing.T) (*store.Store, string) {
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
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	return storage, databasePath
}

func mustCreateAccount(t *testing.T, storage *store.Store, id, username string) {
	t.Helper()
	if _, err := storage.CreateAccount(store.Account{ID: id, Username: username, PasswordSource: store.PasswordSourcePrompt}); err != nil {
		t.Fatalf("create account %s: %v", id, err)
	}
}

func validProfile(id, account string) store.Profile {
	return store.Profile{
		ID:                   id,
		AccountID:            account,
		RegionIDs:            "204",
		SpecialtyIDs:         "132",
		CheckIntervalMinutes: 30,
		Enabled:              true,
		SearchType:           store.SearchTypeStandard,
	}
}

func TestProfilesCRUDKeepsStableIdentity(t *testing.T) {
	storage, _ := openProfileStore(t)

	created, err := storage.CreateProfile(validProfile("myprof", "alice"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.AccountID != "alice" || !created.Enabled {
		t.Fatalf("created = %#v", created)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatal("timestamps are empty")
	}

	shown, err := storage.GetProfile("myprof")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if shown.RegionIDs != "204" || shown.SpecialtyIDs != "132" {
		t.Fatalf("shown = %#v", shown)
	}

	// Editing criteria keeps the stable identity, account, and timestamps.
	clinic := "10,11"
	interval := 15
	updated, err := storage.UpdateProfile("myprof", store.ProfileUpdate{ClinicIDs: &clinic, CheckIntervalMinutes: &interval})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if updated.ID != "myprof" || updated.AccountID != "alice" {
		t.Fatalf("identity changed: %#v", updated)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Fatalf("created_at changed: %q -> %q", created.CreatedAt, updated.CreatedAt)
	}
	if updated.ClinicIDs != "10,11" || updated.CheckIntervalMinutes != 15 {
		t.Fatalf("updated = %#v", updated)
	}
	if !updated.Enabled {
		t.Fatal("edit changed enabled state")
	}

	profiles, err := storage.ListProfiles("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("list = %#v", profiles)
	}
}

func TestProfilesSupportFullCriteria(t *testing.T) {
	storage, _ := openProfileStore(t)

	full := store.Profile{
		ID: "full", AccountID: "alice",
		RegionIDs: "204,205", SpecialtyIDs: "132,133",
		ClinicIDs: "10", DoctorIDs: "20", LanguageIDs: "4,6",
		VisitType: "Center", SearchType: store.SearchTypeDiagnosticProcedure,
		StartDate: "2026-09-01", EndDate: "2026-09-30",
		CheckIntervalMinutes: 60, Enabled: true,
	}
	if _, err := storage.CreateProfile(full); err != nil {
		t.Fatalf("create full: %v", err)
	}
	shown, err := storage.GetProfile("full")
	if err != nil {
		t.Fatalf("show full: %v", err)
	}
	if shown.RegionIDs != "204,205" || shown.LanguageIDs != "4,6" || shown.VisitType != "Center" || shown.SearchType != "DiagnosticProcedure" {
		t.Fatalf("shown = %#v", shown)
	}
	if shown.StartDate != "2026-09-01" || shown.EndDate != "2026-09-30" {
		t.Fatalf("dates = %#v", shown)
	}
}

func TestCreateDuplicateProfileFails(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("dup", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(validProfile("dup", "alice")); err == nil {
		t.Fatal("duplicate create succeeded")
	}
}

func TestCreateProfileRequiresExistingAccount(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("orphan", "nobody")); err == nil {
		t.Fatal("create with unknown account succeeded")
	}
}

func TestProfileValidationRejectsBadInput(t *testing.T) {
	for _, profile := range []store.Profile{
		{ID: "", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30},
		{ID: "bad id!", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30},
		{ID: "ok", AccountID: "", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30},
		{ID: "ok", AccountID: "alice", RegionIDs: "", SpecialtyIDs: "132", CheckIntervalMinutes: 30},
		{ID: "ok", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "", CheckIntervalMinutes: 30},
		{ID: "ok", AccountID: "alice", RegionIDs: "bad", SpecialtyIDs: "132", CheckIntervalMinutes: 30},
		{ID: "ok", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 0},
		{ID: "ok", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 99999},
		{ID: "ok", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, SearchType: "Nope"},
		{ID: "ok", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, StartDate: "2026-13-01"},
		{ID: "ok", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, StartDate: "2026-09-10", EndDate: "2026-09-01"},
	} {
		if err := store.ValidateProfile(profile); err == nil {
			t.Errorf("validation passed for %#v", profile)
		}
	}
}

func TestSetEnabledPausesWithoutDeleting(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("pausable", "alice")); err != nil {
		t.Fatal(err)
	}
	disabled, err := storage.SetProfileEnabled("pausable", false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.Enabled {
		t.Fatal("still enabled after disable")
	}
	shown, err := storage.GetProfile("pausable")
	if err != nil {
		t.Fatalf("show after disable: %v", err)
	}
	if shown.Enabled {
		t.Fatal("show reports enabled after disable")
	}
	enabled, err := storage.SetProfileEnabled("pausable", true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !enabled.Enabled {
		t.Fatal("still disabled after enable")
	}
}

func TestDeleteProfileRemovesConfiguration(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("gone", "alice")); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteProfile("gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := storage.GetProfile("gone"); err == nil {
		t.Fatal("deleted profile still readable")
	}
	if err := storage.DeleteProfile("gone"); err == nil {
		t.Fatal("second delete succeeded")
	}
}

func TestListProfilesFiltersByAccount(t *testing.T) {
	storage, _ := openProfileStore(t)
	mustCreateAccount(t, storage, "bob", "bob@example.com")
	if _, err := storage.CreateProfile(validProfile("a1", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateProfile(validProfile("b1", "bob")); err != nil {
		t.Fatal(err)
	}
	all, err := storage.ListProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %#v", all)
	}
	onlyAlice, err := storage.ListProfiles("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyAlice) != 1 || onlyAlice[0].ID != "a1" {
		t.Fatalf("alice = %#v", onlyAlice)
	}
}

func TestDeleteAccountCascadesProfiles(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("cascaded", "alice")); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteAccount("alice"); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if _, err := storage.GetProfile("cascaded"); err == nil {
		t.Fatal("profile survived account delete")
	}
}

func TestMigrateV2ToV3KeepsAccounts(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, 2, `
		CREATE TABLE application_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
		CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (2, '2026-01-02T00:00:00Z');
		CREATE TABLE accounts (id TEXT PRIMARY KEY, username TEXT NOT NULL, password_source TEXT NOT NULL CHECK(password_source IN ('secret-service','file','prompt')), password_ref TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
		INSERT INTO accounts (id, username, password_source, password_ref, created_at, updated_at) VALUES ('alice', 'a@example.com', 'prompt', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
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
	account, err := storage.GetAccount("alice")
	if err != nil {
		t.Fatalf("account after migrate: %v", err)
	}
	if account.Username != "a@example.com" {
		t.Fatalf("account = %#v", account)
	}
	if _, err := storage.CreateProfile(validProfile("after-migrate", "alice")); err != nil {
		t.Fatalf("create profile after migrate: %v", err)
	}
}

func TestNormalizeIDList(t *testing.T) {
	got, err := store.NormalizeIDList("205, 204,204")
	if err != nil || got != "204,205" {
		t.Fatalf("normalize = %q, %v", got, err)
	}
	if got, _ := store.NormalizeIDList(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	for _, bad := range []string{"0", "-1", "abc", "1.5", "99999999"} {
		if _, err := store.NormalizeIDList(bad); err == nil {
			t.Errorf("normalize passed for %q", bad)
		}
	}
}

func TestNormalizeSearchType(t *testing.T) {
	if got, _ := store.NormalizeSearchType(""); got != store.SearchTypeStandard {
		t.Fatalf("empty = %q", got)
	}
	if got, _ := store.NormalizeSearchType("0"); got != store.SearchTypeStandard {
		t.Fatalf("0 = %q", got)
	}
	if _, err := store.NormalizeSearchType("Nope"); err == nil {
		t.Fatal("bad search type passed")
	}
}

func TestNormalizeIDListRejectsEmptyElements(t *testing.T) {
	for _, bad := range []string{",", ",,", "204,", ",204", "204,,205", "204, ,205", "10,11,"} {
		if got, err := store.NormalizeIDList(bad); err == nil {
			t.Errorf("normalize passed for %q = %q, want invalid_arguments", bad, got)
		}
	}
}

func TestValidateProfileRejectsMalformedIDLists(t *testing.T) {
	base := validProfile("malformed", "alice")
	for _, mutate := range []func(*store.Profile){
		func(p *store.Profile) { p.RegionIDs = "204,,205" },
		func(p *store.Profile) { p.SpecialtyIDs = ",132" },
		func(p *store.Profile) { p.ClinicIDs = "10," },
		func(p *store.Profile) { p.DoctorIDs = "," },
	} {
		profile := base
		mutate(&profile)
		if err := store.ValidateProfile(profile); err == nil {
			t.Errorf("validation passed for %#v", profile)
		}
	}
}

func TestUpdatePreservesEnabledAndUntouchedFields(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("stable", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SetProfileEnabled("stable", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	clinic := "10,11"
	if _, err := storage.UpdateProfile("stable", store.ProfileUpdate{ClinicIDs: &clinic}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	shown, err := storage.GetProfile("stable")
	if err != nil {
		t.Fatal(err)
	}
	if shown.Enabled {
		t.Fatal("edit re-enabled a disabled profile")
	}
	if shown.ClinicIDs != "10,11" || shown.RegionIDs != "204" {
		t.Fatalf("shown = %#v", shown)
	}
}

func TestConcurrentUpdateAndDisableDoNotLoseState(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("racing", "alice")); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err := storage.SetProfileEnabled("racing", true); err != nil {
			t.Fatal(err)
		}
		empty := ""
		if _, err := storage.UpdateProfile("racing", store.ProfileUpdate{ClinicIDs: &empty}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 2)
		go func() {
			clinic := "10"
			_, err := storage.UpdateProfile("racing", store.ProfileUpdate{ClinicIDs: &clinic})
			done <- err
		}()
		go func() {
			_, err := storage.SetProfileEnabled("racing", false)
			done <- err
		}()
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		shown, err := storage.GetProfile("racing")
		if err != nil {
			t.Fatal(err)
		}
		if shown.ClinicIDs != "10" {
			t.Fatalf("concurrent disable lost clinic edit: %#v", shown)
		}
		if shown.Enabled {
			t.Fatalf("concurrent edit lost disable: %#v", shown)
		}
	}
}
