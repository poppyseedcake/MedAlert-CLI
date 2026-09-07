#!/bin/sh

set -eu

image=${1:-medalert:test}
platform=linux/amd64
container_name="medalert-acceptance-$$"
data_volume="medalert-acceptance-data-$$"
secret_volume="medalert-acceptance-secrets-$$"
temporary_dir=$(mktemp -d)
container_started=0

cleanup() {
	set +e
	if command -v docker >/dev/null 2>&1; then
		if [ "$container_started" -eq 1 ]; then
			docker rm -f "$container_name" >/dev/null 2>&1
		fi
		docker volume rm "$data_volume" "$secret_volume" >/dev/null 2>&1
	fi
	rm -rf "$temporary_dir"
}

trap cleanup EXIT HUP INT TERM

if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
	printf '%s\n' 'container acceptance test needs Linux amd64' >&2
	exit 77
fi

if ! command -v docker >/dev/null 2>&1; then
	printf '%s\n' 'container acceptance test needs Docker' >&2
	exit 1
fi

if ! docker image inspect "$image" >/dev/null 2>&1; then
	printf 'Docker image is not available: %s\n' "$image" >&2
	exit 1
fi

image_os=$(docker image inspect --format '{{.Os}}' "$image")
image_architecture=$(docker image inspect --format '{{.Architecture}}' "$image")
if [ "$image_os" != "linux" ] || [ "$image_architecture" != "amd64" ]; then
	printf 'image platform is %s/%s, want linux/amd64\n' "$image_os" "$image_architecture" >&2
	exit 1
fi

image_user=$(docker image inspect --format '{{.Config.User}}' "$image")
if [ -z "$image_user" ] || [ "$image_user" = "0" ] || [ "$image_user" = "root" ]; then
	printf 'image must define a non-root user, got %s\n' "$image_user" >&2
	exit 1
fi

image_command=$(docker image inspect --format '{{json .Config.Cmd}}' "$image")
if [ "$image_command" != '["watch","--non-interactive"]' ]; then
	printf 'image default command is %s, want watch --non-interactive\n' "$image_command" >&2
	exit 1
fi

image_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")
if [ -z "$image_revision" ]; then
	printf '%s\n' 'image source revision label is missing' >&2
	exit 1
fi
version_json=$(docker run --rm --platform "$platform" "$image" version --output json)
if ! printf '%s\n' "$version_json" | grep -F '"commit":"'"$image_revision"'"' >/dev/null 2>&1; then
	printf '%s\n' 'executable source commit does not match the image revision label' >&2
	exit 1
fi

secret_marker="medalert-secret-marker-$$"
if docker image inspect "$image" | grep -F "$secret_marker" >/dev/null 2>&1; then
	printf '%s\n' 'secret marker is present in image metadata' >&2
	exit 1
fi

umask 077
mkdir -p "$temporary_dir/input-secrets"
printf '%s\n' "$secret_marker-password" >"$temporary_dir/input-secrets/password"
printf '%s\n' "$secret_marker-token" >"$temporary_dir/input-secrets/telegram-token"
chmod 0400 "$temporary_dir/input-secrets/password" "$temporary_dir/input-secrets/telegram-token"

docker volume create "$data_volume" >/dev/null
docker volume create "$secret_volume" >/dev/null

# Docker mounts secret files from a host or secret store. This root-only setup
# copies the test files into a mounted volume with the runtime UID and strict
# permissions, which models a Docker secret with uid/gid/mode controls.
docker run --rm --platform "$platform" --user 0:0 \
	--mount "type=bind,src=$temporary_dir/input-secrets,dst=/input,readonly" \
	--mount "type=volume,src=$secret_volume,dst=/run/secrets" \
	--entrypoint /bin/sh "$image" \
	-c 'set -eu
cp /input/password /run/secrets/password
cp /input/telegram-token /run/secrets/telegram-token
chown 10001:10001 /run/secrets/password /run/secrets/telegram-token
chmod 0400 /run/secrets/password /run/secrets/telegram-token'

run_image() {
	docker run --rm --platform "$platform" \
		--mount "type=volume,src=$data_volume,dst=/var/lib/medalert" \
		--mount "type=volume,src=$secret_volume,dst=/run/secrets,readonly" \
		"$image" "$@"
}

run_image database initialize >/dev/null
run_image account create --account acceptance --username acceptance@example.com \
	--password-file /run/secrets/password --non-interactive >/dev/null
run_image telegram create --telegram acceptance --name Acceptance \
	--chat-id 12345 --token-file /run/secrets/telegram-token \
	--non-interactive >/dev/null

if ! docker run --rm --platform "$platform" \
	--mount "type=volume,src=$data_volume,dst=/var/lib/medalert,readonly" \
	--mount "type=volume,src=$secret_volume,dst=/run/secrets,readonly" \
	--entrypoint /bin/sh "$image" \
	-c 'set -eu
[ "$(id -u)" = 10001 ]
[ "$(id -g)" = 10001 ]
[ "$(stat -c %a /var/lib/medalert)" = 700 ]
[ "$(stat -c %a /var/lib/medalert/medalert.db)" = 600 ]
! grep -aF "medalert-secret-marker" /var/lib/medalert/medalert.db' \
	>/dev/null; then
	printf '%s\n' 'runtime identity, durable state, or secret redaction check failed' >&2
	exit 1
fi

docker run -d --name "$container_name" --platform "$platform" \
	--mount "type=volume,src=$data_volume,dst=/var/lib/medalert" \
	--mount "type=volume,src=$secret_volume,dst=/run/secrets,readonly" \
	"$image" >/dev/null
container_started=1

state=starting
iteration=0
while [ "$iteration" -lt 20 ]; do
	state=$(docker inspect --format '{{.State.Status}}' "$container_name")
	if [ "$state" = "running" ]; then
		break
	fi
	iteration=$((iteration + 1))
	sleep 1
done

if [ "$state" != "running" ]; then
	docker logs "$container_name" >&2 || true
	printf 'container did not stay running, state: %s\n' "$state" >&2
	exit 1
fi

if ! docker exec "$container_name" /bin/sh -c '[ "$(id -u)" = 10001 ] && [ "$(id -g)" = 10001 ]' >/dev/null; then
	printf '%s\n' 'running container is not using the medalert non-root identity' >&2
	exit 1
fi

docker stop --time 20 "$container_name" >/dev/null || true
container_state=$(docker inspect --format '{{.State.Status}}' "$container_name")
container_exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$container_name")
container_logs=$(docker logs "$container_name" 2>&1)
if [ "$container_state" != "exited" ] || [ "$container_exit_code" != "0" ]; then
	printf 'controlled stop failed: state=%s exit_code=%s\n' "$container_state" "$container_exit_code" >&2
	printf '%s\n' "$container_logs" >&2
	exit 1
fi
case "$container_logs" in
	*'received SIGTERM'*) ;;
	*)
		printf '%s\n' 'container did not report SIGTERM handling' >&2
		printf '%s\n' "$container_logs" >&2
		exit 1
		;;
esac
case "$container_logs" in
	*'watch stopped after '*) ;;
	*)
		printf '%s\n' 'container did not report a controlled stop' >&2
		printf '%s\n' "$container_logs" >&2
		exit 1
		;;
esac

printf 'container acceptance test passed for %s\n' "$image"
