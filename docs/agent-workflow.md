# Agent-assisted migration workflow

onwardpg has one small contract with a coding agent:

1. The agent identifies the accepted base bundle from its Git context.
2. onwardpg derives everything PostgreSQL catalog state can prove.
3. The agent supplies only product intent that the schemas cannot prove.
4. Intricate product work is edited as SQL, not encoded as orchestration JSON.
5. onwardpg receipts and clone-verifies the resulting bundle, but never applies
   it to a caller-owned database.

The agent may already know the intent before planning. That is useful: semantic
hints can be included in the first `draft` call. They are not accepted on
trust; every hint must match a real ambiguity in the exact current-to-desired
graph diff.

## The loop

Start or refresh one logical feature bundle:

```sh
onwardpg draft --target primary --bundle profile-name --after baseline
```

Branch on the documented result:

| Status / exit | Agent action |
| --- | --- |
| `planned`, `no_changes`, or `absorbed` / `0` | Review phases and hazards, or remove the redundant PR migration when absorbed; then run the applicable CI check. |
| `needs_decisions` / `2` | Select an offered semantic hint only when feature context establishes it; otherwise ask the developer. |
| `needs_sql_edits` / `2` | Edit the named phase files, remove every TODO, add useful assertions, and run `verify`. |
| `unsupported` / `3` | Stop. Change the design, narrow an explicitly accepted ignore, or add planner support. |
| blocker / `4` | Repair history, convergence, or receipt integrity. |
| error / `1` | Fix invocation, credentials, DDL, or environment. |

Repeat `draft` with the same bundle ID whenever the declarative schema changes.
When accepted base history moves, update `--after` to the new accepted tip.
There is no finalize state. Applying SQL to a local developer database does
not advance the durable history head.

## Decisions: predictable ahead of time, offered when needed

Suppose a feature renames `app.users.name` to `display_name`. If the agent
already knows that from the feature request, it can start with:

```sh
onwardpg draft \
  --target primary \
  --bundle profile-name \
  --after baseline \
  --hint '{"kind":"rename","object":"column","from":["app","users","name"],"to":["app","users","display_name"]}'
```

If it does not supply the hint, onwardpg exits `2` and offers the same exact
object as a choice alongside the destructive alternative:

```json
{
  "protocol_version": "onwardpg/draft/3",
  "status": "needs_decisions",
  "next_action": "rerun_same_command_with_hints",
  "decisions": [
    {
      "choices": [
        {
          "hint": {
            "kind": "rename",
            "object": "column",
            "from": ["app", "users", "name"],
            "to": ["app", "users", "display_name"]
          }
        },
        {
          "hint": {
            "kind": "drop",
            "object": "column",
            "name": ["app", "users", "name"]
          },
          "hazards": ["data_loss"]
        }
      ]
    }
  ]
}
```

The choice object is deliberately copyable as a `--hint`. Use `--output text`
for shell-oriented output or `--hints-file` for several already-known choices.
Identifier components are arrays, so quoted names containing punctuation stay
unambiguous.

Current semantic kinds are:

| Kind | Says only what cannot be inferred |
| --- | --- |
| `rename` | The old and new identities denote the same object. |
| `drop` | The missing object is intentionally destructive. |
| `type_change` | Use the direct PostgreSQL transition or hand the conversion to edited SQL. |
| `rollout` | Choose direct or staged `NOT NULL` rollout timing. |
| `confirm` | Approve a precisely described destructive or lifecycle action. |
| `manual_sql` | Hand a named niche operation to the bundle's SQL phase. |

No hint contains schema fingerprints, planner question IDs, SQL statements,
transaction boundaries, or verification queries. Those are either inferable,
generated evidence, or belong in the SQL handoff.

Ahead-of-time hints are strict. A typo that names no actual decision, two hints
that answer the same decision differently, or a hint made obsolete by a schema
edit fails rather than being silently ignored.

## SQL handoff: take the migration the last mile

For a product-specific timestamp-to-date conversion, the agent can choose the
offered manual strategy:

```sh
onwardpg draft \
  --target primary \
  --bundle payment-settlement \
  --after baseline \
  --hint '{"kind":"type_change","name":["app","payments","settled_on"],"strategy":"manual_sql"}'
```

onwardpg creates the bundle and reports only the files needing ownership:

```json
{
  "protocol_version": "onwardpg/draft/3",
  "status": "needs_sql_edits",
  "next_action": "edit_files_then_verify",
  "path": "migrations/onward/primary/payment-settlement",
  "edit": ["phases/migrate.sql"]
}
```

The agent replaces the phase-local TODO with ordinary reviewable SQL:

```sql
-- Product rule: reporting dates use the account's agreed business timezone.
ALTER TABLE app.payments
  ALTER COLUMN settled_on TYPE date
  USING (settled_at AT TIME ZONE 'Atlantic/Reykjavik')::date;
```

It may add postconditions separately:

```sql
-- onwardpg:assert captured_payments_have_a_settlement_date
SELECT count(*) = 0
FROM app.payments
WHERE status = 'captured' AND settled_on IS NULL;
```

Then it asks onwardpg to execute the exact phase bytes on a disposable clone:

```sh
onwardpg verify --target primary --bundle payment-settlement
```

An unresolved TODO, false assertion, execution failure, or residual schema diff
blocks the receipt. No amount of semantic JSON can bypass those checks.

## What is stored

A bundle keeps human-reviewable work and machine evidence together:

```text
migrations/onward/primary/payment-settlement/
├── manifest.json
├── decisions.json       # semantic hints plus their bound internal evidence
├── questions.json       # generated planner receipt
├── answers.json         # generated planner receipt; never hand-authored
├── plan.json
├── verify.sql           # optional agent-authored boolean assertions
└── phases/
    ├── expand.sql
    ├── migrate.sql
    └── contract.sql
```

Only non-empty phase files are present. `decisions.json`, `questions.json`, and
`answers.json` are generated receipts. The supported agent inputs are semantic
`--hint` values and ordinary phase/verification SQL.

Generated phases document purpose, order, hazards, and transactional
boundaries. After an agent takes ownership of SQL, verification receipts the
exact edited bytes. Later regeneration carries an unaffected edit forward and
turns a same-phase conflict into an explicit three-way SQL handoff. The current
file is preserved, the report includes old/current/new SQL, and the resolved
file remains unreceipted until disposable-clone verification succeeds.

## Boundaries

The agent owns Git operations, identification of the accepted predecessor,
enforcement that one PR introduces at most one mutable bundle per target,
deployment timing, application compatibility,
representative data, and execution through the project's normal tooling.
onwardpg owns history-chain validation, schema materialization, graph planning,
decision validation, deterministic bundle generation, and disposable-clone
verification.

An agent must never select a destructive choice merely to make the CLI exit
zero, invent application-specific SQL, treat `unsupported` as equivalence, or
apply generated SQL to a real environment on onwardpg's behalf.
