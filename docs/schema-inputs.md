# Schema DDL inputs

onwardpg keeps one narrow integration boundary: PostgreSQL `CREATE`-statement
DDL. A project can provide that DDL in either
of two ways:

- `schema_file` points to a repository-relative SQL file; or
- `schema_command` is an argument vector whose stdout is the SQL document.

`schema_command` is trusted project code. onwardpg invokes the argument vector
directly, without a shell, from the repository root. It runs the command at
least twice in each CLI command, limits captured output, and requires
byte-identical DDL. It is not an operating-system sandbox and does not prevent
writes by the command, inside or outside the checkout. Use a read-only export
command: in a git work tree, onwardpg reports files that changed while the
command ran as a warning, and it does not stop. Both input paths accept at most 64 MiB. Command
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
twice and rejects nondeterministic output and command failure. `plan`, `draft`,
`verify`, and `init` run it once at the start and once immediately before they
commit a result; see [when the export runs](#when-the-export-runs). Commands
should write the schema only to stdout. onwardpg does not stop a command that
also writes files; see [side effects of the export](#side-effects-of-the-export). Put credentials in the configured environment variable; URL-bearing
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

## When the export runs

Each CLI command that reads the configured schema needs two proofs: the export
is deterministic, and the export did not change while the command worked.

`plan`, `draft`, `verify`, and `init` get both proofs from two runs:

1. The first run is at the start. The command plans or verifies from its output.
2. The second run is immediately before the command writes a bundle, installs
   receipts, or prints its result.

Byte-identical output from the two runs proves both properties. The catalog of
identical DDL on the same scratch server is the catalog that the command
already inspected, so the command does not load the DDL a second time.

If the second run differs from the first, the command runs the export a third
time. A third run that differs from the second is a nondeterministic export,
and the command stops. A third run that equals the second means the schema
changed while the command worked. The command then loads the new DDL into
disposable PostgreSQL and compares catalog fingerprints, as every command did
before; a different fingerprint stops the command and nothing is written.

A command that ends without a write (for example an `unsupported` or
`no_changes` result, or a failed verification) still does the second run. Its
result describes the first export, so it is reported only when the second run
has the same bytes. If the export changed, the command stops with an error and
must be run again. A nondeterministic or outdated export is therefore never
reported as a schema result.

The second run happens once. A command does not repeat a rejected run to get
another answer.

`dev`, `config check`, and `diff --target` run the export twice back to back.

## Side effects of the export

onwardpg does not check what the export command writes. A result depends on
the DDL that the command prints, and the two runs with equal bytes prove that
output. Other work can therefore run in the same checkout while a command
runs: a file that another process writes does not stop the command.

Two things still stop a command, because they change the export output between
its two runs:

- an edit to the schema sources; and
- a new build of a package that the export command imports.

In a git work tree, a command compares `git status` from before the first
export run with `git status` after the last one. If the status of a path
changed, the result has a `warnings` entry with the code `export_side_effects`
and the paths. The warning does not change the status or the exit code. It
does not say who wrote the files. If the export command wrote them, change the
command so that it writes only to stdout. The
[safety model](safety-model.md#the-schema-export-and-the-checkout) states why
the earlier check of the checkout was removed, and the limits of the warning.
