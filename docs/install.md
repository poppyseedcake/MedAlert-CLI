# Native Linux installation

This guide is for Linux amd64 (`x86_64`) with a systemd user manager and a
working Secret Service provider. MedAlert does not support another native
operating system or architecture. It does not publish native archives or
GitHub Releases.

The native build uses Go with CGO disabled. Use Go 1.25 or newer.

## Install the command

Choose one installation method. Both methods install the command at
`$HOME/.local/bin/medalert`, which is the path used by the service unit.

### From the default branch

```sh
install -d -m 0755 "$HOME/.local/bin"
GOBIN="$HOME/.local/bin" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go install github.com/poppyseedcake/MedAlert/cmd/medalert@main
```

### From a local checkout

```sh
install -d -m 0755 "$HOME/.local/bin"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o "$HOME/.local/bin/medalert" ./cmd/medalert
```

If `$HOME/.local/bin` is not in `PATH`, use the full command path or add this
line to the shell startup file:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Check the installed command without opening the terminal interface:

```sh
medalert version
medalert version --output json
```

## Data and secrets

MedAlert stores its SQLite database at:

```text
${XDG_DATA_HOME}/medalert/medalert.db
```

When `XDG_DATA_HOME` is not set to an absolute path, it uses:

```text
$HOME/.local/share/medalert/medalert.db
```

The database directory and file are private. Migration backups are stored in
the `backups` directory beside the database. MedAlert keeps the three newest
backups.

Native Linux stores account passwords, Telegram bot tokens, and Medicover
session state in Secret Service. These values are not stored in SQLite, normal
files, command output, or logs. Make sure a Secret Service provider is
available in the user session before you create an account or start the
service.

Initialize the database before configuration:

```sh
medalert database initialize
```

Create an account in a terminal. The default password source stores the
password in Secret Service:

```sh
medalert account create --account personal --username 'user@example.com'
```

Configure profiles and Telegram destinations with the `profile` and `telegram`
commands. The service does not open prompts, so every enabled account must have
a usable non-interactive Secret Service password source.

## Install and start the systemd user service

From a local checkout, install the unit file:

```sh
install -D -m 0644 systemd/medalert.service \
  "$HOME/.config/systemd/user/medalert.service"
```

If you installed only with `go install`, download the unit from the same
default branch instead:

```sh
install -d -m 0755 "$HOME/.config/systemd/user"
curl --fail --location --silent --show-error \
  https://raw.githubusercontent.com/poppyseedcake/MedAlert/main/systemd/medalert.service \
  --output "$HOME/.config/systemd/user/medalert.service"
```

Reload the user manager and start monitoring:

```sh
systemctl --user daemon-reload
systemctl --user enable --now medalert.service
systemctl --user status medalert.service
```

The unit runs `medalert watch --non-interactive`. It does not start a custom
daemon. It does not set a database path, so the command uses the XDG data path
described above. It clears test-only database and session overrides, so native
operation uses SQLite in the XDG data directory and Secret Service for session
state.

Text watch logs go to standard error. systemd sends standard output and
standard error to the user journal; MedAlert does not create log files.

Read the service log with:

```sh
journalctl --user -u medalert.service --follow
```

## Stop and restart safely

Use systemd to stop or restart the service:

```sh
systemctl --user stop medalert.service
systemctl --user start medalert.service
systemctl --user restart medalert.service
```

`watch` handles SIGTERM and SIGINT. It waits for active work to stop before it
exits. SQLite transactions and durable delivery state are then left in their
last valid state. Do not delete the database, the `backups` directory, or
Secret Service entries as part of a normal stop or restart.

## Update safely

Stop the service before replacing the command:

```sh
systemctl --user stop medalert.service
GOBIN="$HOME/.local/bin" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go install github.com/poppyseedcake/MedAlert/cmd/medalert@main
"$HOME/.local/bin/medalert" database initialize
"$HOME/.local/bin/medalert" doctor
systemctl --user start medalert.service
```

`database initialize` runs automatically when a command opens the database.
When a schema migration is needed, MedAlert first creates a private SQLite
backup in:

```text
${XDG_DATA_HOME}/medalert/backups/medalert-schema-<old-version>-<commit>-<timestamp>.sqlite3
```

The migration runs in a transaction. If it fails, the database is rolled back
and the backup remains available. A newer database schema is rejected without
changing the database.

For a local checkout, build the replacement command with the same Linux amd64
settings, then run `database initialize` as shown above:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o "$HOME/.local/bin/medalert" ./cmd/medalert
```

## Recover from a failed migration

A failed migration normally leaves the current database usable. If recovery is
needed, stop the service and restore a backup while the command is not running:

```sh
systemctl --user stop medalert.service
case ${XDG_DATA_HOME:-} in
  /*) data_dir="$XDG_DATA_HOME/medalert" ;;
  *) data_dir="$HOME/.local/share/medalert" ;;
esac
backup=$(ls -1t "$data_dir"/backups/medalert-schema-*.sqlite3 2>/dev/null | head -n 1)
if [ -z "$backup" ] || [ ! -r "$backup" ]; then
	printf '%s\n' 'no readable migration backup found' >&2
	exit 1
fi
mv "$data_dir/medalert.db" "$data_dir/medalert.db.failed"
install -m 0600 "$backup" "$data_dir/medalert.db"
"$HOME/.local/bin/medalert" doctor
```

The selected backup is the newest saved pre-migration database. Keep
`medalert.db.failed` until recovery is complete. If the new command still
cannot open or migrate the restored database, install the last known-good
source revision, run `doctor`, and start the service:

```sh
GOBIN="$HOME/.local/bin" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go install github.com/poppyseedcake/MedAlert/cmd/medalert@<known-good-commit>
"$HOME/.local/bin/medalert" doctor
systemctl --user start medalert.service
```

## Verify a local installation

Run this from the repository on Linux amd64:

```sh
./scripts/install-smoke-test.sh
```

The smoke test installs the command into a temporary `GOBIN`, checks the
systemd unit, verifies the installed text and JSON command interface, and
confirms that database initialization uses a temporary XDG data directory.
