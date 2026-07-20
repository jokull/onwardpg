# Contract readiness and reconciliation

Status: implemented and proven in the working tree on 2026-07-20, after
`69a1b0d` established acceptance-compatible expand planning.

## Implementation receipts

The hill climb now exists as protocol and executable proof, not only this
roadmap:

- plan protocol v3 carries typed contract gates, reconciliation decisions,
  statement gate references, transition identities, and proof dispositions;
- bundle protocol v3 receipts those gates, the observed post-expand catalog
  checkpoint, and any operator-edited manual Boolean gate in the history hash;
- `onwardpg contract check` inspects the caller database in one read-only,
  repeatable-read transaction, compares the checkpoint, runs exact data gates,
  and validates expiring writer evidence bound to the PlanID, desired
  fingerprint, generation, environment, and bundle entry digest;
- CHECK, NOT NULL, typed MATCH SIMPLE/FULL foreign keys, and modeled unique
  enforcement receive exact generated probes; exclusion and genuinely
  unmodeled semantics remain a reviewed, receipted manual gate;
- every contract enforcement renderer passes one registry which rejects a
  statement without a typed gate or narrow catalog proof; and
- the checked documentation fixtures now reproduce the initial reconciliation
  question, developer cleanup pocket, inline assertion, restoration, and final
  convergence from current CLI output.

Proof commands and outcomes are recorded in [Completion receipts](#completion-receipts).

## Outcome

Make onwardpg's second promise as strong and explicit as its first:

1. `expand.sql` admits the legacy and newly deployed application while they
   overlap, even when that requires temporarily weaker enforcement.
2. `contract.sql` is executable only after legacy writers have drained, any
   rows admitted by the loose overlap have been reconciled, and exact
   postconditions prove the desired enforcement can be restored.

The important word is **writers**, not connections. A closed connection does
not prove an old Vercel preview, worker, cron job, queued retry, ad-hoc script,
or scaled-to-zero deployment cannot reconnect and issue a legacy write.

Onwardpg remains a planner. It will not apply migration SQL to production or
pretend it can discover a team's deployment topology. It will make contract
preconditions machine-readable, run permitted read-only checks, bind external
drain evidence to the exact plan, and refuse to call the contract ready while
required evidence is missing or stale.

## What the investigation found

The phase classifier is materially stronger than the contract-readiness model.

### Already sound

- Proven CHECK widenings and unique-key relaxations converge in expand.
- Unknown CHECK, foreign-key, uniqueness, exclusion, and cross-kind changes
  can use a deliberately loose overlap instead of rejecting the new writer.
- Desired CHECK and foreign-key enforcement can be restored with `NOT VALID`
  followed by a separate validation batch.
- NOT NULL has direct, staged, and operator-authored backfill paths.
- Manual work, answers, and edited SQL are fingerprinted and survive an
  unrelated plan rebase only when their scope fingerprint still matches.
- Clone verification executes exact phase bytes, checks manual Boolean
  verification queries, and proves final catalog convergence.

### Gaps found in the pre-climb architecture

1. A hazard such as `duplicate_rows_possible` or
   `possible_overlap_window_invalid_rows` is descriptive text. It does not
   force a cleanup choice or block the restoring statement.
2. Several loose-envelope paths proceed directly to `VALIDATE CONSTRAINT`,
   `CREATE UNIQUE INDEX`, or exclusion creation. PostgreSQL will correctly
   fail if overlap rows are invalid, but the failure arrives late and without
   a retained reconciliation decision.
3. `ManualWork.VerificationSQL` is optional in the generic protocol. Individual
   features sometimes require it, but cleanup does not have one uniform
   contract.
4. Manual verification is rendered into phase SQL as a `-- Verify:` comment
   and executed by onwardpg only during clone batch verification. It is not an
   ordered production assertion before `VALIDATE CONSTRAINT`, unique-index
   creation, or another restoring statement.
5. `verify` runs against onward-created disposable databases. Those clones do
   not contain production overlap writes, so successful clone convergence is
   not evidence that production is ready for contract.
6. `verify.sql` is a postcondition artifact. Reusing it as a production
   contract-readiness gate would confuse clone proof, development evidence,
   and live preconditions.
7. Onwardpg has no typed drain evidence. Contract comments say “after writers
   drain,” but no artifact identifies which writer cohorts were considered or
   whether previews can still write to production.
8. There is no read-only check that production currently matches the expected
   post-expand catalog checkpoint before contract begins.

Therefore the next hill climb is not another phase heuristic. It is a
first-class **contract gate and reconciliation protocol**.

## The invariants

For a plan with current schema `C`, expand checkpoint `E`, and desired schema
`D`:

~~~text
before deploy:       catalog == E
during overlap:      E accepts LegacyWrites union NewWrites
before contract:     LegacyWriters == drained
before restoration:  DesiredInvariant(data) == true
after contract:      catalog == D
~~~

Every contract restoration must have a typed disposition:

- `no_gate`: catalog-only cleanup or a proof shows overlap cannot violate the
  desired invariant;
- `generated_assertion`: onwardpg can generate an exact Boolean data test;
- `manual_reconciliation`: reviewed cleanup SQL plus a required Boolean test;
- `external_attestation`: deployment state must be supplied by the system or
  agent that can observe writers; or
- `split_plan`: one deployment cannot safely reach the desired contract.

No restoration statement may rely only on a hazard string.

## Developer experience

### Planning

When a loose overlap can create rows outside the desired invariant, `plan`
asks one scoped question:

~~~text
The overlap may admit rows that violate constraint:app:orders:orders_owner_fkey.
How will contract reconcile them after legacy writers drain?

  assert_only   Run the exact generated assertion; no repair is expected.
  manual_sql    Capture cleanup SQL and a Boolean verification query.
  split_plan    Keep the loose schema and restore enforcement in a later plan.
~~~

`assert_only` does not mean “production is clean today.” It means the exact
assertion must pass again after drain, immediately before restoration.

`manual_sql` requires:

- a one-line summary;
- transactional or non-transactional execution mode;
- at least one cleanup/backfill statement; and
- at least one read-only query returning exactly one Boolean `true` row.

That answer is bound to the desired invariant and its dependency closure. A
predicate, key, referenced-column, collation, NULL behavior, or participating
table change invalidates it. Unrelated schema edits retain it.

### Generated contract ordering

Where PostgreSQL supports `NOT VALID`, contract should fence new writes before
repairing historical overlap rows:

~~~sql
-- Contract precondition: legacy writer evidence has passed.

ALTER TABLE app.orders
  ADD CONSTRAINT orders_owner_fkey
  FOREIGN KEY (owner_id) REFERENCES app.users(id) NOT VALID;

-- Fingerprinted operator cleanup, when selected.
UPDATE app.orders ...;

-- Exact generated or operator-authored Boolean assertion.
DO $onwardpg$
BEGIN
  IF NOT (<read-only Boolean postcondition>) THEN
    RAISE EXCEPTION 'onwardpg contract gate failed: orders_owner_fkey';
  END IF;
END
$onwardpg$;

ALTER TABLE app.orders
  VALIDATE CONSTRAINT orders_owner_fkey;
~~~

The new application is already deployed and must satisfy the desired
constraint. `NOT VALID` fences its new writes while allowing historical rows
to be repaired and then validated.

UNIQUE, primary-key, and exclusion enforcement have no general `NOT VALID`
form. Their contract must instead order cleanup, an exact duplicate/conflict
probe, and the build. If concurrent writes can reopen the race before the
constraint exists, the planner must require an application-level idempotency
proof, a bounded write fence, a retryable concurrent-build handoff, or
`split_plan`. “The application probably does not create duplicates” is not a
proof.

### Contract readiness

Add a read-only command whose job is evidence, not execution:

~~~sh
onwardpg contract check \
  --bundle support-new-conversation-slack \
  --database-env PROD_READONLY_DATABASE_URL \
  --evidence deploy-readiness.json
~~~

It returns versioned JSON with one of:

- `ready`: every static, catalog, data, and external gate passed;
- `needs_evidence`: the database is compatible but writer evidence is absent;
- `blocked`: a data assertion, preview policy, or catalog checkpoint failed;
- `stale`: the evidence targets another PlanID, bundle digest, generation,
  desired fingerprint, environment, or expired observation window.

The command must:

1. validate the bundle and hash chain;
2. ensure the selected bundle is the expected chain head;
3. inspect production in a repeatable-read, read-only transaction;
4. compare production with the simulated post-expand checkpoint;
5. run contract data gates read-only where they are safe and bounded;
6. validate external writer evidence; and
7. emit a content-addressed readiness report for the deployment system.

It must never apply `expand.sql`, cleanup SQL, or `contract.sql` to the supplied
database.

Contract SQL repeats its data assertions after any cleanup. The earlier
readiness check is operational feedback, not authority that can survive a
time-of-check/time-of-use race.

## Writer-drain evidence

Drain evidence belongs to the deployment-aware caller. Define a small provider-
neutral document bound to:

- target and environment;
- PlanID, bundle entry digest, and generation;
- application release/cutover identity;
- observation time and expiry;
- every known write-capable cohort; and
- the evidence source for each cohort.

Minimum cohort categories:

- web/application deployments;
- background workers and consumers;
- cron and scheduled jobs;
- queues, retries, and dead-letter replays;
- connection pools and long-running processes;
- preview/test deployments with production credentials; and
- ad-hoc or third-party writers.

Each cohort must be `upgraded`, `drained`, `isolated`, or `read_only`.
`unknown` blocks. Manual evidence is allowed during preview, but it must be
explicit, expiring, reviewable, and no more powerful than provider-derived
evidence.

### Vercel previews

A Vercel preview with production write credentials is an active potential
legacy writer even when it has zero current database sessions. Contract has
four honest options:

1. delete or expire it;
2. redeploy it with the compatible release;
3. isolate it from production or make its production access read-only; or
4. keep the compatibility shape and move cleanup to a later plan.

Ignoring previews is not an allowed attestation. A future Vercel adapter may
produce cohort evidence, but the first protocol should remain provider-neutral
so Kubernetes, ECS, Cloudflare, Render, Fly, and bespoke workers fit the same
model.

## Typed planner model

Introduce machine semantics instead of inferring readiness from hazards:

~~~go
type ContractGate struct {
    ID               string
    Kind             string // catalog_checkpoint, data_assertion, manual_reconciliation, writer_attestation
    ScopeFingerprint string
    Reason           string
    BooleanSQL       string
    RequiredEvidence []string
}

type Reconciliation struct {
    TransitionID string
    Strategy     string // assert_only, manual_sql, split_plan
    Work         *ManualWork
    GateIDs      []string
}
~~~

Contract statements and batches reference gate IDs. Stable statement identity
must include those references. The protocol, plan digest, bundle manifest, and
edited-phase receipts therefore commit to the complete readiness contract.

Use a coordinated preview version bump for `protocol`, `bundle`, and readiness
report formats. Older bundles remain readable only through an explicit
compatibility path; they must not silently acquire gates they never receipted.

## Exact gate coverage

| Desired enforcement | Cleanup risk | First verified gate |
| --- | --- | --- |
| Proven CHECK widening | None: old accepted rows imply desired predicate | No data gate; expand-only convergence |
| Unknown or cross-name CHECK restoration | Overlap rows may make the desired predicate `FALSE` | Generated `NOT EXISTS (... WHERE (predicate) IS FALSE)`; SQL NULL must retain PostgreSQL CHECK semantics |
| New CHECK on an existing table | Existing rows may violate it | Same generated CHECK assertion |
| NOT NULL | Existing or overlap rows may be NULL | Generated `NOT EXISTS (... WHERE column IS NULL)` |
| New or changed FK | Orphans or MATCH-null violations | Generated anti-join only after local/referenced column pairs and MATCH semantics are typed |
| UNIQUE/primary/unique index | Duplicate keys, predicate changes, NULL semantics | Generated duplicate probe for fully modeled keys; otherwise manual verification |
| Exclusion | Pairwise conflicts under arbitrary operators | Manual verification until exact operator semantics and predicates are modeled |
| Constraint-kind change | Depends on desired kind | Gate produced by the desired enforcement family |
| Type/rename/generated data bridge | Product conversion or synchronization drift | Existing manual handoff, upgraded to require Boolean verification |
| Pure enforcement removal | Desired schema does not restore the guarantee | No contract data gate; retain explicit permanent-loss decision |
| New enforcement on a new/empty table | No historical rows | No cleanup gate; normal create ordering |

For CHECK predicates, use `expression IS FALSE`, not `NOT expression`: a
PostgreSQL CHECK accepts `TRUE` and `NULL` and rejects only `FALSE`.

For foreign keys, extend the typed constraint model before generating a probe.
Parsing presentation SQL during rendering is not acceptable. Retain ordered
local/referenced column pairs, MATCH mode, actions, and equality/operator
metadata from the catalog.

For expression and partial unique indexes, the probe must preserve predicate,
expression, collation, operator class, and `NULLS [NOT] DISTINCT` behavior.
Unsupported exact probes require manual reconciliation or a blocker; a
plausible `GROUP BY` is not enough.

## Agent workflow

A coding agent can improve the plan before deployment when it has read-only
production access:

1. inspect cardinality and representative invalid rows;
2. choose `assert_only`, compose bounded cleanup SQL, or recommend
   `split_plan`;
3. put cleanup and exact Boolean verification into the retained answer;
4. run clone verification; and
5. leave contract-time readiness to fresh post-drain evidence.

Sampling production values is useful for designing a backfill or constructing
`verify` fixtures. Sampling is never a contract gate: only an exact query over
the relevant production scope, PostgreSQL's own validation/build, or an
explicitly reviewed manual proof can authorize restoration.

Documentation should give agents one entry point and state this boundary
plainly: “Point your agent at `docs/agent-workflow.md`; read-only production
access improves reconciliation planning but does not prove writer drain.”

## Implementation climbs

### 1. Inventory and fail the missing cases

- Create one registry of every statement that restores or tightens
  enforcement in contract.
- Add a test that fails if such a statement has neither a proof disposition nor
  gate IDs.
- Convert loose-envelope hazard strings into typed transition metadata while
  retaining human-readable hazards.
- Cover new enforcement on existing tables, not only mutations introduced by
  `expand_relax`.

### 2. Reconciliation decisions

- Replace the narrow `prepare_unique` special case with the general
  reconciliation question.
- Require non-empty Boolean verification for reconciliation `ManualWork`.
- Insert cleanup between the desired `NOT VALID` fence and validation where
  PostgreSQL supports it.
- Add `assert_only` and `split_plan`; remove the misleading durable meaning of
  `already_clean`.
- Rebind decisions by desired-invariant scope, invalidating them on meaningful
  dependency changes.

### 3. Exact assertion generators

- Implement CHECK and NOT NULL assertions first.
- Extend typed FK metadata, then implement MATCH-correct anti-joins.
- Implement simple and expression UNIQUE probes only with exact NULL,
  predicate, collation, and key semantics.
- Keep exclusion and unproved forms manual or blocked.
- Render assertions both as readable contract SQL and typed protocol gates.

### 4. Expected expand checkpoint

- Materialize accepted history plus the selected bundle through expand in a
  disposable database.
- Fingerprint that graph and receipt it in the bundle.
- Compare a caller-supplied production catalog to that checkpoint read-only.
- Distinguish missing expand, partial contract, unrelated drift, and ignored
  configured objects in diagnostics.

### 5. Writer evidence and `contract check`

- Define the provider-neutral evidence and readiness-report protocols.
- Bind evidence to PlanID, entry digest, generation, environment, release, and
  expiry.
- Require explicit preview/test cohort disposition.
- Add the read-only command without adding any production execution path.
- Make JSON output concise enough for an agent and CI to act on directly.

### 6. Rebase, edits, and history integrity

- Receipt gates and the expand checkpoint in the bundle manifest.
- Preserve manual reconciliation only when its scope fingerprint remains
  equal.
- Treat generator changes to a gated contract region as an edit conflict.
- Make `verify --check` reject missing, stale, or unreceipted gate artifacts.
- Ensure an absorbed plan cannot leave orphan drain evidence or readiness
  receipts behind.

### 7. Documentation and product proof

- Rewrite the workflow around `expand -> deploy -> drain writers -> reconcile
  -> assert -> contract`.
- Explain potential writers versus current database sessions with the Vercel
  preview example.
- Show easy, medium, hard, and agent-assisted reconciliation cases from
  generated fixtures.
- Keep clone verification, production readiness, and migration execution as
  visibly separate concepts.
- Update the README claim only after every evidence gate below passes.

## Receipt matrix

Every loose-envelope fixture must exercise four states:

1. legacy and new writes both succeed after expand;
2. a desired-invalid overlap row makes the named contract gate fail before
   enforcement is partially restored;
3. captured cleanup plus its Boolean assertion permits contract; and
4. final catalog diff is empty.

Required fixtures:

- known CHECK widening with no contract gate;
- unknown same-name and cross-name CHECK transitions;
- NOT NULL with and without cleanup;
- changed FK with MATCH SIMPLE and MATCH FULL composite keys;
- unique-key relaxation, incomparable uniqueness, partial unique index, and
  `NULLS NOT DISTINCT`;
- exclusion mutation using manual verification;
- same-name constraint-kind change;
- type and rename manual bridges;
- stale reconciliation after predicate/key/dependency change;
- retained reconciliation after unrelated schema change;
- production not at the expected expand checkpoint;
- missing, expired, wrong-plan, wrong-environment, and unknown-preview writer
  evidence; and
- upgraded, drained, isolated, and read-only preview cohorts.

Run catalog, acceptance, contamination, cleanup, and convergence receipts on
PostgreSQL 15, 16, 17, and 18. For concurrency-sensitive uniqueness, include a
writer running during the contract build and prove the selected strategy
either converges or blocks cleanly.

## Completion gates

This hill climb is complete only when:

- every contract enforcement statement has typed gate coverage or a proof that
  no gate is needed;
- every loose overlap that can pollute the desired invariant captures
  `assert_only`, verified manual cleanup, or `split_plan`;
- manual reconciliation cannot be accepted without Boolean verification;
- contract assertions fail before irreversible or partially restored work;
- production can be compared read-only with the exact expected expand graph;
- writer evidence is scoped, expiring, and includes preview environments;
- `contract check` cannot execute migration SQL on a caller database;
- plan rebasing preserves valid reconciliation and rejects stale evidence;
- PostgreSQL 15–18 contamination and convergence receipts pass; and
- documentation no longer implies that clone verification or zero active
  sessions proves contract readiness.

## Completion receipts

The implementation is guarded by these checked receipts:

- `go test ./... -count=1` passes across every Go package;
- `scripts/test-documentation-receipts.sh` regenerates the required-column,
  type-change, rename, and dependency-closure examples from the current CLI,
  executes the edited fixture, and proves convergence;
- `node scripts/check-documentation.mjs`, `pnpm --dir website check`, and
  `pnpm --dir website build` pass;
- PostgreSQL 15, 16, 17, and 18 each pass the required-column, loose-CHECK
  contamination/cleanup, `NULLS NOT DISTINCT` uniqueness, `NOT VALID` foreign
  key, exclusion, and read-only contract-check integration receipts; and
- `TestContinuousConstraintReplacementRetriesFailedBuildWithoutDroppingOldKey`
  passes on PostgreSQL 15–18 with a real writer transaction committing a
  conflicting row while `CREATE UNIQUE INDEX CONCURRENTLY` is running. The
  build blocks, fails without dropping the old key or its inbound foreign key,
  then the deterministic cleanup/retry path converges.

The implementation also closes an audit finding discovered while grounding
the docs: authoritative baseline DDL now discards the staged contract gates
from its graph-planner inventory, and the contract-check integration test uses
the restricted scratch database URL rather than accidentally reusing the
administrator database.

## Non-goals

- Apply expand or contract SQL to production.
- Discover every deployment, worker, queue, or preview without caller evidence.
- Treat `pg_stat_activity` as proof that old writers cannot return.
- Invent product-specific cleanup from DDL or sampled rows.
- Accept sampling as a substitute for an exact contract assertion.
- Build provider-specific deployment integrations before the neutral evidence
  protocol is proven.
- Force every compatibility change into one deployment; `split_plan` remains a
  correct outcome.
