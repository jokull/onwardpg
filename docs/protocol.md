# JSON protocol

`plan` is the agent-facing authoring protocol. It carries one durable H → W
report and one separately scoped D → W development report. `draft` and `diff`
retain lower-level protocols for explicit history and source-to-source callers.
Consumers must branch on the version field before interpreting any document;
incompatible changes receive a new identifier.

## Minimal draft decisions

`onwardpg.draft/v5` deliberately makes output and input asymmetric. onwardpg
emits the context needed to choose safely; the agent returns only semantic
intent that cannot be inferred from schema state.

~~~json
{
  "protocol_version": "onwardpg.draft/v5",
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
~~~

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

`manual_sql` contains no SQL. It returns this nonterminal envelope after writing
a phase-local TODO:

~~~json
{
  "protocol_version": "onwardpg.draft/v5",
  "status": "needs_sql_edits",
  "next_action": "edit_files_then_verify",
  "path": "migrations/onward/app/event-date",
  "edit": ["phases/contract.sql", "phases/expand.sql"]
}
~~~

The agent edits only the generated pockets bounded by stable
`-- onwardpg:edit begin/end` markers unless it deliberately takes ownership of
more of the phase. `ONWARDPG TODO` prevents receipt refresh,
and the bundle cannot participate as immutable base history. Only successful
full clone verification transitions the exact edited files to `planned`.

`--output text` renders the same choices as copyable `--hint` arguments. It
does not change planning semantics or prompt on a TTY.

## Source-to-source compatibility protocol

`onwardpg diff` (and the compatibility spelling `onwardpg plan --from --to`)
writes JSON to standard output by default. Its public command protocol is
`onwardpg.plan/v4` for planned, decision, and unsupported results.
The receipted planner document embedded in bundles retains its separately
versioned internal `onwardpg.plan/v2` schema.

`--output text` is deliberately not JSON: it is a review-only rendering available only
when a plan is ready. It emits SQL comments for phase boundaries and
transactional batches so a committed migration remains legible to both humans
and coding agents. Automation should use JSON and render reviewed SQL from the
`statements` or `batches` fields.

## Result document

Every normal planner result has this shape:

```json
{
  "protocol_version": "onwardpg.plan/v4",
  "current_fingerprint": "sha256:...",
  "desired_fingerprint": "sha256:...",
  "status": "planned | needs_input | needs_sql_edits | unsupported",
  "statements": [],
  "batches": [],
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
optional `hazards`, and a deterministic content-derived `id`. Bundle amendments
will bind to the ID rather than a fragile statement-array index. `phase` is one
of:

- `expand`: an acceptance-compatible schema while pre-deployment code is live,
  before the one application deployment anchored to the bundle. It may
  explicitly relax enforcement so legacy and new SQL are both accepted; or
- `contract`: final catch-up, validation, enforcement, and compatibility
  cleanup after pre-deployment instances and writers have drained.

Backfill and manual work are statement kinds inside one of these phases, not
additional deployment phases. If one newly deployed version cannot work before
and after contract, the change requires another plan.

`batches` are the execution boundary: a batch declares whether it is
transactional and carries its statements. A non-transactional batch must not
be wrapped in an explicit transaction by an executor.

`needs_input` contains typed questions and no executable plan.
`needs_sql_edits` contains phased SQL with at least one explicit TODO and is not
a successful plan. `unsupported` contains catalog/planner conditions that
onwardpg cannot faithfully plan and also contains no executable plan. `ignored`
records exact catalog objects excluded by validated command-line or configured
target ignore selectors; it is not an assertion that their definitions are
safe to change.

Question fields are stable v1 keys: `id`, `kind`, `message`, `key`, `choices`,
`allows_freeform`, `current_fingerprint`, `desired_fingerprint`, and
`scope_fingerprint`. `id` is diagnostic identity; answers are matched by the
`(kind, key)` pair. The scope fingerprint commits to the participating typed
objects and relevant dependency closure, excluding unrelated objects that
merely share a schema.

## Generated internal answer receipt

Answers are the verbose fingerprint-bound implementation protocol beneath
semantic hints. No CLI accepts this as author input; onwardpg generates and
receipts it after validating semantic intent against the observed diff:

```json
{
  "protocol_version": "onwardpg.plan/v1",
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

The planner rejects a different protocol version, stale fingerprints, missing
required fields, duplicate or contradictory entries, values outside a
question's choices, and entries unused by the current plan. `draft` carries
only receipts whose narrow question scope remains equivalent when history or
desired DDL moves. Its report lists carried and invalidated evidence. These
documents are implementation receipts: editing or supplying one is not a
supported way to express intent.

Niche operator work uses a semantic `manual_sql` hint. The hint contains no
SQL; onwardpg writes a blocking TODO into the relevant phase. The agent edits
that SQL file and optional boolean assertions directly, then `verify` receipts
the exact files after disposable clone convergence.

## Exit codes and diagnostics

| Exit code | Meaning | Standard output |
| --- | --- | --- |
| `0` | `planned`, `no_changes`, `absorbed`, `verified`, or `partial_verified` | versioned result JSON, or SQL with `--output text` |
| `2` | `needs_input` or `needs_sql_edits` | versioned decision/handoff JSON |
| `3` | `unsupported` | v1 result JSON |
| `4` | policy blocked, stale, residual, or clone execution failed | command-specific versioned status JSON |
| `1` | invocation, input, connection, configuration, bundle, or internal planning error | `onwardpg.diagnostic/v1` JSON |

Errors outside a normal plan result use a stable diagnostic envelope:

```json
{
  "protocol_version": "onwardpg.diagnostic/v1",
  "status": "error",
  "code": "invalid_config",
  "message": "..."
}
```

Consumers should branch on `protocol_version` and `code`; `message` is
human-readable context and may become more specific without a protocol change.
Current codes distinguish invocation, hints, answers, source, ignore, planning,
configuration, bundle, and history-integrity failures.

`draft` uses `onwardpg.draft/v5` for minimal decision and SQL-edit handoffs.
Complete and blocked reports currently retain detailed replay, reconciliation,
and verification receipts. Reconciliation reports exact preserved, refreshed,
and conflicting phase paths; a conflict leaves the existing bundle untouched.
Decision and SQL-edit handoffs exit `2`; unsupported state exits `3`; history
or convergence blockers exit `4`.

`dev plan` emits `onwardpg.dev-plan/v5`. It reports `preserved` typed objects
when workspace mode intentionally retains D-only catalog state, and uses
`status: "workspace_compatible"` for a converged safe local reconciliation.
`diff` (and the compatibility `plan --from --to`) retains the source-to-source
`onwardpg.plan/v4` protocol; neither writes bundle state.

The preferred high-level `plan` command emits `onwardpg.plan/v5`. Its envelope
contains a durable H → W `draft` report and a separate development D → W
report, keyed by one worktree-local PlanID. Consumers must not treat the
development statements as the durable bundle, and should branch independently
on each nested report's status. `--output sql` writes only D → W statements to
stdout and leaves incomplete-plan diagnostics on stderr.

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

`init` emits `onwardpg.history-init/v2`. A successful document has
`status: "initialized"`, target and bundle identity, installed path, history
head, desired fingerprint, the complete empty-to-desired plan, and an embedded
`onwardpg.verify/v4` clone receipt. `needs_input` and `unsupported` preserve the
ordinary planner exits `2` and `3` without writing a bundle. A pre-existing
history returns `status: "blocked"`, a stable finding and remediation, and
exit `4`.

`history status` emits `onwardpg.history-status/v2` with the ordered chain,
head bundle/digest, exact `head_ref`, and optional selected-bundle relationship.
The `head_ref` is the only accepted `draft --after` value. It never reads Git
or connects to PostgreSQL.

`status` emits `onwardpg.status/v1`. It reads the local active-plan anchor and
content-addressed repository history without Git or PostgreSQL access. A
missing anchor is an explicit `no_active_plan` status rather than an inferred
branch state.

`verify` emits `onwardpg.verify/v4` with the selected phase checkpoint, total
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

`config check` emits `onwardpg.config-check/v3` with `status: "valid"`, the
configuration version, and one materialized-DDL/database/history receipt per
target. `version` emits `onwardpg.version/v1` with `status: "ok"` and the build
version. These are normal versioned success documents, not protocol exceptions.

Execution failures include a stable `failure.code`, the bundle, phase and batch
or assertion identity, the execution mode when relevant, and an exact
remediation. Current codes are `transactional_batch_failed`,
`non_transactional_batch_failed`, `assertion_query_failed`, and
`assertion_false`. Every such execution occurs only in an onwardpg-created
database, which is force-dropped on success, failure, or cancellation.

`drift check` emits `onwardpg.drift-check/v2` with target, history head,
expected and actual fingerprints, exact ignored objects, and deterministic
differences classified as `missing_in_actual`, `unexpected_in_actual`, or
`changed_in_actual`. Drift exits `4`; a matching live catalog exits `0`.
