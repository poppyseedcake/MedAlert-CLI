// Package store delivery operations own durable Telegram availability
// notifications with duplicate control.
//
// One Availability Episode is eligible once for each enabled linked Telegram
// Destination. The unique (profile_id, episode_id, destination_id) index
// enforces this across process restarts and across check and watch.
//
// Delivery is at least once: a pending row is saved before the Telegram call
// and the confirmed result is saved after the call. A process stop between
// those two writes can cause a duplicate on retry; that duplicate is accepted
// as documented behavior.
//
// States are pending, delivered, retry, and permanent_failure. Temporary and
// unknown Telegram results stay retryable for no more than MaxDeliveryAttempts
// total send attempts. A valid retry_after value controls next_attempt_at.
// A pending or retry delivery whose episode is no longer active stops without
// sending and becomes permanent_failure with a stale reason, so MedAlert never
// sends stale availability. SQLite never stores bot tokens.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DeliveryPending is saved before each Telegram call.
	DeliveryPending = "pending"
	// DeliveryDelivered confirms Telegram accepted the message.
	DeliveryDelivered = "delivered"
	// DeliveryRetry keeps a temporary or unknown result for a later attempt.
	DeliveryRetry = "retry"
	// DeliveryPermanentFailure is terminal: a permanent Telegram error, an
	// exhausted retry budget, or a stale cancellation.
	DeliveryPermanentFailure = "permanent_failure"

	// MaxDeliveryAttempts caps total Telegram send attempts for one delivery,
	// including the first attempt. After the fifth temporary or unknown
	// result the delivery becomes permanent_failure.
	MaxDeliveryAttempts = 5

	// staleDeliveryMessage marks a pending delivery stopped because its slot
	// is no longer available. It uses permanent_failure because the allowed
	// states have no separate cancelled value.
	staleDeliveryMessage = "slot is no longer available"
)

var (
	// ErrDeliveryNotFound is returned for an unknown delivery id.
	ErrDeliveryNotFound = errors.New("telegram delivery not found")
	// ErrDeliveryInvalid is returned for invalid delivery input.
	ErrDeliveryInvalid = errors.New("invalid telegram delivery input")
)

// Delivery records one durable Telegram notification for one episode and one
// destination. It contains display references only; bot tokens never enter
// the store.
type Delivery struct {
	ID            string `json:"id"`
	ProfileID     string `json:"profile_id"`
	EpisodeID     string `json:"episode_id"`
	DestinationID string `json:"destination_id"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	DeliveredAt   string `json:"delivered_at,omitempty"`
	MessageID     int64  `json:"message_id,omitempty"`
}

// EnsureEpisodeDeliveries creates one pending delivery for each enabled
// Telegram Destination linked to the profile, for the given episode. Existing
// rows for the same (profile, episode, destination) are kept, so repeated
// successful checks in one continuous episode never create another delivered
// notification. A new episode id after disappearance is eligible again.
// It returns the deliveries created by this call.
func (s *Store) EnsureEpisodeDeliveries(profileID, episodeID string, now time.Time) ([]Delivery, error) {
	if !profileIDPattern.MatchString(profileID) {
		return nil, fmt.Errorf("%w: profile id %q", ErrDeliveryInvalid, profileID)
	}
	if strings.TrimSpace(episodeID) == "" {
		return nil, fmt.Errorf("%w: episode id is required", ErrDeliveryInvalid)
	}
	stamp := deliveryTime(now).Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	// Verify the episode belongs to the profile so a caller cannot link
	// deliveries across profiles.
	var owner string
	if err := conn.QueryRowContext(ctx, `SELECT profile_id FROM availability_episodes WHERE id = ?`, episodeID).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: episode %s", ErrDeliveryInvalid, episodeID)
		}
		return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
	}
	if owner != profileID {
		return nil, fmt.Errorf("%w: episode %s does not belong to profile %s", ErrDeliveryInvalid, episodeID, profileID)
	}
	rows, err := conn.QueryContext(ctx, `SELECT d.id FROM telegram_destinations d JOIN profile_telegram_destinations l ON l.destination_id = d.id WHERE l.profile_id = ? AND d.enabled = 1 ORDER BY d.id`, profileID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
			}
			committed = true
			return []Delivery{}, nil
		}
		return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
	}
	destinationIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
		}
		destinationIDs = append(destinationIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
	}
	created := []Delivery{}
	for _, destinationID := range destinationIDs {
		id, err := newObservationID("delivery")
		if err != nil {
			return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
		}
		delivery := Delivery{
			ID:            id,
			ProfileID:     profileID,
			EpisodeID:     episodeID,
			DestinationID: destinationID,
			Status:        DeliveryPending,
			Attempts:      0,
			NextAttemptAt: "",
			CreatedAt:     stamp,
			UpdatedAt:     stamp,
		}
		result, err := conn.ExecContext(ctx, `INSERT INTO telegram_deliveries (id, profile_id, episode_id, destination_id, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, '', 0) ON CONFLICT(profile_id, episode_id, destination_id) DO NOTHING`, delivery.ID, delivery.ProfileID, delivery.EpisodeID, delivery.DestinationID, delivery.Status, delivery.Attempts, delivery.NextAttemptAt, delivery.CreatedAt, delivery.UpdatedAt)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table") {
				if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
					return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
				}
				committed = true
				return []Delivery{}, nil
			}
			return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
		}
		if affected == 1 {
			created = append(created, delivery)
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("ensure telegram deliveries: %w", err)
	}
	committed = true
	return created, nil
}

// ListDeliveriesForProfile returns all deliveries for a profile ordered by
// creation time. It is the history seam for Telegram deliveries.
func (s *Store) ListDeliveriesForProfile(profileID string) ([]Delivery, error) {
	if !profileIDPattern.MatchString(profileID) {
		return nil, fmt.Errorf("%w: profile id %q", ErrDeliveryInvalid, profileID)
	}
	rows, err := s.db.Query(`SELECT id, profile_id, episode_id, destination_id, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM telegram_deliveries WHERE profile_id = ? ORDER BY created_at, id`, profileID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
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

// ListDueDeliveries returns pending and retry deliveries ready for an attempt:
// attempts below the budget, next_attempt_at reached, episode still active,
// and destination still enabled. Disabled destinations stay pending for a
// later re-enable without consuming attempts.
//
// next_attempt_at is compared in Go, not SQL: RFC3339Nano trims trailing
// zeros, so lexicographic SQL comparison misorders same-second timestamps
// with different fractional precision (for example ".77Z" sorts after
// ".777Z" despite being earlier). Parsing keeps immediate retries due even
// when the check and the watch run in the same second.
func (s *Store) ListDueDeliveries(profileID string, now time.Time) ([]Delivery, error) {
	if !profileIDPattern.MatchString(profileID) {
		return nil, fmt.Errorf("%w: profile id %q", ErrDeliveryInvalid, profileID)
	}
	now = deliveryTime(now)
	rows, err := s.db.Query(`SELECT t.id, t.profile_id, t.episode_id, t.destination_id, t.status, t.attempts, t.next_attempt_at, t.last_error, t.created_at, t.updated_at, t.delivered_at, t.message_id FROM telegram_deliveries t JOIN availability_episodes e ON e.id = t.episode_id JOIN telegram_destinations d ON d.id = t.destination_id WHERE t.profile_id = ? AND t.status IN ('pending', 'retry') AND t.attempts < ? AND e.active = 1 AND d.enabled = 1 ORDER BY t.created_at, t.id`, profileID, MaxDeliveryAttempts)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return []Delivery{}, nil
		}
		return nil, fmt.Errorf("list due telegram deliveries: %w", err)
	}
	defer rows.Close()
	result := []Delivery{}
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("list due telegram deliveries: %w", err)
		}
		if strings.TrimSpace(delivery.NextAttemptAt) == "" {
			result = append(result, delivery)
			continue
		}
		if next, err := time.Parse(time.RFC3339Nano, delivery.NextAttemptAt); err == nil {
			if !next.After(now) {
				result = append(result, delivery)
			}
			continue
		}
		// Unparseable backoff never blocks a retry.
		result = append(result, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due telegram deliveries: %w", err)
	}
	return result, nil
}

// GetDelivery returns one delivery by id.
func (s *Store) GetDelivery(id string) (Delivery, error) {
	if strings.TrimSpace(id) == "" {
		return Delivery{}, fmt.Errorf("%w: delivery id is required", ErrDeliveryInvalid)
	}
	delivery, err := scanDelivery(s.db.QueryRow(`SELECT id, profile_id, episode_id, destination_id, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM telegram_deliveries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
	}
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return Delivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		return Delivery{}, fmt.Errorf("show telegram delivery: %w", err)
	}
	return delivery, nil
}

// GetEpisode returns one availability episode by id.
func (s *Store) GetEpisode(id string) (AvailabilityEpisode, error) {
	if strings.TrimSpace(id) == "" {
		return AvailabilityEpisode{}, fmt.Errorf("%w: episode id is required", ErrDeliveryInvalid)
	}
	episode, err := scanAvailabilityEpisode(s.db.QueryRow(`SELECT id, profile_id, slot_identity, stable_identity, booking_string, appointment_time, clinic, doctor, specialty, visit_type, started_at, ended_at, active FROM availability_episodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AvailabilityEpisode{}, fmt.Errorf("%w: episode %s", ErrDeliveryInvalid, id)
	}
	if err != nil {
		return AvailabilityEpisode{}, fmt.Errorf("show availability episode: %w", err)
	}
	return episode, nil
}

// IsRetryDue reports whether a profile has due retryable deliveries and its
// latest observation run was complete. Watch uses this to schedule a fresh
// observation run before a repeated delivery without hammering Medicover
// while observation itself keeps failing: failed runs respect the profile
// interval, successful runs with pending Telegram work retry promptly.
func (s *Store) IsRetryDue(profileID string, now time.Time) (bool, error) {
	if !profileIDPattern.MatchString(profileID) {
		return false, fmt.Errorf("%w: profile id %q", ErrDeliveryInvalid, profileID)
	}
	due, err := s.ListDueDeliveries(profileID, now)
	if err != nil {
		return false, err
	}
	if len(due) == 0 {
		return false, nil
	}
	var status string
	err = s.db.QueryRow(`SELECT status FROM observation_runs WHERE profile_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, profileID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return false, nil
		}
		return false, fmt.Errorf("check retry eligibility: %w", err)
	}
	return status == ObservationRunComplete, nil
}

// BeginDeliveryAttempt saves the pending state before a Telegram call and
// consumes one of the five attempts. Callers must save this before calling
// Telegram so a crash between the two writes retries later (at-least-once).
func (s *Store) BeginDeliveryAttempt(id string, now time.Time) (Delivery, error) {
	if strings.TrimSpace(id) == "" {
		return Delivery{}, fmt.Errorf("%w: delivery id is required", ErrDeliveryInvalid)
	}
	stamp := deliveryTime(now).Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Delivery{}, fmt.Errorf("begin telegram delivery: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Delivery{}, fmt.Errorf("begin telegram delivery: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var delivery Delivery
	if delivery, err = scanDelivery(conn.QueryRowContext(ctx, `SELECT id, profile_id, episode_id, destination_id, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM telegram_deliveries WHERE id = ?`, id)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		return Delivery{}, fmt.Errorf("begin telegram delivery: %w", err)
	}
	if delivery.Status != DeliveryPending && delivery.Status != DeliveryRetry {
		return Delivery{}, fmt.Errorf("%w: delivery %s is %s", ErrDeliveryInvalid, id, delivery.Status)
	}
	if delivery.Attempts >= MaxDeliveryAttempts {
		return Delivery{}, fmt.Errorf("%w: delivery %s exhausted its attempts", ErrDeliveryInvalid, id)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE telegram_deliveries SET status = ?, attempts = ?, updated_at = ? WHERE id = ?`, DeliveryPending, delivery.Attempts+1, stamp, id); err != nil {
		return Delivery{}, fmt.Errorf("begin telegram delivery: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Delivery{}, fmt.Errorf("begin telegram delivery: %w", err)
	}
	committed = true
	delivery.Status = DeliveryPending
	delivery.Attempts++
	delivery.UpdatedAt = stamp
	return delivery, nil
}

// DeliveryResult carries the confirmed Telegram outcome saved after the call.
type DeliveryResult struct {
	Status       string
	MessageID    int64
	LastError    string
	NextAttempt  time.Time
	HasNextRetry bool
}

// RecordDeliveryResult saves the confirmed result after a Telegram call.
// status is one of delivered, retry, or permanent_failure. Retry with an
// exhausted budget becomes permanent_failure so history stays terminal.
// lastError must never contain the bot token; callers pass the sanitized
// Telegram error message.
func (s *Store) RecordDeliveryResult(id string, result DeliveryResult, now time.Time) (Delivery, error) {
	if strings.TrimSpace(id) == "" {
		return Delivery{}, fmt.Errorf("%w: delivery id is required", ErrDeliveryInvalid)
	}
	switch result.Status {
	case DeliveryDelivered, DeliveryRetry, DeliveryPermanentFailure:
	default:
		return Delivery{}, fmt.Errorf("%w: status %q", ErrDeliveryInvalid, result.Status)
	}
	message := strings.TrimSpace(result.LastError)
	if len(message) > 2048 {
		message = message[:2048]
	}
	stamp := deliveryTime(now).Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Delivery{}, fmt.Errorf("record telegram delivery: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Delivery{}, fmt.Errorf("record telegram delivery: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var current Delivery
	if current, err = scanDelivery(conn.QueryRowContext(ctx, `SELECT id, profile_id, episode_id, destination_id, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM telegram_deliveries WHERE id = ?`, id)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Delivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		return Delivery{}, fmt.Errorf("record telegram delivery: %w", err)
	}
	finalStatus := result.Status
	var nextAttemptAt string
	var deliveredAt string
	var messageID int64
	if finalStatus == DeliveryDelivered {
		deliveredAt = stamp
		messageID = result.MessageID
		message = ""
	} else if finalStatus == DeliveryRetry {
		if current.Attempts >= MaxDeliveryAttempts {
			finalStatus = DeliveryPermanentFailure
			if message == "" {
				message = "telegram delivery failed after 5 attempts"
			} else if !strings.Contains(message, "5 attempts") {
				message = message + " (gave up after 5 attempts)"
				if len(message) > 2048 {
					message = message[:2048]
				}
			}
		} else if result.HasNextRetry && !result.NextAttempt.IsZero() {
			nextAttemptAt = result.NextAttempt.UTC().Format(time.RFC3339Nano)
		} else {
			// No backoff requested: retry on the next monitoring cycle.
			nextAttemptAt = stamp
		}
	}
	if finalStatus == DeliveryPermanentFailure && message == "" {
		message = "telegram delivery failed"
	}
	if _, err := conn.ExecContext(ctx, `UPDATE telegram_deliveries SET status = ?, next_attempt_at = ?, last_error = ?, updated_at = ?, delivered_at = ?, message_id = ? WHERE id = ?`, finalStatus, nextAttemptAt, message, stamp, deliveredAt, messageID, id); err != nil {
		return Delivery{}, fmt.Errorf("record telegram delivery: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Delivery{}, fmt.Errorf("record telegram delivery: %w", err)
	}
	committed = true
	current.Status = finalStatus
	current.NextAttemptAt = nextAttemptAt
	current.LastError = message
	current.UpdatedAt = stamp
	current.DeliveredAt = deliveredAt
	current.MessageID = messageID
	return current, nil
}

// CancelDeliveriesForEndedEpisodes stops pending and retry deliveries whose
// slots are no longer available. They become permanent_failure with a stale
// reason instead of sending stale availability.
func (s *Store) CancelDeliveriesForEndedEpisodes(profileID string, episodeIDs []string, now time.Time) (int, error) {
	if !profileIDPattern.MatchString(profileID) {
		return 0, fmt.Errorf("%w: profile id %q", ErrDeliveryInvalid, profileID)
	}
	ids := []string{}
	for _, raw := range episodeIDs {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	stamp := deliveryTime(now).Format(time.RFC3339Nano)
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(ids)+3)
	args = append(args, DeliveryPermanentFailure, staleDeliveryMessage, stamp)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, profileID)
	query := fmt.Sprintf(`UPDATE telegram_deliveries SET status = ?, last_error = ?, updated_at = ? WHERE episode_id IN (%s) AND profile_id = ? AND status IN ('pending', 'retry')`, placeholders)
	result, err := s.db.Exec(query, args...)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("cancel stale telegram deliveries: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel stale telegram deliveries: %w", err)
	}
	return int(affected), nil
}

func scanDelivery(scanner observationScanner) (Delivery, error) {
	var delivery Delivery
	err := scanner.Scan(&delivery.ID, &delivery.ProfileID, &delivery.EpisodeID, &delivery.DestinationID, &delivery.Status, &delivery.Attempts, &delivery.NextAttemptAt, &delivery.LastError, &delivery.CreatedAt, &delivery.UpdatedAt, &delivery.DeliveredAt, &delivery.MessageID)
	return delivery, err
}

func deliveryTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
