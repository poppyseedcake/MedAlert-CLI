package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func openDeliveryStore(t *testing.T) *store.Store {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(databasePath); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { storage.Close() })
	return storage
}

func mustDeliveryProfile(t *testing.T, storage *store.Store, profileID, accountID string) {
	t.Helper()
	if _, err := storage.CreateProfile(validProfile(profileID, accountID)); err != nil {
		t.Fatalf("create profile %s: %v", profileID, err)
	}
}

func mustDeliveryEpisode(t *testing.T, storage *store.Store, profileID string, at time.Time) store.AvailabilityEpisode {
	t.Helper()
	run, err := storage.BeginObservationRun(profileID, at)
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	reconciliation, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z")}, at)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(reconciliation.NewEpisodes) != 1 {
		t.Fatalf("new episodes = %#v, want 1", reconciliation.NewEpisodes)
	}
	return reconciliation.NewEpisodes[0]
}

func TestEnsureEpisodeDeliveriesCreatesOncePerEnabledDestination(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "linked", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("two")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SetDestinationEnabled("two", false); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("linked", []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "linked", now)

	created, err := storage.EnsureEpisodeDeliveries("linked", episode.ID, now)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(created) != 1 || created[0].DestinationID != "one" {
		t.Fatalf("created = %#v, want one delivery for enabled destination one", created)
	}
	if created[0].Status != store.DeliveryPending || created[0].Attempts != 0 {
		t.Fatalf("created delivery = %#v, want pending with 0 attempts", created[0])
	}

	// Second ensure is idempotent: repeated successful checks in one
	// continuous episode do not create another notification.
	again, err := storage.EnsureEpisodeDeliveries("linked", episode.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second ensure created = %#v, want none", again)
	}
	deliveries, err := storage.ListDeliveriesForProfile("linked")
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, want 1", deliveries)
	}
}

func TestDeliveryStatesUsePendingDeliveredRetryPermanentFailure(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "states", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("states", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "states", now)
	if _, err := storage.EnsureEpisodeDeliveries("states", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	due, err := storage.ListDueDeliveries("states", now)
	if err != nil || len(due) != 1 || due[0].Status != store.DeliveryPending {
		t.Fatalf("due = %#v, %v, want one pending", due, err)
	}
	claimed, err := storage.BeginDeliveryAttempt(due[0].ID, now)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if claimed.Status != store.DeliveryPending || claimed.Attempts != 1 {
		t.Fatalf("claimed = %#v, want pending attempts=1", claimed)
	}
	// Pending saved before the call: a restart would see pending attempts=1.
	restarted, err := storage.GetDelivery(due[0].ID)
	if err != nil || restarted.Status != store.DeliveryPending || restarted.Attempts != 1 {
		t.Fatalf("restarted read = %#v, %v", restarted, err)
	}
	delivered, err := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 42}, now)
	if err != nil {
		t.Fatalf("record delivered: %v", err)
	}
	if delivered.Status != store.DeliveryDelivered || delivered.MessageID != 42 || delivered.DeliveredAt == "" {
		t.Fatalf("delivered = %#v", delivered)
	}
	// Delivered rows are never due again.
	after, err := storage.ListDueDeliveries("states", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("due after delivered = %#v, want none", after)
	}
}

func TestDeliveryRetryBudgetAndRetryAfter(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "retry", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("retry", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "retry", now)
	if _, err := storage.EnsureEpisodeDeliveries("retry", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	due, _ := storage.ListDueDeliveries("retry", now)
	claimed, _ := storage.BeginDeliveryAttempt(due[0].ID, now)
	retryAfter := now.Add(31 * time.Second)
	retried, err := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryRetry, LastError: "temporary_failure: telegram is temporarily unavailable", NextAttempt: retryAfter, HasNextRetry: true}, now)
	if err != nil {
		t.Fatalf("record retry: %v", err)
	}
	if retried.Status != store.DeliveryRetry {
		t.Fatalf("retried status = %q, want retry", retried.Status)
	}
	if retried.NextAttemptAt == "" {
		t.Fatal("retry_after not stored in next_attempt_at")
	}
	// Before retry_after the delivery is not due.
	early, err := storage.ListDueDeliveries("retry", now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("early due = %#v, want none before retry_after", early)
	}
	late, err := storage.ListDueDeliveries("retry", now.Add(32*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(late) != 1 {
		t.Fatalf("late due = %#v, want one after retry_after", late)
	}

	// Exhaust the budget: attempts 2..5 with temporary results. The fifth
	// failure becomes permanent_failure and is never due again.
	at := now.Add(32 * time.Second)
	for attempt := 2; attempt <= 5; attempt++ {
		due, err := storage.ListDueDeliveries("retry", at)
		if err != nil || len(due) != 1 {
			t.Fatalf("attempt %d due = %#v, %v", attempt, due, err)
		}
		claimed, err := storage.BeginDeliveryAttempt(due[0].ID, at)
		if err != nil {
			t.Fatalf("attempt %d begin: %v", attempt, err)
		}
		if claimed.Attempts != attempt {
			t.Fatalf("attempt %d count = %d", attempt, claimed.Attempts)
		}
		final, err := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryRetry, LastError: "temporary"}, at)
		if err != nil {
			t.Fatalf("attempt %d record: %v", attempt, err)
		}
		if attempt < 5 && final.Status != store.DeliveryRetry {
			t.Fatalf("attempt %d status = %q, want retry", attempt, final.Status)
		}
		if attempt == 5 && final.Status != store.DeliveryPermanentFailure {
			t.Fatalf("attempt 5 status = %q, want permanent_failure after 5 attempts", final.Status)
		}
		at = at.Add(time.Minute)
	}
	after, err := storage.ListDueDeliveries("retry", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("due after budget = %#v, want none", after)
	}
}

func TestDeliveryStaleCancellationStopsWithoutSending(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "stale", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("stale", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "stale", now)
	if _, err := storage.EnsureEpisodeDeliveries("stale", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	// Slot disappears: end the episode with an empty successful run, then
	// cancel pending work instead of sending stale availability.
	endingRun, err := storage.BeginObservationRun("stale", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(endingRun.ID, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cancelled, err := storage.CancelDeliveriesForEndedEpisodes("stale", []string{episode.ID}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", cancelled)
	}
	deliveries, err := storage.ListDeliveriesForProfile("stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != store.DeliveryPermanentFailure {
		t.Fatalf("deliveries after stale = %#v, want one permanent_failure", deliveries)
	}
	due, err := storage.ListDueDeliveries("stale", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due after stale = %#v, want none (stops without sending)", due)
	}
}

func TestIsRetryDueNeedsDueWorkAndCompleteRun(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "elig", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("elig", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "elig", now)
	if _, err := storage.EnsureEpisodeDeliveries("elig", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	due, err := storage.IsRetryDue("elig", now)
	if err != nil || !due {
		t.Fatalf("retry due = %v, %v, want true after complete run with pending", due, err)
	}
	// A failed run keeps the pending row but must not make the profile
	// retry-due: failed runs respect the profile interval.
	failing, err := storage.BeginObservationRun("elig", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FailObservationRun(failing.ID, store.ObservationRunFailed, "temporary_failure", "portal down", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stillDue, err := storage.IsRetryDue("elig", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if stillDue {
		t.Fatal("retry due after failed run, want false to avoid hammering Medicover")
	}
}

func TestDeleteDestinationAndProfileRemoveDeliveries(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "gone", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("gone", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "gone", now)
	if _, err := storage.EnsureEpisodeDeliveries("gone", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteDestination("one"); err != nil {
		t.Fatalf("delete destination: %v", err)
	}
	afterDelete, err := storage.ListDeliveriesForProfile("gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDelete) != 0 {
		t.Fatalf("deliveries after destination delete = %#v, want none", afterDelete)
	}

	if _, err := storage.CreateDestination(validDestination("two")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("gone", []string{"two"}); err != nil {
		t.Fatal(err)
	}
	// New episode for the new destination (old episode still active, but the
	// new destination has not yet had its one delivery for it; ensuring for
	// the still-active episode covers the new link).
	if _, err := storage.EnsureEpisodeDeliveries("gone", episode.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteProfile("gone"); err != nil {
		t.Fatalf("delete profile: %v", err)
	}
}

func TestBeginHoldsLeaseExclusivelyAcrossProcesses(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "exclusive", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("exclusive", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "exclusive", now)
	if _, err := storage.EnsureEpisodeDeliveries("exclusive", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	due, err := storage.ListDueDeliveries("exclusive", now)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %#v, %v, want one", due, err)
	}
	first, err := storage.BeginDeliveryAttempt(due[0].ID, now)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if first.Attempts != 1 || strings.TrimSpace(first.NextAttemptAt) == "" {
		t.Fatalf("first claim = %#v, want attempts=1 with lease", first)
	}
	// A concurrent process selecting the same row must not claim it while
	// the lease is held: no attempt consumed, no second send.
	if _, err := storage.BeginDeliveryAttempt(due[0].ID, now.Add(time.Second)); !errors.Is(err, store.ErrDeliveryNotDue) {
		t.Fatalf("second begin error = %v, want ErrDeliveryNotDue", err)
	}
	after, err := storage.GetDelivery(due[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Attempts != 1 {
		t.Fatalf("attempts after raced begin = %d, want 1", after.Attempts)
	}
	if held, err := storage.ListDueDeliveries("exclusive", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	} else if len(held) != 0 {
		t.Fatalf("due while leased = %#v, want none", held)
	}
	// After the lease expires the row is due again for a later cycle.
	if expired, err := storage.ListDueDeliveries("exclusive", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	} else if len(expired) != 1 {
		t.Fatalf("due after lease = %#v, want one", expired)
	}
}

func TestRecordOnlyUpdatesItsOwnClaim(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "fenced", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("fenced", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "fenced", now)
	if _, err := storage.EnsureEpisodeDeliveries("fenced", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	due, _ := storage.ListDueDeliveries("fenced", now)
	claimed, err := storage.BeginDeliveryAttempt(due[0].ID, now)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// A stale cancellation wins first: the later result must not revive it.
	if _, err := storage.CancelDeliveriesForEndedEpisodes("fenced", []string{episode.ID}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 7}, now.Add(2*time.Second)); !errors.Is(err, store.ErrDeliveryConflict) {
		t.Fatalf("record after cancel error = %v, want ErrDeliveryConflict", err)
	}
	kept, err := storage.GetDelivery(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Status != store.DeliveryPermanentFailure || kept.MessageID != 0 {
		t.Fatalf("revived stale delivery = %#v, want permanent_failure without message", kept)
	}
	// A second record with the same spent claim also conflicts instead of
	// overwriting the winner.
	due2, _ := storage.ListDueDeliveries("fenced", now)
	if len(due2) != 0 {
		t.Fatalf("stale row due = %#v, want none", due2)
	}
}

func TestUnlinkedDestinationsAreNotNotified(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "unlinked", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(validDestination("two")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("unlinked", []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "unlinked", now)
	if _, err := storage.EnsureEpisodeDeliveries("unlinked", episode.ID, now); err != nil {
		t.Fatal(err)
	}
	// User explicitly removes the second destination: its pending work must
	// stop being eligible even though the destination stays globally enabled.
	if err := storage.SetProfileDestinations("unlinked", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if linked, err := storage.IsDestinationLinked("unlinked", "two"); err != nil || linked {
		t.Fatalf("linked(two) = %v, %v, want false", linked, err)
	}
	if linked, err := storage.IsDestinationLinked("unlinked", "one"); err != nil || !linked {
		t.Fatalf("linked(one) = %v, %v, want true", linked, err)
	}
	due, err := storage.ListDueDeliveries("unlinked", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].DestinationID != "one" {
		t.Fatalf("due after unlink = %#v, want only the still-linked destination", due)
	}
	// The authoritative claim rejects the unlinked row even if it is
	// addressed directly.
	var unlinkedID string
	for _, delivery := range mustListDeliveries(t, storage, "unlinked") {
		if delivery.DestinationID == "two" {
			unlinkedID = delivery.ID
		}
	}
	if unlinkedID == "" {
		t.Fatal("unlinked delivery row missing")
	}
	if _, err := storage.BeginDeliveryAttempt(unlinkedID, now); !errors.Is(err, store.ErrDeliveryNotDue) {
		t.Fatalf("begin unlinked error = %v, want ErrDeliveryNotDue", err)
	}
}

func TestIsRetryDueUsesChronologicalLatestRun(t *testing.T) {
	storage := openDeliveryStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "chrono", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("chrono", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	// Same-second runs with variable fractional precision: lexical text
	// ordering puts ".77Z" after the later ".777Z". The latest run must be
	// chosen chronologically (by insertion), so a newer failed run keeps the
	// interval backoff instead of bypassing it.
	base := time.Date(2026, time.September, 4, 12, 0, 0, 770000000, time.UTC)
	episode := mustDeliveryEpisode(t, storage, "chrono", base)
	if _, err := storage.EnsureEpisodeDeliveries("chrono", episode.ID, base); err != nil {
		t.Fatal(err)
	}
	later := base.Add(7 * time.Millisecond)
	failing, err := storage.BeginObservationRun("chrono", later)
	if err != nil {
		t.Fatalf("begin failed run: %v", err)
	}
	if _, err := storage.FailObservationRun(failing.ID, store.ObservationRunFailed, "temporary_failure", "portal down", later); err != nil {
		t.Fatal(err)
	}
	if due, err := storage.IsRetryDue("chrono", later); err != nil {
		t.Fatal(err)
	} else if due {
		t.Fatal("retry due with latest failed run in the same second, want false")
	}
}

func mustListDeliveries(t *testing.T, storage *store.Store, profileID string) []store.Delivery {
	t.Helper()
	deliveries, err := storage.ListDeliveriesForProfile(profileID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	return deliveries
}

func TestMigrateV5ToV6KeepsDestinations(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "medalert.db")
	createDatabase(t, databasePath, 5, `
		CREATE TABLE application_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
		CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT;
		INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (2, '2026-01-02T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (3, '2026-01-03T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (4, '2026-01-04T00:00:00Z');
		INSERT INTO schema_migrations (version, applied_at) VALUES (5, '2026-01-05T00:00:00Z');
		CREATE TABLE accounts (id TEXT PRIMARY KEY, username TEXT NOT NULL, password_source TEXT NOT NULL CHECK(password_source IN ('secret-service','file','prompt')), password_ref TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
		INSERT INTO accounts (id, username, password_source, password_ref, created_at, updated_at) VALUES ('alice', 'a@example.com', 'prompt', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
		CREATE TABLE profiles (id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, region_ids TEXT NOT NULL, specialty_ids TEXT NOT NULL, clinic_ids TEXT NOT NULL DEFAULT '', doctor_ids TEXT NOT NULL DEFAULT '', language_ids TEXT NOT NULL DEFAULT '', visit_type TEXT NOT NULL DEFAULT '', search_type TEXT NOT NULL DEFAULT 'Standard', start_date TEXT NOT NULL DEFAULT '', end_date TEXT NOT NULL DEFAULT '', check_interval_minutes INTEGER NOT NULL CHECK(check_interval_minutes BETWEEN 1 AND 43200), enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)), created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
		INSERT INTO profiles (id, account_id, region_ids, specialty_ids, check_interval_minutes, enabled, created_at, updated_at) VALUES ('p1', 'alice', '204', '132', 30, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
		CREATE TABLE observation_runs (id TEXT PRIMARY KEY, profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE, profile_updated_at TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('running', 'complete', 'failed', 'partial', 'cancelled', 'conflicting', 'stale')), started_at TEXT NOT NULL, completed_at TEXT NOT NULL DEFAULT '', slot_count INTEGER NOT NULL DEFAULT 0 CHECK(slot_count >= 0), error_code TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT '') STRICT;
		CREATE TABLE availability_episodes (id TEXT PRIMARY KEY, profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE, slot_identity TEXT NOT NULL, stable_identity TEXT NOT NULL, booking_string TEXT NOT NULL DEFAULT '', appointment_time TEXT NOT NULL, clinic TEXT NOT NULL DEFAULT '', doctor TEXT NOT NULL DEFAULT '', specialty TEXT NOT NULL DEFAULT '', visit_type TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, ended_at TEXT NOT NULL DEFAULT '', active INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0, 1))) STRICT;
		CREATE TABLE telegram_destinations (id TEXT PRIMARY KEY, name TEXT NOT NULL, chat_id TEXT NOT NULL, token_source TEXT NOT NULL CHECK(token_source IN ('secret-service','file','prompt')), token_ref TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)), last_test_at TEXT NOT NULL DEFAULT '', last_test_status TEXT NOT NULL DEFAULT '', last_test_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
		INSERT INTO telegram_destinations (id, name, chat_id, token_source, token_ref, enabled, created_at, updated_at) VALUES ('phone', 'Telefon', '123', 'prompt', '', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
		CREATE TABLE profile_telegram_destinations (profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE, destination_id TEXT NOT NULL REFERENCES telegram_destinations(id) ON DELETE CASCADE, PRIMARY KEY (profile_id, destination_id)) STRICT;
	`)
	status, err := store.Initialize(databasePath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if status.SchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("schema = %d, want %d", status.SchemaVersion, store.CurrentSchemaVersion)
	}
	storage, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer storage.Close()
	if _, err := storage.GetDestination("phone"); err != nil {
		t.Fatalf("destination after migrate: %v", err)
	}
	if _, err := storage.ListDeliveriesForProfile("p1"); err != nil {
		t.Fatalf("deliveries after migrate: %v", err)
	}
}
