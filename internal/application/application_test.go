package application_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/application"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"
)

func TestProfileCancellationDoesNotCommitBlockedCreate(t *testing.T) {
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
	if _, err := storage.CreateAccount(store.Account{
		ID: "alice", Username: "alice@example.com", PasswordSource: store.PasswordSourcePrompt,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	locker, err := openApplicationTestDatabase(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	lockConnection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lockConnection.Close()
	if _, err := lockConnection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type profileResult struct {
		profile store.Profile
		err     error
	}
	result := make(chan profileResult, 1)
	go func() {
		profile, profileErr := application.New(application.Config{Database: databasePath}).Profile(ctx, application.ProfileRequest{
			Action: "create",
			ID:     "blocked",
			Values: application.ProfileValues{
				AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132",
				SearchType: "Standard", CheckIntervalMinutes: "30", Enabled: true,
			},
		})
		result <- profileResult{profile: profile, err: profileErr}
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	if _, err := lockConnection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-result:
		if outcome.err == nil {
			t.Fatalf("cancelled profile create committed: %#v", outcome.profile)
		}
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("cancelled profile create error = %v, want context.Canceled", outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled profile create did not return")
	}

	storage, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.GetProfile("blocked"); !errors.Is(err, store.ErrProfileNotFound) {
		t.Fatalf("profile after cancellation = %v, want not found", err)
	}
}

func TestTelegramTestCancellationDoesNotRecordFailure(t *testing.T) {
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
	if _, err := storage.CreateDestination(store.Destination{
		ID: "phone", Name: "Phone", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = application.New(application.Config{Database: databasePath}).Telegram(ctx, application.TelegramRequest{
		Action: "test", ID: "phone",
	}, func(promptContext context.Context, _ string) (string, error) {
		cancel()
		return "", promptContext.Err()
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("telegram test cancellation error = %v, want context.Canceled", err)
	}

	storage, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	destination, err := storage.GetDestination("phone")
	if err != nil {
		t.Fatal(err)
	}
	if destination.LastTestStatus != "" || destination.LastTestAt != "" {
		t.Fatalf("cancelled telegram test recorded result: %#v", destination)
	}
}

func TestTelegramMetadataEditKeepsAnUnchangedFileReference(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(root, "telegram-token")
	if err := os.WriteFile(tokenFile, []byte("token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(store.Destination{
		ID: "phone", Name: "Phone", ChatID: "123", TokenSource: store.TokenSourceFile, TokenRef: tokenFile, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}

	updated, err := application.New(application.Config{Database: databasePath}).Telegram(context.Background(), application.TelegramRequest{
		Action: "edit", ID: "phone", Values: application.DestinationValues{Name: "Home phone", ChatID: "456", TokenFile: tokenFile},
	}, nil)
	if err != nil {
		t.Fatalf("metadata edit with unavailable unchanged file: %v", err)
	}
	if updated.Name != "Home phone" || updated.ChatID != "456" || updated.TokenSource != store.TokenSourceFile || updated.TokenRef != tokenFile {
		t.Fatalf("updated destination = %#v", updated)
	}
}

func TestTelegramCreateAcceptsAnExplicitTokenSelection(t *testing.T) {
	keyring.MockInit()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatal(err)
	}

	created, err := application.New(application.Config{Database: databasePath}).Telegram(context.Background(), application.TelegramRequest{
		Action: "create", ID: "phone",
		Values:         application.DestinationValues{Name: "Phone", ChatID: "123"},
		TokenSelection: &application.TelegramTokenSelection{Source: store.TokenSourcePrompt},
	}, nil)
	if err != nil {
		t.Fatalf("create with explicit prompt source: %v", err)
	}
	if created.TokenSource != store.TokenSourcePrompt || created.TokenRef != "" {
		t.Fatalf("created destination = %#v", created)
	}
	if _, err := secrets.GetTelegramToken("phone"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("prompt source secret lookup = %v, want not found", err)
	}
}

func TestTelegramEditMovesSecretServiceTokenToPromptSource(t *testing.T) {
	keyring.MockInit()
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
	if _, err := storage.CreateDestination(store.Destination{
		ID: "phone", Name: "Phone", ChatID: "123", TokenSource: store.TokenSourceSecretService, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secrets.SetTelegramToken("phone", "old-token"); err != nil {
		t.Fatal(err)
	}

	updated, err := application.New(application.Config{Database: databasePath}).Telegram(context.Background(), application.TelegramRequest{
		Action: "edit", ID: "phone",
		TokenSelection: &application.TelegramTokenSelection{Source: store.TokenSourcePrompt},
	}, nil)
	if err != nil {
		t.Fatalf("edit to prompt source: %v", err)
	}
	if updated.TokenSource != store.TokenSourcePrompt || updated.TokenRef != "" {
		t.Fatalf("updated destination = %#v", updated)
	}
	if _, err := secrets.GetTelegramToken("phone"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("old secret after source change = %v, want not found", err)
	}
}

func TestTelegramEditStoresASelectedSecretServiceToken(t *testing.T) {
	keyring.MockInit()
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
	if _, err := storage.CreateDestination(store.Destination{
		ID: "phone", Name: "Phone", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true,
	}); err != nil {
		storage.Close()
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}

	updated, err := application.New(application.Config{Database: databasePath}).Telegram(context.Background(), application.TelegramRequest{
		Action: "edit", ID: "phone",
		TokenSelection: &application.TelegramTokenSelection{Source: store.TokenSourceSecretService},
	}, func(context.Context, string) (string, error) {
		return "new-token", nil
	})
	if err != nil {
		t.Fatalf("edit to secret service: %v", err)
	}
	if updated.TokenSource != store.TokenSourceSecretService || updated.TokenRef != "" {
		t.Fatalf("updated destination = %#v", updated)
	}
	stored, err := secrets.GetTelegramToken("phone")
	if err != nil {
		t.Fatalf("stored token: %v", err)
	}
	if stored != "new-token" {
		t.Fatalf("stored token = %q, want new token", stored)
	}
}

func openApplicationTestDatabase(path string) (*sql.DB, error) {
	location := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_busy_timeout=5000"}
	database, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	return database, nil
}
