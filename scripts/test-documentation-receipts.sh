#!/usr/bin/env bash
set -euo pipefail

: "${ONWARDPG_TEST_DATABASE_URL:?set ONWARDPG_TEST_DATABASE_URL to a disposable PostgreSQL administrative database}"

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
receipt_root="$repository_root/docs/receipts/required-column"
fixture=$(mktemp -d)
binary="$fixture/onwardpg"
trap 'rm -rf "$fixture"' EXIT

go build -trimpath -o "$binary" "$repository_root/cmd/onwardpg"
cp "$receipt_root/config.toml" "$fixture/.onwardpg.toml"
cp "$receipt_root/base.sql" "$fixture/schema.sql"

cd "$fixture"
"$binary" init --bundle baseline >init.json
cp "$receipt_root/desired.sql" schema.sql

set +e
"$binary" plan add-booking-status >questions.json
questions_exit=$?
set -e
test "$questions_exit" -eq 2
grep -Eq '"status"[[:space:]]*:[[:space:]]*"needs_input"' questions.json
grep -q '"choices":\["assert_only","manual_sql","split_plan"\]' questions.json

set +e
"$binary" plan add-booking-status \
  --hint '{"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"}' \
  --hint '{"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]}' \
  >plan.json
plan_exit=$?
set -e
test "$plan_exit" -eq 2

if [[ "${ONWARDPG_DOC_RECEIPTS_PRINT:-}" == "1" ]]; then
  printf '%s\n' '--- questions.json ---'
  sed -n '1,200p' questions.json
  printf '%s\n' '--- plan.json ---'
  sed -n '1,240p' plan.json
fi

grep -Eq '"status"[[:space:]]*:[[:space:]]*"needs_sql_edits"' plan.json
grep -Eq '"edit_files"[[:space:]]*:[[:space:]]*\["phases/contract.sql"\]' plan.json

set +e
"$binary" verify add-booking-status >positional-verify.json
positional_verify_exit=$?
set -e
test "$positional_verify_exit" -eq 1
grep -q '"code":"invalid_invocation"' positional-verify.json
grep -q 'verify does not accept positional arguments' positional-verify.json

bundle_root="migrations/onward/app/add-booking-status"
expand="$bundle_root/phases/expand.sql"
contract="$bundle_root/phases/contract.sql"

test -f "$expand"
test -f "$contract"
grep -q 'ADD COLUMN "status" text;' "$expand"
if grep -q 'ADD COLUMN "status" text NOT NULL' "$expand"; then
  echo "required-column expand unexpectedly enforces NOT NULL" >&2
  exit 1
fi
if grep -q 'UPDATE .*bookings' "$expand"; then
  echo "required-column backfill unexpectedly runs while legacy writers remain" >&2
  exit 1
fi
grep -q 'ONWARDPG TODO: provide reconcile_contract_sql SQL for app.bookings.status' "$contract"
grep -q 'SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL);' "$contract"
grep -q 'onwardpg contract gate failed:' "$contract"
grep -q 'ALTER COLUMN "status" SET NOT NULL;' "$contract"

todo_line=$(grep -n 'ONWARDPG TODO' "$contract" | head -1 | cut -d: -f1)
assertion_line=$(grep -n 'onwardpg contract gate failed:' "$contract" | cut -d: -f1)
enforcement_line=$(grep -n 'SET NOT NULL' "$contract" | cut -d: -f1)
test "$todo_line" -lt "$enforcement_line"
test "$assertion_line" -lt "$enforcement_line"

if [[ "${ONWARDPG_DOC_RECEIPTS_PRINT:-}" == "1" ]]; then
  printf '%s\n' '--- bundle files ---'
  find "$bundle_root" -type f | sort
  printf '%s\n' '--- expand.sql ---'
  sed -n '1,240p' "$expand"
  printf '%s\n' '--- contract.sql ---'
  sed -n '1,260p' "$contract"
  exit 0
fi

diff -u "$receipt_root/expand.sql" "$expand"
diff -u "$receipt_root/contract.generated.sql" "$contract"

cp "$receipt_root/contract.edited.sql" "$contract"
cp "$receipt_root/verify.sql" "$bundle_root/verify.sql"

"$binary" verify >verify.json
grep -Eq '"status"[[:space:]]*:[[:space:]]*"verified"' verify.json || {
  sed -n '1,240p' verify.json >&2
  exit 1
}
grep -Eq '"receipts_updated"[[:space:]]*:[[:space:]]*true' verify.json
grep -q 'booking_status_present' verify.json

"$binary" verify --check >verify-check.json
grep -Eq '"status"[[:space:]]*:[[:space:]]*"verified"' verify-check.json

cd "$repository_root"
ONWARDPG_TEST_DATABASE_URL="$ONWARDPG_TEST_DATABASE_URL" \
  go test "$repository_root/internal/graphplan" \
  -run '^(TestRequiredColumnStagingConvergesOnPostgreSQL|TestViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL|TestMaterializedViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL)$' \
  -count=1

type_receipt_root="$repository_root/docs/receipts/type-change"
type_fixture="$fixture/type-change"
mkdir -p "$type_fixture"
cp "$receipt_root/config.toml" "$type_fixture/.onwardpg.toml"
cp "$type_receipt_root/base.sql" "$type_fixture/schema.sql"

cd "$type_fixture"
"$binary" init --bundle baseline >init.json
cp "$type_receipt_root/desired.sql" schema.sql

set +e
"$binary" plan account-age \
  --hint '{"kind":"type_change","name":["app","accounts","age"],"strategy":"manual_sql"}' \
  >plan.json
type_plan_exit=$?
set -e
test "$type_plan_exit" -eq 2
grep -Eq '"status"[[:space:]]*:[[:space:]]*"needs_sql_edits"' plan.json
grep -Eq '"edit_files"[[:space:]]*:[[:space:]]*\["phases/contract.sql","phases/expand.sql"\]' plan.json

type_bundle_root="migrations/onward/app/account-age"
type_expand="$type_bundle_root/phases/expand.sql"
type_contract="$type_bundle_root/phases/contract.sql"
grep -q 'ONWARDPG TODO: replace this comment with reviewed EXPAND SQL' "$type_expand"
grep -q 'Do not use a direct ALTER TYPE here' "$type_expand"
grep -q 'ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL' "$type_contract"
grep -q 'ANALYZE "app"."accounts" ("age");' "$type_contract"

if [[ "${ONWARDPG_DOC_RECEIPTS_PRINT_TYPE:-}" == "1" ]]; then
  printf '%s\n' '--- type-change expand.sql ---'
  sed -n '1,260p' "$type_expand"
  printf '%s\n' '--- type-change contract.sql ---'
  sed -n '1,280p' "$type_contract"
  exit 0
fi

diff -u "$type_receipt_root/expand.generated.sql" "$type_expand"
diff -u "$type_receipt_root/contract.generated.sql" "$type_contract"

rename_receipt_root="$repository_root/docs/receipts/rename"
rename_fixture="$fixture/rename"
mkdir -p "$rename_fixture"
cp "$receipt_root/config.toml" "$rename_fixture/.onwardpg.toml"
cp "$rename_receipt_root/base.sql" "$rename_fixture/schema.sql"

cd "$rename_fixture"
"$binary" init --bundle baseline >init.json
cp "$rename_receipt_root/desired.sql" schema.sql

"$binary" plan rename-display-name \
  --hint '{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}' \
  --hint '{"kind":"rename_backfill","name":["app","accounts","display_name"],"strategy":"single_transaction"}' \
  >plan.json
grep -Eq '"status"[[:space:]]*:[[:space:]]*"planned"' plan.json

rename_bundle_root="migrations/onward/app/rename-display-name"
rename_expand="$rename_bundle_root/phases/expand.sql"
rename_contract="$rename_bundle_root/phases/contract.sql"

if [[ "${ONWARDPG_DOC_RECEIPTS_PRINT_RENAME:-}" == "1" ]]; then
  printf '%s\n' '--- rename expand.sql ---'
  sed -n '1,360p' "$rename_expand"
  printf '%s\n' '--- rename contract.sql ---'
  sed -n '1,360p' "$rename_contract"
  exit 0
fi

grep -q 'CREATE TRIGGER' "$rename_expand"
grep -q 'UPDATE "app"."accounts"' "$rename_expand"
grep -q 'RENAME COLUMN "display_name" TO "full_name";' "$rename_contract"

diff -u "$rename_receipt_root/expand.generated.sql" "$rename_expand"
diff -u "$rename_receipt_root/contract.generated.sql" "$rename_contract"

"$binary" verify >verify.json
grep -Eq '"status"[[:space:]]*:[[:space:]]*"verified"' verify.json

dependency_receipt_root="$repository_root/docs/receipts/dependency-type-change"
dependency_fixture="$fixture/dependency-type-change"
mkdir -p "$dependency_fixture"
cp "$receipt_root/config.toml" "$dependency_fixture/.onwardpg.toml"
cp "$dependency_receipt_root/base.sql" "$dependency_fixture/schema.sql"

cd "$dependency_fixture"
if ! "$binary" init --bundle baseline --concurrent-indexes >init.json; then
  sed -n '1,240p' init.json >&2
  exit 1
fi
cp "$dependency_receipt_root/desired.sql" schema.sql

set +e
"$binary" plan facts-to-bigint --concurrent-indexes \
  --hint '{"kind":"type_change","name":["app","facts","val"],"strategy":"manual_sql"}' \
  >plan.json
dependency_plan_exit=$?
set -e
test "$dependency_plan_exit" -eq 2
grep -Eq '"status"[[:space:]]*:[[:space:]]*"needs_sql_edits"' plan.json

dependency_bundle_root="migrations/onward/app/facts-to-bigint"
dependency_expand="$dependency_bundle_root/phases/expand.sql"
dependency_contract="$dependency_bundle_root/phases/contract.sql"
grep -q 'Dependent view/materialized-view/index objects in scope:' "$dependency_expand"
grep -q 'DROP MATERIALIZED VIEW "app"."fact_cache";' "$dependency_contract"
grep -q 'DROP VIEW "app"."fact_view";' "$dependency_contract"
grep -q 'ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL' "$dependency_contract"
grep -q 'CREATE VIEW "app"."fact_view"' "$dependency_contract"
grep -q 'CREATE MATERIALIZED VIEW "app"."fact_cache"' "$dependency_contract"
grep -q 'CREATE UNIQUE INDEX CONCURRENTLY "fact_cache_val_idx"' "$dependency_contract"

drop_materialized_line=$(grep -n 'DROP MATERIALIZED VIEW "app"."fact_cache";' "$dependency_contract" | cut -d: -f1)
drop_view_line=$(grep -n 'DROP VIEW "app"."fact_view";' "$dependency_contract" | cut -d: -f1)
conversion_line=$(grep -n 'ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL' "$dependency_contract" | cut -d: -f1)
create_view_line=$(grep -n 'CREATE VIEW "app"."fact_view"' "$dependency_contract" | cut -d: -f1)
create_materialized_line=$(grep -n 'CREATE MATERIALIZED VIEW "app"."fact_cache"' "$dependency_contract" | cut -d: -f1)
create_index_line=$(grep -n 'CREATE UNIQUE INDEX CONCURRENTLY "fact_cache_val_idx"' "$dependency_contract" | cut -d: -f1)
test "$drop_materialized_line" -lt "$drop_view_line"
test "$drop_view_line" -lt "$conversion_line"
test "$conversion_line" -lt "$create_view_line"
test "$create_view_line" -lt "$create_materialized_line"
test "$create_materialized_line" -lt "$create_index_line"

if [[ "${ONWARDPG_DOC_RECEIPTS_PRINT_DEPENDENCY:-}" == "1" ]]; then
  printf '%s\n' '--- dependency type-change expand.sql ---'
  sed -n '1,300p' "$dependency_expand"
  printf '%s\n' '--- dependency type-change contract.sql ---'
  sed -n '1,420p' "$dependency_contract"
  exit 0
fi

diff -u "$dependency_receipt_root/expand.generated.sql" "$dependency_expand"
diff -u "$dependency_receipt_root/contract.generated.sql" "$dependency_contract"

echo "Documentation receipts passed"
