---
title: Prisma
description: Generate complete PostgreSQL DDL from an empty source to the current Prisma schema.
---

Prisma’s `migrate diff` can render a script from an empty source to the current declarative schema. Prisma 7 loads its CLI datasource from `prisma.config.ts`, even though this particular comparison uses the schema as its destination. Keep a root config and make `DATABASE_URL` available to the exporter:

```ts
import "dotenv/config"
import { defineConfig, env } from "prisma/config"

export default defineConfig({
  schema: "prisma/schema.prisma",
  datasource: { url: env("DATABASE_URL") },
})
```

This follows Prisma’s current [configuration reference](https://www.prisma.io/docs/orm/reference/prisma-config-reference) and [`migrate diff` source model](https://www.prisma.io/docs/cli/migrate/diff).

```sh
pnpm exec prisma migrate diff \
  --from-empty \
  --to-schema ./prisma/schema.prisma \
  --script
```

In a blind Prisma 7.8 test, a missing datasource configuration made this command exit successfully with empty stdout. Use the checked wrapper, which rejects that unsafe ambiguity:

```toml
[targets.app]
schema_command = [
  "bash", "examples/frameworks/prisma/export-schema.sh"
]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

Copy [`export-schema.sh`](https://github.com/jokull/onwardpg/blob/main/examples/frameworks/prisma/export-schema.sh) into the application repository and adjust its package-manager invocation if needed. If pnpm blocks Prisma’s engine install, approve the prompted `@prisma/engines` and `prisma` build scripts before trusting export output.

Run `onwardpg config check` whenever Prisma or its engines change. It executes the exporter twice and requires identical bytes. Native type annotations matter: for example, Prisma `String` may map to different PostgreSQL types depending on the schema declaration.

Prisma Schema Language does not represent every PostgreSQL feature. If your application relies on custom functions, triggers, extensions, or other SQL, wrap the command in a script that appends their deterministic full definitions.

:::note[Migration ownership]
Keep Prisma’s model as the desired-state source, but do not run `prisma migrate deploy` and the onwardpg bundle for the same production change. Pick one system to own application of that DDL.
:::
