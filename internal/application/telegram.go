package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
	"github.com/poppyseedcake/MedAlert/internal/telegram"
)

// TelegramRequest describes one Telegram Destination action from an adapter.
// Values contain configuration and secret references only; a bot token is
// requested through Prompt when the action needs one.
type TelegramRequest struct {
	Action     string
	ID         string
	Values     DestinationValues
	ProfileIDs []string
}

// Telegram applies Telegram Destination configuration, linking, testing,
// and deletion through the application boundary. The application owns token
// resolution and Telegram protocol calls so the TUI remains a presentation
// adapter.
func (a *Application) Telegram(ctx context.Context, request TelegramRequest, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	id := strings.TrimSpace(request.ID)
	if id == "" {
		return store.Destination{}, &OperationError{Code: "invalid_arguments", Message: "telegram destination id is required"}
	}
	storage, err := openStore(a.config.Database)
	if err != nil {
		return store.Destination{}, &OperationError{Code: "database_error", Message: err.Error(), Cause: err}
	}
	defer storage.Close()

	switch strings.TrimSpace(request.Action) {
	case "create":
		return a.createTelegramDestination(ctx, storage, id, request.Values, prompt)
	case "edit":
		return editTelegramDestination(storage, id, request.Values)
	case "enable", "disable":
		return storage.SetDestinationEnabled(id, request.Action == "enable")
	case "delete":
		return deleteTelegramDestination(storage, id)
	case "link":
		if err := storage.SetDestinationProfiles(id, request.ProfileIDs); err != nil {
			return store.Destination{}, err
		}
		return storage.GetDestination(id)
	case "token":
		return setTelegramDestinationToken(ctx, storage, id, prompt)
	case "test":
		return a.testTelegramDestination(ctx, storage, id, prompt)
	default:
		return store.Destination{}, &OperationError{Code: "invalid_arguments", Message: "unsupported telegram destination action"}
	}
}

func (a *Application) createTelegramDestination(ctx context.Context, storage *store.Store, id string, values DestinationValues, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	source, ref, err := telegramTokenReference(values.TokenFile)
	if err != nil {
		return store.Destination{}, err
	}
	destination := store.Destination{
		ID: id, Name: strings.TrimSpace(values.Name), ChatID: strings.TrimSpace(values.ChatID),
		TokenSource: source, TokenRef: ref, Enabled: true,
	}
	if err := store.ValidateDestination(destination); err != nil {
		return store.Destination{}, err
	}
	if _, err := storage.GetDestination(id); err == nil {
		return store.Destination{}, fmt.Errorf("%w: %s", store.ErrDestinationExists, id)
	} else if !errors.Is(err, store.ErrDestinationNotFound) {
		return store.Destination{}, err
	}
	token := ""
	if source == store.TokenSourceSecretService {
		token, err = promptTelegramToken(ctx, prompt)
		if err != nil {
			return store.Destination{}, err
		}
	}
	created, err := storage.CreateDestination(destination)
	if err != nil {
		token = ""
		return store.Destination{}, err
	}
	if source != store.TokenSourceSecretService {
		return created, nil
	}
	if err := secrets.SetTelegramToken(id, token); err != nil {
		token = ""
		_ = storage.DeleteDestination(id)
		return store.Destination{}, safeTelegramSecretError(err)
	}
	token = ""
	return created, nil
}

func editTelegramDestination(storage *store.Store, id string, values DestinationValues) (store.Destination, error) {
	current, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	update := store.DestinationUpdate{}
	changed := false
	if name := strings.TrimSpace(values.Name); name != "" && name != current.Name {
		update.Name = &name
		changed = true
	}
	if chatID := strings.TrimSpace(values.ChatID); chatID != "" && chatID != current.ChatID {
		update.ChatID = &chatID
		changed = true
	}
	if tokenFile := strings.TrimSpace(values.TokenFile); tokenFile != "" {
		if _, err := secrets.ReadSecretFile(tokenFile); err != nil {
			return store.Destination{}, safeTelegramSecretError(err)
		}
		source, ref := store.TokenSourceFile, tokenFile
		update.TokenSource = &source
		update.TokenRef = &ref
		changed = true
	}
	if !changed {
		return store.Destination{}, &OperationError{Code: "invalid_arguments", Message: "no telegram destination changes requested"}
	}
	updated, err := storage.UpdateDestination(id, update)
	if err != nil {
		return store.Destination{}, err
	}
	if values.TokenFile != "" && current.TokenSource == store.TokenSourceSecretService {
		_ = secrets.DeleteTelegramToken(id)
	}
	return updated, nil
}

func deleteTelegramDestination(storage *store.Store, id string) (store.Destination, error) {
	current, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	if err := storage.DeleteDestination(id); err != nil {
		return store.Destination{}, err
	}
	if current.TokenSource == store.TokenSourceSecretService {
		_ = secrets.DeleteTelegramToken(id)
	}
	return current, nil
}

func setTelegramDestinationToken(ctx context.Context, storage *store.Store, id string, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	current, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	token, err := promptTelegramToken(ctx, prompt)
	if err != nil {
		return store.Destination{}, err
	}
	oldToken, oldKnown := "", false
	if current.TokenSource == store.TokenSourceSecretService {
		if value, getErr := secrets.GetTelegramToken(id); getErr == nil {
			oldToken, oldKnown = value, true
		} else if !errors.Is(getErr, secrets.ErrSecretNotFound) {
			token = ""
			return store.Destination{}, safeTelegramSecretError(getErr)
		}
	}
	if err := secrets.SetTelegramToken(id, token); err != nil {
		token = ""
		oldToken = ""
		return store.Destination{}, safeTelegramSecretError(err)
	}
	token = ""
	source, ref := store.TokenSourceSecretService, ""
	updated, err := storage.UpdateDestination(id, store.DestinationUpdate{TokenSource: &source, TokenRef: &ref})
	if err != nil {
		if oldKnown {
			_ = secrets.SetTelegramToken(id, oldToken)
		} else {
			_ = secrets.DeleteTelegramToken(id)
		}
		oldToken = ""
		return store.Destination{}, err
	}
	oldToken = ""
	return updated, nil
}

func (a *Application) testTelegramDestination(ctx context.Context, storage *store.Store, id string, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	destination, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	token, err := resolveTelegramTokenForApplication(ctx, destination, prompt)
	if err != nil {
		if _, recordErr := storage.RecordDestinationTest(id, store.DeliveryPermanentFailure, "Telegram token is unavailable", time.Now().UTC()); recordErr != nil {
			return store.Destination{}, recordErr
		}
		return store.Destination{}, err
	}
	sendContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, sendErr := telegram.NewClient(telegram.Config{BaseURL: strings.TrimSpace(a.config.TelegramBaseURL)}).SendMessage(
		sendContext, token, destination.ChatID, telegram.FormatTestMessage(destination.Name),
	)
	token = ""
	if sendErr != nil {
		status, detail := telegramTestResult(sendErr)
		if _, recordErr := storage.RecordDestinationTest(id, status, detail, time.Now().UTC()); recordErr != nil {
			return store.Destination{}, recordErr
		}
		return store.Destination{}, sendErr
	}
	updated, err := storage.RecordDestinationTest(id, store.DeliveryDelivered, "test delivered", time.Now().UTC())
	if err != nil {
		return store.Destination{}, err
	}
	return updated, nil
}

func telegramTokenReference(tokenFile string) (string, string, error) {
	if tokenFile = strings.TrimSpace(tokenFile); tokenFile != "" {
		if _, err := secrets.ReadSecretFile(tokenFile); err != nil {
			return "", "", safeTelegramSecretError(err)
		}
		return store.TokenSourceFile, tokenFile, nil
	}
	return store.TokenSourceSecretService, "", nil
}

func promptTelegramToken(ctx context.Context, prompt func(context.Context, string) (string, error)) (string, error) {
	if prompt == nil {
		return "", &OperationError{Code: "missing_input", Message: "telegram bot token input is required", Cause: secrets.ErrMissingInput}
	}
	if err := ctx.Err(); err != nil {
		return "", &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	token, err := prompt(ctx, "telegram-token")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: ctxErr}
		}
		return "", safeTelegramSecretError(err)
	}
	if err := ctx.Err(); err != nil {
		token = ""
		return "", &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	if strings.TrimSpace(token) == "" {
		return "", &OperationError{Code: "missing_input", Message: "telegram bot token input is required", Cause: secrets.ErrMissingInput}
	}
	return token, nil
}

func resolveTelegramTokenForApplication(ctx context.Context, destination store.Destination, prompt func(context.Context, string) (string, error)) (telegram.Secret, error) {
	if destination.TokenSource == store.TokenSourcePrompt {
		token, err := promptTelegramToken(ctx, prompt)
		if err != nil {
			return "", err
		}
		secret := telegram.Secret(token)
		token = ""
		return secret, nil
	}
	token, err := secrets.ResolveTelegramToken(destination.TokenSource, destination.TokenRef, destination.ID, nil, io.Discard, true)
	if err != nil {
		return "", safeTelegramSecretError(err)
	}
	secret := telegram.Secret(token)
	token = ""
	return secret, nil
}

func telegramTestResult(err error) (string, string) {
	var telegramErr *telegram.Error
	if errors.As(err, &telegramErr) {
		detail := telegramErr.Message
		if telegramErr.RetryAfter > 0 {
			detail += fmt.Sprintf("; retry after %ds", int(telegramErr.RetryAfter/time.Second))
		}
		return telegramErr.Code, truncateTelegramDetail(detail)
	}
	return telegram.CodeTemporary, "telegram test failed"
}

func truncateTelegramDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	if value == "" {
		return "telegram test failed"
	}
	return value
}

func safeTelegramSecretError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &OperationError{Code: "timeout", Message: "telegram token input timed out", Cause: err}
	}
	if errors.Is(err, secrets.ErrMissingInput) {
		return &OperationError{Code: "missing_input", Message: "telegram bot token input is required", Cause: err}
	}
	return &OperationError{Code: "secret_error", Message: "cannot access the Telegram bot token", Cause: err}
}
