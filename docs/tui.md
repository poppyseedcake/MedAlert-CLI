# Polish terminal interface

Run `medalert` without a command in a terminal. Both standard input and standard
output must be terminals. The interface always starts on `Stan systemu`.
It shows saved status and required actions. Press `R` to read the current data.
Closing the interface does not start or stop `medalert watch`.

Without a terminal, a missing command returns exit code 2. The same rule applies
with `--non-interactive` or `--output json`. Explicit commands keep their English
text and JSON output. Use `--database` or `MEDALERT_DATABASE` to select a database.
The existing session directory and Medicover endpoint settings also apply.

## Screen size and keyboard

The minimum size is 80 columns by 24 rows. A smaller terminal shows a Polish
size message. At 120 columns, a third panel shows required actions. Lists scroll
with the selected row. The interface uses the terminal colors. Text and the `>`
marker identify status and focus without color.

| Key | Action |
| --- | --- |
| `1`–`6` | Open Stan, Konta, Profile, Telegram, Monitoring, or Historia |
| Up / Down | Select a row |
| Page Up / Page Down | Move through a long list |
| Enter | Open details or the selected required action |
| `R` | Read status again |
| `?` | Open keyboard help outside a form |
| `F1` | Open help, including in a form |
| Tab / Shift+Tab | Select a field or button in a form |
| Esc | Return one level, or return to Stan from an area |
| `Q` | Exit outside a form |
| Ctrl+C | Exit from any screen |

Letter shortcuts accept lowercase and uppercase keys. While a text field has focus, letters and
numbers are input. Arrow keys also move the input cursor. Enter moves to the
next field. Select `Zapisz` to save. Esc asks before it discards changed fields.
A failed save keeps the form values for correction.

## Accounts

In `Konta`, use `A` to add, `E` to edit, `L` to log in, `O` to log out, and `D`
to delete the selected account. Enter shows its login and saved session status.
The saved session status is a local check. It does not prove that the remote
session is still valid.

A new account needs an identifier and a Medicover login. An optional password
file must meet the existing secret file rules. Leave this field empty to enter
a hidden password during login. When editing, an empty password file field
keeps the current source. Existing Secret Service and file sources also work.
Passwords and MFA codes are never form fields for account configuration.

Login first tries the saved session. If it needs a password, the interface uses
the account's secret source. It asks for an MFA code only when Medicover returns
an MFA form. It sends that code in the same challenge, without a second password
login. Hidden fields never display their values. Esc cancels login. Login has a
five-minute limit, including input time.

Logout and deletion default to `Nie` (No). Select `Tak` (Yes) and press Enter
to confirm. Logout removes only the selected account's session. Deletion also
removes its profiles and history. Other accounts keep their sessions.

## Profiles

In `Profile`, use `A` to create, `E` to edit, `P` to enable or disable, `Y` to
run a dry check, `K` to run a durable check, and `D` to delete the selected
profile. A profile form covers the account, region, specialty, clinic, doctor,
language, visit type, search type, date range, and check interval. The form
keeps long input fields visible while you move through them. Validation errors
are shown in Polish and the profile form keeps its values after a failed save.

Deleting a profile also deletes its observation history. Disabling a profile
pauses its monitoring state without deleting its configuration or history.

## Monitoring

`Monitoring` shows each profile's enabled work, its next planned run, profiles
paused because an account needs authentication, and active operational
problems. The next planned run is calculated from the last saved observation
run and the profile interval. `Y` does not change observation history or send
notifications. `K` uses the durable `check` operation and can update history
and notifications.

## Implementation and tests

`internal/tui` owns screen state and keyboard behavior. The
`internal/application` service owns profile actions, checks, and the safe
monitoring query. `internal/cli/tui.go` maps TUI requests to that service and
supplies only display data to the model. It does not send command output or
raw errors to the terminal. The Medicover module owns the MFA challenge and
accepts an optional input callback.

State tests use real SQLite files and a local Medicover test server. They cover
account changes, profile creation and editing, dry and durable checks, profile
pauses, required-action navigation, password and MFA input, session reuse,
account isolation, confirmation, cancellation, input correction, and secret
redaction. One test runs the built executable through a pseudo-terminal. It
also checks resizing, help, alternate-screen cleanup, and secret markers in all
captured output.

Design source: [accepted status-first prototype](https://github.com/poppyseedcake/MedAlert/issues/9#issuecomment-5496941266).
Scope: [issue #33](https://github.com/poppyseedcake/MedAlert/issues/33).
