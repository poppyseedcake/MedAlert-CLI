package main_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const testCommit = "0123456789abcdef"

var executablePath string

func TestMain(m *testing.M) {
	temporaryDirectory, err := os.MkdirTemp("", "medalert-acceptance-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(temporaryDirectory)
	executablePath = filepath.Join(temporaryDirectory, "medalert")
	packagePath := "github.com/poppyseedcake/MedAlert/internal/buildinfo.Commit=" + testCommit
	command := exec.Command("go", "build", "-trimpath", "-ldflags", "-X "+packagePath, "-o", executablePath, ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		panic(fmt.Sprintf("build acceptance executable: %v\n%s", buildErr, output))
	}
	os.Exit(m.Run())
}

func TestVersionReportsSourceCommitInTextAndJSON(t *testing.T) {
	textResult := run(t, nil, "version")
	if textResult.exitCode != 0 || textResult.stderr != "" {
		t.Fatalf("text result = %#v", textResult)
	}
	if textResult.stdout != "medalert "+testCommit+"\n" {
		t.Fatalf("text output = %q", textResult.stdout)
	}

	jsonResult := run(t, nil, "version", "--output", "json")
	if jsonResult.exitCode != 0 || jsonResult.stderr != "" {
		t.Fatalf("JSON result = %#v", jsonResult)
	}
	var envelope struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Commit string `json:"commit"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonResult.stdout), &envelope); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if envelope.SchemaVersion != 1 || envelope.Command != "version" || envelope.Data.Commit != testCommit {
		t.Fatalf("JSON envelope = %#v", envelope)
	}
}

func TestDatabaseInitializeUsesXDGLocationAndPrivatePermissions(t *testing.T) {
	dataHome := t.TempDir()
	result := run(t, []string{"XDG_DATA_HOME=" + dataHome}, "database", "initialize")
	if result.exitCode != 0 || result.stderr != "" {
		t.Fatalf("result = %#v", result)
	}
	if result.stdout != "Initialized database schema 1.\n" {
		t.Fatalf("stdout = %q", result.stdout)
	}
	assertProcessFileMode(t, filepath.Join(dataHome, "medalert"), 0o700)
	assertProcessFileMode(t, filepath.Join(dataHome, "medalert", "medalert.db"), 0o600)
	assertProcessFileMode(t, filepath.Join(dataHome, "medalert", "backups"), 0o700)
}

func TestDatabaseInitializeIgnoresRelativeXDGDataHome(t *testing.T) {
	home := t.TempDir()
	result := run(t, []string{"XDG_DATA_HOME=relative", "HOME=" + home}, "database", "initialize")
	if result.exitCode != 0 || result.stderr != "" {
		t.Fatalf("result = %#v", result)
	}
	assertProcessFileMode(t, filepath.Join(home, ".local", "share", "medalert", "medalert.db"), 0o600)
}

func TestDatabaseInitializeMigratesWithBackupRetention(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessDatabase(t, databasePath, 0, "CREATE TABLE existing (value TEXT); INSERT INTO existing VALUES ('kept');")
	backupDirectory := filepath.Join(root, "backups")
	if err := os.Mkdir(backupDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 4 {
		path := filepath.Join(backupDirectory, fmt.Sprintf("medalert-schema-0-old-%d.sqlite3", index))
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result := run(t, nil, "database", "initialize", "--database", databasePath)
	if result.exitCode != 0 || result.stderr != "" {
		t.Fatalf("result = %#v", result)
	}
	backups, err := filepath.Glob(filepath.Join(backupDirectory, "medalert-schema-*.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 3 {
		t.Fatalf("backup count = %d, want 3", len(backups))
	}
	createdBackups, err := filepath.Glob(filepath.Join(backupDirectory, "medalert-schema-0-"+testCommit+"-*.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(createdBackups) != 1 {
		t.Fatalf("created backup count = %d, want 1: %v", len(createdBackups), createdBackups)
	}
	assertProcessFileMode(t, createdBackups[0], 0o600)
	assertProcessQueryValue(t, createdBackups[0], "SELECT value FROM existing", "kept")
	assertProcessQueryValue(t, createdBackups[0], "PRAGMA user_version", "0")
	assertProcessQueryValue(t, databasePath, "SELECT value FROM existing", "kept")
}

func TestDatabaseInitializeFailureLeavesPriorDatabaseUsable(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessDatabase(t, databasePath, 0, `
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL,
			required_value TEXT NOT NULL
		);
	`)

	result := run(t, nil, "database", "initialize", "--database", databasePath)
	if result.exitCode != 2 || result.stdout != "" || result.stderr == "" {
		t.Fatalf("result = %#v", result)
	}
	assertProcessQueryValue(t, databasePath, "SELECT count(*) FROM schema_migrations", "0")
	assertProcessQueryValue(t, databasePath, "PRAGMA user_version", "0")
	assertProcessQueryValue(t, databasePath, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'application_metadata'", "0")
}

func TestDoctorReportsRequiredMigrationWithoutChangingDatabase(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "missing.db")
	result := run(t, nil, "doctor", "--database", databasePath, "--output", "json")
	if result.exitCode != 0 || result.stderr != "" {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("database exists after doctor: %v", err)
	}
	if !strings.Contains(result.stdout, `"required_schema_version":1`) || !strings.Contains(result.stdout, `"migration_required":true`) {
		t.Fatalf("stdout = %q", result.stdout)
	}
}

func TestNewerSchemaReturnsConfigurationErrorWithoutChangingDatabase(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "future.db")
	initialize := run(t, nil, "database", "initialize", "--database", databasePath)
	if initialize.exitCode != 0 {
		t.Fatalf("initialize = %#v", initialize)
	}
	setUserVersion(t, databasePath, 2)
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	result := run(t, nil, "database", "initialize", "--database", databasePath, "--output", "json")
	if result.exitCode != 2 || result.stdout != "" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.stderr, `"code":"unsupported_schema"`) {
		t.Fatalf("stderr = %q", result.stderr)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("newer database changed")
	}
}

type processResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func run(t *testing.T, environment []string, arguments ...string) processResult {
	t.Helper()
	command := exec.Command(executablePath, arguments...)
	command.Env = append(os.Environ(), environment...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := processResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run command: %v", err)
	}
	result.exitCode = exitError.ExitCode()
	return result
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func createProcessDatabase(t *testing.T, path string, version int, statements string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(statements); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertProcessQueryValue(t *testing.T, path, query, want string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var got string
	if err := database.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query value = %q, want %q", got, want)
	}
}

func assertProcessFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}
}
