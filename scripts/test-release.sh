#!/usr/bin/env bash
set -euo pipefail

: "${ONWARDPG_ACCEPTANCE_DATABASE_URL:?set ONWARDPG_ACCEPTANCE_DATABASE_URL to a disposable PostgreSQL administrative database}"

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repository_root"

go test -race ./...
scripts/test-pglite-preflight.sh
scripts/test-acceptance.sh
ONWARDPG_TEST_DATABASE_URL="$ONWARDPG_ACCEPTANCE_DATABASE_URL" scripts/test-readme-workflow.sh
ONWARDPG_TEST_DATABASE_URL="$ONWARDPG_ACCEPTANCE_DATABASE_URL" scripts/test-documentation-receipts.sh
scripts/test-website-clean-room.sh
git diff --check
