# onwardpg

Plan and verify PostgreSQL schema migrations for rolling deployments.

onwardpg reads your desired schema and generates SQL in two phases: **expand**
before the new application deploys, **contract** after the old processes drain.
It verifies the migration in disposable PostgreSQL databases. Your deployment
system runs the SQL in production.

```sh
brew install jokull/tap/onwardpg
```

**[Read the quick start →](https://onwardpg.solberg.is)**

The walkthrough covers setup, your first migration, required columns, and
deployment. PostgreSQL 15–18 are supported. This is a preview release.

## Working on a migration

With a schema source and scratch database configured:

```sh
onwardpg init                     # Record the schema before the feature change
# Edit your schema or framework models.
onwardpg plan add-booking-status  # Create a migration for the feature
onwardpg verify                   # Replay and check the generated SQL
```

Run `onwardpg plan` again as the feature changes or after a rebase. It revises
the same migration and carries forward decisions that still apply. Renames,
conversions, and data cleanup may need your input; the planner reports what
to answer or edit.

Verification checks execution and the resulting schema. Production lock times,
data cleanup rules, and the drain of old application processes still need review.

## Reference

- [Installation and releases](docs/installation.md)
- [Schema inputs](docs/schema-inputs.md)
- [CLI reference](docs/cli.md)
- [Supported features](docs/supported-features.md)
- [Contract readiness](docs/contract-readiness.md)
- [Safety model](docs/safety-model.md)
- [Testing and example receipts](docs/testing-strategy.md)

For coding agents, use the [onwardpg skill](skills/onwardpg/SKILL.md), also
published at [onwardpg.solberg.is/skill.md](https://onwardpg.solberg.is/skill.md).

[MIT](LICENSE) · [Changelog](CHANGELOG.md) · [Third-party notices](THIRD_PARTY_NOTICES.md)
