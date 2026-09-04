package store_test

import (
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func openIncidentStore(t *testing.T) *store.Store {
	t.Helper()
	storage := openDeliveryStore(t)
	return storage
}

func TestIncidentAuthStartsImmediately(t *testing.T) {
	storage := openIncidentStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "p1", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("p1", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	incident, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, "alice", "alice", "", "", "authentication_required", "auth needed", now, []string{"one"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !newly || len(created) != 1 || incident.ConsecutiveFailures != 1 {
		t.Fatalf("incident = %#v created=%d newly=%v, want immediate with one delivery", incident, len(created), newly)
	}
	// Repeated failures update without new notifications.
	second, again, newlyAgain, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, "alice", "alice", "", "", "authentication_required", "still needed", now.Add(time.Minute), []string{"one"})
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if newlyAgain || len(again) != 0 || second.ConsecutiveFailures != 2 {
		t.Fatalf("second = %#v again=%d newly=%v, want update without noise", second, len(again), newlyAgain)
	}
	// Resolve creates one recovery for the delivered failure.
	deliveries, err := storage.ListIncidentDeliveries(incident.ID)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, %v, want one failure", deliveries, err)
	}
	claimed, err := storage.BeginIncidentDeliveryAttempt(deliveries[0].ID, now)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 1}, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	resolved, recoveries, cancelled, err := storage.ResolveIncident(store.IncidentScopeAccount, "alice", "", "", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != store.IncidentStatusResolved || len(recoveries) != 1 || len(cancelled) != 0 {
		t.Fatalf("resolved = %#v recoveries=%d cancelled=%d, want one recovery", resolved, len(recoveries), len(cancelled))
	}
}

func TestIncidentTemporaryNeedsThreeConsecutive(t *testing.T) {
	storage := openIncidentStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "temp", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("temp", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	for attempt := 1; attempt <= 2; attempt++ {
		incident, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, "temp", "alice", "temp", "", "temporary_failure", "portal down", now.Add(time.Duration(attempt)*time.Minute), []string{"one"})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if newly || len(created) != 0 || incident.ConsecutiveFailures != attempt {
			t.Fatalf("attempt %d incident = %#v created=%d newly=%v, want no notification yet", attempt, incident, len(created), newly)
		}
	}
	third, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, "temp", "alice", "temp", "", "temporary_failure", "portal down", now.Add(3*time.Minute), []string{"one"})
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if !newly || len(created) != 1 || third.ConsecutiveFailures != 3 {
		t.Fatalf("third = %#v created=%d newly=%v, want one notification", third, len(created), newly)
	}
}

func TestIncidentPendingFailureCancelledOnResolve(t *testing.T) {
	storage := openIncidentStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "cancel", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("cancel", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	incident, _, newly, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, "cancel", "alice", "cancel", "", "protocol_changed", "changed", now, []string{"one"})
	if err != nil || !newly {
		t.Fatalf("record = %#v, %v newly=%v", incident, err, newly)
	}
	// Resolve before the failure delivers: pending failure cancelled, no recovery.
	resolved, recoveries, cancelled, err := storage.ResolveIncident(store.IncidentScopeProfile, "cancel", "cancel", "", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != store.IncidentStatusResolved || len(recoveries) != 0 || len(cancelled) != 1 {
		t.Fatalf("resolved = %#v recoveries=%d cancelled=%d, want one cancelled no recovery", resolved, len(recoveries), len(cancelled))
	}
	deliveries, err := storage.ListIncidentDeliveries(incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != store.DeliveryPermanentFailure {
		t.Fatalf("deliveries after cancel = %#v, want one permanent_failure", deliveries)
	}
}

func TestIncidentResolveSkipsInFlightClaimAndRecoversLateDelivery(t *testing.T) {
	storage := openIncidentStore(t)
	mustCreateAccount(t, storage, "alice", "alice@example.com")
	mustDeliveryProfile(t, storage, "race", "alice")
	if _, err := storage.CreateDestination(validDestination("one")); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("race", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	incident, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, "race", "alice", "race", "", "protocol_changed", "changed", now, []string{"one"})
	if err != nil || !newly || len(created) != 1 {
		t.Fatalf("record = %#v, %v newly=%v created=%d", incident, err, newly, len(created))
	}
	// Worker A claims the failure delivery; its Telegram request is in flight.
	claimed, err := storage.BeginIncidentDeliveryAttempt(created[0].ID, now)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Worker B resolves the incident while the send is in flight. The
	// claimed row must survive so the late result is not lost.
	resolved, recoveries, cancelled, err := storage.ResolveIncident(store.IncidentScopeProfile, "race", "race", "", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != store.IncidentStatusResolved {
		t.Fatalf("resolved status = %q, want resolved", resolved.Status)
	}
	if len(recoveries) != 0 || len(cancelled) != 0 {
		t.Fatalf("recoveries=%d cancelled=%d, want none yet (claim in flight)", len(recoveries), len(cancelled))
	}
	// The in-flight send succeeded after resolution. It must record and
	// queue its recovery pair instead of conflicting away silently.
	delivered, err := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 7}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("record late delivery: %v", err)
	}
	if delivered.Status != store.DeliveryDelivered {
		t.Fatalf("late delivery status = %q, want delivered", delivered.Status)
	}
	deliveries, err := storage.ListIncidentDeliveries(incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	var failure, recovery *store.IncidentDelivery
	for index := range deliveries {
		switch deliveries[index].Kind {
		case store.IncidentDeliveryFailure:
			failure = &deliveries[index]
		case store.IncidentDeliveryRecovery:
			recovery = &deliveries[index]
		}
	}
	if failure == nil || failure.Status != store.DeliveryDelivered {
		t.Fatalf("failure = %#v, want delivered", failure)
	}
	if recovery == nil || recovery.Status != store.DeliveryPending {
		t.Fatalf("recovery = %#v, want pending pair for the late failure", recovery)
	}
}
