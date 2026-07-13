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
          │ base integrity                                    │ artifact fidelity
          ▼                                                   ▼
base migrations replayed ─── apply PR handoff ───► head migrations replayed
```

Name them explicitly:

- **BC**: declarative code schema at the exact base commit;
- **BM**: schema produced by replaying base migration history;
- **HC**: declarative code schema at the PR head/working-tree digest; and
- **HM**: schema produced by replaying base migrations plus the PR's committed
  handoff artifact.

The ordinary invariant is `BC == BM` and `HC == HM`. The horizontal top diff
is what the feature means. The horizontal bottom transition is what the
migration runner will actually do. The vertical comparisons catch missing,
stacked, stale, or hand-edited artifacts.

If `BC != BM`, the base branch is already unhealthy. The feature PR must not
quietly absorb that mismatch and call it its own migration. CI reports a base
integrity failure or an explicitly accepted inherited exception. If `HC != HM`,
the PR migration does not implement the schema its code declares.

The local dev database is a fifth, deliberately non-authoritative state. Its
diff to `HC` helps the developer work, but it never substitutes for any corner
of the PR square.

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

- multiple new migration-runner entries for one target created by exploratory
  edits on the same PR;
- a new bundle plus unaccounted branch-local generated migrations;
- a bundle whose baseline predates migrations now present on the PR base;
- a manually edited SQL file whose digest no longer matches `plan.json`;
- a bundle that includes both a deployable expand migration and a contract file
  in a directory the application's migration runner will auto-apply together;
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

Reachability from the protected base branch also makes migrations immutable,
whether or not production has executed them yet. This protects repository
history and migration journals.

## Phase semantics in a squash-merge workflow

One logical bundle does not imply every phase should land in an automatic
migration runner at the same time.

| Phase | Typical timing | PR behavior |
| --- | --- | --- |
| `expand` | Before the application merge/deploy | May be emitted as the PR's deployable migration after approval |
| `migrate` | After compatible code exists, often observed over time | Runbook or reviewed data job; never invented from schema state |
| `manual` | At its explicitly reviewed operational point | Operator-owned SQL and verification with declared execution mode |
| `contract` | After old code and old data paths are gone | Deferred artifact or separate linked contract PR; never accidentally auto-applied with expand |

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
- execution must use or atomically register the exact migration-runner identity
  and digest that will land after squash merge.

Running SQL manually without updating the application's migration journal can
cause the merged runner to execute it again. Reusing a sequential Drizzle
migration number that another PR lands first can be worse: the journal identity
may now name different SQL. The adapter must use a collision-resistant identity
or a merge-ready serialization/lock, and CI must compare runner identity,
digest, and journal state before allowing pre-merge execution.

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
└── handoff/
    └── migration-runner.sql
```

Empty phase files are omitted. `contract.sql` is a deferred proposal unless a
later execution receipt explicitly advances it; the handoff adapter controls
which reviewed artifact enters Drizzle/Django/Prisma/Alembic migration history.

`manifest.json` is versioned and machine-readable. At minimum it records:

```json
{
  "protocol_version": "onwardpg.bundle/v1",
  "bundle_id": "customer-profile",
  "target": "primary-postgres",
  "purpose": "feature",
  "base_ref": "origin/main",
  "base_commit": "<full git sha>",
  "head_commit": "<full git sha or dirty-tree digest>",
  "baseline_source": {
    "kind": "git_migrations",
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

## Drizzle and declarative-schema adapters

The Trip `/regenerate-migrations` workflow contains the right core idea:

1. restore migrations from `origin/main`;
2. discard branch-local generated history; and
3. ask Drizzle Kit to generate one clean PR migration.

onwardpg should generalize that without destructively rewriting the working
tree as its first move.

An adapter has three separate responsibilities:

1. **Desired-schema compiler:** turn the head checkout's Drizzle/Django/Prisma/
   SQLAlchemy schema into executable genesis DDL or a complete typed snapshot.
2. **Baseline materializer:** build the base commit's schema in disposable
   PostgreSQL from its migration history or declarative schema.
3. **Artifact handoff:** translate reviewed onwardpg phase output into the
   application's migration-runner format and metadata without weakening phase
   boundaries.

For Drizzle, the first implementation should:

- run the configured Drizzle Kit export/genesis workflow in an isolated temp
  directory or git worktree;
- materialize the exact base commit/ref migration directory separately;
- never delete the user's migration directory before it has classified which
  files are base-reachable, branch-local, or already executed;
- report the precise branch-local files it proposes to replace;
- require `--replace-draft` for destructive workspace replacement;
- preserve Drizzle journal/snapshot metadata through an adapter-owned writer;
- ensure exactly one new migration-runner entry is handed off for a normal PR;
- assign/check an identity that cannot collide with a concurrently landing PR,
  or require an explicit serialized merge-ready slot before pre-merge apply;
- verify that pre-merge execution records the same identity/digest the merged
  migration runner will observe; and
- verify the handoff artifact, not merely onwardpg's internal SQL, on a clone.

The adapter interface must remain stable and generic. onwardpg owns typed
schema comparison and planning; ORM-specific adapters own compilation and
migration-runner metadata.

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
9. Verify the migration-runner handoff on a clone and save the empty residual
   result or the explicit deferred residual plan.

The agent must never choose answers merely to reach exit code `0`. A clean
`unsupported` or `needs_input` result is a successful guardrail when the
alternative is guessed intent.

## Main-branch erosion and regeneration

Every PR bundle records an exact base commit and base schema fingerprint. CI
fetches the current PR base and classifies the bundle:

- **fresh:** exact base commit/fingerprint and head desired fingerprint match;
- **git-stale:** the base ref advanced, even if the resulting schema is equal;
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

If only the Git commit changes while `BC`, `BM`, and the feature diff remain
identical, onwardpg may fast-path semantic reuse, but it still reissues the
provenance and migration-runner handoff. An unrelated migration can change
runner sequence/journal identity even when it does not change the SQL plan.

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
| Runner handoff integrity | Checked-in ORM migration differs from reviewed phase output/amendments/metadata |
| Runner identity safety | Migration ID collides, was rebound to different SQL, or pre-merge execution is missing journal identity/digest |
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
| Two PRs choose the same migration number/name | Artifact writer regenerates target-runner identity from current base; CI rejects collision |
| PR has two generated migrations from iterative Drizzle work | Classify as stacked draft artifacts and offer one-bundle regeneration |
| Existing migration is already on main | Refuse edit/delete; generate forward successor |
| Expand was applied before merge, then PR changes | Freeze executed generation; plan successor from recorded post-expand environment state |
| Expand was applied, then PR is abandoned | Report orphan expansion; require explicit forward cleanup/adoption decision |
| PR contains a destructive rename intended before merge | Explain old-code incompatibility; require staged design or explicit direct-operation approval |
| Backfill is needed | Emit migrate/manual contract and verification requirements; never synthesize data semantics |
| Contract must wait days/weeks | Keep deferred contract out of auto-runner path; create linked contract PR when ready |
| Production lags several main migrations | Release mode plans from actual clone to release desired; PR bundle is not reused blindly |
| Production has drift absent from git | Release mode surfaces it as an explicit extra change/question |
| Developer's local DB has abandoned objects | Dev mode reports them; PR mode ignores local DB as a baseline |
| Declarative schema changed but migration is intentionally deferred | Require an explicit policy waiver with reason/expiry; do not accept silent absence |
| Migration-only hotfix or drift repair | Allow declared `repair` purpose with database baseline and forward lineage |
| Multiple databases change in one PR | One target-specific bundle and verification result per database |
| Generated SQL is hand-edited | Digest failure until imported as a reviewed amendment or manual-work contract and reverified |
| SQL is applied pre-merge outside the migration runner | Block unless the exact future runner identity/digest is atomically recorded; prevent replay after merge |
| Main advances after pre-merge expand | Executed generation stays immutable; re-check live fingerprint and plan a successor instead of renumbering/replacing it |
| No schema difference remains after rebase | Remove unexecuted draft bundle; retain immutable execution/history receipts if any |

## Architecture evolution

The typed graph planner remains the sole semantic planning engine. Add a
workspace/lifecycle layer around it rather than embedding Git and ORM behavior
inside `internal/graphplan`.

```text
repository + config
        │
        ├── git/base resolver ───────────────┐
        ├── desired-schema adapter ──────────┤
        ├── baseline materializer ───────────┤
        └── migration-runner artifact writer ┤
                                             ▼
                                  typed graph snapshots
                                             │
                                      graph diff/planner
                                             │
                                   questions + plan + hazards
                                             │
                                 bundle/receipt policy engine
                                             │
                         phase artifacts + clone verification + CI report
```

Required packages/interfaces:

- `workspace.Config`: targets, adapters, paths, phase policy, hazard policy;
- `provenance.SourceReceipt`: safe git/database/adapter identity and digest;
- `gitbase.Resolver`: exact base/head resolution and immutable-history checks;
- `adapter.SchemaCompiler`: declarative source to DDL or typed snapshot;
- `adapter.BaselineMaterializer`: base ref migration history to disposable DB;
- `adapter.ArtifactWriter`: reviewed phases to ORM migration format;
- `bundle.Manifest`: versioned lineage, digests, phases, and verification;
- `bundle.Amendment`: reviewed statement additions/replacements/removals with
  rationale, phase, hazards, and verification;
- `bundle.Store`: read/validate/replace draft generations atomically;
- `policy.Checker`: local/CI cardinality, freshness, phase, and hazard rules;
- `verify.CloneRunner`: disposable-only batch execution and residual inspection;
- `report.Summary`: stable JSON plus human-readable PR/terminal output.

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
- Git ref resolution, schema-square validation, ORM compiler/materializer/
  writer execution, amendments, clone receipts, and `pr`/`ci` commands remain
  upcoming work; explicit base/head flags are receipts, not yet independently
  verified provenance.

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

### Adapter contract tests

For Drizzle first, then Django/Prisma/SQLAlchemy:

- equivalent desired schemas yield equivalent typed fingerprints;
- base migration history materializes reproducibly;
- schema compilation runs outside the user's tracked migration directory;
- artifact handoff produces valid ORM journal/snapshot metadata;
- exactly one normal PR migration entry is written;
- the handoff artifact applies on a clone and converges; and
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
- phase handoff policy; and
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
- Add a policy that prevents deferred phases from entering an auto-runner
  handoff.
- Add reviewed amendments tied to stable generated statement IDs.
- Preserve the current answer loop and SQL comments in each phase.

**Exit:** One low-level plan becomes one self-contained, integrity-checked
feature bundle.

### Wave 2 — Git base/head and Drizzle regeneration

- Implement exact base/head resolution and immutable-history classification.
- Add Drizzle desired compiler, base materializer, and artifact writer.
- Implement `pr status` and `pr regenerate --replace-draft`.
- Port the useful “one migration per PR” behavior from Trip without deleting
  working-tree migrations before classification.

**Exit:** In a fixture repo, a multi-day Drizzle branch can absorb new main
migrations and regenerate one clean PR bundle plus one runner entry.

### Wave 3 — clone verification and checkpoint receipts

- Apply reviewed batches only to explicitly disposable clones.
- Verify transaction/non-transaction boundaries, phase checkpoints, and final
  residual diff.
- Record before/after fingerprints and failure/rollback evidence.
- Verify the ORM handoff artifact exactly as committed.

**Exit:** Every ready bundle can prove that its committed handoff converges on
a representative clone.

### Wave 4 — CI and GitHub Action

- Implement deterministic `ci check` and the required policy table above.
- Publish a reusable action with PG14–18 services as appropriate.
- Produce job summaries, annotations, safe artifacts, and exact remediation
  commands.
- Test pull-request and merge-group refs.

**Exit:** CI rejects missing, stacked, stale, mutated, incomplete, unsafe, or
non-convergent migration bundles with actionable diagnostics.

### Wave 5 — executed-phase and long-running rollout lifecycle

- Add signed/attested execution receipt ingestion without production apply.
- Freeze executed generations and generate typed successors.
- Add explicit direct-versus-staged strategy choices and structural
  expand/contract scaffolds for rename, type, nullability, constraint, index,
  materialized-view, and partition transitions.
- Model orphan expansions, delayed contracts, repair bundles, and phase
  continuation across PRs/releases.
- Add PR templates/handoff reports for pre-merge expand approval.

**Exit:** A feature can safely span pre-merge expand, squash merge, observed
backfill, and later contract without rewriting executed history.

### Wave 6 — adapter ecosystem and production hardening

- Add Django, Prisma, and SQLAlchemy adapters through the same contracts.
- Complete feature-map gaps according to developer demand.
- Benchmark large repos/schemas and publish performance bounds.
- Publish binaries, checksums, provenance/signing, changelog, release
  automation, and support policy.
- Complete code-quality/security review with no unresolved critical findings.

**Exit:** A clean production release can be built and its documented feature
and workflow claims match observed CI behavior.

## Developer-preview milestone

The next developer preview should be considered successful when a real Drizzle
fixture demonstrates all of the following:

1. A feature branch changes its schema repeatedly and initially creates
   multiple branch-local Drizzle migrations.
2. Two unrelated migrations land on its base branch.
3. `onwardpg pr status` explains that the existing feature migration is stacked
   and stale.
4. `onwardpg pr regenerate` compiles the final head schema, materializes the
   exact new base, asks an explicit rename decision, and produces one logical
   bundle plus one Drizzle runner entry.
5. The bundle separates a concurrent index, manual/data work, and deferred
   contract from transactional expand work.
6. The base declarative schema equals replayed base history, and the committed
   handoff reaches either the head schema or a fingerprinted expand checkpoint
   with an explicit deferred residual plan.
7. Clone verification applies the committed runner artifact and produces the
   expected checkpoint/final fingerprints with empty residual diff where the
   selected phases promise convergence.
8. A subsequent base or schema change makes the bundle fail freshness checks.
9. CI rejects manually stacked migrations, an unreceipted generated-SQL edit, a
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
- whether the committed migration-runner artifact converges on a clone;
- whether production still differs from the release after accounting for drift;
- exactly which receipts prove those claims.

At that point “one migration per PR” stops being a fragile team convention. It
becomes a checked, resumable, forward-only contract between declarative code,
Git history, the real PostgreSQL catalog, the coding agent, and the operator.
