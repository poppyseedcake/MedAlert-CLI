// Package monitoring incident processing owns durable operational failure
// and recovery notifications.
//
// One continuous operational problem is one active incident. Authentication
// Required and Protocol Changed start an incident immediately; a temporary
// observation problem becomes notifiable after three consecutive failed runs.
// Repeated failures update the active incident without new failure
// notifications. One complete successful run ends the incident with one
// recovery notification per destination that delivered the failure.
//
// Delivery is at-least-once with the same five-attempt budget and retry_after
// handling as availability, but without slot validation. A failure in an
// operational-notification delivery never creates another incident.
package monitoring

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

// IncidentSummary describes what one incident-delivery pass did.
type IncidentSummary struct {
	Attempted  int                      `json:"attempted"`
	Delivered  int                      `json:"delivered"`
	StillRetry int                      `json:"retrying"`
	Failed     int                      `json:"failed"`
	Deliveries []store.IncidentDelivery `json:"deliveries"`
}

// EligibleDestinationsForProfile returns the enabled linked destination ids
// for a profile, ordered lexicographically. Database failures are returned
// instead of an empty list: treating them as "no destinations" would create
// an incident without failure deliveries, and later updates can never add
// the missing routes (one failure notification per destination per
// incident).
func EligibleDestinationsForProfile(storage *store.Store, profileID string) ([]string, error) {
	destinations, err := storage.ListProfileDestinations(profileID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, destination := range destinations {
		if destination.Enabled {
			ids = append(ids, destination.ID)
		}
	}
	return ids, nil
}

// EligibleDestinationsForAccount returns the unique enabled linked
// destination ids across all profiles of an account. One account incident
// notifies each unique route once even when many profiles are affected.
func EligibleDestinationsForAccount(storage *store.Store, accountID string) ([]string, error) {
	profiles, err := storage.ListProfiles(accountID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := []string{}
	for _, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		ids, err := EligibleDestinationsForProfile(storage, profile.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
	}
	return result, nil
}

// OtherDestinationsForProfile returns the enabled linked destinations of a
// profile excluding the failed one. Route failures are reported through the
// other active routes, never the failed route itself.
func OtherDestinationsForProfile(storage *store.Store, profileID, failedDestinationID string) ([]string, error) {
	eligible, err := EligibleDestinationsForProfile(storage, profileID)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, id := range eligible {
		if id != failedDestinationID {
			result = append(result, id)
		}
	}
	return result, nil
}

// RecordAccountFailure tracks an authentication-phase failure for an account.
func RecordAccountFailure(storage *store.Store, accountID, failureCode, failureMessage string, now time.Time) (store.Incident, bool, error) {
	eligible, err := EligibleDestinationsForAccount(storage, accountID)
	if err != nil {
		return store.Incident{}, false, err
	}
	incident, _, newly, err := storage.RecordIncidentFailure(store.IncidentScopeAccount, accountID, accountID, "", "", failureCode, failureMessage, now, eligible)
	return incident, newly, err
}

// ResolveAccountIncident ends the account incident after authentication
// recovers. Recovery goes only to destinations that delivered the failure.
func ResolveAccountIncident(storage *store.Store, accountID string, now time.Time) (store.Incident, error) {
	incident, _, _, err := storage.ResolveIncident(store.IncidentScopeAccount, accountID, "", "", now)
	return incident, err
}

// RecordProfileFailure tracks a search-phase failure for a profile.
func RecordProfileFailure(storage *store.Store, profile store.Profile, failureCode, failureMessage string, now time.Time) (store.Incident, bool, error) {
	eligible, err := EligibleDestinationsForProfile(storage, profile.ID)
	if err != nil {
		return store.Incident{}, false, err
	}
	incident, _, newly, err := storage.RecordIncidentFailure(store.IncidentScopeProfile, profile.ID, profile.AccountID, profile.ID, "", failureCode, failureMessage, now, eligible)
	return incident, newly, err
}

// ResolveProfileIncident ends the profile incident after one complete
// successful run.
func ResolveProfileIncident(storage *store.Store, profile store.Profile, now time.Time) (store.Incident, error) {
	incident, _, _, err := storage.ResolveIncident(store.IncidentScopeProfile, profile.ID, profile.ID, "", now)
	return incident, err
}

// RecordDestinationFailure tracks a permanent Telegram delivery failure for
// one profile and destination. Notifiers exclude the failed route.
func RecordDestinationFailure(storage *store.Store, profile store.Profile, failedDestinationID, failureCode, failureMessage string, now time.Time) (store.Incident, bool, error) {
	notifiers, err := OtherDestinationsForProfile(storage, profile.ID, failedDestinationID)
	if err != nil {
		return store.Incident{}, false, err
	}
	incident, _, newly, err := storage.RecordIncidentFailure(store.IncidentScopeDestination, failedDestinationID, profile.AccountID, profile.ID, failedDestinationID, failureCode, failureMessage, now, notifiers)
	return incident, newly, err
}

// ResolveDestinationIncident ends the destination incident after a later
// delivery to the failed destination succeeds.
func ResolveDestinationIncident(storage *store.Store, profile store.Profile, destinationID string, now time.Time) (store.Incident, error) {
	incident, _, _, err := storage.ResolveIncident(store.IncidentScopeDestination, destinationID, profile.ID, destinationID, now)
	return incident, err
}

// ProcessIncidentDeliveries sends due operational failure and recovery
// notifications. resolveToken returns the bot token without logging it.
// Temporary, unknown, rate-limited, and timeout results stay retryable for no
// more than store.MaxDeliveryAttempts attempts; valid retry_after controls
// next_attempt_at. Permanent results become permanent_failure. A cancelled
// context stops without recording a result so the row retries later.
// One scope's failure never stops other scopes: per-delivery skips continue.
func ProcessIncidentDeliveries(ctx context.Context, storage *store.Store, sender *telegram.Client, resolveToken func(store.Destination) (telegram.Secret, error), now time.Time) (IncidentSummary, error) {
	summary := IncidentSummary{Deliveries: []store.IncidentDelivery{}}
	if sender == nil || resolveToken == nil {
		return summary, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	reaped, err := storage.ReapExpiredIncidentClaims(now)
	if err != nil {
		return summary, err
	}
	for _, final := range reaped {
		summary.Failed++
		summary.Deliveries = append(summary.Deliveries, final)
	}
	due, err := storage.ListDueIncidentDeliveries(now)
	if err != nil {
		return summary, err
	}
	for _, pending := range due {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		outcome, err := attemptOneIncidentDelivery(ctx, storage, pending, sender, resolveToken)
		if err != nil {
			return summary, err
		}
		if outcome.stop {
			break
		}
		if outcome.skipped {
			continue
		}
		summary.Attempted++
		summary.Deliveries = append(summary.Deliveries, outcome.final)
		switch outcome.final.Status {
		case store.DeliveryDelivered:
			summary.Delivered++
		case store.DeliveryRetry, store.DeliveryPending:
			summary.StillRetry++
		case store.DeliveryPermanentFailure:
			summary.Failed++
		}
	}
	return summary, nil
}

type incidentOutcome struct {
	final   store.IncidentDelivery
	skipped bool
	stop    bool
}

func attemptOneIncidentDelivery(ctx context.Context, storage *store.Store, pending store.IncidentDelivery, sender *telegram.Client, resolveToken func(store.Destination) (telegram.Secret, error)) (incidentOutcome, error) {
	incident, err := storage.GetIncident(pending.IncidentID)
	if err != nil {
		if errors.Is(err, store.ErrIncidentNotFound) {
			return incidentOutcome{skipped: true}, nil
		}
		return incidentOutcome{}, err
	}
	destination, err := storage.GetDestination(pending.DestinationID)
	if err != nil {
		if errors.Is(err, store.ErrDestinationNotFound) {
			return incidentOutcome{skipped: true}, nil
		}
		return incidentOutcome{}, err
	}
	if !destination.Enabled {
		return incidentOutcome{skipped: true}, nil
	}
	claimed, err := storage.BeginIncidentDeliveryAttempt(pending.ID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrDeliveryNotFound) || errors.Is(err, store.ErrDeliveryInvalid) || errors.Is(err, store.ErrDeliveryNotDue) {
			return incidentOutcome{skipped: true}, nil
		}
		return incidentOutcome{}, err
	}
	token, err := resolveToken(destination)
	if err != nil {
		status := store.DeliveryPermanentFailure
		if secrets.IsTransient(err) {
			status = store.DeliveryRetry
		}
		final, recordErr := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: status, LastError: shortIncidentMessage(err)}, time.Now().UTC())
		if recordErr != nil {
			if errors.Is(recordErr, store.ErrDeliveryConflict) {
				return incidentOutcome{skipped: true}, nil
			}
			return incidentOutcome{}, recordErr
		}
		return incidentOutcome{final: final}, nil
	}
	var text string
	if pending.Kind == store.IncidentDeliveryRecovery {
		text = telegram.FormatOperationalRecovery(telegram.OperationalProblem{
			Scope:       incident.ScopeType,
			Account:     incident.AccountID,
			Profile:     incident.ProfileID,
			Destination: incident.DestinationID,
			Code:        incident.FailureCode,
			Message:     incident.FailureMessage,
		})
	} else {
		text = telegram.FormatOperationalFailure(telegram.OperationalProblem{
			Scope:       incident.ScopeType,
			Account:     incident.AccountID,
			Profile:     incident.ProfileID,
			Destination: incident.DestinationID,
			Code:        incident.FailureCode,
			Message:     incident.FailureMessage,
		})
	}
	result, err := sender.SendMessage(ctx, token, destination.ChatID, text)
	if err != nil {
		var telegramErr *telegram.Error
		if ctx != nil && ctx.Err() != nil {
			return incidentOutcome{stop: true}, nil
		}
		if errors.As(err, &telegramErr) && telegramErr.Code == telegram.CodeCancelled {
			return incidentOutcome{stop: true}, nil
		}
		recordedAt := time.Now().UTC()
		if errors.As(err, &telegramErr) {
			switch {
			case telegram.IsTemporary(err):
				next := store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortIncidentMessage(err)}
				if telegramErr.Code == telegram.CodeRateLimited && telegramErr.RetryAfter > 0 {
					next.HasNextRetry = true
					next.NextAttempt = recordedAt.Add(telegramErr.RetryAfter)
				}
				final, recordErr := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, next, recordedAt)
				if recordErr != nil {
					if errors.Is(recordErr, store.ErrDeliveryConflict) {
						return incidentOutcome{skipped: true}, nil
					}
					return incidentOutcome{}, recordErr
				}
				return incidentOutcome{final: final}, nil
			case telegram.IsPermanent(err):
				final, recordErr := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryPermanentFailure, LastError: shortIncidentMessage(err)}, recordedAt)
				if recordErr != nil {
					if errors.Is(recordErr, store.ErrDeliveryConflict) {
						return incidentOutcome{skipped: true}, nil
					}
					return incidentOutcome{}, recordErr
				}
				return incidentOutcome{final: final}, nil
			default:
				final, recordErr := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortIncidentMessage(err)}, recordedAt)
				if recordErr != nil {
					if errors.Is(recordErr, store.ErrDeliveryConflict) {
						return incidentOutcome{skipped: true}, nil
					}
					return incidentOutcome{}, recordErr
				}
				return incidentOutcome{final: final}, nil
			}
		}
		final, recordErr := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortIncidentMessage(err)}, recordedAt)
		if recordErr != nil {
			if errors.Is(recordErr, store.ErrDeliveryConflict) {
				return incidentOutcome{skipped: true}, nil
			}
			return incidentOutcome{}, recordErr
		}
		return incidentOutcome{final: final}, nil
	}
	final, err := storage.RecordIncidentDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: result.MessageID}, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrDeliveryConflict) {
			return incidentOutcome{skipped: true}, nil
		}
		return incidentOutcome{}, err
	}
	return incidentOutcome{final: final}, nil
}

func shortIncidentMessage(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 500 {
		return message[:500]
	}
	if message == "" {
		return "telegram send failed"
	}
	return message
}
