package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/poppyseedcake/MedAlert/internal/application"
	"github.com/poppyseedcake/MedAlert/internal/medicover"
)

func runCheck(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	return runCheckWithContext(context.Background(), command, settings, stdin, stdout, stderr)
}

func runCheckWithContext(ctx context.Context, command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	return runCheckWithPrompt(ctx, command, settings, stdin, stdout, stderr, nil)
}

// runCheckWithPrompt adapts the shared application check to command output.
// The optional callback is used by the Polish TUI for hidden password and MFA
// input; regular command checks stay non-interactive.
func runCheckWithPrompt(ctx context.Context, command string, settings options, stdin *os.File, stdout, stderr io.Writer, prompt func(context.Context, string) (string, error)) int {
	id, ok := checkProfileForCheck(command, settings, stderr, settings.output == "json")
	if !ok {
		return 2
	}
	checked, err := application.New(application.Config{
		Database:         settings.database,
		SessionDir:       settings.sessionDir,
		MedicoverBaseURL: settings.medicoverBaseURL,
		TelegramBaseURL:  settings.telegramBaseURL,
	}).Check(ctx, application.CheckRequest{
		ProfileID: id,
		Dry:       settings.dry,
		Stdin:     stdin,
		Prompt:    prompt,
	})
	if err != nil {
		return reportApplicationCheckError(stderr, command, err, settings.output == "json")
	}
	if checked.Dry {
		data := map[string]any{
			"account": checked.Account.ID, "profile": checked.Profile.ID, "dry": true,
			"complete": true, "slots": checked.Search.Slots, "slot_count": len(checked.Search.Slots),
			"pages": checked.Search.Pages,
		}
		if settings.output == "json" {
			writeResult(stdout, command, data)
			return 0
		}
		fmt.Fprintf(stdout, "Dry check for profile %s (account %s) found %d available slots.\n", checked.Profile.ID, checked.Account.ID, len(checked.Search.Slots))
		for _, slot := range checked.Search.Slots {
			fmt.Fprintf(stdout, "%s  %s  %s  %s\n", slot.Time, slot.Doctor, slot.Clinic, slot.Identity)
		}
		return 0
	}
	reconciliation := checked.Reconciliation
	deliveries := checked.Deliveries
	incidents := checked.Incidents
	data := map[string]any{
		"account": checked.Account.ID, "profile": checked.Profile.ID, "dry": false, "complete": true,
		"run": reconciliation.Run, "slots": checked.Search.Slots, "slot_count": len(checked.Search.Slots),
		"pages": checked.Search.Pages, "new_episodes": reconciliation.NewEpisodes,
		"ended_episodes": reconciliation.EndedEpisodes, "newly_available": len(reconciliation.NewEpisodes),
		"ended": len(reconciliation.EndedEpisodes), "active_episode_count": checked.ActiveEpisodeCount,
		"deliveries": deliveries.Deliveries, "delivered": deliveries.Delivered,
		"delivery_failed": deliveries.Failed, "delivery_pending": deliveries.StillRetry,
		"delivery_cancelled": deliveries.Cancelled, "incident_deliveries": incidents.Deliveries,
		"incident_delivered": incidents.Delivered, "incident_failed": incidents.Failed,
		"incident_pending": incidents.StillRetry,
	}
	if settings.output == "json" {
		writeResult(stdout, command, data)
		return 0
	}
	fmt.Fprintf(stdout, "Check for profile %s (account %s) found %d available slots; %d newly available, %d ended.\n", checked.Profile.ID, checked.Account.ID, len(checked.Search.Slots), len(reconciliation.NewEpisodes), len(reconciliation.EndedEpisodes))
	for _, slot := range checked.Search.Slots {
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", slot.Time, slot.Doctor, slot.Clinic, slot.Identity)
	}
	if deliveries.Attempted > 0 || deliveries.Cancelled > 0 {
		fmt.Fprintf(stdout, "Telegram: %d delivered, %d pending, %d failed, %d cancelled.\n", deliveries.Delivered, deliveries.StillRetry, deliveries.Failed, deliveries.Cancelled)
	}
	if incidents.Attempted > 0 {
		fmt.Fprintf(stdout, "Incidents: %d delivered, %d pending, %d failed.\n", incidents.Delivered, incidents.StillRetry, incidents.Failed)
	}
	return 0
}

func checkProfileForCheck(command string, settings options, stderr io.Writer, jsonOutput bool) (string, bool) {
	if len(settings.positionals) > 1 || (settings.profileID != "" && len(settings.positionals) > 0 && settings.positionals[0] != settings.profileID) {
		writeError(stderr, command, "invalid_arguments", "use either --profile or one positional profile id", jsonOutput)
		return "", false
	}
	id := settings.profileID
	if id == "" && len(settings.positionals) == 1 {
		id = settings.positionals[0]
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "profile id is required", jsonOutput)
		return "", false
	}
	return strings.TrimSpace(id), true
}

func reportApplicationCheckError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code, detail := application.ErrorInfo(err)
	switch code {
	case "profile_not_found":
		return reportProfileError(stderr, command, err, jsonOutput)
	case "account_not_found":
		return reportAccountError(stderr, command, err, jsonOutput)
	case "profile_disabled":
		writeError(stderr, command, code, detail, jsonOutput)
		return 2
	case "run_active":
		writeError(stderr, command, code, detail, jsonOutput)
		return 4
	case "conflicting_result", "stale_result":
		writeError(stderr, command, code, detail, jsonOutput)
		return 6
	case "invalid_arguments":
		writeError(stderr, command, code, detail, jsonOutput)
		return 2
	case "missing_input", "secret_error":
		writeError(stderr, command, code, detail, jsonOutput)
		return 2
	case "database_error":
		return reportStoreError(stderr, command, err, jsonOutput)
	default:
		return reportMedicoverError(stderr, command, &medicover.Error{Code: code, Message: detail}, jsonOutput)
	}
}
