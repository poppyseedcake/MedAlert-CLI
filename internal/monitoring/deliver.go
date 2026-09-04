// Package monitoring delivery processing owns durable Telegram availability
// notifications with duplicate control.
//
// ProcessAvailabilityDeliveries runs after a complete Observation Run. It
// cancels stale pending work for ended episodes, ensures one pending delivery
// for each new episode and each enabled linked destination, then attempts due
// deliveries while their slots remain available.
//
// Delivery is at-least-once: BeginDeliveryAttempt saves pending before the
// Telegram call and RecordDeliveryResult saves the confirmed outcome after.
// A stop between those writes retries later and may duplicate; that duplicate
// is accepted as documented behavior.
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

// DeliverySummary describes what one processing pass did. Deliveries holds the
// final states of attempted and stale-cancelled rows in this pass.
type DeliverySummary struct {
	Attempted   int              `json:"attempted"`
	Delivered   int              `json:"delivered"`
	StillRetry  int              `json:"retrying"`
	Failed      int              `json:"failed"`
	Cancelled   int              `json:"cancelled"`
	Deliveries  []store.Delivery `json:"deliveries"`
}

// ProcessAvailabilityDeliveries coordinates Telegram sends after a complete
// run. resolveToken returns the bot token for a destination without logging
// it. Temporary, unknown, rate-limited, and timeout results stay retryable
// for no more than store.MaxDeliveryAttempts attempts; a valid retry_after
// controls next_attempt_at. Permanent results and exhausted budgets become
// permanent_failure. Stale pending work for ended episodes stops without
// sending. A cancelled context stops without recording a result so the
// pending row retries later.
func ProcessAvailabilityDeliveries(ctx context.Context, storage *store.Store, profile store.Profile, reconciliation store.ObservationReconciliation, sender *telegram.Client, resolveToken func(store.Destination) (telegram.Secret, error), now time.Time) (DeliverySummary, error) {
	summary := DeliverySummary{Deliveries: []store.Delivery{}}
	if reconciliation.Run.Status != store.ObservationRunComplete {
		return summary, nil
	}
	if sender == nil || resolveToken == nil {
		return summary, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()

	endedIDs := make([]string, 0, len(reconciliation.EndedEpisodes))
	for _, episode := range reconciliation.EndedEpisodes {
		if trimmed := strings.TrimSpace(episode.ID); trimmed != "" {
			endedIDs = append(endedIDs, trimmed)
		}
	}
	if len(endedIDs) > 0 {
		cancelled, err := storage.CancelDeliveriesForEndedEpisodes(profile.ID, endedIDs, now)
		if err != nil {
			return summary, err
		}
		summary.Cancelled += cancelled
	}

	// Ensure one pending per enabled destination for every active episode.
	// New episodes are the common case; re-ensuring still-active episodes
	// keeps the operation self-healing after a transient store failure and
	// covers destinations linked after the episode started. The unique
	// (profile, episode, destination) index keeps this idempotent, so
	// repeated checks never create another delivered notification.
	activeEpisodes, err := storage.ListAvailabilityEpisodes(profile.ID)
	if err != nil {
		return summary, err
	}
	for _, episode := range activeEpisodes {
		if !episode.Active || strings.TrimSpace(episode.ID) == "" {
			continue
		}
		if _, err := storage.EnsureEpisodeDeliveries(profile.ID, episode.ID, now); err != nil {
			return summary, err
		}
	}

	// Recover rows stuck after a stopped final claim before selecting due
	// work: a pending row with attempts at the budget and an expired lease
	// would otherwise remain pending forever (ListDue and Begin both
	// exclude it). Reaped rows are newly terminal budget failures.
	reaped, err := storage.ReapExpiredMaxAttemptClaims(profile.ID, now)
	if err != nil {
		return summary, err
	}
	for _, final := range reaped {
		summary.Failed++
		summary.Deliveries = append(summary.Deliveries, final)
	}

	due, err := storage.ListDueDeliveries(profile.ID, now)
	if err != nil {
		return summary, err
	}
	for _, pending := range due {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		outcome, err := attemptOneDelivery(ctx, storage, profile, pending, sender, resolveToken)
		if err != nil {
			return summary, err
		}
		if outcome.stop {
			// Cancelled context: leave remaining pending for a later cycle
			// without recording a result.
			break
		}
		if outcome.skipped {
			continue
		}
		if outcome.staleCancelled {
			summary.Cancelled++
			summary.Deliveries = append(summary.Deliveries, outcome.final)
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

// deliveryOutcome distinguishes attempted sends from skips and shutdown stops
// so summary counts stay honest: skips never consume attempts, stale
// cancellations stop without sending, and stops leave pending for later.
type deliveryOutcome struct {
	final         store.Delivery
	skipped       bool
	staleCancelled bool
	stop          bool
}

func attemptOneDelivery(ctx context.Context, storage *store.Store, profile store.Profile, pending store.Delivery, sender *telegram.Client, resolveToken func(store.Destination) (telegram.Secret, error)) (deliveryOutcome, error) {
	// Re-verify the slot remains available with a fresh read. The
	// reconciliation just completed, so this is cheap defense against sending
	// stale availability when the episode ended between ListDue and the send.
	// Only a genuinely missing episode is skipped; other store failures are
	// returned so the pass surfaces them instead of silently dropping work.
	episode, err := storage.GetEpisode(pending.EpisodeID)
	if err != nil {
		if errors.Is(err, store.ErrDeliveryNotFound) {
			// Episode gone (profile deleted races): skip without consuming
			// more attempts; cascade cleanup removes the delivery row.
			return deliveryOutcome{skipped: true}, nil
		}
		return deliveryOutcome{}, err
	}
	if !episode.Active {
		staleNow := time.Now().UTC()
		if _, err := storage.CancelDeliveriesForEndedEpisodes(profile.ID, []string{episode.ID}, staleNow); err != nil {
			return deliveryOutcome{}, err
		}
		cancelled, err := storage.GetDelivery(pending.ID)
		if err != nil {
			return deliveryOutcome{}, err
		}
		return deliveryOutcome{final: cancelled, staleCancelled: true}, nil
	}
	destination, err := storage.GetDestination(pending.DestinationID)
	if err != nil {
		if errors.Is(err, store.ErrDestinationNotFound) {
			return deliveryOutcome{skipped: true}, nil
		}
		return deliveryOutcome{}, err
	}
	if !destination.Enabled {
		// Disabled destinations pause without consuming attempts; a later
		// re-enable makes the row due again while the episode stays active.
		return deliveryOutcome{skipped: true}, nil
	}
	if linked, err := storage.IsDestinationLinked(profile.ID, destination.ID); err != nil || !linked {
		// Explicitly unlinked destinations are never notified, even for
		// pending work created while they were linked. Relinking resumes
		// them via Ensure while the episode stays active.
		if err != nil {
			return deliveryOutcome{}, err
		}
		return deliveryOutcome{skipped: true}, nil
	}
	// Fresh claim timestamp per attempt: the batch `now` may be stale after
	// slow Telegram responses earlier in a long batch, which would store an
	// already-expired lease or backoff.
	claimed, err := storage.BeginDeliveryAttempt(pending.ID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrDeliveryNotFound) || errors.Is(err, store.ErrDeliveryInvalid) || errors.Is(err, store.ErrDeliveryNotDue) {
			// Lost race: claimed by a concurrent process holding the lease,
			// backed off, stale, disabled, or unlinked after ListDue.
			// No attempt consumed on the losing side.
			return deliveryOutcome{skipped: true}, nil
		}
		return deliveryOutcome{}, err
	}
	token, err := resolveToken(destination)
	if err != nil {
		// Transient secret outages (for example Secret Service temporarily
		// unavailable) stay retryable within the shared five-attempt budget;
		// missing or invalid configuration is terminal.
		status := store.DeliveryPermanentFailure
		if secrets.IsTransient(err) {
			status = store.DeliveryRetry
		}
		final, recordErr := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: status, LastError: shortDeliveryMessage(err)}, time.Now().UTC())
		if recordErr != nil {
			if errors.Is(recordErr, store.ErrDeliveryConflict) {
				return deliveryOutcome{skipped: true}, nil
			}
			return deliveryOutcome{}, recordErr
		}
		return deliveryOutcome{final: final}, nil
	}
	text := telegram.FormatAvailability(telegram.Availability{
		Profile:   profile.ID,
		Time:      episode.Time,
		Doctor:    episode.Doctor,
		Specialty: episode.Specialty,
		Clinic:    episode.Clinic,
		VisitType: episode.VisitType,
	})
	result, err := sender.SendMessage(ctx, token, destination.ChatID, text)
	if err != nil {
		var telegramErr *telegram.Error
		if ctx != nil && ctx.Err() != nil {
			return deliveryOutcome{stop: true}, nil
		}
		if errors.As(err, &telegramErr) && telegramErr.Code == telegram.CodeCancelled {
			return deliveryOutcome{stop: true}, nil
		}
		// Fresh confirmation timestamp per attempt: retry_after is relative
		// to the Telegram response, so anchoring it to a stale batch start
		// after a slow response would store an already-past backoff.
		recordedAt := time.Now().UTC()
		if errors.As(err, &telegramErr) {
			switch {
			case telegram.IsTemporary(err):
				next := store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortDeliveryMessage(err)}
				if telegramErr.Code == telegram.CodeRateLimited && telegramErr.RetryAfter > 0 {
					next.HasNextRetry = true
					next.NextAttempt = recordedAt.Add(telegramErr.RetryAfter)
				}
				final, recordErr := storage.RecordDeliveryResult(claimed.ID, claimed, next, recordedAt)
				if recordErr != nil {
					if errors.Is(recordErr, store.ErrDeliveryConflict) {
						return deliveryOutcome{skipped: true}, nil
					}
					return deliveryOutcome{}, recordErr
				}
				return deliveryOutcome{final: final}, nil
			case telegram.IsPermanent(err):
				final, recordErr := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryPermanentFailure, LastError: shortDeliveryMessage(err)}, recordedAt)
				if recordErr != nil {
					if errors.Is(recordErr, store.ErrDeliveryConflict) {
						return deliveryOutcome{skipped: true}, nil
					}
					return deliveryOutcome{}, recordErr
				}
				return deliveryOutcome{final: final}, nil
			default:
				final, recordErr := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortDeliveryMessage(err)}, recordedAt)
				if recordErr != nil {
					if errors.Is(recordErr, store.ErrDeliveryConflict) {
						return deliveryOutcome{skipped: true}, nil
					}
					return deliveryOutcome{}, recordErr
				}
				return deliveryOutcome{final: final}, nil
			}
		}
		final, recordErr := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortDeliveryMessage(err)}, recordedAt)
		if recordErr != nil {
			if errors.Is(recordErr, store.ErrDeliveryConflict) {
				return deliveryOutcome{skipped: true}, nil
			}
			return deliveryOutcome{}, recordErr
		}
		return deliveryOutcome{final: final}, nil
	}
	final, err := storage.RecordDeliveryResult(claimed.ID, claimed, store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: result.MessageID}, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrDeliveryConflict) {
			return deliveryOutcome{skipped: true}, nil
		}
		return deliveryOutcome{}, err
	}
	return deliveryOutcome{final: final}, nil
}

func shortDeliveryMessage(err error) string {
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
