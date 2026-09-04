package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/monitoring"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

// telegramSenderFor builds the concrete Telegram client for availability
// deliveries. BaseURL defaults to production; tests override it with a local
// server through --telegram-base-url or MEDALERT_TELEGRAM_BASE_URL.
func telegramSenderFor(settings options) *telegram.Client {
	return telegram.NewClient(telegram.Config{BaseURL: strings.TrimSpace(settings.telegramBaseURL)})
}



// deliverAfterCheck runs durable Telegram notifications after a complete
// observation run. Observation success always wins: Telegram retryable
// failures stay pending for later cycles and never change the check exit
// code. Only durable store failures are returned.
//
// Token resolution never prompts: check and watch already resolve account
// passwords non-interactively, and a prompt-based destination linked to a
// profile would otherwise block automation. Prompt destinations record a
// permanent missing_input failure so the operator can switch them to a file
// or Secret Service reference.
func deliverAfterCheck(ctx context.Context, storage *store.Store, profile store.Profile, result monitoring.CheckResult, settings options, stdin *os.File, stderr io.Writer) (monitoring.DeliverySummary, error) {
	sender := telegramSenderFor(settings)
	// Resolve through a closure so the monitoring layer never imports secrets.
	resolve := func(destination store.Destination) (telegram.Secret, error) {
		token, err := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, stdin, stderr, true)
		if err != nil {
			return "", err
		}
		secret := telegram.Secret(token)
		token = ""
		return secret, nil
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return monitoring.ProcessAvailabilityDeliveries(deliveryCtx, storage, profile, result.Reconciliation, sender, resolve, time.Now().UTC())
}
