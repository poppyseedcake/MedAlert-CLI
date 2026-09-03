// Package secrets owns protected password access.
//
// SQLite stores account configuration and secret references. It never stores
// passwords or secret values. This module reads passwords from Secret Service,
// safe mounted files, or temporary hidden input. It never writes secret values
// to logs, errors, text, JSON, or history.
package secrets

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	// ServiceName groups MedAlert entries inside Secret Service.
	ServiceName = "medalert"
	// SourceSecretService keeps the password in Secret Service.
	SourceSecretService = "secret-service"
	// SourceFile reads the password from a mounted secret file.
	SourceFile = "file"
	// SourcePrompt asks with hidden input and never stores the value.
	SourcePrompt = "prompt"

	maxSecretBytes = 64 * 1024
)

var (
	// ErrMissingInput is returned when a non-interactive command needs a secret
	// instead of opening a prompt.
	ErrMissingInput = errors.New("missing required input: rerun interactively or provide a secret file")
	// ErrSecretNotFound is returned when a secret reference has no value.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretUnsafe is returned for mounted files with unsafe permissions or links.
	ErrSecretUnsafe = errors.New("secret file is not safe")
)

// ServiceKey derives the Secret Service entry for one account id.
func ServiceKey(accountID string) string {
	return "account:" + accountID
}

// test seams (private, per product decisions)
var (
	isTerminalFunc   = term.IsTerminal
	readPasswordFunc = term.ReadPassword
)

// SetPassword saves a password to Secret Service. value never appears in errors.
func SetPassword(accountID, value string) error {
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required for secret storage")
	}
	if value == "" {
		return errors.New("secret value is empty")
	}
	if len(value) > maxSecretBytes {
		return errors.New("secret value is too large")
	}
	if err := keyring.Set(ServiceName, ServiceKey(accountID), value); err != nil {
		return fmt.Errorf("save account secret for %q: %w", accountID, secretKind(err))
	}
	return nil
}

// GetPassword reads a password from Secret Service. The value never appears in errors.
func GetPassword(accountID string) (string, error) {
	if strings.TrimSpace(accountID) == "" {
		return "", errors.New("account id is required for secret lookup")
	}
	value, err := keyring.Get(ServiceName, ServiceKey(accountID))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("secret for account %q: %w", accountID, ErrSecretNotFound)
		}
		return "", fmt.Errorf("read account secret for %q: %w", accountID, secretKind(err))
	}
	if value == "" {
		return "", fmt.Errorf("secret for account %q: %w", accountID, ErrSecretNotFound)
	}
	return value, nil
}

// DeletePassword removes a Secret Service entry. Missing entries are not an error.
func DeletePassword(accountID string) error {
	if strings.TrimSpace(accountID) == "" {
		return errors.New("account id is required for secret removal")
	}
	if err := keyring.Delete(ServiceName, ServiceKey(accountID)); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("remove account secret for %q: %w", accountID, secretKind(err))
	}
	return nil
}

func secretKind(err error) error {
	// Never include secret values; only classify the failure.
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrSecretNotFound
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || strings.Contains(msg, "missing") {
		return ErrSecretNotFound
	}
	return errors.New("secret service is unavailable")
}

// ReadSecretFile reads a mounted secret file. It rejects symbolic links,
// non-regular files, and files readable by group or others. The file is
// opened with O_NOFOLLOW and O_NONBLOCK, then validated and read through the
// same open descriptor: O_NOFOLLOW rejects a trailing symlink without a
// second path lookup, and O_NONBLOCK keeps the open from blocking on a FIFO
// before the regular-file check can reject it.
func ReadSecretFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("read secret file: %w", ErrSecretNotFound)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return "", fmt.Errorf("read secret file %q: %w", path, ErrSecretNotFound)
		}
		if errors.Is(err, unix.ELOOP) {
			return "", fmt.Errorf("read secret file %q: %w: symbolic links are not permitted", path, ErrSecretUnsafe)
		}
		return "", fmt.Errorf("read secret file %q: %w", path, ErrSecretUnsafe)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", path, ErrSecretUnsafe)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read secret file %q: %w: not a regular file", path, ErrSecretUnsafe)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("read secret file %q: %w: permissions must not allow group or others (got %04o)", path, ErrSecretUnsafe, info.Mode().Perm())
	}
	if info.Size() <= 0 || info.Size() > maxSecretBytes {
		return "", fmt.Errorf("read secret file %q: secret is empty or too large", path)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", path, ErrSecretUnsafe)
	}
	value := strings.TrimSuffix(string(contents), "\n")
	value = strings.TrimSuffix(value, "\r")
	// Clear the raw buffer reference as soon as possible; strings are immutable
	// so this only drops the extra copy, but it documents the intent.
	for i := range contents {
		contents[i] = 0
	}
	if value == "" {
		return "", fmt.Errorf("read secret file %q: secret is empty", path)
	}
	if len(value) > maxSecretBytes {
		return "", fmt.Errorf("read secret file %q: secret is too large", path)
	}
	return value, nil
}

// PromptForPassword reads hidden input. In non-interactive mode it fails
// instead of blocking. The value never appears in errors or output.
func PromptForPassword(prompt string, stdin *os.File, stderr io.Writer, nonInteractive bool) (string, error) {
	if nonInteractive {
		return "", ErrMissingInput
	}
	if stdin == nil || stderr == nil {
		return "", ErrMissingInput
	}
	if !isTerminalFunc(int(stdin.Fd())) {
		return "", ErrMissingInput
	}
	if prompt == "" {
		prompt = "Password: "
	}
	if _, err := fmt.Fprint(stderr, prompt); err != nil {
		return "", ErrMissingInput
	}
	raw, err := readPasswordFunc(int(stdin.Fd()))
	if _, _ = fmt.Fprint(stderr, "\n"); err != nil {
		// Do not include terminal details that could confuse automation; keep
		// the contract stable: missing input instead of a prompt.
		_ = err
		return "", ErrMissingInput
	}
	value := string(raw)
	for i := range raw {
		raw[i] = 0
	}
	if value == "" {
		return "", ErrMissingInput
	}
	return value, nil
}

// Resolve returns the password for an account without ever logging the value.
// source is one of secret-service, file, prompt. ref is the file path for
// file sources and is ignored otherwise. accountID scopes Secret Service keys.
func Resolve(source, ref, accountID string, stdin *os.File, stderr io.Writer, nonInteractive bool) (string, error) {
	switch source {
	case SourceFile:
		return ReadSecretFile(ref)
	case SourceSecretService:
		return GetPassword(accountID)
	case SourcePrompt:
		return PromptForPassword("Password: ", stdin, stderr, nonInteractive)
	default:
		return "", fmt.Errorf("unknown password source %q", source)
	}
}
