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

`current_fingerprint` and `desired_fingerprint` identify the complete typed
catalog graphs used for the decision. A fingerprint change means a new planning
input, even if an object name appears unchanged.

`planned` contains forward SQL. Each statement has `sql`, `safety`, `phase`,
and optional `hazards`. `phase` is one of:

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
`allows_freeform`, `current_fingerprint`, and `desired_fingerprint`. `id` is
diagnostic identity; answers are matched by the `(kind, key)` pair.

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
      "key": "table:public.old_name",
      "value": "table:public.new_name"
    }
  ]
}
```

Generate this from the exact `needs_input` result, save it in the pull request,
and pass it with `--answers answers.json`. The planner rejects a different
protocol version, stale fingerprints, missing required fields, duplicate or
contradictory entries, values outside a question's choices, and entries that
are unused by the current plan. Do not carry answer files between independent
schema revisions.

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
      "SELECT 1;"
    ]
  }
}
```

`execution_mode` is required and is either `transactional` or
`non_transactional`; onwardpg uses it to form the manual batch. `summary` and
each verification query must be one line because they are rendered as SQL
comments. `statements` are the only executable, operator-supplied text and
may contain multi-line SQL. The contract is still fingerprint-bound, checked
for usage, and shown in the resulting statement's `manual` metadata.

Current manual-work questions include `partition_reconfiguration` and
`refresh_materialized_view`. The latter is emitted when a typed changed-view
dependency can leave materialized rows stale; an answer must provide the
operator-chosen `REFRESH MATERIALIZED VIEW` form and any required verification.

## Exit codes and diagnostics

| Exit code | Meaning | Standard output |
| --- | --- | --- |
| `0` | `planned` | v1 result JSON, or SQL with `--sql` |
| `2` | `needs_input` | v1 result JSON |
| `3` | `unsupported` | v1 result JSON |
| `1` | invocation, input, connection, or internal planning error | diagnostic JSON with `status: "error"` and `error` |

The `status: "error"` diagnostic envelope is useful to operators but is not
yet a versioned v1 result document. Treat it as an error message rather than a
stable machine contract; formalizing a versioned error/diagnostic envelope is
an outstanding production-readiness item.
