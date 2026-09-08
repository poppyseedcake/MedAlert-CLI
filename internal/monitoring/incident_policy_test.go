package monitoring_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/monitoring"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

func policyFixture(t *testing.T) (*store.Store, store.Profile, time.Time) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "medalert.db")
	if _, err := store.Initialize(path); err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storage.Close() })
	if _, err := storage.CreateAccount(store.Account{ID: "alice", Username: "alice@example.com", PasswordSource: store.PasswordSourcePrompt}); err != nil {
		t.Fatal(err)
	}
	profile, err := storage.CreateProfile(store.Profile{ID: "morning", AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: 30, Enabled: true, SearchType: store.SearchTypeStandard})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"phone", "backup"} {
		if _, err := storage.CreateDestination(store.Destination{ID: id, Name: id, ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.SetProfileDestinations(profile.ID, []string{"phone", "backup"}); err != nil {
		t.Fatal(err)
	}
	return storage, profile, time.Now().UTC()
}

func TestDestinationIncidentKeepsFailureWhenSamePassAlsoSucceeds(t *testing.T) {
	storage, profile, now := policyFixture(t)
	summary := monitoring.DeliverySummary{Deliveries: []store.Delivery{
		{DestinationID: "phone", Status: store.DeliveryDelivered},
		{DestinationID: "phone", Status: store.DeliveryPermanentFailure, LastError: "chat not found"},
	}}
	changes, err := monitoring.ReconcileDestinationIncidents(storage, profile, summary, now)
	if err != nil || len(changes) != 1 || !changes[0].Started {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
	incident, err := storage.GetActiveIncident(store.IncidentScopeDestination, "phone", profile.ID, "phone")
	if err != nil || incident.ID != changes[0].Incident.ID {
		t.Fatalf("incident=%+v err=%v", incident, err)
	}
	changes, err = monitoring.ReconcileDestinationIncidents(storage, profile, monitoring.DeliverySummary{Deliveries: []store.Delivery{{DestinationID: "phone", Status: store.DeliveryDelivered}}}, now.Add(time.Minute))
	if err != nil || len(changes) != 1 || changes[0].Incident.Status != store.IncidentStatusResolved {
		t.Fatalf("recovery=%+v err=%v", changes, err)
	}
}

func TestCheckIncidentRoutesAuthenticationToAccountAndRecovers(t *testing.T) {
	storage, profile, now := policyFixture(t)
	change, err := monitoring.HandleCheckFailure(storage, profile, &medicover.Error{Code: medicover.CodeAuthRequired, Message: "login required"}, now)
	if err != nil || change.Incident.ScopeType != store.IncidentScopeAccount || !change.Started {
		t.Fatalf("auth=%+v err=%v", change, err)
	}
	if _, err := monitoring.HandleCheckFailure(storage, profile, &medicover.Error{Code: medicover.CodeProtocolChanged, Message: "protocol changed"}, now); err != nil {
		t.Fatal(err)
	}
	changes, err := monitoring.ResolveCheckIncidents(storage, profile, now.Add(time.Minute))
	if err != nil || len(changes) != 2 {
		t.Fatalf("resolved=%+v err=%v", changes, err)
	}
	for _, change := range changes {
		if change.Incident.Status != store.IncidentStatusResolved {
			t.Fatalf("not resolved: %+v", change)
		}
	}
}

func TestIncidentPolicyIgnoresCancellationAndLocalConflicts(t *testing.T) {
	for _, failure := range []error{
		nil, context.Canceled, context.DeadlineExceeded, store.ErrObservationRunActive,
		store.ErrObservationRunStale, store.ErrObservationRunConflicting, store.ErrProfileDisabled,
		store.ErrObservationRunInvalid, store.ErrObservationRunNotFound, store.ErrProfileNotFound,
		&medicover.Error{Code: medicover.CodeCancelled}, &medicover.Error{Code: medicover.CodeTimeout},
		&medicover.Error{Code: medicover.CodeStale}, &medicover.Error{Code: medicover.CodeConflicting},
	} {
		storage, profile, now := policyFixture(t)
		change, err := monitoring.HandleCheckFailure(storage, profile, failure, now)
		if err != nil || change.Incident.ID != "" {
			t.Fatalf("failure=%v change=%+v err=%v", failure, change, err)
		}
		incidents, err := storage.ListIncidents("", "", 100)
		if err != nil || len(incidents) != 0 {
			t.Fatalf("failure=%v incidents=%+v err=%v", failure, incidents, err)
		}
	}
}

func TestAuthenticationIncidentIgnoresMissingInputAndCancellation(t *testing.T) {
	storage, profile, now := policyFixture(t)
	for _, failure := range []error{nil, secrets.ErrMissingInput, context.Canceled, context.DeadlineExceeded, errors.New("secret unavailable"), &medicover.Error{Code: medicover.CodeTimeout}} {
		change, err := monitoring.HandleAuthenticationFailure(storage, profile.AccountID, failure, now)
		if err != nil || change.Incident.ID != "" {
			t.Fatalf("failure=%v change=%+v err=%v", failure, change, err)
		}
	}
	incidents, err := storage.ListIncidents("", "", 100)
	if err != nil || len(incidents) != 0 {
		t.Fatalf("incidents=%+v err=%v", incidents, err)
	}
}

func TestDestinationIncidentIgnoresCancelledAndExhaustedDeliveries(t *testing.T) {
	storage, profile, now := policyFixture(t)
	for _, message := range []string{"slot is no longer available", "incident ended before failure was delivered", "gave up after 5 attempts"} {
		changes, err := monitoring.ReconcileDestinationIncidents(storage, profile, monitoring.DeliverySummary{Deliveries: []store.Delivery{{DestinationID: "phone", Status: store.DeliveryPermanentFailure, LastError: message}}}, now)
		if err != nil || len(changes) != 0 {
			t.Fatalf("message=%s changes=%+v err=%v", message, changes, err)
		}
	}
	incidents, err := storage.ListIncidents("", "", 100)
	if err != nil || len(incidents) != 0 {
		t.Fatalf("incidents=%+v err=%v", incidents, err)
	}
}

func TestTemporaryCheckIncidentNotifiesOnceAfterThreeFailures(t *testing.T) {
	storage, profile, now := policyFixture(t)
	for attempt := 1; attempt <= 4; attempt++ {
		change, err := monitoring.HandleCheckFailure(storage, profile, &medicover.Error{Code: medicover.CodeTemporary, Message: "portal unavailable"}, now.Add(time.Duration(attempt)*time.Minute))
		if err != nil || change.Incident.ConsecutiveFailures != attempt || change.Started != (attempt == 3) {
			t.Fatalf("attempt=%d change=%+v err=%v", attempt, change, err)
		}
		deliveries, err := storage.ListIncidentDeliveries(change.Incident.ID)
		want := 0
		if attempt >= 3 {
			want = 2
		}
		if err != nil || len(deliveries) != want {
			t.Fatalf("attempt=%d deliveries=%+v err=%v", attempt, deliveries, err)
		}
	}
}

func TestCheckRecoveryReportsQueuedNotifications(t *testing.T) {
	storage, profile, now := policyFixture(t)
	change, err := monitoring.HandleCheckFailure(storage, profile, &medicover.Error{Code: medicover.CodeProtocolChanged, Message: "changed"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := storage.ListIncidentDeliveries(change.Incident.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	claim, err := storage.BeginIncidentDeliveryAttempt(pending[0].ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.RecordIncidentDeliveryResult(claim.ID, claim, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 1}, now); err != nil {
		t.Fatal(err)
	}
	changes, err := monitoring.ResolveCheckIncidents(storage, profile, now.Add(time.Minute))
	if err != nil || len(changes) != 1 || !changes[0].RecoveryQueued {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
	deliveries, err := storage.ListIncidentDeliveries(change.Incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveries := 0
	for _, delivery := range deliveries {
		if delivery.Kind == store.IncidentDeliveryRecovery {
			recoveries++
			if delivery.DestinationID != claim.DestinationID {
				t.Fatalf("recovery used wrong destination: %+v", delivery)
			}
		}
	}
	if recoveries != 1 {
		t.Fatalf("deliveries=%+v, want one recovery", deliveries)
	}
	changes, err = monitoring.ResolveCheckIncidents(storage, profile, now.Add(2*time.Minute))
	if err != nil || len(changes) != 0 {
		t.Fatalf("repeat changes=%+v err=%v", changes, err)
	}
}

func TestIncidentPolicyReturnsStorageFailures(t *testing.T) {
	storage, profile, now := policyFixture(t)
	storage.Close()
	failure := &medicover.Error{Code: medicover.CodeProtocolChanged, Message: "changed"}
	if _, err := monitoring.HandleCheckFailure(storage, profile, failure, now); err == nil {
		t.Fatal("check failure was hidden")
	}
	if _, err := monitoring.HandleAuthenticationFailure(storage, profile.AccountID, failure, now); err == nil {
		t.Fatal("authentication failure was hidden")
	}
	if _, err := monitoring.ResolveCheckIncidents(storage, profile, now); err == nil {
		t.Fatal("resolution failure was hidden")
	}
	if _, err := monitoring.ReconcileDestinationIncidents(storage, profile, monitoring.DeliverySummary{Deliveries: []store.Delivery{{DestinationID: "phone", Status: store.DeliveryPermanentFailure, LastError: "chat not found"}}}, now); err == nil {
		t.Fatal("destination failure was hidden")
	}
}
