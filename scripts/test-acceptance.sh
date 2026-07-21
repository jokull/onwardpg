#!/usr/bin/env bash
set -euo pipefail

: "${ONWARDPG_ACCEPTANCE_DATABASE_URL:?set ONWARDPG_ACCEPTANCE_DATABASE_URL to a disposable PostgreSQL administrative database}"

repository_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
artifact_root=${ONWARDPG_ACCEPTANCE_ARTIFACT_DIR:-$(mktemp -d)}
mkdir -p "$artifact_root"

cd "$repository_root"
native_pattern='^(TestReleaseContract|TestNativeContractReadiness|TestPlanLoop|TestRendererParity|TestHarness)'
required_tests='TestReleaseContractNullableColumn,TestReleaseContractRequiredColumnWithReviewedCleanup,TestReleaseContractSameTypeRename,TestNativeContractReadinessRequiresEvidenceAndReconciliation,TestPlanLoopRestackMatrix,TestPlanLoopRestackRetainsSafeEditedPocket,TestPlanLoopDevelopmentAheadWorkIsNotAbsorbed,TestPlanLoopDevelopmentManualSQLHasAction,TestPlanLoopBranchSwitchRestoresParkedPlanIdentity,TestRendererParityRequiredColumnDecision'
listed_tests=$(go test ./acceptance -list "$native_pattern")
if ! grep -Eq '^Test(ReleaseContract|NativeContractReadiness|PlanLoop|RendererParity|Harness)' <<<"$listed_tests"; then
  echo "native acceptance selection matched no tests" >&2
  exit 1
fi
ONWARDPG_ACCEPTANCE=1 \
ONWARDPG_ACCEPTANCE_ARTIFACT_DIR="$artifact_root" \
go test -json ./acceptance -run "$native_pattern" -count=1 -timeout=20m "$@" 2>&1 | tee "$artifact_root/go-test.json"
go run ./scripts/acceptanceevents -log "$artifact_root/go-test.json" -require "$required_tests"
