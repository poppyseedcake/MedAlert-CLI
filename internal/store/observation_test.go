package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func observationSlot(identity, stable, booking, appointmentTime string) store.ObservationSlot {
	return store.ObservationSlot{
		Identity:       identity,
		StableIdentity: stable,
		BookingString:  booking,
		Time:           appointmentTime,
		Clinic:         "Main clinic",
		Doctor:         "Dr Example",
		Specialty:      "Cardiology",
		VisitType:      "Center",
	}
}

func TestObservationLifecycleKeepsEpisodesAcrossRuns(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("observed", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	slot := observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z")

	firstRun, err := storage.BeginObservationRun("observed", now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := storage.ReconcileObservationRun(firstRun.ID, []store.ObservationSlot{slot}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.NewEpisodes) != 1 || len(first.EndedEpisodes) != 0 {
		t.Fatalf("first reconciliation = %#v", first)
	}

	repeatedRun, err := storage.BeginObservationRun("observed", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := storage.ReconcileObservationRun(repeatedRun.ID, []store.ObservationSlot{slot}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.NewEpisodes) != 0 || len(repeated.EndedEpisodes) != 0 {
		t.Fatalf("repeated reconciliation = %#v", repeated)
	}

	disappearanceRun, err := storage.BeginObservationRun("observed", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	disappearance, err := storage.ReconcileObservationRun(disappearanceRun.ID, nil, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(disappearance.NewEpisodes) != 0 || len(disappearance.EndedEpisodes) != 1 || disappearance.EndedEpisodes[0].Active {
		t.Fatalf("disappearance reconciliation = %#v", disappearance)
	}

	returnRun, err := storage.BeginObservationRun("observed", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	returned, err := storage.ReconcileObservationRun(returnRun.ID, []store.ObservationSlot{slot}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(returned.NewEpisodes) != 1 || len(returned.EndedEpisodes) != 0 {
		t.Fatalf("return reconciliation = %#v", returned)
	}

	episodes, err := storage.ListAvailabilityEpisodes("observed")
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("episodes = %#v, want two episodes", episodes)
	}
	active := 0
	for _, episode := range episodes {
		if episode.Active {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active episodes = %d, want 1", active)
	}
	runs, err := storage.ListObservationRuns("observed")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 {
		t.Fatalf("runs = %#v, want four runs", runs)
	}
	for _, run := range runs {
		if run.Status != store.ObservationRunComplete {
			t.Fatalf("run = %#v, want complete", run)
		}
	}
}

func TestObservationFailureDoesNotChangeEpisodes(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("failures", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	initialRun, err := storage.BeginObservationRun("failures", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(initialRun.ID, []store.ObservationSlot{observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z")}, now); err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{store.ObservationRunFailed, store.ObservationRunPartial, store.ObservationRunCancelled, store.ObservationRunConflicting, store.ObservationRunStale} {
		run, err := storage.BeginObservationRun("failures", now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.FailObservationRun(run.ID, status, status, "search did not complete", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		episodes, err := storage.ListAvailabilityEpisodes("failures")
		if err != nil {
			t.Fatal(err)
		}
		if len(episodes) != 1 || !episodes[0].Active {
			t.Fatalf("status %s changed episodes: %#v", status, episodes)
		}
	}
}

func TestObservationIdentityRepresentationChangeKeepsEpisode(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("identity", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	fallback := observationSlot("slot-1", "slot-1", "", "2099-09-10T10:00:00Z")
	firstRun, err := storage.BeginObservationRun("identity", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(firstRun.ID, []store.ObservationSlot{fallback}, now); err != nil {
		t.Fatal(err)
	}

	primary := observationSlot("booking-1", "slot-1", "booking-1", fallback.Time)
	secondRun, err := storage.BeginObservationRun("identity", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.ReconcileObservationRun(secondRun.ID, []store.ObservationSlot{primary}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.NewEpisodes) != 0 || len(second.EndedEpisodes) != 0 {
		t.Fatalf("representation change = %#v", second)
	}
	episodes, err := storage.ListAvailabilityEpisodes("identity")
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].SlotIdentity != "booking-1" || episodes[0].BookingString != "booking-1" {
		t.Fatalf("episodes after representation change = %#v", episodes)
	}
}

func TestObservationRejectsStaleAndConflictingResults(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("stale", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	run, err := storage.BeginObservationRun("stale", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SetProfileEnabled("stale", false); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z")}, now); !errors.Is(err, store.ErrObservationRunStale) {
		t.Fatalf("stale result error = %v", err)
	}

	if _, err := storage.CreateProfile(validProfile("conflicting", "alice")); err != nil {
		t.Fatal(err)
	}
	conflictRun, err := storage.BeginObservationRun("conflicting", now)
	if err != nil {
		t.Fatal(err)
	}
	conflictingSlots := []store.ObservationSlot{
		observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z"),
		observationSlot("booking-1", "slot-2", "booking-1", "2099-09-11T10:00:00Z"),
	}
	if _, err := storage.ReconcileObservationRun(conflictRun.ID, conflictingSlots, now); !errors.Is(err, store.ErrObservationRunConflicting) {
		t.Fatalf("conflicting result error = %v", err)
	}
	if _, err := storage.FailObservationRun(conflictRun.ID, store.ObservationRunConflicting, "conflicting_result", "incompatible slot records", now); err != nil {
		t.Fatal(err)
	}
	if episodes, err := storage.ListAvailabilityEpisodes("conflicting"); err != nil {
		t.Fatal(err)
	} else if len(episodes) != 0 {
		t.Fatalf("conflicting result changed episodes: %#v", episodes)
	}
}

func TestObservationEndsPassedEpisodesAndSkipsPastSlots(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("passed", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	slot := observationSlot("booking-1", "slot-1", "booking-1", "2026-09-03T13:00:00Z")
	firstRun, err := storage.BeginObservationRun("passed", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(firstRun.ID, []store.ObservationSlot{slot}, now); err != nil {
		t.Fatal(err)
	}

	secondRun, err := storage.BeginObservationRun("passed", now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.ReconcileObservationRun(secondRun.ID, []store.ObservationSlot{slot}, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.NewEpisodes) != 0 || len(second.EndedEpisodes) != 1 {
		t.Fatalf("passed episode reconciliation = %#v", second)
	}

	if _, err := storage.CreateProfile(validProfile("already-passed", "alice")); err != nil {
		t.Fatal(err)
	}
	thirdRun, err := storage.BeginObservationRun("already-passed", now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	third, err := storage.ReconcileObservationRun(thirdRun.ID, []store.ObservationSlot{slot}, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(third.NewEpisodes) != 0 {
		t.Fatalf("past slot started an episode: %#v", third)
	}
	episodes, err := storage.ListAvailabilityEpisodes("already-passed")
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 0 {
		t.Fatalf("past slot episodes = %#v", episodes)
	}
}

func TestObservationAllowsOnlyOneActiveRunPerProfile(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("serialized", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	first, err := storage.BeginObservationRun("serialized", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.BeginObservationRun("serialized", now.Add(time.Minute))
	if !errors.Is(err, store.ErrObservationRunActive) || second.ID != "" {
		t.Fatalf("second run = %#v, error = %v", second, err)
	}
	if _, err := storage.FailObservationRun(first.ID, store.ObservationRunCancelled, "cancelled", "test cancellation", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.BeginObservationRun("serialized", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("new run after cancellation: %v", err)
	}
}

func TestDeleteProfileRemovesObservationHistory(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("history", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	run, err := storage.BeginObservationRun("history", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z")}, now); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteProfile("history"); err != nil {
		t.Fatal(err)
	}
	if runs, err := storage.ListObservationRuns("history"); err != nil {
		t.Fatal(err)
	} else if len(runs) != 0 {
		t.Fatalf("runs after profile delete = %#v", runs)
	}
	if episodes, err := storage.ListAvailabilityEpisodes("history"); err != nil {
		t.Fatal(err)
	} else if len(episodes) != 0 {
		t.Fatalf("episodes after profile delete = %#v", episodes)
	}
}
