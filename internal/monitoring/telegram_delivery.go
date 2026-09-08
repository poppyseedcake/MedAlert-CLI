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

// telegramDeliveryOutcome is the common result of one Telegram delivery
// attempt. The caller records Result in its own durable delivery table. A
// stopped attempt has no result because its claim must remain retryable.
type telegramDeliveryOutcome struct {
	result     store.DeliveryResult
	recordedAt time.Time
	stop       bool
}

// sendTelegramDelivery owns token resolution, Telegram error classification,
// retry_after handling, and cancellation for both availability and incident
// deliveries. The durable row remains owned by the caller because the two
// delivery tables have different records and claim methods.
func sendTelegramDelivery(
	ctx context.Context,
	sender *telegram.Client,
	destination store.Destination,
	message string,
	resolveToken func(store.Destination) (telegram.Secret, error),
) telegramDeliveryOutcome {
	token, err := resolveToken(destination)
	if err != nil {
		recordedAt := time.Now().UTC()
		status := store.DeliveryPermanentFailure
		if secrets.IsTransient(err) {
			status = store.DeliveryRetry
		}
		return telegramDeliveryOutcome{
			result: store.DeliveryResult{Status: status, LastError: shortTelegramMessage(err)}, recordedAt: recordedAt,
		}
	}

	result, err := sender.SendMessage(ctx, token, destination.ChatID, message)
	if err == nil {
		return telegramDeliveryOutcome{
			result:     store.DeliveryResult{Status: store.DeliveryDelivered, MessageID: result.MessageID},
			recordedAt: time.Now().UTC(),
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return telegramDeliveryOutcome{stop: true}
	}
	var telegramErr *telegram.Error
	if errors.As(err, &telegramErr) && telegramErr.Code == telegram.CodeCancelled {
		return telegramDeliveryOutcome{stop: true}
	}
	recordedAt := time.Now().UTC()
	return telegramDeliveryOutcome{
		result:     telegramDeliveryResult(err, recordedAt),
		recordedAt: recordedAt,
	}
}

// telegramDeliveryResult maps one non-cancelled Telegram send error to the
// durable retry policy. Unknown errors remain retryable, and rate limits use
// the response timestamp so a slow request cannot consume the backoff.
func telegramDeliveryResult(err error, recordedAt time.Time) store.DeliveryResult {
	result := store.DeliveryResult{Status: store.DeliveryRetry, LastError: shortTelegramMessage(err)}
	var telegramErr *telegram.Error
	if !errors.As(err, &telegramErr) {
		return result
	}
	switch {
	case telegram.IsTemporary(err):
		if telegramErr.Code == telegram.CodeRateLimited && telegramErr.RetryAfter > 0 {
			result.HasNextRetry = true
			result.NextAttempt = recordedAt.Add(telegramErr.RetryAfter)
		}
	case telegram.IsPermanent(err):
		result.Status = store.DeliveryPermanentFailure
	}
	return result
}

func shortTelegramMessage(err error) string {
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
