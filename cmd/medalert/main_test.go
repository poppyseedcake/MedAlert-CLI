package main_test

import (
	"database/sql"
	"encoding/json"
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
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
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
	stdout, err := command.Output()
	result := processResult{stdout: string(stdout)}
	if err == nil {
		return result
	}
	var exitError *exec.ExitError
	if !strings.Contains(fmt.Sprintf("%T", err), "ExitError") {
		t.Fatalf("run command: %v", err)
	}
	exitError = err.(*exec.ExitError)
	result.stderr = string(exitError.Stderr)
	result.exitCode = exitError.ExitCode()
	return result
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
