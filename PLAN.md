# onwardpg: simple decisions, editable SQL, verified replay

This is the product and implementation plan for onwardpg. It replaces plans
that made JSON an authoring language, taught the CLI about Git or pull requests,
or tried to model application-specific migration orchestration.

The product should feel obvious:

```sh
onwardpg dev plan --target primary
onwardpg history status --target primary
onwardpg draft --target primary --bundle customer-profile --after baseline
onwardpg verify --target primary --bundle customer-profile
```

Given PostgreSQL as it exists and CREATE-statement DDL as it should exist,
onwardpg explains the forward path. When schema state cannot establish intent,
it emits bounded decisions. A developer or coding agent selects among those
decisions, then takes ownership of readable phased SQL. onwardpg verifies the
exact edited migration in databases it created and never applies it anywhere
else.

The governing principle is:

> The agent states only intent that schema state cannot establish. onwardpg
> infers the rest, binds accepted intent to observed state, and hands everything
> richer to editable SQL and comments.

## Implementation checkpoint — 2026-07-13

The foundation already exists:

- top-level `init`, `history status`, `dev plan`, `draft`, `verify`, and
  `drift check` commands are Git-free;
- PostgreSQL catalogs and materialized CREATE-statement DDL feed the typed
  dependency graph;
- `init` and `draft` write deterministic hash-chained bundles;
- `draft --bundle --after` plans from an agent-asserted accepted history head
  to working DDL, not from the mutable development database;
- generated plans are clone-verified before they are written;
- agents may edit phase SQL and `verify.sql`, and verification receipts the
  exact edited files;
- repeated drafting preserves non-conflicting agent edits with conservative
  phase-level three-way reconciliation;
- the same selected bundle remains mutable as feature work evolves;
- parent hashes detect base erosion, while mandatory `--after` prevents a
  feature from accidentally stacking on another unpublished bundle;
- `history status` exposes the ordered head and whether a selected bundle is
  current or stale without requiring a database or Git checkout;
- `verify --check` validates receipts without modifying the checkout; and
- `drift check` is an explicit, read-only audit rather than part of ordinary
  feature planning.

The first decision-loop slice is now implemented:

- strict semantic `rename`, `drop`, `type_change`, `rollout`, `confirm`, and
  `manual_sql` hints use structured identifier arrays and reject unknown fields;
- hints may be supplied before planning or copied directly from
  `needs_decisions` output;
- staged planner questions consume hints iteratively, including one drop hint
  resolving both rename rejection and destructive confirmation;
- unused, impossible, duplicate, contradictory, and malformed hints fail;
- internal answers retain narrow scope fingerprints without exposing them to
  the authoring exchange;
- accepted hints persist in generated `decisions.json` receipts;
- `onwardpg/draft/3` decision output contains only semantic choice sets and
  choice-specific hazards;
- `manual_sql` produces `needs_sql_edits` plus a phase-local `ONWARDPG TODO`,
  never an incomplete success; and
- real PostgreSQL acceptance tests cover semantic rename, clone verification,
  base erosion, manual type SQL handoff, edited verification, and receipt
  transition.

The same semantic hints now work in `draft`, `dev plan`, and low-level `plan`.
No command exposes the internal fingerprint-bound answer format as author
input. Type-change and NOT NULL hints omit fields implied by their kind, and
an agent may safely resend one complete ahead-of-time hints file. The unused
free-form `intent.md` channel and low-level bundle writer have also been
removed: bounded hints carry decisions, editable SQL carries everything richer,
and only `draft` writes durable history.

Same-phase conflicts preserve old-generated, current-edited, and new-generated
SQL; generated statements include review comments; and `verify --check`
rejects stale working DDL, unresolved TODOs, unreceipted edits, and invalid
history with typed findings. Git-era commands, packages, fixtures, metadata,
and source-receipt kinds have been removed. Transactional rollback,
non-transactional failure, false assertions, cancellation, scratch cleanup,
and the lifecycle have been exercised on PostgreSQL 14–18. Deterministic
release archives, checksums, embedded versions, performance bounds, and an
internal security review now exist. The vulnerability scan is clean with Go
1.26.5 and pgx 5.9.2. The project is licensed under the MIT License.

The final workflow audit adds exact golden bytes for the minimal decision
protocol; real-PostgreSQL additive draft, convergence, and byte-stability
acceptance; multiple-candidate semantic rename selection; an executable
no-apply assertion; and removal of the abandoned execution-receipt lifecycle.
The complete suite, pinned Atlas/Stripe corpus, and black-box README workflow
pass on PostgreSQL 14–18 from clean disposable servers. The human-owned license
decision is resolved as MIT and release archives include it.

## North star walkthrough

### 1. Configure desired DDL and disposable PostgreSQL

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_command = ["pnpm", "db:export-schema"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
```

`schema_command` prints PostgreSQL CREATE statements. `schema_file` is the
equivalent static-file option. Drizzle, Django, Prisma, SQLAlchemy, or a custom
script may produce that DDL; onwardpg does not need framework adapters.

The scratch server is also the source of truth for the PostgreSQL major. The
detected major is receipted automatically; configuration does not repeat it.

The scratch URL must name a PostgreSQL server on which onwardpg may create and
drop disposable databases. It is not an application database.

```sh
onwardpg config check
```

This materializes the DDL in disposable PostgreSQL, introspects it through the
same catalog path used for live databases, and prints a stable fingerprint.
Unknown catalog state blocks instead of disappearing.

### 2. Establish the replayable ground floor

```sh
onwardpg init --target primary
```

`init` creates the first receipted bundle from empty PostgreSQL to the current
desired schema. For an existing application, this is a replayable baseline; it
does not mean onwardpg applied anything to production.

```text
migrations/onward/primary/
└── 0001_baseline/
    ├── manifest.json
    ├── plan.json
    └── phases/
        ├── expand.sql
        ├── migrate.sql
        └── contract.sql
```

Every later entry names its parent digest. Ordering comes from the chain, not
from filenames, Git history, or an ORM journal.

### 3. Iterate cheaply against the development database

```sh
onwardpg dev plan --target primary
```

This compares the development catalog with working DDL and prints phased SQL.
It never executes that SQL. The agent may apply it through the project's normal
database tooling, keep developing, and rerun the command.

If another column is added tomorrow, the next `dev plan` shows only the local
residual. Applying development SQL is not a migration-history event.

### 4. Draft one logical feature migration

The coding agent first uses its Git context to identify the accepted history
tip in the base checkout, then asks onwardpg to validate that local history:

```sh
onwardpg history status --target primary
```

```sh
onwardpg draft \
  --target primary \
  --bundle customer-profile \
  --after baseline
```

`draft` replays every bundle except the explicitly selected feature bundle,
materializes the current desired DDL, and plans the cumulative transition from
the asserted accepted history head to working schema. The bundle ID identifies
the mutable feature; `--after` identifies the accepted predecessor onto which
it must be restacked. onwardpg validates both against the hash chain. It does
not inspect branches, commits, merge bases, or PRs.

Suppose history contains `users.name` and desired DDL contains
`users.display_name`. Schema state cannot prove whether this is a rename or a
replacement. Human output is concise and directly actionable:

```text
customer-profile needs 1 decision

  users.display_name
    rename users.name to users.display_name
      --hint '{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"]}'

    confirm removal of users.name; display_name is already implied by the DDL
      --hint '{"kind":"drop","object":"column","name":["public","users","name"]}'

Run the same command with one of these hints.
```

The JSON form is non-interactive and carries the same finite choice set:

```sh
onwardpg draft \
  --target primary \
  --bundle customer-profile \
  --after baseline \
  --output json
```

```json
{
  "protocol_version": "onwardpg/draft/3",
  "status": "needs_decisions",
  "next_action": "rerun_same_command_with_hints",
  "decisions": [
    {
      "choices": [
        {
          "hint": {
            "kind": "rename",
            "object": "column",
            "from": ["public", "users", "name"],
            "to": ["public", "users", "display_name"]
          }
        },
        {
          "hint": {
            "kind": "drop",
            "object": "column",
            "name": ["public", "users", "name"]
          },
          "hazards": ["data_loss"]
        }
      ]
    }
  ]
}
```

That is the entire machine exchange. The protocol and status are needed to
interpret the response. The alternative semantic hints and choice-specific
hazards are needed to choose safely. The target, bundle, source and desired
graph fingerprints, question fingerprint, prose prompt, labels, opaque IDs,
and rerun command are omitted because the caller or onwardpg already knows them
or can derive them.

The agent knows from product context that this is a rename. It supplies only the
relationship schema state cannot establish:

```json
{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"]}
```

For the common one-choice iteration, that object is passed directly:

```sh
onwardpg draft \
  --target primary \
  --bundle customer-profile \
  --after baseline \
  --hint '{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"]}'
```

`--hint` is repeatable. For a batch, `--hints-file migration.hints.json` accepts
an array of the same semantic objects. There is no second inline-JSON spelling.

The format is intentionally predictable. If the coding agent already knows
from the code diff and feature context that the column was renamed, it may pass
the rename hint on the first invocation. It need not wait for onwardpg to ask.
The same is true for an intentional drop or an offered structural strategy.
onwardpg validates each predeclared hint against the actual graph diff, rejects
unused or impossible intent, and binds accepted intent to the relevant observed
object fingerprints in the receipt.

Accepted hints are written into the selected draft's `decisions.json`
immediately. If more decisions remain, the next invocation returns only those
decisions; the agent does not resubmit accepted hints or maintain a parallel
answer file. Valid intent survives later feature edits and base erosion through
their hidden narrow-scope bindings.

The agent never copies schema fingerprints, planner IDs, or a question token.
Object names are structured as identifier arrays so quoting and dots in names
are unambiguous. A consumed hint becomes a fingerprint-bound receipt. If later
state changes its meaning, onwardpg invalidates that receipt and returns the new
semantic choices. The only continuation for `needs_decisions` is always to
repeat the same command with one or more hints. `next_action` is a stable enum,
not a rendered shell command.

### 5. Take ownership of readable phased SQL

After bounded decisions are resolved, the selected folder is the handoff:

```text
migrations/onward/primary/customer-profile/
├── manifest.json       # chain, fingerprints, exact file receipts
├── decisions.json      # consumed semantic hints and hidden state bindings
├── plan.json           # generated operations, dependencies, hazards, batches
├── verify.sql          # optional agent-owned boolean assertions
└── phases/
    ├── expand.sql      # additions usable before compatible code deploys
    ├── migrate.sql     # backfills and staged transitions
    └── contract.sql    # removals after compatibility windows close
```

`decisions.json` is generated evidence. It is not the authoring interface and
should not need hand editing.

The SQL explains purpose and timing:

```sql
-- EXPAND
-- Purpose: introduce the replacement date while old application versions still
-- read and write payments.settled_at.
-- Timing: run before deploying dual-write application code.
-- Lock: brief ACCESS EXCLUSIVE metadata lock; no table rewrite expected.

ALTER TABLE public.payments ADD COLUMN settled_on date;
```

```sql
-- MIGRATE
-- Purpose: populate payments.settled_on before it becomes required.
-- Product rule is not derivable from schema state.
-- Replace this TODO with reviewed SQL and add a verification assertion.

-- ONWARDPG TODO: backfill public.payments.settled_on
```

```sql
-- CONTRACT
-- Timing: run only after all deployed application versions read settled_on and
-- the backfill assertion succeeds.
-- Hazard: drops data and takes an ACCESS EXCLUSIVE metadata lock.

ALTER TABLE public.payments
  ALTER COLUMN settled_on SET NOT NULL;
ALTER TABLE public.payments
  DROP COLUMN settled_at;
```

The coding agent may replace TODOs, rewrite generated statements, split work,
add deduplication or batching, and strengthen `verify.sql`. This is intentional:
the migration folder is reviewed code, not generated output that must remain
pristine.

There is no JSON schema for a product-specific backfill, dual-write rollout,
deployment wait, batch size, or business invariant. If onwardpg cannot derive
that work, it identifies the structural need and hands over SQL ownership.

### 6. Verify the exact edited migration

```sh
onwardpg verify \
  --target primary \
  --bundle customer-profile
```

`verify` creates fresh disposable databases, replays immutable parent history,
executes the exact phase files in order, evaluates `verify.sql`, introspects the
result, and proves an empty residual diff against current desired DDL. It then
records exact file digests and verification evidence in the manifest.

```text
verified customer-profile
  source:       sha256:97…
  desired:      sha256:ab…
  phases:       expand -> migrate -> contract
  assertions:   2 passed
  residual:     empty
  receipts:     current

next: review and commit the folder; onwardpg will not apply it
```

`verify --check` is the read-only CI form. It rejects unreceipted edits, stale
DDL or parent history, unresolved decisions or TODOs, failed assertions, broken
chain history, and non-convergence.

Verification proves replay, declared assertions, and structural convergence. It
does not claim that production timing, traffic, realistic data volume,
application compatibility, or an insufficient assertion is safe.

### 7. Keep editing the same feature

The feature bundle does not lock or finalize. If desired DDL changes, rerun the
same command with the same bundle ID and accepted predecessor:

```sh
onwardpg draft --target primary --bundle customer-profile --after baseline
```

It remains one cumulative history-head-to-working-schema migration even if an
earlier draft was applied to the developer database. Development state and
durable migration state are separate.

When generated SQL changes, onwardpg compares:

1. the last generated phase;
2. the agent-edited receipted phase; and
3. the newly generated phase.

One-sided changes are preserved or refreshed. If the generator and agent both
changed the same phase, onwardpg does not overwrite or attempt a SQL merge. It
leaves the current files intact and emits both candidate inputs plus a concrete
resolution action.

### 8. Absorb base erosion without Git integration

During a multi-day feature, the developer or agent pulls and rebases normally.
The agent identifies the new accepted history tip from its Git context, checks
it with `history status`, and reruns the same bundle with the new anchor:

```sh
onwardpg history status --target primary --bundle customer-profile
onwardpg draft \
  --target primary \
  --bundle customer-profile \
  --after upstream-settings
```

If an unrelated unpublished bundle is also present after that accepted tip,
onwardpg blocks instead of silently stacking the feature on it. If accepted
history now contains the complete generated feature change, onwardpg removes
the untouched redundant draft atomically and reports `absorbed`; it never
discards agent-edited SQL this way.

Unchanged scoped decisions survive. Changed decisions are invalidated
individually and returned with new semantic choices. Agent-edited phase SQL is
reconciled conservatively. A remaining fork, missing entry, altered receipt, or
ambiguous head blocks with exact paths and digests.

Git and the coding agent answer which files are accepted and name that tip.
onwardpg proves that the files form one safe replay chain, that the asserted tip
is the actual predecessor, and that the selected migration still converges.
Neither tool impersonates the other.

### 9. Hand off; do not apply

The developer reviews and commits the bundle. Their existing deployment and
database tools run each phase with the necessary operational visibility.

onwardpg has no production apply command, no caller-database apply command, no
migration runner, no down migrations, and no opinion about the repository's Git
hosting or merge strategy.

### 10. Audit production drift separately

```sh
onwardpg drift check \
  --target primary \
  --database "$PRODUCTION_DATABASE_URL"
```

This explicit command compares replayed history with a live catalog read-only.
It is useful for occasional erosion audits. It is never invoked implicitly by
`draft`, and production is not the normal baseline for feature migrations.

## Product boundaries

### The four schemas

- **H — history head:** replay of accepted migration bundles. This is the
  baseline for the next durable migration.
- **D — developer database:** mutable local feature state.
- **W — working schema:** CREATE-statement DDL exported from current code.
- **P — production:** inspected only by an explicit drift audit.

```text
local development:      D --dev plan--> W
feature migration:      H ---draft---> W
occasional drift audit: H -drift check-> P
```

The most important invariant is that `draft` never derives the next production
migration from D.

### Decisions are finite selections

A decision must satisfy all of these rules:

- onwardpg emitted it from a specific source/desired state;
- every offered hint is a complete semantic statement of one valid choice;
- the agent may copy an offered hint or predeclare the same semantic intent;
- every distinct input hint resolves a real current decision or is rejected as
  unused; resending an already-receipted identical hint is idempotent;
- accepted intent is deterministically bound to the narrow participating-object
  scope in its receipt;
- unrelated schema changes do not invalidate it;
- a changed object, candidate set, or planner meaning does invalidate a stored
  receipt;
- duplicate, contradictory, malformed, impossible, stale, and unused hints are
  errors; and
- accepted hints are persisted with their full hidden fingerprints as
  receipts.

The exchange follows a strict minimality test:

- every field emitted by onwardpg must be required to parse the response or to
  choose safely using product context;
- every field returned by the agent must be information onwardpg cannot derive;
- no invocation argument is echoed merely for correlation;
- no fingerprint is copied back to the process that calculated it;
- no opaque decision or choice token is introduced merely for correlation;
- no prose is returned to a machine that already emitted the prose; and
- no accepted hint must be resubmitted after onwardpg has receipted it, though
  resending a complete unchanged hints file is safe.

An outbound decision is only a set of alternative semantic hints plus hazards
that differ between those alternatives. Human prompts and labels are rendered
in text mode from the hints; they do not bloat the JSON contract.

Initial semantic hint families are intentionally small:

- `rename`: object kind plus exact `from` and `to` identifier arrays;
- `drop`: object kind plus exact current identifier array;
- `type_change`: column identifier plus one explicitly supported strategy;
- `rollout`: affected column identifier plus a supported NOT NULL strategy;
- `manual_sql`: exact current/desired object scope handed to editable phase SQL.

A hint cannot contain arbitrary SQL, shell commands, prose, deployment steps,
fingerprints, or an unvalidated object identifier. If the domain is not finite,
onwardpg creates an editable SQL handoff instead.

### Output, interactivity, and input are separate

The target CLI uses orthogonal controls:

- `--output text|json` chooses presentation;
- repeatable `--hint '<json-object>'` supplies semantic intent;
- `--hints-file <path>` supplies an array of the same objects.

All modes are non-interactive. `--output json` never prompts. Text output is for
humans and remains copyable; JSON output is a stable protocol for agents and CI.
TTY detection does not change planning semantics.

The CLI should expose one versioned envelope with stable statuses such as:

- `planned`;
- `needs_decisions`;
- `needs_sql_edits`;
- `conflict`;
- `blocked`;
- `verified`; and
- `drift_detected`.

Every nonterminal response includes structured diagnostics. A status has one
documented continuation, so responses do not echo reconstructible command
lines. Exit codes distinguish successful completion, required input,
verification failure, safety blocking, and internal failure.

### Receipts are detailed; replies are not

The manifest, `decisions.json`, and `plan.json` retain:

- complete source and desired fingerprints;
- narrow decision-scope fingerprints;
- every candidate and accepted choice;
- dependency and batch ordering;
- planner version and options;
- hazards and unsupported-state diagnostics;
- generated and edited phase digests; and
- disposable verification evidence.

This detail makes a bundle auditable without making the agent echo it back.
`answers.json` as a hand-authored API is retired. Existing internal answer types
may remain as an implementation layer until semantic hints are translated and
validated at the boundary.

### SQL is the last-mile extension point

The phase files and `verify.sql` are the supported customization surface. We do
not add:

- a manual-operation JSON DSL;
- framework adapters or plugin APIs;
- ORM journal integration;
- an embedded LLM or MCP server;
- Git, PR, merge-base, or hosting-provider awareness;
- migration-runner handoffs;
- a production or development apply command; or
- down migrations.

An optional ordinary Markdown note may be allowed as an agent-owned bundle
file, but it must not be required to complete or verify a migration. Critical
sequencing and hazards belong beside the SQL they govern.

### Precise safety claims

- **chain-valid:** hashes and parents form one replayable chain.
- **structurally convergent:** all phases end at desired catalog state in fresh
  disposable PostgreSQL.
- **receipted:** checked files match exact verified digests.
- **drift-free:** an explicit live audit matched replayed history.
- **deployment-safe:** never claimed by onwardpg.

An unresolved decision, TODO, unknown catalog family, false assertion, stale
receipt, or incomplete residual cannot be reported as success.

## Target CLI

```text
onwardpg config check    validate configuration and desired DDL
onwardpg init            create the replayable ground floor
onwardpg history status  inspect chain head and selected-bundle freshness
onwardpg dev plan        show caller-owned dev catalog -> working DDL
onwardpg draft           create or refresh one anchored feature bundle
onwardpg verify          clone-verify and receipt exact edited SQL
onwardpg drift check     compare history with a live catalog, read-only
onwardpg plan            low-level explicit source-to-source diff
```

There is deliberately no `answer`, `lock`, `finalize`, `apply`, `deploy`, `pr`,
or `rebase` command in the final surface. An agent supplies the accepted
predecessor from its Git context, repeats `draft` with semantic hints until it
receives SQL, edits the SQL, and repeats `verify` until the exact bundle
converges.

## Implementation plan

### Wave 1 — specify the minimal semantic hint contract

- Define small discriminated `Hint` variants and the `Decision` choice-set
  envelope.
- Use structured identifier arrays and canonical JSON; do not expose internal
  graph keys or opaque correlation tokens.
- Define the canonical inline object and file-array forms.
- Define rejection diagnostics for stale, duplicate, contradictory, unknown,
  invalid, and unused hints.
- Define `needs_decisions`, `needs_sql_edits`, and `conflict` independently.
- Document protocol versioning and exit codes before changing the CLI.
- Add a field-by-field minimality test to the protocol documentation: identify
  the consumer and prove why each field cannot be inferred.

Exit: golden protocol fixtures cover exact bytes, canonical ordering,
ahead-of-time hints, stored-receipt staleness, and minimality.

### Wave 2 — project existing questions into decisions

- Keep existing fingerprint-bound answer validation as the internal safety
  mechanism initially.
- Add a boundary layer that turns planner questions and candidates into minimal
  semantic choices and turns consumed hints into validated internal answers.
- Scope binding to participating objects so unrelated base erosion preserves a
  stored decision.
- Enumerate credible rename candidates as ready-to-submit semantic hints.
- Accept the same valid rename hint ahead of planning and reject any hint that
  does not resolve an actual current diff.
- Ensure `manual_sql` means “produce an incomplete editable handoff,” never
  “ignore the operation” or “assume arbitrary intent.”
- Persist accepted hints automatically; subsequent invocations request only
  unresolved or newly invalidated decisions.
- Persist the expanded internal evidence to generated `decisions.json` without
  requiring the agent to author it.

Exit: rename, destructive change, type conversion, and staged-transition tests
can complete with the same hints whether supplied before or after the first
planner response.

### Wave 3 — make the CLI loop guessable

- Add `--output text|json` consistently to high-level commands.
- Add repeatable `--hint` and `--hints-file` to `draft` and the low-level
  planning surface where decisions may arise.
- Keep every mode non-interactive and deterministic.
- Print exact copyable semantic hints; JSON returns only choice sets and
  choice-specific hazards.
- On stale input, return replacement decisions in the same response.
- Reject hint flags on commands or states that cannot consume them.
- Remove hand-authored `--answers` documentation, fixtures, and CLI aliases.

Exit: a coding agent can discover and complete a two-pass rename solely from
JSON responses without reading protocol documentation.

### Wave 4 — improve the SQL ownership handoff

- Render purpose, timing, lock, rewrite, data-loss, and availability comments
  directly above the statements they describe.
- Render product-specific unknowns as explicit blocking TODOs in the relevant
  phase, with expected structural effect and suggested assertion shape.
- Keep only `expand.sql`, `migrate.sql`, `contract.sql`, and `verify.sql` as the
  normal editable surface.
- Permit agents to replace or reorganize generated SQL without an amendment
  DSL.
- Improve same-phase reconciliation conflicts with old-generated,
  agent-edited, and new-generated artifacts plus one concrete resolution path.
- Never overwrite the current agent-edited file on a conflict.

Exit: a timestamp-to-date migration can choose a staged structure, hand the
business conversion to the agent, accept edited SQL and assertions, and verify
the exact result.

### Wave 5 — preserve decisions and edits through feature evolution

- Rebind unchanged narrow-scope decisions when working DDL changes elsewhere.
- Re-emit invalidated and newly required semantic choices; do not add receipt
  metadata to the minimal decision envelope merely to label the distinction.
- Preserve the same selected bundle across repeated local application and
  further DDL edits.
- Restack the selected bundle when its parent moves after incoming history.
- Carry non-conflicting agent edits and `verify.sql` exactly.
- Reject forks, altered base receipts, ambiguous heads, and multiple mutable
  entries without consulting Git.

Exit: a multi-day fixture absorbs two incoming migrations, preserves a rename
and backfill, adds another feature column, and still produces one cumulative
verified bundle.

### Wave 6 — complete verification and CI diagnostics

- Make `verify --check` the complete read-only gate for chain validity, desired
  DDL freshness, decision completeness, TODOs, exact receipts, assertions, and
  convergence.
- Test transactional rollback, non-transactional failure, false assertions,
  tampering, cancellation, and scratch cleanup.
- Make partial-phase verification report an explicit expected residual without
  claiming success.
- Ensure every refusal identifies what onwardpg preserved and the exact next
  command or file to edit.
- Exercise the complete workflow on PostgreSQL 14–18.

Exit: the README workflow is a real-PostgreSQL acceptance test and a thin CI job
can gate a repository using only `verify --check`.

### Wave 7 — remove superseded surfaces and release the preview

- Delete transitional Git-aware `pr` and Git-derived CI packages and commands.
- Delete or internalize the verbose answer-authoring API after semantic-hint
  fixtures cover every decision family.
- Remove filename-ordered replay after every fixture uses the hash chain.
- Align README, CLI reference, protocol, bundle, architecture, and safety docs
  with observed behavior.
- Keep PostgreSQL feature tables honest: modeled, manually completable,
  explicitly blocked, or unverified.
- Add release artifacts, checksums, version metadata, changelog, and a supported
  PostgreSQL policy.

Exit: a clean developer-preview build contains one product story and no dead
workflow that implies Git intelligence, JSON orchestration, or database apply.

## Acceptance scenarios

### A. Ordinary additive feature

- Add a table, foreign key, nullable column, and index.
- `draft` emits no decisions and readable expand SQL.
- `verify` reaches an empty residual and receipts exact files.
- A second identical draft is byte-stable.

### B. Rename with multiple credible candidates

- Remove one table and add two structurally compatible replacement candidates.
- JSON lists every credible rename plus deliberate drop-and-create.
- Supplying the semantic rename before or after the question produces the same
  intended rename.
- An invented, stale, duplicate, or unused hint is rejected.

### C. Destructive replacement

- Remove a populated column and add an incompatible replacement.
- onwardpg does not interpret similarity as permission to drop.
- The decision offers only supported structural strategies.
- Contract SQL is absent until destructive intent is explicitly selected.

### D. Product-specific backfill

- Change a timestamp column to the desired date representation.
- Select the bounded `manual_sql` handoff rather than inventing a cast.
- Receive a migrate TODO rather than invented business SQL.
- Replace it with agent-authored SQL and boolean assertions.
- Verify exact SQL and final convergence on disposable PostgreSQL.

### E. Feature iteration after local application

- Draft and apply feature SQL to a caller-owned dev database using external
  tools.
- Add another desired column.
- `dev plan` reports only D -> W residual work.
- `draft` refreshes the same cumulative H -> W bundle and preserves edits.

### F. Base erosion

- Insert two valid history entries beneath the selected bundle.
- Have the coding agent supply the new accepted tip with `--after`, without
  onwardpg reading Git metadata.
- Preserve unaffected intent and SQL, invalidate only affected receipts, and
  produce one bundle on the new chain head.
- Place a second unpublished bundle after the asserted accepted tip and prove
  that drafting blocks instead of stacking on it.
- Fully absorb an untouched generated draft into accepted history and prove
  that onwardpg removes it atomically with `status: absorbed`.

### G. Same-phase conflict

- Edit expand SQL and independently change desired DDL so generated expand SQL
  also changes.
- Preserve the current file.
- Return `conflict`, all three relevant artifacts, and a concrete resolution
  action without attempting a SQL merge.

### H. Failure and tampering

- Modify each receipted artifact independently.
- Fail transactional and non-transactional statements and an assertion.
- Leave no false-success receipt, report cleanup, and never touch a
  caller-owned database.

### I. Drift remains separate

- Create a live catalog with an extra index and a missing history change.
- `draft` never inspects it.
- Explicit `drift check` reports the divergence read-only.

### J. PostgreSQL cannot express the requested physical order

- Insert a desired column between retained columns in CREATE-statement DDL.
- Return a typed `column_physical_order` unsupported finding before any bundle
  is written.
- Never enter a late fingerprint-mismatch regeneration loop.

### K. Protocol, help, and partial verification

- Every top-level JSON envelope uses `protocol_version` and `status`; decision
  and SQL-edit handoffs include a stable `next_action`.
- Every command and subcommand `--help` exits successfully.
- Partial clone verification reports exactly which bundle phases were
  simulated and which remain; it never implies those phases ran in production.

## Developer-preview definition of done

A new developer or coding agent can follow the README to:

1. configure exported DDL and scratch PostgreSQL;
2. initialize a replayable ground floor;
3. inspect D -> W development SQL without automatic application;
4. identify the accepted predecessor and draft one cumulative H -> W feature
   bundle;
5. resolve a rename with a semantic hint it could have supplied in advance;
6. edit a product-specific backfill directly in phased SQL;
7. add verification assertions;
8. verify and receipt the exact edited bundle;
9. evolve the feature and restack it over incoming history by supplying the
   new accepted predecessor, without Git-aware onwardpg commands;
10. pass `verify --check`; and
11. understand from the CLI alone that deployment application is outside the
    product.

Completion is demonstrated by real PostgreSQL 14–18 acceptance tests, not only
unit tests or documentation. The same fixture must exercise rename intent,
manual backfill, a later feature edit, two incoming base migrations,
decision preservation and invalidation, edited-SQL reconciliation, exact
receipts, CI verification, and empty final residual.

The PR-anchor audit additionally makes the accepted predecessor explicit,
reports accidental unpublished stacking as a typed blocker, removes a
generated-only bundle when incoming accepted history fully absorbs it, reports
partial clone verification in terms of phases simulated and still remaining,
and rejects physical column-order transitions PostgreSQL cannot express rather
than failing late with an opaque fingerprint mismatch.

## Immediate next slice

Do not expand PostgreSQL feature coverage in this slice. Finish the PR-anchor
hardening as one coherent developer-preview change:

1. require and validate `draft --after`, expose `history status`, and cover
   stale, missing, conflicting, and absorbed bundle lifecycles;
2. reject impossible physical column ordering before bundle generation;
3. normalize command envelopes, help exits, config validation, and partial
   verification reporting;
4. execute the README, integration, race, static-analysis, and PostgreSQL 14–18
   suites from disposable servers; and
5. publish the first preview tag only if generated artifacts and observed CLI
   output still match the documentation.

This is the junction for every future proposal: if it does not make
replay, draft, decision, SQL ownership, or verification simpler and safer, it
does not belong in the developer-preview path.
