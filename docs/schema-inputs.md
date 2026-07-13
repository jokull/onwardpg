# Schema DDL inputs

onwardpg deliberately has no framework-adapter system. Its declarative input
is PostgreSQL `CREATE`-statement DDL. A project can provide that DDL in either
of two ways:

- `schema_file` points to a repository-relative SQL file; or
- `schema_command` is an argument vector whose stdout is the SQL document.

```toml
version = 1
bundle_root = "migrations/onward"

[targets.primary-postgres]
schema_command = ["pnpm", "--filter", "db", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
postgres_major = 16
```

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
twice and rejects nondeterministic output, empty output, command failure, or
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
