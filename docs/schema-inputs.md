# Schema DDL inputs

onwardpg deliberately has no framework-adapter system. Its declarative input
is PostgreSQL `CREATE`-statement DDL. A project can provide that DDL in either
of two ways:

- `schema_file` points to a repository-relative SQL file; or
- `schema_command` is an argument vector whose stdout is the SQL document.

`schema_command` is trusted project code. onwardpg invokes the argument vector
directly, without a shell, from the repository root. It runs the command twice,
limits captured output, requires byte-identical DDL, and rejects direct changes
to the checkout that it can observe. It is not an operating-system sandbox and
cannot prevent writes outside the checkout or through external symlink targets;
use a read-only export command.

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

The PostgreSQL major is inferred from the configured scratch server and bound
to generated history. It is not a user-maintained configuration value.

Drizzle, Django, Prisma, SQLAlchemy, handwritten SQL, or any future code-schema
source is usable only when the project has a reliable command that emits the
complete PostgreSQL DDL. This is not uniformly built into those frameworks.
For example, a Django project may need to replay its migrations in scratch
PostgreSQL and dump the resulting schema. onwardpg does not import the
framework's model format and does not read, write, or translate its migration
journal.

## Why DDL is the boundary

onwardpg loads the SQL into a disposable PostgreSQL database and inspects the
resulting catalogs. PostgreSQL—not a partial SQL parser—therefore resolves
names, expressions, dependencies, and version-specific semantics. Equivalent
DDL sources converge on equivalent catalog graphs.

The export command runs in an isolated prepared tree. PR regeneration runs it
twice and rejects nondeterministic output, command failure, or
undeclared changes to that tree. Commands should write the schema only to
stdout. Put credentials in the configured environment variable; URL-bearing
command arguments are rejected, and receipts never record environment values.

## Intentionally out of scope

There are no framework-specific plugins, migration-runner handoffs, or generic
adapter SDK commitments in the current roadmap. The product surface remains
the CLI loop: export DDL, diff, answer explicit questions, regenerate the
bundle, and review the phase SQL.

The Go implementation contains internal artifact types used to move DDL and
catalog snapshots between packages. They are not a promise of an integration
ecosystem. New input mechanisms should not be added until the development and
PR-restacking workflows are mature and a concrete need cannot be met by DDL.
