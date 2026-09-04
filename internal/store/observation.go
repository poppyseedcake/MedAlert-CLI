package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	ObservationRunRunning     = "running"
	ObservationRunComplete    = "complete"
	ObservationRunFailed      = "failed"
	ObservationRunPartial     = "partial"
	ObservationRunCancelled   = "cancelled"
	ObservationRunConflicting = "conflicting"
	ObservationRunStale       = "stale"

	observationRunLease = 15 * time.Minute
	portalLocationName  = "Europe/Warsaw"
)

var portalLocation = mustLoadPortalLocation()

var (
	// ErrObservationRunActive is returned when another process is checking
	// the same profile.
	ErrObservationRunActive = errors.New("observation run is already active")
	// ErrObservationRunNotFound is returned for an unknown observation run.
	ErrObservationRunNotFound = errors.New("observation run not found")
	// ErrObservationRunStale is returned when the profile changed during a run.
	ErrObservationRunStale = errors.New("observation run is stale")
	// ErrObservationRunConflicting is returned for incompatible slot records.
	ErrObservationRunConflicting = errors.New("observation run has conflicting slots")
	// ErrObservationRunInvalid is returned for invalid run status or slot data.
	ErrObservationRunInvalid = errors.New("invalid observation run")
	// ErrProfileDisabled is returned when observation is paused for a profile.
	ErrProfileDisabled = errors.New("profile is disabled")
)

// ObservationRun records one attempt to observe a profile. A run is complete
// only after the full Medicover result has been accepted and reconciled.
type ObservationRun struct {
	ID               string `json:"id"`
	ProfileID        string `json:"profile_id"`
	ProfileUpdatedAt string `json:"profile_updated_at"`
	Status           string `json:"status"`
	StartedAt        string `json:"started_at"`
	CompletedAt      string `json:"completed_at,omitempty"`
	SlotCount        int    `json:"slot_count"`
	ErrorCode        string `json:"error_code,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
}

// ObservationSlot is the safe domain representation of one available slot.
// It contains display data only; authentication and transport data never
// enter the store.
type ObservationSlot struct {
	Identity       string `json:"identity"`
	StableIdentity string `json:"stable_identity"`
	BookingString  string `json:"booking_string,omitempty"`
	Time           string `json:"time,omitempty"`
	Clinic         string `json:"clinic,omitempty"`
	Doctor         string `json:"doctor,omitempty"`
	Specialty      string `json:"specialty,omitempty"`
	VisitType      string `json:"visit_type,omitempty"`
}

// AvailabilityEpisode records one continuous period in which a slot was
// visible for one profile.
type AvailabilityEpisode struct {
	ID             string `json:"id"`
	ProfileID      string `json:"profile_id"`
	SlotIdentity   string `json:"slot_identity"`
	StableIdentity string `json:"stable_identity"`
	BookingString  string `json:"booking_string,omitempty"`
	Time           string `json:"time"`
	Clinic         string `json:"clinic,omitempty"`
	Doctor         string `json:"doctor,omitempty"`
	Specialty      string `json:"specialty,omitempty"`
	VisitType      string `json:"visit_type,omitempty"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at,omitempty"`
	Active         bool   `json:"active"`
}

// ObservationReconciliation describes the durable changes made by one
// complete run.
type ObservationReconciliation struct {
	Run           ObservationRun        `json:"run"`
	NewEpisodes   []AvailabilityEpisode `json:"new_episodes"`
	EndedEpisodes []AvailabilityEpisode `json:"ended_episodes"`
}

// BeginObservationRun reserves a profile for one observation attempt and
// stores the profile version used by that attempt.
func (s *Store) BeginObservationRun(profileID string, now time.Time) (ObservationRun, error) {
	if !profileIDPattern.MatchString(profileID) {
		return ObservationRun{}, fmt.Errorf("%w: profile id %q", ErrObservationRunInvalid, profileID)
	}
	now = observationTime(now)
	startedAt := now.Format(time.RFC3339Nano)
	transaction, err := beginObservationTransaction(s.db)
	if err != nil {
		return ObservationRun{}, fmt.Errorf("begin observation run: %w", err)
	}
	defer func() {
		_ = transaction.Rollback()
		_ = transaction.Close()
	}()

	var profileUpdatedAt string
	var enabled int
	if err := transaction.QueryRow("SELECT updated_at, enabled FROM profiles WHERE id = ?", profileID).Scan(&profileUpdatedAt, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ObservationRun{}, fmt.Errorf("%w: %s", ErrProfileNotFound, profileID)
		}
		return ObservationRun{}, fmt.Errorf("begin observation run: %w", err)
	}
	if enabled != 1 {
		return ObservationRun{}, fmt.Errorf("%w: %s", ErrProfileDisabled, profileID)
	}
	var activeID, activeStartedAt string
	activeErr := transaction.QueryRow("SELECT id, started_at FROM observation_runs WHERE profile_id = ? AND status = ? ORDER BY started_at DESC LIMIT 1", profileID, ObservationRunRunning).Scan(&activeID, &activeStartedAt)
	if activeErr == nil {
		started, parseErr := time.Parse(time.RFC3339Nano, activeStartedAt)
		if parseErr != nil || now.Before(started.Add(observationRunLease)) {
			return ObservationRun{}, fmt.Errorf("%w: %s", ErrObservationRunActive, activeID)
		}
		if _, err := transaction.Exec(`UPDATE observation_runs SET status = ?, completed_at = ?, error_code = ?, error_message = ? WHERE id = ? AND status = ?`, ObservationRunStale, startedAt, "stale_run", "observation run lease expired", activeID, ObservationRunRunning); err != nil {
			return ObservationRun{}, fmt.Errorf("begin observation run: %w", err)
		}
	} else if !errors.Is(activeErr, sql.ErrNoRows) {
		return ObservationRun{}, fmt.Errorf("begin observation run: %w", activeErr)
	}

	id, err := newObservationID("run")
	if err != nil {
		return ObservationRun{}, fmt.Errorf("begin observation run: %w", err)
	}
	if _, err := transaction.Exec(`INSERT INTO observation_runs (id, profile_id, profile_updated_at, status, started_at) VALUES (?, ?, ?, ?, ?)`, id, profileID, profileUpdatedAt, ObservationRunRunning, startedAt); err != nil {
		return ObservationRun{}, fmt.Errorf("begin observation run: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return ObservationRun{}, fmt.Errorf("begin observation run: %w", err)
	}
	return ObservationRun{ID: id, ProfileID: profileID, ProfileUpdatedAt: profileUpdatedAt, Status: ObservationRunRunning, StartedAt: startedAt}, nil
}

// FailObservationRun records a failed, partial, cancelled, conflicting, or
// stale attempt without changing availability episodes.
func (s *Store) FailObservationRun(runID, status, errorCode, errorMessage string, at time.Time) (ObservationRun, error) {
	if !validFailedRunStatus(status) {
		return ObservationRun{}, fmt.Errorf("%w: status %q", ErrObservationRunInvalid, status)
	}
	at = observationTime(at)
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if len(errorCode) > 128 || len(errorMessage) > 2048 {
		return ObservationRun{}, fmt.Errorf("%w: error details are too long", ErrObservationRunInvalid)
	}
	result, err := s.db.Exec(`UPDATE observation_runs SET status = ?, completed_at = ?, error_code = ?, error_message = ? WHERE id = ? AND status = ?`, status, at.Format(time.RFC3339Nano), errorCode, errorMessage, runID, ObservationRunRunning)
	if err != nil {
		return ObservationRun{}, fmt.Errorf("fail observation run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ObservationRun{}, fmt.Errorf("fail observation run: %w", err)
	}
	if affected == 0 {
		return ObservationRun{}, fmt.Errorf("%w: %s", ErrObservationRunNotFound, runID)
	}
	return s.getObservationRun(runID)
}

// ReconcileObservationRun atomically completes a run and updates the active
// availability episodes for its profile.
func (s *Store) ReconcileObservationRun(runID string, slots []ObservationSlot, now time.Time) (ObservationReconciliation, error) {
	now = observationTime(now)
	endedAt := now.Format(time.RFC3339Nano)
	canonical, err := canonicalObservationSlots(slots)
	if err != nil {
		return ObservationReconciliation{}, err
	}
	transaction, err := beginObservationTransaction(s.db)
	if err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	defer func() {
		_ = transaction.Rollback()
		_ = transaction.Close()
	}()

	run, err := scanObservationRun(transaction.QueryRow(`SELECT id, profile_id, profile_updated_at, status, started_at, completed_at, slot_count, error_code, error_message FROM observation_runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return ObservationReconciliation{}, fmt.Errorf("%w: %s", ErrObservationRunNotFound, runID)
	}
	if err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	if run.Status != ObservationRunRunning {
		return ObservationReconciliation{}, fmt.Errorf("%w: %s", ErrObservationRunInvalid, runID)
	}

	var currentProfileUpdatedAt string
	var enabled int
	profileErr := transaction.QueryRow("SELECT updated_at, enabled FROM profiles WHERE id = ?", run.ProfileID).Scan(&currentProfileUpdatedAt, &enabled)
	if errors.Is(profileErr, sql.ErrNoRows) {
		return finishStaleRun(transaction, run, endedAt, "profile was deleted")
	}
	if profileErr != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", profileErr)
	}
	if currentProfileUpdatedAt != run.ProfileUpdatedAt || enabled != 1 {
		return finishStaleRun(transaction, run, endedAt, "profile changed during the run")
	}

	activeEpisodes, err := listActiveEpisodes(transaction, run.ProfileID)
	if err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	matched := make([]bool, len(canonical))
	newEpisodes := make([]AvailabilityEpisode, 0)
	endedEpisodes := make([]AvailabilityEpisode, 0)
	for _, episode := range activeEpisodes {
		match := -1
		for index, slot := range canonical {
			if matched[index] || !sameLogicalSlot(episode, slot) {
				continue
			}
			match = index
			break
		}
		if match >= 0 && !slotHasPassed(canonical[match].Time, now) {
			matched[match] = true
			updated := episodeFromSlot(episode.ID, run.ProfileID, episode.StartedAt, canonical[match], true)
			if err := updateEpisode(transaction, updated); err != nil {
				return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
			}
			continue
		}
		ended := episode
		ended.Active = false
		ended.EndedAt = endedAt
		if _, err := transaction.Exec("UPDATE availability_episodes SET active = 0, ended_at = ? WHERE id = ? AND active = 1", endedAt, episode.ID); err != nil {
			return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
		}
		endedEpisodes = append(endedEpisodes, ended)
	}
	for index, slot := range canonical {
		if matched[index] || slotHasPassed(slot.Time, now) {
			continue
		}
		id, err := newObservationID("episode")
		if err != nil {
			return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
		}
		episode := episodeFromSlot(id, run.ProfileID, endedAt, slot, true)
		if err := insertEpisode(transaction, episode); err != nil {
			return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
		}
		newEpisodes = append(newEpisodes, episode)
	}
	if _, err := transaction.Exec(`UPDATE observation_runs SET status = ?, completed_at = ?, slot_count = ?, error_code = '', error_message = '' WHERE id = ? AND status = ?`, ObservationRunComplete, endedAt, len(canonical), runID, ObservationRunRunning); err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	run.Status = ObservationRunComplete
	run.CompletedAt = endedAt
	run.SlotCount = len(canonical)
	run.ErrorCode = ""
	run.ErrorMessage = ""
	return ObservationReconciliation{Run: run, NewEpisodes: newEpisodes, EndedEpisodes: endedEpisodes}, nil
}

// ListObservationRuns returns a profile's runs from newest to oldest.
func (s *Store) ListObservationRuns(profileID string) ([]ObservationRun, error) {
	rows, err := s.db.Query(`SELECT id, profile_id, profile_updated_at, status, started_at, completed_at, slot_count, error_code, error_message FROM observation_runs WHERE profile_id = ? ORDER BY started_at DESC, id DESC`, profileID)
	if err != nil {
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

// ListAvailabilityEpisodes returns all episodes for a profile from newest to
// oldest. Active episodes remain in the result indefinitely.
func (s *Store) ListAvailabilityEpisodes(profileID string) ([]AvailabilityEpisode, error) {
	rows, err := s.db.Query(`SELECT id, profile_id, slot_identity, stable_identity, booking_string, appointment_time, clinic, doctor, specialty, visit_type, started_at, ended_at, active FROM availability_episodes WHERE profile_id = ? ORDER BY started_at DESC, id DESC`, profileID)
	if err != nil {
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

type observationScanner interface {
	Scan(dest ...any) error
}

// observationTransaction starts with an immediate SQLite lock. This closes
// the read-then-insert race when two medalert processes check one profile.
type observationTransaction struct {
	connection *sql.Conn
	context    context.Context
	done       bool
}

func beginObservationTransaction(database *sql.DB) (*observationTransaction, error) {
	ctx := context.Background()
	connection, err := database.Conn(ctx)
	if err != nil {
		return nil, err
	}
	transaction := &observationTransaction{connection: connection, context: ctx}
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return transaction, nil
}

func (t *observationTransaction) Exec(query string, args ...any) (sql.Result, error) {
	return t.connection.ExecContext(t.context, query, args...)
}

func (t *observationTransaction) Query(query string, args ...any) (*sql.Rows, error) {
	return t.connection.QueryContext(t.context, query, args...)
}

func (t *observationTransaction) QueryRow(query string, args ...any) *sql.Row {
	return t.connection.QueryRowContext(t.context, query, args...)
}

func (t *observationTransaction) Commit() error {
	if t.done {
		return nil
	}
	if _, err := t.connection.ExecContext(t.context, "COMMIT"); err != nil {
		return err
	}
	t.done = true
	return nil
}

func (t *observationTransaction) Rollback() error {
	if t.done {
		return nil
	}
	_, err := t.connection.ExecContext(t.context, "ROLLBACK")
	t.done = true
	return err
}

func (t *observationTransaction) Close() error {
	return t.connection.Close()
}

func (s *Store) getObservationRun(id string) (ObservationRun, error) {
	run, err := scanObservationRun(s.db.QueryRow(`SELECT id, profile_id, profile_updated_at, status, started_at, completed_at, slot_count, error_code, error_message FROM observation_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ObservationRun{}, fmt.Errorf("%w: %s", ErrObservationRunNotFound, id)
	}
	if err != nil {
		return ObservationRun{}, fmt.Errorf("read observation run: %w", err)
	}
	return run, nil
}

func scanObservationRun(scanner observationScanner) (ObservationRun, error) {
	var run ObservationRun
	err := scanner.Scan(&run.ID, &run.ProfileID, &run.ProfileUpdatedAt, &run.Status, &run.StartedAt, &run.CompletedAt, &run.SlotCount, &run.ErrorCode, &run.ErrorMessage)
	return run, err
}

func listActiveEpisodes(scanner interface {
	Query(string, ...any) (*sql.Rows, error)
}, profileID string) ([]AvailabilityEpisode, error) {
	rows, err := scanner.Query(`SELECT id, profile_id, slot_identity, stable_identity, booking_string, appointment_time, clinic, doctor, specialty, visit_type, started_at, ended_at, active FROM availability_episodes WHERE profile_id = ? AND active = 1 ORDER BY started_at, id`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AvailabilityEpisode{}
	for rows.Next() {
		episode, err := scanAvailabilityEpisode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, episode)
	}
	return result, rows.Err()
}

func scanAvailabilityEpisode(scanner observationScanner) (AvailabilityEpisode, error) {
	var episode AvailabilityEpisode
	var active int
	err := scanner.Scan(&episode.ID, &episode.ProfileID, &episode.SlotIdentity, &episode.StableIdentity, &episode.BookingString, &episode.Time, &episode.Clinic, &episode.Doctor, &episode.Specialty, &episode.VisitType, &episode.StartedAt, &episode.EndedAt, &active)
	episode.Active = active == 1
	return episode, err
}

func insertEpisode(connection interface {
	Exec(string, ...any) (sql.Result, error)
}, episode AvailabilityEpisode) error {
	_, err := connection.Exec(`INSERT INTO availability_episodes (id, profile_id, slot_identity, stable_identity, booking_string, appointment_time, clinic, doctor, specialty, visit_type, started_at, ended_at, active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, episode.ID, episode.ProfileID, episode.SlotIdentity, episode.StableIdentity, episode.BookingString, episode.Time, episode.Clinic, episode.Doctor, episode.Specialty, episode.VisitType, episode.StartedAt, episode.EndedAt, boolInt(episode.Active))
	return err
}

func updateEpisode(connection interface {
	Exec(string, ...any) (sql.Result, error)
}, episode AvailabilityEpisode) error {
	_, err := connection.Exec(`UPDATE availability_episodes SET slot_identity = ?, stable_identity = ?, booking_string = ?, appointment_time = ?, clinic = ?, doctor = ?, specialty = ?, visit_type = ? WHERE id = ? AND active = 1`, episode.SlotIdentity, episode.StableIdentity, episode.BookingString, episode.Time, episode.Clinic, episode.Doctor, episode.Specialty, episode.VisitType, episode.ID)
	return err
}

func finishStaleRun(transaction *observationTransaction, run ObservationRun, completedAt, reason string) (ObservationReconciliation, error) {
	if _, err := transaction.Exec(`UPDATE observation_runs SET status = ?, completed_at = ?, error_code = ?, error_message = ? WHERE id = ? AND status = ?`, ObservationRunStale, completedAt, "stale_result", reason, run.ID, ObservationRunRunning); err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return ObservationReconciliation{}, fmt.Errorf("reconcile observation run: %w", err)
	}
	run.Status = ObservationRunStale
	run.CompletedAt = completedAt
	run.ErrorCode = "stale_result"
	run.ErrorMessage = reason
	return ObservationReconciliation{Run: run}, fmt.Errorf("%w: %s", ErrObservationRunStale, reason)
}

func canonicalObservationSlots(slots []ObservationSlot) ([]ObservationSlot, error) {
	result := make([]ObservationSlot, 0, len(slots))
	byIdentity := make(map[string]int, len(slots))
	for _, raw := range slots {
		slot := normalizeObservationSlot(raw)
		if slot.Identity == "" || slot.Time == "" {
			return nil, fmt.Errorf("%w: slot identity and time are required", ErrObservationRunInvalid)
		}
		if slot.StableIdentity == "" {
			slot.StableIdentity = slot.Identity
		}
		if index, ok := byIdentity[slot.Identity]; ok {
			if !sameSlotDetails(result[index], slot) {
				return nil, fmt.Errorf("%w: slot %q has incompatible records", ErrObservationRunConflicting, slot.Identity)
			}
			continue
		}
		byIdentity[slot.Identity] = len(result)
		result = append(result, slot)
	}
	return result, nil
}

func normalizeObservationSlot(slot ObservationSlot) ObservationSlot {
	slot.Identity = strings.TrimSpace(slot.Identity)
	slot.StableIdentity = strings.TrimSpace(slot.StableIdentity)
	slot.BookingString = strings.TrimSpace(slot.BookingString)
	slot.Time = strings.TrimSpace(slot.Time)
	slot.Clinic = strings.TrimSpace(slot.Clinic)
	slot.Doctor = strings.TrimSpace(slot.Doctor)
	slot.Specialty = strings.TrimSpace(slot.Specialty)
	slot.VisitType = strings.TrimSpace(slot.VisitType)
	return slot
}

func sameSlotDetails(left, right ObservationSlot) bool {
	return left.StableIdentity == right.StableIdentity && left.BookingString == right.BookingString && left.Time == right.Time && left.Clinic == right.Clinic && left.Doctor == right.Doctor && left.Specialty == right.Specialty && left.VisitType == right.VisitType
}

func sameLogicalSlot(episode AvailabilityEpisode, slot ObservationSlot) bool {
	if episode.SlotIdentity == slot.Identity {
		return true
	}
	if episode.StableIdentity == "" || episode.StableIdentity != slot.StableIdentity {
		return false
	}
	return episode.SlotIdentity == episode.StableIdentity || slot.Identity == slot.StableIdentity
}

func episodeFromSlot(id, profileID, startedAt string, slot ObservationSlot, active bool) AvailabilityEpisode {
	return AvailabilityEpisode{ID: id, ProfileID: profileID, SlotIdentity: slot.Identity, StableIdentity: slot.StableIdentity, BookingString: slot.BookingString, Time: slot.Time, Clinic: slot.Clinic, Doctor: slot.Doctor, Specialty: slot.Specialty, VisitType: slot.VisitType, StartedAt: startedAt, Active: active}
}

func slotHasPassed(raw string, now time.Time) bool {
	value := strings.TrimSpace(raw)
	if value == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, portalLocation); err == nil {
			return !parsed.After(now)
		}
	}
	return false
}

func mustLoadPortalLocation() *time.Location {
	location, err := time.LoadLocation(portalLocationName)
	if err != nil {
		panic(fmt.Sprintf("load portal time zone %q: %v", portalLocationName, err))
	}
	return location
}

func observationTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func validFailedRunStatus(status string) bool {
	switch status {
	case ObservationRunFailed, ObservationRunPartial, ObservationRunCancelled, ObservationRunConflicting, ObservationRunStale:
		return true
	default:
		return false
	}
}

func validObservationRunStatus(status string) bool {
	switch status {
	case ObservationRunRunning, ObservationRunComplete, ObservationRunFailed, ObservationRunPartial, ObservationRunCancelled, ObservationRunConflicting, ObservationRunStale:
		return true
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func newObservationID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(raw), nil
}
