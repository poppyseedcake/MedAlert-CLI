# MedAlert

MedAlert monitors Medicover appointment availability and sends Telegram
notifications. The native installation supports Linux amd64 only.

## Native installation

Install the current source from the default branch:

```sh
install -d -m 0755 "$HOME/.local/bin"
GOBIN="$HOME/.local/bin" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go install github.com/poppyseedcake/MedAlert/cmd/medalert@main
```

For the systemd user service, data paths, updates, backups, and recovery, see
[`docs/install.md`](docs/install.md).

Run the installation smoke test from a local checkout:

```sh
./scripts/install-smoke-test.sh
```

The service unit is [`systemd/medalert.service`](systemd/medalert.service).

## Docker installation

Builds for `main` publish the Linux amd64 image at
`ghcr.io/poppyseedcake/medalert`. The image runs as a non-root user, stores
SQLite data and Medicover Session State in `/var/lib/medalert`, reads mounted
secret files, and starts `watch --non-interactive` by default.

See [`docs/docker.md`](docs/docker.md) for secret mounts, persistent data,
commit image tags, updates, and rollback.
