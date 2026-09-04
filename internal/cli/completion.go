package cli

import (
	"fmt"
	"io"
	"strings"
)

// runCompletion prints a shell completion script. The script text is static
// and never touches the database or secrets, so completion works offline and
// never prompts. JSON output wraps the same script in the versioned envelope
// so automation keeps one contract.
func runCompletion(command string, settings options, stdout, stderr io.Writer) int {
	jsonOutput := settings.output == "json"
	shell := ""
	if len(settings.positionals) > 0 {
		shell = strings.ToLower(strings.TrimSpace(settings.positionals[0]))
	}
	if shell == "" {
		shell = "bash"
	}
	script, ok := completionScript(shell)
	if !ok {
		writeError(stderr, command, "invalid_arguments", "shell must be bash, zsh, fish, or powershell", jsonOutput)
		return 2
	}
	if len(settings.positionals) > 1 {
		writeError(stderr, command, "invalid_arguments", "too many arguments for completion", jsonOutput)
		return 2
	}
	if jsonOutput {
		writeResult(stdout, command, map[string]any{"shell": shell, "script": script})
		return 0
	}
	fmt.Fprint(stdout, script)
	return 0
}

// checkCompletionFlags rejects every flag completion does not consume.
// Completion is offline and static: only --output selects text or JSON.
func checkCompletionFlags(command string, settings options) error {
	switch {
	case settings.accountID != "":
		return fmt.Errorf("--account is not supported for %s", command)
	case settings.username != "":
		return fmt.Errorf("--username is not supported for %s", command)
	case settings.passwordFile != "":
		return fmt.Errorf("--password-file is not supported for %s", command)
	case settings.passwordPrompt:
		return fmt.Errorf("--password-prompt is not supported for %s", command)
	case settings.noStoredPassword:
		return fmt.Errorf("--no-stored-password is not supported for %s", command)
	case settings.mfaCodeFile != "":
		return fmt.Errorf("--mfa-code-file is not supported for %s", command)
	case settings.forgetSecret:
		return fmt.Errorf("--forget-secret is not supported for %s", command)
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
	case settings.telegramIDsRaw != "":
		return fmt.Errorf("--telegram is not supported for %s", command)
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
	case settings.clearTelegram:
		return fmt.Errorf("--clear-telegram is not supported for %s", command)
	case settings.dry:
		return fmt.Errorf("--dry is not supported for %s", command)
	case settings.watchOnce:
		return fmt.Errorf("--once is not supported for %s", command)
	case settings.maxIterationsRaw != "":
		return fmt.Errorf("--max-iterations is not supported for %s", command)
	case settings.pollIntervalRaw != "":
		return fmt.Errorf("--poll-interval is not supported for %s", command)
	}
	return checkHistoryFlagsForNonHistoryCommands(command, settings)
}

func completionScript(shell string) (string, bool) {
	switch shell {
	case "bash":
		return `# medalert bash completion
_medalert_complete() {
  local cur prev commands
  commands="account profile telegram check watch history doctor version completion"
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  case "$prev" in
    account) COMPREPLY=($(compgen -W "create list show edit delete login authenticate logout status" -- "$cur")); return 0;;
    profile) COMPREPLY=($(compgen -W "create list show edit enable disable delete" -- "$cur")); return 0;;
    telegram) COMPREPLY=($(compgen -W "create list show edit enable disable test delete" -- "$cur")); return 0;;
    history) COMPREPLY=($(compgen -W "runs episodes incidents deliveries incident-deliveries status retention prune" -- "$cur")); return 0;;
    completion) COMPREPLY=($(compgen -W "bash zsh fish powershell" -- "$cur")); return 0;;
  esac
  if [[ $COMP_CWORD -eq 1 ]]; then
    COMPREPLY=($(compgen -W "$commands" -- "$cur"))
    return 0
  fi
  COMPREPLY=($(compgen -W "--database --output --non-interactive --help" -- "$cur"))
}
complete -F _medalert_complete medalert
`, true
	case "zsh":
		return `# medalert zsh completion
#compdef medalert
_medalert() {
  local -a commands
  commands=(account profile telegram check watch history doctor version completion)
  _describe 'command' commands
}
compdef _medalert medalert
`, true
	case "fish":
		return `# medalert fish completion
complete -c medalert -f -n '__fish_use_subcommand' -a account -d 'Manage accounts'
complete -c medalert -f -n '__fish_use_subcommand' -a profile -d 'Manage profiles'
complete -c medalert -f -n '__fish_use_subcommand' -a telegram -d 'Manage destinations'
complete -c medalert -f -n '__fish_use_subcommand' -a check -d 'Run a check'
complete -c medalert -f -n '__fish_use_subcommand' -a watch -d 'Monitor continuously'
complete -c medalert -f -n '__fish_use_subcommand' -a history -d 'Inspect history'
complete -c medalert -f -n '__fish_use_subcommand' -a doctor -d 'Diagnose setup'
complete -c medalert -f -n '__fish_use_subcommand' -a version -d 'Show version'
complete -c medalert -f -n '__fish_use_subcommand' -a completion -d 'Print completion'
`, true
	case "powershell":
		return `# medalert powershell completion
Register-ArgumentCompleter -Native -CommandName medalert -ScriptBlock {
  param($wordToComplete, $commandAst, $cursorPosition)
  $commands = @('account','profile','telegram','check','watch','history','doctor','version','completion')
  $commands | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
    [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
  }
}
`, true
	default:
		return "", false
	}
}
