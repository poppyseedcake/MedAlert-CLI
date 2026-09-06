package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poppyseedcake/MedAlert/internal/buildinfo"
	"github.com/poppyseedcake/MedAlert/internal/store"
)

const resultSchemaVersion = 1

type options struct {
	command          []string
	positionals      []string
	database         string
	output           string
	nonInteractive   bool
	versionRequested bool
	accountID        string
	username         string
	passwordFile     string
	passwordPrompt   bool
	noStoredPassword bool
	mfaCodeFile      string
	medicoverBaseURL string
	sessionDir       string
	forgetSecret     bool
	// Observation Profile criteria. checkIntervalRaw stays a string so the
	// CLI can tell "flag missing" apart from "flag is 0" (invalid).
	profileID        string
	region           string
	specialty        string
	clinic           string
	doctor           string
	language         string
	visitType        string
	searchType       string
	startDate        string
	endDate          string
	checkIntervalRaw string
	profileDisabled  bool
	clearClinic      bool
	clearDoctor      bool
	clearLanguage    bool
	clearVisitType   bool
	clearSearchType  bool
	clearStartDate   bool
	clearEndDate     bool
	dry              bool
	// Watch controls. Raw strings keep "flag missing" apart from invalid
	// values; --once is shorthand for exactly one iteration.
	watchOnce        bool
	maxIterationsRaw string
	pollIntervalRaw  string
	// Telegram Destination fields. telegramIDsRaw stays a string so the CLI
	// can tell "flag missing" apart from "flag empty" (clear).
	telegramIDsRaw  string
	telegramName    string
	chatID          string
	tokenFile       string
	tokenPrompt     bool
	noStoredToken   bool
	telegramBaseURL string
	clearTelegram   bool
	// History inspection. historyLimitRaw caps list output; historyStatus
	// filters by record status; historyScope filters incidents by scope;
	// historyIncidentID selects incident deliveries; retentionDaysRaw sets
	// the saved retention policy for history retention.
	historyLimitRaw   string
	historyStatus     string
	historyScope      string
	historyIncidentID string
	retentionDaysRaw  string
}

type errorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter *int   `json:"retry_after,omitempty"`
}

func Run(arguments []string, stdout, stderr io.Writer, getenv func(string) string) int {
	return RunWithIO(arguments, os.Stdin, stdout, stderr, getenv)
}

func RunWithIO(arguments []string, stdin *os.File, stdout, stderr io.Writer, getenv func(string) string) int {
	settings, err := parse(arguments, getenv)
	if err != nil {
		writeError(stderr, "command", "invalid_arguments", err.Error(), wantsJSON(arguments, getenv))
		return 2
	}
	// --version takes precedence over any command: print the version and exit
	// instead of treating the flag as a positional account id or similar.
	if settings.versionRequested {
		commit := buildinfo.SourceCommit()
		if settings.output == "json" {
			writeResult(stdout, "version", map[string]any{"commit": commit})
		} else {
			fmt.Fprintf(stdout, "medalert %s\n", commit)
		}
		return 0
	}
	commandName := strings.Join(settings.command, " ")
	if err := checkCommandFlags(commandName, settings); err != nil {
		writeError(stderr, commandName, "invalid_arguments", err.Error(), settings.output == "json")
		return 2
	}
	if commandName == "" && !settings.nonInteractive && settings.output == "text" && terminalPair(stdin, stdout) {
		return runTUI(settings, stdin, stdout, stderr)
	}
	switch commandName {
	case "version":
		if len(settings.positionals) > 0 {
			writeError(stderr, commandName, "invalid_arguments", "too many arguments for version", settings.output == "json")
			return 2
		}
		commit := buildinfo.SourceCommit()
		if settings.output == "json" {
			writeResult(stdout, "version", map[string]any{"commit": commit})
		} else {
			fmt.Fprintf(stdout, "medalert %s\n", commit)
		}
		return 0
	case "doctor":
		if settings.sessionDir == "" {
			settings.sessionDir = strings.TrimSpace(getenv("MEDALERT_SESSION_DIR"))
		}
		return runDoctor(commandName, settings, stdout, stderr)
	case "database initialize":
		if len(settings.positionals) > 0 {
			writeError(stderr, commandName, "invalid_arguments", "too many arguments for database initialize", settings.output == "json")
			return 2
		}
		status, initializeErr := store.Initialize(settings.database)
		if initializeErr != nil {
			return reportStoreError(stderr, commandName, initializeErr, settings.output == "json")
		}
		if settings.output == "json" {
			writeResult(stdout, commandName, status)
		} else {
			fmt.Fprintf(stdout, "Initialized database schema %d.\n", status.SchemaVersion)
		}
		return 0
	case "account create", "account list", "account show", "account edit", "account delete":
		return runAccount(commandName, settings, stdin, stdout, stderr)
	case "profile create", "profile list", "profile show", "profile edit", "profile enable", "profile disable", "profile delete":
		return runProfile(commandName, settings, stdout, stderr)
	case "telegram create", "telegram list", "telegram show", "telegram edit", "telegram enable", "telegram disable", "telegram test", "telegram delete":
		if settings.telegramBaseURL == "" {
			settings.telegramBaseURL = strings.TrimSpace(getenv("MEDALERT_TELEGRAM_BASE_URL"))
		}
		return runTelegram(commandName, settings, stdin, stdout, stderr)
	case "account login", "account authenticate", "account logout", "account status":
		// Resolve session directory defaults from the environment before
		// selecting the session backend.
		if settings.sessionDir == "" {
			settings.sessionDir = strings.TrimSpace(getenv("MEDALERT_SESSION_DIR"))
		}
		if settings.medicoverBaseURL == "" {
			settings.medicoverBaseURL = strings.TrimSpace(getenv("MEDALERT_MEDICOVER_BASE_URL"))
		}
		return runAuth(commandName, settings, stdin, stdout, stderr)
	case "check":
		if settings.sessionDir == "" {
			settings.sessionDir = strings.TrimSpace(getenv("MEDALERT_SESSION_DIR"))
		}
		if settings.medicoverBaseURL == "" {
			settings.medicoverBaseURL = strings.TrimSpace(getenv("MEDALERT_MEDICOVER_BASE_URL"))
		}
		return runCheck(commandName, settings, stdin, stdout, stderr)
	case "watch":
		if settings.sessionDir == "" {
			settings.sessionDir = strings.TrimSpace(getenv("MEDALERT_SESSION_DIR"))
		}
		if settings.medicoverBaseURL == "" {
			settings.medicoverBaseURL = strings.TrimSpace(getenv("MEDALERT_MEDICOVER_BASE_URL"))
		}
		return runWatch(commandName, settings, stdin, stdout, stderr)
	case "history runs", "history episodes", "history incidents", "history deliveries", "history incident-deliveries", "history status", "history retention", "history prune", "history":
		if settings.sessionDir == "" {
			settings.sessionDir = strings.TrimSpace(getenv("MEDALERT_SESSION_DIR"))
		}
		return runHistory(commandName, settings, stdin, stdout, stderr)
	case "completion":
		return runCompletion(commandName, settings, stdout, stderr)
	default:
		writeError(stderr, commandName, "invalid_arguments", "a supported command is required", settings.output == "json")
		return 2
	}
}

func parse(arguments []string, getenv func(string) string) (options, error) {
	settings := options{database: defaultDatabasePath(getenv), output: "text"}
	if environmentOutput := getenv("MEDALERT_OUTPUT"); environmentOutput != "" {
		settings.output = environmentOutput
	}
	if environmentDatabase := getenv("MEDALERT_DATABASE"); environmentDatabase != "" {
		settings.database = environmentDatabase
	}
	if isEnvTrue(getenv("MEDALERT_NON_INTERACTIVE")) {
		settings.nonInteractive = true
	}
	if sessionDir := strings.TrimSpace(getenv("MEDALERT_SESSION_DIR")); sessionDir != "" {
		settings.sessionDir = sessionDir
	}
	if baseURL := strings.TrimSpace(getenv("MEDALERT_MEDICOVER_BASE_URL")); baseURL != "" {
		settings.medicoverBaseURL = baseURL
	}
	if telegramBaseURL := strings.TrimSpace(getenv("MEDALERT_TELEGRAM_BASE_URL")); telegramBaseURL != "" {
		settings.telegramBaseURL = telegramBaseURL
	}
	var raw []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--version":
			settings.versionRequested = true
		case "--output", "--database", "--account", "--username", "--user", "--password-file", "--mfa-code-file", "--medicover-base-url", "--session-dir",
			"--profile", "--region", "--specialty", "--specialties", "--clinic", "--clinics", "--doctor", "--doctors",
			"--language", "--languages", "--doctor-language", "--doctor-languages", "--doctor-language-ids",
			"--visit-type", "--visit_type", "--search-type", "--search_type", "--slot-search-type",
			"--start-date", "--date", "--from-date", "--end-date", "--enddate", "--to-date",
			"--check-interval-minutes", "--check-interval", "--interval-minutes", "--interval",
			"--max-iterations", "--max_iterations", "--poll-interval", "--poll_interval",
			"--telegram", "--destination", "--destinations", "--telegram-destination", "--telegram-destinations",
			"--name", "--chat-id", "--chat_id", "--chat", "--token-file", "--token_file", "--telegram-base-url",
			"--limit", "--history-limit", "--status", "--history-status", "--scope", "--history-scope",
			"--incident", "--history-incident", "--incident-id", "--retention-days", "--retention_days":
			if index+1 >= len(arguments) {
				return settings, fmt.Errorf("%s needs a value", argument)
			}
			index++
			value := arguments[index]
			switch argument {
			case "--output":
				settings.output = value
			case "--database":
				settings.database = value
			case "--account":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--account needs a non-empty value")
				}
				settings.accountID = value
			case "--username", "--user":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.username = value
			case "--password-file":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--password-file needs a non-empty value")
				}
				settings.passwordFile = value
			case "--mfa-code-file":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--mfa-code-file needs a non-empty value")
				}
				settings.mfaCodeFile = value
			case "--medicover-base-url":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--medicover-base-url needs a non-empty value")
				}
				settings.medicoverBaseURL = value
			case "--session-dir":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--session-dir needs a non-empty value")
				}
				settings.sessionDir = value
			case "--profile":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--profile needs a non-empty value")
				}
				settings.profileID = value
			case "--region", "--specialty", "--specialties", "--clinic", "--clinics", "--doctor", "--doctors",
				"--language", "--languages", "--doctor-language", "--doctor-languages", "--doctor-language-ids":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				switch argument {
				case "--region":
					settings.region = value
				case "--specialty", "--specialties":
					settings.specialty = value
				case "--clinic", "--clinics":
					settings.clinic = value
				case "--doctor", "--doctors":
					settings.doctor = value
				case "--language", "--languages", "--doctor-language", "--doctor-languages", "--doctor-language-ids":
					settings.language = value
				}
			case "--visit-type", "--visit_type":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.visitType = value
			case "--search-type", "--search_type", "--slot-search-type":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.searchType = value
			case "--start-date", "--date", "--from-date":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.startDate = value
			case "--end-date", "--enddate", "--to-date":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.endDate = value
			case "--check-interval-minutes", "--check-interval", "--interval-minutes", "--interval":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.checkIntervalRaw = value
			case "--max-iterations", "--max_iterations":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.maxIterationsRaw = value
			case "--poll-interval", "--poll_interval":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.pollIntervalRaw = value
			case "--telegram", "--destination", "--destinations", "--telegram-destination", "--telegram-destinations":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.telegramIDsRaw = value
			case "--name":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--name needs a non-empty value")
				}
				settings.telegramName = value
			case "--chat-id", "--chat_id", "--chat":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.chatID = value
			case "--token-file", "--token_file":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.tokenFile = value
			case "--telegram-base-url":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("--telegram-base-url needs a non-empty value")
				}
				settings.telegramBaseURL = value
			case "--limit", "--history-limit":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.historyLimitRaw = value
			case "--status", "--history-status":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.historyStatus = value
			case "--scope", "--history-scope":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.historyScope = value
			case "--incident", "--history-incident", "--incident-id":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.historyIncidentID = value
			case "--retention-days", "--retention_days":
				if strings.TrimSpace(value) == "" {
					return settings, fmt.Errorf("%s needs a non-empty value", argument)
				}
				settings.retentionDaysRaw = value
			}
		case "--password-prompt":
			settings.passwordPrompt = true
		case "--token-prompt":
			settings.tokenPrompt = true
		case "--no-stored-token":
			settings.noStoredToken = true
		case "--clear-telegram", "--clear-telegram-destination", "--clear-telegram-destinations", "--clear-destination", "--clear-destinations":
			settings.clearTelegram = true
		case "--disabled":
			settings.profileDisabled = true
		case "--clear-clinic":
			settings.clearClinic = true
		case "--clear-doctor":
			settings.clearDoctor = true
		case "--clear-language":
			settings.clearLanguage = true
		case "--clear-visit-type":
			settings.clearVisitType = true
		case "--clear-search-type":
			settings.clearSearchType = true
		case "--clear-start-date":
			settings.clearStartDate = true
		case "--clear-end-date":
			settings.clearEndDate = true
		case "--no-stored-password":
			settings.noStoredPassword = true
		case "--forget-secret":
			settings.forgetSecret = true
		case "--non-interactive":
			settings.nonInteractive = true
		case "--dry":
			settings.dry = true
		case "--once":
			settings.watchOnce = true
		default:
			if strings.HasPrefix(argument, "-") {
				return settings, fmt.Errorf("unknown flag: %s", argument)
			}
			raw = append(raw, argument)
		}
	}
	settings.command, settings.positionals = splitCommand(raw)
	if settings.output != "text" && settings.output != "json" {
		return settings, fmt.Errorf("output must be text or json")
	}
	// Completion is offline and static: it works without a database so shell
	// setup never needs state directories.
	if len(settings.command) == 1 && settings.command[0] == "completion" {
		return settings, nil
	}
	if settings.database == "" {
		return settings, errors.New("database path is empty")
	}
	return settings, nil
}

// checkCommandFlags rejects account-specific flags for commands that do not
// consume them, so typos and copy-paste errors fail instead of being silently
// ignored. --database, --output, --non-interactive, --medicover-base-url, and
// --session-dir remain global.
func checkCommandFlags(command string, settings options) error {
	if settings.dry && command != "check" {
		return fmt.Errorf("--dry is not supported for %s", command)
	}
	if settings.watchOnce && command != "watch" {
		return fmt.Errorf("--once is not supported for %s", command)
	}
	if settings.maxIterationsRaw != "" && command != "watch" {
		return fmt.Errorf("--max-iterations is not supported for %s", command)
	}
	if settings.pollIntervalRaw != "" && command != "watch" {
		return fmt.Errorf("--poll-interval is not supported for %s", command)
	}
	if command == "check" {
		return checkObservationFlags(settings)
	}
	if command == "watch" {
		return checkWatchFlags(settings)
	}
	if command == "history" || strings.HasPrefix(command, "history ") {
		return checkHistoryFlags(command, settings)
	}
	if command == "completion" {
		return checkCompletionFlags(command, settings)
	}
	if command == "doctor" {
		if err := checkDoctorFlags(command, settings); err != nil {
			return err
		}
	}
	if err := checkHistoryFlagsForNonHistoryCommands(command, settings); err != nil {
		return err
	}
	hasAccountID := settings.accountID != ""
	hasUsername := settings.username != ""
	hasPasswordFile := settings.passwordFile != ""
	hasMFACodeFile := settings.mfaCodeFile != ""
	if err := checkProfileFlagsForNonProfileCommands(command, settings); err != nil {
		return err
	}
	if err := checkTelegramFlagsForNonTelegramCommands(command, settings); err != nil {
		return err
	}
	switch command {
	case "", "version", "doctor", "database initialize":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		}
	case "account list":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for account list")
		case hasUsername:
			return fmt.Errorf("--username is not supported for account list")
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for account list")
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for account list")
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for account list")
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for account list")
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for account list")
		}
	case "account show", "account delete":
		switch {
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		}
	case "account create", "account edit":
		switch {
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		}
	case "account login", "account authenticate":
		switch {
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		}
	case "account logout":
		switch {
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		}
	case "account status":
		switch {
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		}
	case "profile create":
		switch {
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.clearClinic:
			return fmt.Errorf("--clear-clinic is not supported for %s", command)
		case settings.clearDoctor:
			return fmt.Errorf("--clear-doctor is not supported for %s", command)
		case settings.clearLanguage:
			return fmt.Errorf("--clear-language is not supported for %s", command)
		case settings.clearVisitType:
			return fmt.Errorf("--clear-visit-type is not supported for %s", command)
		case settings.clearSearchType:
			return fmt.Errorf("--clear-search-type is not supported for %s", command)
		case settings.clearStartDate:
			return fmt.Errorf("--clear-start-date is not supported for %s", command)
		case settings.clearEndDate:
			return fmt.Errorf("--clear-end-date is not supported for %s", command)
		}
	case "profile list":
		switch {
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.region != "":
			return fmt.Errorf("--region is not supported for %s", command)
		case settings.specialty != "":
			return fmt.Errorf("--specialty is not supported for %s", command)
		case settings.clinic != "":
			return fmt.Errorf("--clinic is not supported for %s", command)
		case settings.doctor != "":
			return fmt.Errorf("--doctor is not supported for %s", command)
		case settings.language != "":
			return fmt.Errorf("--language is not supported for %s", command)
		case settings.visitType != "":
			return fmt.Errorf("--visit-type is not supported for %s", command)
		case settings.searchType != "":
			return fmt.Errorf("--search-type is not supported for %s", command)
		case settings.startDate != "":
			return fmt.Errorf("--start-date is not supported for %s", command)
		case settings.endDate != "":
			return fmt.Errorf("--end-date is not supported for %s", command)
		case settings.checkIntervalRaw != "":
			return fmt.Errorf("--check-interval-minutes is not supported for %s", command)
		case settings.profileDisabled:
			return fmt.Errorf("--disabled is not supported for %s", command)
		case settings.clearClinic:
			return fmt.Errorf("--clear-clinic is not supported for %s", command)
		case settings.clearDoctor:
			return fmt.Errorf("--clear-doctor is not supported for %s", command)
		case settings.clearLanguage:
			return fmt.Errorf("--clear-language is not supported for %s", command)
		case settings.clearVisitType:
			return fmt.Errorf("--clear-visit-type is not supported for %s", command)
		case settings.clearSearchType:
			return fmt.Errorf("--clear-search-type is not supported for %s", command)
		case settings.clearStartDate:
			return fmt.Errorf("--clear-start-date is not supported for %s", command)
		case settings.clearEndDate:
			return fmt.Errorf("--clear-end-date is not supported for %s", command)
		}
	case "profile show", "profile enable", "profile disable", "profile delete":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.region != "":
			return fmt.Errorf("--region is not supported for %s", command)
		case settings.specialty != "":
			return fmt.Errorf("--specialty is not supported for %s", command)
		case settings.clinic != "":
			return fmt.Errorf("--clinic is not supported for %s", command)
		case settings.doctor != "":
			return fmt.Errorf("--doctor is not supported for %s", command)
		case settings.language != "":
			return fmt.Errorf("--language is not supported for %s", command)
		case settings.visitType != "":
			return fmt.Errorf("--visit-type is not supported for %s", command)
		case settings.searchType != "":
			return fmt.Errorf("--search-type is not supported for %s", command)
		case settings.startDate != "":
			return fmt.Errorf("--start-date is not supported for %s", command)
		case settings.endDate != "":
			return fmt.Errorf("--end-date is not supported for %s", command)
		case settings.checkIntervalRaw != "":
			return fmt.Errorf("--check-interval-minutes is not supported for %s", command)
		case settings.profileDisabled:
			return fmt.Errorf("--disabled is not supported for %s", command)
		case settings.clearClinic:
			return fmt.Errorf("--clear-clinic is not supported for %s", command)
		case settings.clearDoctor:
			return fmt.Errorf("--clear-doctor is not supported for %s", command)
		case settings.clearLanguage:
			return fmt.Errorf("--clear-language is not supported for %s", command)
		case settings.clearVisitType:
			return fmt.Errorf("--clear-visit-type is not supported for %s", command)
		case settings.clearSearchType:
			return fmt.Errorf("--clear-search-type is not supported for %s", command)
		case settings.clearStartDate:
			return fmt.Errorf("--clear-start-date is not supported for %s", command)
		case settings.clearEndDate:
			return fmt.Errorf("--clear-end-date is not supported for %s", command)
		}
	case "profile edit":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s (the account of a profile never changes; delete and recreate to move it)", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.profileDisabled:
			return fmt.Errorf("--disabled is not supported for %s (use profile enable and profile disable)", command)
		}
	case "telegram create":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		case settings.clearTelegram:
			return fmt.Errorf("--clear-telegram is not supported for %s", command)
		}
	case "telegram list":
		switch {
		case settings.telegramIDsRaw != "":
			return fmt.Errorf("--telegram is not supported for %s", command)
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		}
	case "telegram show", "telegram enable", "telegram disable", "telegram delete", "telegram test":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		case settings.region != "":
			return fmt.Errorf("--region is not supported for %s", command)
		case settings.specialty != "":
			return fmt.Errorf("--specialty is not supported for %s", command)
		case settings.clearTelegram:
			return fmt.Errorf("--clear-telegram is not supported for %s", command)
		}
	case "telegram edit":
		switch {
		case hasAccountID:
			return fmt.Errorf("--account is not supported for %s", command)
		case hasUsername:
			return fmt.Errorf("--username is not supported for %s", command)
		case hasPasswordFile:
			return fmt.Errorf("--password-file is not supported for %s", command)
		case settings.passwordPrompt:
			return fmt.Errorf("--password-prompt is not supported for %s", command)
		case settings.noStoredPassword:
			return fmt.Errorf("--no-stored-password is not supported for %s", command)
		case hasMFACodeFile:
			return fmt.Errorf("--mfa-code-file is not supported for %s", command)
		case settings.forgetSecret:
			return fmt.Errorf("--forget-secret is not supported for %s", command)
		case settings.profileID != "":
			return fmt.Errorf("--profile is not supported for %s", command)
		case settings.clearTelegram:
			return fmt.Errorf("--clear-telegram is not supported for %s", command)
		}
	}
	return nil
}

// checkObservationFlags rejects account and ad-hoc criteria flags. Both dry
// and durable checks use the saved account and profile configuration.
func checkObservationFlags(settings options) error {
	unsupported := ""
	switch {
	case settings.accountID != "":
		unsupported = "--account"
	case settings.username != "":
		unsupported = "--username"
	case settings.passwordFile != "":
		unsupported = "--password-file"
	case settings.passwordPrompt:
		unsupported = "--password-prompt"
	case settings.noStoredPassword:
		unsupported = "--no-stored-password"
	case settings.mfaCodeFile != "":
		unsupported = "--mfa-code-file"
	case settings.forgetSecret:
		unsupported = "--forget-secret"
	case settings.region != "":
		unsupported = "--region"
	case settings.specialty != "":
		unsupported = "--specialty"
	case settings.clinic != "":
		unsupported = "--clinic"
	case settings.doctor != "":
		unsupported = "--doctor"
	case settings.language != "":
		unsupported = "--language"
	case settings.visitType != "":
		unsupported = "--visit-type"
	case settings.searchType != "":
		unsupported = "--search-type"
	case settings.startDate != "":
		unsupported = "--start-date"
	case settings.endDate != "":
		unsupported = "--end-date"
	case settings.checkIntervalRaw != "":
		unsupported = "--check-interval"
	case settings.profileDisabled:
		unsupported = "--disabled"
	case settings.clearClinic:
		unsupported = "--clear-clinic"
	case settings.clearDoctor:
		unsupported = "--clear-doctor"
	case settings.clearLanguage:
		unsupported = "--clear-language"
	case settings.clearVisitType:
		unsupported = "--clear-visit-type"
	case settings.clearSearchType:
		unsupported = "--clear-search-type"
	case settings.clearStartDate:
		unsupported = "--clear-start-date"
	case settings.clearEndDate:
		unsupported = "--clear-end-date"
	case settings.telegramIDsRaw != "":
		unsupported = "--telegram"
	case settings.telegramName != "":
		unsupported = "--name"
	case settings.chatID != "":
		unsupported = "--chat-id"
	case settings.tokenFile != "":
		unsupported = "--token-file"
	case settings.tokenPrompt:
		unsupported = "--token-prompt"
	case settings.noStoredToken:
		unsupported = "--no-stored-token"
	case settings.clearTelegram:
		unsupported = "--clear-telegram"
	}
	if unsupported != "" {
		return fmt.Errorf("%s is not supported for check", unsupported)
	}
	return nil
}

// checkProfileFlagsForNonProfileCommands rejects --profile and profile
// criteria flags for commands that do not consume them.
func checkProfileFlagsForNonProfileCommands(command string, settings options) error {
	if strings.HasPrefix(command, "profile ") || command == "check" {
		return nil
	}
	switch {
	case settings.profileID != "":
		return fmt.Errorf("--profile is not supported for %s", command)
	case settings.region != "":
		return fmt.Errorf("--region is not supported for %s", command)
	case settings.specialty != "":
		return fmt.Errorf("--specialty is not supported for %s", command)
	case settings.clinic != "":
		return fmt.Errorf("--clinic is not supported for %s", command)
	case settings.doctor != "":
		return fmt.Errorf("--doctor is not supported for %s", command)
	case settings.language != "":
		return fmt.Errorf("--language is not supported for %s", command)
	case settings.visitType != "":
		return fmt.Errorf("--visit-type is not supported for %s", command)
	case settings.searchType != "":
		return fmt.Errorf("--search-type is not supported for %s", command)
	case settings.startDate != "":
		return fmt.Errorf("--start-date is not supported for %s", command)
	case settings.endDate != "":
		return fmt.Errorf("--end-date is not supported for %s", command)
	case settings.checkIntervalRaw != "":
		return fmt.Errorf("--check-interval-minutes is not supported for %s", command)
	case settings.profileDisabled:
		return fmt.Errorf("--disabled is not supported for %s", command)
	case settings.clearClinic:
		return fmt.Errorf("--clear-clinic is not supported for %s", command)
	case settings.clearDoctor:
		return fmt.Errorf("--clear-doctor is not supported for %s", command)
	case settings.clearLanguage:
		return fmt.Errorf("--clear-language is not supported for %s", command)
	case settings.clearVisitType:
		return fmt.Errorf("--clear-visit-type is not supported for %s", command)
	case settings.clearSearchType:
		return fmt.Errorf("--clear-search-type is not supported for %s", command)
	case settings.clearStartDate:
		return fmt.Errorf("--clear-start-date is not supported for %s", command)
	case settings.clearEndDate:
		return fmt.Errorf("--clear-end-date is not supported for %s", command)
	}
	return nil
}

// checkTelegramFlagsForNonTelegramCommands rejects --telegram linking and
// destination configuration flags for commands that do not consume them.
// Telegram commands consume --telegram as a single id. Profile create and
// edit consume --telegram as a comma-separated link list.
func checkTelegramFlagsForNonTelegramCommands(command string, settings options) error {
	isTelegram := strings.HasPrefix(command, "telegram ")
	isProfileCreate := command == "profile create"
	isProfileEdit := command == "profile edit"
	if !isTelegram && !isProfileCreate && !isProfileEdit {
		switch {
		case settings.telegramIDsRaw != "":
			return fmt.Errorf("--telegram is not supported for %s", command)
		case settings.clearTelegram:
			return fmt.Errorf("--clear-telegram is not supported for %s", command)
		}
	}
	if isProfileCreate && settings.clearTelegram {
		return fmt.Errorf("--clear-telegram is not supported for profile create")
	}
	if command != "telegram create" && command != "telegram edit" {
		switch {
		case settings.telegramName != "":
			return fmt.Errorf("--name is not supported for %s", command)
		case settings.chatID != "":
			return fmt.Errorf("--chat-id is not supported for %s", command)
		case settings.tokenFile != "":
			return fmt.Errorf("--token-file is not supported for %s", command)
		case settings.tokenPrompt:
			return fmt.Errorf("--token-prompt is not supported for %s", command)
		case settings.noStoredToken:
			return fmt.Errorf("--no-stored-token is not supported for %s", command)
		}
	}
	return nil
}

func splitCommand(raw []string) ([]string, []string) {
	candidates := [][]string{
		{"database", "initialize"},
		{"account", "create"},
		{"account", "list"},
		{"account", "show"},
		{"account", "edit"},
		{"account", "delete"},
		{"account", "login"},
		{"account", "authenticate"},
		{"account", "logout"},
		{"account", "status"},
		{"profile", "create"},
		{"profile", "list"},
		{"profile", "show"},
		{"profile", "edit"},
		{"profile", "enable"},
		{"profile", "disable"},
		{"profile", "delete"},
		{"telegram", "create"},
		{"telegram", "list"},
		{"telegram", "show"},
		{"telegram", "edit"},
		{"telegram", "enable"},
		{"telegram", "disable"},
		{"telegram", "test"},
		{"telegram", "delete"},
		{"history", "runs"},
		{"history", "episodes"},
		{"history", "incidents"},
		{"history", "deliveries"},
		{"history", "incident-deliveries"},
		{"history", "status"},
		{"history", "retention"},
		{"history", "prune"},
		{"check"},
		{"watch"},
		{"history"},
		{"version"},
		{"doctor"},
		{"completion"},
	}
	for _, candidate := range candidates {
		if len(raw) >= len(candidate) {
			matched := true
			for i, word := range candidate {
				if raw[i] != word {
					matched = false
					break
				}
			}
			if matched {
				return candidate, raw[len(candidate):]
			}
		}
	}
	// Unknown command: keep at most two words as the command name so errors stay readable.
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) == 1 {
		return raw, nil
	}
	return raw[:2], raw[2:]
}

func defaultDatabasePath(getenv func(string) string) string {
	if dataHome := getenv("XDG_DATA_HOME"); filepath.IsAbs(dataHome) {
		return filepath.Join(dataHome, "medalert", "medalert.db")
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "share", "medalert", "medalert.db")
	}
	return ""
}

func isEnvTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func writeResult(writer io.Writer, command string, data any) {
	writeJSON(writer, map[string]any{"schema_version": resultSchemaVersion, "command": command, "data": data})
}

func reportStoreError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code := "database_error"
	if store.IsUnsupportedSchema(err) {
		code = "unsupported_schema"
	}
	writeError(stderr, command, code, err.Error(), jsonOutput)
	return 2
}

func reportAccountError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code := "database_error"
	switch {
	case errors.Is(err, store.ErrAccountExists):
		code = "account_exists"
	case errors.Is(err, store.ErrAccountNotFound):
		code = "account_not_found"
	case errors.Is(err, store.ErrAccountInvalid):
		code = "invalid_arguments"
	}
	writeError(stderr, command, code, err.Error(), jsonOutput)
	return 2
}

func reportProfileError(stderr io.Writer, command string, err error, jsonOutput bool) int {
	code := "database_error"
	switch {
	case errors.Is(err, store.ErrProfileExists):
		code = "profile_exists"
	case errors.Is(err, store.ErrProfileNotFound):
		code = "profile_not_found"
	case errors.Is(err, store.ErrAccountNotFound):
		code = "account_not_found"
	case errors.Is(err, store.ErrProfileInvalid):
		code = "invalid_arguments"
	}
	writeError(stderr, command, code, err.Error(), jsonOutput)
	return 2
}

func writeError(writer io.Writer, command, code, message string, jsonOutput bool) {
	if jsonOutput {
		writeJSON(writer, map[string]any{"schema_version": resultSchemaVersion, "command": command, "error": errorBody{Code: code, Message: message}})
		return
	}
	fmt.Fprintf(writer, "Error: %s\n", message)
}

// writeRateLimitError reports a 429 with the server-requested backoff so
// automation can honor Retry-After instead of polling blindly. The retry
// value is omitted when the server did not provide a valid one.
func writeRateLimitError(writer io.Writer, command, message string, retryAfter time.Duration, jsonOutput bool) {
	seconds := int(retryAfter / time.Second)
	var retryField *int
	display := message
	if seconds > 0 {
		retryField = &seconds
		display = fmt.Sprintf("%s (retry after %ds)", message, seconds)
	}
	if jsonOutput {
		writeJSON(writer, map[string]any{"schema_version": resultSchemaVersion, "command": command, "error": errorBody{Code: "rate_limited", Message: display, RetryAfter: retryField}})
		return
	}
	fmt.Fprintf(writer, "Error: %s\n", display)
}

func writeJSON(writer io.Writer, value any) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func wantsJSON(arguments []string, getenv func(string) string) bool {
	if getenv("MEDALERT_OUTPUT") == "json" {
		return true
	}
	for index, argument := range arguments {
		if argument == "--output" && index+1 < len(arguments) && arguments[index+1] == "json" {
			return true
		}
	}
	return false
}

func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}
