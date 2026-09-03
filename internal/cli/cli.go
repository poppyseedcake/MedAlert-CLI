package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	accountID        string
	username         string
	passwordFile     string
	passwordPrompt   bool
	noStoredPassword bool
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
	commandName := strings.Join(settings.command, " ")
	switch commandName {
	case "version":
		commit := buildinfo.SourceCommit()
		if settings.output == "json" {
			writeResult(stdout, "version", map[string]any{"commit": commit})
		} else {
			fmt.Fprintf(stdout, "medalert %s\n", commit)
		}
		return 0
	case "doctor":
		status, inspectErr := store.Inspect(settings.database)
		if inspectErr != nil {
			return reportStoreError(stderr, commandName, inspectErr, settings.output == "json")
		}
		if settings.output == "json" {
			writeResult(stdout, "doctor", status)
		} else if status.MigrationRequired {
			fmt.Fprintf(stdout, "Database migration to schema %d is required.\n", status.RequiredVersion)
		} else {
			fmt.Fprintf(stdout, "Database schema %d is supported.\n", status.SchemaVersion)
		}
		return 0
	case "database initialize":
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
	var raw []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--version":
			raw = append(raw, "version")
		case "--output", "--database", "--account", "--username", "--user", "--password-file":
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
			}
		case "--password-prompt":
			settings.passwordPrompt = true
		case "--no-stored-password":
			settings.noStoredPassword = true
		case "--non-interactive":
			settings.nonInteractive = true
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
	if settings.database == "" {
		return settings, errors.New("database path is empty")
	}
	return settings, nil
}

func splitCommand(raw []string) ([]string, []string) {
	candidates := [][]string{
		{"database", "initialize"},
		{"account", "create"},
		{"account", "list"},
		{"account", "show"},
		{"account", "edit"},
		{"account", "delete"},
		{"version"},
		{"doctor"},
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

func writeError(writer io.Writer, command, code, message string, jsonOutput bool) {
	if jsonOutput {
		writeJSON(writer, map[string]any{"schema_version": resultSchemaVersion, "command": command, "error": errorBody{Code: code, Message: message}})
		return
	}
	fmt.Fprintf(writer, "Error: %s\n", message)
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
