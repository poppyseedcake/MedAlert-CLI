// Package store telegram operations own Telegram Destination configuration.
//
// SQLite stores destination configuration and secret references. It never
// stores bot tokens or other secret values. One Observation Profile can link
// to more than one Telegram Destination through profile_telegram_destinations.
package store

import (
	"context"
	"database/sql"
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
	// Delivery history belongs to the destination. Foreign keys cascade, but
	// an explicit delete keeps history consistent when constraints are off
	// and tolerates databases without the deliveries table.
	if _, err := tx.Exec(`DELETE FROM telegram_deliveries WHERE destination_id = ?`, id); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("delete telegram destination deliveries: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM operational_deliveries WHERE destination_id = ?`, id); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("delete telegram destination deliveries: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM operational_deliveries WHERE incident_id IN (SELECT id FROM operational_incidents WHERE destination_id = ?)`, id); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("delete telegram destination deliveries: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM operational_incidents WHERE destination_id = ?`, id); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("delete telegram destination incidents: %w", err)
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
	if _, err := tx.Exec(`UPDATE profiles SET updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), profileID); err != nil {
		return fmt.Errorf("link telegram destinations: %w", err)
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

// ListDestinationProfileIDs returns the profiles linked to one destination,
// ordered by profile id. It is the reverse-link seam used by the terminal
// interface.
func (s *Store) ListDestinationProfileIDs(destinationID string) ([]string, error) {
	if !destinationIDPattern.MatchString(destinationID) {
		return nil, fmt.Errorf("%w: destination id %q", ErrDestinationInvalid, destinationID)
	}
	rows, err := s.db.Query(
		`SELECT p.id FROM profiles p JOIN profile_telegram_destinations l ON l.profile_id = p.id WHERE l.destination_id = ? ORDER BY p.id`,
		destinationID,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list destination profiles: %w", err)
	}
	defer rows.Close()
	profiles := []string{}
	for rows.Next() {
		var profileID string
		if err := rows.Scan(&profileID); err != nil {
			return nil, fmt.Errorf("list destination profiles: %w", err)
		}
		profiles = append(profiles, profileID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list destination profiles: %w", err)
	}
	return profiles, nil
}

// SetDestinationProfiles replaces the profiles linked to one destination.
// An empty list removes all links while preserving each profile's other
// destinations. The operation is atomic so a missing profile cannot leave a
// partial link update.
func (s *Store) SetDestinationProfiles(destinationID string, profileIDs []string) error {
	if !destinationIDPattern.MatchString(destinationID) {
		return fmt.Errorf("%w: destination id %q", ErrDestinationInvalid, destinationID)
	}
	normalized, err := normalizeProfileIDs(profileIDs)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("link destination profiles: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("link destination profiles: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var destinationCount int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM telegram_destinations WHERE id = ?`, destinationID).Scan(&destinationCount); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return fmt.Errorf("%w: %s", ErrDestinationNotFound, destinationID)
		}
		return fmt.Errorf("link destination profiles: %w", err)
	}
	if destinationCount == 0 {
		return fmt.Errorf("%w: %s", ErrDestinationNotFound, destinationID)
	}
	for _, profileID := range normalized {
		var profileCount int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM profiles WHERE id = ?`, profileID).Scan(&profileCount); err != nil {
			return fmt.Errorf("link destination profiles: %w", err)
		}
		if profileCount == 0 {
			return fmt.Errorf("%w: %s", ErrProfileNotFound, profileID)
		}
	}
	linkedProfiles, err := destinationProfileIDs(conn, ctx, destinationID)
	if err != nil {
		return fmt.Errorf("link destination profiles: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM profile_telegram_destinations WHERE destination_id = ?`, destinationID); err != nil {
		return fmt.Errorf("link destination profiles: %w", err)
	}
	for _, profileID := range normalized {
		if _, err := conn.ExecContext(ctx, `INSERT INTO profile_telegram_destinations (profile_id, destination_id) VALUES (?, ?)`, profileID, destinationID); err != nil {
			return fmt.Errorf("link destination profiles: %w", err)
		}
		linkedProfiles = append(linkedProfiles, profileID)
	}
	linkedProfiles = uniqueStrings(linkedProfiles)
	bumpedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for _, profileID := range linkedProfiles {
		if _, err := conn.ExecContext(ctx, `UPDATE profiles SET updated_at = ? WHERE id = ?`, bumpedAt, profileID); err != nil {
			return fmt.Errorf("link destination profiles: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("link destination profiles: %w", err)
	}
	committed = true
	return nil
}

func destinationProfileIDs(conn *sql.Conn, ctx context.Context, destinationID string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT profile_id FROM profile_telegram_destinations WHERE destination_id = ?`, destinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []string{}
	for rows.Next() {
		var profileID string
		if err := rows.Scan(&profileID); err != nil {
			return nil, err
		}
		profiles = append(profiles, profileID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return profiles, nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func normalizeProfileIDs(ids []string) ([]string, error) {
	seen := map[string]bool{}
	profiles := []string{}
	for _, raw := range ids {
		profileID := strings.TrimSpace(raw)
		if profileID == "" {
			return nil, fmt.Errorf("%w: profile id is required", ErrProfileInvalid)
		}
		if !profileIDPattern.MatchString(profileID) {
			return nil, fmt.Errorf("%w: profile id %q", ErrProfileInvalid, raw)
		}
		if seen[profileID] {
			continue
		}
		seen[profileID] = true
		profiles = append(profiles, profileID)
	}
	sort.Strings(profiles)
	return profiles, nil
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

// CreateProfileWithDestinations stores a new profile and its Telegram links
// atomically. If any destination is missing the profile is not created, so a
// retry does not hit a partial orphan row.
func (s *Store) CreateProfileWithDestinations(profile Profile, destinationIDs []string) (Profile, error) {
	normalizedProfile, err := normalizeProfile(profile)
	if err != nil {
		return Profile{}, err
	}
	profile = normalizedProfile
	normalizedDestinations, err := normalizeDestinationIDs(destinationIDs)
	if err != nil {
		return Profile{}, err
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var accountCount int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = ?`, profile.AccountID).Scan(&accountCount); err != nil {
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	if accountCount == 0 {
		return Profile{}, fmt.Errorf("%w: %s", ErrAccountNotFound, profile.AccountID)
	}
	for _, id := range normalizedDestinations {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM telegram_destinations WHERE id = ?`, id).Scan(&count); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return Profile{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
			}
			return Profile{}, fmt.Errorf("create profile: %w", err)
		}
		if count == 0 {
			return Profile{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, id)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	profile.CreatedAt = now
	profile.UpdatedAt = now
	enabled := 0
	if profile.Enabled {
		enabled = 1
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO profiles (id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		profile.ID, profile.AccountID, profile.RegionIDs, profile.SpecialtyIDs, profile.ClinicIDs, profile.DoctorIDs, profile.LanguageIDs, profile.VisitType, profile.SearchType, profile.StartDate, profile.EndDate, profile.CheckIntervalMinutes, enabled, profile.CreatedAt, profile.UpdatedAt,
	); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "primary") {
			return Profile{}, fmt.Errorf("%w: %s", ErrProfileExists, profile.ID)
		}
		if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			return Profile{}, fmt.Errorf("%w: %s", ErrAccountNotFound, profile.AccountID)
		}
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	for _, id := range normalizedDestinations {
		if _, err := conn.ExecContext(ctx, `INSERT INTO profile_telegram_destinations (profile_id, destination_id) VALUES (?, ?)`, profile.ID, id); err != nil {
			return Profile{}, fmt.Errorf("create profile: %w", err)
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	committed = true
	return profile, nil
}

// UpdateProfileWithDestinations changes search criteria and/or Telegram links
// atomically. destinationIDs nil means keep current links; non-nil (including
// empty) replaces them. If any destination is missing neither criteria nor
// links are changed.
func (s *Store) UpdateProfileWithDestinations(id string, update ProfileUpdate, hasCriteria bool, destinationIDs []string, hasDestinations bool) (Profile, error) {
	if !profileIDPattern.MatchString(id) {
		return Profile{}, fmt.Errorf("%w: profile id %q", ErrProfileInvalid, id)
	}
	var normalizedDestinations []string
	if hasDestinations {
		normalized, err := normalizeDestinationIDs(destinationIDs)
		if err != nil {
			return Profile{}, err
		}
		normalizedDestinations = normalized
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var current Profile
	var enabledFlag int
	err = conn.QueryRowContext(ctx,
		`SELECT id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at FROM profiles WHERE id = ?`, id,
	).Scan(&current.ID, &current.AccountID, &current.RegionIDs, &current.SpecialtyIDs, &current.ClinicIDs, &current.DoctorIDs, &current.LanguageIDs, &current.VisitType, &current.SearchType, &current.StartDate, &current.EndDate, &current.CheckIntervalMinutes, &enabledFlag, &current.CreatedAt, &current.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, id)
		}
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	current.Enabled = enabledFlag == 1
	merged := current
	if hasCriteria {
		if update.RegionIDs != nil {
			merged.RegionIDs = *update.RegionIDs
		}
		if update.SpecialtyIDs != nil {
			merged.SpecialtyIDs = *update.SpecialtyIDs
		}
		if update.ClinicIDs != nil {
			merged.ClinicIDs = *update.ClinicIDs
		}
		if update.DoctorIDs != nil {
			merged.DoctorIDs = *update.DoctorIDs
		}
		if update.LanguageIDs != nil {
			merged.LanguageIDs = *update.LanguageIDs
		}
		if update.VisitType != nil {
			merged.VisitType = *update.VisitType
		}
		if update.SearchType != nil {
			merged.SearchType = *update.SearchType
		}
		if update.StartDate != nil {
			merged.StartDate = *update.StartDate
		}
		if update.EndDate != nil {
			merged.EndDate = *update.EndDate
		}
		if update.CheckIntervalMinutes != nil {
			merged.CheckIntervalMinutes = *update.CheckIntervalMinutes
		}
		normalized, err := normalizeProfile(merged)
		if err != nil {
			return Profile{}, err
		}
		merged = normalized
	}
	if hasDestinations {
		for _, destID := range normalizedDestinations {
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM telegram_destinations WHERE id = ?`, destID).Scan(&count); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "no such table") {
					return Profile{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, destID)
				}
				return Profile{}, fmt.Errorf("edit profile: %w", err)
			}
			if count == 0 {
				return Profile{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, destID)
			}
		}
	}
	if hasCriteria {
		sets := make([]string, 0, 11)
		args := make([]any, 0, 12)
		if update.RegionIDs != nil {
			sets = append(sets, "region_ids = ?")
			args = append(args, merged.RegionIDs)
		}
		if update.SpecialtyIDs != nil {
			sets = append(sets, "specialty_ids = ?")
			args = append(args, merged.SpecialtyIDs)
		}
		if update.ClinicIDs != nil {
			sets = append(sets, "clinic_ids = ?")
			args = append(args, merged.ClinicIDs)
		}
		if update.DoctorIDs != nil {
			sets = append(sets, "doctor_ids = ?")
			args = append(args, merged.DoctorIDs)
		}
		if update.LanguageIDs != nil {
			sets = append(sets, "language_ids = ?")
			args = append(args, merged.LanguageIDs)
		}
		if update.VisitType != nil {
			sets = append(sets, "visit_type = ?")
			args = append(args, merged.VisitType)
		}
		if update.SearchType != nil {
			sets = append(sets, "search_type = ?")
			args = append(args, merged.SearchType)
		}
		if update.StartDate != nil {
			sets = append(sets, "start_date = ?")
			args = append(args, merged.StartDate)
		}
		if update.EndDate != nil {
			sets = append(sets, "end_date = ?")
			args = append(args, merged.EndDate)
		}
		if update.CheckIntervalMinutes != nil {
			sets = append(sets, "check_interval_minutes = ?")
			args = append(args, merged.CheckIntervalMinutes)
		}
		updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
		sets = append(sets, "updated_at = ?")
		args = append(args, updatedAt)
		args = append(args, id)
		result, err := conn.ExecContext(ctx,
			fmt.Sprintf(`UPDATE profiles SET %s WHERE id = ?`, strings.Join(sets, ", ")),
			args...,
		)
		if err != nil {
			return Profile{}, fmt.Errorf("edit profile: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return Profile{}, fmt.Errorf("edit profile: %w", err)
		}
		if affected == 0 {
			return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, id)
		}
	}
	if hasDestinations {
		if _, err := conn.ExecContext(ctx, `DELETE FROM profile_telegram_destinations WHERE profile_id = ?`, id); err != nil {
			return Profile{}, fmt.Errorf("edit profile: %w", err)
		}
		for _, destID := range normalizedDestinations {
			if _, err := conn.ExecContext(ctx, `INSERT INTO profile_telegram_destinations (profile_id, destination_id) VALUES (?, ?)`, id, destID); err != nil {
				return Profile{}, fmt.Errorf("edit profile: %w", err)
			}
		}
		// Link changes must invalidate in-flight runs: observation_runs
		// captures profiles.updated_at at run start and reconciliation
		// treats a matching timestamp as current. Without a bump here a
		// destinations-only edit would leave notification changes invisible.
		bumpedAt := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `UPDATE profiles SET updated_at = ? WHERE id = ?`, bumpedAt, id); err != nil {
			return Profile{}, fmt.Errorf("edit profile: %w", err)
		}
	}
	var fresh Profile
	var freshEnabled int
	err = conn.QueryRowContext(ctx,
		`SELECT id, account_id, region_ids, specialty_ids, clinic_ids, doctor_ids, language_ids, visit_type, search_type, start_date, end_date, check_interval_minutes, enabled, created_at, updated_at FROM profiles WHERE id = ?`, id,
	).Scan(&fresh.ID, &fresh.AccountID, &fresh.RegionIDs, &fresh.SpecialtyIDs, &fresh.ClinicIDs, &fresh.DoctorIDs, &fresh.LanguageIDs, &fresh.VisitType, &fresh.SearchType, &fresh.StartDate, &fresh.EndDate, &fresh.CheckIntervalMinutes, &freshEnabled, &fresh.CreatedAt, &fresh.UpdatedAt)
	if err != nil {
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	fresh.Enabled = freshEnabled == 1
	if err := ValidateProfile(fresh); err != nil {
		return Profile{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Profile{}, fmt.Errorf("edit profile: %w", err)
	}
	committed = true
	return fresh, nil
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
