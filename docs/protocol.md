# JSON protocol

`draft` is the agent-facing authoring protocol. `plan` retains a more detailed
low-level protocol used by internal receipts and explicit source-to-source
callers. Consumers must branch on the version field before interpreting either
document; incompatible changes receive a new identifier.

## Minimal draft decisions

`onwardpg/draft/2` deliberately makes output and input asymmetric. onwardpg
emits the context needed to choose safely; the agent returns only semantic
intent that cannot be inferred from schema state.

~~~json
{
  "protocol": "onwardpg/draft/2",
  "status": "needs_decisions",
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
question key, prose labels, correlation IDs, and rerun command. The caller
already knows the invocation, and onwardpg already knows the fingerprints.
Every remaining field is needed to parse the response or choose between
different effects.

Each `hint` object is the exact accepted input shape. Pass one with repeatable
`--hint '<json-object>'`, or pass an array with `--hints-file`. Hints use strict
decoding: unknown fields, wrong identifier arity, empty identifiers, invalid
strategies, duplicates, contradictions, impossible mappings, and unused intent
fail. Identifier arrays preserve quoted names without inventing a delimiter.

Current semantic kinds are:

- `rename`: object kind plus exact `from` and `to` identifiers;
- `drop`: object kind plus exact current identifier;
- `type_change`: column identifier and `direct` or `manual_sql` strategy;
- `rollout`: column identifier and offered strategy (`not_null` is implied);
- `confirm`: exact object and authorization/rebuild/destructive action; and
- `manual_sql`: exact object and action handed to editable phase SQL.

Hints may be supplied before a question is emitted. An agent that already
understands the feature may construct the complete hints file in advance. The
planner iterates through its dependency-ordered question stages and consumes
any matching intent. One drop hint can therefore reject a rename and later
confirm removal. A hint that never consumes a real decision is rejected rather
than ignored.

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
  "protocol": "onwardpg/draft/2",
  "status": "needs_sql_edits",
  "path": "migrations/onward/primary/event-date",
  "edit": ["phases/migrate.sql"]
}
~~~

The agent edits those files directly. `ONWARDPG TODO` prevents receipt refresh,
and the bundle cannot participate as immutable base history. Only successful
full clone verification transitions the exact edited files to `planned`.

`--output text` renders the same choices as copyable `--hint` arguments. It
does not change planning semantics or prompt on a TTY.

## Low-level plan protocol

`onwardpg plan` writes JSON to standard output by default. Its current protocol
is `onwardpg.plan/v1`.

`--output text` is deliberately not JSON: it is a review-only rendering available only
when a plan is ready. It emits SQL comments for phase boundaries and
transactional batches so a committed migration remains legible to both humans
and coding agents. Automation should use JSON and render reviewed SQL from the
`statements` or `batches` fields.

## Result document

Every normal planner result has this shape:

```json
{
  "protocol_version": "onwardpg.plan/v1",
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

- `expand`: compatible additions to run before the application relies on them;
- `migrate`: reviewed schema work between compatible application releases;
  application-specific backfills remain an explicit human/agent responsibility;
- `contract`: work deferred until old application behavior is gone; or
- `manual`: review-only output that requires an explicit operator decision.

`batches` are the execution boundary: a batch declares whether it is
transactional and carries its statements. A non-transactional batch must not
be wrapped in an explicit transaction by an executor.

`needs_input` contains typed questions and no executable plan.
`needs_sql_edits` contains phased SQL with at least one explicit TODO and is not
a successful plan. `unsupported` contains catalog/planner conditions that
onwardpg cannot faithfully plan and also contains no executable plan. `ignored`
records catalog objects excluded by validated `--ignore` selectors; it is not
an assertion that their definitions are safe to change.

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
| `0` | `planned` | v1 result JSON, or SQL with `--output text` |
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

`draft` uses `onwardpg/draft/2` for minimal decision and SQL-edit handoffs.
Complete and blocked reports currently retain detailed replay, reconciliation,
and verification receipts. Reconciliation reports exact preserved, refreshed,
and conflicting phase paths; a conflict leaves the existing bundle untouched.
Decision and SQL-edit handoffs exit `2`; unsupported state exits `3`; history
or convergence blockers exit `4`.

`init` emits `onwardpg.history-init/v1`. A successful document has
`outcome: "initialized"`, target and bundle identity, installed path, history
head, desired fingerprint, the complete empty-to-desired plan, and an embedded
`onwardpg.verify/v1` clone receipt. `needs_input` and `unsupported` preserve the
ordinary planner exits `2` and `3` without writing a bundle. A pre-existing
history returns `outcome: "blocked"`, a stable finding and remediation, and
exit `4`.

`verify` emits `onwardpg.verify/v1` with the selected phase checkpoint,
executed batch count, observed/full fingerprints, and residual plan or typed
execution failure. It exits `4` for a residual or expected verification
failure and has no caller-database application surface. A successful normal
verification may set `receipts_updated: true` after atomically recording exact
edited phase and assertion digests. This is evidence, not a finalized or locked
bundle state; `--check` never writes it.

Execution failures include a stable `failure.code`, the bundle, phase and batch
or assertion identity, the execution mode when relevant, and an exact
remediation. Current codes are `transactional_batch_failed`,
`non_transactional_batch_failed`, `assertion_query_failed`, and
`assertion_false`. Every such execution occurs only in an onwardpg-created
database, which is force-dropped on success, failure, or cancellation.

`drift check` emits `onwardpg.drift-check/v1` with target, history head,
expected and actual fingerprints, exact ignored objects, and deterministic
differences classified as `missing_in_actual`, `unexpected_in_actual`, or
`changed_in_actual`. Drift exits `4`; a matching live catalog exits `0`.
