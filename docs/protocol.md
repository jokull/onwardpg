# JSON protocol

`onwardpg plan` writes JSON to standard output by default. The current stable
success/decision protocol is `onwardpg.plan/v1`. Consumers must branch on
`protocol_version` before interpreting a result. A future incompatible change
will use a new identifier rather than silently changing v1 fields.

`--sql` is deliberately not JSON: it is a review-only rendering available only
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
  "status": "planned | needs_input | unsupported",
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

`needs_input` contains typed questions and no executable plan. `unsupported`
contains catalog/planner conditions that onwardpg cannot faithfully plan and
also contains no executable plan. `ignored` records catalog objects excluded by
validated `--ignore` selectors; it is not an assertion that their definitions
are safe to change.

Question fields are stable v1 keys: `id`, `kind`, `message`, `key`, `choices`,
`allows_freeform`, `current_fingerprint`, `desired_fingerprint`, and
`scope_fingerprint`. `id` is diagnostic identity; answers are matched by the
`(kind, key)` pair. The scope fingerprint commits to the participating typed
objects and relevant dependency closure, excluding unrelated objects that
merely share a schema.

## Answer document

Answers are a separate, fingerprint-bound input:

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

Generate this from the exact `needs_input` result, save it in the pull request,
and pass it with `--answers answers.json`. The planner rejects a different
protocol version, stale fingerprints, missing required fields, duplicate or
contradictory entries, values outside a question's choices, and entries that
are unused by the current plan. Copy `scope_fingerprint` from the exact
question into the answer's `question_fingerprint`.

Do not manually edit whole-schema fingerprints to carry an answer between
revisions. `pr status` and `pr regenerate` use
`onwardpg.answer-rebind/v1` to carry only answers whose question scope is
byte-for-byte equivalent. Its report lists `carried`, `invalidated`,
`unanswered`, and staged decisions that are `deferred` until their planner
stage becomes reachable. The rebound answer document receives the new global
fingerprints.

### Manual work contract

Some questions, such as `partition_reconfiguration`, require a reviewed
operator-owned contract instead of an acknowledgement. Its answer uses
`value: "provided"` and contains the SQL onwardpg must not invent:

```json
{
  "kind": "partition_reconfiguration",
  "key": "table:app:events_2026",
  "value": "provided",
  "manual": {
    "summary": "move the empty partition to its reviewed range",
    "execution_mode": "transactional",
    "statements": [
      "ALTER TABLE \"app\".\"events\" DETACH PARTITION \"app\".\"events_2026\";",
      "ALTER TABLE \"app\".\"events\" ATTACH PARTITION \"app\".\"events_2026\" FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');"
    ],
    "verification_sql": [
      "SELECT count(*) = 0 FROM \"app\".\"events_2026\";"
    ]
  }
}
```

`execution_mode` is required and is either `transactional` or
`non_transactional`; onwardpg uses it to form the manual batch. `summary` and
each verification query must be one line because they are rendered as SQL
comments. During clone verification each query must return exactly one boolean
row, and every value must be true. `statements` are the only executable,
operator-supplied text and may contain multi-line SQL. The contract is still
fingerprint-bound, checked for usage, and shown in the resulting statement's
`manual` metadata.

Current manual-work questions include `partition_reconfiguration`,
`refresh_materialized_view`, and `backfill_not_null`. The last is a second
stage after choosing `staged_with_backfill` for `set_not_null`: onwardpg emits
the `NOT VALID` structural guard, executes only the supplied application-owned
backfill in the manual phase, requires its boolean postcondition, and leaves
validation plus `SET NOT NULL` for contract. A plain `staged` choice remains
available when the developer has separately proved the data is already clean.

## Exit codes and diagnostics

| Exit code | Meaning | Standard output |
| --- | --- | --- |
| `0` | `planned` | v1 result JSON, or SQL with `--sql` |
| `2` | `needs_input` | v1 result JSON |
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
Current codes distinguish invocation, answers, source, ignore, planning,
configuration, bundle, and Git-status failures.

`pr regenerate` emits `onwardpg.pr-analysis/v1`. It contains prepared-tree
revision receipts, complete schema-square fingerprints, the ordinary
`onwardpg.plan/v1` result, and the written bundle generation/path. A blocked
base-integrity result omits `plan` and exits `4`; needs-input and unsupported
planner results retain exits `2` and `3`. The optional Git CLI wrapper reports
its separate `onwardpg.git-status/v1` result when repository classification is
blocked.

`pr status --bundle` emits `onwardpg.pr-status/v1` with repository,
PR-analysis, and `onwardpg.freshness/v1` receipts. It is read-only. Stale
findings carry stable codes and remediation text and exit `4`.

`bundle verify` emits `onwardpg.verify/v1` with the selected phase checkpoint,
executed batch count, observed/full fingerprints, and residual plan or typed
execution failure. `ci check` emits `onwardpg.ci-check/v1`, composing repository
classification, PR freshness/schema-square receipts, and full clone
verification. Both exit `4` for an expected non-success outcome; neither has a
production-apply surface.
