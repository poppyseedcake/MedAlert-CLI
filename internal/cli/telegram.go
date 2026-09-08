package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/application"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

func runTelegram(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	switch command {
	case "telegram list":
		return telegramList(command, settings, stdout, stderr, jsonOutput)
	case "telegram create":
		return telegramCreate(command, settings, stdin, stdout, stderr, jsonOutput)
	case "telegram show":
		return telegramShow(command, settings, stdout, stderr, jsonOutput)
	case "telegram edit":
		return telegramEdit(command, settings, stdin, stdout, stderr, jsonOutput)
	case "telegram enable":
		return telegramSetEnabled(command, settings, true, stdout, stderr, jsonOutput)
	case "telegram disable":
		return telegramSetEnabled(command, settings, false, stdout, stderr, jsonOutput)
	case "telegram test":
		return telegramTest(command, settings, stdin, stdout, stderr, jsonOutput)
	case "telegram delete":
		return telegramDelete(command, settings, stdout, stderr, jsonOutput)
	default:
		writeError(stderr, command, "invalid_arguments", "a supported telegram command is required", jsonOutput)
		return 2
	}
}

func telegramApplicationFor(settings options) *application.Application {
	return application.New(application.Config{
		Database:        settings.database,
		TelegramBaseURL: settings.telegramBaseURL,
	})
}

func resolveTelegramID(settings options) string {
	if strings.TrimSpace(settings.telegramIDsRaw) != "" {
		// For telegram commands the flag holds a single id. Commas are
		// rejected by the handlers so a copy-paste list fails loudly.
		return strings.TrimSpace(settings.telegramIDsRaw)
	}
	if len(settings.positionals) > 0 {
		return strings.TrimSpace(settings.positionals[0])
	}
	return ""
}

func checkTelegramIDPositionals(command string, settings options, needsID bool, stderr io.Writer, jsonOutput bool) (string, bool) {
	flagID := strings.TrimSpace(settings.telegramIDsRaw)
	var positionalID string
	if len(settings.positionals) > 0 {
		positionalID = strings.TrimSpace(settings.positionals[0])
	}
	if flagID != "" && positionalID != "" && flagID != positionalID {
		writeError(stderr, command, "invalid_arguments", "use either --telegram or a positional id, not both", jsonOutput)
		return "", false
	}
	if needsID {
		if len(settings.positionals) > 1 {
			writeError(stderr, command, "invalid_arguments", "too many arguments: expected at most one telegram id", jsonOutput)
			return "", false
		}
	} else {
		if len(settings.positionals) > 0 {
			writeError(stderr, command, "invalid_arguments", "too many arguments for telegram list", jsonOutput)
			return "", false
		}
	}
	return resolveTelegramID(settings), true
}

func telegramList(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	if _, ok := checkTelegramIDPositionals(command, settings, false, stderr, jsonOutput); !ok {
		return 2
	}
	if strings.TrimSpace(settings.telegramIDsRaw) != "" {
		writeError(stderr, command, "invalid_arguments", "--telegram is not supported for telegram list", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	destinations, err := storage.ListDestinations()
	if err != nil {
		return reportTelegramError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"destinations": destinations})
		return 0
	}
	if len(destinations) == 0 {
		fmt.Fprintln(stdout, "No telegram destinations found.")
		return 0
	}
	for _, destination := range destinations {
		fmt.Fprintln(stdout, formatDestinationLine(destination))
	}
	return 0
}

func telegramCreate(command string, settings options, stdin *os.File, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkTelegramIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "telegram id is required (use --telegram or a positional id)", jsonOutput)
		return 2
	}
	if strings.Contains(id, ",") {
		writeError(stderr, command, "invalid_arguments", "telegram id must be a single id, not a list", jsonOutput)
		return 2
	}
	name := strings.TrimSpace(settings.telegramName)
	if name == "" {
		writeError(stderr, command, "invalid_arguments", "name is required (use --name)", jsonOutput)
		return 2
	}
	chatID := strings.TrimSpace(settings.chatID)
	if chatID == "" {
		writeError(stderr, command, "invalid_arguments", "chat id is required (use --chat-id)", jsonOutput)
		return 2
	}
	if countTokenOptions(settings) > 1 {
		writeError(stderr, command, "invalid_arguments", "use only one of --token-file, --token-prompt, --no-stored-token", jsonOutput)
		return 2
	}
	source, ref := intendedTokenSource(settings)
	created, err := telegramApplicationFor(settings).Telegram(context.Background(), application.TelegramRequest{
		Action: "create", ID: id,
		Values:         application.DestinationValues{Name: name, ChatID: chatID},
		TokenSelection: &application.TelegramTokenSelection{Source: source, Ref: ref},
	}, telegramPromptForCLI(settings, stdin, stderr))
	if err != nil {
		return reportApplicationTelegramError(stderr, command, err, jsonOutput)
	}
	writeDestination(stdout, command, created, fmt.Sprintf("Created telegram destination %s.\n", created.ID), jsonOutput)
	return 0
}

func telegramShow(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkTelegramIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "telegram id is required (use --telegram or a positional id)", jsonOutput)
		return 2
	}
	if strings.Contains(id, ",") {
		writeError(stderr, command, "invalid_arguments", "telegram id must be a single id, not a list", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	destination, err := storage.GetDestination(strings.TrimSpace(id))
	if err != nil {
		return reportTelegramError(stderr, command, err, jsonOutput)
	}
	writeDestination(stdout, command, destination, "", jsonOutput)
	return 0
}

func telegramEdit(command string, settings options, stdin *os.File, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkTelegramIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "telegram id is required (use --telegram or a positional id)", jsonOutput)
		return 2
	}
	if strings.Contains(id, ",") {
		writeError(stderr, command, "invalid_arguments", "telegram id must be a single id, not a list", jsonOutput)
		return 2
	}
	if countTokenOptions(settings) > 1 {
		writeError(stderr, command, "invalid_arguments", "use only one of --token-file, --token-prompt, --no-stored-token", jsonOutput)
		return 2
	}
	hasName := strings.TrimSpace(settings.telegramName) != ""
	hasChat := strings.TrimSpace(settings.chatID) != ""
	hasTokenChange := settings.tokenFile != "" || settings.tokenPrompt || settings.noStoredToken
	if !hasName && !hasChat && !hasTokenChange {
		writeError(stderr, command, "invalid_arguments", "no telegram changes requested (use --name, --chat-id, --token-file, --token-prompt, or --no-stored-token)", jsonOutput)
		return 2
	}
	values := application.DestinationValues{}
	if hasName {
		values.Name = strings.TrimSpace(settings.telegramName)
	}
	if hasChat {
		values.ChatID = strings.TrimSpace(settings.chatID)
	}
	request := application.TelegramRequest{Action: "edit", ID: strings.TrimSpace(id), Values: values}
	if hasTokenChange {
		source, ref := intendedTokenSource(settings)
		request.TokenSelection = &application.TelegramTokenSelection{Source: source, Ref: ref}
	}
	updated, err := telegramApplicationFor(settings).Telegram(context.Background(), request, telegramPromptForCLI(settings, stdin, stderr))
	if err != nil {
		return reportApplicationTelegramError(stderr, command, err, jsonOutput)
	}
	writeDestination(stdout, command, updated, fmt.Sprintf("Updated telegram destination %s.\n", updated.ID), jsonOutput)
	return 0
}

func telegramSetEnabled(command string, settings options, enabled bool, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkTelegramIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "telegram id is required (use --telegram or a positional id)", jsonOutput)
		return 2
	}
	if strings.Contains(id, ",") {
		writeError(stderr, command, "invalid_arguments", "telegram id must be a single id, not a list", jsonOutput)
		return 2
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	updated, err := telegramApplicationFor(settings).Telegram(context.Background(), application.TelegramRequest{Action: action, ID: strings.TrimSpace(id)}, nil)
	if err != nil {
		return reportApplicationTelegramError(stderr, command, err, jsonOutput)
	}
	verb := "Enabled"
	if !enabled {
		verb = "Disabled"
	}
	writeDestination(stdout, command, updated, fmt.Sprintf("%s telegram destination %s.\n", verb, updated.ID), jsonOutput)
	return 0
}

func telegramDelete(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkTelegramIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "telegram id is required (use --telegram or a positional id)", jsonOutput)
		return 2
	}
	if strings.Contains(id, ",") {
		writeError(stderr, command, "invalid_arguments", "telegram id must be a single id, not a list", jsonOutput)
		return 2
	}
	id = strings.TrimSpace(id)
	if _, err := telegramApplicationFor(settings).Telegram(context.Background(), application.TelegramRequest{Action: "delete", ID: id}, nil); err != nil {
		return reportApplicationTelegramError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"deleted": id})
		return 0
	}
	fmt.Fprintf(stdout, "Deleted telegram destination %s.\n", id)
	return 0
}

func telegramTest(command string, settings options, stdin *os.File, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkTelegramIDPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "telegram id is required (use --telegram or a positional id)", jsonOutput)
		return 2
	}
	if strings.Contains(id, ",") {
		writeError(stderr, command, "invalid_arguments", "telegram id must be a single id, not a list", jsonOutput)
		return 2
	}
	id = strings.TrimSpace(id)
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	destination, err := storage.GetDestination(id)
	if err != nil {
		return reportTelegramError(stderr, command, err, jsonOutput)
	}
	nonInteractive := isNonInteractive(settings, stdin)
	token, resolveErr := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, stdin, stderr, nonInteractive)
	if resolveErr != nil {
		_, _ = storage.RecordDestinationTest(id, telegram.CodePermanent, shortTelegramMessage(resolveErr), time.Now().UTC())
		return reportSecretError(stderr, command, resolveErr, jsonOutput)
	}
	client := telegram.NewClient(telegram.Config{BaseURL: strings.TrimSpace(settings.telegramBaseURL)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tokenSecret := telegram.Secret(token)
	token = ""
	message := telegram.FormatTestMessage(destination.Name)
	result, sendErr := client.SendMessage(ctx, tokenSecret, destination.ChatID, message)
	if sendErr != nil {
		var telegramErr *telegram.Error
		status := telegram.CodeTemporary
		detail := shortTelegramMessage(sendErr)
		if errors.As(sendErr, &telegramErr) {
			status = telegramErr.Code
			detail = telegramDetailWithRetry(sendErr, telegramErr)
		}
		_, _ = storage.RecordDestinationTest(id, status, detail, time.Now().UTC())
		return reportTelegramSendError(stderr, command, sendErr, jsonOutput)
	}
	if _, err := storage.RecordDestinationTest(id, "delivered", "test delivered", time.Now().UTC()); err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{
			"destination": destination.ID,
			"chat_id":     destination.ChatID,
			"delivered":   true,
			"message_id":  result.MessageID,
		})
		return 0
	}
	fmt.Fprintf(stdout, "Sent test message to telegram destination %s (chat %s).\n", destination.ID, destination.ChatID)
	return 0
}

func countTokenOptions(settings options) int {
	count := 0
	if settings.tokenFile != "" {
		count++
	}
	if settings.tokenPrompt {
		count++
	}
	if settings.noStoredToken {
		count++
	}
	return count
}

// intendedTokenSource returns the token source and reference selected by the
// flags, without prompting, reading files, or saving anything, so callers can
// validate the resulting destination before causing side effects.
func intendedTokenSource(settings options) (string, string) {
	switch {
	case settings.tokenFile != "":
		return store.TokenSourceFile, settings.tokenFile
	case settings.noStoredToken:
		return store.TokenSourcePrompt, ""
	default:
		return store.TokenSourceSecretService, ""
	}
}

func telegramPromptForCLI(settings options, stdin *os.File, stderr io.Writer) func(context.Context, string) (string, error) {
	return func(ctx context.Context, _ string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		value, err := secrets.PromptForPassword("Telegram bot token: ", stdin, stderr, isNonInteractive(settings, stdin))
		if err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			value = ""
			return "", err
		}
		return value, nil
	}
}

func reportApplicationTelegramError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code, detail := application.ErrorInfo(err)
	switch code {
	case "cancelled":
		writeError(stderr, command, code, detail, jsonOutput)
		return 6
	case "timeout":
		writeError(stderr, command, code, detail, jsonOutput)
		return 4
	case "database_error":
		return reportStoreError(stderr, command, err, jsonOutput)
	default:
		writeError(stderr, command, code, detail, jsonOutput)
		return 2
	}
}

func reportTelegramError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code := "database_error"
	switch {
	case errors.Is(err, store.ErrDestinationExists):
		code = "destination_exists"
	case errors.Is(err, store.ErrDestinationNotFound):
		code = "destination_not_found"
	case errors.Is(err, store.ErrProfileNotFound):
		code = "profile_not_found"
	case errors.Is(err, store.ErrAccountNotFound):
		code = "account_not_found"
	case errors.Is(err, store.ErrDestinationInvalid):
		code = "invalid_arguments"
	}
	writeError(stderr, command, code, err.Error(), jsonOutput)
	return 2
}

func reportTelegramSendError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	var telegramErr *telegram.Error
	if errors.As(err, &telegramErr) {
		switch telegramErr.Code {
		case telegram.CodePermanent:
			writeError(stderr, command, "permanent_failure", telegramErr.Message, jsonOutput)
			return 2
		case telegram.CodeCancelled:
			writeError(stderr, command, "cancelled", telegramErr.Message, jsonOutput)
			return 6
		case telegram.CodeRateLimited:
			writeRateLimitError(stderr, command, telegramErr.Message, telegramErr.RetryAfter, jsonOutput)
			return 4
		case telegram.CodeTimeout:
			writeError(stderr, command, "timeout", telegramErr.Message, jsonOutput)
			return 4
		case telegram.CodeTemporary:
			writeError(stderr, command, "temporary_failure", telegramErr.Message, jsonOutput)
			return 4
		case telegram.CodeUnknown:
			// Unknown means the server may or may not have accepted the
			// message, so a retry may duplicate it. Report a distinct code
			// instead of collapsing to temporary_failure.
			writeError(stderr, command, "unknown_delivery", telegramErr.Message, jsonOutput)
			return 4
		}
	}
	if errors.Is(err, secrets.ErrMissingInput) {
		writeError(stderr, command, "missing_input", err.Error(), jsonOutput)
		return 2
	}
	writeError(stderr, command, "temporary_failure", "telegram is temporarily unavailable", jsonOutput)
	return 4
}

func shortTelegramMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 500 {
		return message[:500]
	}
	if message == "" {
		return "telegram send failed"
	}
	return message
}

// telegramDetailWithRetry keeps the server-requested backoff in the persisted
// test detail so history shows when to retry. Without it a 429 leaves no
// backoff for operators or automation.
func telegramDetailWithRetry(sendErr error, telegramErr *telegram.Error) string {
	base := shortTelegramMessage(sendErr)
	if telegramErr == nil || telegramErr.Code != telegram.CodeRateLimited || telegramErr.RetryAfter <= 0 {
		return base
	}
	seconds := int(telegramErr.RetryAfter / time.Second)
	suffixed := fmt.Sprintf("%s (retry after %ds)", base, seconds)
	if len(suffixed) > 500 {
		return suffixed[:500]
	}
	return suffixed
}

func writeDestination(stdout io.Writer, command string, destination store.Destination, textTemplate string, jsonOutput bool) {
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"destination": destination})
		return
	}
	if textTemplate != "" {
		fmt.Fprint(stdout, textTemplate)
	}
	fmt.Fprintf(stdout, "ID: %s\n", destination.ID)
	fmt.Fprintf(stdout, "Name: %s\n", destination.Name)
	fmt.Fprintf(stdout, "Chat: %s\n", destination.ChatID)
	fmt.Fprintf(stdout, "Enabled: %s\n", formatEnabled(destination.Enabled))
	fmt.Fprintf(stdout, "Token source: %s\n", destination.TokenSource)
	if destination.TokenRef != "" {
		fmt.Fprintf(stdout, "Token ref: %s\n", destination.TokenRef)
	}
	if destination.LastTestStatus != "" {
		if destination.LastTestError != "" {
			fmt.Fprintf(stdout, "Last test: %s (%s)\n", destination.LastTestStatus, destination.LastTestError)
		} else {
			fmt.Fprintf(stdout, "Last test: %s\n", destination.LastTestStatus)
		}
	}
}

func formatDestinationLine(destination store.Destination) string {
	return fmt.Sprintf("%s %s chat:%s %s", destination.ID, destination.Name, destination.ChatID, formatEnabled(destination.Enabled))
}
