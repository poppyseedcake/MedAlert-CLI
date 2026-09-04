// Package monitoring schedule helpers own continuous-watch timing rules.
//
// The helpers are pure time calculations so the watch adapter can decide
// which enabled profiles are due without touching SQLite or the network.
// Durable lease enforcement stays in Store.BeginObservationRun; these
// helpers only prevent busy loops inside one process.
package monitoring

import (
	"time"

	"github.com/poppyseedcake/MedAlert/internal/store"
)

// IsProfileDue reports whether an enabled profile should be checked now.
// A profile with no previous run is due immediately. Disabled profiles are
// never due. A non-positive interval is treated as due so a misconfigured
// row cannot silently stall monitoring (profile validation normally
// prevents this).
func IsProfileDue(profile store.Profile, lastRun time.Time, now time.Time) bool {
	if !profile.Enabled {
		return false
	}
	if lastRun.IsZero() {
		return true
	}
	interval := time.Duration(profile.CheckIntervalMinutes) * time.Minute
	if interval <= 0 {
		return true
	}
	return !now.Before(lastRun.Add(interval))
}

// NextRunAfter returns the next planned start after a run that began at last.
// Callers use it to throttle lease conflicts and failures the same way as
// successful runs so two processes sharing one database do not busy-loop.
func NextRunAfter(profile store.Profile, last time.Time) time.Time {
	interval := time.Duration(profile.CheckIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = time.Minute
	}
	return last.Add(interval)
}

// DueProfiles filters enabled profiles to those due at now. lastRuns maps a
// profile id to its last known start time; missing entries mean never run.
func DueProfiles(profiles []store.Profile, lastRuns map[string]time.Time, now time.Time) []store.Profile {
	due := []store.Profile{}
	for _, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		if IsProfileDue(profile, lastRuns[profile.ID], now) {
			due = append(due, profile)
		}
	}
	return due
}
