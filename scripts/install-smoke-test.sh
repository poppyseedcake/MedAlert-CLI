#!/bin/sh

set -eu

if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
	printf '%s\n' 'installation smoke test needs Linux amd64' >&2
	exit 77
fi

repository_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
unit_file="$repository_dir/systemd/medalert.service"

if [ ! -f "$unit_file" ]; then
	printf 'missing systemd unit: %s\n' "$unit_file" >&2
	exit 1
fi

grep -Fqx 'ExecStart=%h/.local/bin/medalert watch --non-interactive' "$unit_file"
grep -Fqx 'StandardOutput=journal' "$unit_file"
grep -Fqx 'StandardError=journal' "$unit_file"
grep -Fqx 'UnsetEnvironment=MEDALERT_DATABASE MEDALERT_SESSION_DIR' "$unit_file"

if ! command -v systemd-analyze >/dev/null 2>&1; then
	printf '%s\n' 'systemd-analyze is required by the installation smoke test' >&2
	exit 1
fi

temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM
mkdir -p "$temporary_dir/home" "$temporary_dir/bin" "$temporary_dir/data"

(
	cd "$repository_dir"
	GOBIN="$temporary_dir/bin" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go install ./cmd/medalert
)

installed_command="$temporary_dir/bin/medalert"
if [ ! -x "$installed_command" ]; then
	printf 'go install did not create executable: %s\n' "$installed_command" >&2
	exit 1
fi

verified_unit="$temporary_dir/medalert.service"
sed "s|^ExecStart=.*|ExecStart=$installed_command watch --non-interactive|" "$unit_file" >"$verified_unit"
systemd-analyze verify "$verified_unit"

run_installed() {
	env \
		-u MEDALERT_DATABASE \
		-u MEDALERT_SESSION_DIR \
		-u MEDALERT_OUTPUT \
		-u MEDALERT_NON_INTERACTIVE \
		HOME="$temporary_dir/home" \
		XDG_DATA_HOME="$temporary_dir/data" \
		"$installed_command" "$@"
}

version_text=$(run_installed --version)
case "$version_text" in
	medalert\ *) ;;
	*)
		printf 'unexpected --version output: %s\n' "$version_text" >&2
		exit 1
		;;
esac

version_json=$(run_installed version --output json)
case "$version_json" in
	*'"command":"version"'*'"commit":"'*) ;;
	*)
		printf 'unexpected version JSON output: %s\n' "$version_json" >&2
		exit 1
		;;
esac

run_installed database initialize >/dev/null
database_file="$temporary_dir/data/medalert/medalert.db"
if [ ! -f "$database_file" ]; then
	printf 'database was not created at XDG data path: %s\n' "$database_file" >&2
	exit 1
fi

watch_stdout="$temporary_dir/watch.stdout"
watch_stderr="$temporary_dir/watch.stderr"
if ! run_installed watch --non-interactive --once >"$watch_stdout" 2>"$watch_stderr"; then
	cat "$watch_stderr" >&2
	printf '%s\n' 'installed watch command failed' >&2
	exit 1
fi
if [ -s "$watch_stdout" ]; then
	printf '%s\n' 'text watch output must stay on standard error' >&2
	cat "$watch_stdout" >&2
	exit 1
fi
grep -Fq 'watch completed 1 iterations' "$watch_stderr"

doctor_json=$(run_installed doctor --output json)
case "$doctor_json" in
	*'"command":"doctor"'*'"schema_version":'[0-9]*) ;;
	*)
		printf 'unexpected doctor JSON output: %s\n' "$doctor_json" >&2
		exit 1
		;;
esac

printf '%s\n' 'installation smoke test passed'
