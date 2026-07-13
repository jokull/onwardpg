# onwardpg

`onwardpg` is an agent-friendly PostgreSQL schema-diff planner. It helps a
developer or coding agent do two things well:

1. keep a development database aligned while a feature evolves; and
2. turn the final feature into one reviewable, forward-only migration bundle
   before its pull request lands.

Give onwardpg a schema that exists and PostgreSQL `CREATE`-statement DDL for
the schema you want. It inspects both with PostgreSQL, reports their semantic
difference, asks when the schemas cannot prove your intent, and emits readable
`EXPAND`, `MIGRATE`, `CONTRACT`, and `MANUAL` work.

**onwardpg never applies a plan to a caller-supplied database.** The developer
or agent owns real execution because it has the application, CI, deployment,
traffic, and timing context. onwardpg may execute reviewed SQL only in random
disposable databases that it creates and destroys for verification.

The approach comes from [Use a diff tool for SQL
migrations](https://www.solberg.is/sql-diff-migrations): derive migrations
from the real difference between declarative code and a real database, keep
manual control, detect drift, and prove difficult work on a clone.

## The idea in one minute

Feature-branch schema work has two comparison boundaries:

| Loop | Current schema | Desired schema | Durable result |
| --- | --- | --- | --- |
| **Develop** | Your development database | DDL exported from the working tree | Usually none; plan, apply deliberately, and diff again |
| **PR** | Declarative schema and onwardpg history at the latest PR base | The schema the feature would produce after merge | One restackable migration bundle per target |

During development, run the cheap loop as often as useful. Before review,
collapse the feature's exploratory history into one logical PR migration. If
`origin/main` advances, regenerate that same unexecuted bundle from the new
base instead of stacking another migration on top.

```text
code-exported DDL ───► disposable PostgreSQL ──► desired catalog graph
                                                        │
live / replayed DB ─────────────────────────────► current catalog graph
                                                        │
                                                        ▼
                                             typed dependency diff
                                                        │
                                  questions ◄──► fingerprinted answers
                                                        │
                                                        ▼
                          reviewed plan + phased SQL + hazards + receipts
```

This is “one migration per PR” at the level that matters: one logical feature
outcome. It may span several SQL files, transactions, and deployment moments.

## Install the developer preview

There are no published binaries yet. Build from a checkout with Go 1.26 or
newer:

```sh
git clone https://github.com/jokull/onwardpg.git
cd onwardpg
go install ./cmd/onwardpg
command -v onwardpg
```

If the last command fails, add your configured `GOBIN` or
`$(go env GOPATH)/bin` to `PATH` and retry.

The basic diff engine needs PostgreSQL 14–18. The Git-aware PR commands also
need Git and the dependencies used by your `schema_command`. CI currently
tests Linux against every supported PostgreSQL major; source development also
works on macOS.

## Quick start

Configure one database target. Your application or ORM only needs to export
complete PostgreSQL DDL to a file or stdout:

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.primary-postgres]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
postgres_major = 16
```

`schema_file = "schema.sql"` can be used instead of `schema_command`. The
environment variable must name a PostgreSQL server on which onwardpg can
create disposable databases. Database credentials are not stored in bundles.

Check the setup, then plan the development database diff:

```sh
onwardpg config check
onwardpg dev plan --target primary-postgres
```

The configured role needs `LOGIN`, `CREATEDB`, `CONNECT` on the named
development database, and enough catalog visibility to inspect every in-scope
object. It does not need permission to read or write application rows. For
`dev plan`, the database named by `ONWARDPG_DEV_DATABASE_URL` is the current
schema and is inspected read-only; onwardpg also uses the same server
connection to create randomly named scratch databases for DDL materialization.
PR planning and verification use only scratch databases. Use local or isolated
infrastructure—not production credentials—as this administrative connection.

| Command | Caller-owned database | Disposable PostgreSQL | Checkout |
| --- | --- | --- | --- |
| `plan` / `dev plan` | Catalog read only | Creates and drops databases when DDL must be materialized | No writes |
| `history init` | Not inspected or modified | Builds and verifies empty→declarative genesis | Writes only the first target bundle |
| `pr status --bundle` | Not used as a migration destination | Compiles and replays schemas | Read only |
| `pr regenerate` | Not used as a migration destination | Compiles, replays, and proves convergence | Writes only the requested bundle |
| `bundle verify` | Not accepted | Executes the validated bundle in random databases | No writes |
| `ci check` | Not accepted | Replays and verifies the bundle | Read only |

Scratch databases use `onwardpg_ddl_*` or `onwardpg_verify_*` names and are
dropped after use. A killed process or failed cleanup can leave one behind for
the database owner to remove.

Planner exits are designed for agents:

| Exit | Meaning | Next action |
| --- | --- | --- |
| `0` | Plan ready, including an empty residual | Review JSON or rerun with `--sql` |
| `2` | Explicit intent required | Write fingerprint-bound answers and rerun |
| `3` | Schema state is outside the modeled safety boundary | Narrowly ignore it or handle it outside onwardpg |
| `4` | Bundle/CI policy blocked, stale, residual, or verification failed | Follow the typed remediation |
| `1` | Invocation, configuration, connection, or internal error | Fix the reported error |

## The agent decision loop

onwardpg does not guess that a column was renamed, a drop is intentional, a
cast is correct, or a backfill is safe. The loop is deliberately mechanical:

1. Run the planner and read its versioned JSON.
2. If it returns `needs_input`, combine the typed question with the developer's
   stated feature intent.
3. Record the chosen value and exact question fingerprint in an answer file.
4. Rerun. Later question stages appear only after earlier ambiguities resolve.
5. Continue until the result is either a complete plan or an explicit
   unsupported result—never a partial successful plan.
6. Render SQL, review its phases and hazards, and let the developer or agent
   decide when and where to run it.
7. Diff again. The live PostgreSQL catalog is the journal of what remains.

Capture the exact result rather than copying fingerprints from terminal text:

```sh
set +e
onwardpg dev plan --target primary-postgres > dev-plan.json
status=$?
set -e

# status=2 means the questions and fingerprints are in dev-plan.json.
jq '{current_fingerprint, desired_fingerprint, questions}' dev-plan.json
```

`jq` is optional here; it only makes the JSON easier to inspect.

There is not yet a separate answer-template command. A developer or agent
creates the small answer document from that exact result: copy the two root
fingerprints, then copy each chosen question's `kind`, `key`, selected value,
and `scope_fingerprint` as `question_fingerprint`. The example below shows the
complete shape.

Answers are bound to the complete current and desired states and to the
participating objects' dependency scope. Stale, contradictory, invalid, and
unused answers are rejected. During PR restacking, onwardpg carries a decision
only when that scoped state is unchanged and explicitly reports every carried,
invalidated, unanswered, or deferred answer.

### Small example: make a rename explicit

Current:

```sql
CREATE TABLE public.users (
    id bigint PRIMARY KEY,
    name text NOT NULL
);
```

Desired:

```sql
CREATE TABLE public.users (
    id bigint PRIMARY KEY,
    full_name text NOT NULL
);
```

The first run exits `2` with a `rename_column` question. It does not decide
that dropping `name` and creating `full_name` is safe merely because their
types look alike. The agent records the developer's rename intent:

```json
{
  "protocol_version": "onwardpg.plan/v1",
  "current_fingerprint": "sha256:...",
  "desired_fingerprint": "sha256:...",
  "answers": [
    {
      "kind": "rename_column",
      "key": "column:public:users:name",
      "value": "column:public:users:full_name",
      "question_fingerprint": "sha256:..."
    }
  ]
}
```

Then it asks onwardpg for the complete reviewed SQL (abridged here):

```sh
onwardpg dev plan \
  --target primary-postgres \
  --answers migration.answers.json \
  --sql > dev-plan.sql
```

```sql
-- CONTRACT — a direct rename requires coordinated application rollout.
-- Batch: transactional
ALTER TABLE "public"."users" RENAME COLUMN "name" TO "full_name";
```

After the developer or agent deliberately runs the SQL against the development
database, another `dev plan` reports an empty residual. onwardpg itself never
performed the application.

The SQL renderer includes all non-empty phases and batch comments. A
transactional batch may be wrapped atomically by the caller; a
non-transactional batch—such as `CREATE INDEX CONCURRENTLY`—must run outside
`BEGIN`/`COMMIT`. `MANUAL` statements are developer-supplied, fingerprinted
work: clone verification executes them and checks their boolean postconditions,
while the developer or agent decides how the real deployment system should run
and observe them.

### Feature showcase: expand, backfill, validate, contract

Suppose an earlier compatible release added nullable `events.occurred_on date`
beside `events.occurred_at timestamptz`, and the current feature is ready to
finish the transition and add an index. A direct rename-and-cast would have
rewritten or locked a large table and immediately broken old application code.
Schema state also cannot tell onwardpg how the business wants timestamps
mapped to dates.

With explicit answers, the reviewed plan can instead express a rollout like
this (abridged, with the generated guard name shortened for readability):

```sql
-- EXPAND — compatible with the old application.
ALTER TABLE "public"."events"
  ADD CONSTRAINT "events_occurred_on_nn"
  CHECK ("occurred_on" IS NOT NULL) NOT VALID;

-- EXPAND — non-transactional batch.
CREATE INDEX CONCURRENTLY "events_occurred_on_idx"
  ON "public"."events" ("occurred_on");

-- MANUAL — application-owned data work supplied as a fingerprinted contract.
UPDATE "public"."events"
SET "occurred_on" = "occurred_at"::date
WHERE "occurred_on" IS NULL;
-- Verify: SELECT count(*) = 0 FROM "public"."events"
--         WHERE "occurred_on" IS NULL;

-- CONTRACT — only after compatible code is deployed and data work is observed.
ALTER TABLE "public"."events"
  VALIDATE CONSTRAINT "events_occurred_on_nn";
ALTER TABLE "public"."events"
  ALTER COLUMN "occurred_on" SET NOT NULL;
ALTER TABLE "public"."events"
  DROP CONSTRAINT "events_occurred_on_nn";
ALTER TABLE "public"."events" DROP COLUMN "occurred_at";
```

The plan records transactional boundaries, concurrent-index requirements,
lock/rewrite/data-loss hazards, the supplied backfill, and its verification
query. The developer or agent still decides how this maps onto deploys:

1. run compatible `EXPAND` work;
2. deploy code that can tolerate both shapes;
3. run and observe the application-owned backfill;
4. switch application reads to the new column; and
5. run `CONTRACT` only after the compatibility window closes.

This is why phase files are guidance rather than an apply API. Only the caller
has enough operational context to sequence the real rollout.

## From a feature branch to one PR bundle

The durable loop starts from the latest base, not from whichever development
migrations happened to exist yesterday:

```sh
git fetch origin

# Read-only: is the existing bundle fresh, stale, conflicting, or blocked?
onwardpg pr status \
  --base origin/main \
  --target primary-postgres \
  --bundle account-profile

# Replace the same unexecuted logical bundle after feature edits or base erosion.
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle account-profile \
  --replace-draft
```

Omit `--replace-draft` when creating the bundle for the first time. Replacement
is always explicit; onwardpg does not overwrite an existing directory merely
because its name matches.

If regeneration exits `2`, create the fingerprint-bound answer file from the
bundle's `questions.json`/decision result and rerun with `--answers ...
--replace-draft`. The command:

- exports the schema at the exact PR base and from the would-be merged feature;
- replays base migration history and proves it matches base declarative code;
- plans base to feature and proves the proposed history reaches the feature
  schema;
- preserves still-valid decisions while invalidating only affected ones; and
- replaces `account-profile`, rather than adding a second migration for the
  same feature.

The resulting bundle is reviewable without rerunning the planner:

```text
migrations/onward/primary-postgres/account-profile/
├── manifest.json
├── intent.md                 # when supplied
├── questions.json
├── decisions/
│   └── attempt-001.json
├── answers.json
├── plan.json
└── phases/
    ├── expand.sql
    ├── migrate.sql
    ├── manual.sql
    └── contract.sql
```

Only non-empty phase files are written. The manifest fingerprints inputs,
planner options, answers, history parent, plan, and SQL artifacts. Per-target
history is a parent/digest chain, not filename order. Forks, stale parents,
missing entries, altered history, ambiguous ordering, and unreceipted edits
are rejected.

### Initialize the history ground floor

Before the first restackable feature PR, establish what “the schema on main”
means:

```sh
onwardpg history init --target primary-postgres
onwardpg bundle verify --target primary-postgres --bundle baseline
```

`history init` compiles the current declarative schema, plans an empty
PostgreSQL database to that schema, builds a `baseline` root bundle, and proves
the complete bundle converges in disposable PostgreSQL **before** writing it.
It refuses to run if the target already has any history. It never inspects or
modifies an existing application database.

Initialization proves **empty replay equals declarative code**; it does not
claim that a particular deployed database already equals that code. Before
adopting an existing project, run a normal read-only diff against the
environment your team treats as ground truth and resolve or explicitly record
any drift.

For an existing project, the generated genesis SQL is a reproducible ground
floor for replay and new environments. It is not a migration to run against
the already-existing database. Review the bundle in a small onboarding PR and
merge it to the protected base before creating ordinary feature bundles. That
separate merge is what lets later `pr regenerate` prove that base declarative
code and replayed base history are equal.

This intentionally does not import or interpret a framework's legacy journal.
If the exported schema cannot be reconstructed from empty PostgreSQL within
onwardpg's modeled boundary, initialization returns `unsupported` and writes
nothing.

## Verification and CI

Before review, prove the committed bundle converges:

```sh
onwardpg bundle verify \
  --target primary-postgres \
  --bundle account-profile

onwardpg ci check \
  --base origin/main \
  --target primary-postgres \
  --bundle account-profile
```

`bundle verify` has no database destination flag. It creates random disposable
databases, executes the validated history and selected phases, catalog-inspects
the result, reports the residual diff, and destroys the databases. A partial
checkpoint such as `--through expand` may intentionally have a residual; the
complete bundle must converge.

`ci check` is read-only with respect to the checkout and all caller-supplied
databases. It rejects stale or forked history, edited artifacts, unanswered or
invalidated decisions, incorrectly stacked bundles, schema/history mismatch,
dirty source input, and failed clone convergence.

Neither command deploys anything. After handoff, rerunning a normal diff
against a real environment shows exactly which reviewed work that environment
still needs.

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
- release engineering: published binaries, checksums/signing, benchmarked
  performance bounds, and a complete real-PostgreSQL compatibility corpus.

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

Git-aware PR commands are checkout conveniences around prepared base and
desired trees. Git is not part of the typed schema-diff engine, and the basic
`onwardpg plan --from SOURCE --to SOURCE` command is repository-independent.

## Philosophy

[Migra](https://github.com/djrobstep/migra) proved that PostgreSQL migrations
can be derived from state instead of remembered as model-edit history.
onwardpg keeps that beautifully direct idea, then takes a stricter operational
position:

- **PostgreSQL decides what the schemas mean.** DDL is materialized and both
  sides are read back from catalogs; onwardpg does not maintain a partial SQL
  interpretation beside the server.
- **A schema proves destination, not intent.** Renames, drops, casts,
  backfills, and rollout timing become explicit questions or manual contracts,
  never guesses hidden inside plausible SQL.
- **The result is a handoff, not a deployment.** SQL is forward-only,
  reviewable, phased, and never applied to a caller-owned database by
  onwardpg.
- **Feature work has a different lifecycle from deployed history.** While a PR
  is unmerged, regenerate one logical bundle from the newest base instead of
  stacking every exploratory edit or base-erosion repair.
- **Agents need receipts.** Stable JSON, fingerprints, carried and invalidated
  answers, hazards, history hashes, clone convergence, and residual diffs make
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
- [JSON protocol and answer files](docs/protocol.md)
- [Schema DDL inputs](docs/schema-inputs.md)
- [Forward-only migration workflow](docs/migration-workflow.md)
- [Safety model](docs/safety-model.md)
- [Architecture](docs/architecture.md)
- [Stripe pg-schema-diff executable reference](docs/stripe-reference.md)
- [Supported features](docs/supported-features.md)
- [Ecosystem comparison](docs/ecosystem-comparison.md)
- [Migra compatibility](docs/compatibility.md)
- [Installation and release status](docs/installation.md)
- [Changelog](CHANGELOG.md)

The immediate path from developer preview is hardening, not a broader
application surface: improve Git-wrapper diagnostics, expand real-PostgreSQL
convergence coverage, benchmark large schemas, and publish reproducible release
artifacts. The planner remains a planner.
