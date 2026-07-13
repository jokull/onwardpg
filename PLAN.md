# onwardpg: ultimate agent-native migration DX plan

## North star

onwardpg should answer one question exceptionally well:

> Given a specific schema that exists, a specific schema we want, and the
> developer's stated intent, what reviewed forward work gets us safely from
> here to there?

The tool is for developers and coding agents working in ordinary pull-request
flows. A feature branch may change its declarative schema many times, be open
for days while its base branch advances, and ultimately be squash-merged as one
logical change. The migration should describe the final feature, not preserve
the branch's exploratory history.

The ideal experience is therefore more than schema diffing. It combines:

- PostgreSQL-backed semantic diffing;
- explicit, resumable decisions where schema state cannot prove intent;
- one logical migration bundle per PR and database target;
- safe regeneration as the PR base erodes;
- expand/migrate/contract rollout guidance;
- immutable receipts once any work is executed;
- clone convergence proof; and
- CI checks that explain exactly why a PR is stale, stacked, incomplete, or
  unsafe to merge.

onwardpg never applies a migration to production. It may apply reviewed plans
only to disposable verification databases. Production execution remains an
explicit operator/deployment-system action.

## Product doctrine

1. **Diffs describe outcomes, not development history.** Intermediate branch
   edits do not deserve permanent migration files merely because they existed
   in a commit.
2. **One migration per PR means one logical bundle per target.** A bundle may
   contain several execution phases and transaction batches. It is not
   necessarily one SQL file, one transaction, or one deployment moment.
3. **The comparison boundary must be named.** A local dev reconciliation, a PR
   contribution, and a production release plan can all produce different
   correct diffs from the same branch.
4. **Draft work is replaceable; executed work is immutable.** A feature-branch
   bundle can be regenerated until any phase has run or the migration becomes
   reachable from the protected base branch. After that, corrections are new
   forward work linked to the original receipt.
5. **A remote push does not itself make a migration immutable.** Squash-merge
   workflows routinely rewrite feature-branch history. Execution or
   publication into the protected migration lineage is the immutability gate.
6. **Schema state cannot establish application intent.** Renames, destructive
   operations, casts, backfills, rollout timing, refresh modes, and partition
   data movement require explicit decisions.
7. **No receipt, no merge.** Generated SQL without its source fingerprints,
   decisions, planner options, and clone result is an unverifiable artifact.
8. **No incomplete success.** Unknown catalog state, unused/stale answers,
   missing phases, invalid transaction boundaries, or residual diff must
   produce a non-success result.
9. **PostgreSQL is the semantic authority.** Declarative DDL is materialized in
   disposable PostgreSQL and catalog-inspected; onwardpg does not grow a
   partial SQL parser.
10. **Forward-only is an operational rule.** Recovery is another reviewed
    forward plan. Generated down migrations are not a substitute for recovery
    design.
11. **Simple is a product constraint.** The ordinary workflow stays: identify
    current and desired schemas, answer only genuine ambiguities, and receive
    one reviewable forward plan. Git, GitHub, ORM journals, bundle storage, and
    CI are optional adapters around that workflow. They must not leak into the
    semantic engine or become prerequisites for using it.
12. **Complexity belongs in receipts, not incantations.** The CLI may perform
    deep verification, but the golden path must remain one discoverable
    command with actionable output. Advanced flags expose composition points;
    they do not replace a good default.

## The four comparison boundaries

The current CLI exposes generic `--from` and `--to` sources. The next DX layer
must make the purpose of each comparison explicit because using the wrong
boundary produces a plausible but incorrect migration.

| Mode | Current / `from` | Desired / `to` | Question answered | Durable output |
| --- | --- | --- | --- | --- |
| **Develop** | Developer's local/dev database | Current checkout's declarative schema | “What local database work lets me keep developing?” | Usually none; scratch plan only |
| **PR** | Schema represented by the PR base commit/ref | Feature branch's final declarative schema | “What schema contribution does this PR make?” | One feature migration bundle per target |
| **Release** | Real deployment database or fresh clone | Schema represented by the release commit | “What must this environment actually receive, including lag or drift?” | Reviewed deployment plan/receipt |
| **Verify** | Disposable clone after applying selected phases | Expected checkpoint or final desired schema | “Did the reviewed work produce the promised state?” | Clone execution and residual-diff receipt |

These modes share the same typed graph and planner, but they have different
provenance and policy:

- A dev database may contain abandoned experiments and must never silently
  become the PR baseline.
- `origin/main` describes repository history, not necessarily deployed
  production state.
- Production may lag main, contain approved hotfixes, or have accidental
  drift; a production release plan is therefore not interchangeable with the
  PR diff.
- A PR bundle proves what the PR contributes relative to its base. A release
  plan proves what a particular database still needs.

The manifest and CLI must call these boundaries by name. Generic `plan` remains
the low-level engine; agent-facing workflows use explicit modes.

## The PR schema square

A PR is not one diff. It has four schema states that must agree:

```text
base declarative schema ───── PR intent diff ─────► head declarative schema
          │                                                   │
          │ base integrity                                    │ history fidelity
          ▼                                                   ▼
base bundle chain replayed ── replay PR phases ──► head bundle chain replayed
```

Name them explicitly:

- **BC**: declarative code schema at the exact base commit;
- **BM**: schema produced by replaying the base onwardpg bundle chain;
- **HC**: declarative code schema at the PR head/working-tree digest; and
- **HM**: schema produced by replaying the base chain plus the PR's complete
  onwardpg phase bundle.

The ordinary invariant is `BC == BM` and `HC == HM`. The horizontal top diff
is what the feature means. The horizontal bottom transition is what the
reviewed onwardpg history will do on a fresh database. The vertical comparisons catch missing,
stacked, stale, or hand-edited artifacts.

If `BC != BM`, the base branch is already unhealthy. The feature PR must not
quietly absorb that mismatch and call it its own migration. CI reports a base
integrity failure or an explicitly accepted inherited exception. If `HC != HM`,
the PR migration does not implement the schema its code declares.

The local dev database is a fifth, deliberately non-authoritative state. Its
diff to `HC` helps the developer work, but it never substitutes for any corner
of the PR square.

`HC` is the tree that would actually result from merging the PR contribution
into the exact recorded base, not necessarily the raw feature-branch checkout.
When the base advanced after the branch forked, the caller must supply that
would-be merged tree and a content receipt. The optional Git wrapper constructs
it from `base + (merge-base → head patch)` in isolation; GitHub CI may use the
platform's synthetic merge ref. The core neither invokes Git nor trusts a ref
name as schema content. A merge conflict is an explicit wrapper/CI blocked
state, never a guessed desired schema. Dirty trees are identified by a digest
of the actual compiled tree, not by a Git patch-format digest.

Deferred contract work complicates exact `HC == HM` at merge time. In that
case the bundle records a real-PostgreSQL post-expand checkpoint plus an
explicit residual plan to final `HC`. CI requires every residual operation to
be classified as deferred migrate/manual/contract work. Code compatibility
with that intermediate schema is a developer assertion; it cannot be inferred
from DDL alone.

## “One migration per PR” contract

The default rule is:

> For each configured database target whose declarative schema changes, a PR
> owns exactly one logical migration bundle from its base schema to its head
> schema.

### What counts as one

- One bundle can contain `expand`, `migrate`, `manual`, and deferred `contract`
  artifacts.
- Transactional and non-transactional batches remain separate even within one
  phase.
- A monorepo PR changing two independent databases has one bundle per target,
  not one bundle for the whole repository.
- A migration-only drift repair or delayed contract PR may have a bundle even
  when the declarative application schema does not change, but its manifest
  must declare that purpose and lineage explicitly.
- A PR with no schema change must not add an empty migration bundle.

### What counts as stacking

CI should reject, unless an explicit exception applies:

- multiple new onwardpg bundles for one target created by exploratory edits on
  the same PR;
- a new bundle plus unaccounted branch-local phase artifacts;
- a bundle whose baseline predates migrations now present on the PR base;
- a manually edited SQL file whose digest no longer matches `plan.json`;
- an execution command that collapses phases or transaction boundaries without
  an explicit reviewed receipt;
- reordering, deleting, or editing any migration reachable from the protected
  base branch; and
- replacing any bundle generation that has an execution receipt.

### Legitimate exceptions

- multiple configured database targets;
- a successor generation required because part of the original bundle was
  already executed before the PR changed;
- a deliberately separate follow-up contract PR linked to the original expand
  bundle;
- emergency forward repair after a failed or partially applied operation; and
- explicitly modeled stacked PRs whose base is another feature branch. Once
  the parent merges, the child must regenerate against the new protected base.

Exceptions never erase history. They form a typed lineage (`supersedes`,
`continues`, or `repairs`) inside one logical feature change.

Manual control remains a core feature. Raw edits invalidate the generated
receipt, but the developer may adopt an edit as a versioned amendment that
records the affected statement IDs, rationale, phase, execution mode, hazards,
and verification. The amended artifact must pass clone convergence. “No
unreceipted edits” is the guardrail; “generated SQL is untouchable” is not.

## Bundle lifecycle and immutability

Each target-specific bundle follows this state machine:

```text
developing
    │
    ▼
generated ──► needs_input ──► planned ──► clone_verified ──► reviewed
    ▲               │             │              │              │
    └── regenerate ─┴─────────────┴──────────────┘              │
                                                                ▼
                                                    expand_executed (immutable)
                                                                │
                                                    application_merged
                                                                │
                                                     migrate_observed
                                                                │
                                                      contract_ready
                                                                │
                                                         complete
```

Before execution, a generation becomes stale and replaceable when:

- the base commit/ref advances;
- the base schema fingerprint changes;
- the head declarative schema changes;
- planner version or schema-affecting options change;
- a checked-in answer no longer matches the questions; or
- generated SQL or adapter output changes.

After any phase is executed, the generation is immutable. If the PR then
changes, onwardpg produces a successor forward generation from the environment
checkpoint to the new desired state. It must not rewrite the already executed
SQL merely to recover the appearance of one file.

Reachability from the protected base branch also makes bundles immutable,
whether or not production has executed them yet. This protects the
content-addressed onwardpg history chain.

## Phase semantics in a squash-merge workflow

One logical bundle does not imply every phase should run at the same time.

| Phase | Typical timing | PR behavior |
| --- | --- | --- |
| `expand` | Before the application merge/deploy | Reviewed phase artifact may be run explicitly after approval |
| `migrate` | After compatible code exists, often observed over time | Runbook or reviewed data job; never invented from schema state |
| `manual` | At its explicitly reviewed operational point | Operator-owned SQL and verification with declared execution mode |
| `contract` | After old code and old data paths are gone | Deferred artifact or separate linked contract PR; never accidentally collapsed into expand |

For a purely additive change, `expand.sql` may be the entire deployable
migration and can be applied ahead of squash merge. For a rename/type change,
the safest plan often spans releases:

1. expand with a shadow/new column or compatibility object;
2. deploy dual-read/dual-write code;
3. backfill and verify;
4. deploy code that no longer uses the old shape; and
5. contract in a later linked PR.

A direct rename can still be planned when explicitly approved, but the tool
must describe why it cannot safely be applied ahead of code that uses the old
name.

### Strategy selection beyond a raw diff

For high-risk mutations, the best “how to get there” is often an intermediate
schema that does not appear in either endpoint. The planner should offer typed
strategy choices, not silently choose one:

| Mutation | Possible reviewed strategies |
| --- | --- |
| Column/table rename | Direct contract rename; or add compatible shadow shape, dual-write/backfill, then later contract |
| Type change | Direct cast with explicit `USING`; shadow column plus backfill/swap; or manual transition |
| `NOT NULL` | Direct lock/scan; or check `NOT VALID`, validate, set not null, remove helper constraint |
| Foreign key/check | Direct add; or `NOT VALID` then validate at an observed point |
| Large index | Ordinary transactional create; or concurrent non-transactional create |
| View/routine change with materialized dependent | Rebuild; retain where PostgreSQL proves identity; or explicit refresh contract |
| Partition change | Direct supported attach/detach; or operator-authored reconfiguration/data-movement contract |

onwardpg may generate structural scaffolding and checkpoints for a chosen
strategy. It still cannot infer dual-write application code, backfill meaning,
acceptable lock duration, cast correctness, or data verification. Those remain
intent/manual receipts. Strategy selection should make sophisticated rollout
patterns approachable without disguising them as automatic schema inference.

If an expand phase is applied from an unmerged PR and the PR is abandoned, the
database now intentionally differs from main. onwardpg must report that as an
executed orphan expansion, not encourage deletion or a down migration. The
operator chooses a forward cleanup bundle or leaves the compatible addition in
place for future adoption.

Pre-merge execution has two additional hard preconditions:

- immediately before execution, the target database fingerprint must equal the
  generation's expected deployment baseline; and
- the operator must identify the exact bundle generation, phase digest, target,
  and before/after fingerprints in an execution receipt.

There is no application migration-runner journal to keep in sync. The catalog
is the applied-state journal: replanning against the environment reports the
residual expand/migrate/contract work. Execution receipts prove who ran which
reviewed artifact and when; catalog inspection proves what state actually
exists. If they disagree, onwardpg reports drift rather than guessing which
bookkeeping source is authoritative.

## Required bundle receipts

The recommended repository shape is:

```text
migrations/onward/<target>/<feature-id>/
├── manifest.json
├── intent.md
├── decisions/
│   ├── attempt-001.json
│   └── attempt-002.json
├── answers.json
├── amendments.json        # optional reviewed changes to generated statements
├── plan.json
├── phases/
│   ├── expand.sql
│   ├── migrate.sql
│   ├── manual.sql
│   └── contract.sql
├── verification/
│   ├── clone-before.json
│   ├── execution.json
│   ├── clone-after.json
│   └── residual-plan.json
```

Empty phase files are omitted. `contract.sql` is a deferred proposal unless a
later execution receipt explicitly advances it. The bundle itself is the
migration-history entry; there is no translation into Drizzle, Django, Prisma,
Alembic, or another runner format.

`manifest.json` is versioned and machine-readable. At minimum it records:

```json
{
  "protocol_version": "onwardpg.bundle/v1",
  "bundle_id": "customer-profile",
  "target": "primary-postgres",
  "purpose": "feature",
  "history": {
    "parent_digest": "sha256:...",
    "entry_digest": "sha256:..."
  },
  "base_ref": "origin/main",
  "base_commit": "<full git sha>",
  "head_commit": "<full git sha or dirty-tree digest>",
  "baseline_source": {
    "kind": "onwardpg_history",
    "fingerprint": "sha256:..."
  },
  "desired_source": {
    "kind": "adapter_ddl",
    "adapter": "drizzle",
    "fingerprint": "sha256:..."
  },
  "planner": {
    "version": "...",
    "options": {}
  },
  "plan_digest": "sha256:...",
  "phase_digests": {},
  "amendments_digest": null,
  "lineage": null
}
```

URLs, credentials, and secret environment values must never be persisted.
Source receipts store safe identities, commit SHAs, PostgreSQL major version,
and schema fingerprints.

## Onwardpg-owned history and declarative compilers

Each target has one append-only, content-addressed bundle chain. A merged
bundle records the exact prior `parent_digest` it extends; its `entry_digest`
commits to that parent plus the reviewed plan, decisions, amendments, and phase
artifacts. Chain traversal, validation, and replay are pure and do not depend
on directory names, timestamps, lexical SQL filenames, Git commit order, or an
ORM journal.

This deliberately turns base erosion into an ordinary freshness event. Two PRs
may begin from the same parent digest, but only the first merged child extends
the protected chain. The other must regenerate against the new chain head;
merge-queue CI enforces that serialization. A bundle path remains a stable
human feature identity while the hash link supplies ordering and collision
proof.

Declarative integrations have one responsibility: compile application schema
state into executable genesis DDL or a complete typed snapshot. For example,
`drizzle-kit export` may compile `schema.ts` to DDL, but onwardpg never reads or
writes Drizzle's migration journal, snapshots, or runtime `migrate()` ledger.
The same boundary applies to Django, Prisma, and SQLAlchemy: use their schema
export capability when useful; keep runner-specific migration machinery out of
the onwardpg lifecycle.

The ordinary invariants become:

- `BC == BM`: base declarative export equals replay of the protected onwardpg
  bundle chain;
- `HC == HM`: head declarative export equals replay of that chain plus the
  complete proposed PR bundle on a disposable database; and
- a live environment's residual diff identifies phases it still owes without
  consulting an applied-migration ledger.

## Two speeds, one semantic engine

The dev loop must stay cheap. An agent can compare a disposable/local
development database with the current declarative export, review mostly
expand-only SQL, apply it through the developer's normal database client, and
re-diff. No bundle, Git receipt, history-chain update, or CI ceremony is
required. onwardpg still emits rather than auto-applies SQL.

The PR/release loop adds the ceremony that production work needs: exact source
receipts, one logical bundle, explicit ambiguities, phased SQL, immutable
history links, clone proof, and execution attestations. Both speeds use the
same catalog snapshots and graph planner; the difference is durable policy,
not schema semantics.

For every environment, the catalog is the state journal. Bundle-chain history
proves what the repository intends; execution receipts prove what an operator
attempted; a fresh residual diff proves what the database still needs.

## Proposed agent-facing CLI

The existing `onwardpg plan --from ... --to ...` remains the low-level,
source-explicit primitive. The next layer should make lifecycle intent obvious:

```sh
# Scratch reconciliation; no PR artifact by default.
onwardpg dev diff --target primary-postgres

# Inspect whether the branch needs a bundle and whether an existing one is stale.
onwardpg pr status --base origin/main --target primary-postgres

# Generate/replace one unpublished feature bundle from exact base to head.
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile

# Rerun with reviewed, fingerprint-bound answers.
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile \
  --answers migrations/onward/primary-postgres/customer-profile/answers.json \
  --replace-draft

# Apply only to a disposable clone and demand phase/final convergence.
onwardpg bundle verify migrations/onward/primary-postgres/customer-profile \
  --clone "$CLONE_DATABASE_URL"

# Compare actual deployment state with the release schema.
onwardpg release plan \
  --from "$PRODUCTION_READ_ONLY_URL" \
  --release HEAD \
  --target primary-postgres

# Run the same deterministic repository policy as CI.
onwardpg ci check --base origin/main --head HEAD
```

The repository supplies `.onwardpg.toml` (or equivalent) with targets,
adapters, schema-generation commands, migration paths, and policy. Commands
resolve refs to full SHAs and record them. They do not silently fetch, rebase,
checkout, commit, push, or apply production SQL.

`pr status` is read-only. `pr regenerate` may replace only classified draft
artifacts and must print its complete proposed mutation set before doing so.

## Agent decision loop

An agent may have developer intent in context, but every decision still leaves
a typed receipt:

1. Generate an answer-free PR plan and store the full `needs_input` result.
2. For each question, cite the relevant intent in `intent.md`.
3. Select an offered rename/drop/direct-conversion choice only when the intent
   clearly establishes it.
4. Supply freeform SQL only for a manual-work contract the developer/operator
   actually specified and reviewed.
5. Ask the developer when intent is absent, contradictory, or operationally
   consequential.
6. Rerun with a complete answer file. Stale, duplicate, contradictory, invalid,
   or unused answers fail.
7. Save the ready plan and render phase files from it.
8. If SQL needs expert adjustment, import it as a typed amendment rather than
   silently editing the rendered artifact.
9. Replay the complete onwardpg bundle chain on a clone and save the empty
   residual result or the explicit deferred residual plan.

The agent must never choose answers merely to reach exit code `0`. A clean
`unsupported` or `needs_input` result is a successful guardrail when the
alternative is guessed intent.

## Main-branch erosion and regeneration

Every PR bundle records an exact base commit and base schema fingerprint. CI
fetches the current PR base and classifies the bundle:

- **fresh:** exact base commit/fingerprint and head desired fingerprint match;
- **provenance-stale:** the prepared tree receipt changed, even if the
  resulting schema is equal;
- **schema-stale:** base or head schema fingerprint changed;
- **decision-stale:** questions/answers no longer match;
- **artifact-stale:** plan or SQL digest differs; or
- **immutable-successor-required:** a phase from the old generation was
  executed, so replacement is forbidden.

If `origin/main` gains unrelated migrations, regeneration should normally
produce the same feature operations with a new baseline receipt and migration
sequence/name. If it gains overlapping schema changes, the new diff may ask new
questions or reject a conflict. That is useful information; onwardpg must not
try to preserve the old SQL for aesthetic stability.

Git classification uses two boundaries at once: the merge-base-to-head diff is
the PR-owned contribution, while the exact current base tree defines protected
migration history and the regeneration baseline. Files newly added to the base
are not PR deletions. Editing or removing a file reachable from either the old
merge base or current base is an immutable-history violation; adding a branch
file at a path now occupied by the base is a concurrent identity collision.

If only the Git commit changes while `BC`, `BM`, and the feature diff remain
identical, onwardpg may fast-path semantic reuse, but it still reissues the
provenance and history-parent receipt. An unrelated merged bundle changes the
target's chain head even when it does not change this feature's SQL plan.

The base is the PR's configured base branch, not always `origin/main`. GitHub
Actions should use the pull request base SHA. A stacked PR may temporarily use
its parent branch as base, but must regenerate after the parent is squash-merged
because the commit and possibly migration identity changed.

## GitHub Actions and CI policy

Provide a versioned reusable action and a standalone `onwardpg ci check`
command. CI must be read-only with respect to the branch: it reports and emits
artifacts but never commits regenerated migrations behind the developer's back.

### Required checks

| Check | Failure condition |
| --- | --- |
| Schema-change detection | Head declarative schema differs from base but no target bundle exists |
| Base integrity | Base declarative schema and replayed base migration history differ |
| No-op bundle | Bundle exists without a declared schema/drift/contract purpose |
| Bundle cardinality | More than one unlinked bundle for one changed target |
| Base freshness | Manifest base SHA/fingerprint differs from the PR base |
| Head freshness | Desired fingerprint differs from current checkout output |
| Migration immutability | A base-reachable migration was edited, removed, or reordered |
| Draft replacement safety | A generation with an execution receipt was replaced |
| Answer validity | Missing, stale, contradictory, invalid, duplicate, or unused answer |
| Receipt integrity | Planner/options/source/plan/phase digests do not agree |
| History-chain integrity | Bundle parent is not the protected target chain head, entry digest is invalid, or history forks |
| Phase artifact integrity | Checked-in phase SQL differs from the reviewed plan/amendments/metadata |
| Phase policy | Deferred contract/manual work would be auto-applied with expand |
| Transaction policy | Non-transactional statements are wrapped or batch boundaries changed |
| Hazard policy | Repository-configured lock/rewrite/data-loss threshold lacks approval |
| Clone replay | Base cannot materialize, migration fails, rollback semantics fail, or residual diff remains |
| Unknown state | Catalog blocker is neither supported nor matched by a validated narrow ignore |

### PR summary

The action should publish a job summary and optional PR check annotation with:

- base/head commits and schema fingerprints;
- changed object counts by phase;
- unresolved questions and unsupported objects;
- hazards and transactional boundaries;
- whether the bundle is fresh, replaceable, or immutable;
- clone convergence status; and
- the exact local regeneration command.

It should upload the answer-free decision document and plan preview as CI
artifacts without exposing connection URLs or secrets.

### Merge queue

The merge queue creates a new effective base containing earlier queued PRs.
The action must run against the merge-group SHA and detect when the checked-in
bundle was planned against an older base. Initial policy should fail with a
clear regeneration requirement. Later, a bot may open/update a branch commit,
but it must never silently change an approved or executed bundle.

Repositories that require pre-merge production expand work may choose to leave
the queue and regenerate after earlier schema PRs merge. Pretending the old
baseline is still valid is not an acceptable optimization.

## Lifecycle scenario matrix

| Scenario | Expected onwardpg behavior |
| --- | --- |
| Feature schema changes repeatedly before review | Replace the same draft bundle generation; keep one logical PR bundle |
| Main receives unrelated migrations | Mark base stale; regenerate and reverify against new base |
| Main changes the same table/object | Re-diff; ask new intent questions or report conflict/unsupported transition |
| Feature branch merges/rebases main | Recompute both git and schema provenance; never assume conflict resolution preserved intent |
| Stacked PR targets another feature branch | Plan from parent branch; regenerate from protected base after parent squash merge |
| Two PRs extend the same target history parent | First merged child wins; the other becomes parent-stale and regenerates |
| PR has multiple exploratory onwardpg drafts | Replace the unexecuted draft with one final logical bundle |
| Existing migration is already on main | Refuse edit/delete; generate forward successor |
| Expand was applied before merge, then PR changes | Freeze executed generation; plan successor from recorded post-expand environment state |
| Expand was applied, then PR is abandoned | Report orphan expansion; require explicit forward cleanup/adoption decision |
| PR contains a destructive rename intended before merge | Explain old-code incompatibility; require staged design or explicit direct-operation approval |
| Backfill is needed | Emit migrate/manual contract and verification requirements; never synthesize data semantics |
| Contract must wait days/weeks | Leave the contract phase unapplied or create a linked contract PR; residual diff keeps it visible |
| Production lags several main migrations | Release mode plans from actual clone to release desired; PR bundle is not reused blindly |
| Production has drift absent from git | Release mode surfaces it as an explicit extra change/question |
| Developer's local DB has abandoned objects | Dev mode reports them; PR mode ignores local DB as a baseline |
| Declarative schema changed but migration is intentionally deferred | Require an explicit policy waiver with reason/expiry; do not accept silent absence |
| Migration-only hotfix or drift repair | Allow declared `repair` purpose with database baseline and forward lineage |
| Multiple databases change in one PR | One target-specific bundle and verification result per database |
| Generated SQL is hand-edited | Digest failure until imported as a reviewed amendment or manual-work contract and reverified |
| SQL is applied pre-merge | Require the exact bundle/phase digest plus before/after catalog fingerprints; residual planning determines what remains |
| Main advances after pre-merge expand | Executed generation stays immutable; re-check live fingerprint and plan a successor instead of renumbering/replacing it |
| No schema difference remains after rebase | Remove unexecuted draft bundle; retain immutable execution/history receipts if any |

## Architecture evolution

The typed graph planner remains the sole semantic planning engine. The
lifecycle core is Git-free and consumes prepared directories, typed snapshots,
and caller-supplied provenance receipts. Git wrappers and declarative schema
compilers are thin edge adapters. This boundary is a release constraint: no core planning,
freshness, bundle, history, or verification package may execute Git.

```text
prepared current + desired inputs
                 │
                 ├── live PostgreSQL / DDL / typed snapshot
                 └── prepared base/head directories + content receipts
                                      │
                               typed graph snapshots
                                      │
                               graph diff/planner
                                      │
                            questions + plan + hazards
                                      │
                           bundle/freshness/history core
                                      │
                    phase artifacts + clone verification

optional edge adapters
  Git CLI / GitHub Action ──► prepared directories + provenance receipts
  Drizzle / Django / Prisma ─► DDL or typed snapshot
```

Required packages/interfaces:

- `workspace.Config`: targets, adapters, paths, phase policy, hazard policy;
- `provenance.SourceReceipt`: safe database/adapter/tree identity and digest;
- `workflow.PreparedInput`: caller-owned base/head directories, migration
  snapshots, content digests, and opaque provenance;
- `adapter.SchemaCompiler`: declarative source to DDL or typed snapshot;
- `history.Chain`: validate and replay ordered onwardpg bundle history;
- `adapter.BaselineMaterializer`: bundle chain to disposable PostgreSQL;
- `bundle.Manifest`: versioned lineage, digests, phases, and verification;
- `bundle.Amendment`: reviewed statement additions/replacements/removals with
  rationale, phase, hazards, and verification;
- `bundle.Store`: read/validate/replace draft generations atomically;
- `policy.Checker`: local/CI cardinality, freshness, phase, and hazard rules;
- `verify.CloneRunner`: disposable-only batch execution and residual inspection;
- `report.Summary`: stable JSON plus human-readable PR/terminal output.

The optional `gitbase` adapter may resolve refs, classify protected history,
detect merge conflicts, and prepare trees. The CLI may call it to preserve the
one-command local experience. The same core operation must also be callable
with explicit directories and receipts, while CI owns authoritative
Git-semantic checks such as reachability and mergeability. Git output is never
parsed in a semantic core package.

The production CLI must not gain a generic “apply” command. Clone verification
requires an explicitly disposable target and should stamp that fact in its
receipt.

## Current foundation to preserve

- `pgschema.Snapshot` is the sole production schema representation; the legacy
  flat planner is removed.
- Live URLs and `file://` DDL sources produce catalog-backed snapshots in a
  consistent read-only inspection transaction.
- Plans are forward-only and never auto-applied.
- `onwardpg.plan/v1` provides fingerprints, typed questions/answers, hazards,
  phases, and transactional/non-transactional batches.
- Unknown/unmodeled catalog state is explicit unsupported state unless matched
  by a validated narrow ignore selector.
- PostgreSQL 14–18 is the supported server policy.
- Schemas, tables, columns, common constraints/indexes, enums, standalone
  sequences, extensions, ordinary/materialized views, modeled routines,
  triggers, and substantial partition behavior are graph-modeled within the
  documented feature boundary.
- Rename and manual-work decisions are fingerprint-bound. Materialized-view
  refresh and partition reconfiguration require operator-owned contracts.
- `parity/pgmig-roadmap.json` remains the product-facing feature map.
- `parity/atlas-postgres.json` remains a pinned reference-behavior study, not a
  release or one-for-one compatibility goal. When PostgreSQL planner behavior
  is unclear, the authenticated Atlas CLI may be used against disposable
  schemas as evidence; onwardpg's safety policy is decided independently.

### DX implementation progress

- Wave 0/1 foundation now includes deterministic statement IDs,
  `onwardpg.bundle/v1`, canonical artifact digests, preserved decision history,
  explicit safe draft replacement, strict `.onwardpg.toml` validation, and
  `onwardpg.diagnostic/v1` errors.
- Low-level `plan --bundle` writes non-empty phase files without changing the
  normal plan JSON or exit behavior. Source descriptions are redacted, and a
  real-PostgreSQL CLI integration test covers bundle creation.
- Read-only `pr status` now resolves exact base/head/merge-base identities,
  fingerprints dirty checkout state, separates PR-owned changes from base
  erosion, and blocks base-history edits and concurrent migration-path
  collisions.
- `pr regenerate` now materializes the exact base and synthetic would-be merge
  tree, overlays fingerprinted dirty state without staging it, runs generic
  declarative compilers deterministically, replays plain-SQL base history
  in PostgreSQL, requires `BC == BM`, and records the partial schema square in
  the bundle.
- The discarded generic runner/handoff experiment never entered committed
  history. In the accepted model, phase artifacts inside the bundle are the
  migration history; no ORM runner translation is planned.
- Core PR analysis no longer imports or executes `gitbase`.
  Explicit prepared-tree inputs exercise the same engine as the convenient
  Git wrapper.
- `pr status --bundle` is now a read-only freshness oracle over freshly
  prepared/compiled trees. It returns typed provenance/schema/decision/artifact
  findings with remediation and never regenerates as a side effect.
- Same-contract reruns now retain their generation, identical decision results
  retain their attempt, and replacement checks bundle identity, generation,
  phase/plan fidelity, lifecycle races, unsafe path tokens, and secret-bearing
  source descriptions.
- Hash-chained bundle ordering/replay, execution/amendment/clone receipts, and
  `ci` remain upcoming work. Low-level
  `plan --bundle` base/head flags are still caller receipts rather than
  independently verified provenance.

## REVIEW.md round-two decisions and work queue

The 2026-07-13 review is accepted as the current architectural correction.
Its findings are not merely cleanup: the Git-free boundary, freshness oracle,
answer survival, and generation semantics define whether the product remains
simple enough for developers and agents.

| Priority | Accepted work |
| --- | --- |
| P0 architecture | Make PR analysis, regeneration, freshness, history replay, and verification consume prepared trees/snapshots and receipts. Keep Git only in an optional CLI/CI adapter. |
| P0 DX | Implement a read-only freshness oracle that classifies fresh, provenance-stale, schema-stale, decision-stale, artifact-stale, and immutable-successor-required, each with a remediation command. `status` must never mutate a bundle. |
| P0 lifecycle | Same base/head/schema/options stay in the same generation. Decision reruns increment attempts only when the decision document changes; a new generation represents changed source state or a forward successor. Idempotent reruns do not churn files. |
| P0 integrity | Reject `.`/`..` identities, revalidate replacement immediately before swap, enforce bundle identity/generation compare-and-swap, recover or explicitly diagnose orphan backups, cross-check phase SQL against `plan.json`, and use length-framed digests. |
| P0 history | Replace foreign/plain migration-path replay with an onwardpg-owned per-target hash chain and prove `BC == BM` / `HC == HM` from it. |
| P1 decisions | Fingerprint questions from participating objects/operations, preserve unaffected answers across base erosion, and add an explicit answer-rebind flow that reports carried and invalidated decisions. |
| P1 receipts | Reserve and validate execution, verification, and amendment receipt slots so strict bundle reads do not deadlock the documented lifecycle. |
| P1 amendments | Represent edited SQL as typed, statement-ID-bound amendments with rationale and reverification; never make a reviewed bundle unrecoverable through undocumented edits. |
| P1 compilers | Keep Drizzle/Django/Prisma/SQLAlchemy integrations export-only; equivalent DDL or typed snapshots must produce equivalent graphs. |
| P1 diagnostics | Preserve typed merge-conflict/blocked results and add stable remediation fields to high-traffic diagnostics. |
| P2 hardening | Use a minimal compiler environment, stronger deterministic-output evidence, config overlap validation, safe URL/libpq redaction, and consolidated hashing/path/exec validation. |

The simplicity gate for every item is: does it make the default command more
predictable or remove a decision from the developer? Features that merely add
workflow policy without improving the current-to-desired plan remain optional.

## Safety and code-quality debt carried from REVIEW.md

The detailed colleague review remains in `REVIEW.md`. Fixed findings require
permanent regressions; they must not disappear from history merely because the
DX plan changed. Remaining work includes:

| Priority | Work |
| --- | --- |
| P0/P1 regression | Prove source defaults are never evaluated on production; sparse `attnum`, one-sided ignores, FK/table rename, extension drop, partition children, type-before-default, batch phase validation, and typed transactionality remain covered on real PostgreSQL. |
| P1 | Finish same-name standalone-sequence move ordering, matching the fixed standalone-index collision behavior. |
| P1 | Add the combined serial plus nullability/comment real-database regression. |
| P2 | Surface disposable-DDL database cleanup failures without replacing the primary result. |
| Cleanup | Centralize safe identifier quoting and the modeled-object registry. |
| Cleanup | Remove/populate fingerprinted dead fields and consolidate rename scaffolding only after behavior tests exist. |
| Performance | Address O(n²) rename scans, ready-queue sorting, FK dependency lookup, and implicit constraint-index scans after correctness. |
| Fixtures | Share integration executor, clone, and advisory-lock fixtures across packages. |

## Verification strategy for the DX layer

### Repository scenario tests

Create temporary real Git repositories and exercise:

- multiple branch schema edits collapsed into one bundle;
- base-branch erosion with unrelated and overlapping migrations;
- merge, rebase, squash, stacked-PR, and merge-queue base shapes;
- migration number/name collisions;
- attempts to edit base-reachable history;
- replaceable versus executed draft generations;
- dirty worktrees and unrelated uncommitted migration files;
- multiple database targets; and
- no-op, feature, repair, and contract-only bundles.

### Declarative compiler contract tests

For Drizzle first, then Django/Prisma/SQLAlchemy:

- equivalent desired schemas yield equivalent typed fingerprints;
- schema compilation runs without touching the user's migration machinery;
- no ORM journal, snapshot, or runtime migration ledger is read or written;
- the same onwardpg chain replay is used regardless of compiler; and
- adapter command failure, nondeterminism, or undeclared output is explicit.

### Planner and PostgreSQL tests

- Keep unit, property/fuzz, race, vet, formatting, and static-analysis gates.
- Require real PostgreSQL convergence for every documented supported behavior.
- Exercise PostgreSQL 14, 15, 16, 17, and 18, respecting feature floors.
- Test failure and rollback for transactional batches and rejection of wrapping
  non-transactional batches.
- Preserve pinned-reference differential tests where they are useful evidence,
  without turning them into the product definition.

### Bundle/protocol tests

- canonical serialization and stable digests;
- stale base/head/planner/options/answer/artifact detection;
- base-code/base-history and head-code/head-replay schema-square validation;
- amended-plan integrity and rejection of unreceipted SQL edits;
- lineage cycles and invalid successor relationships;
- partial/corrupt receipt directories;
- secret redaction;
- phase execution policy; and
- clone checkpoint and final residual fingerprints.

## Delivery waves

### Wave 0 — vocabulary and reproducible manifest

- Document the four comparison boundaries everywhere.
- Define `onwardpg.bundle/v1`, source receipts, phase digests, and lineage.
- Add `.onwardpg.toml` target/adapter configuration.
- Make planner build version, normalized options, and input provenance part of
  every reproducible plan.
- Formalize versioned structured error/diagnostic codes.

**Exit:** Given existing snapshots, onwardpg can write and validate a complete
bundle without Git or ORM automation.

### Wave 1 — phase-specific bundle generation

- Add atomic bundle storage and draft replacement.
- Render separate expand/migrate/manual/contract artifacts.
- Record execution mode and timing constraints without constructing an
  auto-runner path.
- Add reviewed amendments tied to stable generated statement IDs.
- Preserve the current answer loop and SQL comments in each phase.

**Exit:** One low-level plan becomes one self-contained, integrity-checked
feature bundle.

### Wave 2 — Git-free freshness and regeneration core

- Accept caller-prepared base/head directories, migration snapshots, and
  content receipts without requiring a repository.
- Implement the read-only bundle freshness oracle and remediation protocol.
- Correct generation/attempt semantics and preserve unaffected answers.
- Move existing Git behavior behind a thin convenience wrapper with the same
  core result.

**Exit:** The exact same regeneration and freshness result is produced from
explicit tree inputs and from the optional Git wrapper; status is read-only and
idempotent reruns do not churn a bundle.

### Wave 3 — onwardpg-owned history chain and replay

- Add per-target parent/entry digests and reject forks, cycles, stale parents,
  missing entries, and altered merged history.
- Replay the protected base chain and proposed PR bundle in disposable
  PostgreSQL, proving both sides of the schema square.
- Replace `migration_path` and Drizzle/plain-SQL migration replay with the
  onwardpg bundle chain as the sole history source.
- Keep Drizzle as an export-only schema compiler fixture.

**Exit:** In a fixture repo, a multi-day feature branch can absorb new main
bundles, regenerate one clean PR bundle, and prove `BC == BM` plus `HC == HM`
without an ORM migration journal or Git inside the replay core.

### Wave 4 — clone verification and checkpoint receipts

- Apply reviewed batches only to explicitly disposable clones.
- Verify transaction/non-transaction boundaries, phase checkpoints, and final
  residual diff.
- Record before/after fingerprints and failure/rollback evidence.
- Verify the complete committed onwardpg phase bundle exactly as reviewed.

**Exit:** Every ready bundle can prove that its committed history entry converges on
a representative clone.

### Wave 5 — CI and GitHub Action

- Implement deterministic `ci check` and the required policy table above.
- Publish a reusable action with PG14–18 services as appropriate.
- Produce job summaries, annotations, safe artifacts, and exact remediation
  commands.
- Test pull-request and merge-group refs.

**Exit:** CI rejects missing, stacked, stale, mutated, incomplete, unsafe, or
non-convergent migration bundles with actionable diagnostics.

### Wave 6 — executed-phase and long-running rollout lifecycle

- Add signed/attested execution receipt ingestion without production apply.
- Freeze executed generations and generate typed successors.
- Add explicit direct-versus-staged strategy choices and structural
  expand/contract scaffolds for rename, type, nullability, constraint, index,
  materialized-view, and partition transitions.
- Model orphan expansions, delayed contracts, repair bundles, and phase
  continuation across PRs/releases.
- Add PR templates/rollout reports for pre-merge expand approval.

**Exit:** A feature can safely span pre-merge expand, squash merge, observed
backfill, and later contract without rewriting executed history.

### Wave 7 — compiler ecosystem and production hardening

- Add export-only Django, Prisma, and SQLAlchemy compilers through the same
  DDL/typed-snapshot contract.
- Complete feature-map gaps according to developer demand.
- Benchmark large repos/schemas and publish performance bounds.
- Publish binaries, checksums, provenance/signing, changelog, release
  automation, and support policy.
- Complete code-quality/security review with no unresolved critical findings.

**Exit:** A clean production release can be built and its documented feature
and workflow claims match observed CI behavior.

## Developer-preview milestone

The next developer preview should be considered successful when a real Drizzle
DDL-export fixture demonstrates all of the following:

1. A feature branch changes its declarative schema repeatedly while retaining
   one replaceable onwardpg draft bundle.
2. Two unrelated onwardpg bundles land on its base branch.
3. `onwardpg pr status` explains that the existing feature bundle has a stale
   history parent.
4. `onwardpg pr regenerate` compiles the final head schema, materializes the
   exact new base, asks an explicit rename decision, and produces one logical
   onwardpg history entry without touching Drizzle migration metadata.
5. The bundle separates a concurrent index, manual/data work, and deferred
   contract from transactional expand work.
6. The base declarative schema equals replayed base history, and the committed
   bundle reaches either the head schema or a fingerprinted expand checkpoint
   with an explicit deferred residual plan.
7. Clone verification applies the committed onwardpg phase artifacts and produces the
   expected checkpoint/final fingerprints with empty residual diff where the
   selected phases promise convergence.
8. A subsequent base or schema change makes the bundle fail freshness checks.
9. CI rejects a forked history parent, an unreceipted generated-SQL edit, a
   stale answer, and an attempted rewrite of base history.
10. No command applies production SQL, modifies Git history, fetches/rebases, or
   commits/pushes without an explicit surrounding developer workflow.

## Explicit non-goals

- No automatic production migration application.
- No generated down migrations.
- No inference of application intent from name similarity alone.
- No promise that repository main, migration history, and production are
  identical; the modes exist because they can diverge.
- No hidden Git mutation or background branch rewriting.
- No ORM-specific schema semantics inside the typed graph planner.
- No claim of one-for-one Atlas or Migra compatibility.
- No acceptance of an incomplete plan merely to keep CI green.

## Definition of the ultimate DX

The experience is complete when an agent can join a multi-day feature branch,
read the developer intent, and run one deterministic workflow that says:

- what the branch changed relative to its exact current base;
- whether its migration bundle is absent, stacked, stale, replaceable, or
  immutable;
- which decisions are established by developer intent and which require a
  person/operator;
- what runs before merge, during observed data migration, and after old code is
  gone;
- how every operation is batched and what hazards it carries;
- whether the committed onwardpg history entry converges on a clone;
- whether production still differs from the release after accounting for drift;
- exactly which receipts prove those claims.

At that point “one migration per PR” stops being a fragile team convention. It
becomes a checked, resumable, forward-only contract between declarative code,
Git history, the real PostgreSQL catalog, the coding agent, and the operator.
