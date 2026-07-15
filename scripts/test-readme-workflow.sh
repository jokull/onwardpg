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

[targets.app]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
dev_mode = "workspace"
EOF

cat >"$fixture/schema.sql" <<'EOF'
CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint,
  display_name text
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

"$binary" init --bundle baseline >init.json
grep -q '"status":"initialized"' init.json

cat >schema.sql <<'EOF'
CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint,
  full_name text
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
"$binary" plan customer-profile >decisions.json
decision_exit=$?
set -e
test "$decision_exit" -eq 2
grep -q '"status":"needs_input"' decisions.json
if ! grep -q '"kind":"rename"' decisions.json; then
  echo "plan did not offer the expected rename decision" >&2
  sed -n '1,160p' decisions.json >&2
  exit 1
fi

set +e
"$binary" plan \
  --hint '{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}' \
  >planned.json
planned_exit=$?
set -e
test "$planned_exit" -eq 0
grep -q '"status":"planned"' planned.json

"$binary" verify --through expand >expand-verify.json
grep -q '"status":"partial_verified"' expand-verify.json
grep -q '"remaining_bundle_phases":\["contract"\]' expand-verify.json

"$binary" verify >verify.json
grep -q '"status":"verified"' verify.json

"$binary" verify --check >check.json
grep -q '"status":"verified"' check.json

"$binary" status >status.json
grep -q '"plan_id":"plan_' status.json

"$binary" plan --output sql >dev-plan.sql
grep -q 'development workspace reconciliation' dev-plan.sql
grep -q 'CREATE TABLE' dev-plan.sql

test ! -e .git
test -f migrations/onward/app/customer-profile/manifest.json
grep -q 'CREATE TRIGGER' migrations/onward/app/customer-profile/phases/expand.sql
grep -q 'RENAME COLUMN "display_name" TO "full_name"' migrations/onward/app/customer-profile/phases/contract.sql

echo "README workflow passed"
