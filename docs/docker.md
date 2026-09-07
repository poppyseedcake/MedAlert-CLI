# Docker installation

MedAlert provides one container image for Linux amd64 (`x86_64`). The image
runs as the `medalert` user with UID and GID `10001`. It does not support
another operating system or architecture.

The image is published at:

```text
ghcr.io/poppyseedcake/medalert
```

Every successful update to `main` publishes `latest` and a commit tag in this
form:

```text
ghcr.io/poppyseedcake/medalert:sha-<full-40-character-commit>
```

Use a commit tag for an installation that must stay on one source revision.
Use `latest` only when an automatic update to the current `main` image is
acceptable.

## Data and secret paths

Mount one persistent Docker volume at `/var/lib/medalert`. The image sets
these paths:

```text
SQLite database:       /var/lib/medalert/medalert.db
Migration backups:     /var/lib/medalert/backups/
Medicover session state: /var/lib/medalert/session/sessions/<account>.json
```

The application creates private directories and files. Session files use mode
`0600` inside a mode `0700` directory. Do not remove the volume during an
image update or rollback.

Docker password and Telegram token secrets must be mounted as regular files.
The files must have no group or other permissions, for example mode `0400` or
`0600`, and must be readable by UID `10001`. Do not put secret values in an
image, Dockerfile, `-e` option, `--env-file`, or build argument.

The examples below use two host files. Docker Swarm or a secret manager can
provide the same files at the same paths when it sets UID `10001`, GID
`10001`, and mode `0400`.

Prepare the files without putting secret values in shell history:

```sh
secret_dir="$PWD/medalert-secrets"
install -d -m 0700 "$secret_dir"
# Create medicover-password and telegram-token with your secret manager.
chmod 0400 "$secret_dir/medicover-password" "$secret_dir/telegram-token"
sudo chown 10001:10001 "$secret_dir/medicover-password" "$secret_dir/telegram-token"
```

## Configure the volume

Create a named volume. Docker copies the image's private data directory into
the new volume on its first use:

```sh
docker volume create medalert-data
```

Set the image and mount arguments once for the following setup commands:

```sh
image=ghcr.io/poppyseedcake/medalert:latest
data_mount='type=volume,src=medalert-data,dst=/var/lib/medalert'
password_mount="type=bind,src=$PWD/medalert-secrets/medicover-password,dst=/run/secrets/medicover-password,readonly"
token_mount="type=bind,src=$PWD/medalert-secrets/telegram-token,dst=/run/secrets/telegram-token,readonly"

run_medalert() {
  docker run --rm --platform linux/amd64 \
    --mount "$data_mount" \
    --mount "$password_mount" \
    --mount "$token_mount" \
    "$image" "$@"
}
```

Initialize the database and save only secret references in it:

```sh
run_medalert database initialize
run_medalert account create \
  --account personal \
  --username 'user@example.com' \
  --password-file /run/secrets/medicover-password \
  --non-interactive
run_medalert telegram create \
  --telegram phone \
  --name Phone \
  --chat-id '<telegram-chat-id>' \
  --token-file /run/secrets/telegram-token \
  --non-interactive
```

Configure profiles with the `profile` commands. Keep both secret mounts on
every `watch` container so MedAlert can authenticate and send notifications.

## Start and stop monitoring

The image entrypoint is `medalert`. Its default command is
`watch --non-interactive`, so no command is needed after the image name:

```sh
docker run -d \
  --name medalert \
  --restart unless-stopped \
  --platform linux/amd64 \
  --mount "$data_mount" \
  --mount "$password_mount" \
  --mount "$token_mount" \
  "$image"
```

Read text logs with:

```sh
docker logs --follow medalert
```

Stop the container with a controlled signal. MedAlert waits for active work
before it exits:

```sh
docker stop --time 20 medalert
```

The container exits with status `0` after SIGTERM. The data volume and secret
files remain outside the container.

## Check the source commit

The `version` command reports the commit used for the image build. The image
also stores it in the OCI revision label:

```sh
docker run --rm --platform linux/amd64 "$image" version --output json
docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image"
```

Both values identify the source revision. The version command does not need a
data volume or secret mount.

## Update safely

Select the new full commit identity from the successful CI run. Pull the
immutable commit image before stopping the current container:

```sh
new_commit='<full-40-character-commit>'
new_image="ghcr.io/poppyseedcake/medalert:sha-$new_commit"
docker pull --platform linux/amd64 "$new_image"
docker stop --time 20 medalert
docker rm medalert
docker run -d \
  --name medalert \
  --restart unless-stopped \
  --platform linux/amd64 \
  --mount "$data_mount" \
  --mount "$password_mount" \
  --mount "$token_mount" \
  "$new_image"
```

The same `medalert-data` volume is used by the new container. `database
initialize` runs when the application opens the database and creates a
private migration backup before a schema migration.

## Roll back safely

Use the last known-good full commit identity. The rollback changes only the
image tag and keeps the data volume and secret mounts:

```sh
known_good_commit='<full-40-character-commit>'
known_good_image="ghcr.io/poppyseedcake/medalert:sha-$known_good_commit"
docker pull --platform linux/amd64 "$known_good_image"
docker stop --time 20 medalert
docker rm medalert
docker run -d \
  --name medalert \
  --restart unless-stopped \
  --platform linux/amd64 \
  --mount "$data_mount" \
  --mount "$password_mount" \
  --mount "$token_mount" \
  "$known_good_image"
```

If the new image needs a newer database schema, an older image may reject it.
Use the Docker recovery steps below. Do not delete `medalert-data`.

## Recover a Docker volume after a failed migration

Stop the container and keep its named volume. Use any MedAlert image as a
temporary root helper to restore the newest private migration backup:

```sh
known_good_commit='<full-40-character-commit>'
recovery_image="ghcr.io/poppyseedcake/medalert:sha-$known_good_commit"
data_mount='type=volume,src=medalert-data,dst=/var/lib/medalert'
docker pull --platform linux/amd64 "$recovery_image"
docker stop --time 20 medalert || true
docker rm medalert || true
docker run --rm \
  --platform linux/amd64 \
  --user 0:0 \
  --mount "$data_mount" \
  --entrypoint /bin/sh \
  "$recovery_image" \
  -c 'set -eu
data_dir=/var/lib/medalert
backup=$(ls -1t "$data_dir"/backups/medalert-schema-*.sqlite3 2>/dev/null | head -n 1)
if [ -z "$backup" ] || [ ! -r "$backup" ]; then
  printf "%s\\n" "no readable migration backup found" >&2
  exit 1
fi
mv "$data_dir/medalert.db" "$data_dir/medalert.db.failed"
install -m 0600 "$backup" "$data_dir/medalert.db"
chown 10001:10001 "$data_dir/medalert.db"
chmod 0700 "$data_dir"'
```

Keep `medalert.db.failed` until recovery is complete. Run the final container
command from [Roll back safely](#roll-back-safely) with the known-good image,
the same `medalert-data` volume, and the same secret mounts.
