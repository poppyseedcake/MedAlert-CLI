package monitoring

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

type deliverFixture struct {
	storage       *store.Store
	profile       store.Profile
	reconciliation store.ObservationReconciliation
	server        *httptest.Server
	hits          *int64
	sender        *telegram.Client
}

func setupDeliverFixture(t *testing.T, handler http.Handler) *deliverFixture {
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
	if _, err := storage.CreateAccount(store.Account{ID: "alice", Username: "alice@example.com", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	profile, err := storage.CreateProfile(store.Profile{ID: "morning", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, Enabled: true, SearchType: store.SearchTypeStandard})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := storage.CreateDestination(store.Destination{ID: "phone", Name: "Telefon", ChatID: "123456", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := storage.SetProfileDestinations("morning", []string{"phone"}); err != nil {
		t.Fatalf("link: %v", err)
	}
	now := time.Now().UTC()
	run, err := storage.BeginObservationRun("morning", now)
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	reconciliation, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{{
		Identity: "booking-1", StableIdentity: "slot-1", BookingString: "booking-1",
		Time: "2099-09-10T10:00:00Z", Clinic: "Main clinic", Doctor: "Dr Example",
		Specialty: "Cardiology", VisitType: "Center",
	}}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(reconciliation.NewEpisodes) != 1 {
		t.Fatalf("new episodes = %#v, want one", reconciliation.NewEpisodes)
	}
	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	sender := telegram.NewClient(telegram.Config{BaseURL: server.URL, HTTPClient: server.Client()})
	return &deliverFixture{storage: storage, profile: profile, reconciliation: reconciliation, server: server, hits: &hits, sender: sender}
}

func telegramSuccess() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	})
}

func okResolver(secret string) func(store.Destination) (telegram.Secret, error) {
	return func(store.Destination) (telegram.Secret, error) {
		return telegram.Secret(secret), nil
	}
}

func deliveryStatus(t *testing.T, storage *store.Store) (string, int) {
	t.Helper()
	deliveries, err := storage.ListDeliveriesForProfile("morning")
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, want one", deliveries)
	}
	return deliveries[0].Status, deliveries[0].Attempts
}

func TestProcessTransientSecretErrorRetriesThenDelivers(t *testing.T) {
	fixture := setupDeliverFixture(t, telegramSuccess())
	transient := errors.New(`read telegram secret for "phone": secret service is unavailable`)
	calls := 0
	resolve := func(store.Destination) (telegram.Secret, error) {
		calls++
		if calls == 1 {
			return "", transient
		}
		return telegram.Secret("token"), nil
	}
	ctx := context.Background()
	summary, err := ProcessAvailabilityDeliveries(ctx, fixture.storage, fixture.profile, fixture.reconciliation, fixture.sender, resolve, time.Now().UTC())
	if err != nil {
		t.Fatalf("first process: %v", err)
	}
	if summary.Attempted != 1 || summary.StillRetry != 1 {
		t.Fatalf("first summary = %#v, want one attempted retrying", summary)
	}
	if status, attempts := deliveryStatus(t, fixture.storage); status != store.DeliveryRetry || attempts != 1 {
		t.Fatalf("after transient: status=%q attempts=%d, want retry/1", status, attempts)
	}
	if got := atomic.LoadInt64(fixture.hits); got != 0 {
		t.Fatalf("telegram hits after token failure = %d, want 0", got)
	}
	summary, err = ProcessAvailabilityDeliveries(ctx, fixture.storage, fixture.profile, fixture.reconciliation, fixture.sender, resolve, time.Now().UTC())
	if err != nil {
		t.Fatalf("second process: %v", err)
	}
	if summary.Delivered != 1 {
		t.Fatalf("second summary = %#v, want one delivered", summary)
	}
	if status, attempts := deliveryStatus(t, fixture.storage); status != store.DeliveryDelivered || attempts != 2 {
		t.Fatalf("after recovery: status=%q attempts=%d, want delivered/2", status, attempts)
	}
}

func TestProcessPermanentSecretErrorFailsTerminally(t *testing.T) {
	fixture := setupDeliverFixture(t, telegramSuccess())
	resolve := func(store.Destination) (telegram.Secret, error) {
		return "", fmt.Errorf("token: %w", secrets.ErrSecretNotFound)
	}
	ctx := context.Background()
	summary, err := ProcessAvailabilityDeliveries(ctx, fixture.storage, fixture.profile, fixture.reconciliation, fixture.sender, resolve, time.Now().UTC())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if summary.Attempted != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %#v, want one attempted failed", summary)
	}
	if status, _ := deliveryStatus(t, fixture.storage); status != store.DeliveryPermanentFailure {
		t.Fatalf("status = %q, want permanent_failure", status)
	}
	// Terminal rows are never retried.
	summary, err = ProcessAvailabilityDeliveries(ctx, fixture.storage, fixture.profile, fixture.reconciliation, fixture.sender, okResolver("token"), time.Now().UTC())
	if err != nil {
		t.Fatalf("second process: %v", err)
	}
	if summary.Attempted != 0 {
		t.Fatalf("second summary = %#v, want no attempts", summary)
	}
	if got := atomic.LoadInt64(fixture.hits); got != 0 {
		t.Fatalf("telegram hits = %d, want 0", got)
	}
}

func TestProcessAnchorsBackoffToResponseNotBatchStart(t *testing.T) {
	slowRateLimited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":120}}`))
	})
	fixture := setupDeliverFixture(t, slowRateLimited)
	before := time.Now().UTC()
	// A batch timestamp an hour stale must not leak into the claim lease or
	// the response-relative backoff.
	staleBatch := before.Add(-time.Hour)
	summary, err := ProcessAvailabilityDeliveries(context.Background(), fixture.storage, fixture.profile, fixture.reconciliation, fixture.sender, okResolver("token"), staleBatch)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if summary.StillRetry != 1 {
		t.Fatalf("summary = %#v, want one retrying", summary)
	}
	deliveries, err := fixture.storage.ListDeliveriesForProfile("morning")
	if err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, deliveries[0].NextAttemptAt)
	if err != nil {
		t.Fatalf("next_attempt_at = %q: %v", deliveries[0].NextAttemptAt, err)
	}
	// Old code stored staleBatch+120s, already ~58 minutes in the past.
	if !next.After(before.Add(115 * time.Second)) || !next.Before(time.Now().UTC().Add(180*time.Second)) {
		t.Fatalf("next_attempt_at = %v, want response time + ~120s", next)
	}
}

func TestProcessSurfacesStoreFailures(t *testing.T) {
	fixture := setupDeliverFixture(t, telegramSuccess())
	if err := fixture.storage.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := ProcessAvailabilityDeliveries(context.Background(), fixture.storage, fixture.profile, fixture.reconciliation, fixture.sender, okResolver("token"), time.Now().UTC())
	if err == nil {
		t.Fatal("process on closed store succeeded, want store error")
	}
}

func TestProcessSkipsOnlyMissingEpisode(t *testing.T) {
	// Store-level contract the skip branch relies on: genuinely missing rows
	// report NotFound while other failures keep their error.
	storage := setupDeliverFixture(t, telegramSuccess()).storage
	if _, err := storage.GetEpisode("episode-missing"); !errors.Is(err, store.ErrDeliveryNotFound) {
		t.Fatalf("missing episode error = %v, want ErrDeliveryNotFound", err)
	}
	if _, err := storage.GetDestination("destination-missing"); !errors.Is(err, store.ErrDestinationNotFound) {
		t.Fatalf("missing destination error = %v, want ErrDestinationNotFound", err)
	}
}
