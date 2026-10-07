# JSON interface

`plan` is the agent-facing authoring interface. It carries one durable H → W
report and one separately scoped D → W development report. `draft` and `diff`
retain lower-level interfaces for explicit history and source-to-source callers.
There is no protocol-version field: onwardpg has no compatibility burden yet.
Consumers branch on `status`, `code`, and the fields relevant to that status.

## Minimal draft decisions

`draft` deliberately makes output and input asymmetric. onwardpg
emits the context needed to choose safely; the agent returns only semantic
intent that cannot be inferred from schema state.

```json
{
  "status": "needs_decisions",
  "next_action": "rerun_without_create_with_hints",
  "path": "migrations/onward/primary/profile-name",
  "written_receipts": ["manifest.json", "questions.json", "decisions/attempt-001.json"],
  "decisions": [
    {
      "choices": [
        {
          "hint": {
            "kind": "rename",
            "object": "column",
            "from": ["public", "users", "name"],
            "to": ["public", "users", "display_name"]
          }
        },
        {
          "hint": {
            "kind": "drop",
            "object": "column",
            "name": ["public", "users", "name"]
          },
          "hazards": ["data_loss"]
        }
      ]
    }
  ]
}
```

The response omits the target, bundle, whole-schema fingerprints, internal
question key, prose labels, and correlation IDs. The caller already knows the
invocation, and onwardpg already knows the fingerprints. `path` and
`written_receipts` make the intentional checkout mutation explicit.
`next_action` is a stable enum rather than a shell command. On first invocation
it tells the caller to omit the one-shot `--create`; later decision attempts use
`rerun_same_command_with_hints`. Every remaining field is needed to parse the
response or choose between different effects.

Each `hint` object is the exact accepted input shape. Pass one with repeatable
`--hint '<json-object>'`, or pass an array with `--hints-file`. Hints use strict
decoding: unknown fields, wrong identifier arity, empty identifiers, invalid
strategies, duplicates, contradictions, impossible mappings, and unused intent
fail. Identifier arrays preserve quoted names without inventing a delimiter.

Current semantic kinds are:

- `identity`: an explicit table `from`/`to` assertion applied before rename
  candidacy. It is for a known relation identity that exporter naming or other
  bounded catalog differences would otherwise hide; it never permits a guessed
  rename. If onwardpg cannot derive the structural transition, it emits the
  editable `rename_compatibility_bridge` handoff instead of a drop/create;
- `rename`: object kind plus exact `from` and `to` identifiers;
- `drop`: object kind plus exact current identifier;
- `type_change`: column identifier and the `manual_sql` handoff strategy;
- `rollout`: column identifier and offered strategy (`not_null` is implied);
- `rename_backfill`: old column identifier and one offered strategy
  (`manual_sql`, `single_transaction`, or `split_plan`);
- `confirm`: exact object and authorization/rebuild/destructive action; and
- `manual_sql`: exact object and action handed to editable phase SQL.

Hints may be supplied before a question is emitted. An agent that already
understands the feature may construct the complete hints file in advance. The
planner iterates through its dependency-ordered question stages and consumes
any matching intent. One drop hint can therefore reject a rename and later
confirm removal. A hint that never consumes a real decision is rejected rather
than ignored.

JSON plan results can include `analysis`: stable records for credible rename
near-misses rejected before a normal question existed. Each record names the
old and desired objects plus a reason such as `child_identity_mismatch:...`.
An agent can inspect that evidence and, when product context proves the
relation identity, provide a table `identity` hint. The assertion remains
fingerprint-bound through the resulting question and can only lead to an
automatic plan or an editable compatibility bridge.

Consumed hints are stored automatically in generated `decisions.json` beside
their internal scoped answer evidence. The agent never copies fingerprints and
does not need to resubmit accepted hints. Resending the same complete hints
file is idempotent; a newly irrelevant entry still fails as unused intent.
Unrelated schema erosion preserves a scoped decision; changed meaning
invalidates it.

A minimal `manual_sql` hint contains no SQL. It returns this nonterminal
envelope after writing a phase-local TODO when `work` is omitted. Agents may
instead attach a complete typed `work` object to the `manual_sql` hint so no
prose-to-SQL handoff is required:

<!-- onwardpg-receipt: draft-needs-sql-edits -->
```json
{
  "status": "needs_sql_edits",
  "next_action": "edit_files_then_verify",
  "path": "migrations/onward/app/event-date",
  "edit": [
    "phases/contract.sql"
  ]
}
```

The agent edits only the generated pockets bounded by stable
`-- onwardpg:edit begin/end` markers unless it deliberately takes ownership of
more of the phase. `ONWARDPG TODO` prevents receipt refresh,
and the bundle cannot participate as immutable base history. Only successful
full clone verification transitions the exact edited files to `planned`.

`--output text` renders the same choices as copyable `--hint` arguments. It
does not change planning semantics or prompt on a TTY.

## Source-to-source compatibility interface

`onwardpg diff` (and the compatibility spelling `onwardpg plan --from --to`)
writes JSON to standard output by default. Planned, decision, and unsupported
results use the same status-oriented shape as the receipted planner document
embedded in bundles.

`--output text` is deliberately not JSON: it is a review-only rendering available only
when a plan is ready. It emits SQL comments for phase boundaries and
transactional batches so a committed migration remains legible to both humans
and coding agents. Automation should use JSON and render reviewed SQL from the
`statements` or `batches` fields.

## Result document

Every normal planner result has this shape:

```json
{
  "current_fingerprint": "sha256:...",
  "desired_fingerprint": "sha256:...",
  "status": "planned | needs_input | needs_sql_edits | unsupported",
  "statements": [],
  "batches": [],
  "contract_gates": [],
  "reconciliations": [],
  "questions": [],
  "ignored": [],
  "unsupported": []
}
```

Empty arrays use `omitempty` and may be absent. Consumers must treat an omitted
array as empty.

`current_fingerprint` and `desired_fingerprint` identify the complete typed
catalog graphs used for the decision. A fingerprint change means a new planning
input, even if an object name appears unchanged.

`planned` contains forward SQL. Each statement has `sql`, `safety`, `phase`,
optional `hazards`, gate references, a transition identity, a typed contract
disposition, and a deterministic content-derived `id`. Bundle amendments
will bind to the ID rather than a fragile statement-array index. `phase` is one
of:

- `expand`: an acceptance-compatible schema while pre-deployment code is live,
  before the one application deployment anchored to the bundle. It may
  explicitly relax enforcement so legacy and new SQL are both accepted; or
- `contract`: final catch-up, validation, enforcement, and compatibility
  cleanup after pre-deployment instances and writers have drained.

One-shot backfill and manual work are statement kinds inside one of these
phases. Repeatable `operator_batched` work and `external_attestation` are
separately receipted `operations`, not a third schema phase. If one newly
deployed version cannot work before and after contract, the change requires
another plan.

`batches` are the execution boundary: a batch declares whether it is
transactional and carries its statements. A non-transactional batch must not
be wrapped in an explicit transaction by an executor.

Manual execution modes are `transactional_once`, `nontransactional_once`,
`operator_batched`, and `external_attestation`. An operator-batched operation
contains a bounded batch template, progress key, idempotency notes, and exact
completion query in `operations/<id>.json`; it is never smuggled into a phase
batch. External attestation contains evidence categories and no invented SQL.

`contract_gates` records exact read-only data assertions and external writer
attestations. `reconciliations` binds `assert_only` or reviewed `manual_sql`
work to the affected transition and gate IDs. A contract enforcement statement
must either reference its gate or carry a narrow catalog/atomic proof
disposition; a hazard string alone is invalid. Stable statement identity and
the bundle hash chain include these fields.

`needs_input` contains typed questions and no executable plan.
`needs_sql_edits` contains phased SQL with at least one explicit TODO and is not
a successful plan. `unsupported` contains catalog/planner conditions that
onwardpg cannot faithfully plan and also contains no executable plan. `ignored`
records exact catalog objects excluded by validated command-line or configured
target ignore selectors; it is not an assertion that their definitions are
safe to change.

Question fields are `id`, `kind`, `message`, `key`, `choices`,
`allows_freeform`, `current_fingerprint`, `desired_fingerprint`, and
`scope_fingerprint`. `id` is diagnostic identity; answers are matched by the
`(kind, key)` pair. The scope fingerprint commits to the participating typed
objects and relevant dependency closure, excluding unrelated objects that
merely share a schema.

## Generated internal answer receipt

Answers are the verbose fingerprint-bound implementation receipt beneath
semantic hints. No CLI accepts this as author input; onwardpg generates and
receipts it after validating semantic intent against the observed diff:

```json
{
  "current_fingerprint": "sha256:...",
  "desired_fingerprint": "sha256:...",
  "answers": [
    {
      "kind": "rename_table",
      "key": "table:public:old_name",
      "value": "table:public:new_name",
      "question_fingerprint": "sha256:..."
    }
  ]
}
```

The planner rejects stale fingerprints, missing required fields, duplicate or
contradictory entries, values outside a
question's choices, and entries unused by the current plan. `draft` carries
only receipts whose narrow question scope remains equivalent when history or
desired DDL moves. Its report lists carried and invalidated evidence. These
documents are implementation receipts: editing or supplying one is not a
supported way to express intent.

Niche operator work uses a semantic `manual_sql` hint. Without `work`, onwardpg
writes a blocking TODO into the relevant phase. A coding agent may instead
supply typed `work` directly, including a bounded operator-batched template.
One-shot edited SQL and Boolean gate pockets are receipted only after structural
validation and disposable clone convergence.

## Index lock mode

`plan` reports the mode of the durable bundle as `durable.index_lock_mode`.
`draft` reports the same object as the top-level `index_lock_mode`. The member
is absent when the command stopped before it selected a bundle. It names the mode
of the bundle on disk: a run that stops before it writes an existing bundle
reports the stored mode.

```json
{
  "status": "ready",
  "durable": {
    "status": "planned",
    "bundle_id": "search-indexes",
    "generation": 3,
    "index_lock_mode": {
      "concurrent_indexes": true,
      "source": "bundle"
    }
  }
}
```

| Field | Meaning |
| --- | --- |
| `concurrent_indexes` | `true`: `CREATE INDEX CONCURRENTLY` and `DROP INDEX CONCURRENTLY` in nontransactional batches. `false`: plain statements in transactional batches. |
| `source` | What decided the mode: `flag` (`--concurrent-indexes` on this run), `bundle` (the choice in `planner.options.concurrent_indexes` of the previous generation), `config` (`concurrent_indexes` of the target), or `default`. |
| `previous_concurrent_indexes` | Present only when this run wrote the bundle in another mode than its previous generation. The run also emits the warning `index_lock_mode_changed`. |
| `config_concurrent_indexes` | Present, and `true`, only when the bundle keeps the blocking mode of its previous generation although the target has `concurrent_indexes = true`. The run also emits the warning `index_lock_mode_differs_from_config`. |

The stored choice is the existing optional manifest member
`planner.options.concurrent_indexes`. It is absent for the blocking mode. No
bundle file has a new member, and no digest changes. The
[CLI reference](cli.md#index-lock-mode) has the precedence and the operations
that stay non-concurrent.

## Drop decisions

A drop asks for a decision only when it loses rows or a guarantee. The
`hazards` of the `drop` choice name what is lost:

| Dropped object | Decision | Choice hazards |
| --- | --- | --- |
| Schema, table, column, sequence, type, and every other object not listed here | Yes | `data_loss` |
| Standalone index that is not unique, is not the replica identity, and is not the index that the table is clustered on | No | — |
| Standalone unique index | Yes | `unique_index_enforcement_removed`, `duplicate_rows_possible` |
| Primary-key or unique constraint | Yes | `temporary_or_permanent_uniqueness_unenforced`, `duplicate_rows_possible` |
| Check, foreign-key, or exclusion constraint | Yes | The enforcement that is removed, for example `referential_enforcement_removed`, `orphan_rows_possible` |

Every drop of a unique index or of a constraint keeps one decision, also when
another index enforces the same uniqueness. onwardpg does not try to prove
that a unique index is redundant: uniqueness has other uses (foreign-key
targets, `ON CONFLICT`, replica identity) that such a proof must cover.

A statement that needs no decision is still in the plan and is still
classified. An index that enforces nothing is dropped in `contract` with
`safety=review` and the hazards `slower_queries_possible` and `blocking_lock`
(or `concurrent_index_drop`). Each contract statement requires the
`writers:legacy` gate, as before: code from before the deployment can still
depend on the index for speed.

A `drop` hint for an index that needs no decision is accepted
and is not written to the bundle. `plan` and `draft` report it as a finding
with the code `hint_not_needed`. Earlier versions asked for these hints.

### One hints file for many confirmations

A decision with exactly one choice is a confirmation. When a result has two or
more of them, `next_actions` has one more action after the `semantic_hint`
actions:

```json
{
  "scope": "durable",
  "kind": "semantic_hints_file",
  "decision_count": 30,
  "purpose": "answer the 30 decisions that have exactly one choice with one file",
  "hints": [{"kind": "drop", "object": "column", "name": ["public", "users", "legacy_a"]}],
  "hazards": ["data_loss"],
  "argv": ["onwardpg", "plan", "--target", "primary", "--hints-file", "onwardpg-hints.json"],
  "resolution": "review every hint and the hazards, remove each hint that the repository does not prove, write the hints array to onwardpg-hints.json, and run argv"
}
```

`hints` is the complete content of the file: the hints that the plan already
carries, then the one hint of each such decision. `hazards` is the union of
their hazards. A decision with more than one choice, such as rename or drop,
is never in the list. onwardpg does not write the file and confirms nothing by
itself. The `development` scope has the same action with `--dev-hints-file`.

## Warnings

A result document, and an error diagnostic, can have one more top-level member,
`warnings`. It is always the last member. A warning is an observation for the reader. It never changes
`status` or the exit code, and a consumer can ignore it. It exists only in the
output of the command: no bundle, manifest, receipt, or digest contains it.
The member is absent when there is nothing to report.

```json
{
  "status": "verified",
  "warnings": [
    {
      "code": "export_side_effects",
      "message": "the git status of 2 path(s) changed while schema_command ran; ...",
      "paths": ["generated/schema.ts", "packages/db/meta/_journal.json"],
      "path_count": 2,
      "remediation": "make schema_command write only to standard output; ..."
    }
  ]
}
```

Consumers should branch on `code`. The codes are:

| Code | Commands | Meaning |
| --- | --- | --- |
| `export_side_effects` | `init`, `plan`, `draft`, `verify`, `dev plan`, `config check` | The git status of the work tree changed while `schema_command` ran. |
| `index_lock_mode_changed` | `plan`, `draft` | The run wrote a bundle whose [index lock mode](#index-lock-mode) differs from its previous generation, in either direction. The message names both modes and what decided the new one. |
| `index_lock_mode_differs_from_config` | `plan`, `draft` | The bundle keeps the blocking mode of its previous generation although the target has `concurrent_indexes = true`. Run the plan once with `--concurrent-indexes` to move it. |

The two `index_lock_mode` warnings have `code`, `message`, and `remediation`.

For `export_side_effects`: `init`, `plan`, `draft`, `verify`, `dev plan`, and
`config check` emit it when they run a `schema_command` in a git work tree and
the output of `git status` after the last export run differs from the output
before the first. A command that stops with an error before its last export
run takes the second status as it writes the diagnostic. `paths` lists up to 50 changed paths in sorted order,
relative to the top level of the git work tree, and `path_count` is the number
that changed. The paths can be the work of the export command, of another
process, or of the developer. Files that git ignores are never listed. No
warning does not prove that the export command wrote nothing: see the limits in
the [safety model](safety-model.md#the-schema-export-and-the-checkout).

When a command prints SQL or text instead of a JSON document, it writes the
same member as one JSON line, `{"warnings":[...]}`, to standard error.

## Exit codes and diagnostics

| Exit code | Meaning | Standard output |
| --- | --- | --- |
| `0` | `planned`, `no_changes`, `absorbed`, `verified`, or `partial_verified` | result JSON, or SQL with `--output text` |
| `2` | `needs_input` or `needs_sql_edits` | decision/handoff JSON |
| `3` | `unsupported` | result JSON |
| `4` | policy blocked, stale, residual, or clone execution failed | command-specific status JSON |
| `1` | invocation, input, connection, configuration, bundle, or internal planning error | diagnostic JSON |

Errors outside a normal plan result use a stable diagnostic envelope:

```json
{
  "status": "error",
  "code": "invalid_config",
  "message": "..."
}
```

Consumers should branch on `code`; `message` is human-readable context and may
become more specific without changing the meaning of the code.
Current codes distinguish invocation, hints, answers, source, ignore, planning,
configuration, bundle, and history-integrity failures.

`draft` uses minimal decision and SQL-edit handoffs.
Complete and blocked reports currently retain detailed replay, reconciliation,
and verification receipts. Reconciliation reports exact preserved, refreshed,
and conflicting phase paths; a conflict leaves the existing bundle untouched.
Decision and SQL-edit handoffs exit `2`; unsupported state exits `3`; history
or convergence blockers exit `4`.

`dev plan` reports `preserved` typed objects
when workspace mode intentionally retains D-only catalog state, and uses
`status: "workspace_compatible"` for a converged safe local reconciliation.
`diff` (and the compatibility `plan --from --to`) uses the source-to-source
result shape; neither writes bundle state.

The preferred high-level `plan` command emits an envelope that
contains a durable H → W `draft` report and a separate development D → W
report, keyed by one worktree-local PlanID. Consumers must not treat the
development statements as the durable bundle, and should branch independently
on each nested report's status. The top-level state is `ready`, `needs_action`,
`blocked`, or `stale`, and ordered `next_actions` contain exact hints, argv,
edit pockets, failed postcondition pointers, and a `workspace_fast_forward`
handoff when D → W has safe statements. That handoff includes the rendered SQL,
statement count, preserved D-only objects, and `plan --output sql` argv. Its
reason is `accepted_history_changed` after a rebase and otherwise
`development_database_behind_desired_schema`. It never changes the durable
bundle. The compact high-level envelope does not expose the planner's internal
`applied_hints` list. Instead, every handoff or remaining decision choice
repeats the consumed ephemeral development hints in its argv, making the newest
command cumulatively executable without enumerating every combination of
independent choices. Argv is scoped to the same configured repository context;
it does not embed environment secrets or recreate the framework runtime.
If D → W reaches `needs_sql_edits`, the high-level envelope emits an
`inspect_manual_sql_handoff` action instead of leaving the agent at a blocking
status. Its cumulative `onwardpg dev plan` argv exposes the full typed manual
work template. The developer can complete that `manual_sql` hint and return it
to high-level `plan` as a `--dev-hint`, or rebuild the caller-owned development
database. This never creates a second durable edit pocket or changes H → W.

Top-level `ready` describes the durable artifact and is intentionally
non-mutating; consumers still inspect `next_actions` for an optional
`workspace_fast_forward`. If no durable bundle remains active because H already
equals W, the handoff argv names the plan explicitly and remains replayable.

The nested durable report calls the raw graph result `generated_plan`. Its
status describes generator-owned input before edit-pocket reconciliation; the
authoritative effective state is `durable.status`, backed by bundle and
verification receipts. Thus a verified edited artifact can be `planned` while
its `generated_plan` records why it originally needed SQL edits.
The durable report's `base_history_head` is the accepted H digest the feature
was planned from. Its nested verification `history_head` is the finalized chain
including the selected feature bundle; it must equal that installed manifest's
entry digest.

Durable H → W planning always runs. When the configured development database environment variable is absent,
the nested development report is `not_available`; an explicit development hint
requires that database. `--output sql` writes only D → W statements to stdout
and leaves incomplete-plan diagnostics on stderr.

When `development.status` is `needs_input`, its `next_action` is
`rerun_plan_with_dev_hints`. Re-run `onwardpg plan` with one `--dev-hint` per
development decision (or `--dev-hints-file`). The ordinary `--hint` and
`--hints-file` flags remain durable H → W intent only. This scope split keeps a
long-lived development database from accidentally authorizing a PR migration,
or vice versa.

The development report may include `accepted_postconditions`. These are only
assertions from newly accepted history explicitly marked
`-- onwardpg:dev-postcondition` in `verify.sql`; onwardpg runs each in a
read-only PostgreSQL transaction and reports `passed` or `failed`. An omitted
assertion is deliberately not evaluated, and either result is evidence rather
than proof that a historical migration phase ran.

`init` emits a successful document with
`status: "initialized"`, target and bundle identity, installed path, history
head, desired fingerprint, the complete empty-to-desired plan, and an embedded
clone-verification receipt. `needs_input` and `unsupported` preserve the
ordinary planner exits `2` and `3` without writing a bundle. A pre-existing
history returns `status: "blocked"`, a stable finding and remediation, and
exit `4`.

`history status` emits the ordered chain,
head bundle/digest, exact `head_ref`, and optional selected-bundle relationship.
The `head_ref` is the only accepted `draft --after` value. It never reads Git
or connects to PostgreSQL.

`status` reads the local active-plan anchor and
content-addressed repository history without Git or PostgreSQL access. Its
authoring states are `absent`, `parked`, `active`, `stale_parent`, `blocked`,
and `merge_ready`; verification is reported separately.

`verify` emits the selected phase checkpoint, total
and selected-bundle batch counts, assertion IDs, observed/full fingerprints,
and residual plan or typed execution failure. Full verification reports
`verified_assertions`. Partial verification instead reports assertions run by
the independent full execution as `full_continuation_assertions`; it does not
claim those assertions passed on the selected prefix. It exits `4` for
an unexpected residual or verification failure and has no caller-database
application surface. A successful normal
verification may set `receipts_updated: true` after atomically recording exact
edited phase and assertion digests. This is evidence, not a finalized or locked
bundle state; `--check` never writes it. Partial verification returns
`partial_verified` after proving both the selected prefix and full continuation
on independent disposable clones. It reports `simulated_bundle_phases`,
`remaining_bundle_phases`, and the expected residual; none records real
environment application or installs edited-file receipts.

`config check` emits `status: "valid"`, the
configuration version, and one materialized-DDL/database/history receipt per
target. `version` emits `status: "ok"` and a build
identity containing version, commit, dirty marker, build time, Go version, and
supported PostgreSQL majors.

Execution failures include a stable `failure.code`, the bundle, phase and batch
or assertion identity, the execution mode when relevant, and an exact
remediation. Current codes are `transactional_batch_failed`,
`non_transactional_batch_failed`, `assertion_query_failed`, and
`assertion_false`. Every such execution occurs only in an onwardpg-created
database, which is force-dropped on success, failure, or cancellation.

`drift check` emits target, history head,
expected and actual fingerprints, exact ignored objects, and deterministic
differences classified as `missing_in_actual`, `unexpected_in_actual`, or
`changed_in_actual`. Drift exits `4`; a matching live catalog exits `0`. Catalog
state the planner cannot model is listed in `unsupported` with status
`unsupported` and exit `3`, alongside the differences. `observer.mode` is
`database_owner`, `predefined_read_role`, or `dedicated_read_only` (`refused`
in the error for a role that the guard did not accept), and
`observer.bypass_rls` is present and `true` when the role has `BYPASSRLS`. When the
role of the live connection is refused, the error diagnostic also has
`observer` and `next_actions`; each action has `kind`, `reason`, and a `sql`
template for a [valid observer role](cli.md#observer-role). The findings
`observer_role_elevated` and `observer_access_incomplete` of `contract check`
carry the same `next_actions`.
