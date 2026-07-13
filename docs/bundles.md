# Migration bundles

An onwardpg bundle is the durable receipt for one forward schema transition.
Per-target history is ordered by cryptographic parent links, not directory
names.

## Lifecycle

Create the ground floor once:

~~~sh
onwardpg init --target primary
~~~

Create or refresh one feature entry:

~~~sh
onwardpg draft --target primary --bundle customer-profile
~~~

Verify generated history:

~~~sh
onwardpg verify --target primary --bundle customer-profile
~~~

The explicitly selected bundle is the only mutable entry for a draft command.
Every other entry is immutable base history. Selecting a new bundle naturally
makes the previously verified entry part of the base chain; no Git-derived
draft status or promotion service is required.

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

A complete generated bundle currently looks like:

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
    ├── migrate.sql
    └── contract.sql
~~~

Only non-empty phases are written. Phase SQL preserves planner comments,
transaction boundaries, hazards, and deterministic statement IDs.

The manifest is onwardpg.bundle/v1. It records:

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
- forks, gaps, disconnected entries, and altered parents are rejected;
- a bundle with an execution receipt is immutable.

Direct phase edits make strict reads fail until onwardpg verify succeeds. The
verifier prepares refreshed receipts in memory, executes the exact phase files
only in disposable PostgreSQL, runs verify.sql boolean assertions, proves an
empty residual, then atomically updates manifest receipts. Failure writes
nothing. verify --check never updates receipts.

`verify --check` also requires the selected bundle to be the history head and
materializes the repository's current configured DDL. A bundle that converges
to its own old target but no longer matches the working schema is `stale`, not
verified; the finding points back to the same `draft --bundle` loop.

Files without directives are transactional. Exact -- onwardpg:batch comments
split transactional and nontransactional chunks without requiring onwardpg to
parse application SQL. Exact -- onwardpg:assert NAME comments split multiple
verification queries. Verification does not freeze the explicitly selected
draft. On redraft, onwardpg compares old generated, old edited, and new
generated phase files. It carries one-sided changes and reports a conflict
when both the agent and generator changed one phase. For a conflict, onwardpg
atomically installs the new plan receipts but preserves the current phase bytes
unreceipted. The report returns old-generated, current, and new-generated SQL.
After the agent merges the intended work, normal `verify` executes and receipts
that exact file; `verify --check` continues to reject it until then.

## Moving base

If an upstream bundle lands beside an older feature bundle, both may initially
name the same parent. Ordinary history loading rejects that fork.

~~~sh
onwardpg draft --target primary --bundle customer-profile
~~~

By naming customer-profile, the developer tells onwardpg which one entry to
exclude. The remaining entries must form one valid base chain. The selected
draft is then regenerated from the new head to the current desired DDL. No Git
API is involved.

A fork that remains after excluding the selected bundle is ambiguous and
blocks. Selecting a historical entry with descendants also blocks because the
remaining chain has a missing parent.

## Verification

~~~sh
onwardpg verify --target primary --bundle customer-profile
onwardpg verify --target primary --bundle customer-profile --through expand
~~~

Verification executes only in onwardpg-created disposable databases. A partial
checkpoint may intentionally report a residual; the complete contract
checkpoint must converge. Transactional batches roll back on failure.
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
~~~

schema_command may replace schema_file. Configuration rejects unknown fields,
escaping paths, literal URLs, ambiguous schema sources, bundle/schema path
overlap, and unsafe environment-variable names.

dev_database_env supplies the read-only dev catalog. scratch_database_env
supplies disposable-database authority. Omitting the latter falls back to the
former for compatibility; new repositories should configure both.
onwardpg discovers the scratch server's PostgreSQL major, records it in every
bundle receipt, and refuses to replay that history against a different major.

onwardpg does not translate bundles into Drizzle, Django, Prisma, Alembic, or
another runner. Deployment remains the developer's responsibility.
