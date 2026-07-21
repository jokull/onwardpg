# Agent-assisted migration workflow

onwardpg is a planning and verification CLI for a coding agent that already
understands the feature, repository, and deployment environment. It does not
embed an agent, inspect Git as a correctness input, or apply to a caller-owned
database.

The normal loop is intentionally small:

```sh
# Once per target.
onwardpg init

# Start a feature migration, then repeat after every schema edit or rebase.
onwardpg plan profile-name
onwardpg plan

# Copy only the direct local reconciliation when it is useful for development.
onwardpg plan --output sql

# Before review or CI.
onwardpg verify
onwardpg status
```

For a confirmed cross-name/type transition, keep using that same loop. Supply
the explicit rename hint first, then choose `type_change: manual_sql`. onwardpg
writes exactly two transition-bound pockets and lists the current and desired
dependency closure in them. The agent fills expand with the new column,
forward/reverse conversion, conflict and retry behavior, and initial backfill;
it fills contract with final catch-up and Boolean assertions, compatibility
removal, desired view/materialized-view recreation, index recreation, and the
chosen freshness proof. It must test NULL, blank, malformed, and target-range
cases from application and read-only production evidence rather than assuming
a cast from the type names.

A same-type rename with an intentionally changed dependent view is narrower:
onwardpg still owns the generated two-way column bridge and provides three
view pockets around it. The agent preserves the legacy output prefix in
expand, removes the overlap before cutover, and recreates the exact desired
closure afterward. It does not replace generated bridge SQL or add unrelated
objects to those pockets.

`init` establishes accepted history. The first `plan NAME` creates a
worktree-local active-plan anchor and a durable bundle. Subsequent `plan`
calls replace that same bundle: there is no finalize state and no accumulating
series of speculative feature migrations.

## Schema authorities and their comparisons

An agent should keep these states separate:

| Symbol | State | Trust level | Used for |
| --- | --- | --- | --- |
| H | accepted onwardpg history replayed in a disposable database | strict | the PR migration, H → W |
| W | DDL exported from the working declarative schema | intended | migration destination |
| D | a long-lived developer database | convenience only | direct local reconciliation, D → W |
| E | verified post-expand checkpoint for the selected bundle | receipted artifact evidence | contract-readiness expectation, P → E |
| P | production | optional observed evidence | drift against H and contract readiness against E |
| V | independent disposable verification replays | artifact evidence | exact bundle convergence to W |

The durable bundle is always H → W. This is the SQL reviewed and applied by
the team’s existing deployment process before compatible application code.
When configured, onwardpg also computes D → W for a developer’s local work.
That output is never confused with the PR bundle: in workspace mode it avoids
proposing drops for surplus objects so changing branches cannot turn a local
database into a destructive branch mirror.

onwardpg records an unreachable PostgreSQL physical column order as a
compatibility difference while adding required columns at the reachable end.
Semantic H → W clone convergence ignores ordinal-only differences; the typed
catalog snapshot still retains them for review.

An unaccepted name change is also comparison-specific. H → W forgets an
intermediate column name that existed only in an earlier draft and emits the
final additive shape. If that draft was already applied to D, D → W returns a
dev-scoped rename choice. A confirmed choice renders one direct local rename;
`preserve` keeps the old development object. The production bundle never
inherits a development-only rename.

Production is deliberately outside planning and clone verification. Run
`onwardpg drift check --database "$PROD_URL"` periodically or
after an incident to compare P with H; resolve a finding with a reviewed forward
migration. After expand and deployment, use `onwardpg contract check` to compare
P with the verified post-expand checkpoint E and enforce the separate live
catalog, data-gate, and writer-evidence boundary before contract.

## Decision loop

Schemas cannot prove whether two differently named objects are the same object,
whether data loss is intended, or how product data should be converted.
onwardpg exits `2` with the smallest typed decision it needs. The agent may
supply a known decision on the first call or copy a proposed choice:

```sh
onwardpg plan profile-name \
  --hint '{"kind":"rename","object":"column","from":["app","users","name"],"to":["app","users","display_name"]}'
```

Hints express only non-inferable intent. They contain no SQL, opaque question
IDs, fingerprints, or transaction rules. onwardpg binds valid choices to the
exact before/after schema state and rejects stale, contradictory, invalid, or
unused hints. Re-run `plan` after answering; it is safe to do this repeatedly.

There may also be an independent D → W question in a messy local database. Do
not answer it by assumption merely because the H → W question was answered.
The durable bundle remains reviewable even if local reconciliation waits.
For a deliberately disposable `strict` development database, answer that
separate question on the same command with `--dev-hint`; it is never reused as
durable `--hint` intent. In the default workspace mode, D-only objects are
preserved as possible work from another branch rather than treated as rename or
drop candidates.

## SQL handoff: take the last mile in the bundle

onwardpg writes generated SQL in `expand.sql` and `contract.sql`, with batch,
lock, rewrite, validation, and timing
comments. The generated structural SQL closes the catalog diff. Product-aware
work belongs in the ordinary editable phase files.

The filenames surround exactly one application deployment; they do not describe
SQL transaction scopes. Each file declares its own transactional and
non-transactional batches. Product-aware initial synchronization belongs in
expand when it is safe with old code live; final catch-up and enforcement
belong in contract after old writers drain.

For example, one application-owned backfill inside a larger reviewed
expand/contract type bridge might be:

```sql
-- Product rule: reporting days use each account's agreed business timezone.
UPDATE app.payments
SET settled_on = (settled_at AT TIME ZONE 'Atlantic/Reykjavik')::date
WHERE settled_on IS NULL;
```

This is deliberately not presented as complete onwardpg output: the generated
type-change receipt requires both an expand compatibility interface and a
contract cutover. The agent replaces the exact TODO pockets around product SQL
such as this and preserves their markers.

If the planner leaves a named Boolean contract-gate pocket, resolve it in
`contract.sql`; it is the production precondition repeated immediately before
enforcement. Add separate Boolean assertions in `verify.sql` when synthetic
conversion examples or clone-only postconditions are useful. Those optional
assertions strengthen disposable verification but do not authorize production
contract. Then run:

```sh
onwardpg verify
```

Verification executes exact bundle bytes only in onwardpg-created disposable
PostgreSQL databases and proves a final empty residual diff. It does not run
anything on D, P, staging, or production. The deployment-aware developer or
agent owns the actual execution and any dual-write/application rollout.

For a plan that loosens enforcement, `verify` also receipts the exact graph it
observed after expand. A release system or agent with read-only production
access can later run `contract check` against that checkpoint. The command
evaluates exact data gates and expiring evidence for web, workers, queues,
schedules, pools, previews, and ad-hoc writers; it never treats a lack of active
database sessions as proof that an old writer cannot reconnect.

## Rebase and restack

The agent uses Git to learn that new migrations arrived; onwardpg does not need
Git to prove a migration chain. After rebase, run the same command:

```sh
onwardpg plan
```

onwardpg validates the content-addressed accepted history, replays its current
head, and rebuilds the existing PlanID as H(new) → W. It preserves only
still-valid answers. Edits inside stable pockets are transplanted into refreshed
generated SQL; generator-owned conflicts stop with all three phase versions.
It never silently replays or declares application data work safe.

If a new base migration needs manual data work, review its receipt and use the
team’s normal process to apply it to the development database before relying on
D → W output. A plan report makes this distinction explicit rather than
pretending PostgreSQL catalog state proves application data effects.

An accepted `verify.sql` assertion can opt in to development evidence with a
line immediately under its assertion marker:

```sql
-- onwardpg:assert emails_normalized
-- onwardpg:dev-postcondition
SELECT count(*) = 0 FROM app.customer WHERE email IS DISTINCT FROM lower(email);
```

Only these marked Boolean queries run against D, each in a PostgreSQL
read-only transaction. Their pass/fail result is evidence, not permission to
replay a phase or infer a product-data repair. Unmarked verification assertions
remain disposable-clone-only.

If a normal branch switch removes the active bundle from the checkout, invoke
`onwardpg plan OTHER-NAME`. onwardpg parks the absent local
PlanID and restores a parked identity if that named bundle returns later. It
blocks rather than creating a second mutable plan when the current bundle is
still present. This is filesystem-local ergonomics, not Git-derived proof.

## Exit codes

| Exit | Meaning | Agent action |
| --- | --- | --- |
| 0 | planned, compatible, or verified | review SQL/diagnostics and continue |
| 2 | decision or SQL edit required | provide justified intent or edit named files |
| 3 | unsupported catalog state | stop; model it, narrow an explicit ignore, or change design |
| 4 | history, receipt, stale-parent, residual, or clone blocker | repair the evidence before proceeding |
| 1 | invocation/environment error | fix configuration, DDL, or credentials |

All machine output is status-oriented JSON by default. `--output sql` is intentionally
reserved for copyable D → W development SQL; diagnostic JSON remains on stderr
when planning is incomplete. See [CLI reference](cli.md) and
[migration workflow](migration-workflow.md) for command details.
