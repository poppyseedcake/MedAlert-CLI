package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/medicover"
	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/session"
	"golang.org/x/term"
)

func runAuth(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	switch command {
	case "account login", "account authenticate":
		return accountLogin(command, settings, stdin, stdout, stderr, jsonOutput)
	case "account logout":
		return accountLogout(command, settings, stdout, stderr, jsonOutput)
	case "account status":
		return accountStatus(command, settings, stdout, stderr, jsonOutput)
	default:
		writeError(stderr, command, "invalid_arguments", "a supported account command is required", jsonOutput)
		return 2
	}
}

func sessionStoreFor(settings options) session.Store {
	if strings.TrimSpace(settings.sessionDir) != "" {
		return session.FileStore{Dir: settings.sessionDir}
	}
	return session.SecretServiceStore{}
}

func medicoverClientFor(settings options) *medicover.Client {
	cfg := medicover.Config{}
	if baseURL := strings.TrimSpace(settings.medicoverBaseURL); baseURL != "" {
		cfg.Issuer = baseURL
		// Test and Docker overrides point the OIDC issuer at a local server.
		// The registered callback must live on the same server, otherwise
		// exact callback matching rejects the fake code redirect.
		cfg.RedirectURI = strings.TrimSuffix(baseURL, "/") + "/signin-oidc"
	}
	return medicover.NewClient(cfg)
}

func isNonInteractive(settings options, stdin *os.File) bool {
	if settings.nonInteractive {
		return true
	}
	if stdin == nil {
		return true
	}
	return !term.IsTerminal(int(stdin.Fd()))
}

func accountLogin(command string, settings options, stdin *os.File, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkAccountPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "account id is required (use --account or a positional id)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	account, err := storage.GetAccount(id)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}

	storeBackend := sessionStoreFor(settings)
	var saved *medicover.SessionState
	if loaded, loadErr := storeBackend.Load(id); loadErr == nil {
		saved = loaded
	} else if !errors.Is(loadErr, session.ErrNotFound) && !isSessionCorrupt(loadErr) {
		// Secret Service unavailability is a temporary failure, not a
		// missing session. Corrupt or missing sessions fall through to a
		// full login and become Authentication Required on failure.
		if isSessionBackendUnavailable(loadErr) {
			writeError(stderr, command, "temporary_failure", "session storage is temporarily unavailable", jsonOutput)
			return 4
		}
	}

	client := medicoverClientFor(settings)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mfaCode := ""
	if strings.TrimSpace(settings.mfaCodeFile) != "" {
		value, readErr := secrets.ReadSecretFile(settings.mfaCodeFile)
		if readErr != nil {
			return reportSecretError(stderr, command, readErr, jsonOutput)
		}
		mfaCode = strings.TrimSpace(value)
	}

	// Try trusted-session reuse before touching the password. A prompt-based
	// account or an unavailable Secret Service must not block reuse when the
	// saved session is still valid.
	if saved != nil {
		if reused, reuseErr := client.Authenticate(ctx, medicover.AuthRequest{Session: saved}); reuseErr == nil {
			if saveErr := storeBackend.Save(id, reused.Session); saveErr != nil {
				if errors.Is(saveErr, session.ErrUnsafe) {
					writeError(stderr, command, "invalid_arguments", saveErr.Error(), jsonOutput)
					return 2
				}
				writeError(stderr, command, "temporary_failure", "cannot save session state", jsonOutput)
				return 4
			}
			mfaCode = ""
			if jsonOutput {
				writeResult(stdout, command, map[string]any{
					"account":    id,
					"reused":     true,
					"mfa_used":   reused.MFAUsed,
					"expires_at": reused.ExpiresAt.UTC().Format(time.RFC3339),
				})
				return 0
			}
			fmt.Fprintf(stdout, "Reused trusted session for account %s.\n", id)
			return 0
		} else {
			var medicoverErr *medicover.Error
			if errors.As(reuseErr, &medicoverErr) {
				switch medicoverErr.Code {
				case medicover.CodeAuthRequired, medicover.CodeInvalidCredentials, medicover.CodeMFARequired:
					// Fall through to a full password login.
				default:
					return reportMedicoverError(stderr, command, reuseErr, jsonOutput)
				}
			} else if !errors.Is(reuseErr, secrets.ErrMissingInput) {
				return reportMedicoverError(stderr, command, reuseErr, jsonOutput)
			}
		}
	}

	// Resolve the password through the account's configured secret source.
	// check/watch never prompt; only login may prompt when interactive.
	nonInteractive := isNonInteractive(settings, stdin)
	password, err := secrets.Resolve(account.PasswordSource, account.PasswordRef, account.ID, stdin, stderr, nonInteractive)
	if err != nil {
		return reportSecretError(stderr, command, err, jsonOutput)
	}
	// Drop the raw password reference as soon as the request is built; the
	// value never reaches logs, errors, JSON, or SQLite.
	passwordSecret := medicover.Secret(password)
	password = ""

	result, err := client.Authenticate(ctx, medicover.AuthRequest{
		Username: medicover.Secret(account.Username),
		Password: passwordSecret,
		MFACode:  medicover.Secret(mfaCode),
		Session:  saved,
	})
	// Clear MFA material immediately.
	mfaCode = ""
	if err != nil {
		return reportMedicoverError(stderr, command, err, jsonOutput)
	}
	if err := storeBackend.Save(id, result.Session); err != nil {
		// A failed save after a successful exchange keeps the in-memory
		// token out of durable state. Report a configuration error without
		// exposing any secret.
		if errors.Is(err, session.ErrUnsafe) {
			writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
			return 2
		}
		writeError(stderr, command, "temporary_failure", "cannot save session state", jsonOutput)
		return 4
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{
			"account":    id,
			"reused":     result.Reused,
			"mfa_used":   result.MFAUsed,
			"expires_at": result.ExpiresAt.UTC().Format(time.RFC3339),
		})
		return 0
	}
	if result.Reused {
		fmt.Fprintf(stdout, "Reused trusted session for account %s.\n", id)
	} else if result.MFAUsed {
		fmt.Fprintf(stdout, "Logged in account %s with multi-factor authentication.\n", id)
	} else {
		fmt.Fprintf(stdout, "Logged in account %s.\n", id)
	}
	return 0
}

func accountLogout(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkAccountPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "account id is required (use --account or a positional id)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	account, err := storage.GetAccount(id)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	_ = account
	storeBackend := sessionStoreFor(settings)
	// Logging out removes only this account's session state. Other accounts
	// are untouched because every backend keys state by account id.
	if err := storeBackend.Delete(id); err != nil {
		if errors.Is(err, session.ErrUnsafe) {
			writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
			return 2
		}
		writeError(stderr, command, "temporary_failure", "cannot remove session state", jsonOutput)
		return 4
	}
	if settings.forgetSecret {
		// DeletePassword already ignores missing entries; any other error
		// means the password may still exist and must be reported.
		if err := secrets.DeletePassword(id); err != nil {
			writeError(stderr, command, "temporary_failure", "cannot remove saved password", jsonOutput)
			return 4
		}
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"logged_out": id})
		return 0
	}
	fmt.Fprintf(stdout, "Logged out account %s.\n", id)
	return 0
}

func accountStatus(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkAccountPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "account id is required (use --account or a positional id)", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	if _, err := storage.GetAccount(id); err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	storeBackend := sessionStoreFor(settings)
	state, err := storeBackend.Load(id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) || isSessionCorrupt(err) {
			if jsonOutput {
				writeResult(stdout, command, map[string]any{"account": id, "authenticated": false})
				return 3
			}
			fmt.Fprintf(stdout, "Account %s requires authentication.\n", id)
			return 3
		}
		if errors.Is(err, session.ErrUnsafe) {
			writeError(stderr, command, "invalid_arguments", err.Error(), jsonOutput)
			return 2
		}
		writeError(stderr, command, "temporary_failure", "session storage is temporarily unavailable", jsonOutput)
		return 4
	}
	_ = state
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"account": id, "authenticated": true})
		return 0
	}
	fmt.Fprintf(stdout, "Account %s is authenticated.\n", id)
	return 0
}

func isSessionCorrupt(err error) bool {
	return errors.Is(err, session.ErrCorrupt)
}

func isSessionBackendUnavailable(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "secret service is unavailable")
}

func reportMedicoverError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	var medicoverErr *medicover.Error
	if errors.As(err, &medicoverErr) {
		switch medicoverErr.Code {
		case medicover.CodeAuthRequired, medicover.CodeMFARequired, medicover.CodeInvalidCredentials:
			// Invalid credentials and missing MFA are authentication
			// failures for automation: exit 3, never expose values.
			code := "authentication_required"
			if medicoverErr.Code == medicover.CodeMFARequired {
				code = "mfa_required"
			} else if medicoverErr.Code == medicover.CodeInvalidCredentials {
				code = "invalid_credentials"
			}
			writeError(stderr, command, code, medicoverErr.Message, jsonOutput)
			return 3
		case medicover.CodeRateLimited, medicover.CodeTemporary:
			code := "temporary_failure"
			if medicoverErr.Code == medicover.CodeRateLimited {
				code = "rate_limited"
			}
			writeError(stderr, command, code, medicoverErr.Message, jsonOutput)
			return 4
		case medicover.CodeProtocolChanged:
			writeError(stderr, command, "protocol_changed", medicoverErr.Message, jsonOutput)
			return 5
		}
	}
	if errors.Is(err, secrets.ErrMissingInput) {
		writeError(stderr, command, "missing_input", err.Error(), jsonOutput)
		return 2
	}
	writeError(stderr, command, "temporary_failure", "authentication is temporarily unavailable", jsonOutput)
	return 4
}
