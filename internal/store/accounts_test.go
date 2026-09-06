package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func TestAccountsCRUDKeepsSecretValuesOut(t *testing.T) {
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
	defer storage.Close()

	created, err := storage.CreateAccount(store.Account{
		ID:             "alice",
		Username:       "alice@example.com",
		PasswordSource: store.PasswordSourceFile,
		PasswordRef:    "/run/secrets/alice",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != "alice" || created.PasswordSource != "file" {
		t.Fatalf("created = %#v", created)
	}

	second, err := storage.CreateAccount(store.Account{
		ID:             "bob",
		Username:       "bob@example.com",
		PasswordSource: store.PasswordSourcePrompt,
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if second.PasswordRef != "" {
		t.Fatalf("prompt ref = %q, want empty", second.PasswordRef)
	}

	accounts, err := storage.ListAccounts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 2 || accounts[0].ID != "alice" || accounts[1].ID != "bob" {
		t.Fatalf("list = %#v", accounts)
	}

	shown, err := storage.GetAccount("alice")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if shown.Username != "alice@example.com" {
		t.Fatalf("shown = %#v", shown)
	}

	updated, err := storage.UpdateAccount("alice", store.AccountUpdate{
		Username: strPtr("alice2@example.com"),
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if updated.Username != "alice2@example.com" {
		t.Fatalf("updated = %#v", updated)
	}

	if err := storage.DeleteAccount("bob"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := storage.GetAccount("bob"); err == nil {
		t.Fatal("deleted account still readable")
	}
}

func TestCreateDuplicateAccountFails(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	account := store.Account{ID: "dup", Username: "u", PasswordSource: store.PasswordSourcePrompt}
	if _, err := storage.CreateAccount(account); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateAccount(account); err == nil {
		t.Fatal("duplicate create succeeded")
	}
}

func TestAccountContextCancellationDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.CreateAccount(store.Account{
		ID:             "alice",
		Username:       "alice@example.com",
		PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.CreateAccountContext(ctx, store.Account{
		ID:             "bob",
		Username:       "bob@example.com",
		PasswordSource: store.PasswordSourcePrompt,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create error = %v, want context.Canceled", err)
	}
	if _, err := storage.UpdateAccountContext(ctx, "alice", store.AccountUpdate{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled update error = %v, want context.Canceled", err)
	}
	if err := storage.DeleteAccountContext(ctx, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delete error = %v, want context.Canceled", err)
	}
	if _, err := storage.GetAccount("alice"); err != nil {
		t.Fatalf("account after cancellation = %v, want present", err)
	}
	if _, err := storage.GetAccount("bob"); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("created account after cancellation = %v, want not found", err)
	}
}

func TestAccountValidationRejectsSecretsInRef(t *testing.T) {
	for _, account := range []store.Account{
		{ID: "", Username: "u", PasswordSource: store.PasswordSourcePrompt},
		{ID: "bad id!", Username: "u", PasswordSource: store.PasswordSourcePrompt},
		{ID: "ok", Username: "", PasswordSource: store.PasswordSourcePrompt},
		{ID: "ok", Username: "u", PasswordSource: "env"},
		{ID: "ok", Username: "u", PasswordSource: store.PasswordSourceFile, PasswordRef: ""},
		{ID: "ok", Username: "u", PasswordSource: store.PasswordSourcePrompt, PasswordRef: "/should/be/empty"},
	} {
		if err := store.ValidateAccount(account); err == nil {
			t.Errorf("validation passed for %#v", account)
		}
	}
}

func TestMigrateV1ToV2KeepsData(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, 1, `
		CREATE TABLE application_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
		CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z');
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
	if _, err := storage.CreateAccount(store.Account{ID: "migrated", Username: "m", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		t.Fatalf("create after migrate: %v", err)
	}
}

func strPtr(value string) *string { return &value }
