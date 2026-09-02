package store_test

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/store"
	_ "modernc.org/sqlite"
)

func TestInitializeCreatesPrivateCurrentDatabase(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "medalert.db")

	status, err := store.Initialize(databasePath)
	if err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	if status.SchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", status.SchemaVersion, store.CurrentSchemaVersion)
	}
	assertMode(t, filepath.Dir(databasePath), 0o700)
	assertMode(t, databasePath, 0o600)
	assertMode(t, filepath.Join(filepath.Dir(databasePath), "backups"), 0o700)
}

func TestInitializeMigratesWithBackupAndKeepsThreeNewest(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, 0, "CREATE TABLE existing (value TEXT); INSERT INTO existing VALUES ('kept');")
	backupDir := filepath.Join(root, "backups")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 4 {
		name := filepath.Join(backupDir, fmt.Sprintf("medalert-schema-0-old-%d.sqlite3", index))
		if err := os.WriteFile(name, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	backups, err := filepath.Glob(filepath.Join(backupDir, "medalert-schema-*.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 3 {
		t.Fatalf("backup count = %d, want 3: %v", len(backups), backups)
	}
	for _, backup := range backups {
		assertMode(t, backup, 0o600)
	}
	assertQueryValue(t, databasePath, "SELECT value FROM existing", "kept")
}

func TestInitializeRollsBackFailedMigration(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, 0, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL,
			required_value TEXT NOT NULL
		);
	`)

	if _, err := store.Initialize(databasePath); err == nil {
		t.Fatal("initialize succeeded, want migration error")
	}

	database := openDatabase(t, databasePath)
	defer database.Close()
	var count int
	if err := database.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'application_metadata'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("application_metadata exists after failed migration")
	}
	var version int
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("schema version = %d, want 0", version)
	}
	assertQueryValue(t, databasePath, "SELECT count(*) FROM schema_migrations", "0")
}

func TestInspectRejectsNewerSchemaWithoutChangingDatabase(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, store.CurrentSchemaVersion+1, "CREATE TABLE future_data (value TEXT); INSERT INTO future_data VALUES ('safe');")
	before := fileHash(t, databasePath)

	_, err := store.Inspect(databasePath)
	if err == nil {
		t.Fatal("inspect succeeded, want unsupported schema error")
	}
	if !store.IsUnsupportedSchema(err) {
		t.Fatalf("error = %v, want unsupported schema", err)
	}
	after := fileHash(t, databasePath)
	if before != after {
		t.Fatal("newer database changed")
	}
	assertQueryValue(t, databasePath, "SELECT value FROM future_data", "safe")
}

func createDatabase(t *testing.T, path string, version int, statements string) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database := openDatabase(t, path)
	defer database.Close()
	if _, err := database.Exec(statements); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func openDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func assertQueryValue(t *testing.T, path, query, want string) {
	t.Helper()
	database := openDatabase(t, path)
	defer database.Close()
	var got string
	if err := database.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query value = %q, want %q", got, want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func fileHash(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(contents)
}
