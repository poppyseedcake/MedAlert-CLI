package operatorstatus_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/operatorstatus"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

func statusFixture(t *testing.T) (*store.Store, session.FileStore, time.Time) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(path); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	if _, err := storage.CreateAccount(store.Account{ID: "alice", Username: "alice@example.com", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		t.Fatal(err)
	}
	return storage, session.FileStore{Dir: filepath.Join(root, "sessions")}, time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
}

func TestReadRequiresLoginForActiveAuthIncidentWithUsableCookies(t *testing.T) {
	storage, backend, now := statusFixture(t)
	if err := backend.Save("alice", &medicover.SessionState{
		DeviceID: "device", Cookies: []medicover.StoredCookie{{
			Name: "MedicoverTrusted", Value: "test-cookie", Domain: "login.example.test", Path: "/", Secure: true,
			Expires: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := operatorstatus.Read(storage, backend, "https://login.example.test", now)
	if err != nil || len(before.Accounts) != 1 || !before.Accounts[0].Authenticated || len(before.RequiredActions) != 0 {
		t.Fatalf("before incident = %+v, err = %v", before, err)
	}
	if _, _, _, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, "alice", "alice", "", "", "authentication_required", "login required", now, nil); err != nil {
		t.Fatal(err)
	}
	after, err := operatorstatus.Read(storage, backend, "https://login.example.test", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Accounts) != 1 || after.Accounts[0].Authenticated || !after.Accounts[0].AuthRequired {
		t.Fatalf("after incident = %+v, want login required", after.Accounts)
	}
	logins := 0
	for _, action := range after.RequiredActions {
		if action.Code == "authentication_required" && action.Scope == "account" && action.ID == "alice" {
			logins++
		}
	}
	if logins != 1 {
		t.Fatalf("actions = %+v, want exactly one login action", after.RequiredActions)
	}
}

// Session failures are supplied at the external session-store interface.
type savedSession struct {
	state *medicover.SessionState
	err   error
}

func (s savedSession) Load(string) (*medicover.SessionState, error) { return s.state, s.err }
func (savedSession) Save(string, *medicover.SessionState) error {
	panic("status must not save a session")
}
func (savedSession) Delete(string) error { panic("status must not delete a session") }

func TestReadDistinguishesMissingSessionFromUnavailableStorage(t *testing.T) {
	storage, _, now := statusFixture(t)
	for _, tc := range []struct {
		name              string
		backend           savedSession
		wantAuthenticated bool
		wantAuthRequired  bool
		wantAction        string
	}{
		{name: "missing", backend: savedSession{err: session.ErrNotFound}, wantAuthRequired: true, wantAction: "authentication_required"},
		{name: "corrupt", backend: savedSession{err: session.ErrCorrupt}, wantAuthRequired: true, wantAction: "authentication_required"},
		{name: "unsafe", backend: savedSession{err: session.ErrUnsafe}, wantAction: "session_unavailable"},
		{name: "unavailable", backend: savedSession{err: errors.New("session storage offline")}, wantAction: "session_unavailable"},
		{name: "expired", backend: savedSession{state: &medicover.SessionState{Cookies: []medicover.StoredCookie{{Name: "MedicoverTrusted", Value: "cookie", Expires: now.Add(-time.Second).Format(time.RFC3339)}}}}, wantAuthRequired: true, wantAction: "authentication_required"},
		{name: "wrong issuer", backend: savedSession{state: &medicover.SessionState{Cookies: []medicover.StoredCookie{{Name: "MedicoverTrusted", Value: "cookie", Domain: "other.example.test"}}}}, wantAuthRequired: true, wantAction: "authentication_required"},
		{name: "usable", backend: savedSession{state: &medicover.SessionState{Cookies: []medicover.StoredCookie{{Name: "MedicoverTrusted", Value: "cookie", Domain: "login.example.test"}}}}, wantAuthenticated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := operatorstatus.Read(storage, tc.backend, "https://login.example.test", now)
			if err != nil {
				t.Fatal(err)
			}
			if len(status.Accounts) != 1 || status.Accounts[0].Authenticated != tc.wantAuthenticated || status.Accounts[0].AuthRequired != tc.wantAuthRequired {
				t.Fatalf("accounts = %+v", status.Accounts)
			}
			if tc.wantAction == "" {
				if len(status.RequiredActions) != 0 {
					t.Fatalf("unexpected actions: %+v", status.RequiredActions)
				}
			} else if len(status.RequiredActions) != 1 || status.RequiredActions[0] != (operatorstatus.Action{Code: tc.wantAction, Scope: "account", ID: "alice"}) {
				t.Fatalf("actions = %+v, want %s for alice", status.RequiredActions, tc.wantAction)
			}
		})
	}
}
