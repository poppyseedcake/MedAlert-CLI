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
	Action         string
	ID             string
	Values         DestinationValues
	TokenSelection *TelegramTokenSelection
	ProfileIDs     []string
}

// TelegramTokenSelection selects how a destination obtains its bot token.
// Ref is used only for file sources and never contains the token itself.
type TelegramTokenSelection struct {
	Source string
	Ref    string
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
		return a.createTelegramDestination(ctx, storage, id, request.Values, request.TokenSelection, prompt)
	case "edit":
		return editTelegramDestination(ctx, storage, id, request.Values, request.TokenSelection, prompt)
	case "enable", "disable":
		return storage.SetDestinationEnabled(id, request.Action == "enable")
	case "delete":
		return deleteTelegramDestination(storage, id)
	case "link":
		if err := storage.SetDestinationProfilesContext(ctx, id, request.ProfileIDs); err != nil {
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

func (a *Application) createTelegramDestination(ctx context.Context, storage *store.Store, id string, values DestinationValues, selection *TelegramTokenSelection, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	source, ref := telegramTokenReference(values.TokenFile, selection)
	var err error
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
	if err := ctx.Err(); err != nil {
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	if err := validateTelegramTokenReference(source, ref); err != nil {
		return store.Destination{}, err
	}
	token := ""
	if source == store.TokenSourceSecretService {
		token, err = promptTelegramToken(ctx, prompt)
		if err != nil {
			return store.Destination{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		token = ""
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
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
		secretErr := safeTelegramSecretError(err)
		cleanupErr := storage.DeleteDestination(id)
		if cleanupErr != nil {
			return store.Destination{}, errors.Join(secretErr, cleanupErr)
		}
		return store.Destination{}, secretErr
	}
	token = ""
	return created, nil
}

func editTelegramDestination(ctx context.Context, storage *store.Store, id string, values DestinationValues, selection *TelegramTokenSelection, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
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
	tokenChanged := false
	newSource, newRef := "", ""
	if selection != nil {
		tokenChanged = true
		newSource, newRef = telegramTokenReference(values.TokenFile, selection)
	} else {
		tokenFile := strings.TrimSpace(values.TokenFile)
		tokenChanged = tokenFile != "" && (current.TokenSource != store.TokenSourceFile || tokenFile != strings.TrimSpace(current.TokenRef))
		if tokenChanged {
			newSource, newRef = store.TokenSourceFile, tokenFile
		}
	}
	if tokenChanged {
		candidate := current
		if update.Name != nil {
			candidate.Name = *update.Name
		}
		if update.ChatID != nil {
			candidate.ChatID = *update.ChatID
		}
		candidate.TokenSource = newSource
		candidate.TokenRef = newRef
		if err := store.ValidateDestination(candidate); err != nil {
			return store.Destination{}, err
		}
		if err := validateTelegramTokenReference(newSource, newRef); err != nil {
			return store.Destination{}, err
		}
		source, ref := newSource, newRef
		update.TokenSource = &source
		update.TokenRef = &ref
		changed = true
	}
	if !changed {
		return store.Destination{}, &OperationError{Code: "invalid_arguments", Message: "no telegram destination changes requested"}
	}
	if err := ctx.Err(); err != nil {
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	return updateTelegramDestination(ctx, storage, id, current, update, tokenChanged, prompt)
}

// updateTelegramDestination applies a destination update and keeps the
// Secret Service entry consistent when the token source changes. Secret
// values are staged before the database update only when the new source needs
// one; every failure path restores the previous state where possible.
func updateTelegramDestination(ctx context.Context, storage *store.Store, id string, current store.Destination, update store.DestinationUpdate, tokenChanged bool, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	if !tokenChanged {
		return storage.UpdateDestination(id, update)
	}
	if err := ctx.Err(); err != nil {
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}

	newSource := current.TokenSource
	if update.TokenSource != nil {
		newSource = *update.TokenSource
	}
	oldToken, oldKnown := "", false
	if current.TokenSource == store.TokenSourceSecretService {
		value, err := secrets.GetTelegramToken(id)
		if err == nil {
			oldToken, oldKnown = value, true
		} else if !errors.Is(err, secrets.ErrSecretNotFound) {
			return store.Destination{}, safeTelegramSecretError(err)
		}
	}
	defer func() { oldToken = "" }()

	if newSource == store.TokenSourceSecretService {
		newToken, err := promptTelegramToken(ctx, prompt)
		if err != nil {
			return store.Destination{}, err
		}
		if err := secrets.SetTelegramToken(id, newToken); err != nil {
			newToken = ""
			return store.Destination{}, safeTelegramSecretError(err)
		}
		newToken = ""
		if err := ctx.Err(); err != nil {
			return rollbackTelegramDestinationUpdate(id, current.TokenSource, oldToken, oldKnown, err)
		}
		updated, err := storage.UpdateDestination(id, update)
		if err != nil {
			return rollbackTelegramDestinationUpdate(id, current.TokenSource, oldToken, oldKnown, err)
		}
		return updated, nil
	}

	tokenRemoved := false
	if current.TokenSource == store.TokenSourceSecretService && oldKnown {
		if err := secrets.DeleteTelegramToken(id); err != nil {
			return store.Destination{}, safeTelegramSecretError(err)
		}
		tokenRemoved = true
	}
	if err := ctx.Err(); err != nil {
		if tokenRemoved {
			return restoreTelegramDestinationSecret(id, oldToken, err)
		}
		return store.Destination{}, &OperationError{Code: "cancelled", Message: "operation was cancelled", Cause: err}
	}
	updated, err := storage.UpdateDestination(id, update)
	if err != nil {
		if tokenRemoved {
			return restoreTelegramDestinationSecret(id, oldToken, err)
		}
		return store.Destination{}, err
	}
	return updated, nil
}

func rollbackTelegramDestinationUpdate(id, oldSource, oldToken string, oldKnown bool, cause error) (store.Destination, error) {
	var restoreErr error
	if oldSource == store.TokenSourceSecretService && oldKnown {
		restoreErr = secrets.SetTelegramToken(id, oldToken)
	} else {
		restoreErr = secrets.DeleteTelegramToken(id)
	}
	if restoreErr != nil {
		return store.Destination{}, errors.Join(cause, safeTelegramSecretError(restoreErr))
	}
	return store.Destination{}, cause
}

func restoreTelegramDestinationSecret(id, token string, cause error) (store.Destination, error) {
	if err := secrets.SetTelegramToken(id, token); err != nil {
		return store.Destination{}, errors.Join(cause, safeTelegramSecretError(err))
	}
	return store.Destination{}, cause
}

func deleteTelegramDestination(storage *store.Store, id string) (store.Destination, error) {
	current, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	oldToken := ""
	if current.TokenSource == store.TokenSourceSecretService {
		oldToken, err = secrets.GetTelegramToken(id)
		if err != nil {
			if !errors.Is(err, secrets.ErrSecretNotFound) {
				return store.Destination{}, safeTelegramSecretError(err)
			}
			oldToken = ""
		} else if err := secrets.DeleteTelegramToken(id); err != nil {
			oldToken = ""
			return store.Destination{}, safeTelegramSecretError(err)
		}
	}
	if err := storage.DeleteDestination(id); err != nil {
		if oldToken != "" {
			if restoreErr := secrets.SetTelegramToken(id, oldToken); restoreErr != nil {
				oldToken = ""
				return store.Destination{}, errors.Join(err, safeTelegramSecretError(restoreErr))
			}
		}
		oldToken = ""
		return store.Destination{}, err
	}
	oldToken = ""
	return current, nil
}

func setTelegramDestinationToken(ctx context.Context, storage *store.Store, id string, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	current, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	source, ref := store.TokenSourceSecretService, ""
	return updateTelegramDestination(ctx, storage, id, current, store.DestinationUpdate{TokenSource: &source, TokenRef: &ref}, true, prompt)
}

func (a *Application) testTelegramDestination(ctx context.Context, storage *store.Store, id string, prompt func(context.Context, string) (string, error)) (store.Destination, error) {
	destination, err := storage.GetDestination(id)
	if err != nil {
		return store.Destination{}, err
	}
	token, err := resolveTelegramTokenForApplication(ctx, destination, prompt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return store.Destination{}, err
		}
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

func telegramTokenReference(tokenFile string, selection *TelegramTokenSelection) (string, string) {
	if selection != nil {
		source := strings.TrimSpace(selection.Source)
		ref := strings.TrimSpace(selection.Ref)
		if source == store.TokenSourceFile && ref == "" {
			ref = strings.TrimSpace(tokenFile)
		}
		return source, ref
	}
	if tokenFile = strings.TrimSpace(tokenFile); tokenFile != "" {
		return store.TokenSourceFile, tokenFile
	}
	return store.TokenSourceSecretService, ""
}

func validateTelegramTokenReference(source, ref string) error {
	if source != store.TokenSourceFile {
		return nil
	}
	if _, err := secrets.ReadSecretFile(ref); err != nil {
		return safeTelegramSecretError(err)
	}
	return nil
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
