# Stripe pg-schema-diff port roadmap

## Outcome

Onwardpg should preserve its stricter one-deployment safety decisions while
removing the mechanical SQL work that begins after a developer makes one of
those decisions.

```text
Developer changes the desired schema
├── Column evolution                    very likely
├── Change crosses dependencies         likely
│   ├── keys, indexes, foreign keys     very likely
│   └── views and materialized views    occasional
└── Partition topology changes          niche, high impact
```

The goal is not to reproduce Stripe's planner statement for statement. The
goal is to port its useful PostgreSQL choreography into onwardpg's existing
expand/contract model, editable SQL handoffs, hazards, receipts, and clone
verification.

## Evidence baseline

- Reference: Stripe `pg-schema-diff` v1.0.7 at commit
  `6208f8f3ceccae8ca634055dc47907a6a864cb76`.
- Corpus: 415 acceptance cases.
- Current evidence: 31 exact differential parity cases, 11 deliberate
  differential differences, 10 onward-only cases, 361 family-classified cases
  awaiting exact differential verification, and 2 deliberate out-of-scope
  cases.
- No case is currently marked confirmed missing. The roadmap below is based on
  known developer friction and visible strategy differences; it must not turn
  unverified cases into unsupported claims.

The human feature tree is in
[`docs/stripe-gap-inventory.md`](docs/stripe-gap-inventory.md). The checked-in
case ledger is
[`parity/stripe-pg-schema-diff-v1.0.7.json`](parity/stripe-pg-schema-diff-v1.0.7.json).

Implementation status (2026-07-18): milestones 0–4 are complete. The four
developer encounters now produce bounded SQL or non-executable split guidance;
the named Stripe receipts are checked into the case ledger; unit, race, vet,
and the full pinned differential suite pass; and the complete live planner
suite passes on PostgreSQL 15, 16, 17, and 18. Product-specific transforms,
write-conflict policy, traffic/drain evidence, and destructive partition
cleanup remain explicit operator decisions as required by this plan.

## Porting rules

1. Preserve onwardpg's choice between a reviewed one-deployment bridge and a
   genuine split plan. A valid PostgreSQL cast does not prove rolling
   application compatibility.
2. Generate every step that is mechanically derivable from typed catalog
   state. Ask the developer only for product semantics: casts, backfills,
   conflict policy, traffic gates, and destructive authorization.
3. Put generated scaffolding inside receipted, editable expand/contract
   bundles. Onwardpg still does not apply SQL to caller-owned databases.
4. Prefer online construction, brief identity swaps, `NOT VALID` plus
   validation, explicit timeouts, and verification checkpoints where
   PostgreSQL permits them.
5. Never copy a Stripe data-deleting rebuild merely because it converges.
   Adapt it into a data-preserving handoff unless deletion is explicitly
   fingerprint-authorized.
6. A port is complete only with case-level evidence and real-PostgreSQL
   convergence on every applicable supported major, currently PostgreSQL
   15–18.

## Milestone 0: establish differential receipts

Before broad implementation, select representative Stripe fixtures for each
milestone and run both planners against the same source and desired schemas.
Record whether onwardpg has exact parity, an intentional safety difference, or
a real capability gap.

The first receipt set should cover:

- a type, default, and nullability change on one column;
- a primary or unique key replacement with an external foreign key;
- a type change with ordinary and materialized-view dependents; and
- unpartitioned-to-partitioned and partition-key replacement cases.

Exit criteria:

- every implementation issue points to named Stripe cases;
- every selected case has a reproducible current-onward result;
- parity cases are closed as evidence work instead of rewritten; and
- intentional authorization and rolling-deployment differences remain
  explicit.

## Milestone 1: complete column-evolution handoffs

### Developer encounter

A developer changes a column's type together with its default, nullability,
collation, checks, or generated dependencies:

```sql
-- Current
CREATE TABLE invoices (
    id bigint PRIMARY KEY,
    amount text NOT NULL DEFAULT '0'
);

-- Desired
CREATE TABLE invoices (
    id bigint PRIMARY KEY,
    amount bigint NOT NULL DEFAULT 0
);
```

Onwardpg correctly asks whether this is reviewed one-deployment SQL or a split
plan, but the editable handoff still leaves too much ordering and dependency
work to the developer.

### Port

Once the developer supplies or confirms the cast, generate the database
choreography around it:

```sql
ALTER TABLE invoices ALTER COLUMN amount DROP DEFAULT;
ALTER TABLE invoices
    ALTER COLUMN amount TYPE bigint
    USING amount::bigint;
ANALYZE invoices (amount);
ALTER TABLE invoices ALTER COLUMN amount SET DEFAULT 0;
```

Compose this with onwardpg's existing staged `NOT NULL` primitive when the
desired change also tightens nullability:

```sql
ALTER TABLE invoices
    ADD CONSTRAINT onwardpg_amount_nn
    CHECK (amount IS NOT NULL) NOT VALID;
ALTER TABLE invoices VALIDATE CONSTRAINT onwardpg_amount_nn;
ALTER TABLE invoices ALTER COLUMN amount SET NOT NULL;
ALTER TABLE invoices DROP CONSTRAINT onwardpg_amount_nn;
```

For a split plan, generate a bounded shadow-column scaffold rather than a
same-name cast. Leave dual-write semantics and backfill expressions as explicit
editable work.

### Required implementation

- Model the ordered mutation recipe instead of emitting unrelated column
  alterations.
- Preserve and restore compatible defaults, checks, comments, collation, and
  nullability in dependency-valid order.
- Reuse a compatible validated `IS NOT NULL` check when catalog identity and
  semantics prove it safe.
- Insert `ANALYZE` after a rewrite when planner statistics are invalidated.
- Include the exact dependent-object closure in the handoff.
- Do not infer casts, reverse transforms, or backfill values.

### Exit criteria

- direct and split-plan choices both produce useful editable scaffolds;
- combined type/default/check/nullability fixtures converge;
- edited transformation SQL survives regeneration and base erosion;
- hazards distinguish table rewrite, table scan, and brief enforcement lock;
  and
- PostgreSQL 15–18 clone verification reaches an empty final diff.

## Milestone 2: coordinate keys, indexes, and foreign keys

### Developer encounter

A developer changes a primary or unique key that is referenced elsewhere.
Onwardpg already continuously replaces ordinary indexes and dependency-free
primary/unique constraints, but external dependents can turn the change into a
refusal or a manual dependency exercise.

### Port

Extend continuous replacement across the typed foreign-key closure:

```sql
ALTER TABLE orders DROP CONSTRAINT orders_customer_fkey;

CREATE UNIQUE INDEX CONCURRENTLY customers_key_new
    ON customers (tenant_id, customer_id);

ALTER TABLE customers DROP CONSTRAINT customers_pkey;
ALTER TABLE customers
    ADD CONSTRAINT customers_pkey
    PRIMARY KEY USING INDEX customers_key_new;

ALTER TABLE orders
    ADD CONSTRAINT orders_customer_fkey
    FOREIGN KEY (tenant_id, customer_id)
    REFERENCES customers (tenant_id, customer_id)
    NOT VALID;
ALTER TABLE orders VALIDATE CONSTRAINT orders_customer_fkey;
```

### Required implementation

- Find inbound and outbound foreign keys, including cross-schema and cyclic
  relationships.
- Build replacement unique indexes before removing usable access paths.
- Keep each constraint identity swap in a short, timeout-bounded transactional
  batch.
- Re-add eligible foreign keys as `NOT VALID`, then validate separately.
- Preserve deferrability, actions, match mode, comments, grant-visible names,
  and replica-identity edges.
- Generalize the strategy to partition-backed keys only after ordinary-table
  dependency closure is proven.

### Exit criteria

- no supported plan has an avoidable interval without a usable key index;
- one-way, mutual, and cyclic foreign-key cases converge;
- failed concurrent builds leave the old constraint and index usable;
- identity, timeout, and cleanup statements are deterministic and resumable;
  and
- the selected Stripe key/index fixtures have exact differential receipts.

## Milestone 3: generate derived-object rebuild closures

### Developer encounter

A table or column change reaches an ordinary view, materialized view, or an
index on a materialized view. Onwardpg can identify much of this closure, but
the developer may still have to reconstruct definitions, drop order, indexes,
comments, and refresh obligations.

### Port

For dependencies fully represented by PostgreSQL catalogs, generate an ordered
rebuild scaffold:

```sql
DROP MATERIALIZED VIEW reporting.daily_sales;
DROP VIEW reporting.sales;

ALTER TABLE sales
    ALTER COLUMN amount TYPE bigint
    USING amount::bigint;

CREATE VIEW reporting.sales AS
SELECT id, amount FROM sales;

CREATE MATERIALIZED VIEW reporting.daily_sales AS
SELECT sum(amount) AS total FROM reporting.sales;

CREATE UNIQUE INDEX daily_sales_key
    ON reporting.daily_sales (total);
```

### Required implementation

- Topologically order direct, whole-row, cross-schema, and transitive view
  dependencies.
- Preserve view options, check options, ownership, comments, privileges,
  materialized-view indexes, and typed trigger dependencies.
- Distinguish recreation, populated refresh, `WITH NO DATA`, and a manual
  concurrent-refresh choice rather than silently choosing data semantics.
- Keep opaque procedural-body references outside automatic proof; list them in
  a bounded handoff when catalog dependencies cannot establish safety.
- Reuse the closure engine for column evolution and partition conversion.

### Exit criteria

- unaffected views are excluded from the plan;
- changed and unchanged dependents are recreated in valid order;
- materialized-view data state is explicit and verified;
- definitions and secondary objects survive round-trip introspection; and
- representative ordinary/materialized-view Stripe cases converge on
  PostgreSQL 15–18.

## Milestone 4: scaffold partition topology conversions

### Developer encounter

A developer converts an ordinary table to partitioning, removes partitioning,
or changes a partition key, strategy, parent, or bound. These changes are rare
for most applications but central for time-series and very large multi-tenant
tables. Onwardpg currently produces a fingerprint-bound manual contract rather
than guessing data movement or destructive subtree recreation.

### Port

Port Stripe's complete hierarchy/dependency enumeration, but default to a
data-preserving shadow hierarchy:

```sql
CREATE TABLE events_new
    (LIKE events INCLUDING ALL)
    PARTITION BY RANGE (created_at);

CREATE TABLE events_2026
    PARTITION OF events_new
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');

INSERT INTO events_new SELECT * FROM events;

-- Verification checkpoint: row counts, key validity, and partition bounds.

ALTER TABLE events RENAME TO events_old;
ALTER TABLE events_new RENAME TO events;
```

The generated handoff must include affected keys, foreign keys, indexes,
triggers, views, materialized views, grants, policies, sequences, and
partition-local objects. It must leave synchronization and final cutover policy
editable when writes can continue during the copy.

### Required implementation

- Generate the complete old and desired partition trees with deterministic
  temporary identities.
- Provide attach/detach and prevalidated-bound paths when PostgreSQL can avoid
  a scan.
- Separate bulk copy, catch-up, verification, brief rename cutover, and old
  hierarchy cleanup.
- Prove that unique and primary keys include required partition columns.
- Require explicit authorization before dropping an old populated hierarchy.
- Never imply that disposable-clone convergence proves production copy time or
  cutover tolerance.

### Exit criteria

- ordinary-to-partitioned, partitioned-to-ordinary, and key/strategy changes
  receive complete editable runbooks;
- row-count and partition-bound assertions block unsafe cutover;
- dependent-object recreation uses the same typed closure as earlier
  milestones;
- destructive cleanup remains separately fingerprint-authorized; and
- PostgreSQL 15–18 tests cover nested and cross-schema hierarchies, circular
  foreign keys, local indexes, and retained data.

## Delivery order and issue slices

Implement vertical, independently verifiable slices in this order:

1. Add the four representative differential receipt groups.
2. Generate a direct reviewed type-change recipe around a user-supplied cast.
3. Add the split-plan shadow-column scaffold.
4. Extend continuous primary/unique replacement across one inbound foreign
   key, then generalize to cycles.
5. Extract a reusable typed dependency-closure and recreation planner.
6. Apply the closure to ordinary and materialized views.
7. Apply the same closure to partition topology handoffs.
8. Expand the exact Stripe differential matrix family by family and update the
   human inventory after every closed slice.

Each slice must include catalog modeling, graph edges, planner output, hazards,
timeouts, bundle rendering, edit preservation, real-PostgreSQL convergence,
idempotent replanning, and documentation. Avoid broad renderer changes that
land without a complete developer scenario.

## Explicit non-ports

- Staged `NOT NULL` is already implemented; improve composition and reuse, not
  the basic four-statement sequence.
- Same-name ordinary-index replacement and dependency-free primary/unique
  swaps are already implemented; extend their dependency envelope.
- Keep onwardpg's fingerprint-bound RLS, privilege, ownership, and destructive
  confirmations even where Stripe proceeds automatically.
- Do not add Stripe's apply workflow. Onwardpg continues to generate and verify
  plans without mutating caller-owned databases.
- Do not add physical column data-packing rewrites. Declarative column order is
  compatibility evidence, not permission to rewrite a table.
- Do not treat opaque SQL or procedural text search as a proven dependency
  graph.

## Roadmap completion

This roadmap is complete when:

- the four developer encounters produce bounded, useful SQL instead of blank
  or open-ended handoffs;
- generated work preserves the one-deployment application contract or clearly
  requires a split plan;
- every claimed Stripe port has a named case-level differential receipt;
- every successful final plan converges and replans empty on PostgreSQL 15–18;
- failures leave the prior usable schema intact wherever the selected online
  strategy promises that property;
- authorization and data-movement decisions remain explicit and receipted;
  and
- the Stripe inventory contains no family-only row that documentation presents
  as proven parity.

# Foundation: one-deployment expand/contract plan

## North star

One onwardpg bundle surrounds exactly one application deployment.

```text
accepted history + exported PostgreSQL DDL
                    |
                    v
               expand.sql
                    |
                    v
          ship one application version
                    |
       old and new instances overlap while
       requests, workers, pools, and queues drain
                    |
                    v
              contract.sql
                    |
                    v
              desired schema
```

The version being shipped must work against the database immediately after
expand, throughout the old/new overlap, and after contract. Old code only has
to work after expand and during the overlap. If the new version cannot work on
both sides of contract, the change genuinely needs another code deployment and
therefore another onwardpg plan.

This is a planning and SQL-generation contract. onwardpg never deploys code,
waits for traffic, observes connection draining, tracks environment progress,
or applies SQL to a caller-owned database.

## Product rules

1. A plan is anchored to one code deployment, not an arbitrary sequence of
   operational phases.
2. New bundles contain only `expand.sql` and `contract.sql`; an empty phase is
   omitted.
3. Backfill, validation, deduplication, and normalization are kinds of work,
   not a third deployment phase.
4. Initial synchronization and backfill run in expand when they can safely run
   while old code is live. Final catch-up, assertions, enforcement, and cleanup
   run in contract after old instances have drained.
5. Transactional and non-transactional batches remain explicit inside either
   file. File boundaries do not stand in for `BEGIN`/`COMMIT`.
6. A generated compatibility bridge must preserve both old and new application
   contracts during overlap, including writes—not merely make the final
   catalogs equal.
7. onwardpg never invents lossy or product-specific transforms, reverse
   transforms, conflict precedence, backfill values, traffic gates, or evidence
   that old instances are gone.
8. A transition that requires two application releases is reported as such. It
   is not squeezed into one bundle by relabeling work or leaving a misleading
   contract file.

## The two files

### `expand.sql`

Everything here must be safe while the pre-deployment application is still
serving traffic. It may contain multiple annotated batches:

- compatible schemas, tables, columns, indexes, constraints, routines, and
  views;
- synchronization functions and triggers that establish an overlap invariant;
- initial or batched backfill after that invariant exists;
- `NOT VALID` guards and other pre-deployment safety structures;
- assertions whose truth is required before application rollout; and
- narrowly bounded agent-authored SQL when product semantics are required.

Expand is not required to be one transaction. Concurrent indexes and large
backfills retain explicit execution boundaries and hazards.

### `contract.sql`

Everything here assumes the operator has completed the one application rollout
and established that pre-deployment instances, workers, pools, queues, and
prepared traffic paths are gone. It may contain:

- final catch-up or idempotent reconciliation;
- Boolean assertions and constraint validation;
- final `NOT NULL`, uniqueness, and other enforcement;
- compatibility trigger/function/view removal;
- destructive column, table, index, type, or routine cleanup; and
- narrowly bounded agent-authored SQL tied to the post-drain gate.

The newly deployed application must continue to work before, during, and after
these batches. Contract is not permission to assume a second code release.

## Column overlap strategy

### Same-type column rename or replacement

For a credible, explicitly confirmed same-type column identity, prefer two
physical columns plus a synchronization trigger when the table semantics allow
it.

Expand should:

1. add the desired column without enforcing an unreachable invariant;
2. install a deterministic `BEFORE INSERT OR UPDATE` row trigger;
3. synchronize old-only writes to the new column;
4. synchronize new-only writes to the old column when legacy readers need it;
5. reject a row that explicitly changes both columns to different values;
6. backfill existing rows after the trigger is active; and
7. leave both application contracts writable during rollout.

Contract should:

1. perform a final idempotent catch-up;
2. assert that the new column is complete and the pair is consistent;
3. enforce final constraints;
4. remove the synchronization trigger and function; and
5. remove the old column in a dependency-valid, lock-aware batch.

Generated identifiers must be deterministic, quoted, collision-checked, and
within PostgreSQL's 63-byte identifier limit.

The generated trigger must distinguish these cases using `OLD` and `NEW`
values:

| Write shape | Required behavior |
| --- | --- |
| old changed, new unchanged | copy old to new |
| new changed, old unchanged | copy new to old |
| both changed to the same value | accept |
| both changed to different values | reject visibly |
| neither changed | leave unchanged |

Nulls, defaults, generated columns, identities, partition propagation, existing
trigger ordering, RLS, privileges, and dependent objects must be modeled in the
eligibility check. Any unproven interaction blocks the generated strategy.

### Type or semantic transformation

A trigger bridge is not automatically reversible. For example,
`timestamp -> date` can discard timezone and time-of-day information. The
planner must not generate `date -> timestamp`, choose precedence, or assume
that only one application version writes.

The agent-facing result should offer only bounded outcomes:

- supply reviewed overlap SQL in the editable expand/contract pockets,
  including forward synchronization, reverse synchronization if required,
  conflict behavior, catch-up, and assertions; or
- split the feature into two plans because two code deployments are required.

The exact supplied SQL remains receipted and clone-verified. Manual SQL never
waives unknown catalog state or final convergence.

### When no middle data work is needed

Pure additive changes may emit only expand. A table rename may use the existing
updatable-view bridge when its tested eligibility rules hold, then atomically
remove the view and restore the physical table name in contract. A trigger is
not added merely to make every migration look alike.

Automatically updatable views remain a narrow table-identity strategy, not a
blanket claim about ORM compatibility. Duplicate base-column aliases, named
constraint upserts, grants, RLS, triggers, relation introspection, and prepared
queries require explicit acceptance coverage or a blocker.

## Two-deployment boundary

The planner cannot inspect application behavior, but it can identify structural
cases where no supported bridge proves single-deployment compatibility. Its
question or blocker must say:

```text
This desired schema cannot be reached safely around one application deployment
with the available compatibility strategy. Keep the old contract in this
feature's declarative DDL and remove it in a later feature plan, or supply a
reviewed bridge that proves the newly deployed code works before and after
contract.
```

Typical split:

```text
Plan A / deployment A
  expand the new shape
  deploy compatibility code
  synchronize and backfill
  keep the old shape in desired DDL

Plan B / deployment B
  deploy code that no longer references the old shape
  drain deployment-A instances
  contract the old shape
```

The authoring protocol should express only the irreducible choice—reviewed
single-deployment bridge or split plan. It must not grow into an application
deployment DSL.

## Protocol and bundles

- Render only `phases/expand.sql` and `phases/contract.sql`.
- Restrict new statement phases to `expand` and `contract`.
- Preserve safety, hazard, timeout, transactional, manual-work, and stable
  statement metadata; do not add a second orchestration hierarchy.
- Put clear section comments inside phase files for compatibility DDL,
  synchronization, backfill/validation, assertions, and destructive cleanup.
- Keep `verify.sql` for Boolean product assertions.
- Change `--through` to `expand|contract`.
- Make a clean developer-preview break: old three-phase bundles are invalid and
  must be regenerated. There are no users and no compatibility promise to
  preserve.
- Bump bundle/protocol versions wherever the two-phase shape changes their
  meaning. Reject old receipts with one direct regeneration error rather than
  carrying legacy replay branches.

## Planner audit

Inventory every current producer of phase `migrate` and classify it before
changing rendering. At minimum audit:

- required-column backfill and staged `NOT NULL`;
- manual type-conversion TODOs;
- partition attach/move work;
- materialized-view rebuild and refresh;
- routine-dependent materialized-view refresh;
- index and constraint replacement;
- identity/sequence changes;
- authorization changes; and
- agent-authored manual SQL.

For each producer record:

- whether it is safe with old code still live;
- the invariant required before it runs;
- whether it belongs in expand or contract;
- whether it can be generated, requires agent SQL, or implies two plans;
- transactional and lock requirements; and
- a real-PostgreSQL convergence and overlap test.

No global `migrate -> contract` string replacement is acceptable.

## Verification model

Clone verification must prove both checkpoints:

### Expand checkpoint

- exact expand bytes execute successfully;
- the expected residual consists only of receipted contract work;
- old and new interfaces are concurrently readable and writable for generated
  bridges;
- synchronization handles inserts and updates from both client shapes;
- conflicting dual writes fail explicitly; and
- no caller-owned database is mutated.

### Contract checkpoint

- exact contract bytes execute after the simulated drain gate;
- every assertion passes;
- compatibility objects are removed;
- the newly deployed interface remains usable;
- final catalog diff is empty; and
- a repeated plan is idempotent.

Disposable verification cannot prove traffic drain, production data volume,
backfill duration, lock tolerance, or application compatibility. SQL comments,
JSON diagnostics, and README language must say so consistently.

## Acceptance scenarios

1. **Same-type column rename:** old and new simulated clients insert and update
   during overlap; trigger synchronization is bidirectional; contract converges.
2. **Conflicting dual write:** one statement supplies divergent old/new values
   and receives a deterministic error.
3. **Timestamp-to-date replacement:** planner refuses to invent the reverse
   transform; reviewed agent SQL or split-plan choice is required.
4. **Required additive column:** trigger/default/backfill strategy keeps old
   writers valid; final `NOT NULL` occurs in contract.
5. **Table rename:** eligible updatable-view bridge remains writable from old
   and new names during overlap and survives grants/RLS checks within the
   supported boundary.
6. **Two-deployment feature:** no supported overlap bridge exists; planner
   explicitly asks for an intermediate desired schema rather than emitting a
   misleading successful plan.
7. **Large/manual backfill:** agent-edited batches survive regeneration and
   restacking, with false assertions blocking contract verification.
8. **Base erosion:** a colleague's accepted bundle lands underneath the active
   plan; trigger names, decisions, edited SQL, and receipts regenerate on the
   new head without stacking another feature migration.
9. **Old bundle rejection:** an expand/migrate/contract receipt receives one
   actionable clean-break regeneration error.
10. **PostgreSQL 15–18:** all generated trigger/view strategies and failure
    modes run on every supported major version.

## README target narrative

The README should explain the product in this order:

1. Declarative frameworks describe the desired schema and export complete
   PostgreSQL CREATE DDL; onwardpg uses SQL as the language-agnostic boundary.
2. A final-state diff is necessary but insufficient because production must
   survive one rolling code deployment.
3. Show `expand -> ship once -> drain -> contract` before introducing bundle
   internals.
4. Show a simple additive example.
5. Show one same-type trigger bridge with old/new client writes during overlap,
   initial backfill in expand, and final catch-up/cleanup in contract.
6. Show a transformation that is explicitly split into two plans because no
   reversible single-deployment bridge is known.
7. State plainly that onwardpg does not deploy, apply, observe drain, or prove
   production behavior.
8. Keep preview and detailed feature coverage to one short paragraph linked to
   the evidence-backed feature matrix.

Do not describe `migrate` as a deployment boundary. Do not imply a second code
ship. Do not use a timestamp replacement as a successful one-plan example
unless the complete synchronization semantics are present.

## Small CLI cleanup aligned with the narrative

The normal single-database quickstart should not require `--target primary`.
Target resolution should be:

1. explicit `--target` when supplied;
2. the only configured target when exactly one exists; or
3. an actionable ambiguity error listing target names when multiple exist.

Do not special-case the word `primary`, which sounds like a replication role.
Keep multi-target support for monorepos without exposing it as noise in the
common path.

## Implementation waves

### Wave 1: lock semantics and inventory

- Add tests that encode the one-deployment compatibility matrix.
- Inventory and classify every current migrate-phase producer.
- Specify the clean-break protocol versions and old-receipt rejection.
- Rewrite the README target narrative before changing output so tests and code
  have a concrete DX contract.

### Wave 2: two-file bundle foundation

- Teach protocol, renderer, receipts, editing, restacking, verification, CLI,
  and docs about new expand/contract-only bundles.
- Remove generation of `migrate.sql`.
- Route manual TODOs to a timing-explicit expand or contract pocket.

### Wave 3: trigger-backed same-type overlap

- Implement the complete typed vertical slice: eligibility, deterministic
  names, function/trigger graph nodes and dependencies, SQL, hazards, conflict
  behavior, cleanup, and convergence.
- Prove concurrent old/new inserts and updates on real PostgreSQL 15–18.
- Block every unmodeled trigger interaction.

### Wave 4: planner reclassification

- Move each audited migrate producer individually.
- Introduce explicit split-plan diagnostics where one deployment is not enough.
- Keep transformation SQL agent-owned and fingerprint-bound.
- Re-run the Trip-derived corpus and inspect every changed plan.

### Wave 5: DX and release receipts

- Default the sole configured target.
- Update README, CLI help, protocol, workflow, safety, bundle, feature, and
  comparison docs.
- Run cold-read feedback against the README without giving the reviewer code or
  tests.
- Run formatting, vet, race, unit, real-PostgreSQL 15–18, scenario, restack,
  old-receipt rejection, and clone-convergence suites.
- Publish an honest changelog entry describing the bundle/protocol transition.

## Explicit non-goals

- No second code deployment inside a plan.
- No production apply command or environment phase ledger.
- No traffic, connection, worker, queue, or pool observation.
- No framework adapters or ORM migration-journal integration.
- No embedded agent or deployment DSL.
- No guessed transformations, reverse mappings, conflict precedence, or
  backfill values.
- No claim that an automatically updatable view behaves like a table for every
  ORM or PostgreSQL feature.
- No broad PostgreSQL feature-parity work unrelated to this methodology.

## Completion criteria

This methodology is adopted only when:

- every newly generated bundle has at most `expand.sql` and `contract.sql`;
- the code and README describe exactly one application deployment;
- every former migrate producer has an evidence-backed timing classification;
- same-type column overlap is proven with concurrent old/new writes and an
  explicit divergent-write failure;
- non-reversible transformations require reviewed SQL or a split plan;
- old three-phase receipts are rejected with an actionable regeneration error;
- edited SQL and decisions survive no-op regeneration and base erosion;
- expand-only verification reports the precise expected contract residual;
- full verification converges on PostgreSQL 15–18;
- no caller-owned database is ever mutated; and
- a cold reader can explain why a feature is one plan or two without learning
  an onwardpg-specific orchestration language.
