# onwardpg

onwardpg is an agent-native, forward-only PostgreSQL schema-diff planner.

It gives a developer or coding agent a clean answer to:

> Given the migration chain beneath this feature and the PostgreSQL schema the
> code now declares, what migration gets us from here to there?

onwardpg replays the accepted migration chain in disposable PostgreSQL,
materializes the desired CREATE-statement DDL, asks for intent the schemas
cannot prove, and produces reviewable expand/migrate/contract SQL.

**It never applies migrations to development, staging, or production.** The
developer or agent owns real execution because it has the application, CI,
deployment, traffic, and timing context.

The approach comes from [Use a diff tool for SQL
migrations](https://www.solberg.is/sql-diff-migrations): derive migrations from
real schema state, keep manual control, expose drift, and prove difficult work
on a clone.

## The model

There are four schemas to keep separate:

| Name | Meaning |
| --- | --- |
| **H — history head** | The schema produced by replaying the onwardpg migration chain |
| **D — developer database** | The developer's mutable local database |
| **W — working schema** | Complete CREATE-statement DDL exported from the current code |
| **P — production** | The live production catalog, inspected only by an explicit drift audit |

They form two everyday loops and one occasional audit:

~~~text
local development:      D ──diff──> W     ephemeral; the agent may apply it
next migration:         H ──diff──> W     durable; committed as one bundle
periodic drift audit:   H ──diff──> P     read-only diagnostic
~~~

The durable migration is never based on the developer database. Local state can
be useful, behind, ahead, or messy; it is not the historical source of truth.

## Install the developer preview

There are no published binaries yet. Build from a checkout with Go 1.26 or
newer:

~~~sh
git clone https://github.com/jokull/onwardpg.git
cd onwardpg
go install ./cmd/onwardpg
onwardpg version
~~~

The preview targets PostgreSQL 14–18.

## Configure one target

Your application or ORM only needs to export complete PostgreSQL DDL:

~~~toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
~~~

Use schema_file = "schema.sql" instead of schema_command when the DDL already
exists in the repository. Drizzle, Django, Prisma, SQLAlchemy, or handwritten
SQL may produce it; onwardpg does not contain framework adapters.

dev_database_env names the read-only development catalog.
scratch_database_env names a PostgreSQL administrative URL that may create and
drop disposable databases. For compatibility, omitting scratch_database_env
falls back to dev_database_env; new repositories should keep them separate.
Credentials are never written to bundles. `config check` connects to both
URLs, materializes the DDL, validates the history chain, and requires the
development, scratch, and existing history PostgreSQL majors to agree. The
major is recorded in receipts and never duplicated in config.

~~~sh
onwardpg config check
~~~

DDL is materialized in random onwardpg_ddl_* databases and verification uses
onwardpg_verify_* databases. They are dropped after use.

For a disposable local trial, one PostgreSQL server may fill both roles:

~~~sh
docker run --rm --name onwardpg-preview-pg -d \
  -e POSTGRES_PASSWORD=onwardpg \
  -p 55432:5432 postgres:16
until docker exec onwardpg-preview-pg pg_isready -U postgres; do sleep 1; done
docker exec onwardpg-preview-pg createdb -U postgres onwardpg_dev

export ONWARDPG_DEV_DATABASE_URL='postgres://postgres:onwardpg@localhost:55432/onwardpg_dev?sslmode=disable'
export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:onwardpg@localhost:55432/postgres?sslmode=disable'
onwardpg config check
~~~

The development user needs catalog read access. The scratch user must be able
to create and drop databases. Keep these privileges away from production
credentials.

## 1. Establish the replayable ground floor

Run this once for a target:

~~~sh
onwardpg init --target primary
~~~

The command plans empty PostgreSQL to the current desired DDL, creates a
content-addressed baseline bundle, clone-verifies it, and only then installs it
under the configured bundle root.

For an existing system, the baseline is a replayable declaration of the ground
beneath future work. It is not SQL to apply to the already-existing production
database.

~~~text
migrations/onward/primary/
└── baseline/
    ├── manifest.json
    ├── plan.json
    └── phases/
        └── expand.sql
~~~

History ordering comes from parent and entry digests, not filenames. Forks,
missing parents, altered artifacts, and ambiguous order are rejected.


## 2. Iterate on the development database

While the feature evolves:

~~~sh
onwardpg dev plan --target primary
~~~

This compares D → W. It prints a versioned plan and never runs it. A coding
agent may deliberately apply the SQL through the project's own tools, observe
the application, change the declarative schema, and run the command again.

After a pull or rebase, D may be missing both upstream and feature work.
dev plan simply reports the complete local reconciliation. It does not affect
the durable migration baseline.

Applying a local plan does not advance onwardpg history. If the developer
applies today's feature SQL to D and adds another column tomorrow, `dev plan`
shows only that new `D → W` residual. The durable feature migration is still
the complete `H → W` transition.

## 3. Keep one logical feature migration current

First identify the accepted history tip in the base checkout the agent intends
to build on:

~~~sh
onwardpg history status --target primary
~~~

The response returns the ordered chain, `head_bundle`, and `history_head`.
Git remains outside onwardpg: the coding agent runs this against the rebased
main checkout or otherwise uses its Git context to identify that accepted tip.
It then creates or refreshes the PR-owned bundle explicitly after it:

~~~sh
onwardpg draft \
  --target primary \
  --bundle payment-settlement \
  --after baseline
~~~

Keep using the same bundle ID and accepted predecessor while the feature
evolves. `--after` is the one fact supplied from the agent's Git context. The
command does not inspect the branch, origin/main, commits, merge bases, or pull
requests.

draft:

1. excludes only payment-settlement from the candidate base;
2. validates every other bundle as one hash chain;
3. requires that chain to end at the bundle named by `--after`;
4. replays that history on disposable PostgreSQL;
5. exports and materializes the working DDL;
6. plans H → W through the typed PostgreSQL graph;
7. stops for intent it cannot infer;
8. writes or refreshes that same selected bundle; and
9. clone-verifies generated complete plans before writing them.

This prevents accidental stacking. If another unpublished feature bundle sits
between `baseline` and `payment-settlement`, the actual base head does not
match `--after baseline`, so draft exits 4 without writing. The agent must make
the checkout contain accepted history plus only the PR-owned mutable bundle.

The selected bundle remains a mutable cumulative `H → W` draft even after its
SQL has been deliberately applied to the developer database. There is no
`lock`, `finalize`, or `promote` command. Local execution changes D, not H.

If accounts became customers, onwardpg asks rather than guessing:

~~~json
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
            "object": "table",
            "from": ["app", "accounts"],
            "to": ["app", "customers"]
          }
        },
        {
          "hint": {
            "kind": "drop",
            "object": "table",
            "name": ["app", "accounts"]
          },
          "hazards": ["data_loss"]
        }
      ]
    }
  ]
}
~~~

The coding agent knows the product intent, so it runs the same draft again with
the offered semantic hint:

~~~sh
onwardpg draft \
  --target primary \
  --bundle customer-rename \
  --after baseline \
  --hint '{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}'
~~~

When more than one desired object is structurally credible, onwardpg offers
every candidate plus deliberate drop-and-create. It never suppresses the
ambiguity or chooses the closest name.

The agent can supply the same hint on the first invocation when it already
knows the rename from the feature context. It may prepare a complete hints file
for every decision it anticipates; onwardpg walks staged questions until all
matching intent is consumed. A hint says only what schema state cannot: the
relationship between the old and new identities. onwardpg validates it against
the real graph diff, rejects malformed, contradictory, impossible, and unused
intent, then adds narrow fingerprints to its own receipt. Accepted intent
persists automatically; the agent does not maintain an answer file. Resending
the same complete hints file is safe.

`--hint` is repeatable. `--hints-file` accepts an array of the same objects.
Object names are identifier arrays, so quoted names containing dots or colons
remain unambiguous. `--output text` prints copyable hints; JSON is the default.

An agent can author common intent before the first call because the hint
vocabulary is semantic and finite:

| Kind | Only information not present in schema state |
| --- | --- |
| `rename` | Old and new identifiers are the same object |
| `drop` | Removal is deliberate, not an unrecognized rename |
| `type_change` | Use direct DDL or hand the conversion to editable SQL |
| `rollout` | Use direct or staged `NOT NULL` sequencing |
| `confirm` | Approve the exact niche lifecycle action named by the planner |
| `manual_sql` | Put the named niche operation into an editable phase TODO |

The first four are usually predictable from feature context. `confirm` and
`manual_sql` are deliberately easiest to copy from planner output when the
agent did not already know onwardpg would require them.

If onwardpg cannot derive product-specific SQL, the choice is semantic rather
than a JSON orchestration contract:

~~~sh
onwardpg draft \
  --target primary \
  --bundle event-date \
  --after baseline \
  --hint '{"kind":"type_change","name":["app","events","occurred_on"],"strategy":"manual_sql"}'
~~~

The command returns `needs_sql_edits`, writes an `ONWARDPG TODO` in the relevant
phase, and does not claim convergence. The agent replaces that TODO with
reviewed SQL, adds assertions when useful, and runs `onwardpg verify`. Only
TODO-free SQL that converges on a disposable clone receives planned receipts.

Planner exits are stable for agents:

| Exit | Meaning |
| --- | --- |
| 0 | Complete plan, no changes, or a generated bundle absorbed by the new base |
| 2 | A semantic decision or SQL edit is required |
| 3 | Unsupported schema state |
| 4 | History, convergence, or policy blocker |
| 1 | Invocation, configuration, connection, or internal error |

## 4. Review the generated bundle

A planned bundle currently looks like:

~~~text
migrations/onward/primary/customer-rename/
├── manifest.json
├── decisions.json
├── questions.json
├── answers.json
├── decisions/
│   └── attempt-001.json
├── plan.json
└── phases/
    ├── expand.sql
    ├── migrate.sql
    └── contract.sql
~~~

Only non-empty phases are written. SQL includes phase, batch, transaction, and
hazard comments. CREATE INDEX CONCURRENTLY and similar operations are separated
from transactional batches.

`decisions.json`, `questions.json`, and `answers.json` are generated receipts.
Only the compact semantic hint is authored by the agent; fingerprints and
planner keys remain internal evidence.

### Take ownership of the SQL

The generated plan remains a receipt, but the agent may now add or rewrite SQL
directly under phases/. It may create a missing migrate.sql or contract.sql and
add verify.sql with boolean assertions.

~~~sql
-- phases/migrate.sql
-- Product-aware rule supplied by the coding agent.
UPDATE public.payments
SET settled_on = (captured_at AT TIME ZONE 'Atlantic/Reykjavik')::date
WHERE status = 'captured' AND settled_on IS NULL;
~~~

~~~sql
-- verify.sql
-- onwardpg:assert captured_payments_are_settled
SELECT count(*) = 0
FROM public.payments
WHERE status = 'captured' AND settled_on IS NULL;
~~~

An edited phase with no directive is one transactional batch. Split execution
boundaries with exact comment directives when needed:

~~~sql
-- onwardpg:batch transactional
UPDATE public.payments SET settled_on = captured_at::date
WHERE settled_on IS NULL;

-- onwardpg:batch nontransactional
CREATE INDEX CONCURRENTLY payments_settled_on_idx
ON public.payments (settled_on);
~~~

Each nontransactional batch should contain one operation that PostgreSQL permits
outside a transaction. Multiple assertions use distinct
-- onwardpg:assert NAME markers. An unresolved ONWARDPG TODO blocks
verification.

## 5. Verify the generated migration

~~~sh
onwardpg verify \
  --target primary \
  --bundle customer-rename
~~~

verify creates disposable databases, replays history through the selected
bundle, executes its exact phase files in order, evaluates verify.sql,
introspects the result, and requires an empty residual diff for full
verification. On success it refreshes the edited phase and assertion receipts
in manifest.json. That records which exact bytes converged; it does not freeze
the selected feature draft. It has no destination flag.

~~~sh
onwardpg verify --target primary --bundle customer-rename --check
~~~

--check is read-only: it rejects unreceipted edits and clone-verifies a
receipted bundle without changing the checkout. It also recompiles the current
configured DDL and rejects a bundle whose recorded desired fingerprint is
stale, directing the agent back to `draft`.

A partial checkpoint is diagnostic:

~~~sh
onwardpg verify \
  --target primary \
  --bundle customer-rename \
  --through expand
~~~

The report names `simulated_bundle_phases` and
`remaining_bundle_phases`. These describe disposable-clone execution only;
they are not a journal claiming that an environment ran anything.

Verification proves structural replay and declared manual postconditions. It
does not prove production timing, realistic data volume, application
compatibility, or business correctness.

## 6. Absorb a moving base without Git integration

Suppose another migration lands while the feature is open. The developer or
coding agent pulls and rebases normally, identifies the new accepted tip from
that Git base, then reruns:

~~~sh
onwardpg draft \
  --target primary \
  --bundle customer-rename \
  --after upstream-audit
~~~

The checkout now contains the new upstream entry and customer-rename still
names its old parent. Selecting customer-rename lets onwardpg exclude that one
entry, validate the new base chain, and detect the stale parent from content
digests alone.

If the checkout does not actually contain one base chain ending at
`upstream-audit`, onwardpg refuses the restack. Otherwise it replans
`new-head → W` and replaces the same logical bundle. It carries
only answers whose dependency scope is unchanged. For developer-edited SQL it
compares the previous generated phase, the edited phase, and the newly
generated phase:

- when only the generator changed a phase, the phase is refreshed;
- when only the agent changed a phase, the exact agent SQL is preserved;
- `verify.sql` is agent-owned and carried forward; and
- when both changed the same phase, onwardpg anchors the folder to the new plan
  while preserving the current SQL bytes unreceipted. The JSON report includes
  the old generated, current edited, and new generated SQL. The coding agent
  merges the intended work in that phase and runs `onwardpg verify`; only clone
  convergence receipts the resolution.

This is intentionally phase-grained, not a semantic SQL merge. Remaining forks
or ambiguous entries are blockers.

If the new base already produces W and the selected bundle contains only
generated SQL, the result is `status: "absorbed"`: onwardpg removes the
redundant selected folder rather than committing an empty history entry. If
the bundle contains developer-owned SQL, onwardpg does not guess that the data
work is redundant; it preserves a review handoff for the agent.

This is the responsibility split:

- Git tells the coding agent which files belong in the checkout.
- The explicit bundle ID tells onwardpg which migration is mutable.
- `--after` tells it which accepted predecessor the agent intends.
- Parent digests reveal whether the ground moved.
- Disposable replay proves whether the resulting chain converges.

The same mechanism handles ordinary feature iteration. Apply the current SQL
locally, change the DDL again, and rerun `draft --bundle ... --after ...` with
the same identities. The
bundle remains the complete migration that will be reviewed for merge; it does
not stack a second feature migration merely because D moved.

## 7. Hand off; onwardpg does not deploy

The developer commits the reviewed migration folder and uses the application's
existing CI and deployment tooling to decide when expand, migrate, and contract
run. Once a bundle has actually been published or run in a shared environment,
rewriting it is no longer safe. onwardpg cannot infer that operational fact and
does not model it with a lock command: the agent stops selecting that bundle as
the feature draft and creates a successor for later work.

There is no production apply command, migration-runner integration, down
migration, embedded coding agent, framework adapter, or hidden Git mutation.

`history status --target primary --bundle customer-rename` is the Git-free
chain inspection command. It reports whether the selected bundle is current,
stale, missing, or blocked and names its actual base head. `verify --check` is
the read-only clone gate over that same bundle contract.

In PR CI, the coding agent or ordinary Git-aware CI step—not onwardpg—discovers
which bundle directory changed relative to the merge base. The policy is:

1. exactly one mutable bundle per target may be introduced by the PR;
2. its manifest parent must be the accepted main head brought into the
   checkout;
3. `onwardpg history status --target TARGET --bundle ID` must be current; and
4. `onwardpg verify --target TARGET --bundle ID --check` must succeed.

Two changed feature bundle directories are a PR-policy failure even if each is
individually valid. Git-aware `pr` and `ci` commands are intentionally not
exposed; the caller already has the correct merge-base knowledge.

The deployment system owns a separate record of which exact phase digest ran
in which environment. onwardpg's phase and assertion digests are the handoff
material, not an environment journal. In particular, `verify --through` never
claims expand or migrate has run outside its disposable clone.

## 8. Audit production drift separately

The normal feature workflow trusts that production has applied the accepted
chain. It does not query production for every PR.

Run the audit explicitly:

~~~sh
onwardpg drift check \
  --target primary \
  --database "$PRODUCTION_DATABASE_URL"
~~~

It compares replayed H with live P, read-only, and reports typed
missing_in_actual, unexpected_in_actual, and changed_in_actual objects with
both fingerprints. This catches a missing migration, unexpected index, or
manual catalog edit. It will not
generate an emergency repair, mutate history, or become a prerequisite for
ordinary draft generation.

## What onwardpg currently proves

A valid generated chain proves:

- every entry has one known parent and unmodified receipted content;
- entries replay in content-addressed order;
- the selected draft was planned from the actual replayed head;
- generated complete phases converge to the desired catalog in disposable
  PostgreSQL; and
- questions, answers, planner options, hazards, and fingerprints are retained.

It does not prove that production has applied the chain. That operational fact
belongs to deployment observability and the separate drift-audit boundary.

## What the developer preview supports

onwardpg is PostgreSQL-only and currently targets PostgreSQL 14–18. This is a
developer preview: use it where the reported safety boundary matches your
schema, and expect an explicit `unsupported` result where it does not.

| Area | Preview behavior |
| --- | --- |
| Inputs | Read-only live PostgreSQL URLs and declarative DDL from `schema_file`, `schema_command`, or `file://`, materialized in disposable PostgreSQL |
| Core relations | Schemas, tables, columns, defaults, identity/generated columns, collations, comments, indexes, sequences, enums, and common constraints |
| Higher-level objects | Views, materialized views, routines, triggers, partitioned tables, and partition children within the documented boundaries |
| Security | Typed RLS enable/force state, policies, and ordinary/partitioned-table privileges with quoted roles and explicit authorization decisions |
| Intent | Fingerprint-bound questions for credible renames, destructive drops, type conversions, staged nullability, and manual operational work |
| Output | Versioned JSON and readable forward SQL divided into phased, transactional or non-transactional batches |
| Safety | No apply command, no down migration, no guessed cast/backfill, and explicit blockers for the preview's inventoried unsupported families |

The preview plans structural index create/drop/rename/rebuild, including
continuous same-name replacement on ordinary tables, materialized views, and
independent local partitions. It preserves the old index while the new one
builds concurrently, emits deterministic temporary names and timeout guidance,
and defers cleanup to `CONTRACT`. Standalone partitioned parents, including
nested partition trees, use recursive `ON ONLY` shells and concurrently
built/attached leaves before the old hierarchy is retired. A structurally
matching prebuilt local index can also be attached to an incomplete parent
without a rebuild. A new local primary/unique constraint can first claim a
same-named matching unique index and then attach it to the constraint-owned
parent. Detach, reparent, or attach-plus-mutation rejects. Ordinary
primary/unique constraint
changes without external dependents build their replacement index concurrently
and perform a short transactional constraint swap; standalone sequence
create/drop/parameter and `OWNED BY` changes; complete identity generation,
min/max/start/increment/cache/cycle changes; and explicit confirmation before
identity removal drops its owned sequence state;
extension create/drop/version/schema changes; ordinary view
create/replace/drop/rename; materialized view create/drop/rename when safe; and
typed dependency ordering for routines, triggers, foreign-key cycles, and
partitions. Ambiguous rebuilds or data-moving transitions stop for manual work
or return unsupported.

Current notable gaps include:

- physical column reordering and middle insertion are explicitly unsupported
  because ordinary PostgreSQL `ALTER TABLE` cannot move retained attributes;
  append new declarative columns or use a separately reviewed replacement-table
  migration;
- a table rename is offered only when retained child identities are provably
  compatible; exports that regenerate table-derived primary-key, constraint,
  or index names can currently appear as destructive replacement unless those
  child names are kept stable explicitly;
- explicitly inventoried blockers for domains, composite/range types,
  standalone collations, aggregates, and foreign tables;
- catalog families now blocked rather than discarded—including explicit
  ownership, non-table and column ACL/default-privilege state, rules, text-search objects, event
  triggers, publications, extended statistics, and FDWs/servers/user
  mappings—plus replica identity, clustered/invalid indexes, table storage
  options, explicit relation tablespaces, traditional inheritance,
  subscriptions, custom access methods/operators/casts/conversions/languages,
  security labels, unmodeled comments, and PostgreSQL 18-only constraint and
  generated-column attributes—while their planning verticals remain
  incomplete;
- arbitrary view/routine-body dependency rewrites and complex dependent or
  nested materialized-view rebuild/refresh transitions;
- partition-bound, parent, or default-partition reconfiguration without an
  explicit operator-authored contract;
- broad multi-schema moves and complex cross-schema dependency transitions;
  and
- release engineering: no preview tag, signatures, or signed provenance yet;
  and
- the broader compatibility corpus remains preview hardening work.

Known unmodeled state covered by the blocker inventory is a stop, not a
footnote. Narrow ignore selectors must match and are reported exactly. Every
PostgreSQL 14–18 catalog table is now classified, but the modeled-attribute
audit is still open; do not treat a clean preview result as a claim that every
possible catalog attribute is already understood.

## Deliberate product boundaries

- No production, staging, or development apply command.
- No migration runner or ORM-journal handoff.
- No framework adapters or plugin system. A project using Drizzle, Django,
  Prisma, SQLAlchemy, or another framework may provide `schema_command` only
  when it has a deterministic full-schema export or replay-and-dump command;
  onwardpg does not assume every framework ships one.
- No generated down migrations. Recovery is another reviewed forward plan.
- No inferred rename, destructive intent, cast, or backfill.
- No hidden Git mutation, fetch, rebase, commit, or push.
- No claim of one-for-one Atlas or Migra compatibility.
- No incomplete plan reported as success.

Git is not part of the authoring model. A coding agent or developer changes the
working tree; onwardpg sees only the validated migration chain, the explicitly
selected draft, and the desired DDL. The low-level
`onwardpg plan --from SOURCE --to SOURCE` command remains repository-independent.

## Philosophy

[Migra](https://github.com/djrobstep/migra) proved that PostgreSQL migrations
can be derived from state instead of remembered as model-edit history.
onwardpg keeps that beautifully direct idea, then takes a stricter operational
position:

- **PostgreSQL decides what the schemas mean.** DDL is materialized and both
  sides are read back from catalogs; onwardpg does not maintain a partial SQL
  interpretation beside the server.
- **A schema proves destination, not intent.** Renames, drops, casts,
  backfills, and rollout timing become explicit decisions or editable SQL handoffs,
  never guesses hidden inside plausible SQL.
- **The result is a handoff, not a deployment.** SQL is forward-only,
  reviewable, phased, and never applied to a caller-owned database by
  onwardpg.
- **Feature work has a different lifecycle from accepted history.** While a
  feature is unmerged, regenerate one explicitly selected logical bundle from
  the newest replayed head instead of stacking exploratory edits or base-erosion
  repairs.
- **Agents need receipts.** Stable JSON, fingerprints, carried and invalidated
  decisions, hazards, history hashes, clone convergence, and residual diffs make
  the loop deterministic and resumable.
- **Known unmodeled state is a result.** Inventoried unsupported families block
  unless a narrow validated ignore says exactly what may be excluded. Closing
  the remaining catalog and attribute inventory is preview hardening work.

## How it compares

These projects overlap, but they do not all solve the same layer:

| Tool | Strongest fit | Important difference from onwardpg |
| --- | --- | --- |
| [Migra](https://github.com/djrobstep/migra) | Direct, historically broad PostgreSQL schema diff | Deprecated; no durable intent, rollout phases, or PR history; its Python API can apply |
| [pgmig](https://github.com/Apakottur/pgmig) | Small, read-only two-database Python diff | Simpler and already ahead on domains, basic composites, and table ownership; narrower ordering/safety/workflow model |
| [Stripe pg-schema-diff](https://github.com/stripe/pg-schema-diff) | Released Go planner with excellent native online DDL and hazards | Broader today, especially advanced partition/local/constraint index cases; onwardpg now covers typed RLS/policies/table grants but adds intent questions; Stripe uses name identity, a flat plan, an apply-capable CLI, and no PR restacking |
| [Atlas](https://atlasgo.io/) | Broad declarative diff, migration directories, linting, and multi-database tooling | Much broader product and PostgreSQL surface; includes execution/workflow capabilities rather than onwardpg's permanent plan-only boundary |
| [Alembic](https://alembic.sqlalchemy.org/) | Programmable SQLAlchemy revision framework and runner | Autogenerate covers the ORM metadata surface; rename/advanced PG work is edited or extended manually |
| [Django migrations](https://docs.djangoproject.com/en/6.0/topics/migrations/) | Mature model-state graph, rename prompts, data operations, merge and squash workflows | Compares models with historical Django state, not arbitrary live catalog state; owns apply/unapply |
| [Drizzle Kit](https://orm.drizzle.team/docs/kit-overview) | TypeScript-first schema, broad PostgreSQL declarations, snapshots, SQL generation and application | Framework-specific snapshot/journal model; interactive rather than durable fingerprinted intent; no general phased planner |

The deeper guide also places Atlas, Prisma Migrate, psqldef, pgroll, and
traditional migration runners in their adjacent categories.

Stripe is currently ahead where the PostgreSQL operation itself is difficult.
Alembic, Django, and Drizzle are far ahead as framework ecosystems and
migration runners. onwardpg's narrower bet is that the difficult part for a
developer and coding agent is preserving intent, rollout sequence, and one
coherent unexecuted PR migration while both the feature and its base evolve.

This is not a claim of one-for-one compatibility with any project. The
[deep ecosystem comparison](docs/ecosystem-comparison.md) separates automatic
diff coverage from handwritten escape hatches and records the gaps. See also
the [Migra compatibility guide](docs/compatibility.md), [supported-feature
ledger](docs/supported-features.md), machine-readable [pgmig roadmap map](parity/pgmig-roadmap.json),
the [Atlas reference study](parity/atlas-postgres.json), and the pinned
[Stripe reference boundary and corpus](docs/stripe-reference.md).

## Documentation

- [CLI reference](docs/cli.md)
- [Agent-assisted walkthrough](docs/agent-workflow.md)
- [Migration bundles and repository configuration](docs/bundles.md)
- [Semantic decision and receipt protocol](docs/protocol.md)
- [Schema DDL inputs](docs/schema-inputs.md)
- [Forward-only migration workflow](docs/migration-workflow.md)
- [Safety model](docs/safety-model.md)
- [Architecture](docs/architecture.md)
- [Stripe pg-schema-diff executable reference](docs/stripe-reference.md)
- [Supported features](docs/supported-features.md)
- [Ecosystem comparison](docs/ecosystem-comparison.md)
- [Migra compatibility](docs/compatibility.md)
- [Installation and release status](docs/installation.md)
- [Performance envelope](docs/performance.md)
- [Developer-preview security review](docs/security-review.md)
- [Changelog](CHANGELOG.md)

onwardpg is available under the [MIT License](LICENSE). The immediate path to
the first preview tag is final documentation review and release-candidate
testing from the generated archives. The planner remains a planner.

CI runs this walkthrough as a black-box CLI fixture from a new directory on
every supported PostgreSQL major; see
[`scripts/test-readme-workflow.sh`](scripts/test-readme-workflow.sh).
