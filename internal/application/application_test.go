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
	"github.com/poppyseedcake/MedAlert/internal/store"
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

func openApplicationTestDatabase(path string) (*sql.DB, error) {
	location := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_busy_timeout=5000"}
	database, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	return database, nil
}
