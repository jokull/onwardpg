# Schema DDL inputs

onwardpg keeps one narrow integration boundary: PostgreSQL `CREATE`-statement
DDL. A project can provide that DDL in either
of two ways:

- `schema_file` points to a repository-relative SQL file; or
- `schema_command` is an argument vector whose stdout is the SQL document.

`schema_command` is trusted project code. onwardpg invokes the argument vector
directly, without a shell, from the repository root. It runs the command twice,
limits captured output, requires byte-identical DDL, and rejects direct changes
to the checkout that it can observe. It is not an operating-system sandbox and
cannot prevent writes outside the checkout or through external symlink targets;
use a read-only export command. Both input paths accept at most 64 MiB. Command
exports have a five-minute deadline per run and their output size is monitored
while running. See [exporter resource limits](exporter-limits.md) for the exact
limits and process-containment boundaries.

```toml
version = 1
bundle_root = "migrations/onward"

[targets.primary-postgres]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
ignore = ["extension:pg_stat_statements"]
```

`ignore` is a reviewed catalog boundary for provider-owned state that should
not enter the declarative history. It is not a DDL filter and it does not make
schema selectors recursive. `config check` validates each selector against the
exported DDL plus the read-only development catalog and reports what matched.

Selectors use a dot between schema and object name, for example
`table:public.django_migrations` or `table:drizzle.__drizzle_migrations`.
They are different from the colon-separated object IDs shown in plans.
Configure reviewed bookkeeping exclusions before planning so the bundle
records the same boundary for later contract checks. If such a table exists
only in the live database, configure `dev_database_env` when validating the
selectors; an exporter-only catalog cannot confirm that it exists. The bundle
stores exact observed live-only exclusions in `planner.observer_ignore_selectors`
and keeps clone-schema exclusions in `planner.ignore_selectors`. Contract checks
use that saved policy; editing current configuration cannot widen an existing
bundle's boundary. Plan again to receipt a reviewed boundary change.

### Untrusted extensions in disposable databases

PostgreSQL lets a role without superuser rights create only *trusted*
extensions. onwardpg runs project DDL as exactly such a role, so an untrusted
contrib extension (`earthdistance`, `dblink`, `pg_prewarm`, ...) fails with
`permission denied to create extension`. A target can name the extensions that
the scratch administrator may install instead:

```toml
[targets.primary-postgres]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
scratch_admin_extensions = [
  { name = "earthdistance", schema = "extensions" },
  { name = "pg_prewarm", schema = "tools", version = "1.1" },
]
```

Each entry is a reviewed grant. `name` and `schema` (the schema the project
installs the extension into; `"public"` when the DDL names none) are required.
`version` is optional. Unknown fields, bad names or versions, and duplicates are
rejected.

**The version comes from the entry, not from the DDL.** The administrator
installs exactly `version` when it is set and the server's default version when
it is not. A `VERSION` clause in project DDL is not honored for a listed
extension: onwardpg does not parse SQL, and the project's own
`CREATE EXTENSION IF NOT EXISTS` is a no-op once the extension exists (on
PostgreSQL 15 through 18 it only raises a notice; without `IF NOT EXISTS` it is
an error). The installed version is what the graph records, so a mismatch shows
up as a fingerprint difference against a server where the owner created the
extension with that clause, but onwardpg cannot detect it from the DDL itself.
State the same version in the entry. `config check` fails when the server does
not offer the version.

The DDL stays the only source of schema state:

- Nothing is inferred from the DDL text. The administrator acts only after
  PostgreSQL's own `CREATE EXTENSION` path refused the restricted role for a
  listed name, so an entry for an extension the DDL never creates changes
  nothing, and an entry for a trusted extension is never used. The refusal is
  recognized by the server's non-localized error fields (SQLSTATE `42501`,
  source file `extension.c`, routine `execute_extension_script`), never by
  message text, so an error raised by project SQL cannot trigger an install.
- The administrator creates the extension with `CREATE EXTENSION ... WITH SCHEMA
  <schema> [VERSION v] CASCADE`, after creating that schema for the restricted
  role if it is missing, then the unchanged statement list runs again.
  Dependencies installed by `CASCADE` (`earthdistance` requires `cube`) land in
  the same schema, and a dependency that itself needs superuser rights must be
  listed too, so the grant never reaches an extension the project did not name.
  List a trusted dependency only when the project places it in a different
  schema.
- The rerun requires idempotent creation: `CREATE SCHEMA IF NOT EXISTS` and
  `CREATE EXTENSION IF NOT EXISTS` in the DDL, and `--if-not-exists` when
  onwardpg generates the statements (it also covers extension creation). A bare
  statement fails with a hint naming this requirement. A baseline replays the
  DDL as written.
- The extension ends up owned by the restricted role, as if it had created the
  extension itself. A transient `NOLOGIN SUPERUSER` role installs it inside one
  transaction, `REASSIGN OWNED` hands the extension and its members to the
  restricted role, and the role is dropped before the transaction commits.
  Foreign-data wrappers and event triggers can only be owned by a superuser and
  stay with the administrator, as members of a trusted extension stay with the
  bootstrap superuser. Reads of the disposable catalog therefore need no
  exception, and a later bundle can `DROP` or `ALTER` the extension.

**Receipts and replay.** The list is receipted in
`planner.scratch_admin_extensions`, bound by the bundle's history entry digest,
and never part of a source fingerprint. The whole configured list is receipted
whether or not a server needed it, so the same project writes the same bytes on
every machine. Each accepted bundle replays under the list *it* receipted, in
`plan`, `draft`, `verify`, `drift check`, and `init`: a bundle receipted without
the field (every bundle written before this setting) replays with an empty list.
Only the mutable work, the desired DDL and the bundle being planned, runs under
the current configuration. Removing an extension from the DDL and the
configuration therefore does not stop the older bundle that created it from
replaying, and changing the list is the normal state while a new bundle is
drafted. One place still compares: `verify --check` blocks with
`scratch_admin_extensions_changed` when the bundle it checks receipted a list
that differs from the configuration it is checked under, because a read-only
check claims that the receipted evidence still holds under the configuration at
hand. Plan again to receipt a reviewed change.

`config check` rejects an entry (or version) the scratch server does not
provide, and reports an entry the restricted role could create anyway as a note
rather than an error, so one configuration stays valid on servers that trust
different extensions. It also lists `scratch_admin_installed`: what the
administrator had to install to materialize the DDL on that server.

The PostgreSQL major is inferred from the configured scratch server and bound
to generated history. It is not a user-maintained configuration value.

Drizzle, Django, Prisma, SQLAlchemy, handwritten SQL, or any future code-schema
source is usable when the project has a reliable command that emits complete
PostgreSQL DDL. The [example exporters](../examples/frameworks/) cover Drizzle,
Prisma, and Django. For Django, the exporter asks
`MigrationLoader` for final `ProjectState` and materializes it with
`SchemaEditor`; it therefore includes state-only operations without executing
historical `RunPython` or `RunSQL` work. Run `makemigrations` after model edits:
the Django example rejects changes absent from migration state. Follow the
[existing-project adoption recipes](../examples/frameworks/README.md) for
baseline timing, exporter setup, and framework migration bookkeeping.

## Why DDL is the boundary

onwardpg loads the SQL into a disposable PostgreSQL database and inspects the
resulting catalogs. PostgreSQL—not a partial SQL parser—therefore resolves
names, expressions, dependencies, and version-specific semantics. Equivalent
DDL sources converge on equivalent catalog graphs.

The export command runs from the repository root. PR regeneration runs it
twice and rejects nondeterministic output, command failure, or changes to
repository inputs. Version-control internals and dependency-installation trees
(`.git` and `node_modules` at any depth) are excluded from that mutation check;
generated project files are not. Commands should write the schema only to
stdout. Put credentials in the configured environment variable; URL-bearing
command arguments are rejected, and receipts never record environment values.

## Stable boundary, framework recipes

The product surface remains the CLI loop: export DDL, materialize and inspect
it in PostgreSQL, answer explicit questions, regenerate the bundle, and review
the phase SQL. Framework recipes may own hermetic export mechanics and document
which production migration runner must stand down; they do not bypass the same
DDL determinism, graph inspection, planning, or verification boundary.

The Go implementation contains internal artifact types used to move DDL and
catalog snapshots between packages. They are not a promise of an integration
ecosystem. New input mechanisms should not be added until the development and
PR-restacking workflows are mature and a concrete need cannot be met by DDL.
