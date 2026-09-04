package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/monitoring"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

// incidentFailureCode extracts a stable failure code and safe message for
// operational incidents. Medicover codes cross the seam sanitized; store
// sentinel errors map to their run codes.
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

// shouldRecordProfileIncident reports whether a check failure should update
// the profile incident. Lease conflicts, disabled profiles, stale/conflicting
// edits, and shutdown cancellations are not portal problems.
func shouldRecordProfileIncident(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrObservationRunActive) ||
		errors.Is(err, store.ErrProfileDisabled) ||
		errors.Is(err, store.ErrObservationRunInvalid) ||
		errors.Is(err, store.ErrObservationRunNotFound) ||
		errors.Is(err, store.ErrProfileNotFound) {
		return false
	}
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		switch medicoverErr.Code {
		case medicover.CodeCancelled:
			return false
		case medicover.CodeConflicting, medicover.CodeStale:
			return false
		default:
			return true
		}
	}
	if errors.Is(err, store.ErrObservationRunConflicting) || errors.Is(err, store.ErrObservationRunStale) {
		return false
	}
	// Context cancellations during shutdown never create incidents.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// shouldRecordAccountIncident reports whether an authentication failure
// should update the account incident. Secret configuration errors and
// shutdown cancellations are not operational incidents.
func shouldRecordAccountIncident(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, secrets.ErrMissingInput) {
		return false
	}
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		if medicoverErr.Code == medicover.CodeCancelled {
			return false
		}
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return false
}

// recordAccountFailure tracks an auth-phase failure and queues the failure
// notification when it becomes notifiable. Store failures are returned;
// Telegram retry state never changes the caller's exit code.
func recordAccountFailure(storage *store.Store, account store.Account, authErr error, now time.Time) error {
	if !shouldRecordAccountIncident(authErr) {
		return nil
	}
	code, message := incidentFailureCode(authErr)
	if _, _, err := monitoring.RecordAccountFailure(storage, account.ID, code, message, now); err != nil {
		return err
	}
	return nil
}

// recordProfileFailure tracks a search-phase failure for a profile.
func recordProfileFailure(storage *store.Store, profile store.Profile, checkErr error, now time.Time) error {
	if !shouldRecordProfileIncident(checkErr) {
		return nil
	}
	code, message := incidentFailureCode(checkErr)
	if _, _, err := monitoring.RecordProfileFailure(storage, profile, code, message, now); err != nil {
		return err
	}
	return nil
}

// resolveIncidentsOnSuccess ends the profile incident and the account
// incident when one complete run proves monitoring works again. The first
// success for any profile of a paused account resumes the account; remaining
// profile-specific problems keep their own incidents.
func resolveIncidentsOnSuccess(storage *store.Store, profile store.Profile, now time.Time) error {
	if _, err := monitoring.ResolveProfileIncident(storage, profile, now); err != nil {
		return err
	}
	if _, err := monitoring.ResolveAccountIncident(storage, profile.AccountID, now); err != nil {
		return err
	}
	return nil
}

// trackDestinationIncidents creates destination incidents for permanent
// non-stale availability failures and resolves incidents for destinations
// that delivered successfully. Stale cancellations (slot disappeared) are
// normal lifecycle, never destination incidents. Budget-exhausted retries
// ("gave up after 5 attempts") indicate a persistent transport problem, not a
// destination configuration problem, and stay as delivery history without a
// separate incident to avoid notification storms when all routes fail.
func trackDestinationIncidents(storage *store.Store, profile store.Profile, summary monitoring.DeliverySummary, now time.Time) error {
	for _, delivery := range summary.Deliveries {
		if delivery.Status != store.DeliveryPermanentFailure {
			continue
		}
		if strings.Contains(strings.ToLower(delivery.LastError), "no longer available") {
			continue
		}
		if strings.Contains(strings.ToLower(delivery.LastError), "incident ended before") {
			continue
		}
		if strings.Contains(strings.ToLower(delivery.LastError), "5 attempts") {
			continue
		}
		// Only availability permanent failures reach here; incident summary
		// deliveries are handled separately and never create incidents.
		if _, _, err := monitoring.RecordDestinationFailure(storage, profile, delivery.DestinationID, "permanent_failure", delivery.LastError, now); err != nil {
			return err
		}
	}
	// A later successful delivery to a previously failed destination ends its
	// destination incident; recovery goes through the routes that delivered
	// the failure.
	for _, delivery := range summary.Deliveries {
		if delivery.Status != store.DeliveryDelivered {
			continue
		}
		if _, err := monitoring.ResolveDestinationIncident(storage, profile, delivery.DestinationID, now); err != nil {
			return err
		}
	}
	return nil
}

// processIncidentDeliveries sends due operational failure/recovery
// notifications. Observation success wins: retryable Telegram failures stay
// pending for later cycles and never change the caller's exit code. Only
// durable store failures are returned.
func processIncidentDeliveries(ctx context.Context, storage *store.Store, settings options, stdin *os.File, stderr io.Writer) (monitoring.IncidentSummary, error) {
	sender := telegramSenderFor(settings)
	resolve := func(destination store.Destination) (telegram.Secret, error) {
		token, err := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, stdin, stderr, true)
		if err != nil {
			return "", err
		}
		secret := telegram.Secret(token)
		token = ""
		return secret, nil
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return monitoring.ProcessIncidentDeliveries(deliveryCtx, storage, sender, resolve, time.Now().UTC())
}

func incidentStderr(_ io.Writer) {}
