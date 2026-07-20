#!/usr/bin/env bash
set -euo pipefail

output=$(mktemp)
trap 'rm -f "$output"' EXIT

pnpm exec prisma migrate diff \
  --from-empty \
  --to-schema ./prisma/schema.prisma \
  --script >"$output"

if ! grep -q '[^[:space:]]' "$output"; then
  echo "Prisma schema export produced empty stdout; check prisma.config.ts, DATABASE_URL, and Prisma engine installation" >&2
  exit 1
fi

cat "$output"
