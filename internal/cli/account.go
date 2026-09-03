package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/poppyseedcake/MedAlert/internal/secrets"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

func runAccount(command string, settings options, stdin *os.File, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	switch command {
	case "account list":
		return accountList(command, settings, stdout, stderr, jsonOutput)
	case "account create":
		return accountCreate(command, settings, stdin, stdout, stderr, jsonOutput)
	case "account show":
		return accountShow(command, settings, stdout, stderr, jsonOutput)
	case "account edit":
		return accountEdit(command, settings, stdin, stdout, stderr, jsonOutput)
	case "account delete":
		return accountDelete(command, settings, stdout, stderr, jsonOutput)
	default:
		writeError(stderr, command, "invalid_arguments", "a supported account command is required", jsonOutput)
		return 2
	}
}

func resolveAccountID(settings options) string {
	if settings.accountID != "" {
		return settings.accountID
	}
	if len(settings.positionals) > 0 {
		return settings.positionals[0]
	}
	return ""
}

func checkAccountPositionals(command string, settings options, needsID bool, stderr io.Writer, jsonOutput bool) (string, bool) {
	if settings.accountID != "" && len(settings.positionals) > 0 && settings.positionals[0] != settings.accountID {
		writeError(stderr, command, "invalid_arguments", "use either --account or a positional id, not both", jsonOutput)
		return "", false
	}
	if needsID {
		if len(settings.positionals) > 1 {
			writeError(stderr, command, "invalid_arguments", "too many arguments: expected at most one account id", jsonOutput)
			return "", false
		}
	} else {
		if len(settings.positionals) > 0 {
			writeError(stderr, command, "invalid_arguments", "too many arguments for account list", jsonOutput)
			return "", false
		}
	}
	return resolveAccountID(settings), true
}

func ensureStore(database string) (*store.Store, error) {
	if _, err := store.Initialize(database); err != nil {
		return nil, err
	}
	return store.Open(database)
}

func accountList(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
	if _, ok := checkAccountPositionals(command, settings, false, stderr, jsonOutput); !ok {
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	accounts, err := storage.ListAccounts()
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"accounts": accounts})
		return 0
	}
	if len(accounts) == 0 {
		fmt.Fprintln(stdout, "No accounts found.")
		return 0
	}
	for _, account := range accounts {
		fmt.Fprintln(stdout, formatAccountLine(account))
	}
	return 0
}

func accountCreate(command string, settings options, stdin *os.File, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkAccountPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "account id is required (use --account or a positional id)", jsonOutput)
		return 2
	}
	if strings.TrimSpace(settings.username) == "" {
		writeError(stderr, command, "invalid_arguments", "username is required (use --username)", jsonOutput)
		return 2
	}
	if countPasswordOptions(settings) > 1 {
		writeError(stderr, command, "invalid_arguments", "use only one of --password-file, --password-prompt, --no-stored-password", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	// Check for duplicates before prompting or saving anything: a failed
	// create must not replace the existing account's Secret Service entry.
	if _, err := storage.GetAccount(id); err == nil {
		writeError(stderr, command, "account_exists", fmt.Sprintf("account %q already exists", id), jsonOutput)
		return 2
	} else if !errors.Is(err, store.ErrAccountNotFound) && !errors.Is(err, store.ErrAccountInvalid) {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	source, ref, secretErr := prepareNewSecret(id, settings, stdin, stderr)
	if secretErr != nil {
		return reportSecretError(stderr, command, secretErr, jsonOutput)
	}
	created, err := storage.CreateAccount(store.Account{
		ID:             id,
		Username:       strings.TrimSpace(settings.username),
		PasswordSource: source,
		PasswordRef:    ref,
	})
	if err != nil {
		// Best effort: a failed create after a Secret Service save leaves an
		// orphaned entry that the next create or edit overwrites. The database
		// remains the source of truth and never holds the secret value.
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	writeAccount(stdout, command, created, "Created account %s.\n", jsonOutput)
	return 0
}

func accountShow(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
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
	writeAccount(stdout, command, account, "", jsonOutput)
	return 0
}

func accountEdit(command string, settings options, stdin *os.File, stdout, stderr io.Writer, jsonOutput bool) int {
	id, ok := checkAccountPositionals(command, settings, true, stderr, jsonOutput)
	if !ok {
		return 2
	}
	if strings.TrimSpace(id) == "" {
		writeError(stderr, command, "invalid_arguments", "account id is required (use --account or a positional id)", jsonOutput)
		return 2
	}
	if countPasswordOptions(settings) > 1 {
		writeError(stderr, command, "invalid_arguments", "use only one of --password-file, --password-prompt, --no-stored-password", jsonOutput)
		return 2
	}
	storage, err := ensureStore(settings.database)
	if err != nil {
		return reportStoreError(stderr, command, err, jsonOutput)
	}
	defer storage.Close()
	current, err := storage.GetAccount(id)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	update := store.AccountUpdate{}
	if settings.username != "" {
		trimmed := strings.TrimSpace(settings.username)
		update.Username = &trimmed
	}
	oldSource := current.PasswordSource
	newSource := ""
	newRef := ""
	needsSecretUpdate := false
	if settings.passwordFile != "" || settings.passwordPrompt || settings.noStoredPassword {
		source, ref, secretErr := prepareNewSecret(id, settings, stdin, stderr)
		if secretErr != nil {
			return reportSecretError(stderr, command, secretErr, jsonOutput)
		}
		newSource = source
		newRef = ref
		update.PasswordSource = &newSource
		update.PasswordRef = &newRef
		needsSecretUpdate = true
	}
	updated, err := storage.UpdateAccount(id, update)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	if needsSecretUpdate && oldSource == store.PasswordSourceSecretService && newSource != store.PasswordSourceSecretService {
		// Best effort cleanup; the database update already succeeded and never
		// held a secret value.
		_ = secrets.DeletePassword(id)
	}
	writeAccount(stdout, command, updated, "Updated account %s.\n", jsonOutput)
	return 0
}

func accountDelete(command string, settings options, stdout, stderr io.Writer, jsonOutput bool) int {
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
	current, err := storage.GetAccount(id)
	if err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	if err := storage.DeleteAccount(id); err != nil {
		return reportAccountError(stderr, command, err, jsonOutput)
	}
	if current.PasswordSource == store.PasswordSourceSecretService {
		_ = secrets.DeletePassword(id)
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"deleted": id})
		return 0
	}
	fmt.Fprintf(stdout, "Deleted account %s.\n", id)
	return 0
}

func countPasswordOptions(settings options) int {
	count := 0
	if settings.passwordFile != "" {
		count++
	}
	if settings.passwordPrompt {
		count++
	}
	if settings.noStoredPassword {
		count++
	}
	return count
}

// prepareNewSecret resolves the password source without ever returning the
// secret value to callers. For file sources it validates the file is safe and
// readable. For Secret Service it prompts once and saves. For prompt-only it
// stores nothing.
func prepareNewSecret(accountID string, settings options, stdin *os.File, stderr io.Writer) (string, string, error) {
	switch {
	case settings.passwordFile != "":
		if _, err := secrets.ReadSecretFile(settings.passwordFile); err != nil {
			return "", "", err
		}
		return store.PasswordSourceFile, settings.passwordFile, nil
	case settings.passwordPrompt:
		value, err := secrets.PromptForPassword("Password: ", stdin, stderr, settings.nonInteractive)
		if err != nil {
			return "", "", err
		}
		if err := secrets.SetPassword(accountID, value); err != nil {
			return "", "", err
		}
		return store.PasswordSourceSecretService, "", nil
	case settings.noStoredPassword:
		return store.PasswordSourcePrompt, "", nil
	default:
		// No explicit password flag: interactive commands prompt once and keep
		// the value in Secret Service; non-interactive commands fail instead.
		value, err := secrets.PromptForPassword("Password: ", stdin, stderr, settings.nonInteractive)
		if err != nil {
			return "", "", err
		}
		if err := secrets.SetPassword(accountID, value); err != nil {
			return "", "", err
		}
		return store.PasswordSourceSecretService, "", nil
	}
}

func reportSecretError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	if errors.Is(err, secrets.ErrMissingInput) {
		writeError(stderr, command, "missing_input", err.Error(), jsonOutput)
		return 2
	}
	writeError(stderr, command, "secret_error", err.Error(), jsonOutput)
	return 2
}

func writeAccount(stdout io.Writer, command string, account store.Account, textTemplate string, jsonOutput bool) {
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"account": account})
		return
	}
	if textTemplate != "" {
		fmt.Fprintf(stdout, textTemplate, account.ID)
	}
	fmt.Fprintf(stdout, "ID: %s\n", account.ID)
	fmt.Fprintf(stdout, "Username: %s\n", account.Username)
	fmt.Fprintf(stdout, "Password source: %s\n", account.PasswordSource)
	if account.PasswordRef != "" {
		fmt.Fprintf(stdout, "Password ref: %s\n", account.PasswordRef)
	}
}

func formatAccountLine(account store.Account) string {
	if account.PasswordRef != "" {
		return fmt.Sprintf("%s %s %s:%s", account.ID, account.Username, account.PasswordSource, account.PasswordRef)
	}
	return fmt.Sprintf("%s %s %s", account.ID, account.Username, account.PasswordSource)
}
