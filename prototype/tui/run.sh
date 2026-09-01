#!/bin/sh
set -eu

cd "$(dirname "$0")/../.."
printf '%s\n' 'MedAlert TUI prototype: http://127.0.0.1:4173'
exec python3 -m http.server 4173 --bind 0.0.0.0 --directory prototype/tui
