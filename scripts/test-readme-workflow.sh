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
CREATE TABLE app.profile_kinds (
  id bigint PRIMARY KEY
);
EOF

cd "$fixture"

set +e
"$binary" apply >no-apply.json
apply_exit=$?
set -e
test "$apply_exit" -eq 1
grep -q '"code":"invalid_invocation"' no-apply.json
grep -q 'unknown command.*apply' no-apply.json

"$binary" config check >config-check.json
grep -q '"status":"valid"' config-check.json
grep -q '"fingerprint":"sha256:' config-check.json

"$binary" init --target primary --bundle baseline >init.json
grep -q '"status":"initialized"' init.json

"$binary" history status --target primary >history.json
grep -q '"status":"valid"' history.json
grep -q '"head_bundle":"baseline"' history.json
head_ref=$(jq -r .head_ref history.json)
test -n "$head_ref"

"$binary" dev plan --target primary --output text >dev-plan.sql
grep -q 'EXPAND' dev-plan.sql

cat >schema.sql <<'EOF'
CREATE SCHEMA app;
CREATE TABLE app.customers (
  id bigint,
  occurred_at date
);
CREATE TABLE app.profile_kinds (
  id bigint PRIMARY KEY
);
CREATE TABLE app.customer_profiles (
  id bigint PRIMARY KEY,
  kind_id bigint NOT NULL REFERENCES app.profile_kinds (id),
  biography text
);
CREATE INDEX customer_profiles_kind_id_idx
  ON app.customer_profiles (kind_id);
EOF

set +e
"$binary" draft --target primary --bundle customer-profile --after "$head_ref" --create >decisions.json
decision_exit=$?
set -e
test "$decision_exit" -eq 2
grep -q '"status":"needs_decisions"' decisions.json
if ! grep -q '"kind":"rename"' decisions.json; then
  echo "draft did not offer the expected rename decision" >&2
  sed -n '1,160p' decisions.json >&2
  exit 1
fi

set +e
"$binary" draft \
  --target primary \
  --bundle customer-profile \
  --after "$head_ref" \
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
grep -q '"status":"verified"' verify.json
grep -q '"receipts_updated":true' verify.json

"$binary" verify --target primary --bundle customer-profile --check >check.json
grep -q '"status":"verified"' check.json

test ! -e .git
test -f migrations/onward/primary/customer-profile/manifest.json
grep -q 'ALTER TABLE "app"."accounts"' migrations/onward/primary/customer-profile/phases/migrate.sql

echo "README workflow passed"
