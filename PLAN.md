# onwardpg: one evolving migration per feature

This is the product and implementation plan for onwardpg's developer-preview
workflow. It supersedes the older workflow that made the agent copy a history
`head_ref`, distinguish `draft --create` from refresh, and run a separate
`dev plan` command.

The product promise is:

> Keep one reviewable migration current while a feature evolves, automatically
> restack it over an advancing accepted migration chain, and show the developer
> exactly what their present database still needs—without applying anything.

The positioning is migration planning designed around the actual lifecycle of
a feature branch:

- application schema declarations change repeatedly before review;
- developers deliberately apply partial versions to their own databases;
- other features merge underneath a multi-day branch;
- the feature is rebased or otherwise updated by a Git-aware developer or
  agent;
- the PR should still contain one cumulative migration from the latest
  accepted ground to the would-be merged schema; and
- long-lived development databases take pragmatic paths and must not be
  mistaken for migration history.

We may describe this as mirroring real-world Git flows. We should not claim to
be the first product to do so until that market claim has been researched.

## The irreducible model

Four schemas must remain separate:

| Name | Meaning | Trust |
| --- | --- | --- |
| **H — history head** | Schema obtained by replaying the accepted onwardpg bundle chain | Durable baseline for the next migration |
| **W — working schema** | Complete CREATE-statement DDL exported from current code | Desired schema after the feature merges |
| **D — developer database** | A worktree sandbox or long-lived development database | Useful observed state, never durable history |
| **P — production** | Deployed catalog | Consulted only by an explicit optional drift audit |

The daily command computes two paths, for different purposes:

```text
durable PR migration:    H ───────────────▶ W
local reconciliation:   D ───────────────▶ W
```

The first path replaces one migration folder in the feature branch. The second
path is printed for deliberate local use and never changes history.

Production remains separate:

```text
periodic drift audit:    H ◀──────────────▶ P
```

The durable migration must never be derived from D. A development database may
be behind, ahead, partially migrated, manually changed, populated with useful
test data, or contain remnants from another branch.

## Two meanings of convergence

Disposable verification and long-lived development have different correct
end states.

### Strict convergence

Fresh worktree sandboxes and onwardpg-created verification databases must end
at exact catalog equality with W. There may be no unexplained residual diff.

Strict convergence is used for:

- replaying H;
- verifying H plus the active migration bundle;
- CI;
- disposable per-scenario databases; and
- explicit drift inspection.

### Workspace compatibility

A long-lived development database should contain everything required by W,
but may contain surplus objects left by another branch or experiment. Absence
from the currently checked-out DDL is not, by itself, permission to remove
anything from D.

Workspace reconciliation therefore:

- creates missing required objects;
- changes incompatible required objects after any necessary decision;
- performs a rename or contract only when the active durable plan carries the
  matching explicit intent;
- reports objects that exist only in D as **surplus**;
- preserves surplus objects by default; and
- blocks when same-name incompatible state cannot be reconciled safely.

After its SQL is applied, D may be *compatible* rather than exactly equal to W.
Periodic drift inspection or a deliberate rebuild handles abandoned surplus
state. Merely switching branches must not make onwardpg suggest dropping the
previous branch's tables, columns, indexes, enum values, or constraints.

## North-star CLI walkthrough

The ordinary lifecycle should fit in four commands:

```sh
onwardpg init
onwardpg plan [name]
onwardpg verify
onwardpg drift check --database "$DATABASE_URL"
```

There is no `apply`, `deploy`, `lock`, `finalize`, `rebase`, `pr`, `merge`, or
`rollback` command.

### 1. Configure DDL, development, and scratch PostgreSQL

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_command = ["pnpm", "db:export-schema"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

`schema_file` remains the static-file alternative to `schema_command`.
Drizzle's role, for example, is simply:

```sh
drizzle-kit export
```

It translates the TypeScript declaration into complete PostgreSQL DDL. It
does not determine H, inspect D, own onwardpg history, or supply a migration
journal. The same boundary works for any tool capable of exporting CREATE
statements; onwardpg does not add framework adapters.

The scratch URL must point to a server on which onwardpg may create and drop
disposable databases. Caller-owned development and production databases are
always read-only inputs to onwardpg.

### 2. Establish the ground floor

```sh
onwardpg init --target primary
```

`init` creates and verifies the baseline bundle. This says where onwardpg's
accepted history begins; it is not SQL to apply to an existing production
database.

For an existing application, establishing that baseline may include a
one-time explicit comparison with the authoritative deployed or replayed
schema. Production comparison is not repeated for every PR.

### 3. Start the feature plan

On the first call, the developer or agent supplies a semantic name:

```sh
onwardpg plan booking-dates --target primary
```

This call:

1. records a stable logical plan ID;
2. records a worktree-local active-plan anchor;
3. validates and replays the unique accepted history chain H;
4. exports and materializes W in disposable PostgreSQL;
5. plans the complete H to W migration;
6. asks only for intent not present in those schemas;
7. writes the reviewable feature bundle;
8. inspects D when configured; and
9. prints the direct D to W workspace reconciliation SQL.

The migration name is presentation. The stable logical ID inside the manifest
is identity. A sortable path may be resequenced when the plan is restacked:

```text
migrations/onward/primary/0042_booking-dates/
```

If another migration merges first, the same logical plan may move to
`0043_booking-dates`. Ordering is proven by manifest parent digests, never by
the filename.

### 4. Keep developing

After initialization, the common invocation is just:

```sh
onwardpg plan --target primary
```

Suppose the first desired schema was W1 and the developer deliberately applied
the local SQL to D:

```text
PR bundle:   H  ─────────▶ W1
dev output:  D0 ─────────▶ W1
```

The developer then adds another column, producing W2:

```text
PR bundle:   H  ─────────▶ W2   replaces the same bundle
dev output:  W1 ─────────▶ W2   prints only the observed local residual
```

Local execution never advances H and never freezes the feature bundle. There
is no explicit lock transition.

### 5. Absorb an advancing base

Another feature may merge while this work is open. The developer or coding
agent fetches, rebases, merges, or otherwise updates the checkout using normal
Git tools. onwardpg does none of that.

The next ordinary call observes:

- the active bundle still names its previous parent A;
- after excluding that explicitly selected bundle, the remaining content forms
  a unique accepted chain ending at B; and
- B is a descendant of A containing the newly arrived bundles.

It therefore restacks automatically:

```text
before:  A ──▶ active feature
             ╲
after Git:     └──▶ B

after plan: A ──▶ B ──▶ refreshed active feature
```

The result is still one migration:

```text
durable PR bundle:  B ─────────▶ W
local reconciliation: D ───────▶ W
```

The caller does not copy a `head_ref` during the ordinary path. `--onto
<content-bound-head>` exists only when excluding the selected plan leaves more
than one valid base head and onwardpg cannot infer the intended one.

If accepted history was modified rather than appended, a parent is missing, a
remaining fork exists, or the selected plan has accepted descendants, planning
blocks instead of rewriting history.

### 6. Understand local SQL after ground advancement

After B arrives, D may already contain an older version of this feature but
none of B. The local output may consequently contain:

- catalog-visible effects of newly accepted bundles;
- incremental feature changes since the previous local application;
- corrections caused by interactions between those two; and
- explicitly confirmed contract work from the active feature.

It is always planned directly from the actual D catalog to W. It is never
constructed by concatenating the newly accepted migration SQL with a feature
increment.

For example, an old local feature may have renamed `name` to `full_name`, while
an incoming accepted migration renamed `name` to `display_name`. Replaying the
incoming SQL literally would fail against D because `name` is gone. Direct
reconciliation observes the real source and asks whether `full_name` should
become `display_name`.

Successful SQL output starts with an unmistakable receipt comment:

```sql
-- onwardpg development workspace reconciliation
-- source: sha256:...
-- desired: sha256:...
-- accepted base advanced: 0040 -> 0042
-- surplus objects preserved: 4
-- this is not the cumulative PR migration

ALTER TABLE public.payments ADD COLUMN status text;
ALTER TABLE public.bookings ADD COLUMN local_date date;
```

### 7. Do not pretend catalog state proves data history

Schema diffing cannot prove that an accepted data-only migration ran:

```sql
UPDATE customer SET email = lower(email);
```

Nor can desired DDL recover the business rule from a structural migration:

```sql
ALTER TABLE payment ADD COLUMN status text;
UPDATE payment SET status = /* product-specific rule */;
ALTER TABLE payment ALTER COLUMN status SET NOT NULL;
```

When the accepted base advances, onwardpg knows which accepted bundles lie
between the old and new parent. It must inventory their non-catalog work and
report it separately:

```text
Accepted history advanced through 2 bundles.

Catalog-visible effects are represented in the local reconciliation.
The following accepted data/manual steps cannot be proven from catalog state:
  0041_payment-status/phases/migrate.sql
  0042_normalize-email/phases/migrate.sql

Review whether these effects already hold in this development database.
```

Those statements are not silently copied into stdout. The agent or developer
may know they were already run, may deliberately replay them, or may need to
adapt them because D took a different path. A recorded, safe boolean
postcondition may provide evidence, but absence of evidence remains explicit.

This is the hard boundary of the claim: local SQL proves a structural workspace
path, not application of every historical data operation.

### 8. Resolve only genuine ambiguity

Schemas cannot prove renames, destructive intent, product-specific casts, or
backfill rules. onwardpg emits semantic choices rather than guessing:

```json
{
  "status": "needs_decisions",
  "scope": "bundle",
  "choices": [
    {
      "hint": {
        "kind": "rename",
        "object": "column",
        "from": ["public", "booking", "occurred_at"],
        "to": ["public", "booking", "occurred_on"]
      }
    },
    {
      "hint": {
        "kind": "drop",
        "object": "column",
        "name": ["public", "booking", "occurred_at"]
      },
      "hazards": ["data_loss"]
    }
  ]
}
```

The coding agent may pass a known semantic hint on the first call or repeat the
same command with the offered hint. Accepted durable intent is narrowly bound
to the participating objects and persisted in the bundle. Unrelated feature
edits and base advancement preserve it; a changed meaning invalidates it.

The D to W companion plan has its own source fingerprint. Durable decisions
may authorize equivalent local operations when their exact semantic scope
still matches. Any local-only ambiguity is reported separately and must never
weaken or invalidate an otherwise complete durable bundle.

### 9. Hand the SQL to the agent

The durable output is ordinary, reviewable migration code:

```text
migrations/onward/primary/0043_booking-dates/
├── manifest.json
├── decisions.json
├── plan.json
├── verify.sql
└── phases/
    ├── expand.sql
    ├── migrate.sql
    └── contract.sql
```

SQL comments explain purpose, phase timing, locks, rewrites, validation,
availability hazards, and data loss. The files answer:

- what may run before compatible application code;
- what product-aware data work must happen;
- what must wait for a deployment or compatibility window; and
- what onwardpg cannot derive.

The coding agent may replace TODOs, implement a backfill, split batches,
rewrite generated SQL, or add boolean assertions. The SQL folder—not a large
JSON orchestration language—is the last-mile extension point.

On regeneration:

- unchanged generated SQL may be refreshed;
- agent-only edits are preserved;
- `verify.sql` remains agent-owned;
- narrowly valid decisions survive;
- if generator and agent both changed the same phase, onwardpg preserves the
  current bytes, emits old-generated and new-generated candidates, and reports
  a conflict; and
- no generated or agent-owned work is silently discarded.

### 10. Verify exact edited bytes

```sh
onwardpg verify --target primary
```

Using the active plan anchor, `verify`:

1. creates disposable PostgreSQL databases;
2. replays the immutable parent chain;
3. executes the exact edited expand, migrate, and contract batches;
4. evaluates `verify.sql` assertions;
5. introspects the resulting catalog;
6. proves an empty residual against W; and
7. receipts the exact verified bytes.

`verify --check` is the read-only CI form. It rejects stale DDL, changed parent
history, unanswered decisions, unresolved TODOs, unreceipted edits, broken
chains, failed assertions, and non-convergence.

Partial verification may explain the expected remaining phases, but cannot
claim complete success.

### 11. Audit production only when requested

```sh
onwardpg drift check \
  --target primary \
  --database "$PRODUCTION_DATABASE_URL"
```

This compares replayed H with live P, read-only. It detects missing accepted
schema changes and manual production catalog drift. It is encouraged as a
periodic health check, not required for every feature plan and never used as
the PR migration baseline.

## Active-plan identity and the narrow Git hint

onwardpg needs to know which one bundle is mutable. It cannot infer “belongs to
this PR” from schemas or the migration chain alone.

The normal mechanism is a gitignored, worktree-local anchor such as:

```json
{
  "plan_id": "01J2...",
  "bundle": "migrations/onward/primary/0043_booking-dates",
  "branch_hint": "feature/booking-dates"
}
```

The manifest carries the durable `plan_id`. The local anchor makes repeated
`onwardpg plan` and `onwardpg verify` calls guessable. A fresh checkout or CI
may select the bundle explicitly.

Reading the current branch name is acceptable only as optional ergonomics:

- suggest the initial slug when no name was supplied;
- notice that the user switched branches;
- avoid accidentally reusing another branch's active plan; and
- present a useful label.

The branch name is never hashed into schema identity and never determines a
base, accepted history, ordering, mutability, or safety decision. Branch
renames, detached HEADs, CI, non-Git directories, and agents passing an
explicit name must all work.

The principle is:

> Git-derived hints may improve ergonomics; Git-derived claims never establish
> migration correctness.

onwardpg never fetches, pulls, rebases, merges, commits, pushes, discovers a
merge base, or decides what origin/main means. The coding agent is already good
at those jobs.

## Branch-switch behavior

When the optional branch hint no longer matches the local active anchor:

1. do not revise the old plan;
2. do not interpret objects absent from the new branch's W as drops;
3. look for an explicitly selected plan or require a new semantic name;
4. retain the old local anchor information so switching back can resume it;
5. report surplus D objects without cleanup SQL; and
6. suggest a disposable sandbox when same-name incompatible branch state makes
   workspace compatibility impossible.

An active plan's confirmed destructive intent is scoped to that plan ID and
its relevant fingerprints. It cannot authorize drops after switching to a
different plan.

## Command and stream contract

The target public workflow surface is:

```text
onwardpg init             establish a verified history baseline
onwardpg plan [name]      create, revise, or restack one feature plan and plan D -> W
onwardpg verify           clone-verify and receipt the active edited bundle
onwardpg drift check      compare history with a live catalog, read-only
```

Diagnostic and expert surfaces may remain:

```text
onwardpg status           inspect active identity, chain, base movement, and freshness
onwardpg diff             low-level explicit source-to-source schema diff
```

The common `plan` path replaces today's public ceremony:

| Current preview | Target |
| --- | --- |
| `history status`, copy `head_ref` | Infer the unique base after excluding the active plan |
| `draft --bundle NAME --after HEAD --create` | `plan NAME` |
| repeated `draft --bundle NAME --after HEAD` | `plan` |
| separate `dev plan` | Companion D to W workspace result from `plan` |
| explicit `--after` every time | `--onto` only when the content graph is genuinely ambiguous |

Output modes must not make stdout ambiguous:

- SQL mode writes only complete development reconciliation SQL to stdout;
- human status and warnings go to stderr;
- JSON mode returns one versioned object with separate durable and development
  statuses;
- no executable SQL is printed when the development result still needs a
  decision;
- a complete durable bundle may still be installed when only the independent
  development companion needs input, but that distinction must be explicit;
- incomplete, unsupported, or unverified durable work is never reported as a
  successful PR plan; and
- every nonterminal status has one clear next action.

The JSON exchange continues to contain only what cannot be inferred. It is a
decision and diagnostic protocol, not the authoring surface for complex
migration orchestration.

## What automatic restacking can and cannot prove

It can prove:

- the explicitly active logical plan was removed from consideration as base;
- every remaining accepted entry forms one content-addressed chain;
- the new base is an append-only descendant of the old parent;
- the refreshed bundle plans from that exact replayed base to W;
- preserved decisions still mean the same thing in their narrow scope;
- exact edited SQL converges in disposable PostgreSQL; and
- no residual catalog diff remains after all phases.

It cannot prove:

- that a branch is merged;
- that a chain-tip bundle has run in production;
- that a caller truthfully selected the PR-owned mutable bundle;
- that production has applied accepted history without an explicit drift
  audit;
- that historical data operations ran in D without a suitable postcondition;
- that application versions are ready for a contract phase; or
- that a logically valid migration is operationally safe at production scale.

Once a bundle has actually been published or run in a shared production
environment, the developer or agent stops selecting it as mutable and creates
a successor. There is intentionally no onwardpg `lock` command pretending it
can observe that organizational event.

## Safety invariants

1. The typed PostgreSQL dependency graph remains the sole production source of
   schema truth.
2. H to W and D to W are independently planned from independently
   fingerprinted sources.
3. The PR bundle is always based on replayed H, never D or P.
4. Development workspace mode preserves absent-from-W objects unless exact
   active-plan intent authorizes their removal.
5. Unknown or unmodeled catalog state blocks or is covered by a validated,
   reported narrow ignore selector.
6. Renames, destructive intent, casts, backfills, and rollout timing are never
   guessed.
7. A schema diff never claims to prove data-only history.
8. Transactional and nontransactional work remains in valid, explicit batches.
9. Regeneration never silently overwrites agent-authored SQL.
10. Verification executes only in onwardpg-created disposable databases.
11. Caller-owned development, production, and any future shared environment
    remain read-only to onwardpg.
12. There are no down migrations or production apply commands.

## Explicit non-goals

- Framework adapters, ORM snapshot integration, and ORM migration journals.
- A migration runner or environment application ledger.
- Production, staging, or development apply commands.
- Deployment orchestration, traffic observation, or compatibility-window
  automation.
- An embedded LLM, agent SDK, plugin API, or MCP server.
- Git mutation or Git hosting integration.
- Guessing product-specific SQL.
- Achieving another tool's complete PostgreSQL feature checklist before the
  core lifecycle is excellent.

PostgreSQL coverage remains important, but unsupported features must block
honestly rather than distract from this workflow. The machine-readable
inventories in `parity/*.json` remain the source of coverage claims and must
stay linked to tests and `docs/supported-features.md`.

## Current implementation versus target

The foundation is already substantial:

- live PostgreSQL and exported DDL flow through one typed catalog graph;
- DDL is materialized in disposable PostgreSQL rather than partially parsed;
- history bundles are deterministic and hash-chained;
- `init`, `history status`, `dev plan`, `draft`, `verify`, and `drift check`
  exist;
- semantic hints, narrow decision receipts, phased SQL, editable SQL,
  three-way regeneration, and clone verification exist;
- the planner never applies to caller databases; and
- PostgreSQL 15–18 integration coverage and machine-readable feature matrices
  exist.

The current DX still exposes a few implementation mechanics:

- the agent must call `history status` and copy a `head_ref`;
- the agent must distinguish `draft --create` from refresh;
- some expert/legacy commands still require bundle identity and copied base
  references;
- branch switching requires a semantic `plan NAME` invocation when the active
  bundle is absent; and
- accepted incoming phase work is inventoried; explicitly marked Boolean
  postconditions are evaluated read-only against D, while unmarked upstream
  assertions remain review-only evidence.

The next work is to simplify those mechanics without weakening the receipts or
the graph architecture.

## Implementation waves

### Wave 1 — executable lifecycle specification

- Add black-box fixtures for first plan, revision, ground advancement, branch
  switch, local prior application, surplus dev state, and accepted manual data
  work.
- Freeze the target CLI examples, stream behavior, exit statuses, and active
  plan state machine before changing commands.
- Define `strict_equal`, `workspace_compatible`, `needs_decisions`,
  `conflict`, and `blocked` precisely.
- Keep the current commands working behind tests until the replacement path
  proves equivalent behavior.

Exit: the full lifecycle can be tested against a fake CLI implementation using
golden filesystem, SQL, stderr, and JSON outputs.

### Wave 2 — stable plan identity and local anchor

- Add stable logical `plan_id` independent of folder name.
- Add gitignored worktree-local active-plan state.
- Support explicit selection for fresh checkouts and CI.
- Permit best-effort branch-name hints only under the narrow rules above.
- Detect branch-hint mismatch, missing bundle, stale anchor, and accepted
  descendants without mutating anything.
- Make path resequencing preserve logical identity and Git-visible edits.

Exit: `plan NAME`, repeated `plan`, branch switch, switch back, detached HEAD,
and fresh-CI explicit selection all choose the correct bundle or block clearly.

### Wave 3 — content-derived restacking

- Exclude the explicitly selected logical plan before validating base history.
- Infer a unique remaining accepted head when possible.
- Prove append-only movement from the old recorded parent.
- Replace mandatory `--after` with exceptional `--onto`.
- Reject altered accepted history, missing parents, remaining forks, ambiguous
  heads, and selected plans with accepted descendants.
- Preserve narrow decisions and phase edits using the existing conservative
  reconciliation machinery.

Exit: a feature absorbs two newly accepted bundles and remains one cumulative,
verified migration without any Git query or copied head token.

### Wave 4 — unified plan command and workspace-safe dev output

- Compose durable H to W planning and companion D to W planning under one
  command while keeping their sources, decisions, and statuses separate.
- Add workspace compatibility comparison that reports and preserves surplus
  state.
- Allow local removals only when exact active-plan destructive or rename intent
  applies.
- Render source, desired, base movement, preserved surplus, and non-PR warning
  comments in SQL output.
- Keep a low-level exact diff for expert use and drift inspection.
- Define atomic filesystem behavior when the durable result succeeds but the
  independent local result needs a decision.

Exit: switching branches never emits absence-driven drops, while an intentional
feature contract can still be deliberately tested in dev.

### Wave 5 — accepted non-catalog work accounting

- Identify accepted bundles traversed between the old and new base parent.
- Inventory generated structural phases separately from agent-authored,
  migrate, manual, and data-only phases.
- Report effects that the D catalog cannot prove.
- Evaluate only explicitly safe read-only postconditions; never infer success
  from catalog equality.
- Do not concatenate accepted SQL into the direct D to W output.

Exit: the agent receives correct structural local SQL plus an honest, complete
list of upstream data/manual effects requiring human or product-aware review.

### Wave 6 — finish the handoff and verification loop

- Make `verify` resolve the active plan by default.
- Preserve exact agent-owned SQL and assertions across restacks.
- Improve conflicts so all three inputs are visible without overwriting current
  bytes.
- Require full disposable replay, assertions, and empty residual before
  receipting success.
- Keep partial verification explicitly partial.
- Ensure `verify --check` is sufficient for a thin Git-aware CI wrapper to
  enforce one changed logical bundle and immutable accepted history.

Exit: no separate JSON orchestration model or production runner is needed to
complete a complex feature migration.

### Wave 7 — collapse the old surface and publish the preview

- Promote the new `plan` path in README and help.
- Retire ordinary use of `draft --after --create`, separate `dev plan`, and
  copied `head_ref` ceremony after compatibility fixtures pass.
- Keep expert diagnostics only when they expose genuinely distinct concepts.
- Update architecture, protocol, safety, supported-features, and comparison
  docs to observed behavior.
- Validate `parity/*.json` and prohibit documentation claims not backed by its
  linked tests.
- Run formatting, vet, race, static analysis, security review, PostgreSQL
  15–18 integration, release archive, checksum, and install tests.

Exit: a cold-read agent can use the product from `--help` and the README without
learning internal history mechanics.

## Acceptance narrative

One repository-local fixture must demonstrate the whole product:

1. Main contains a verified baseline and several accepted migrations.
2. A worktree starts `booking-dates` with `onwardpg plan booking-dates`.
3. The plan resolves a rename and writes one phased feature bundle.
4. The agent deliberately applies the printed dev SQL using external tools.
5. The feature adds another column; repeated `onwardpg plan` replaces the same
   cumulative bundle and prints only the local residual.
6. The agent adds a product-specific timestamp-to-date backfill and assertion
   directly to the bundle.
7. A buddy's additive migration and a second migration with a data step merge
   into main.
8. The coding agent rebases using Git outside onwardpg.
9. Repeated `onwardpg plan` detects the advanced content chain, restacks the
   active logical plan, preserves valid intent and agent SQL, and resequences
   the path if necessary.
10. The local SQL directly reconciles the actual dev catalog with the new W,
    including visible upstream structure and current feature increments.
11. The report names the buddy's data step as unprovable rather than silently
    replaying or ignoring it.
12. Surplus objects in the long-lived dev database are reported and preserved.
13. Switching to another branch neither reuses the old active plan nor emits
    drop SQL for the old branch's objects.
14. Switching back resumes the correct logical plan.
15. `onwardpg verify` executes exact edited bytes in disposable PostgreSQL and
    reaches an empty residual.
16. `verify --check` passes in CI using an explicitly selected bundle in a
    fresh checkout.
17. An optional H-to-P drift check reports a deliberately introduced production
    index difference without changing P or the feature plan.

Run the scenario on PostgreSQL 15–18. Add failure variants for:

- ambiguous base heads;
- altered accepted history;
- a missing active bundle;
- same-name incompatible cross-branch dev state;
- stale and contradictory decisions;
- generator/agent same-phase conflict;
- transactional rollback;
- failed nontransactional index creation;
- false verification assertions;
- unknown catalog state;
- unreceipted edits; and
- accepted data-only work with no postcondition.

## Developer-preview definition of done

The workflow is ready when a developer or coding agent can, without reading
implementation documentation:

1. initialize history;
2. create one named plan;
3. repeatedly call `plan` while changing declarative code;
4. deliberately apply complete stdout SQL to its chosen development database;
5. survive incoming main migrations without copying a history token;
6. understand which upstream data effects cannot be proven;
7. switch branches without destructive cleanup suggestions;
8. edit sophisticated backfill SQL directly in the migration folder;
9. verify the exact final bundle on a disposable clone;
10. pass a read-only CI check; and
11. hand the phases to existing deployment tooling with no onwardpg apply
    command.

The README walkthrough must be executable and match actual CLI bytes. The
implementation is not complete merely because unit tests pass: the lifecycle
fixture, real PostgreSQL 15–18 matrix, chain and edit failure tests, protocol
goldens, machine-readable coverage validation, documentation, and release
build must all agree.

## Decision filter for future work

Before adding a command, manifest field, protocol type, or integration, ask:

1. Can the typed schema graph or existing bundle receipt infer this?
2. Is it information only the developer or coding agent can know?
3. Can the answer live more naturally as readable SQL or a SQL comment?
4. Does it improve one evolving migration, local reconciliation, restacking,
   verification, or drift inspection?
5. Does it accidentally turn onwardpg into a Git client, migration runner,
   deployment system, ORM adapter, or application-specific DSL?

If the new concept is not required by those tests, it does not belong in the
developer-preview path.
