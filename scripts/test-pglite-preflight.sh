#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repository_root"

ONWARDPG_PGLITE=1 go test ./acceptance -run '^TestPGlite' -count=1 -timeout=5m "$@"
