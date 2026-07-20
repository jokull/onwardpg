# Migration bundles

An onwardpg bundle is the durable receipt for one forward schema transition.
Per-target history is ordered by cryptographic parent links, not directory
names.

## Lifecycle

Create the ground floor once:

~~~sh
onwardpg init
~~~

Create one feature entry from the worktree with the preferred high-level
command:

~~~sh
onwardpg plan customer-profile
~~~

Verify generated history:

~~~sh
onwardpg verify
~~~

`plan` creates a worktree-local PlanID that selects the bundle on later calls;
`status` shows that anchor. The compatibility `draft` command
below documents the older explicit `history status` / `--after` interface for
automation and diagnosis, not the default authoring loop.

The explicitly selected bundle is the only mutable entry for a draft command.
Every other entry is immutable base history, and that base must end at the
exact name-and-digest reference asserted by `--after`. This is supplied by the
coding agent from its Git context; onwardpg validates the chain without
inspecting Git. `--create` is one-shot; later refreshes require the folder to
remain present. A second unpublished bundle or same-named rewritten head causes
an anchor mismatch instead of silently stacking migrations.

Applying an earlier version to a developer database does not change this
lifecycle. Keep selecting the same bundle while the feature evolves; `draft`
recomputes the complete history-to-working-schema transition. `verify` updates
evidence for the current bytes. Neither command locks or finalizes the bundle.

## Decisions

A needs-input draft returns minimal semantic choices. Each choice is itself a
valid hint, so an agent can copy it or supply the same predictable intent before
onwardpg asks:

~~~json
{"kind":"rename","object":"table","from":["app","accounts"],"to":["app","customers"]}
~~~

Pass one object with `--hint` or an array with `--hints-file`. A hint never
contains planner IDs, fingerprints, prose, or SQL. It is accepted only when it
consumes a real current decision; unused, impossible, malformed, duplicate, and
contradictory hints fail.

Internally the draft records both the semantic intent and its exact
fingerprint-bound planner evidence:

~~~text
customer-profile/
├── manifest.json
├── decisions.json
├── questions.json
├── answers.json
└── decisions/
    └── attempt-001.json
~~~

`decisions.json` is generated evidence, not an authoring format. The agent does
not edit or resubmit it. If unrelated history arrives, draft carries intent
only when its typed dependency scope is unchanged. A changed decision is
invalidated explicitly.

Choosing `manual_sql` writes a `needs_sql_edits` bundle with an `ONWARDPG TODO`
in the relevant phase. This is an incomplete handoff, not a successful plan.
The agent replaces the TODO directly in SQL; full disposable clone verification
is required before the manifest becomes `planned`.

## Generated files

A decision-bearing bundle can contain all of these generated files:

~~~text
customer-profile/
├── manifest.json
├── decisions.json            # semantic intent and bound evidence
├── questions.json
├── decisions/
│   └── attempt-001.json
├── answers.json
├── plan.json
└── phases/
    ├── expand.sql
    └── contract.sql
~~~

Only non-empty phases are written. Phase SQL preserves planner comments,
transaction boundaries, hazards, and deterministic statement IDs. Decision,
question, answer, and verification files exist only when that plan actually
uses them; a simple additive plan is correspondingly smaller.

The manifest is `onwardpg.bundle/v2`. Older three-phase developer-preview
bundles are rejected with a regeneration action. It records:

- logical bundle ID, generation, target, and purpose;
- redacted source descriptions and typed graph fingerprints;
- planner version and normalized options;
- history parent and canonical entry digest;
- decision, question, and answer receipts; and
- semantic-decision receipts without caller-supplied fingerprints; and
- plan and phase digests.

Database URLs and absolute DDL paths are never stored.

## Integrity

Generated and receipted bundles are strict receipts:

- every recorded file must exist;
- unrecorded files are rejected;
- plan, answers, questions, intent, and phases must match their digests;
- generated phase SQL must match plan.json;
- the history entry digest commits to its parent and manifest;
- forks, gaps, disconnected entries, and altered parents are rejected.

Direct phase edits make strict reads fail until `onwardpg verify` succeeds. The
verifier prepares refreshed receipts in memory, executes the exact phase files
only in disposable PostgreSQL, runs verify.sql boolean assertions, proves an
empty residual, then atomically updates manifest receipts. Failure writes
nothing. verify --check never updates receipts.

`verify --check` also requires the selected bundle to be the history head and
materializes the repository's current configured DDL. A bundle that converges
to its own old target but no longer matches the working schema is `stale`, not
verified; the finding points back to the same
`draft --bundle ... --after ...` loop.

Files without directives are transactional. Exact `-- onwardpg:batch` comments
split transactional and nontransactional chunks without requiring onwardpg to
parse application SQL. Exact `-- onwardpg:assert NAME` comments split multiple
verification queries.

Generated TODOs are bounded by stable `-- onwardpg:edit begin ID` and
`-- onwardpg:edit end ID` markers. On redraft, edits confined to such a pocket
are transplanted into refreshed generated surroundings. Other phase edits use a
conservative three-way comparison. If both developer and generator changed the
same generator-owned region, onwardpg installs the new plan receipts but
preserves the current bytes unreceipted and reports old-generated, current, and
new-generated SQL. Normal `verify` receipts the resolved file;
`verify --check` rejects it until then.

## Moving base

If an upstream bundle lands beside an older feature bundle, both may initially
name the same parent. Ordinary history loading rejects that fork. After the
agent rebases and identifies the accepted upstream tip, it runs:

~~~sh
onwardpg draft --bundle customer-profile --after "$NEW_BASE_HEAD"
~~~

By naming customer-profile, the developer tells onwardpg which one entry to
exclude. By passing the new exact `head_ref`, it asserts where the remaining
valid base chain must end. The selected draft is then regenerated from that
head to the current desired DDL. No Git API is involved.

A fork that remains after excluding the selected bundle is ambiguous and
blocks. Selecting a historical entry with descendants also blocks because the
remaining chain has a missing parent.

If upstream completely absorbs a generated feature, draft removes that
selected folder and reports `absorbed`; empty history entries are not retained.
Developer-edited SQL is never discarded by this inference.

## Verification

~~~sh
onwardpg verify --bundle customer-profile
onwardpg verify --bundle customer-profile --through expand
~~~

Verification executes only in onwardpg-created disposable databases. A partial
checkpoint is `partial_verified` when both its prefix and the complete contract
continuation converge on independent clones; its reported residual is expected.
Assertions requiring the continuation are reported as
`full_continuation_assertions`, never as assertions verified by the prefix.
Transactional batches roll back on failure.
Non-transactional batches are executed outside explicit transactions. Optional
boolean assertions in `verify.sql` must all return true.

Verification proves replay and catalog convergence. It does not prove
production timing, application compatibility, realistic data-volume behavior,
or that production applied the chain.

## Repository configuration

~~~toml
version = 1
bundle_root = "migrations/onward"

[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_SCRATCH_DATABASE_URL"
ignore = ["extension:pg_stat_statements"]
~~~

schema_command may replace schema_file. Configuration rejects unknown fields,
escaping paths, literal URLs, ambiguous schema sources, bundle/schema path
overlap, unsafe environment-variable names, and malformed ignore selectors.
Configured ignores are target policy; only selectors active in a durable H → W
comparison are copied into that bundle's receipts.

dev_database_env supplies the read-only dev catalog. scratch_database_env
supplies disposable-database authority. Omitting the latter falls back to the
former for compatibility; new repositories should configure both.
onwardpg discovers the scratch server's PostgreSQL major, records it in every
bundle receipt, and refuses to replay that history against a different major.

onwardpg does not translate bundles into Drizzle, Django, Prisma, Alembic, or
another runner. Deployment remains the developer's responsibility.
