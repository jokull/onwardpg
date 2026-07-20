#!/usr/bin/env bash
set -euo pipefail

# drizzle-kit export does not currently emit CREATE SCHEMA for pgSchema().
# List every named schema used by the model before the exported objects.
printf 'CREATE SCHEMA "app";\n'
pnpm exec drizzle-kit export --sql=true
