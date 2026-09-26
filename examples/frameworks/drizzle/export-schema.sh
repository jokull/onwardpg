#!/usr/bin/env bash
set -euo pipefail

# For pgSchema() models, prepend CREATE SCHEMA statements here if your
# drizzle-kit version omits them. Public-schema models need no preamble.
# For example: printf 'CREATE SCHEMA "app";\n'
exec npx --no-install drizzle-kit export --sql=true "$@"
