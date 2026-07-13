#!/usr/bin/env bash
set -euo pipefail

: "${ONWARDPG_TEST_DATABASE_URL:?set ONWARDPG_TEST_DATABASE_URL to a disposable PostgreSQL administrative database}"

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
binary="$fixture/onwardpg"
trap 'rm -rf "$fixture"' EXIT

go build -trimpath -o "$binary" "$repository_root/cmd/onwardpg"

cat >"$fixture/.onwardpg.toml" <<'EOF'
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
EOF

cat >"$fixture/schema.sql" <<'EOF'
CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint,
  occurred_at timestamp
);
EOF

cd "$fixture"

"$binary" config check >config-check.json
grep -q '"status":"valid"' config-check.json
grep -q '"fingerprint":"sha256:' config-check.json

"$binary" init --target primary --bundle baseline >init.json
grep -q '"outcome":"initialized"' init.json

"$binary" dev plan --target primary --output text >dev-plan.sql
grep -q 'EXPAND' dev-plan.sql

cat >schema.sql <<'EOF'
CREATE SCHEMA app;
CREATE TABLE app.customers (
  id bigint,
  occurred_at date
);
EOF

set +e
"$binary" draft --target primary --bundle customer-profile >decisions.json
decision_exit=$?
set -e
test "$decision_exit" -eq 2
grep -q '"status":"needs_decisions"' decisions.json
grep -q '"kind":"rename"' decisions.json

set +e
"$binary" draft \
  --target primary \
  --bundle customer-profile \
  --hint '{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}' \
  --hint '{"kind":"type_change","name":["app","accounts","occurred_at"],"strategy":"manual_sql"}' \
  >sql-handoff.json
handoff_exit=$?
set -e
test "$handoff_exit" -eq 2
grep -q '"status":"needs_sql_edits"' sql-handoff.json

cat >migrations/onward/primary/customer-profile/phases/migrate.sql <<'EOF'
-- Product-aware conversion supplied by the coding agent.
ALTER TABLE "app"."accounts"
  ALTER COLUMN "occurred_at" TYPE date
  USING "occurred_at"::date;
EOF

cat >migrations/onward/primary/customer-profile/verify.sql <<'EOF'
-- onwardpg:assert occurred_at_is_date
SELECT data_type = 'date'
FROM information_schema.columns
WHERE table_schema = 'app'
  AND table_name = 'customers'
  AND column_name = 'occurred_at';
EOF

"$binary" verify --target primary --bundle customer-profile >verify.json
grep -q '"outcome":"verified"' verify.json
grep -q '"receipts_updated":true' verify.json

"$binary" verify --target primary --bundle customer-profile --check >check.json
grep -q '"outcome":"verified"' check.json

test ! -e .git
test -f migrations/onward/primary/customer-profile/manifest.json
grep -q 'ALTER TABLE "app"."accounts"' migrations/onward/primary/customer-profile/phases/migrate.sql

echo "README workflow passed"
