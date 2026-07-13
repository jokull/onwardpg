# Migration bundles

An onwardpg bundle is the durable receipt for one logical migration generation.
`pr regenerate` resolves exact Git base/head schema state and links the new
bundle to the validated per-target history head. Phase artifacts are the
onwardpg migration history; no ORM runner handoff is planned.

## Current command

Regenerate one logical feature bundle from the latest PR base:

```sh
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile
```

The command writes under `bundle_root/TARGET/BUNDLE_ID`, preserves planner JSON
on stdout, and keeps the normal exit codes. Database URLs and absolute DDL
paths are not stored in the receipt. The lower-level `plan --bundle` option can
write a standalone planning receipt, but it has no history parent and must not
be placed in the configured history root.

## Decision regeneration

If the first call exits `2`, it writes the exact result under
`decisions/attempt-001.json`. Commit or review a fingerprint-bound answer file,
then rerun:

```sh
onwardpg pr regenerate \
  --base origin/main \
  --target primary-postgres \
  --bundle customer-profile \
  --answers migrations/onward/primary-postgres/customer-profile/answers.json \
  --replace-draft
```

Draft replacement is explicit. The same schema/planner/history-parent contract
stays in the same generation even when equivalent Git provenance receipts are
refreshed: an identical decision result reuses its attempt, while a changed
decision result adds an attempt. A new generation is reserved for a changed
semantic source/planner contract or a forward successor. Prior decision receipts
within the generation are preserved as the ready plan, answers, and phase
artifacts are added. A directory
without a valid onwardpg manifest is never replaced. A bundle containing an
execution receipt is immutable and cannot be replaced as a draft.

When the base or desired graph changes, regeneration does not rewrite answer
fingerprints optimistically. It compares question-scoped object receipts,
carries exact matches, invalidates affected decisions, and defers decisions
from later planner stages until those stages are reachable. The complete
`onwardpg.answer-rebind/v1` report is included in PR analysis output.

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
- history parent and canonical entry digest;
- decision history; and
- plan, answer, intent, and phase digests.

Bundles produced by `pr regenerate` record the complete schema square: base
declarative fingerprint, replayed base-history fingerprint, synthetic head
declarative fingerprint, and replayed proposed-history fingerprint.
Regeneration refuses to bundle unless both `BC == BM` and `HC == HM`.

The phase files are the migration history. onwardpg does not copy or translate
them into Drizzle, Django, Prisma, Alembic, or another migration runner. A
developer or coding agent explicitly orchestrates phase execution, and future
execution receipts plus residual catalog diff make that rollout checkable.

## Repository configuration

`.onwardpg.toml` declares repository-relative target paths and a DDL file or
export command without storing database credentials. Validate it with:

```sh
onwardpg config check --config .onwardpg.toml
```

See [the example configuration](../.onwardpg.example.toml). Configuration is
strict: unknown fields, absolute/escaping paths, literal URLs, ambiguous schema
sources, overlapping configured paths, unsupported PostgreSQL majors, and
duplicate policy hazards are rejected. `pr status` and the top-edge/base-integrity portion of
`pr regenerate` use this configuration today. Onwardpg-owned chain replay,
durable execution receipts, and `ci check` are not implemented yet.
