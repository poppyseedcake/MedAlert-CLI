// Package store history operations own Observation History inspection and
// retention. History covers observation runs (including failures),
// Availability Episodes, operational incidents, and Telegram delivery
// attempts. SQLite never stores passwords, bot tokens, or session secrets,
// so history rows are safe to show to operators and automation.
//
// Retention is a single saved policy: history_retention_days in
// application_metadata. Old terminal records are removed automatically;
// active episodes, active incidents, and pending/retry deliveries are kept
// indefinitely because they describe current work.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrHistoryInvalid is returned for invalid history or retention input.
	ErrHistoryInvalid = errors.New("invalid history input")
)

const (
	// DefaultHistoryRetentionDays keeps three months of terminal history.
	DefaultHistoryRetentionDays = 90
	// MinHistoryRetentionDays keeps pruning from deleting yesterday's history.
	MinHistoryRetentionDays = 1
	// MaxHistoryRetentionDays caps retention at ten years.
	MaxHistoryRetentionDays = 3650

	historyRetentionKey = "history_retention_days"
)

// PruneResult counts rows removed by one retention pass.
type PruneResult struct {
	ObservationRuns       int `json:"observation_runs"`
	AvailabilityEpisodes  int `json:"availability_episodes"`
	TelegramDeliveries    int `json:"telegram_deliveries"`
	OperationalIncidents  int `json:"operational_incidents"`
	OperationalDeliveries int `json:"operational_deliveries"`
}

// GetHistoryRetentionDays returns the saved retention policy in days. A
// missing or empty value means the default. An invalid saved value is
// reported so operators can repair it instead of silently using the default.
func (s *Store) GetHistoryRetentionDays() (int, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM application_metadata WHERE key = ?`, historyRetentionKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultHistoryRetentionDays, nil
	}
	if err != nil {
		if isNoSuchTable(err) {
			return DefaultHistoryRetentionDays, nil
		}
		return 0, fmt.Errorf("read history retention: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultHistoryRetentionDays, nil
	}
	var days int
	if _, err := fmt.Sscanf(raw, "%d", &days); err != nil {
		return 0, fmt.Errorf("%w: history retention %q is not a number", ErrHistoryInvalid, raw)
	}
	// Sscanf accepts "90days" as 90; require the full string to be numeric.
	trimmed := strings.TrimSpace(raw)
	numeric := fmt.Sprintf("%d", days)
	if trimmed != numeric {
		return 0, fmt.Errorf("%w: history retention %q is not a number", ErrHistoryInvalid, raw)
	}
	if days < MinHistoryRetentionDays || days > MaxHistoryRetentionDays {
		return 0, fmt.Errorf("%w: history retention must be %d..%d days", ErrHistoryInvalid, MinHistoryRetentionDays, MaxHistoryRetentionDays)
	}
	return days, nil
}

// SetHistoryRetentionDays saves the retention policy. It validates the range
// without touching history rows; pruning happens on the next automatic or
// manual pass.
func (s *Store) SetHistoryRetentionDays(days int) (int, error) {
	if days < MinHistoryRetentionDays || days > MaxHistoryRetentionDays {
		return 0, fmt.Errorf("%w: history retention must be %d..%d days", ErrHistoryInvalid, MinHistoryRetentionDays, MaxHistoryRetentionDays)
	}
	if _, err := s.db.Exec(`INSERT INTO application_metadata (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, historyRetentionKey, fmt.Sprintf("%d", days)); err != nil {
		if isNoSuchTable(err) {
			return 0, fmt.Errorf("%w: history retention is unavailable", ErrHistoryInvalid)
		}
		return 0, fmt.Errorf("save history retention: %w", err)
	}
	return days, nil
}

// PruneHistory removes old terminal history according to the saved policy.
// Active episodes, active incidents, running runs, and pending/retry
// deliveries are always kept because they describe current work. Resolved
// incidents, ended episodes, completed/failed runs, and terminal deliveries
// older than the cutoff are removed. Missing tables (older databases) prune
// to zero without an error.
func (s *Store) PruneHistory(now time.Time) (PruneResult, error) {
	days, err := s.GetHistoryRetentionDays()
	if err != nil {
		return PruneResult{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	result := PruneResult{}
	// Old terminal operational deliveries first so incident deletes do not
	// leave orphans when foreign keys are off.
	if count, err := pruneQuery(s.db, `DELETE FROM operational_deliveries WHERE created_at < ? AND status IN ('delivered', 'permanent_failure')`, cutoff); err != nil {
		return PruneResult{}, err
	} else {
		result.OperationalDeliveries = count
	}
	if count, err := pruneQuery(s.db, `DELETE FROM operational_incidents WHERE status = ? AND ended_at != '' AND ended_at < ?`, IncidentStatusResolved, cutoff); err != nil {
		return PruneResult{}, err
	} else {
		result.OperationalIncidents = count
	}
	// Terminal availability deliveries before episodes: deleting an ended
	// episode cascades its deliveries when foreign keys are on, and the
	// explicit delivery delete keeps history consistent when they are off.
	if count, err := pruneQuery(s.db, `DELETE FROM telegram_deliveries WHERE created_at < ? AND status IN ('delivered', 'permanent_failure')`, cutoff); err != nil {
		return PruneResult{}, err
	} else {
		result.TelegramDeliveries = count
	}
	if count, err := pruneQuery(s.db, `DELETE FROM availability_episodes WHERE active = 0 AND ended_at != '' AND ended_at < ?`, cutoff); err != nil {
		return PruneResult{}, err
	} else {
		result.AvailabilityEpisodes = count
	}
	if count, err := pruneQuery(s.db, `DELETE FROM observation_runs WHERE started_at < ? AND status != ?`, cutoff, ObservationRunRunning); err != nil {
		return PruneResult{}, err
	} else {
		result.ObservationRuns = count
	}
	return result, nil
}

func pruneQuery(database *sql.DB, query string, args ...any) (int, error) {
	result, err := database.Exec(query, args...)
	if err != nil {
		if isNoSuchTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("prune history: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune history: %w", err)
	}
	return int(affected), nil
}

// ListRecentObservationRuns returns observation runs across all profiles from
// newest to oldest, up to limit (0 means a sane default cap).
func (s *Store) ListRecentObservationRuns(limit int) ([]ObservationRun, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.Query(`SELECT id, profile_id, profile_updated_at, status, started_at, completed_at, slot_count, error_code, error_message FROM observation_runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		if isNoSuchTable(err) {
			return []ObservationRun{}, nil
		}
		return nil, fmt.Errorf("list observation runs: %w", err)
	}
	defer rows.Close()
	result := []ObservationRun{}
	for rows.Next() {
		run, err := scanObservationRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list observation runs: %w", err)
		}
		result = append(result, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list observation runs: %w", err)
	}
	return result, nil
}

// ListRecentEpisodes returns availability episodes from newest to oldest.
// An empty profileID returns episodes for all profiles. activeOnly keeps only
// active episodes. limit 0 means a sane default cap.
func (s *Store) ListRecentEpisodes(profileID string, activeOnly bool, limit int) ([]AvailabilityEpisode, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, profile_id, slot_identity, stable_identity, booking_string, appointment_time, clinic, doctor, specialty, visit_type, started_at, ended_at, active FROM availability_episodes WHERE 1 = 1`
	args := []any{}
	if trimmed := strings.TrimSpace(profileID); trimmed != "" {
		if !profileIDPattern.MatchString(trimmed) {
			return nil, fmt.Errorf("%w: profile id %q", ErrObservationRunInvalid, profileID)
		}
		query += ` AND profile_id = ?`
		args = append(args, trimmed)
	}
	if activeOnly {
		query += ` AND active = 1`
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		if isNoSuchTable(err) {
			return []AvailabilityEpisode{}, nil
		}
		return nil, fmt.Errorf("list availability episodes: %w", err)
	}
	defer rows.Close()
	result := []AvailabilityEpisode{}
	for rows.Next() {
		episode, err := scanAvailabilityEpisode(rows)
		if err != nil {
			return nil, fmt.Errorf("list availability episodes: %w", err)
		}
		result = append(result, episode)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list availability episodes: %w", err)
	}
	return result, nil
}

// ListRecentDeliveries returns Telegram availability deliveries from oldest to
// newest (creation order). An empty profileID returns deliveries for all
// profiles. An empty status returns all statuses. limit 0 means a sane
// default cap.
func (s *Store) ListRecentDeliveries(profileID, status string, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, profile_id, episode_id, destination_id, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM telegram_deliveries WHERE 1 = 1`
	args := []any{}
	if trimmed := strings.TrimSpace(profileID); trimmed != "" {
		if !profileIDPattern.MatchString(trimmed) {
			return nil, fmt.Errorf("%w: profile id %q", ErrDeliveryInvalid, profileID)
		}
		query += ` AND profile_id = ?`
		args = append(args, trimmed)
	}
	if trimmed := strings.TrimSpace(status); trimmed != "" {
		switch trimmed {
		case DeliveryPending, DeliveryDelivered, DeliveryRetry, DeliveryPermanentFailure:
		default:
			return nil, fmt.Errorf("%w: delivery status %q", ErrDeliveryInvalid, status)
		}
		query += ` AND status = ?`
		args = append(args, trimmed)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		if isNoSuchTable(err) {
			return []Delivery{}, nil
		}
		return nil, fmt.Errorf("list telegram deliveries: %w", err)
	}
	defer rows.Close()
	result := []Delivery{}
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("list telegram deliveries: %w", err)
		}
		result = append(result, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list telegram deliveries: %w", err)
	}
	return result, nil
}

// ListRecentIncidentDeliveries returns operational deliveries for one incident
// (empty incidentID returns recent deliveries across incidents) ordered by
// creation. limit 0 means a sane default cap.
func (s *Store) ListRecentIncidentDeliveries(incidentID string, limit int) ([]IncidentDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE 1 = 1`
	args := []any{}
	if trimmed := strings.TrimSpace(incidentID); trimmed != "" {
		query += ` AND incident_id = ?`
		args = append(args, trimmed)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		if isNoSuchTable(err) {
			return []IncidentDelivery{}, nil
		}
		return nil, fmt.Errorf("list operational deliveries: %w", err)
	}
	defer rows.Close()
	result := []IncidentDelivery{}
	for rows.Next() {
		var delivery IncidentDelivery
		if err := scanIncidentDelivery(rows, &delivery); err != nil {
			return nil, fmt.Errorf("list operational deliveries: %w", err)
		}
		result = append(result, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list operational deliveries: %w", err)
	}
	return result, nil
}
