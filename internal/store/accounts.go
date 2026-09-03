// Package store account operations own Medicover Account configuration.
// SQLite stores account configuration and secret references. It never stores
// passwords or other secret values.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// PasswordSourceSecretService keeps the password in the OS Secret Service.
	PasswordSourceSecretService = "secret-service"
	// PasswordSourceFile reads the password from a mounted secret file.
	PasswordSourceFile = "file"
	// PasswordSourcePrompt asks for the password with hidden input each time.
	PasswordSourcePrompt = "prompt"
)

var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Account describes one Medicover Account without any secret value.
type Account struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	PasswordSource string `json:"password_source"`
	PasswordRef    string `json:"password_ref"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// AccountUpdate carries optional edit fields. Nil means keep the current value.
type AccountUpdate struct {
	Username       *string
	PasswordSource *string
	PasswordRef    *string
}

var (
	// ErrAccountExists is returned when an account id is already taken.
	ErrAccountExists = errors.New("account already exists")
	// ErrAccountNotFound is returned when an account id does not exist.
	ErrAccountNotFound = errors.New("account not found")
	// ErrAccountInvalid is returned when account input fails validation.
	ErrAccountInvalid = errors.New("invalid account input")
)

// Open returns a Store handle for an initialized database file.
func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("database path is needed for account storage")
	}
	status, err := Inspect(path)
	if err != nil {
		return nil, err
	}
	if status.MigrationRequired {
		return nil, fmt.Errorf("database migration to schema %d is required", status.RequiredVersion)
	}
	database, err := openDatabase(path, false)
	if err != nil {
		return nil, err
	}
	return &Store{db: database}, nil
}

// Store owns account SQL. Callers never see SQL.
type Store struct {
	db *sql.DB
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateAccount stores a new account reference. Secret values must never reach this layer.
func (s *Store) CreateAccount(account Account) (Account, error) {
	if err := ValidateAccount(account); err != nil {
		return Account{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	account.CreatedAt = now
	account.UpdatedAt = now
	_, err := s.db.Exec(
		`INSERT INTO accounts (id, username, password_source, password_ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		account.ID, account.Username, account.PasswordSource, account.PasswordRef, account.CreatedAt, account.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "primary") {
			return Account{}, fmt.Errorf("%w: %s", ErrAccountExists, account.ID)
		}
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	return account, nil
}

// ListAccounts returns all accounts ordered by id.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT id, username, password_source, password_ref, created_at, updated_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	accounts := []Account{}
	for rows.Next() {
		var account Account
		if err := rows.Scan(&account.ID, &account.Username, &account.PasswordSource, &account.PasswordRef, &account.CreatedAt, &account.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return accounts, nil
}

// GetAccount returns one account by id.
func (s *Store) GetAccount(id string) (Account, error) {
	if !accountIDPattern.MatchString(id) {
		return Account{}, fmt.Errorf("%w: account id %q", ErrAccountInvalid, id)
	}
	var account Account
	err := s.db.QueryRow(
		`SELECT id, username, password_source, password_ref, created_at, updated_at FROM accounts WHERE id = ?`, id,
	).Scan(&account.ID, &account.Username, &account.PasswordSource, &account.PasswordRef, &account.CreatedAt, &account.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	if err != nil {
		return Account{}, fmt.Errorf("show account: %w", err)
	}
	return account, nil
}

// UpdateAccount changes username and/or secret reference. Secret values never reach this layer.
func (s *Store) UpdateAccount(id string, update AccountUpdate) (Account, error) {
	current, err := s.GetAccount(id)
	if err != nil {
		return Account{}, err
	}
	if update.Username != nil {
		current.Username = *update.Username
	}
	if update.PasswordSource != nil {
		current.PasswordSource = *update.PasswordSource
		// When the caller changes the source without an explicit ref, reset the
		// ref so stale file paths or service keys cannot linger.
		if update.PasswordRef == nil {
			current.PasswordRef = ""
		}
	}
	if update.PasswordRef != nil {
		current.PasswordRef = *update.PasswordRef
	}
	if err := ValidateAccount(current); err != nil {
		return Account{}, err
	}
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(
		`UPDATE accounts SET username = ?, password_source = ?, password_ref = ?, updated_at = ? WHERE id = ?`,
		current.Username, current.PasswordSource, current.PasswordRef, current.UpdatedAt, id,
	)
	if err != nil {
		return Account{}, fmt.Errorf("edit account: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Account{}, fmt.Errorf("edit account: %w", err)
	}
	if affected == 0 {
		return Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	return current, nil
}

// DeleteAccount removes one account row. Secret values live outside SQLite,
// so callers remove the Secret Service entry separately when needed.
func (s *Store) DeleteAccount(id string) error {
	if !accountIDPattern.MatchString(id) {
		return fmt.Errorf("%w: account id %q", ErrAccountInvalid, id)
	}
	result, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	return nil
}

// ValidateAccount checks id, username, source, and ref rules without touching secrets.
func ValidateAccount(account Account) error {
	if !accountIDPattern.MatchString(account.ID) {
		return fmt.Errorf("%w: account id must match [A-Za-z0-9_-], start alphanumerically, max 64", ErrAccountInvalid)
	}
	username := strings.TrimSpace(account.Username)
	if username == "" || len(username) > 256 || strings.ContainsRune(username, '\x00') {
		return fmt.Errorf("%w: username is required, max 256", ErrAccountInvalid)
	}
	switch account.PasswordSource {
	case PasswordSourceSecretService, PasswordSourcePrompt:
		if account.PasswordRef != "" && (strings.ContainsRune(account.PasswordRef, '\x00') || len(account.PasswordRef) > 1024) {
			return fmt.Errorf("%w: password reference is too long", ErrAccountInvalid)
		}
		// For managed sources the ref stays empty; the account id derives the
		// Secret Service key. A custom ref is allowed but never a secret value.
		if account.PasswordSource == PasswordSourcePrompt && account.PasswordRef != "" {
			return fmt.Errorf("%w: prompt accounts must not keep a password reference", ErrAccountInvalid)
		}
	case PasswordSourceFile:
		if strings.TrimSpace(account.PasswordRef) == "" || strings.ContainsRune(account.PasswordRef, '\x00') || len(account.PasswordRef) > 1024 {
			return fmt.Errorf("%w: password file path is required", ErrAccountInvalid)
		}
	default:
		return fmt.Errorf("%w: password source must be secret-service, file, or prompt", ErrAccountInvalid)
	}
	return nil
}
