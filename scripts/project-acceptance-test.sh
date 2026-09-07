#!/bin/sh

set -eu

repository_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
readme="$repository_dir/README.md"
workflow="$repository_dir/.github/workflows/ci.yml"
native_docs="$repository_dir/docs/install.md"
container_docs="$repository_dir/docs/docker.md"
license="$repository_dir/LICENSE"

fail() {
	printf 'project acceptance check failed: %s\n' "$1" >&2
	exit 1
}

contains() {
	file=$1
	text=$2
	grep -Fq -- "$text" "$file" || fail "$file does not contain: $text"
}

[ -f "$license" ] || fail 'LICENSE is missing'

contains "$license" 'GNU GENERAL PUBLIC LICENSE'
contains "$license" 'Version 3, 29 June 2007'
contains "$readme" 'GNU General Public'
contains "$readme" 'License v3.0'
contains "$readme" 'MediCzuwacz'
contains "$readme" 'https://github.com/SteveSteve24/MediCzuwacz'
contains "$readme" 'Linux amd64'

contains "$workflow" 'go test ./...'
contains "$workflow" 'go vet ./...'
contains "$workflow" 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
contains "$workflow" 'platforms: linux/amd64'
contains "$workflow" 'needs: verify'
contains "$workflow" 'ghcr.io/poppyseedcake/medalert'
contains "$workflow" 'sha-$COMMIT'
contains "$workflow" 'provenance: false'
contains "$workflow" 'sbom: false'

if grep -Eiq 'arm64|aarch64' "$workflow"; then
	fail 'the CI workflow mentions an unsupported arm64 build or test'
fi
if git -C "$repository_dir" grep -I -n -E 'actions/(create-release|github-release)|softprops/action-gh-release|gh[[:space:]]+release|goreleaser|cosign|syft|attest(ation)?s?[[:space:]:=]|release[-_ ]candidate|git tag -(s|u|a)|sha256sum|checksums?\.txt|^[[:space:]]*sbom:[[:space:]]*true' -- . ':!LICENSE' ':!README.md' ':!docs/**' ':!scripts/project-acceptance-test.sh' >/dev/null 2>&1; then
	fail 'the repository contains a release, attestation, SBOM, or checksum publication path'
fi
if git -C "$repository_dir" ls-files | grep -E '(^|/)(release|releases|dist|artifacts?)/|(^|/)[^/]*(checksum|sha256)[^/]*\.(txt|json|yaml|yml)$|\.(zip|tar\.gz|tgz)$' >/dev/null 2>&1; then
	fail 'the repository contains a release archive or checksum artifact'
fi

for text in 'go install' 'systemd' 'backup' 'recovery' 'Secret Service' 'journal' 'Linux amd64'; do
	contains "$native_docs" "$text"
done
for text in 'persistent' 'secret' 'rollback' 'docker logs' 'Linux amd64'; do
	contains "$container_docs" "$text"
done

(cd "$repository_dir" && go test ./cmd/medalert -count=1 -run '^(TestFinalAcceptanceSecretMarkersStayOutOfOutputsAndHistory|TestTerminalAccountKeyboardSmoke|TestWatchStreamsTextLogsOnStderrAndJSONLinesOnStdout)$')
printf '%s\n' 'project acceptance test passed'
