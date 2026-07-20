---
title: Drizzle
description: Export a Drizzle schema as complete PostgreSQL DDL for onwardpg.
---

Drizzle Kit has the cleanest integration: `export` prints the SQL representation of your current schema to stdout.

## Configure Drizzle

```ts
// drizzle.config.ts
import { defineConfig } from "drizzle-kit";

export default defineConfig({
  dialect: "postgresql",
  schema: "./src/db/schema.ts",
});
```

Ensure every model is exported from the configured schema file or glob. Test the desired DDL:

```sh
pnpm exec drizzle-kit export --sql=true
```

## Connect onwardpg

```toml
# .onwardpg.toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_command = ["pnpm", "exec", "drizzle-kit", "export", "--sql=true"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

Then plan from the Drizzle model, not from Drizzle’s incremental migration folder:

```sh
pnpm exec drizzle-kit export --sql=true > /tmp/desired.sql
onwardpg config check
onwardpg plan add-booking-status
```

:::note[Choose one production migration path]
Use the onwardpg phase bundle as the reviewed production path for changes it owns. Do not also apply a generated Drizzle migration for the same model change.
:::

Drizzle’s export covers the schema it can represent. Add deterministic SQL to your export command for PostgreSQL objects maintained outside the Drizzle model.

