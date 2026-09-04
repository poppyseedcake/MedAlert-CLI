// Package store incident operations own durable operational incidents and
// their Telegram failure/recovery notifications.
//
// One continuous operational problem is one active incident. Repeated related
// failures update the active incident instead of creating message noise. One
// failure notification and one recovery notification is sent for each eligible
// Telegram Destination.
//
// Scopes are account, profile, and destination:
//   - account scope (scope_id=account_id) pauses only that account's profiles.
//   - profile scope (scope_id=profile_id) tracks observation failures.
//   - destination scope (scope_id=destination_id, profile_id set) tracks a
//     permanent Telegram delivery failure for one profile; notifications go
//     through the profile's other active destinations, never the failed route.
//
// Authentication Required and Protocol Changed start an incident immediately.
// A temporary observation problem becomes notifiable after three consecutive
// failed runs. One complete successful run ends the incident.
//
// Incident deliveries reuse the pending/delivered/retry/permanent_failure
// states with at-least-once semantics: a pending row is saved before the
// Telegram call and the confirmed result after. Temporary and unknown results
// are retried for no more than MaxDeliveryAttempts attempts; a valid
// retry_after controls next_attempt_at. A pending failure cancelled when the
// incident ends becomes permanent_failure so history stays terminal; recovery
// is sent only to destinations that delivered the matching failure.
// A failure in an operational-notification delivery never creates another
// incident. SQLite never stores bot tokens.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	IncidentScopeAccount     = "account"
	IncidentScopeProfile     = "profile"
	IncidentScopeDestination = "destination"

	IncidentStatusActive   = "active"
	IncidentStatusResolved = "resolved"

	IncidentKindAuth      = "authentication_required"
	IncidentKindProtocol  = "protocol_changed"
	IncidentKindTemporary = "temporary_failure"
	IncidentKindDelivery  = "delivery_permanent"

	IncidentDeliveryFailure  = "failure"
	IncidentDeliveryRecovery = "recovery"

	incidentCancelledMessage = "incident ended before failure was delivered"
)

var (
	ErrIncidentNotFound = errors.New("operational incident not found")
	ErrIncidentInvalid  = errors.New("invalid operational incident input")
)

// Incident records one continuous operational problem.
type Incident struct {
	ID                  string `json:"id"`
	ScopeType           string `json:"scope_type"`
	ScopeID             string `json:"scope_id"`
	AccountID           string `json:"account_id"`
	ProfileID           string `json:"profile_id"`
	DestinationID       string `json:"destination_id"`
	Kind                string `json:"kind"`
	Status              string `json:"status"`
	FailureCode         string `json:"failure_code"`
	FailureMessage      string `json:"failure_message"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	FirstSeenAt         string `json:"first_seen_at"`
	LastSeenAt          string `json:"last_seen_at"`
	EndedAt             string `json:"ended_at,omitempty"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

// IncidentDelivery records one durable failure or recovery notification for
// one incident and one destination.
type IncidentDelivery struct {
	ID            string `json:"id"`
	IncidentID    string `json:"incident_id"`
	DestinationID string `json:"destination_id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	DeliveredAt   string `json:"delivered_at,omitempty"`
	MessageID     int64  `json:"message_id,omitempty"`
}

// NormalizeIncidentKind maps a detailed failure code to the incident kind
// used for immediacy decisions.
func NormalizeIncidentKind(failureCode string) string {
	switch strings.TrimSpace(failureCode) {
	case "authentication_required", "mfa_required", "invalid_credentials":
		return IncidentKindAuth
	case "protocol_changed":
		return IncidentKindProtocol
	case "permanent_failure", "delivery_permanent":
		return IncidentKindDelivery
	default:
		return IncidentKindTemporary
	}
}

// IsIncidentImmediate reports whether a failure code starts an incident
// immediately without waiting for three consecutive failures.
func IsIncidentImmediate(failureCode string) bool {
	switch strings.TrimSpace(failureCode) {
	case "authentication_required", "mfa_required", "invalid_credentials", "protocol_changed":
		return true
	default:
		return NormalizeIncidentKind(failureCode) == IncidentKindDelivery
	}
}

// incidentTime normalizes timestamps to UTC.
func incidentTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

// hasLiveClaimLease reports whether a delivery row is currently claimed by
// another worker's in-flight Telegram request. Claims hold a future
// next_attempt_at lease that comfortably covers transport timeouts, while
// fresh pending rows carry an empty timestamp and scheduled retries carry a
// past-due or backoff timestamp with retry status. Only pending rows can hold
// claim leases: claiming always resets status to pending.
func hasLiveClaimLease(nextAttemptAt string, now time.Time) bool {
	if strings.TrimSpace(nextAttemptAt) == "" {
		return false
	}
	next, err := time.Parse(time.RFC3339Nano, nextAttemptAt)
	if err != nil {
		return false
	}
	return next.After(now)
}

func normalizeIncidentScope(scopeType, scopeID, accountID, profileID, destinationID string) (string, string, string, string, string, error) {
	scopeType = strings.TrimSpace(scopeType)
	scopeID = strings.TrimSpace(scopeID)
	accountID = strings.TrimSpace(accountID)
	profileID = strings.TrimSpace(profileID)
	destinationID = strings.TrimSpace(destinationID)
	switch scopeType {
	case IncidentScopeAccount:
		if scopeID == "" {
			return "", "", "", "", "", fmt.Errorf("%w: account scope needs a scope id", ErrIncidentInvalid)
		}
		if accountID == "" {
			accountID = scopeID
		}
		if accountID != scopeID {
			return "", "", "", "", "", fmt.Errorf("%w: account scope id mismatch", ErrIncidentInvalid)
		}
		profileID = ""
		destinationID = ""
	case IncidentScopeProfile:
		if scopeID == "" {
			return "", "", "", "", "", fmt.Errorf("%w: profile scope needs a scope id", ErrIncidentInvalid)
		}
		if profileID == "" {
			profileID = scopeID
		}
		if profileID != scopeID {
			return "", "", "", "", "", fmt.Errorf("%w: profile scope id mismatch", ErrIncidentInvalid)
		}
		destinationID = ""
	case IncidentScopeDestination:
		if scopeID == "" || profileID == "" || destinationID == "" {
			return "", "", "", "", "", fmt.Errorf("%w: destination scope needs scope, profile, and destination ids", ErrIncidentInvalid)
		}
		if destinationID != scopeID {
			return "", "", "", "", "", fmt.Errorf("%w: destination scope id mismatch", ErrIncidentInvalid)
		}
	default:
		return "", "", "", "", "", fmt.Errorf("%w: scope type %q", ErrIncidentInvalid, scopeType)
	}
	return scopeType, scopeID, accountID, profileID, destinationID, nil
}

func normalizeIncidentDestinations(ids []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		if !destinationIDPattern.MatchString(id) {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

// RecordIncidentFailure creates or updates the active incident for a scope.
// eligibleDestinationIDs are the enabled linked destinations that may receive
// the failure notification (for destination scope: the profile's other
// destinations, never the failed route itself).
//
// It returns the incident, the failure deliveries created by this call (empty
// when the incident was only updated or is not yet notifiable), and whether
// this call made the incident newly notifiable.
func (s *Store) RecordIncidentFailure(scopeType, scopeID, accountID, profileID, destinationID, failureCode, failureMessage string, now time.Time, eligibleDestinationIDs []string) (Incident, []IncidentDelivery, bool, error) {
	scopeType, scopeID, accountID, profileID, destinationID, err := normalizeIncidentScope(scopeType, scopeID, accountID, profileID, destinationID)
	if err != nil {
		return Incident{}, nil, false, err
	}
	failureCode = strings.TrimSpace(failureCode)
	if failureCode == "" {
		failureCode = "temporary_failure"
	}
	if len(failureCode) > 128 {
		failureCode = failureCode[:128]
	}
	failureMessage = strings.TrimSpace(failureMessage)
	if len(failureMessage) > 2048 {
		failureMessage = failureMessage[:2048]
	}
	kind := NormalizeIncidentKind(failureCode)
	// Delivery-permanent incidents keep their delivery kind even when the
	// caller's code is a generic telegram code.
	if scopeType == IncidentScopeDestination && kind != IncidentKindDelivery {
		kind = IncidentKindDelivery
	}
	now = incidentTime(now)
	stamp := now.Format(time.RFC3339Nano)
	eligible := normalizeIncidentDestinations(eligibleDestinationIDs)

	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var incident Incident
	findErr := scanIncident(conn.QueryRowContext(ctx, `SELECT id, scope_type, scope_id, account_id, profile_id, destination_id, kind, status, failure_code, failure_message, consecutive_failures, first_seen_at, last_seen_at, ended_at, created_at, updated_at FROM operational_incidents WHERE scope_type = ? AND scope_id = ? AND profile_id = ? AND destination_id = ? AND status = ?`, scopeType, scopeID, profileID, destinationID, IncidentStatusActive), &incident)
	if findErr != nil && !errors.Is(findErr, sql.ErrNoRows) {
		if isNoSuchTable(findErr) {
			return Incident{}, nil, false, fmt.Errorf("%w: operational incidents are unavailable", ErrIncidentInvalid)
		}
		return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", findErr)
	}
	if errors.Is(findErr, sql.ErrNoRows) {
		id, err := newObservationID("incident")
		if err != nil {
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
		incident = Incident{
			ID:                  id,
			ScopeType:           scopeType,
			ScopeID:             scopeID,
			AccountID:           accountID,
			ProfileID:           profileID,
			DestinationID:       destinationID,
			Kind:                kind,
			Status:              IncidentStatusActive,
			FailureCode:         failureCode,
			FailureMessage:      failureMessage,
			ConsecutiveFailures: 1,
			FirstSeenAt:         stamp,
			LastSeenAt:          stamp,
			CreatedAt:           stamp,
			UpdatedAt:           stamp,
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO operational_incidents (id, scope_type, scope_id, account_id, profile_id, destination_id, kind, status, failure_code, failure_message, consecutive_failures, first_seen_at, last_seen_at, ended_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`, incident.ID, incident.ScopeType, incident.ScopeID, incident.AccountID, incident.ProfileID, incident.DestinationID, incident.Kind, incident.Status, incident.FailureCode, incident.FailureMessage, incident.ConsecutiveFailures, incident.FirstSeenAt, incident.LastSeenAt, incident.CreatedAt, incident.UpdatedAt); err != nil {
			if isNoSuchTable(err) {
				return Incident{}, nil, false, fmt.Errorf("%w: operational incidents are unavailable", ErrIncidentInvalid)
			}
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
	} else {
		incident.Kind = kind
		incident.FailureCode = failureCode
		incident.FailureMessage = failureMessage
		incident.ConsecutiveFailures++
		incident.LastSeenAt = stamp
		incident.UpdatedAt = stamp
		if _, err := conn.ExecContext(ctx, `UPDATE operational_incidents SET kind = ?, failure_code = ?, failure_message = ?, consecutive_failures = ?, last_seen_at = ?, updated_at = ? WHERE id = ? AND status = ?`, incident.Kind, incident.FailureCode, incident.FailureMessage, incident.ConsecutiveFailures, incident.LastSeenAt, incident.UpdatedAt, incident.ID, IncidentStatusActive); err != nil {
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
	}

	// Already notified? One failure notification per destination per incident.
	var existingFailures int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM operational_deliveries WHERE incident_id = ? AND kind = ?`, incident.ID, IncidentDeliveryFailure).Scan(&existingFailures); err != nil {
		if !isNoSuchTable(err) {
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
		existingFailures = 0
	}
	if existingFailures > 0 {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
		committed = true
		return incident, nil, false, nil
	}
	notifiable := IsIncidentImmediate(failureCode) || incident.ConsecutiveFailures >= 3
	if !notifiable {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
		committed = true
		return incident, nil, false, nil
	}
	created := []IncidentDelivery{}
	for _, destinationID := range eligible {
		id, err := newObservationID("incident-delivery")
		if err != nil {
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
		delivery := IncidentDelivery{
			ID:            id,
			IncidentID:    incident.ID,
			DestinationID: destinationID,
			Kind:          IncidentDeliveryFailure,
			Status:        DeliveryPending,
			Attempts:      0,
			CreatedAt:     stamp,
			UpdatedAt:     stamp,
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO operational_deliveries (id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id) VALUES (?, ?, ?, ?, ?, ?, '', '', ?, ?, '', 0) ON CONFLICT(incident_id, destination_id, kind) DO NOTHING`, delivery.ID, delivery.IncidentID, delivery.DestinationID, delivery.Kind, delivery.Status, delivery.Attempts, delivery.CreatedAt, delivery.UpdatedAt); err != nil {
			if isNoSuchTable(err) {
				break
			}
			// A missing destination row violates the foreign key: skip it so
			// one removed destination does not fail the whole incident.
			if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
				continue
			}
			return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
		}
		created = append(created, delivery)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Incident{}, nil, false, fmt.Errorf("record operational incident: %w", err)
	}
	committed = true
	return incident, created, len(created) > 0, nil
}

// ResolveIncident ends the active incident for a scope. Pending failure
// notifications that never delivered are cancelled, except rows with a live
// claim lease: another worker's Telegram request is in flight for those, and
// cancelling them would lose the send result (the late record conflicts and
// is skipped) leaving a failure with no recovery. Claimed rows finish
// instead, and a late delivered failure queues its recovery on record. One
// pending recovery is created for each destination that delivered the
// failure.
func (s *Store) ResolveIncident(scopeType, scopeID, profileID, destinationID string, now time.Time) (Incident, []IncidentDelivery, []IncidentDelivery, error) {
	scopeType, scopeID, _, profileID, destinationID, err := normalizeIncidentScope(scopeType, scopeID, "", profileID, destinationID)
	if err != nil {
		// Account resolve passes account via scope only; re-derive without
		// the account equality check.
		if scopeType == IncidentScopeAccount {
			scopeType = IncidentScopeAccount
		} else {
			return Incident{}, nil, nil, err
		}
	}
	now = incidentTime(now)
	stamp := now.Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var incident Incident
	if err := scanIncident(conn.QueryRowContext(ctx, `SELECT id, scope_type, scope_id, account_id, profile_id, destination_id, kind, status, failure_code, failure_message, consecutive_failures, first_seen_at, last_seen_at, ended_at, created_at, updated_at FROM operational_incidents WHERE scope_type = ? AND scope_id = ? AND profile_id = ? AND destination_id = ? AND status = ?`, scopeType, scopeID, profileID, destinationID, IncidentStatusActive), &incident); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			committed = true
			return Incident{}, nil, nil, nil
		}
		if isNoSuchTable(err) {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			committed = true
			return Incident{}, nil, nil, nil
		}
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE operational_incidents SET status = ?, ended_at = ?, updated_at = ? WHERE id = ? AND status = ?`, IncidentStatusResolved, stamp, stamp, incident.ID, IncidentStatusActive); err != nil {
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	incident.Status = IncidentStatusResolved
	incident.EndedAt = stamp
	incident.UpdatedAt = stamp

	rows, err := conn.QueryContext(ctx, `SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE incident_id = ? AND kind = ?`, incident.ID, IncidentDeliveryFailure)
	if err != nil {
		if isNoSuchTable(err) {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			committed = true
			return incident, nil, nil, nil
		}
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	type failureRow struct {
		delivery IncidentDelivery
	}
	failures := []failureRow{}
	for rows.Next() {
		var delivery IncidentDelivery
		if err := scanIncidentDelivery(rows, &delivery); err != nil {
			rows.Close()
			return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
		}
		failures = append(failures, failureRow{delivery: delivery})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	cancelled := []IncidentDelivery{}
	recoveries := []IncidentDelivery{}
	for _, row := range failures {
		delivery := row.delivery
		switch delivery.Status {
		case DeliveryDelivered:
			id, err := newObservationID("incident-delivery")
			if err != nil {
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			recovery := IncidentDelivery{
				ID:            id,
				IncidentID:    incident.ID,
				DestinationID: delivery.DestinationID,
				Kind:          IncidentDeliveryRecovery,
				Status:        DeliveryPending,
				CreatedAt:     stamp,
				UpdatedAt:     stamp,
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO operational_deliveries (id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id) VALUES (?, ?, ?, ?, ?, 0, '', '', ?, ?, '', 0) ON CONFLICT(incident_id, destination_id, kind) DO NOTHING`, recovery.ID, recovery.IncidentID, recovery.DestinationID, recovery.Kind, recovery.Status, recovery.CreatedAt, recovery.UpdatedAt); err != nil {
				if isNoSuchTable(err) {
					break
				}
				if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
					continue
				}
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			recoveries = append(recoveries, recovery)
		case DeliveryPending, DeliveryRetry:
			if delivery.Status == DeliveryPending && hasLiveClaimLease(delivery.NextAttemptAt, now) {
				continue
			}
			result, err := conn.ExecContext(ctx, `UPDATE operational_deliveries SET status = ?, last_error = ?, updated_at = ? WHERE id = ? AND status IN ('pending', 'retry')`, DeliveryPermanentFailure, incidentCancelledMessage, stamp, delivery.ID)
			if err != nil {
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
			}
			if affected == 1 {
				delivery.Status = DeliveryPermanentFailure
				delivery.LastError = incidentCancelledMessage
				delivery.UpdatedAt = stamp
				cancelled = append(cancelled, delivery)
			}
		default:
			// Terminal failures stay as history; no recovery without a
			// delivered failure.
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Incident{}, nil, nil, fmt.Errorf("resolve operational incident: %w", err)
	}
	committed = true
	return incident, recoveries, cancelled, nil
}

// GetActiveIncident returns the active incident for a scope or
// ErrIncidentNotFound when none exists.
func (s *Store) GetActiveIncident(scopeType, scopeID, profileID, destinationID string) (Incident, error) {
	scopeType, scopeID, _, profileID, destinationID, err := normalizeIncidentScope(scopeType, scopeID, "", profileID, destinationID)
	if err != nil {
		return Incident{}, err
	}
	var incident Incident
	if err := scanIncident(s.db.QueryRow(`SELECT id, scope_type, scope_id, account_id, profile_id, destination_id, kind, status, failure_code, failure_message, consecutive_failures, first_seen_at, last_seen_at, ended_at, created_at, updated_at FROM operational_incidents WHERE scope_type = ? AND scope_id = ? AND profile_id = ? AND destination_id = ? AND status = ?`, scopeType, scopeID, profileID, destinationID, IncidentStatusActive), &incident); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Incident{}, fmt.Errorf("%w: %s %s", ErrIncidentNotFound, scopeType, scopeID)
		}
		if isNoSuchTable(err) {
			return Incident{}, fmt.Errorf("%w: %s %s", ErrIncidentNotFound, scopeType, scopeID)
		}
		return Incident{}, fmt.Errorf("show operational incident: %w", err)
	}
	return incident, nil
}

// GetIncident returns one incident by id.
func (s *Store) GetIncident(id string) (Incident, error) {
	if strings.TrimSpace(id) == "" {
		return Incident{}, fmt.Errorf("%w: incident id is required", ErrIncidentInvalid)
	}
	var incident Incident
	if err := scanIncident(s.db.QueryRow(`SELECT id, scope_type, scope_id, account_id, profile_id, destination_id, kind, status, failure_code, failure_message, consecutive_failures, first_seen_at, last_seen_at, ended_at, created_at, updated_at FROM operational_incidents WHERE id = ?`, id), &incident); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Incident{}, fmt.Errorf("%w: %s", ErrIncidentNotFound, id)
		}
		if isNoSuchTable(err) {
			return Incident{}, fmt.Errorf("%w: %s", ErrIncidentNotFound, id)
		}
		return Incident{}, fmt.Errorf("show operational incident: %w", err)
	}
	return incident, nil
}

// ListIncidents returns incidents ordered by first_seen_at descending.
// Empty scope filters return all incidents.
func (s *Store) ListIncidents(status, scopeType string, limit int) ([]Incident, error) {
	query := `SELECT id, scope_type, scope_id, account_id, profile_id, destination_id, kind, status, failure_code, failure_message, consecutive_failures, first_seen_at, last_seen_at, ended_at, created_at, updated_at FROM operational_incidents WHERE 1 = 1`
	args := []any{}
	if strings.TrimSpace(status) != "" {
		query += ` AND status = ?`
		args = append(args, strings.TrimSpace(status))
	}
	if strings.TrimSpace(scopeType) != "" {
		query += ` AND scope_type = ?`
		args = append(args, strings.TrimSpace(scopeType))
	}
	query += ` ORDER BY first_seen_at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		if isNoSuchTable(err) {
			return []Incident{}, nil
		}
		return nil, fmt.Errorf("list operational incidents: %w", err)
	}
	defer rows.Close()
	result := []Incident{}
	for rows.Next() {
		var incident Incident
		if err := scanIncident(rows, &incident); err != nil {
			return nil, fmt.Errorf("list operational incidents: %w", err)
		}
		result = append(result, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list operational incidents: %w", err)
	}
	return result, nil
}

// ListIncidentDeliveries returns all deliveries for an incident ordered by creation.
func (s *Store) ListIncidentDeliveries(incidentID string) ([]IncidentDelivery, error) {
	if strings.TrimSpace(incidentID) == "" {
		return nil, fmt.Errorf("%w: incident id is required", ErrIncidentInvalid)
	}
	rows, err := s.db.Query(`SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE incident_id = ? ORDER BY created_at, id`, incidentID)
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

// GetIncidentDelivery returns one incident delivery by id.
func (s *Store) GetIncidentDelivery(id string) (IncidentDelivery, error) {
	if strings.TrimSpace(id) == "" {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery id is required", ErrIncidentInvalid)
	}
	var delivery IncidentDelivery
	if err := scanIncidentDelivery(s.db.QueryRow(`SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE id = ?`, id), &delivery); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IncidentDelivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		if isNoSuchTable(err) {
			return IncidentDelivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		return IncidentDelivery{}, fmt.Errorf("show operational delivery: %w", err)
	}
	return delivery, nil
}

// ListDueIncidentDeliveries returns pending and retry incident deliveries
// ready for an attempt: attempts below budget, backoff reached, destination
// still enabled. Unlike availability, no episode or link check applies:
// incident scope already fixed eligibility at creation, and disabling a
// destination pauses without consuming attempts.
func (s *Store) ListDueIncidentDeliveries(now time.Time) ([]IncidentDelivery, error) {
	now = incidentTime(now)
	rows, err := s.db.Query(`SELECT t.id, t.incident_id, t.destination_id, t.kind, t.status, t.attempts, t.next_attempt_at, t.last_error, t.created_at, t.updated_at, t.delivered_at, t.message_id FROM operational_deliveries t JOIN telegram_destinations d ON d.id = t.destination_id WHERE t.status IN ('pending', 'retry') AND t.attempts < ? AND d.enabled = 1 ORDER BY t.created_at, t.id`, MaxDeliveryAttempts)
	if err != nil {
		if isNoSuchTable(err) {
			return []IncidentDelivery{}, nil
		}
		return nil, fmt.Errorf("list due operational deliveries: %w", err)
	}
	defer rows.Close()
	result := []IncidentDelivery{}
	for rows.Next() {
		var delivery IncidentDelivery
		if err := scanIncidentDelivery(rows, &delivery); err != nil {
			return nil, fmt.Errorf("list due operational deliveries: %w", err)
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
		result = append(result, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due operational deliveries: %w", err)
	}
	return result, nil
}

// BeginIncidentDeliveryAttempt exclusively claims one due incident delivery
// before a Telegram call and consumes one attempt. The claim sets
// next_attempt_at to now+lease so concurrent processes do not double-send.
func (s *Store) BeginIncidentDeliveryAttempt(id string, now time.Time) (IncidentDelivery, error) {
	if strings.TrimSpace(id) == "" {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery id is required", ErrIncidentInvalid)
	}
	now = incidentTime(now)
	stamp := now.Format(time.RFC3339Nano)
	lease := now.Add(deliveryClaimLease).Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var delivery IncidentDelivery
	if err = scanIncidentDelivery(conn.QueryRowContext(ctx, `SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE id = ?`, id), &delivery); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IncidentDelivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	if delivery.Status != DeliveryPending && delivery.Status != DeliveryRetry {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery %s is %s", ErrDeliveryNotDue, id, delivery.Status)
	}
	if delivery.Attempts >= MaxDeliveryAttempts {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery %s exhausted its attempts", ErrDeliveryNotDue, id)
	}
	if strings.TrimSpace(delivery.NextAttemptAt) != "" {
		if next, err := time.Parse(time.RFC3339Nano, delivery.NextAttemptAt); err == nil && next.After(now) {
			return IncidentDelivery{}, fmt.Errorf("%w: delivery %s backs off until %s", ErrDeliveryNotDue, id, delivery.NextAttemptAt)
		}
	}
	var enabled int
	if err := conn.QueryRowContext(ctx, `SELECT enabled FROM telegram_destinations WHERE id = ?`, delivery.DestinationID).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IncidentDelivery{}, fmt.Errorf("%w: delivery %s lost its destination", ErrDeliveryNotDue, id)
		}
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	if enabled != 1 {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery %s destination is disabled", ErrDeliveryNotDue, id)
	}
	oldStatus, oldAttempts, oldUpdated := delivery.Status, delivery.Attempts, delivery.UpdatedAt
	result, err := conn.ExecContext(ctx, `UPDATE operational_deliveries SET status = ?, attempts = ?, next_attempt_at = ?, updated_at = ? WHERE id = ? AND status = ? AND attempts = ? AND updated_at = ?`, DeliveryPending, oldAttempts+1, lease, stamp, id, oldStatus, oldAttempts, oldUpdated)
	if err != nil {
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	if affected == 0 {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery %s claimed concurrently", ErrDeliveryNotDue, id)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return IncidentDelivery{}, fmt.Errorf("begin operational delivery: %w", err)
	}
	committed = true
	delivery.Status = DeliveryPending
	delivery.Attempts = oldAttempts + 1
	delivery.NextAttemptAt = lease
	delivery.UpdatedAt = stamp
	return delivery, nil
}

// RecordIncidentDeliveryResult saves the confirmed Telegram outcome after the
// call, fenced on the claim so concurrent winners are never overwritten.
func (s *Store) RecordIncidentDeliveryResult(id string, claimed IncidentDelivery, result DeliveryResult, now time.Time) (IncidentDelivery, error) {
	if strings.TrimSpace(id) == "" {
		return IncidentDelivery{}, fmt.Errorf("%w: delivery id is required", ErrIncidentInvalid)
	}
	switch result.Status {
	case DeliveryDelivered, DeliveryRetry, DeliveryPermanentFailure:
	default:
		return IncidentDelivery{}, fmt.Errorf("%w: status %q", ErrDeliveryInvalid, result.Status)
	}
	if strings.TrimSpace(claimed.ID) == "" || claimed.ID != id {
		return IncidentDelivery{}, fmt.Errorf("%w: claim mismatch for delivery %s", ErrDeliveryInvalid, id)
	}
	message := strings.TrimSpace(result.LastError)
	if len(message) > 2048 {
		message = message[:2048]
	}
	now = incidentTime(now)
	stamp := now.Format(time.RFC3339Nano)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var current IncidentDelivery
	if err = scanIncidentDelivery(conn.QueryRowContext(ctx, `SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE id = ?`, id), &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IncidentDelivery{}, fmt.Errorf("%w: %s", ErrDeliveryNotFound, id)
		}
		return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
	}
	if current.Status != DeliveryPending || current.Attempts != claimed.Attempts || current.UpdatedAt != claimed.UpdatedAt {
		return current, fmt.Errorf("%w: delivery %s", ErrDeliveryConflict, id)
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
			nextAttemptAt = stamp
		}
	}
	if finalStatus == DeliveryPermanentFailure && message == "" {
		message = "telegram delivery failed"
	}
	fenced, err := conn.ExecContext(ctx, `UPDATE operational_deliveries SET status = ?, next_attempt_at = ?, last_error = ?, updated_at = ?, delivered_at = ?, message_id = ? WHERE id = ? AND status = ? AND attempts = ? AND updated_at = ?`, finalStatus, nextAttemptAt, message, stamp, deliveredAt, messageID, id, DeliveryPending, claimed.Attempts, claimed.UpdatedAt)
	if err != nil {
		return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
	}
	affected, err := fenced.RowsAffected()
	if err != nil {
		return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
	}
	if affected == 0 {
		var fresh IncidentDelivery
		if freshErr := scanIncidentDelivery(conn.QueryRowContext(ctx, `SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE id = ?`, id), &fresh); freshErr != nil {
			return current, fmt.Errorf("%w: delivery %s", ErrDeliveryConflict, id)
		}
		return fresh, fmt.Errorf("%w: delivery %s", ErrDeliveryConflict, id)
	}
	if finalStatus == DeliveryDelivered && current.Kind == IncidentDeliveryFailure {
		// A failure delivered after its incident already resolved (its send
		// was in flight during resolve) still owes its recovery pair.
		// Recoveries are never created for recovery rows themselves: a
		// failure in an operational notification never creates another
		// incident or notification.
		var incidentStatus string
		if err := conn.QueryRowContext(ctx, `SELECT status FROM operational_incidents WHERE id = ?`, current.IncidentID).Scan(&incidentStatus); err != nil {
			if !errors.Is(err, sql.ErrNoRows) && !isNoSuchTable(err) {
				return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
			}
		} else if incidentStatus == IncidentStatusResolved {
			recoveryID, err := newObservationID("incident-delivery")
			if err != nil {
				return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO operational_deliveries (id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id) VALUES (?, ?, ?, ?, ?, 0, '', '', ?, ?, '', 0) ON CONFLICT(incident_id, destination_id, kind) DO NOTHING`, recoveryID, current.IncidentID, current.DestinationID, IncidentDeliveryRecovery, DeliveryPending, stamp, stamp); err != nil {
				if !isNoSuchTable(err) && !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
					return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
				}
			}
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return IncidentDelivery{}, fmt.Errorf("record operational delivery: %w", err)
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

// ReapExpiredIncidentClaims finalizes pending incident rows stuck after a
// stopped fifth claim, mirroring availability reaping.
func (s *Store) ReapExpiredIncidentClaims(now time.Time) ([]IncidentDelivery, error) {
	now = incidentTime(now)
	rows, err := s.db.Query(`SELECT id, incident_id, destination_id, kind, status, attempts, next_attempt_at, last_error, created_at, updated_at, delivered_at, message_id FROM operational_deliveries WHERE status = ? AND attempts >= ?`, DeliveryPending, MaxDeliveryAttempts)
	if err != nil {
		if isNoSuchTable(err) {
			return []IncidentDelivery{}, nil
		}
		return nil, fmt.Errorf("reap operational deliveries: %w", err)
	}
	defer rows.Close()
	candidates := []IncidentDelivery{}
	for rows.Next() {
		var delivery IncidentDelivery
		if err := scanIncidentDelivery(rows, &delivery); err != nil {
			return nil, fmt.Errorf("reap operational deliveries: %w", err)
		}
		if strings.TrimSpace(delivery.NextAttemptAt) == "" {
			candidates = append(candidates, delivery)
			continue
		}
		if next, err := time.Parse(time.RFC3339Nano, delivery.NextAttemptAt); err != nil || !next.After(now) {
			candidates = append(candidates, delivery)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reap operational deliveries: %w", err)
	}
	reaped := []IncidentDelivery{}
	for _, candidate := range candidates {
		message := strings.TrimSpace(candidate.LastError)
		if message == "" {
			message = "telegram delivery failed after 5 attempts"
		} else if !strings.Contains(message, "5 attempts") {
			message = message + " (gave up after 5 attempts)"
			if len(message) > 2048 {
				message = message[:2048]
			}
		}
		stamp := now.Format(time.RFC3339Nano)
		result, err := s.db.Exec(`UPDATE operational_deliveries SET status = ?, last_error = ?, updated_at = ? WHERE id = ? AND status = ? AND attempts >= ?`, DeliveryPermanentFailure, message, stamp, candidate.ID, DeliveryPending, MaxDeliveryAttempts)
		if err != nil {
			if isNoSuchTable(err) {
				return reaped, nil
			}
			return nil, fmt.Errorf("reap operational deliveries: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("reap operational deliveries: %w", err)
		}
		if affected == 0 {
			continue
		}
		candidate.Status = DeliveryPermanentFailure
		candidate.LastError = message
		candidate.UpdatedAt = stamp
		reaped = append(reaped, candidate)
	}
	return reaped, nil
}

type incidentScanner interface {
	Scan(dest ...any) error
}

func scanIncident(scanner incidentScanner, incident *Incident) error {
	return scanner.Scan(&incident.ID, &incident.ScopeType, &incident.ScopeID, &incident.AccountID, &incident.ProfileID, &incident.DestinationID, &incident.Kind, &incident.Status, &incident.FailureCode, &incident.FailureMessage, &incident.ConsecutiveFailures, &incident.FirstSeenAt, &incident.LastSeenAt, &incident.EndedAt, &incident.CreatedAt, &incident.UpdatedAt)
}

func scanIncidentDelivery(scanner incidentScanner, delivery *IncidentDelivery) error {
	return scanner.Scan(&delivery.ID, &delivery.IncidentID, &delivery.DestinationID, &delivery.Kind, &delivery.Status, &delivery.Attempts, &delivery.NextAttemptAt, &delivery.LastError, &delivery.CreatedAt, &delivery.UpdatedAt, &delivery.DeliveredAt, &delivery.MessageID)
}
