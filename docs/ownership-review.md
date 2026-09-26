# Ownership review — 2026-09-26

Reviewed baseline commit `2d35e02` and the fixes accompanying this report.
This is a source review and an executable test drive, not an independent audit
or proof that every migration is safe under production load.

## Is this useful?

Yes, for applications whose old and new versions overlap against one database.
The useful combination is phased SQL, explicit decisions where schema state
cannot determine intent, editable migration bundles, and a stable feature plan
that can be revised or restacked as accepted history changes. Keeping local
development reconciliation separate from durable migration history also solves
a real branch-switching problem.

The design has credible safeguards: restricted disposable database owners,
content-bound receipts, explicit unsupported states, and no CLI command that
applies migration phases to production. The column-rename bridge refuses cases
with defaults, existing triggers, partitions, or RLS that would require more
write-precedence analysis.

The appropriate confidence level is **useful developer preview with reviewed
production deployment**, not unattended migration automation. Converging on an
empty clone does not establish data preservation or availability on a busy,
populated database. Passing the original tests did not establish the assertion
boundary either: every finding below was missed by the baseline suite.

## Reproduced findings and fixes

Severity reflects this project's safety promises and the prerequisites below;
it is not a CVSS score. The SQL examples ran only in disposable databases.

### High: a read-only gate could commit a write

When a caller configured pgx's simple protocol, a gate beginning with `SELECT`
could contain additional statements:

```sql
SELECT true; COMMIT; UPDATE audit_state SET value = 1;
```

The gate returned true and the update persisted outside its read-only
transaction. The role needed write privileges, which the permitted database-owner
observer can have. This affected the contract gate executor and the analogous
opt-in development postcondition path. It was not a remote unauthenticated
attack: it required supplied assertion SQL and this connection mode.

**Fixed:** assertion execution forces extended protocol per query, so PostgreSQL
rejects statement lists regardless of the connection default. Each assertion
runs inside a read-only transaction or savepoint and always rolls it back.
Regressions exercise both connection modes and verify that persistent data did
not change.

### High: clone assertions could write or ignore failing rows

Manual verification queries and edited `verify.sql` assertions used `QueryRow`
without a read-only transaction. A data-modifying CTE could change the state it
was meant to check. `SELECT true UNION ALL SELECT false` also passed because only
the first row was consumed. This could produce invalid verification evidence.

**Fixed:** one shared `internal/sqlcheck` executor enforces read-only execution
and exactly one non-null PostgreSQL Boolean column and row. It supports checks
of uncommitted migration work through a savepoint, then restores the surrounding
transaction's settings. Regressions cover writes, extra rows/columns, missing
rows, NULL, non-Boolean values, access-mode changes, and continued use of the
parent transaction after a rejected assertion.

### High: forced RLS could produce false contract readiness

The database-owner observer bypassed the dedicated observer's RLS rejection.
With `FORCE ROW LEVEL SECURITY`, a policy could hide a row violating a data gate.
`NOT EXISTS (...)` then returned true without examining that row.

**Fixed:** assertions set `row_security = off`. PostgreSQL errors if RLS would
filter the query; this setting does not grant permission to bypass policies.
The regression uses a restricted database owner and an actual hidden NULL row.

### Medium: transactional batches could end or replace their transaction

Verification accepted both `SELECT 1; COMMIT;` and
`SELECT 1; COMMIT; BEGIN;` inside a batch labeled transactional. A successful
final `Commit` call did not prove the batch was atomic.

**Fixed:** verification checks the backend transaction status and assigned
transaction ID before accepting transactional execution. Non-transactional
batches must leave the connection idle. Regression cases cover explicit commit,
rollback/restart, unfinished transactions, and rollback of a normally failing
batch. Detection rejects verification evidence; it cannot undo SQL that already
committed in the disposable database.

### Dependency vulnerabilities

The initial `govulncheck` reported four reachable advisories: GO-2026-6090,
GO-2026-6088, GO-2026-5972, and GO-2026-5970, affecting the Go standard library and
`golang.org/x/text`. Updated the toolchain from Go 1.26.5 to 1.26.8 and `x/text`
from 0.29.0 to 0.42.0. The same pinned scan then reported no vulnerabilities.
Reachability is scanner evidence, not proof that each advisory was exploitable
through this CLI. Existing binaries must be rebuilt to receive these fixes.

## Initial test drive and validation

A standalone compiled-CLI run on PostgreSQL 18 created a baseline and a
same-type column rename. Planning first asked for identity/backfill intent; the
live development catalog required its own `--dev-hint`. After verification,
expand and contract were applied only to the disposable workload database.

Between phases, two real client connections performed 2,000 concurrent updates
through the old and new column names across 100 seeded rows. The bridge kept
both values equal and preserved every counter increment. NULL updates and
inserts through either name worked, conflicting dual-name inserts failed, and
contract retained all 102 rows and all increments. The final CLI drift check
reported `drift_free`; no bridge trigger remained.

| Validation | Result |
| --- | --- |
| Baseline unit/race tests and PostgreSQL 18 native suite | Passed before fixes |
| Baseline compiled-CLI acceptance | 10 authoritative tests passed, none skipped |
| New assertion and transaction regressions, PostgreSQL 18 | Failed before fixes; passed after fixes |
| Fixed unit/race suite, `go vet`, staticcheck | Passed |
| Graph, identifier, and decision fuzz targets | Passed, 10 seconds each; about 687,000 executions total |
| Fixed `govulncheck@v1.6.0` | No vulnerabilities found |
| Atlas/Stripe differential comparisons, PostgreSQL 15–18 | Passed |
| Native suite, PostgreSQL 15–18 | Passed |
| Additional CLI assertion/cleanup regressions, PostgreSQL 15–18 | Passed |
| Fixed compiled-CLI release acceptance, PostgreSQL 15–18 | 10 authoritative tests per version passed, none skipped |
| Scoped PGlite preflight | Passed |
| Documentation invariants, Go formatting, whitespace checks | Passed |

Native runs used isolated Docker PostgreSQL 15.18, 16.14, 17.10, and 18.4
clusters. The differential suite used the repository's pinned Atlas and Stripe
executables. No existing application database was accessed.

The durable regression tests are in:

- `internal/sqlcheck/boolean_integration_test.go`
- `internal/contractcheck/audit_integration_test.go`
- `internal/verify/audit_integration_test.go`
- `internal/devflow/devflow_test.go`
- `cmd/onwardpg/main_integration_test.go`

The new native tests require `ONWARDPG_TEST_DATABASE_URL` pointing to a disposable
administrative PostgreSQL cluster; they skip without it. The normal unit/race
command alone does not exercise this database boundary. The repository's native
CI matrix supplies the variable.

Local command logs, before/after reproductions, the standalone CLI driver, and
its generated migration workspace are retained in
`/tmp/onwardpg-ownership-review`. They are temporary investigation artifacts,
not files required by the library or its test suite. The test clusters were
removed after validation. Before removal, the pinned Stripe reference had left
temporary databases (two per cluster, three on PostgreSQL 18) and one dependent
test role. No onwardpg-owned disposable database remained. Reference-process
cleanup was addressed in the release follow-up below.

## Consolidated release follow-up

The release review assigned adversarial assertion work to GPT-6 Astra at high
effort, resource and cleanup implementations to GPT-6 Sol at high effort, and
the bounded reference-test cleanup to GPT-6 Sol at medium effort. The coordinator
reviews integration and release evidence separately from worker claims.

The independent assertion pass reproduced two additional problems:

- A single transaction command is valid extended-protocol SQL. Checking its
  result afterward is too late. Parse/Describe now verifies the Boolean result
  shape before executing the statement.
- A function can temporarily enable RLS and return a misleading result while
  restoring the caller's setting. Assertions now reject any database containing
  a table whose RLS applies to the current role, including unrelated or unmanaged
  tables. Ordinary owner-bypassed, non-forced RLS remains supported.

The resource follow-up adds bounded schema reads, streaming checkout hashes,
exporter deadlines and live size monitoring, and process containment. The monitor
is not a disk quota; see [exporter limits](exporter-limits.md). Scratch cleanup
now has bounded operations, reconnects after connection loss, retains cleanup
errors alongside the original failure, and supports exact-resource retries.
See [scratch recovery](scratch-recovery.md).

Release publication now depends on the full CI workflow, including PostgreSQL
15–18 integration/differential and compiled-CLI acceptance. Preview tags remain
prereleases and take their notes from a reviewed changelog section.

The reference harness now gives each pinned Stripe invocation its own temporary
role and cleans databases by that exact role's OID. A fresh cleanup connection
handles canceled setup. Regressions preserve unrelated resources and require
cleanup failures to surface; the PostgreSQL 18 rerun left no temporary resources.

The release adds mandatory compiled-CLI rehearsals for populated rename overlap
and blocked-DDL cancellation/retry. Two prepared clients make 2,048 updates to
the same 64 rows, preserving all 33 increments per row across both workload
stages. See [operational rehearsals](operational-rehearsals.md) for the scope.

Combined local validation passed the unit/race suite, vet, staticcheck,
vulnerability scan, PostgreSQL 18 assertion/cleanup suites, README and
documentation receipts, PGlite preflight, and the clean-room website build.
All 12 required compiled-CLI acceptance tests passed on PostgreSQL 18 without
skips; the two operational rehearsals also passed under the race detector.
The new Windows containment tests require a native Windows runner; cross-builds
alone are insufficient. Native Windows and the full PostgreSQL 15–18 matrices
are mandatory CI gates before publication. Release workflow results provide the
final record for the tagged commit. Local integration logs are retained in
`/tmp/onwardpg-release-preview3`.

## Remaining risks and ownership priorities

1. **Deployment-scale evidence.** CI rehearsals protect specific concurrency,
   cancellation, and retry invariants. They do not establish production-scale
   availability or replication-lag behavior. Prepared statements using
   `SELECT *`, positional inserts, application caches, and application-specific
   invariants require their own compatibility tests.
2. **External rollout evidence.** Writer attestations are supplied evidence,
   not observations that the CLI independently verifies with deployment
   providers. Readiness is a snapshot that can become stale. The deployer must
   actually drain old writers and retain inline enforcement during contract.
3. **Trusted project code.** Exporter monitoring and SQL read-only transactions
   are resource/safety controls, not an operating-system sandbox. Deliberately
   detached processes, functions changing identity, and external effects still
   require review and suitable execution isolation.
4. **Process death.** A killed CLI cannot complete cleanup. Exact-resource
   recovery remains necessary if the process or scratch cluster disappears.
5. **Maintainability of safety rules.** The large planner combines many object
   lifecycles and phase rules. Extract behavior by migration family only with
   the native compatibility tests retained. The assertion bugs show why a
   safety invariant should have one implementation shared across entry points.

Prioritize production-scale rehearsals and failure recovery before adding more
automatically supported migration families. The architecture is worth
continuing; these fixes narrow specific risks rather than granting a general
“safe migration” guarantee.
