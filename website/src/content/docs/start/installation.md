---
title: Installation
description: Install onwardpg and connect authoritative DDL, development, and scratch PostgreSQL.
---

:::tip[Hand this page to a person; hand the skill to an agent]
The human documentation explains the model and tradeoffs. A coding agent should begin with the versioned operating contract at [`/skill.md`](/skill.md), then follow its links to targeted Markdown pages. [`/llms.txt`](/llms.txt) is the documentation directory, not the procedure.
:::

## Install the preview

On macOS or Linux with Homebrew:

```sh
brew install jokull/tap/onwardpg
onwardpg version
```

Go developers can install the pinned preview directly:

```sh
go install github.com/jokull/onwardpg/cmd/onwardpg@v0.1.0-preview.1
```

## Declare a target

Add `.onwardpg.toml` at the repository root:

```toml
version = 1
bundle_root = "migrations/onward"

[targets.app]
schema_file = "schema.sql"
# Or run a deterministic exporter:
# schema_command = ["pnpm", "--silent", "schema:export"]
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
dev_mode = "workspace"
```

The schema source must build the complete desired PostgreSQL state from empty—not only the latest `ALTER` statements. See the [framework guides](/guides/drizzle/) for export patterns.

## Set the two database roles

```sh
export ONWARDPG_DEV_DATABASE_URL='postgres://readonly:secret@localhost/myapp_dev'
export ONWARDPG_SCRATCH_DATABASE_URL='postgres://postgres:secret@localhost/postgres'
```

The development URL is inspected read-only. The scratch URL is a control-plane administrator for a dedicated local or CI PostgreSQL cluster; it creates and force-drops random databases and short-lived restricted login roles.

:::danger[Never use a shared application cluster as scratch]
The scratch identity needs database and role lifecycle authority. Point it only at disposable local or CI infrastructure—never production, staging, or a shared developer cluster.
:::

## Check the boundary

```sh
onwardpg config check
```

This validates configuration, exports DDL deterministically, materializes it, checks history, and ensures the development and scratch PostgreSQL major versions agree. PostgreSQL 15–18 are supported independently; evidence from one major is not treated as evidence for another.
