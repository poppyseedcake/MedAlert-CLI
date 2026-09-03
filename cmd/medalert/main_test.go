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
	if result.stdout != "Initialized database schema 2.\n" {
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

func TestConcurrentDatabaseInitializeUsesCurrentSchemaAfterLock(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	createProcessDatabase(t, databasePath, 0, "CREATE TABLE existing (payload BLOB); INSERT INTO existing VALUES (zeroblob(16777216));")

	start := make(chan struct{})
	results := make(chan processResult, 2)
	for range 2 {
		go func() {
			<-start
			results <- run(t, nil, "database", "initialize", "--database", databasePath)
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.exitCode != 0 || result.stderr != "" {
			t.Errorf("concurrent result = %#v", result)
		}
	}
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
	if !strings.Contains(result.stdout, `"required_schema_version":2`) || !strings.Contains(result.stdout, `"migration_required":true`) {
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
	setUserVersion(t, databasePath, 3)
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

func writeProcessSecretFile(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAccountManageMultipleAccountsWithTextAndJSON(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	markerOne := "MARKER-ACCOUNT-ONE-" + t.Name() + "-unique"
	markerTwo := "MARKER-ACCOUNT-TWO-unique"
	fileOne := writeProcessSecretFile(t, secretDir, "one", markerOne)
	fileTwo := writeProcessSecretFile(t, secretDir, "two", markerTwo)

	createOne := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "alice", "--username", "alice@example.com", "--password-file", fileOne)
	if createOne.exitCode != 0 || createOne.stderr != "" {
		t.Fatalf("create alice = %#v", createOne)
	}
	createTwo := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "bob", "--username", "bob@example.com", "--password-file", fileTwo, "--output", "json")
	if createTwo.exitCode != 0 || createTwo.stderr != "" {
		t.Fatalf("create bob = %#v", createTwo)
	}
	if !strings.Contains(createTwo.stdout, `"command":"account create"`) || !strings.Contains(createTwo.stdout, `"schema_version":1`) {
		t.Fatalf("bob json = %q", createTwo.stdout)
	}

	listText := run(t, nil, "account", "list", "--database", databasePath)
	if listText.exitCode != 0 {
		t.Fatalf("list = %#v", listText)
	}
	if !strings.Contains(listText.stdout, "alice") || !strings.Contains(listText.stdout, "bob") {
		t.Fatalf("list stdout = %q", listText.stdout)
	}

	listJSON := run(t, nil, "account", "list", "--database", databasePath, "--output", "json")
	if listJSON.exitCode != 0 {
		t.Fatalf("list json = %#v", listJSON)
	}
	var listEnvelope struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Accounts []struct {
				ID             string `json:"id"`
				Username       string `json:"username"`
				PasswordSource string `json:"password_source"`
				PasswordRef    string `json:"password_ref"`
			} `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON.stdout), &listEnvelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listEnvelope.SchemaVersion != 1 || listEnvelope.Command != "account list" || len(listEnvelope.Data.Accounts) != 2 {
		t.Fatalf("list envelope = %#v", listEnvelope)
	}

	show := run(t, nil, "account", "show", "--database", databasePath, "--account", "alice", "--output", "json")
	if show.exitCode != 0 {
		t.Fatalf("show = %#v", show)
	}

	edit := run(t, nil, "account", "edit", "--database", databasePath, "--non-interactive", "--account", "alice", "--username", "alice2@example.com")
	if edit.exitCode != 0 || edit.stderr != "" {
		t.Fatalf("edit = %#v", edit)
	}
	assertProcessQueryValue(t, databasePath, "SELECT username FROM accounts WHERE id = 'alice'", "alice2@example.com")

	deleted := run(t, nil, "account", "delete", "--database", databasePath, "--account", "bob")
	if deleted.exitCode != 0 {
		t.Fatalf("delete = %#v", deleted)
	}
	afterDelete := run(t, nil, "account", "list", "--database", databasePath, "--output", "json")
	if !strings.Contains(afterDelete.stdout, "alice") || strings.Contains(afterDelete.stdout, `"id":"bob"`) {
		t.Fatalf("after delete = %q", afterDelete.stdout)
	}

	// Secret values must never appear in outputs, errors, or the database file.
	for _, output := range []string{createOne.stdout, createOne.stderr, createTwo.stdout, createTwo.stderr, listText.stdout, listText.stderr, listJSON.stdout, show.stdout, edit.stdout, edit.stderr, deleted.stdout, afterDelete.stdout} {
		if strings.Contains(output, markerOne) || strings.Contains(output, markerTwo) {
			t.Fatalf("output leaks secret: %q", output)
		}
	}
	raw, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), markerOne) || strings.Contains(string(raw), markerTwo) {
		t.Fatal("database file contains secret value")
	}
	assertProcessQueryValue(t, databasePath, "SELECT password_source FROM accounts WHERE id = 'alice'", "file")
	assertProcessQueryValue(t, databasePath, "SELECT password_ref FROM accounts WHERE id = 'alice'", fileOne)
}

func TestAccountRejectsUnsafeSecretFiles(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(secretDir, "open")
	if err := os.WriteFile(openPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := "MARKER-UNSAFE-unique"
	result := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "unsafe", "--username", "u@example.com", "--password-file", openPath)
	if result.exitCode != 2 || result.stdout != "" {
		t.Fatalf("unsafe result = %#v", result)
	}
	if strings.Contains(result.stderr, marker) {
		t.Fatal("error leaks secret")
	}

	target := filepath.Join(secretDir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(secretDir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	linkResult := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "linked", "--username", "u@example.com", "--password-file", link)
	if linkResult.exitCode != 2 {
		t.Fatalf("symlink result = %#v", linkResult)
	}
}

func TestAccountNonInteractiveRejectsMissingInput(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	result := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "nopass", "--username", "u@example.com")
	if result.exitCode != 2 || result.stdout != "" || result.stderr == "" {
		t.Fatalf("missing input result = %#v", result)
	}
	if !strings.Contains(strings.ToLower(result.stderr), "missing") {
		t.Fatalf("stderr = %q, want missing input", result.stderr)
	}
	jsonResult := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--output", "json", "--account", "nopass2", "--username", "u@example.com")
	if jsonResult.exitCode != 2 || !strings.Contains(jsonResult.stderr, `"code":"missing_input"`) {
		t.Fatalf("json missing = %#v", jsonResult)
	}
}

func TestAccountDuplicateCreateChecksExistenceBeforeSecrets(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := writeProcessSecretFile(t, secretDir, "valid", "valid-secret")
	openPath := filepath.Join(secretDir, "open")
	if err := os.WriteFile(openPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	created := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "dup", "--username", "u@example.com", "--password-file", valid)
	if created.exitCode != 0 {
		t.Fatalf("create = %#v", created)
	}
	// The id already exists and the replacement file is unsafe: existence must
	// win so the command reports account_exists without touching secrets.
	again := run(t, nil, "account", "create", "--database", databasePath, "--non-interactive", "--account", "dup", "--username", "other@example.com", "--password-file", openPath, "--output", "json")
	if again.exitCode != 2 || !strings.Contains(again.stderr, `"code":"account_exists"`) {
		t.Fatalf("duplicate result = %#v", again)
	}
	assertProcessQueryValue(t, databasePath, "SELECT username FROM accounts WHERE id = 'dup'", "u@example.com")
}

func TestAccountRejectsIrrelevantFlags(t *testing.T) {
	root := privateTempDir(t)
	databasePath := filepath.Join(root, "medalert.db")
	secretDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := writeProcessSecretFile(t, secretDir, "valid", "valid-secret")

	for _, args := range [][]string{
		{"doctor", "--database", databasePath, "--account", "alice"},
		{"version", "--username", "u@example.com"},
		{"database", "initialize", "--database", databasePath, "--password-prompt"},
		{"account", "list", "--database", databasePath, "--account", "alice"},
		{"account", "list", "--database", databasePath, "--username", "u@example.com"},
		{"account", "show", "--database", databasePath, "--account", "alice", "--username", "u@example.com"},
		{"account", "delete", "--database", databasePath, "--account", "alice", "--password-file", valid},
	} {
		result := run(t, nil, args...)
		if result.exitCode != 2 || result.stdout != "" || !strings.Contains(result.stderr, "not supported") {
			t.Fatalf("args %v result = %#v", args, result)
		}
	}
}
