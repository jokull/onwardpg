#!/usr/bin/env bash
set -euo pipefail

: "${ONWARDPG_DJANGO_ADMIN_URL:?set ONWARDPG_DJANGO_ADMIN_URL to a disposable PostgreSQL administrative database}"

database_name="onwardpg_django_$(python -c 'import secrets; print(secrets.token_hex(12))')"
database_url=$(python - "$ONWARDPG_DJANGO_ADMIN_URL" "$database_name" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit

source = urlsplit(sys.argv[1])
print(urlunsplit((source.scheme, source.netloc, "/" + sys.argv[2], source.query, source.fragment)))
PY
)

cleanup() {
  psql "$ONWARDPG_DJANGO_ADMIN_URL" -X --set=ON_ERROR_STOP=1 \
    --command="DROP DATABASE IF EXISTS \"$database_name\" WITH (FORCE)" >/dev/null
}
trap cleanup EXIT

psql "$ONWARDPG_DJANGO_ADMIN_URL" -X --set=ON_ERROR_STOP=1 \
  --command="CREATE DATABASE \"$database_name\"" >/dev/null

server_major=$(psql "$database_url" -X --tuples-only --no-align \
  --command="SELECT current_setting('server_version_num')::integer / 10000")
dump_major=$(pg_dump --version | sed -E 's/.* ([0-9]+)(\..*)?$/\1/')
if [[ "$server_major" != "$dump_major" ]]; then
  echo "pg_dump major $dump_major does not match disposable PostgreSQL major $server_major" >&2
  exit 1
fi

export DJANGO_SCHEMA_DATABASE_URL="$database_url"
export PYTHONDONTWRITEBYTECODE=1

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
python manage.py shell < "$script_dir/export-current-model-state.py" >&2

pg_dump "$database_url" \
  --schema-only \
  --no-owner \
  --no-privileges \
  --exclude-table=django_migrations \
  | sed '/^\\restrict /d; /^\\unrestrict /d'
