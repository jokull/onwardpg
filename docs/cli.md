# CLI reference

The preferred developer-preview surface is:

~~~text
onwardpg config check
onwardpg init
onwardpg plan NAME
onwardpg plan
onwardpg plan --output sql
onwardpg status
onwardpg verify
onwardpg contract check --environment production --database-env PROD_READONLY_DATABASE_URL
onwardpg drift check --database URL
onwardpg drift check --database-env PROD_READONLY_DATABASE_URL
onwardpg diff --from SOURCE --to SOURCE
~~~

All commands print JSON by default. onwardpg never applies migration SQL to a
caller-owned database. `plan` keeps one worktree-local active migration anchor;
it derives the durable bundle from accepted history to working DDL and, when a
development database is configured, separately prints a safe D → W
reconciliation with `--output sql`. init, plan, and verify use scratch_database_env when
configured and otherwise fall back to dev_database_env for compatibility.
Commands select the sole configured target automatically. In a multi-target
repository, pass `--target NAME`; an omitted ambiguous target is an error that
lists the available names.

The [generated CLI help](generated-cli-help.md)
is dumped from the current binary and is the exhaustive source for registered
flags and defaults. This guide focuses on semantics and workflow.

`draft`, `dev plan`, `history status`, and source-to-source `plan --from --to`
remain lower-level compatibility interfaces. New integrations should use
`plan`, `status`, and `diff` above. They are documented below because their
receipts and protocols remain supported during the developer preview.

## plan

~~~sh
onwardpg plan [NAME] \
  [--target NAME] [--bundle ID] [--config FILE] \
  [--purpose feature|repair|contract] \
  [--hint JSON] [--hints-file FILE] \
  [--dev-hint JSON] [--dev-hints-file FILE] \
  [--output json|text|sql]
~~~

`plan NAME` starts one evolving logical migration in the current worktree.
Later `plan` calls revise it. When a branch switch makes the active bundle
disappear, `plan OTHER-NAME` parks that identity and selects the bundle present
in the new checkout; returning and naming the earlier plan restores its PlanID.
When accepted history absorbs the bundle, the next `plan` retires its local
anchor automatically. None of this claims merge or deployment. The bundle is
always recalculated from replayed accepted history (H)
to exported working DDL (W), so an incoming accepted migration becomes part of
the ground beneath the same feature bundle rather than a second feature
migration. `--bundle` selects the same anchor in a clean CI checkout.

When development configuration is present, `plan --output sql` writes only the
direct development reconciliation (D → W) to stdout. It is deliberately not
the cumulative PR bundle and it preserves surplus dev objects in workspace
mode, preventing branch switches from suggesting destructive cleanup. JSON and
text output contain the reviewable H → W bundle report plus that D → W report.
When safe statements exist, `next_actions` includes a
`workspace_fast_forward` item with the SQL, statement count, preserved D-only
objects, and exact `onwardpg plan --output sql` argv. After a rebase its reason
is `accepted_history_changed`, making the upstream fast-forward distinguishable
from the durable feature bundle. If D → W required an ephemeral `--dev-hint`,
the action repeats every consumed hint in that argv. While several dev choices
remain, each newly emitted choice argv also carries the hints consumed by prior
iterations; follow the latest response rather than replaying an older fragment.
All argv arrays assume the same repository working directory, configured
environment variables, and exporter toolchain as the command that produced
them.
`ready` means the durable migration artifact is ready; it does not claim that
an optional caller-owned development database was mutated. Agents should still
inspect `next_actions` on successful responses and may apply a
`workspace_fast_forward` when they want D to catch up. When H already equals W
and there is no durable bundle to keep active, that action names the plan so
its replay argv remains executable.
No output is applied automatically. If the development database environment
variable is absent, durable planning still succeeds and reports development as
`not_available`; providing a dev hint makes that database required.
`plan` also accepts the planner and ignore flags listed under `dev plan`; the
generated help page is authoritative for their exact spelling and defaults.

`plan` exits `2` when either comparison needs a decision or editable SQL. A
durable question is answered with the same strict `--hint` form as `draft`.
Development questions are reported independently and are answered on the same
command with `--dev-hint` (or `--dev-hints-file`). A valid history-to-working
answer is never guessed to mean the same thing for an arbitrary long-lived
development database. Workspace mode preserves an absence-only object from D
rather than proposing a local rename or drop; scoped development hints are for
strict/disposable databases or an actual incompatible D → W transition.

## status

~~~sh
onwardpg status [--target NAME] [--config FILE]
~~~

Reads the worktree-local active-plan anchor and repository history only. It
reports the PlanID, selected bundle, and whether its parent is current, stale,
or invalid. It does not read Git or contact PostgreSQL. `verify` uses
this same anchor by default.

## config check

~~~sh
onwardpg config check [--config .onwardpg.toml]
~~~

Validates the versioned repository configuration, connects to the development
and scratch URLs, deterministically exports every target's DDL, materializes it
in disposable PostgreSQL, validates existing history, and requires all three
PostgreSQL-major receipts to agree.

When a target has an `ignore` list, `config check` also inspects the development
catalog read-only. Every selector must match the exported DDL or development
catalog, and the JSON receipt lists the exact excluded objects. This lets a
target acknowledge provider-owned state that may be absent from replay history.

A target may also list `live_ignore` selectors for provider-owned state that
exists only in live clusters. See [live_ignore](#live_ignore).

## live_ignore

A managed PostgreSQL provider owns some state in its own clusters: extensions
that its administrative role installed, its own schema, and `pg_parameter_acl`
grants to its roles. That state cannot be in the exported DDL or in a
development catalog, so it cannot be a target `ignore` selector, and it blocks
every command that reads the live catalog as `unsupported`. A target can
acknowledge exactly that state in `.onwardpg.toml`:

~~~toml
[targets.primary]
live_ignore = [
  "ownership:extension:earthdistance=pscale_admin",
  "ownership:schema:pscale_extensions=pscale_admin",
  "parameter_acl:session_replication_role",
]
~~~

Only three selector forms are accepted: `ownership:extension:NAME=ROLE`,
`ownership:schema:NAME=ROLE`, and `parameter_acl:NAME`, each exact, with no
wildcard. The list is validated for form only. A selector that matches nothing
in a given cluster is not an error, because one configuration serves clusters
that differ in what their provider installs.

`live_ignore` is read only by commands that inspect a live catalog: `drift
check`, `contract check`, and `diff` when it is given `--target`. It removes
the named blocker markers from the live snapshot and nothing else. It never
removes a typed object or adds an ignore receipt, so every difference in the
modeled graph stays visible; for example, the
extension itself is still compared when the project DDL creates it, and a
provider schema that is not in the project DDL still appears as an unexpected
object. It does not apply to the replayed history or to DDL sources, to `init`,
`plan` (including the legacy `plan --from --to` spelling, which does not accept
`--target`), `draft`, `verify`, or to `dev plan`. Acknowledged state is not hidden:
`drift check` and `contract check` list it in `observer.live_ignored`, and
`diff` lists it as `live_ignored:SELECTOR` in `workspace_compatibility` of a
planned or unsupported result; the decision envelope, which carries no
compatibility list, omits it.

A configured selector that matched nothing in this catalog is reported next to
them: `observer.live_ignore_unmatched` for `drift check` and `contract check`,
and `live_ignore_unmatched:SELECTOR` in `workspace_compatibility` for `diff`
(only when a PostgreSQL URL source was inspected; a selector must match on
neither side to be listed). It is information only. It never changes the status,
the exit code, or a fingerprint, because one configuration legitimately serves
several environments, so a selector for state only production has is unmatched
everywhere else.

Fingerprints follow one rule. The acknowledged selectors are removed before
the comparison fingerprint is computed, because unsupported markers are part of
a snapshot's fingerprint and a catalog whose only difference from the expected
graph is acknowledged provider state must compare equal: `drift check` then
reports `drift_free` with `expected_fingerprint` equal to `actual_fingerprint`,
and `contract check` matches the receipted checkpoint. The reported
`actual_fingerprint` (and `diff`'s `current_fingerprint`) therefore depends on
the `live_ignore` list. The removed selectors are listed, and
`observer.observed_fingerprint` reports the fingerprint of the live catalog
before removal, so a reader can tell that two runs saw the same catalog. It is
present in `drift check` and `contract check` results only when something was
removed. `diff` has no such field.

A selector must be written the way the report prints it, with every identifier
as PostgreSQL's `quote_ident` writes it: unquoted lower-case letters, digits,
and underscores that do not start with a digit, otherwise double-quoted with
embedded quotes doubled, for example `parameter_acl:"extwlist.extensions"` and
`ownership:schema:"Provider Schema"=pscale_admin`. Both the name and the role are
required, the single `=` outside quotes separates them, and nothing may follow.
Because an unmatched selector is not an error, a selector that cannot be a
generated one is rejected rather than silently matching nothing. The grammar
does not know PostgreSQL's keyword list, which differs by major version, so an
unquoted keyword such as `parameter_acl:user` is accepted although PostgreSQL
prints `parameter_acl:"user"`. Copy the selector from the report; if one still
matches nothing, a typo or a name that needs quotes shows up in
`live_ignore_unmatched`.

An ownership selector names the owning role, so a change of owner blocks again.
A `parameter_acl` selector names a parameter, not a grantee: a grant on a new
parameter blocks, but once `parameter_acl:session_replication_role` is listed, a
later grant of that parameter to another role is not visible to these commands.
Review the grantees of every listed parameter in the cluster itself. Ownership of
anything other than an extension or a schema, event triggers, and every other
unsupported family cannot be acknowledged this way.

## history status

~~~sh
onwardpg history status [--bundle payment-settlement]
~~~

Inspects only repository receipts. Without `--bundle`, it returns the ordered
chain, `head_bundle`, digest, and exact `head_ref`. With `--bundle`, the selected
head entry is excluded and the command reports whether its parent is current,
stale, or missing. Selecting a valid historical non-head is blocked explicitly.
Invalid forks, missing parents, and altered history exit 4. The command does
not inspect Git or connect to PostgreSQL.

## init

~~~sh
onwardpg init \
  [--bundle baseline] \
  [--concurrent-indexes] \
  [--ignore-extension-version NAME] \
  [--ignore SELECTOR]
~~~

Creates the first content-addressed history entry from empty PostgreSQL to the
configured desired DDL. It clone-verifies the bundle before installation and
refuses a non-empty target history. It creates databases only through the
configured administrative role.


## dev plan

~~~sh
onwardpg dev plan \
  [--hint '{"kind":"..."}'] \
  [--hints-file hints.json] \
  [--output text|json]
~~~

Reads the current catalog from dev_database_env and compares it with the
configured working DDL. The current database is inspected read-only. Desired
DDL is materialized in a disposable database. The development and disposable
servers must run the same PostgreSQL major; cross-major comparison is rejected.

This D → W loop is intentionally independent from durable history. The command
never runs its emitted SQL.

Planner options include:

| Flag | Meaning |
| --- | --- |
| --hint JSON | Semantic decision; repeatable |
| --hints-file FILE | Array of semantic decisions |
| --concurrent-indexes | Build eligible standalone indexes concurrently |
| --if-not-exists | Use supported IF NOT EXISTS forms |
| --if-exists | Use supported IF EXISTS forms |
| --cascade-drops | Permit supported CASCADE rendering after destructive approval |
| --schema-qualifier VALUE | Scope and render through one schema qualifier |
| --ignore-extension-version NAME | Suppress version changes for this exact extension name; repeatable, and unmatched names are allowed |
| --ignore SELECTOR | Narrow validated catalog exclusion; repeatable |
| --output text\|json | Render copyable decisions/phased SQL or the JSON protocol |

Repeatable `--ignore` flags are strict for that invocation: an unused selector
is an error. Long-lived provider exclusions belong in the target-level `ignore`
list in `.onwardpg.toml`; those may be dormant in one H → W comparison because
the object exists only in the development catalog.

## draft

~~~sh
onwardpg draft \
  --bundle payment-settlement \
  --after "$BASE_HEAD" \
  [--create] \
  [--hint '{"kind":"..."}'] \
  [--hints-file hints.json] \
  [--output text|json] \
  [--purpose feature|repair|contract]
~~~

draft is the durable H → W loop. `$BASE_HEAD` denotes the exact `head_ref`
copied from `history status`; it is not a bare bundle name.

1. the named bundle is the only excluded/mutable history entry;
2. every other bundle must form one valid content-addressed chain;
3. that chain must end at the exact accepted name-and-digest `head_ref` named
   by `--after`;
4. the chain is replayed in disposable PostgreSQL;
5. working DDL is compiled and materialized in disposable PostgreSQL, while
   step 4 runs; it is compiled again immediately before the bundle is written,
   and the two outputs must be byte-identical or have the same catalog
   fingerprint (see [when the export runs](schema-inputs.md#when-the-export-runs));
6. the typed graph planner emits semantic choices or a complete plan;
7. a complete generated plan is clone-verified before the bundle is written.

The command is deliberately Git-free. A coding agent is responsible for
pulling, rebasing, selecting the bundle ID, and copying the accepted base
`head_ref` into `--after`. `--create` is required once when the bundle is absent;
later refreshes reject a missing folder so a rebase cannot silently lose agent
SQL. A mismatch blocks accidental stacking on another unpublished or
same-named rewritten bundle. If new accepted history makes the selected parent
stale, rerun the same command with the new exact head reference. Only exact
participating-object scope matches are carried across that change.

`--hint` is repeatable and accepts one strict semantic JSON object.
`--hints-file` accepts an array of the same objects. Current kinds are identity,
rename, rename_backfill, drop, type_change, rollout, confirm, and manual_sql.
`rename_backfill` chooses manual SQL, an explicitly hazardous single
transaction, or a split plan after column identity is confirmed. `identity` is a
table-only upstream assertion: it lets an agent state that two observed table
names are the same relation before normal rename candidacy. It never guesses a
transition; an unautomatable asserted identity becomes an editable compatibility
bridge. Hints may be supplied on the first invocation. A valid later hint
hidden by an earlier dependency-ordered question is reported as deferred and
re-emitted without being receipted. A hint that cannot occur for the current
graph remains a strict error.

Without a typed `work` object, `manual_sql` writes a `needs_sql_edits` bundle
with a phase-local TODO. Replace the named pocket and run verify. An agent may
instead supply `transactional_once`, `nontransactional_once`,
`operator_batched`, or `external_attestation` work directly. Operator-batched
work is receipted under `operations/` and never enters a phase transaction. An
incomplete bundle exits 2 and cannot become immutable base history.

A valid selected bundle remains replaceable because the bundle ID is explicit,
including after local application or verification. Receipted agent edits are
carried when their phase did not also change in the new generated plan;
same-phase conflicts preserve the current SQL, install the new plan as its
unreceipted basis, and return all three SQL versions plus the `verify` next
step.

draft supports the same planner flags as dev plan.
Its decision output uses `status: "needs_decisions"` and contains
semantic choice sets plus the path and receipts it wrote, while
`needs_sql_edits` names the bundle path and files the agent must edit.
Fingerprint-bound answers are generated receipts, not an agent-facing
authoring format.

If the asserted base already produces the desired schema, a generated-only
selected bundle is removed with `status: "absorbed"`. An edited bundle is
preserved for explicit reconciliation because onwardpg cannot infer whether
its data work remains necessary.

## verify

~~~sh
onwardpg verify \
  --bundle payment-settlement \
  [--through expand|contract]
~~~

Creates disposable databases, executes validated history through the selected
phase, catalog-inspects the result, and compares it with a separate full-chain
execution. The two executions run at the same time, each in its own database.
Full verification exits zero only for an empty residual. Edited
phase files and verify.sql assertions are receipted only after this succeeds.
Partial verification exits zero with `partial_verified` only after the exact
prefix and its full continuation both succeed on independent disposable
clones. Output includes `simulated_bundle_phases`, `remaining_bundle_phases`,
and the expected residual; these never claim a real environment applied them.
Assertions that run only after the continuation are named separately as
`full_continuation_assertions`, not `verified_assertions`.

Use --check for read-only CI-style verification. It rejects unreceipted edits
instead of refreshing their receipts, requires the selected bundle to be the
history head, recompiles the configured desired DDL, and rejects a stale
desired fingerprint before clone execution.


An edited phase without directives is transactional. Exact
-- onwardpg:batch transactional and -- onwardpg:batch nontransactional comments
split additional execution batches. Exact -- onwardpg:assert NAME comments
split boolean queries in verify.sql.

## drift check

~~~sh
onwardpg drift check \
  (--database "$PRODUCTION_DATABASE_URL" | --database-env ENV) \
  [--target NAME] [--config .onwardpg.toml] \
  [--ignore SELECTOR]
~~~

`--database-env` names an environment variable that holds the live URL, so the
credential stays out of process arguments, like `contract check --database-env`.
Pass exactly one of `--database` and `--database-env`.

Inspects the explicitly supplied live catalog read-only, replays the complete
receipted history head in disposable PostgreSQL, and reports typed missing,
unexpected, and changed objects. Exit zero means drift_free; drift exits 4.

The live catalog is read first. Each check that needs only the live connection
(the [observer role](#observer-role) guard and the catalog read) runs before
the history replay, so a wrong role or URL stops the command in about a second
and creates no disposable database.

The replay is the one that `verify` uses. Each bundle runs batch by batch in
the mode that the bundle declares: a non-transactional batch, such as
`CREATE INDEX CONCURRENTLY` or `DROP INDEX CONCURRENTLY` from
`--concurrent-indexes`, is not put in a transaction, an edited phase is split
at its batch directives, and one connection runs the complete history. `plan`
and `draft` replay their base history the same way. A scratch server whose
PostgreSQL major is not the one in the history receipts is refused.

When the live catalog holds state the planner cannot model, such as an object
owned by a role other than the one inspecting, a `pg_parameter_acl` grant, or
an event trigger, the status is `unsupported`, the exit code is 3, and the
selectors are listed in `unsupported`. `diff --from URL` refuses the same
catalog with the same selectors. The `differences` are still computed and
listed, so one run shows everything; the `unsupported` list also includes any
such state in the replayed history. A result without `unsupported` entries
means there is no modeled drift and nothing the planner would refuse. State
that the target's [live_ignore](#live_ignore) list acknowledges is not listed in
`unsupported`; it appears in `observer.live_ignored`.

The audit never generates repair SQL, changes history, or participates in
ordinary draft generation.

### Observer role

`drift check` and `contract check` accept three kinds of role for the live
connection. The result names the kind in `observer.mode`.

| `observer.mode` | Role |
| --- | --- |
| `database_owner` | The owner of the database. |
| `predefined_read_role` | A login role that is a member of PostgreSQL predefined read-only roles. |
| `dedicated_read_only` | A login role with no membership, or with membership only in dedicated `NOLOGIN` roles. |

A role that is not the database owner must be `NOSUPERUSER`, `NOCREATEDB`,
`NOCREATEROLE`, `NOREPLICATION`, and `NOBYPASSRLS`. Each role that it is a
member of, directly or through another role, must be one of these:

- a predefined read-only role: `pg_read_all_data`, `pg_monitor`,
  `pg_read_all_settings`, `pg_read_all_stats`, or `pg_stat_scan_tables`;
- a dedicated role that it is a direct member of: not built in, `NOLOGIN`,
  without those five capabilities, and without memberships of its own.

No membership can have `ADMIN OPTION`. No such role can hold `CREATE` or a
grant option on an application schema. Any other role stops the command with
`observer_role_elevated` or `observer_access_policy_unsafe` (`drift check`
prefixes the code with `drift_`). The error of `drift check` and the finding of
`contract check` carry `next_actions` with the SQL for a valid role.

What the role must be able to read depends on the command:

- `drift check` reads system catalogs only. PostgreSQL lets every role read
  them, and the inspection calls no function whose result depends on a
  privilege of the caller. The role needs `CONNECT` on the database and
  nothing else: no `USAGE` on a schema and no `SELECT` on a relation. A role
  without access reads the same graph as the database owner; the test suite
  proves this on PostgreSQL 15 to 18. There is therefore no access check, and
  no object, ignored or not, can cause an access error. `dev plan` reads the
  development catalog the same way.
- `contract check` also runs data gates on application rows and proves that
  row-level security hides no row. The role needs `USAGE` on each application
  schema and `SELECT` on each table and view of the database, including
  objects that an ignore selector excludes, or the result is
  `observer_access_incomplete`. `pg_read_all_data` gives that access. It does
  not bypass row-level security.

The simplest valid role is a member of `pg_read_all_data`:

~~~sql
CREATE ROLE onwardpg_observer LOGIN PASSWORD '...'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS
  IN ROLE pg_read_all_data;
GRANT CONNECT ON DATABASE app TO onwardpg_observer;
~~~

On a managed provider, create a role that inherits `pg_read_all_data` with the
provider's role tool. On PlanetScale:

~~~sh
pscale role create DATABASE BRANCH onwardpg-observer --inherited-roles pg_read_all_data --ttl 24h
~~~

This role puts no grant on an application object, so it adds nothing to the
catalog and nothing shows as drift.

The second form gets its access from a dedicated role:

~~~sql
CREATE ROLE onwardpg_observer_grants NOLOGIN
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE onwardpg_observer LOGIN PASSWORD '...'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS
  IN ROLE onwardpg_observer_grants;
GRANT CONNECT ON DATABASE app TO onwardpg_observer;
-- contract check only; repeat for each application schema:
GRANT USAGE ON SCHEMA app TO onwardpg_observer_grants;
GRANT SELECT ON ALL TABLES IN SCHEMA app TO onwardpg_observer_grants;
~~~

For `drift check` the two `GRANT ... ON SCHEMA` statements are not necessary.
When they exist, they are catalog state. A check that runs as this observer
removes its own grants from the graph before the comparison and lists them in
`observer.projected_access`: a non-grantable `USAGE` on a schema and a
non-grantable `SELECT` on a relation, each granted by the owner, to the
observer or to its dedicated role. A check that runs as another role (the
database owner, or a `pg_read_all_data` role) does not remove them. It reports
each `SELECT` grant as an `unexpected_in_actual` table privilege and each
schema grant as `acl:schema:NAME` in `unsupported`. `live_ignore` does not
accept these selectors. To get a clean result, run the checks as the role that
holds the grants, or use the `pg_read_all_data` form and revoke the grants.
A grant to a predefined role, such as `GRANT SELECT ON app.users TO
pg_read_all_data`, is application state for every observer and is always
compared.

`--ignore` and `live_ignore` do not change what the role must be able to read.
`--ignore SELECTOR` removes one object from the comparison. `live_ignore`
acknowledges who owns provider state or a parameter grant; it removes no
object from the comparison.

It also does not normalize historical physical object names to a newer DDL
exporter's naming convention. Such a difference is drift evidence. If a later
feature must touch the object, the developer or agent owns an explicit,
reviewed physical-to-declarative transition in the migration bundle.

## contract check

~~~sh
onwardpg contract check \
  [--target NAME] [--bundle ID] \
  --environment NAME \
  --database-env ENV \
  [--evidence writer-evidence.json] \
  [--statement-timeout 30s] [--config .onwardpg.toml]
~~~

Checks whether the selected history-head bundle is ready for contract without
executing migration SQL. The environment variable supplies the database URL so
credentials do not enter bundle receipts. The command validates the hash chain,
requires the disposable-verification post-expand checkpoint, inspects the
caller database in one repeatable-read/read-only transaction, compares its
typed graph, runs receipted Boolean data gates, and validates expiring writer
evidence bound to the exact plan and environment.

The report includes the selected target, environment, bundle and PlanID
identity, generation, entry digest,
expected and observed fingerprints, check time, gate results, findings, and a
report digest. Its status is `ready`, `reconciliation_required`,
`needs_evidence`, `blocked`, `stale`, or `unsupported`. `unsupported` (exit 3)
means the production catalog holds state the planner cannot model; the report
lists the selectors in `unsupported` and the finding `unsupported_catalog_state`,
and runs no data gate. Catalog drift is still classified on the modeled graph,
so unsupported state is no longer reported as `catalog_drift`.
`--statement-timeout` limits each read-only catalog or data-gate query and
defaults to 30 seconds. `ready` does not execute or schedule contract. See
[contract readiness](contract-readiness.md) for the evidence format.

## diff (and compatibility `plan --from --to`)

~~~sh
onwardpg diff --from SOURCE --to SOURCE [options]
~~~

SOURCE is either a PostgreSQL URL or file:///absolute/path/schema.sql. Live URLs
are inspected read-only. DDL files are executed in disposable PostgreSQL
through --dev-url and then catalog-inspected; onwardpg does not partially parse
DDL.

| Flag | Meaning |
| --- | --- |
| --from SOURCE | Required current schema, unless `--from-env` is given |
| --to SOURCE | Required desired schema, unless `--to-env` is given |
| --from-env ENV, --to-env ENV | Read that side's PostgreSQL URL from an environment variable, so a live URL stays out of process arguments; mutually exclusive with `--from` or `--to` for the same side |
| --dev-url URL | Administrative URL required for DDL sources |
| --hint JSON | Semantic decision; repeatable |
| --hints-file FILE | Array of semantic decisions |
| --output text\|json | JSON by default; text renders decisions or SQL |
| --target NAME | `diff` only: apply this target's [live_ignore](#live_ignore) list to PostgreSQL URL sources; the rest of the target is not read |
| --config FILE | `diff` only: repository configuration read for `--target`; requires `--target` |

The remaining planner and ignore flags match dev plan. `diff` never writes a
bundle. `plan --from --to` is retained as a compatibility spelling. Use the
high-level `plan NAME --target TARGET` when the result belongs to configured
onwardpg history.

## Exit codes

| Exit | Meaning |
| --- | --- |
| 0 | Complete plan, no changes, absorbed generated draft, or successful full verification |
| 2 | A semantic decision, SQL edit, or explicitly checked development postcondition needs review |
| 3 | Unsupported schema state |
| 4 | History, convergence, residual, or policy blocker |
| 1 | Invocation, configuration, source, or internal error |

There is no apply command, production destination, ORM journal handoff, hidden
Git mutation, or generated down migration.
