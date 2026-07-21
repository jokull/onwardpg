#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/repository"
rsync -a \
  --exclude '.git/' \
  --exclude 'node_modules/' \
  --exclude 'website/node_modules/' \
  --exclude 'website/dist/' \
  --exclude 'website/.blume/' \
  "$repository_root/" "$fixture/repository/"

corepack enable
pnpm --dir "$fixture/repository/website" install --frozen-lockfile
pnpm --dir "$fixture/repository/website" audit --audit-level high
pnpm --dir "$fixture/repository/website" check
pnpm --dir "$fixture/repository/website" validate
pnpm --dir "$fixture/repository/website" build
pnpm --dir "$fixture/repository/website" audit:site
pnpm --dir "$fixture/repository/website" check:agent-docs

echo "Clean-room website workflow passed"
