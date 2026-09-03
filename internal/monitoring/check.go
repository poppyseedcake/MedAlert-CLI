// Package monitoring owns observation runs and availability episode changes.
package monitoring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

// CheckResult contains the search result and the durable changes made by a
// complete check.
type CheckResult struct {
	Search         medicover.SearchResult
	Reconciliation store.ObservationReconciliation
}

// Check runs one observation search and reconciles its result. A search error
// records the run outcome but never changes availability episodes.
func Check(ctx context.Context, storage *store.Store, profile store.Profile, client *medicover.Client, accessToken string, now time.Time) (CheckResult, error) {
	run, err := storage.BeginObservationRun(profile.ID, now)
	if err != nil {
		return CheckResult{}, err
	}
	// The caller may have loaded the profile before authentication or another
	// process may have edited it while the run was being reserved. Use the
	// exact version captured by the run, and let reconciliation reject any
	// later edit.
	currentProfile, err := storage.GetProfile(profile.ID)
	if err != nil {
		return CheckResult{}, recordFailedRun(storage, run, err, now)
	}
	if currentProfile.UpdatedAt != run.ProfileUpdatedAt || !currentProfile.Enabled {
		return CheckResult{}, recordFailedRun(storage, run, fmt.Errorf("%w: profile changed before the search started", store.ErrObservationRunStale), now)
	}
	profile = currentProfile
	search, err := client.Search(ctx, accessToken, medicover.SearchCriteria{
		RegionIDs:    profile.RegionIDs,
		SpecialtyIDs: profile.SpecialtyIDs,
		ClinicIDs:    profile.ClinicIDs,
		DoctorIDs:    profile.DoctorIDs,
		LanguageIDs:  profile.LanguageIDs,
		VisitType:    profile.VisitType,
		SearchType:   profile.SearchType,
		StartDate:    profile.StartDate,
		EndDate:      profile.EndDate,
	})
	if err != nil {
		return CheckResult{}, recordFailedRun(storage, run, err, now)
	}
	if err := ctx.Err(); err != nil {
		cancelled := &medicover.Error{Code: medicover.CodeCancelled, Message: "appointment search was cancelled"}
		return CheckResult{}, recordFailedRun(storage, run, cancelled, now)
	}
	// Use the completion time for time-based episode rules. A slow search can
	// cross the appointment start time after the run begins.
	completedAt := time.Now().UTC()
	reconciliation, err := storage.ReconcileObservationRun(run.ID, observationSlots(search.Slots), completedAt)
	if err != nil {
		if errors.Is(err, store.ErrObservationRunStale) {
			return CheckResult{}, err
		}
		return CheckResult{}, recordFailedRun(storage, run, err, now)
	}
	return CheckResult{Search: search, Reconciliation: reconciliation}, nil
}

func observationSlots(slots []medicover.Slot) []store.ObservationSlot {
	result := make([]store.ObservationSlot, 0, len(slots))
	for _, slot := range slots {
		result = append(result, store.ObservationSlot{
			Identity:       slot.Identity,
			StableIdentity: slot.StableIdentity,
			BookingString:  slot.BookingString,
			Time:           slot.Time,
			Clinic:         slot.Clinic,
			Doctor:         slot.Doctor,
			Specialty:      slot.Specialty,
			VisitType:      slot.VisitType,
		})
	}
	return result
}

func recordFailedRun(storage *store.Store, run store.ObservationRun, original error, now time.Time) error {
	status, code := failedRunStatus(original)
	message := safeErrorMessage(original)
	if _, err := storage.FailObservationRun(run.ID, status, code, message, now); err != nil {
		return errors.Join(original, fmt.Errorf("record observation run: %w", err))
	}
	return original
}

func failedRunStatus(err error) (string, string) {
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		switch medicoverErr.Code {
		case medicover.CodePartial:
			return store.ObservationRunPartial, medicover.CodePartial
		case medicover.CodeCancelled:
			return store.ObservationRunCancelled, medicover.CodeCancelled
		case medicover.CodeConflicting:
			return store.ObservationRunConflicting, medicover.CodeConflicting
		case medicover.CodeStale:
			return store.ObservationRunStale, medicover.CodeStale
		default:
			return store.ObservationRunFailed, medicoverErr.Code
		}
	}
	if errors.Is(err, store.ErrObservationRunConflicting) {
		return store.ObservationRunConflicting, "conflicting_result"
	}
	if errors.Is(err, store.ErrObservationRunStale) {
		return store.ObservationRunStale, "stale_result"
	}
	return store.ObservationRunFailed, "observation_failure"
}

func safeErrorMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 2048 {
		return message[:2048]
	}
	return message
}
