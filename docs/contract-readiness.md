# Contract readiness

`contract.sql` is allowed to tighten the deliberately loose overlap only after
two different facts are true:

1. rows admitted during overlap satisfy the desired invariant; and
2. every potential pre-deployment writer is upgraded, drained, isolated, or
   read-only.

These are not clone-verification facts. `onwardpg verify` proves exact bundle
replay on disposable PostgreSQL and receipts the catalog observed immediately
after expand. `onwardpg contract check` compares a caller database with that
checkpoint and evaluates current readiness in one `REPEATABLE READ, READ ONLY`
transaction. Neither command applies migration SQL to the caller database.

## What the planner generates

Consider replacing an old CHECK with a desired CHECK that accepts a different
set of values. When neither predicate implies the other, expand removes the old
constraint so both application releases can write:

```sql
ALTER TABLE "app"."delivery"
  DROP CONSTRAINT "delivery_tier_check";
```

Planning captures one scoped reconciliation decision:

- `assert_only`: no repair is expected, but the exact assertion must pass;
- `manual_sql`: receipt reviewed cleanup SQL and the required Boolean contract
  gate; or
- `split_plan`: retain the loose shape and restore enforcement in a later
  deployment.

For CHECK and foreign-key constraints, contract installs the desired constraint
`NOT VALID` first. That fences new desired-version writes without pretending
historical overlap rows are clean. Cleanup and the exact assertion follow, then
PostgreSQL validation restores the guarantee:

```sql
ALTER TABLE "app"."delivery"
  ADD CONSTRAINT "delivery_tier_check"
  CHECK (tier IN ('shared', 'new')) NOT VALID;

UPDATE "app"."delivery"
SET tier = 'new'
WHERE tier = 'legacy';

DO $onwardpg$
BEGIN
  IF NOT COALESCE((
    SELECT NOT EXISTS (
      SELECT 1 FROM "app"."delivery"
      WHERE (tier IN ('shared', 'new')) IS FALSE
    )
  ), false) THEN
    RAISE EXCEPTION 'onwardpg contract gate failed: data:...';
  END IF;
END
$onwardpg$;

ALTER TABLE "app"."delivery"
  VALIDATE CONSTRAINT "delivery_tier_check";
```

`IS FALSE` is deliberate: PostgreSQL CHECK constraints accept `TRUE` and
`NULL`, and reject only `FALSE`. NOT NULL uses an exact NULL probe. Foreign-key
gates use typed ordered column pairs and MATCH SIMPLE/FULL semantics. Supported
btree uniqueness uses exact key expressions, predicates, collations, and NULL
semantics; exclusion constraints and unproved operator classes require reviewed
verification.

When the catalog cannot derive that Boolean exactly, the generated assertion
is itself a bounded edit pocket in `contract.sql`. Edit the SQL there, not the
gate metadata. Successful `onwardpg verify` extracts the effective query into
verifier-written `contract-gate-overrides.json`, binds it to the generated gate
ID and history digest, and uses it for later `contract check`. Optional
`verify.sql` assertions are separate clone postconditions; they are not live
contract-readiness gates.

Unique and exclusion enforcement has no general `NOT VALID` form. Those plans
also require an explicit write-fence attestation so concurrent desired-version
writers cannot reopen a conflict between the probe and build.

## Check a live post-expand database

The bundle must be the selected target's history head and must contain the
post-expand checkpoint receipted by disposable verification:

```sh
export PROD_READONLY_DATABASE_URL='postgres://...'

onwardpg contract check \
  --target app \
  --bundle delivery-tier \
  --environment production \
  --database-env PROD_READONLY_DATABASE_URL \
  --evidence deploy-readiness.json \
  --statement-timeout 30s
```

The `onwardpg.contract-readiness/v1` result is `ready`, `needs_evidence`,
`blocked`, or `stale`. Catalog mismatch
distinguishes an unapplied expand baseline, an already-contracted desired graph,
and unrelated/partial drift. Data gates run in the same read-only snapshot.

Writer evidence is provider-neutral and bound to target, environment, PlanID,
desired fingerprint, bundle entry digest, generation, release, observation
time, and expiry. Observation windows longer than 24 hours are stale:

```json
{
  "protocol_version": "onwardpg.writer-evidence/v1",
  "target": "app",
  "environment": "production",
  "plan_id": "plan_0123456789abcdef0123456789abcdef",
  "bundle_entry_digest": "sha256:...",
  "desired_fingerprint": "sha256:...",
  "generation": 3,
  "release": "web-2026-07-20.1",
  "observed_at": "2026-07-20T12:00:00Z",
  "expires_at": "2026-07-20T12:20:00Z",
  "cohorts": [
    {
      "category": "web",
      "name": "application deployments",
      "status": "upgraded",
      "source_kind": "provider",
      "source": "release/web-2026-07-20.1"
    },
    {
      "category": "workers",
      "name": "background consumers",
      "status": "drained",
      "source_kind": "provider",
      "source": "workers/drain-8472"
    },
    {
      "category": "scheduled_jobs",
      "name": "production schedules",
      "status": "upgraded",
      "source_kind": "provider",
      "source": "schedules/release-8472"
    },
    {
      "category": "queues",
      "name": "retries and dead letters",
      "status": "drained",
      "source_kind": "provider",
      "source": "queues/drain-8472"
    },
    {
      "category": "connection_pools",
      "name": "long-lived pools",
      "status": "drained",
      "source_kind": "provider",
      "source": "pools/drain-8472"
    },
    {
      "category": "previews",
      "name": "Vercel production-connected previews",
      "status": "isolated",
      "source_kind": "provider",
      "source": "vercel-policy/8472"
    },
    {
      "category": "ad_hoc_writers",
      "name": "operator and third-party access",
      "status": "read_only",
      "source_kind": "manual",
      "source": "change-review/8472"
    }
  ]
}
```

Every required category needs at least one explicit cohort. Allowed statuses
are `upgraded`, `drained`, `isolated`, and `read_only`; `unknown` blocks. The
minimum categories are web, workers, schedules, queues, connection pools,
previews, and ad-hoc writers. A scaled-to-zero preview with production write
credentials is still a potential writer because it can reconnect later.
Uniqueness and exclusion transitions can add a plan-specific
`write_fence:<transition>` category; the deployment must attest the bounded
application or maintenance fence selected for that exact build.

The readiness report is operational feedback, not durable authority. Contract
SQL repeats its data assertions after any cleanup, immediately before restoring
enforcement. The deployment system still executes the receipted batches and
owns release ordering, lock budgets, replica health, and approval.
