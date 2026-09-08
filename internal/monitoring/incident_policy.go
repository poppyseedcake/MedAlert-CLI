package monitoring

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

// IncidentChange reports a saved transition. Adapters use it for display and
// events; the policy itself does not send messages or write command output.
type IncidentChange struct {
	Incident       store.Incident
	Started        bool
	RecoveryQueued bool
}

// ReconcileDestinationIncidents applies availability delivery outcomes. A
// failure wins over success for the same destination in one pass. Cancelled
// deliveries and exhausted retries do not indicate destination configuration
// failures. Partial changes are returned with any subsequent storage error.
func ReconcileDestinationIncidents(storage *store.Store, profile store.Profile, summary DeliverySummary, now time.Time) ([]IncidentChange, error) {
	changes := []IncidentChange{}
	failed := map[string]bool{}
	for _, delivery := range summary.Deliveries {
		message := strings.ToLower(delivery.LastError)
		if delivery.Status != store.DeliveryPermanentFailure || strings.Contains(message, "no longer available") || strings.Contains(message, "incident ended before") || strings.Contains(message, "5 attempts") {
			continue
		}
		failed[delivery.DestinationID] = true
		incident, newly, err := RecordDestinationFailure(storage, profile, delivery.DestinationID, "permanent_failure", delivery.LastError, now)
		if err != nil {
			return changes, err
		}
		changes = append(changes, IncidentChange{Incident: incident, Started: newly})
	}
	for _, delivery := range summary.Deliveries {
		if delivery.Status != store.DeliveryDelivered || failed[delivery.DestinationID] {
			continue
		}
		incident, recoveries, _, err := storage.ResolveIncident(store.IncidentScopeDestination, delivery.DestinationID, profile.ID, delivery.DestinationID, now)
		if err != nil {
			return changes, err
		}
		if incident.ID != "" {
			changes = append(changes, IncidentChange{Incident: incident, RecoveryQueued: len(recoveries) > 0})
		}
	}
	return changes, nil
}

// HandleAuthenticationFailure classifies an authentication error and records
// the account incident when required. A zero change means no incident.
func HandleAuthenticationFailure(storage *store.Store, accountID string, failure error, now time.Time) (IncidentChange, error) {
	if !shouldRecordAccountIncident(failure) {
		return IncidentChange{}, nil
	}
	code, message := incidentFailureCode(failure)
	incident, newly, err := RecordAccountFailure(storage, accountID, code, message, now)
	return IncidentChange{Incident: incident, Started: newly}, err
}

// HandleCheckFailure selects the account for authentication failures and the
// profile for other operational failures. Local conflicts and cancellation
// do not change incidents.
func HandleCheckFailure(storage *store.Store, profile store.Profile, failure error, now time.Time) (IncidentChange, error) {
	if medicover.IsAuthRequired(failure) {
		return HandleAuthenticationFailure(storage, profile.AccountID, failure, now)
	}
	if !shouldRecordProfileIncident(failure) {
		return IncidentChange{}, nil
	}
	code, message := incidentFailureCode(failure)
	incident, newly, err := RecordProfileFailure(storage, profile, code, message, now)
	return IncidentChange{Incident: incident, Started: newly}, err
}

// ResolveCheckIncidents ends profile and account incidents after a complete
// check. Both scopes are attempted; saved changes and joined errors let each
// adapter report partial completion without losing recovery events.
func ResolveCheckIncidents(storage *store.Store, profile store.Profile, now time.Time) ([]IncidentChange, error) {
	changes := []IncidentChange{}
	incident, recoveries, _, profileErr := storage.ResolveIncident(store.IncidentScopeProfile, profile.ID, profile.ID, "", now)
	if profileErr == nil && incident.ID != "" {
		changes = append(changes, IncidentChange{Incident: incident, RecoveryQueued: len(recoveries) > 0})
	}
	incident, recoveries, _, accountErr := storage.ResolveIncident(store.IncidentScopeAccount, profile.AccountID, "", "", now)
	if accountErr == nil && incident.ID != "" {
		changes = append(changes, IncidentChange{Incident: incident, RecoveryQueued: len(recoveries) > 0})
	}
	return changes, errors.Join(profileErr, accountErr)
}

func incidentFailureCode(err error) (string, string) {
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		code := strings.TrimSpace(medicoverErr.Code)
		if code == "" {
			code = "temporary_failure"
		}
		return code, strings.TrimSpace(medicoverErr.Message)
	}
	switch {
	case errors.Is(err, store.ErrObservationRunConflicting):
		return medicover.CodeConflicting, strings.TrimSpace(err.Error())
	case errors.Is(err, store.ErrObservationRunStale):
		return medicover.CodeStale, strings.TrimSpace(err.Error())
	case errors.Is(err, store.ErrObservationRunActive):
		return "run_active", strings.TrimSpace(err.Error())
	case errors.Is(err, store.ErrProfileDisabled):
		return "profile_disabled", strings.TrimSpace(err.Error())
	default:
		message := strings.TrimSpace(err.Error())
		if len(message) > 500 {
			message = message[:500]
		}
		if message == "" {
			message = "observation failed"
		}
		return "temporary_failure", message
	}
}

func shouldRecordProfileIncident(err error) bool {
	if err == nil || errors.Is(err, store.ErrObservationRunActive) || errors.Is(err, store.ErrProfileDisabled) ||
		errors.Is(err, store.ErrObservationRunInvalid) || errors.Is(err, store.ErrObservationRunNotFound) || errors.Is(err, store.ErrProfileNotFound) {
		return false
	}
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		switch medicoverErr.Code {
		case medicover.CodeCancelled, medicover.CodeTimeout, medicover.CodeConflicting, medicover.CodeStale:
			return false
		default:
			return true
		}
	}
	if errors.Is(err, store.ErrObservationRunConflicting) || errors.Is(err, store.ErrObservationRunStale) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func shouldRecordAccountIncident(err error) bool {
	if err == nil || errors.Is(err, secrets.ErrMissingInput) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var medicoverErr *medicover.Error
	return errors.As(err, &medicoverErr) && medicoverErr.Code != medicover.CodeCancelled && medicoverErr.Code != medicover.CodeTimeout
}
