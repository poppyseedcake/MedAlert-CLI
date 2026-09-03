// Package store owns the MedAlert SQLite lifecycle.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/buildinfo"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const (
	CurrentSchemaVersion = 2
	backupLimit          = 3
)

type Status struct {
	Exists            bool `json:"exists"`
	SchemaVersion     int  `json:"schema_version"`
	RequiredVersion   int  `json:"required_schema_version"`
	MigrationRequired bool `json:"migration_required"`
}

type UnsupportedSchemaError struct {
	Found     int
	Supported int
}

func (problem *UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("database schema %d is newer than supported schema %d", problem.Found, problem.Supported)
}

func IsUnsupportedSchema(err error) bool {
	var problem *UnsupportedSchemaError
	return errors.As(err, &problem)
}

// Inspect reports database state without creating or migrating it.
func Inspect(path string) (Status, error) {
	status := Status{RequiredVersion: CurrentSchemaVersion}
	if path == "" {
		return status, errors.New("database path is empty")
	}
	info, err := secureFileInfo(path)
	if errors.Is(err, os.ErrNotExist) {
		status.MigrationRequired = true
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if err := validateDatabaseFile(path, info); err != nil {
		return status, err
	}
	status.Exists = true
	database, err := openDatabase(path, true)
	if err != nil {
		return status, err
	}
	defer database.Close()
	if err := database.QueryRow("PRAGMA user_version").Scan(&status.SchemaVersion); err != nil {
		return status, fmt.Errorf("read database schema version: %w", err)
	}
	if status.SchemaVersion > CurrentSchemaVersion {
		return status, &UnsupportedSchemaError{Found: status.SchemaVersion, Supported: CurrentSchemaVersion}
	}
	status.MigrationRequired = status.SchemaVersion < CurrentSchemaVersion
	return status, nil
}

// Initialize creates a private database or applies supported forward migrations.
func Initialize(path string) (Status, error) {
	if path == "" || path == ":memory:" {
		return Status{}, errors.New("database initialization needs a file path")
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateDirectory(parent); err != nil {
		return Status{}, err
	}
	backupDirectory := filepath.Join(parent, "backups")
	if err := ensurePrivateDirectory(backupDirectory); err != nil {
		return Status{}, err
	}

	existed, err := ensurePrivateDatabaseFile(path)
	if err != nil {
		return Status{}, err
	}
	lockFile, err := lockInitialization(path)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		_ = lockFile.Close()
	}()
	status, err := Inspect(path)
	if err != nil {
		return Status{}, err
	}
	if !status.MigrationRequired {
		return status, nil
	}

	database, err := openDatabase(path, false)
	if err != nil {
		return Status{}, err
	}
	defer database.Close()
	if existed {
		if err := createBackup(database, backupDirectory, status.SchemaVersion); err != nil {
			return Status{}, err
		}
	}
	if err := migrate(database); err != nil {
		return Status{}, err
	}
	if existed {
		if err := pruneBackups(backupDirectory); err != nil {
			return Status{}, err
		}
	}
	status.Exists = true
	status.SchemaVersion = CurrentSchemaVersion
	status.MigrationRequired = false
	return status, nil
}

func lockInitialization(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open database initialization lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock database initialization: %w", err)
	}
	return file, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private directory %s: %w", path, err)
	}
	info, err := secureFileInfo(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state path is not a directory: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("directory permissions are not private: %s has mode %04o", path, info.Mode().Perm())
	}
	return nil
}

func ensurePrivateDatabaseFile(path string) (bool, error) {
	info, err := secureFileInfo(path)
	if err == nil {
		if err := validateDatabaseFile(path, info); err != nil {
			return true, err
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create private database: %w", err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close new database: %w", err)
	}
	return false, nil
}

func validateDatabaseFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("database permissions are not private: %s has mode %04o", path, info.Mode().Perm())
	}
	return nil
}

func secureFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symbolic links are not permitted for state paths: %s", path)
	}
	return info, nil
}

func openDatabase(path string, readOnly bool) (*sql.DB, error) {
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	location := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode}
	database, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if readOnly {
		return database, nil
	}
	if _, err := database.Exec("PRAGMA busy_timeout = 5000; PRAGMA foreign_keys = ON"); err != nil {
		database.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	return database, nil
}

func createBackup(database *sql.DB, directory string, schemaVersion int) error {
	commit := safeName(buildinfo.SourceCommit())
	timestamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	path := filepath.Join(directory, fmt.Sprintf("medalert-schema-%d-%s-%s.sqlite3", schemaVersion, commit, timestamp))
	if _, err := database.Exec("VACUUM INTO ?", path); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("create migration backup: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("protect migration backup: %w", err)
	}
	return nil
}

func safeName(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' {
			result.WriteRune(character)
		}
	}
	if result.Len() == 0 {
		return "unknown"
	}
	return result.String()
}

func pruneBackups(directory string) error {
	backups, err := filepath.Glob(filepath.Join(directory, "medalert-schema-*.sqlite3"))
	if err != nil {
		return fmt.Errorf("list migration backups: %w", err)
	}
	type backup struct {
		path    string
		modTime time.Time
	}
	items := make([]backup, 0, len(backups))
	for _, path := range backups {
		info, statErr := secureFileInfo(path)
		if statErr != nil {
			return fmt.Errorf("inspect migration backup: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("migration backup is not a regular file: %s", path)
		}
		items = append(items, backup{path: path, modTime: info.ModTime()})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].modTime.Equal(items[right].modTime) {
			return items[left].path > items[right].path
		}
		return items[left].modTime.After(items[right].modTime)
	})
	if len(items) <= backupLimit {
		return nil
	}
	for _, old := range items[backupLimit:] {
		if err := os.Remove(old.path); err != nil {
			return fmt.Errorf("remove old migration backup: %w", err)
		}
	}
	return nil
}

func migrate(database *sql.DB) (err error) {
	ctx := context.Background()
	connection, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve migration connection: %w", err)
	}
	defer connection.Close()
	if _, err = connection.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return fmt.Errorf("start migration: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = connection.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var fromVersion int
	if err = connection.QueryRowContext(ctx, "PRAGMA user_version").Scan(&fromVersion); err != nil {
		return fmt.Errorf("read locked database schema version: %w", err)
	}
	if fromVersion > CurrentSchemaVersion {
		return &UnsupportedSchemaError{Found: fromVersion, Supported: CurrentSchemaVersion}
	}
	if fromVersion < 1 {
		statements := []string{
			"CREATE TABLE application_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT",
			"CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT",
			"INSERT INTO schema_migrations (version, applied_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))",
			"PRAGMA user_version = 1",
		}
		for _, statement := range statements {
			if _, err = connection.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration 1: %w", err)
			}
		}
	}
	if fromVersion < 2 {
		statements := []string{
			"CREATE TABLE accounts (id TEXT PRIMARY KEY, username TEXT NOT NULL, password_source TEXT NOT NULL CHECK(password_source IN ('secret-service','file','prompt')), password_ref TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT",
			"INSERT INTO schema_migrations (version, applied_at) VALUES (2, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))",
			"PRAGMA user_version = 2",
		}
		for _, statement := range statements {
			if _, err = connection.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration 2: %w", err)
			}
		}
	}
	if _, err = connection.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
