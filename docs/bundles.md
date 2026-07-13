# Migration bundles

An onwardpg bundle is the durable receipt for one logical migration generation.
The first bundle implementation wraps the existing explicit-source `plan`
command. `pr regenerate` can now resolve and compile exact Git base/head schema
state. Phase artifacts are the onwardpg migration history; no ORM runner
handoff is planned.

## Current command

Supply the exact current and desired sources as usual, then opt into a bundle:

```sh
onwardpg plan \
  --from "$CLONE_DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --bundle migrations/onward/primary-postgres/customer-profile \
  --bundle-id customer-profile \
  --target primary-postgres \
  --base-ref origin/main \
  --base-commit "$BASE_SHA" \
  --head-revision "$HEAD_SHA"
```

The command preserves the normal planner JSON on stdout and the normal exit
codes. The bundle is an additional filesystem receipt. Database URLs and
absolute DDL paths are not stored in it.

For `bundle-mode=pr` (the default), `base-ref`, a full 40- or 64-character
lowercase Git object ID, and `head-revision` are required. Until the Git-aware PR layer is
implemented, these values are explicit caller assertions; the bundle does not
yet materialize or verify the Git refs itself.

## Decision regeneration

If the first call exits `2`, it writes the exact result under
`decisions/attempt-001.json`. Commit or review a fingerprint-bound answer file,
then rerun:

```sh
onwardpg plan \
  --from "$CLONE_DATABASE_URL" \
  --to "file://$PWD/schema.sql" \
  --dev-url "$DEV_POSTGRES_URL" \
  --answers migrations/onward/primary-postgres/customer-profile/answers.json \
  --bundle migrations/onward/primary-postgres/customer-profile \
  --bundle-id customer-profile \
  --target primary-postgres \
  --base-ref origin/main \
  --base-commit "$BASE_SHA" \
  --head-revision "$HEAD_SHA" \
  --replace-draft
```

Draft replacement is explicit. The same base/head/schema/planner contract stays
in the same generation: an identical decision result reuses its attempt, while
a changed decision result adds an attempt. A new generation is reserved for a
changed source/planner contract or a forward successor. Prior decision receipts
within the generation are preserved as the ready plan, answers, and phase
artifacts are added. A directory
without a valid onwardpg manifest is never replaced. A bundle containing an
execution receipt is immutable and cannot be replaced as a draft.

## Files and integrity

A ready bundle currently looks like:

```text
customer-profile/
├── manifest.json
├── intent.md                 # when --intent is supplied
├── decisions/
│   └── attempt-001.json      # preserved from earlier needs_input runs
├── answers.json
├── plan.json
└── phases/
    ├── expand.sql
    ├── migrate.sql
    ├── manual.sql
    └── contract.sql
```

Only non-empty phases are written. Each phase preserves the planner's batch
comments, including non-transactional execution boundaries. Every statement
has a deterministic content-derived ID. The manifest records exact digests for
the planner result, answers, intent, decisions, and phase files; tampering or an
unrecorded file fails bundle validation. Validation also re-renders phase SQL
from `plan.json`, so internally consistent but plan-divergent phase edits fail.
Bundle IDs reject filesystem path tokens, and source descriptions reject URL
and libpq-secret forms.

The manifest is `onwardpg.bundle/v1`. It records:

- logical bundle ID, generation, target, purpose, mode, and optional lineage;
- explicit base ref/commit and head revision;
- redacted source descriptions and graph fingerprints;
- planner build version and normalized options;
- decision history; and
- plan, answer, intent, and phase digests.

Bundles produced by `pr regenerate` also record the partial schema square:
base declarative fingerprint, replayed base-history fingerprint, synthetic
head declarative fingerprint, and `head_history_fidelity: "not_replayed"`.
Regeneration refuses to bundle an unhealthy base. Hash-chained onwardpg history
replay will prove `HC == HM`; it is not implemented yet.

The phase files are the migration history. onwardpg does not copy or translate
them into Drizzle, Django, Prisma, Alembic, or another migration runner. A
developer or coding agent explicitly orchestrates phase execution, and future
execution receipts plus residual catalog diff make that rollout checkable.

## Repository configuration

`.onwardpg.toml` declares repository-relative target paths and schema compiler
commands without storing database credentials. Validate it with:

```sh
onwardpg config check --config .onwardpg.toml
```

See [the example configuration](../.onwardpg.example.toml). Configuration is
strict: unknown fields, absolute/escaping paths, literal URLs, ambiguous schema
sources, overlapping configured paths, unsupported PostgreSQL majors, and
duplicate policy hazards are rejected. `pr status` and the top-edge/base-integrity portion of
`pr regenerate` use this configuration today. Onwardpg-owned chain replay,
durable execution receipts, and `ci check` are not implemented yet.
