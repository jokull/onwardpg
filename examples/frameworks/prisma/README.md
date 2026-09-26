# Prisma

Keep `schema.prisma` authoritative and install Prisma in your project. For
Prisma 7, keep its datasource URL in `prisma.config.ts` and export with:

```toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_command = ["npx", "--no-install", "prisma", "migrate", "diff", "--from-empty", "--to-schema", "./prisma/schema.prisma", "--script"]
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
```

The command resolves `prisma.config.ts`, so its referenced environment variables
(often `DATABASE_URL`) must be set even though this diff starts from empty.
Use your installed package manager consistently; with pnpm, replace `npx
--no-install` with `pnpm exec`. Run dependency installation and any required
package build approvals before planning, outside `schema_command`.

The optional [wrapper](export-schema.sh) rejects unexpectedly empty output.
Copy it into the app and configure its actual path if you want that check.

For an existing Prisma-managed database, inspect and exclude its migration
journal before baseline so live contract checks use that boundary too:

```toml
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
dev_mode = "workspace"
ignore = ["table:public._prisma_migrations"]
```

Put these keys under `[targets.app]` and set the dev URL to the existing local
database. Use the actual journal schema if it differs. This retains Prisma's
history while keeping it outside onwardpg's managed application schema.

## Switch the DDL runner

Follow the [existing-app adoption sequence](../README.md). Initialize onwardpg
while models still match the deployed Prisma migration history. Then edit
`schema.prisma`, run `onwardpg plan`, and verify the resulting bundle.

Run `prisma generate` separately when the client needs updating. It writes
generated files and belongs outside the read-only schema exporter. Review
queries that see overlap nulls even if the desired model declares a field
required. In our Prisma 7.10 fixture, `findMany` returned `null` for an expanded
`account_status` despite the generated model type declaring `string`. Handle
that state before using the value, use a reviewed query projection, or split
the rollout into an optional-field release followed by enforcement. Test the
actual client/version and queries you deploy.

Keep old Prisma migrations and `_prisma_migrations` as historical records.
Disable `prisma migrate deploy`, application startup migration hooks, and
`prisma db push` for changes now owned by onwardpg. Do not mark onwardpg bundles
as Prisma migrations. Bootstrap new databases through onwardpg's baseline and
subsequent phases with your deployment runner. Resuming Prisma Migrate later
requires a separate reviewed history reconciliation.

With the Prisma migrator retired, a fresh onwardpg-managed database does not
need `_prisma_migrations` rows for Prisma Client queries. Do not run both
baselines or populate a fake Prisma journal for onwardpg SQL.

Fixture versions: Prisma 7.10.0 and PostgreSQL 18. For other Prisma versions,
check `prisma migrate diff --help` for the schema flag supported by that release.

Prisma's diff covers the database features its schema represents. Inventory
handwritten SQL objects such as triggers and views before adopting; include
their reviewed DDL in the exporter or keep them outside the managed boundary
explicitly. A Prisma diff alone does not establish their equivalence. See the
[Prisma CLI reference](https://www.prisma.io/docs/orm/v7/reference/prisma-cli-reference#migrate-diff).
