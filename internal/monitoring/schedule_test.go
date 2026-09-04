package monitoring

import (
	"testing"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

func scheduleProfile(id string, interval int, enabled bool) store.Profile {
	return store.Profile{ID: id, AccountID: "alice", RegionIDs: "204", SpecialtyIDs: "132", CheckIntervalMinutes: interval, Enabled: enabled, SearchType: store.SearchTypeStandard}
}

func TestIsProfileDueCoversNewOverdueAndWaiting(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	profile := scheduleProfile("morning", 30, true)

	if !IsProfileDue(profile, time.Time{}, now) {
		t.Fatal("never-run profile must be due immediately")
	}
	if !IsProfileDue(profile, now.Add(-31*time.Minute), now) {
		t.Fatal("overdue profile must be due")
	}
	if !IsProfileDue(profile, now.Add(-30*time.Minute), now) {
		t.Fatal("profile exactly at its interval must be due")
	}
	if IsProfileDue(profile, now.Add(-29*time.Minute), now) {
		t.Fatal("recently checked profile must wait for its interval")
	}
	if IsProfileDue(scheduleProfile("paused", 30, false), time.Time{}, now) {
		t.Fatal("disabled profile must never be due")
	}
}

func TestNextRunAfterThrottlesByInterval(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	next := NextRunAfter(scheduleProfile("morning", 30, true), now)
	if !next.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("next run = %v, want +30m", next)
	}
}

func TestDueProfilesKeepsOnlyEnabledDue(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	profiles := []store.Profile{
		scheduleProfile("due", 30, true),
		scheduleProfile("waiting", 30, true),
		scheduleProfile("disabled", 1, false),
	}
	lastRuns := map[string]time.Time{"waiting": now.Add(-time.Minute)}
	due := DueProfiles(profiles, lastRuns, now)
	if len(due) != 1 || due[0].ID != "due" {
		t.Fatalf("due profiles = %#v, want only due", due)
	}
}
