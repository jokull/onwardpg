# Drizzle

Install matching Drizzle 1.0 RC packages (`npm install drizzle-orm@rc` and
`npm install -D drizzle-kit@rc`) in the project and keep `drizzle.config.ts`
pointing at your TypeScript models. For models in `public`, configure onwardpg
directly; neither a package script nor a wrapper is required:

```toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_command = ["npx", "--no-install", "drizzle-kit", "export", "--sql=true"]
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
```

With pnpm, use `["pnpm", "exec", "drizzle-kit", "export", "--sql=true"]`.
Run from the project root. Use a locally installed version; do not download or
generate migration files from inside the exporter.

For `pgSchema()` models, inspect the exported SQL. If `CREATE SCHEMA` is missing,
copy [export-schema.sh](export-schema.sh) into your project and add the required
statements before the export. Do not add an unused `app` schema to public-only
models. Configure `schema_command = ["bash", "scripts/export-schema.sh"]` with
the actual copied path.

For the default Drizzle migration journal, inspect the catalog and configure
its table and schema explicitly before baseline:

```toml
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
dev_mode = "workspace"
ignore = ["schema:drizzle", "table:drizzle.__drizzle_migrations"]
```

These keys belong under `[targets.app]`. Set the development URL to the existing
local database so validation can observe the journal. This example assumes
`drizzle` contains only bookkeeping; use the actual names if customized.
Excluding the schema does not recursively exclude its objects. In this fixture,
the journal's serial sequence belongs to its ID column and is covered with the
table. Add a sequence selector only if inspection reports a separate sequence;
an unused sequence selector is rejected.

## Switch the DDL runner

Follow the [existing-app sequence](../README.md) while the original Drizzle
migration history is fully applied. After baseline, edit the models and use
`onwardpg plan` / `verify` instead of `drizzle-kit generate` / `migrate` for the
changes managed by onwardpg. Disable startup hooks that call Drizzle's migrator,
and do not run `drizzle-kit push` against the deployment database.

Keep old Drizzle migrations and their journal as historical records. Do not
manually append onwardpg bundles to `__drizzle_migrations`: they are different
formats. Bootstrap new databases from onwardpg's baseline and subsequent
phases, using your deployment runner. The Drizzle ORM continues querying the
database from the TypeScript models.

For a fresh database: start empty, have your runner replay the onwardpg baseline
and accepted bundles in history order with their batch boundaries, and keep the
Drizzle migrator disabled. Do not also run the old Drizzle baseline. You do not
need to create or populate `__drizzle_migrations` for the ORM to query this
database. Record onwardpg phase progress in your deployment runner's ledger;
there is no onwardpg-to-Drizzle journal conversion command.

During overlap, a desired `.notNull()` field can still contain nulls written by
old code. Handle that at the query/application boundary, for example with a
reviewed `COALESCE` expression; TypeScript's inferred type does not validate
database values. New writers must supply the field before contract.

Adoption fixture: drizzle-kit and drizzle-orm 1.0.0-rc.4, PostgreSQL 18.
Earlier rounds also exercised drizzle-kit 0.31.11 and drizzle-orm 0.45.3.
See [Drizzle's export documentation](https://orm.drizzle.team/docs/drizzle-kit-export).
