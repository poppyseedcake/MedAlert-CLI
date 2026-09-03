// Package session owns Medicover Session State persistence.
//
// Session state is separate for each Medicover Account. It contains cookies,
// a refresh token, and trusted-device data. It never contains the short-lived
// access token, passwords, or MFA codes.
//
// Native Linux stores session state in Secret Service. Container-style use
// stores it atomically in a protected mutable file. The first Go version
// never imports, edits, or deletes MediCzuwacz session files.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/zalando/go-keyring"
	"golang.org/x/sys/unix"
)

const (
	serviceName    = "medalert"
	maxSessionSize = 256 * 1024
)

var (
	// ErrNotFound is returned when an account has no saved session.
	ErrNotFound = errors.New("no saved session for account")
	// ErrCorrupt is returned when saved session state cannot be decoded.
	ErrCorrupt = errors.New("saved session is corrupt")
	// ErrUnsafe is returned for session files with unsafe permissions or links.
	ErrUnsafe = errors.New("session file is not safe")
)

// Store persists per-account session state. Implementations keep accounts
// independent: one account never changes another account's state.
type Store interface {
	Load(accountID string) (*medicover.SessionState, error)
	Save(accountID string, state *medicover.SessionState) error
	Delete(accountID string) error
}

// ServiceKey derives the Secret Service entry for one account session. It is
// separate from the password entry (account:<id>).
func ServiceKey(accountID string) string {
	return "session:" + accountID
}

// SecretServiceStore keeps session state in Secret Service (native Linux).
type SecretServiceStore struct{}

func (SecretServiceStore) Load(accountID string) (*medicover.SessionState, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, errors.New("account id is required for session lookup")
	}
	raw, err := keyring.Get(serviceName, ServiceKey(accountID))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, fmt.Errorf("session for account %q: %w", accountID, ErrNotFound)
		}
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "not found") || strings.Contains(message, "no such") || strings.Contains(message, "missing") {
			return nil, fmt.Errorf("session for account %q: %w", accountID, ErrNotFound)
		}
		return nil, fmt.Errorf("read session for account %q: secret service is unavailable", accountID)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("session for account %q: %w", accountID, ErrNotFound)
	}
	var state medicover.SessionState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("session for account %q: %w", accountID, ErrCorrupt)
	}
	if err := validateState(&state); err != nil {
		return nil, fmt.Errorf("session for account %q: %w", accountID, ErrCorrupt)
	}
	return &state, nil
}

func (SecretServiceStore) Save(accountID string, state *medicover.SessionState) error {
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required for session storage")
	}
	if err := validateState(state); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode session for account %q: secret service is unavailable", accountID)
	}
	if len(raw) > maxSessionSize {
		return fmt.Errorf("encode session for account %q: session is too large", accountID)
	}
	if err := keyring.Set(serviceName, ServiceKey(accountID), string(raw)); err != nil {
		return fmt.Errorf("save session for account %q: secret service is unavailable", accountID)
	}
	return nil
}

func (SecretServiceStore) Delete(accountID string) error {
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required for session removal")
	}
	if err := keyring.Delete(serviceName, ServiceKey(accountID)); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "not found") || strings.Contains(message, "no such") || strings.Contains(message, "missing") {
			return nil
		}
		return fmt.Errorf("remove session for account %q: secret service is unavailable", accountID)
	}
	return nil
}

// FileStore keeps one protected JSON file per account:
// <dir>/sessions/<account-id>.json with mode 0600 inside a 0700 directory.
// Writes are atomic (temp file + rename) and never follow symlinks.
type FileStore struct {
	Dir string
}

func (s FileStore) sessionPath(accountID string) (string, error) {
	if strings.TrimSpace(accountID) == "" {
		return "", errors.New("account id is required for session storage")
	}
	// Account ids are already validated ([A-Za-z0-9_-], max 64), so the file
	// name cannot escape the sessions directory.
	sessions := filepath.Join(s.Dir, "sessions")
	return filepath.Join(sessions, accountID+".json"), nil
}

func (s FileStore) Load(accountID string) (*medicover.SessionState, error) {
	path, err := s.sessionPath(accountID)
	if err != nil {
		return nil, err
	}
	if err := openDirNoFollow(filepath.Join(s.Dir, "sessions")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session for account %q: %w", accountID, ErrNotFound)
		}
		return nil, fmt.Errorf("read session for account %q: %w", accountID, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session for account %q: %w", accountID, ErrNotFound)
		}
		return nil, fmt.Errorf("read session for account %q: %w", accountID, ErrUnsafe)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("read session for account %q: %w: symbolic links are not permitted", accountID, ErrUnsafe)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read session for account %q: %w: not a regular file", accountID, ErrUnsafe)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("read session for account %q: %w: permissions must be 0600 (got %04o)", accountID, ErrUnsafe, info.Mode().Perm())
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("session for account %q: %w", accountID, ErrNotFound)
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("read session for account %q: %w: symbolic links are not permitted", accountID, ErrUnsafe)
		}
		return nil, fmt.Errorf("read session for account %q: %w", accountID, ErrUnsafe)
	}
	file := os.NewFile(uintptr(fd), path)
	contents, readErr := readAllLimited(file, maxSessionSize+1)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read session for account %q: %w", accountID, ErrCorrupt)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("read session for account %q: %w", accountID, ErrUnsafe)
	}
	var state medicover.SessionState
	if err := json.Unmarshal(contents, &state); err != nil {
		return nil, fmt.Errorf("session for account %q: %w", accountID, ErrCorrupt)
	}
	if err := validateState(&state); err != nil {
		return nil, fmt.Errorf("session for account %q: %w", accountID, ErrCorrupt)
	}
	return &state, nil
}

func (s FileStore) Save(accountID string, state *medicover.SessionState) error {
	if err := validateState(state); err != nil {
		return err
	}
	sessions := filepath.Join(s.Dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", ErrUnsafe)
	}
	if err := ensurePrivateDir(s.Dir); err != nil {
		return err
	}
	if err := ensurePrivateDir(sessions); err != nil {
		return err
	}
	if err := openDirNoFollow(sessions); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	path, err := s.sessionPath(accountID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return errors.New("encode session: session is too large")
	}
	if len(raw) > maxSessionSize {
		return errors.New("encode session: session is too large")
	}
	temporary, err := os.CreateTemp(sessions, ".session-*")
	if err != nil {
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	tempName := temporary.Name()
	// Best effort cleanup; a successful rename removes the temp path.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tempName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	success = true
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	}
	// Verify the renamed file is a private regular file, not a symlink left
	// by a replacement between the directory check and the rename.
	if info, err := os.Lstat(path); err != nil {
		return fmt.Errorf("save session for account %q: %w", accountID, ErrUnsafe)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = os.Remove(path)
		return fmt.Errorf("save session for account %q: %w: session file was replaced", accountID, ErrUnsafe)
	}
	return nil
}

func (s FileStore) Delete(accountID string) error {
	path, err := s.sessionPath(accountID)
	if err != nil {
		return err
	}
	if err := openDirNoFollow(filepath.Join(s.Dir, "sessions")); err != nil {
		// A missing sessions directory means there is nothing to delete.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove session for account %q: %w", accountID, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove session for account %q: %w", accountID, ErrUnsafe)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Never follow a symlink on delete: remove the link itself so a
		// planted link cannot delete an unrelated file.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove session for account %q: %w", accountID, ErrUnsafe)
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session for account %q: %w", accountID, ErrUnsafe)
	}
	return nil
}

// openDirNoFollow verifies path is a real directory without following a
// trailing symlink. It closes the descriptor immediately; callers re-check
// the final file with O_NOFOLLOW, so this only narrows the replacement
// window for the sessions directory itself.
func openDirNoFollow(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return os.ErrNotExist
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return ErrUnsafe
		}
		return ErrUnsafe
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrUnsafe
	}
	if !info.IsDir() {
		return ErrUnsafe
	}
	return nil
}

func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect session directory: %w", ErrUnsafe)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session directory must not be a symlink: %w", ErrUnsafe)
	}
	if !info.IsDir() {
		return fmt.Errorf("session path is not a directory: %w", ErrUnsafe)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("session directory permissions must be 0700 (got %04o): %w", info.Mode().Perm(), ErrUnsafe)
	}
	return nil
}

func validateState(state *medicover.SessionState) error {
	if state == nil {
		return errors.New("session state is required")
	}
	if strings.TrimSpace(state.DeviceID) == "" {
		return errors.New("session device id is required")
	}
	if len(state.DeviceID) > 256 {
		return errors.New("session device id is too long")
	}
	if len(state.RefreshToken) > 16*1024 {
		return errors.New("session refresh token is too large")
	}
	if len(state.Cookies) > 256 {
		return errors.New("session has too many cookies")
	}
	for _, cookie := range state.Cookies {
		if strings.TrimSpace(cookie.Name) == "" || len(cookie.Name) > 256 || len(cookie.Value) > 8*1024 {
			return errors.New("session cookie is invalid")
		}
	}
	return nil
}

func readAllLimited(file *os.File, limit int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnsafe
	}
	contents := make([]byte, 0, 4096)
	buffer := make([]byte, 4096)
	var total int64
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return nil, errors.New("session is too large")
			}
			contents = append(contents, buffer[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	return contents, nil
}
