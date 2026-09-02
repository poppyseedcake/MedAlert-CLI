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
	command  []string
	database string
	output   string
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func Run(arguments []string, stdout, stderr io.Writer, getenv func(string) string) int {
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
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "--version":
			settings.command = []string{"version"}
		case "--output", "--database":
			if index+1 >= len(arguments) {
				return settings, fmt.Errorf("%s needs a value", argument)
			}
			index++
			if argument == "--output" {
				settings.output = arguments[index]
			} else {
				settings.database = arguments[index]
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return settings, fmt.Errorf("unknown flag: %s", argument)
			}
			settings.command = append(settings.command, argument)
		}
	}
	if settings.output != "text" && settings.output != "json" {
		return settings, fmt.Errorf("output must be text or json")
	}
	if settings.database == "" {
		return settings, errors.New("database path is empty")
	}
	return settings, nil
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
