// Package store telegram operations own Telegram Destination configuration.
//
// SQLite stores destination configuration and secret references. It never
// stores bot tokens or other secret values. One Observation Profile can link
// to more than one Telegram Destination through profile_telegram_destinations.
package store

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// TokenSourceSecretService keeps the bot token in the OS Secret Service.
	TokenSourceSecretService = "secret-service"
	// TokenSourceFile reads the bot token from a mounted secret file.
	TokenSourceFile = "file"
	// TokenSourcePrompt asks for the bot token with hidden input each time.
	TokenSourcePrompt = "prompt"
)

var destinationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

var chatIDPattern = regexp.MustCompile(`^-?\d{1,19}$`)

// Destination describes one Telegram Destination without any secret value.
// TokenRef is the file path for file sources and is empty otherwise.
type Destination struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ChatID         string `json:"chat_id"`
	TokenSource    string `json:"token_source"`
	TokenRef       string `json:"token_ref"`
	Enabled        bool   `json:"enabled"`
	LastTestAt     string `json:"last_test_at,omitempty"`
	LastTestStatus string `json:"last_test_status,omitempty"`
	LastTestError  string `json:"last_test_error,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// DestinationUpdate carries optional edit fields. Nil means keep the current value.
type DestinationUpdate struct {
	Name        *string
	ChatID      *string
	TokenSource *string
	TokenRef    *string
}

var (
	// ErrDestinationExists is returned when a destination id is already taken.
	ErrDestinationExists = errors.New("telegram destination already exists")
	// ErrDestinationNotFound is returned when a destination id does not exist.
	ErrDestinationNotFound = errors.New("telegram destination not found")
	// ErrDestinationInvalid is returned when destination input fails validation.
	ErrDestinationInvalid = errors.New("invalid telegram destination input")
)

// CreateDestination stores a new Telegram Destination reference. Secret values
// must never reach this layer.
func (s *Store) CreateDestination(destination Destination) (Destination, error) {
	if err := ValidateDestination(destination); err != nil {
		return Destination{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	destination.CreatedAt = now
	destination.UpdatedAt = now
	enabled := 0
	if destination.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO telegram_destinations (id, name, chat_id, token_source, token_ref, enabled, last_test_at, last_test_status, last_test_error, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		destination.ID, destination.Name, destination.ChatID, destination.TokenSource, destination.TokenRef, enabled, destination.LastTestAt, destination.LastTestStatus, destination.LastTestError, destination.CreatedAt, destination.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "primary") {
			return Destination{}, fmt.Errorf("%w: %s", ErrDestinationExists, destination.ID)
		}
		return Destination{}, fmt.Errorf("create telegram destination: %w", err)
	}
	return destination, nil
}

// ListDestinations returns all destinations ordered by id.
func (s *Store) ListDestinations() ([]Destination, error) {
	rows, err := s.db.Query(`SELECT id, name, chat_id, token_source, token_ref, enabled, last_test_at, last_test_status, last_test_error, created_at, updated_at FROM telegram_destinations ORDER BY id`)
	if err != nil {
		// Older databases without the table report no destinations.
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return []Destination{}, nil
		}
		return nil, fmt.Errorf("list telegram destinations: %w", err)
	}
	defer rows.Close()
	destinations := []Destination{}
	for rows.Next() {
		var destination Destination
		var enabled int
		if err := rows.Scan(&destination.ID, &destination.Name, &destination.ChatID, &destination.TokenSource, &destination.TokenRef, &enabled, &destination.LastTestAt, &destination.LastTestStatus, &destination.LastTestError, &destination.CreatedAt, &destination.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list telegram destinations: %w", err)
		}
		destination.Enabled = enabled == 1
		destinations = append(destinations, destination)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list telegram destinations: %w", err)
	}
	return destinations, nil
}

// GetDestination returns one destination by its stable id.
func (s *Store) GetDestination(id string) (Destination, error) {
	if !destinationIDPattern.MatchString(id) {
		return Destination{}, fmt.Errorf("%w: destination id %q", ErrDestinationInvalid, id)
	}
	var destination Destination
	var enabled int
	err := s.db.QueryRow(
		`SELECT id, name, chat_id, token_source, token_ref, enabled, last_test_at, last_test_status, last_test_error, created_at, updated_at FROM telegram_destinations WHERE id = ?`, id,
	).Scan(&destination.ID, &destination.Name, &destination.ChatID, &destination.TokenSource, &destination.TokenRef, &enabled, &destination.LastTestAt, &destination.LastTestStatus, &destination.LastTestError, &destination.CreatedAt, &destination.UpdatedAt)
	if err != nil {
		if isNoSuchTable(err) {
			return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
		}
		// modernc sqlite returns generic error for missing table on QueryRow scan?
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
		}
		if isNoRows(err) {
			return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
		}
		return Destination{}, fmt.Errorf("show telegram destination: %w", err)
	}
	destination.Enabled = enabled == 1
	return destination, nil
}

// UpdateDestination changes name, chat, and/or token reference. Secret values
// never reach this layer. Enabled state is managed by SetDestinationEnabled.
func (s *Store) UpdateDestination(id string, update DestinationUpdate) (Destination, error) {
	current, err := s.GetDestination(id)
	if err != nil {
		return Destination{}, err
	}
	if update.Name != nil {
		current.Name = *update.Name
	}
	if update.ChatID != nil {
		current.ChatID = *update.ChatID
	}
	if update.TokenSource != nil {
		current.TokenSource = *update.TokenSource
		if update.TokenRef == nil {
			current.TokenRef = ""
		}
	}
	if update.TokenRef != nil {
		current.TokenRef = *update.TokenRef
	}
	if err := ValidateDestination(current); err != nil {
		return Destination{}, err
	}
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	value := 0
	if current.Enabled {
		value = 1
	}
	result, err := s.db.Exec(
		`UPDATE telegram_destinations SET name = ?, chat_id = ?, token_source = ?, token_ref = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		current.Name, current.ChatID, current.TokenSource, current.TokenRef, value, current.UpdatedAt, id,
	)
	if err != nil {
		return Destination{}, fmt.Errorf("edit telegram destination: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Destination{}, fmt.Errorf("edit telegram destination: %w", err)
	}
	if affected == 0 {
		return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
	}
	return current, nil
}

// SetDestinationEnabled pauses (false) or resumes (true) delivery without
// deleting the destination row.
func (s *Store) SetDestinationEnabled(id string, enabled bool) (Destination, error) {
	current, err := s.GetDestination(id)
	if err != nil {
		return Destination{}, err
	}
	current.Enabled = enabled
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	value := 0
	if enabled {
		value = 1
	}
	result, err := s.db.Exec(`UPDATE telegram_destinations SET enabled = ?, updated_at = ? WHERE id = ?`, value, current.UpdatedAt, id)
	if err != nil {
		return Destination{}, fmt.Errorf("set telegram destination enabled: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Destination{}, fmt.Errorf("set telegram destination enabled: %w", err)
	}
	if affected == 0 {
		return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
	}
	return current, nil
}

// DeleteDestination removes one destination and its profile links. Secret
// values live outside SQLite, so callers remove the Secret Service entry
// separately when needed.
func (s *Store) DeleteDestination(id string) error {
	if !destinationIDPattern.MatchString(id) {
		return fmt.Errorf("%w: destination id %q", ErrDestinationInvalid, id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete telegram destination: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM profile_telegram_destinations WHERE destination_id = ?`, id); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("delete telegram destination links: %w", err)
		}
	}
	result, err := tx.Exec(`DELETE FROM telegram_destinations WHERE id = ?`, id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
		}
		return fmt.Errorf("delete telegram destination: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete telegram destination: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete telegram destination: %w", err)
	}
	return nil
}

// RecordDestinationTest stores the last test outcome on the destination row.
// status is one of delivered, temporary_failure, permanent_failure, unknown,
// rate_limited, cancelled, or timeout. message is a safe human summary that
// must never contain the bot token.
func (s *Store) RecordDestinationTest(id, status, message string, at time.Time) (Destination, error) {
	current, err := s.GetDestination(id)
	if err != nil {
		return Destination{}, err
	}
	status = strings.TrimSpace(status)
	message = strings.TrimSpace(message)
	if len(message) > 2048 {
		message = message[:2048]
	}
	current.LastTestAt = at.UTC().Format(time.RFC3339Nano)
	current.LastTestStatus = status
	current.LastTestError = message
	current.UpdatedAt = current.LastTestAt
	result, err := s.db.Exec(
		`UPDATE telegram_destinations SET last_test_at = ?, last_test_status = ?, last_test_error = ?, updated_at = ? WHERE id = ?`,
		current.LastTestAt, current.LastTestStatus, current.LastTestError, current.UpdatedAt, id,
	)
	if err != nil {
		return Destination{}, fmt.Errorf("record telegram test: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Destination{}, fmt.Errorf("record telegram test: %w", err)
	}
	if affected == 0 {
		return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
	}
	return current, nil
}

// SetProfileDestinations replaces the Telegram Destinations linked to one
// profile. An empty list unlinks all destinations. Each id must reference an
// existing destination.
func (s *Store) SetProfileDestinations(profileID string, destinationIDs []string) error {
	if !profileIDPattern.MatchString(profileID) {
		return fmt.Errorf("%w: profile id %q", ErrProfileInvalid, profileID)
	}
	normalized, err := normalizeDestinationIDs(destinationIDs)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("link telegram destinations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM profiles WHERE id = ?`, profileID).Scan(&exists); err != nil {
		return fmt.Errorf("link telegram destinations: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, profileID)
	}
	for _, id := range normalized {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM telegram_destinations WHERE id = ?`, id).Scan(&count); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
			}
			return fmt.Errorf("link telegram destinations: %w", err)
		}
		if count == 0 {
			return fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
		}
	}
	if _, err := tx.Exec(`DELETE FROM profile_telegram_destinations WHERE profile_id = ?`, profileID); err != nil {
		return fmt.Errorf("link telegram destinations: %w", err)
	}
	for _, id := range normalized {
		if _, err := tx.Exec(`INSERT INTO profile_telegram_destinations (profile_id, destination_id) VALUES (?, ?)`, profileID, id); err != nil {
			return fmt.Errorf("link telegram destinations: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("link telegram destinations: %w", err)
	}
	return nil
}

// ListProfileDestinations returns the destinations linked to one profile
// ordered by id.
func (s *Store) ListProfileDestinations(profileID string) ([]Destination, error) {
	if !profileIDPattern.MatchString(profileID) {
		return nil, fmt.Errorf("%w: profile id %q", ErrProfileInvalid, profileID)
	}
	rows, err := s.db.Query(
		`SELECT d.id, d.name, d.chat_id, d.token_source, d.token_ref, d.enabled, d.last_test_at, d.last_test_status, d.last_test_error, d.created_at, d.updated_at FROM telegram_destinations d JOIN profile_telegram_destinations l ON l.destination_id = d.id WHERE l.profile_id = ? ORDER BY d.id`,
		profileID,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return []Destination{}, nil
		}
		return nil, fmt.Errorf("list profile destinations: %w", err)
	}
	defer rows.Close()
	destinations := []Destination{}
	for rows.Next() {
		var destination Destination
		var enabled int
		if err := rows.Scan(&destination.ID, &destination.Name, &destination.ChatID, &destination.TokenSource, &destination.TokenRef, &enabled, &destination.LastTestAt, &destination.LastTestStatus, &destination.LastTestError, &destination.CreatedAt, &destination.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list profile destinations: %w", err)
		}
		destination.Enabled = enabled == 1
		destinations = append(destinations, destination)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list profile destinations: %w", err)
	}
	return destinations, nil
}

// ListProfileDestinationIDs returns only the linked destination ids ordered
// lexicographically.
func (s *Store) ListProfileDestinationIDs(profileID string) ([]string, error) {
	destinations, err := s.ListProfileDestinations(profileID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(destinations))
	for _, destination := range destinations {
		ids = append(ids, destination.ID)
	}
	return ids, nil
}

// NormalizeDestinationIDs trims, validates, deduplicates, and sorts a list of
// destination ids. Empty input normalizes to an empty list (no destinations).
func NormalizeDestinationIDs(ids []string) ([]string, error) {
	return normalizeDestinationIDs(ids)
}

func normalizeDestinationIDs(ids []string) ([]string, error) {
	seen := map[string]bool{}
	result := []string{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("%w: telegram destination id is required", ErrDestinationInvalid)
		}
		if !destinationIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: destination id %q", ErrDestinationInvalid, raw)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

// ParseDestinationIDList parses a comma-separated destination list as used by
// CLI flags. Empty input means no destinations.
func ParseDestinationIDList(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{}, nil
	}
	parts := strings.Split(trimmed, ",")
	return normalizeDestinationIDs(parts)
}

// ValidateDestination checks identity, name, chat, and token reference rules
// without touching secrets.
func ValidateDestination(destination Destination) error {
	if !destinationIDPattern.MatchString(destination.ID) {
		return fmt.Errorf("%w: destination id must match [A-Za-z0-9_-], start alphanumerically, max 64", ErrDestinationInvalid)
	}
	name := strings.TrimSpace(destination.Name)
	if name == "" || len(name) > 256 || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("%w: name is required, max 256", ErrDestinationInvalid)
	}
	chat := strings.TrimSpace(destination.ChatID)
	if !chatIDPattern.MatchString(chat) {
		return fmt.Errorf("%w: chat id must be an integer like 123456 or -100123456", ErrDestinationInvalid)
	}
	if number, err := strconv.ParseInt(chat, 10, 64); err != nil || number == 0 {
		return fmt.Errorf("%w: chat id must be a non-zero integer", ErrDestinationInvalid)
	}
	switch destination.TokenSource {
	case TokenSourceSecretService, TokenSourcePrompt:
		if destination.TokenRef != "" && (strings.ContainsRune(destination.TokenRef, '\x00') || len(destination.TokenRef) > 1024) {
			return fmt.Errorf("%w: token reference is too long", ErrDestinationInvalid)
		}
		if destination.TokenSource == TokenSourcePrompt && destination.TokenRef != "" {
			return fmt.Errorf("%w: prompt destinations must not keep a token reference", ErrDestinationInvalid)
		}
	case TokenSourceFile:
		if strings.TrimSpace(destination.TokenRef) == "" || strings.ContainsRune(destination.TokenRef, '\x00') || len(destination.TokenRef) > 1024 {
			return fmt.Errorf("%w: token file path is required", ErrDestinationInvalid)
		}
	default:
		return fmt.Errorf("%w: token source must be secret-service, file, or prompt", ErrDestinationInvalid)
	}
	if len(destination.LastTestError) > 2048 {
		return fmt.Errorf("%w: test result is too long", ErrDestinationInvalid)
	}
	return nil
}

func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no rows") || strings.Contains(msg, "sql: no rows")
}

func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
}
