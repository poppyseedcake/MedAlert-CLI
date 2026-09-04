package store_test

import (
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func TestHistoryRetentionDefaultsAndValidation(t *testing.T) {
	storage, _ := openProfileStore(t)
	days, err := storage.GetHistoryRetentionDays()
	if err != nil {
		t.Fatalf("default retention: %v", err)
	}
	if days != store.DefaultHistoryRetentionDays {
		t.Fatalf("default = %d, want %d", days, store.DefaultHistoryRetentionDays)
	}
	for _, invalid := range []int{0, -1, store.MaxHistoryRetentionDays + 1} {
		if _, err := storage.SetHistoryRetentionDays(invalid); err == nil {
			t.Fatalf("set %d succeeded, want error", invalid)
		}
	}
	saved, err := storage.SetHistoryRetentionDays(30)
	if err != nil || saved != 30 {
		t.Fatalf("set 30 = %d, %v", saved, err)
	}
	days, err = storage.GetHistoryRetentionDays()
	if err != nil || days != 30 {
		t.Fatalf("get after set = %d, %v", days, err)
	}
}

func TestPruneHistoryKeepsActiveAndRemovesOldTerminal(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("prune", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -100)
	slot := observationSlot("booking-old", "slot-old", "booking-old", "2099-09-10T10:00:00Z")

	oldRun, err := storage.BeginObservationRun("prune", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(oldRun.ID, []store.ObservationSlot{slot}, old); err != nil {
		t.Fatal(err)
	}
	// Old completed run is terminal and older than the default 90-day cutoff.
	newRun, err := storage.BeginObservationRun("prune", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReconcileObservationRun(newRun.ID, []store.ObservationSlot{slot}, now); err != nil {
		t.Fatal(err)
	}
	// Old failed run.
	failedRun, err := storage.BeginObservationRun("prune", old.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Finish the old failed run before starting assertions: Begin leaves it
	// running, and running rows are never pruned.
	if _, err := storage.FailObservationRun(failedRun.ID, store.ObservationRunFailed, "temporary_failure", "old failure", old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := storage.PruneHistory(now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.ObservationRuns == 0 {
		t.Fatal("prune removed no runs, want old terminal runs removed")
	}
	runs, err := storage.ListObservationRuns("prune")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ID == oldRun.ID {
			t.Fatalf("old run was not pruned: %#v", run)
		}
	}
	// Active episodes are kept indefinitely even when old.
	episodes, err := storage.ListAvailabilityEpisodes("prune")
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) == 0 {
		t.Fatal("active episode was pruned, want it kept")
	}
}

func TestPruneHistoryKeepsPendingDeliveries(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("deliver", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(store.Destination{ID: "dest", Name: "Test", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("deliver", []string{"dest"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	run, err := storage.BeginObservationRun("deliver", now)
	if err != nil {
		t.Fatal(err)
	}
	reconciliation, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{observationSlot("booking-1", "slot-1", "booking-1", "2099-09-10T10:00:00Z")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.NewEpisodes) != 1 {
		t.Fatalf("new episodes = %#v", reconciliation)
	}
	created, err := storage.EnsureEpisodeDeliveries("deliver", reconciliation.NewEpisodes[0].ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("deliveries = %#v", created)
	}
	// A pending delivery is current work and must survive even with a 1-day
	// policy and an old creation time: backdate is not possible through the
	// API, so pruning now must keep it.
	if _, err := storage.SetHistoryRetentionDays(1); err != nil {
		t.Fatal(err)
	}
	result, err := storage.PruneHistory(now)
	if err != nil {
		t.Fatal(err)
	}
	if result.TelegramDeliveries != 0 {
		t.Fatalf("pending delivery was pruned: %#v", result)
	}
	deliveries, err := storage.ListRecentDeliveries("deliver", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries after prune = %#v", deliveries)
	}
}

func TestListRecentQueriesRespectLimits(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("recent", "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		run, err := storage.BeginObservationRun("recent", now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.FailObservationRun(run.ID, store.ObservationRunFailed, "temporary_failure", "failure", now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := storage.ListRecentObservationRuns(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("recent runs = %d, want 2", len(runs))
	}
	episodes, err := storage.ListRecentEpisodes("", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if episodes == nil {
		t.Fatal("episodes is nil, want empty slice")
	}
	deliveries, err := storage.ListRecentDeliveries("", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries == nil {
		t.Fatal("deliveries is nil, want empty slice")
	}
}

func TestListRecentDeliveriesReturnsNewestRows(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("recent-deliveries", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(store.Destination{ID: "recent-dest", Name: "Recent", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("recent-deliveries", []string{"recent-dest"}); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	for index, at := range []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute)} {
		run, err := storage.BeginObservationRun("recent-deliveries", at)
		if err != nil {
			t.Fatal(err)
		}
		reconciliation, err := storage.ReconcileObservationRun(run.ID, []store.ObservationSlot{
			observationSlot("recent-slot-"+string(rune('a'+index)), "recent-stable-"+string(rune('a'+index)), "recent-booking", "2099-01-10T10:00:00Z"),
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(reconciliation.NewEpisodes) != 1 {
			t.Fatalf("new episodes = %#v, want one", reconciliation)
		}
		if _, err := storage.EnsureEpisodeDeliveries("recent-deliveries", reconciliation.NewEpisodes[0].ID, at); err != nil {
			t.Fatal(err)
		}
	}

	deliveries, err := storage.ListRecentDeliveries("", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %#v, want two", deliveries)
	}
	if deliveries[0].CreatedAt != base.Add(2*time.Minute).Format(time.RFC3339Nano) || deliveries[1].CreatedAt != base.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("deliveries = %#v, want newest first", deliveries)
	}
}

func TestListRecentIncidentDeliveriesReturnsNewestRows(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateDestination(store.Destination{ID: "incident-recent-dest", Name: "Recent", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	for index, at := range []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute)} {
		if _, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, "incident-recent-"+string(rune('a'+index)), "", "", "", "authentication_required", "login required", at, []string{"incident-recent-dest"}); err != nil {
			t.Fatal(err)
		} else if !newly || len(created) != 1 {
			t.Fatalf("created = %#v, newly = %v, want one new delivery", created, newly)
		}
	}

	deliveries, err := storage.ListRecentIncidentDeliveries("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, want one", deliveries)
	}
	if deliveries[0].CreatedAt != base.Add(2*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("delivery = %#v, want newest first", deliveries[0])
	}
}

func TestPruneHistoryKeepsPendingIncidentChildren(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateDestination(store.Destination{ID: "prune-incident-dest", Name: "Prune", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -100)
	incident, created, newly, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, "prune-account", "", "", "", "authentication_required", "login required", old, []string{"prune-incident-dest"})
	if err != nil || !newly || len(created) != 1 {
		t.Fatalf("record = %#v, %v, newly=%v, created=%d", incident, err, newly, len(created))
	}
	claimed, err := storage.BeginIncidentDeliveryAttempt(created[0].ID, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: 1}, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, recoveries, cancelled, err := storage.ResolveIncident(store.IncidentScopeAccount, "prune-account", "", "", old.Add(2*time.Minute))
	if err != nil || len(recoveries) != 1 || len(cancelled) != 0 {
		t.Fatalf("resolve recoveries=%d cancelled=%d err=%v, want one pending recovery", len(recoveries), len(cancelled), err)
	}
	if _, err := storage.SetHistoryRetentionDays(1); err != nil {
		t.Fatal(err)
	}

	result, err := storage.PruneHistory(now)
	if err != nil {
		t.Fatal(err)
	}
	if result.OperationalIncidents != 0 {
		t.Fatalf("prune removed incident with pending recovery: %#v", result)
	}
	deliveries, err := storage.ListIncidentDeliveries(incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Kind != store.IncidentDeliveryRecovery || deliveries[0].Status != store.DeliveryPending {
		t.Fatalf("incident deliveries after prune = %#v, want pending recovery", deliveries)
	}
}

func TestPruneHistoryKeepsPendingEpisodeChildren(t *testing.T) {
	storage, _ := openProfileStore(t)
	if _, err := storage.CreateProfile(validProfile("prune-episode", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateDestination(store.Destination{ID: "prune-episode-dest", Name: "Prune", ChatID: "123", TokenSource: store.TokenSourcePrompt, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetProfileDestinations("prune-episode", []string{"prune-episode-dest"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -100)
	firstRun, err := storage.BeginObservationRun("prune-episode", old)
	if err != nil {
		t.Fatal(err)
	}
	first, err := storage.ReconcileObservationRun(firstRun.ID, []store.ObservationSlot{observationSlot("prune-slot", "prune-stable", "prune-booking", "2099-01-10T10:00:00Z")}, old)
	if err != nil || len(first.NewEpisodes) != 1 {
		t.Fatalf("first reconciliation = %#v, err=%v", first, err)
	}
	if _, err := storage.EnsureEpisodeDeliveries("prune-episode", first.NewEpisodes[0].ID, old); err != nil {
		t.Fatal(err)
	}
	endRun, err := storage.BeginObservationRun("prune-episode", old.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ended, err := storage.ReconcileObservationRun(endRun.ID, nil, old.Add(time.Minute))
	if err != nil || len(ended.EndedEpisodes) != 1 {
		t.Fatalf("ending reconciliation = %#v, err=%v", ended, err)
	}
	if _, err := storage.SetHistoryRetentionDays(1); err != nil {
		t.Fatal(err)
	}

	result, err := storage.PruneHistory(now)
	if err != nil {
		t.Fatal(err)
	}
	if result.AvailabilityEpisodes != 0 {
		t.Fatalf("prune removed episode with pending delivery: %#v", result)
	}
	episodes, err := storage.ListAvailabilityEpisodes("prune-episode")
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := storage.ListRecentDeliveries("prune-episode", store.DeliveryPending, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].Active || len(deliveries) != 1 {
		t.Fatalf("after prune episodes=%#v deliveries=%#v, want ended episode and pending delivery", episodes, deliveries)
	}
}
