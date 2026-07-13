# Forward-only migration workflow

The normal durable workflow is one command repeated with the same bundle ID:

```sh
onwardpg draft --target primary --bundle payment-settlement
```

onwardpg replays accepted history, materializes the configured CREATE-statement
DDL, and plans from that history head to the schema declared by the working
code. It never applies SQL to the development, staging, or production database.

## Decisions are semantic and optional up front

If schema state cannot prove intent, `draft` exits `2` with only the valid
semantic choices. For example:

```json
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

The agent reruns the same command with the choice justified by its feature
context:

```sh
onwardpg draft --target primary --bundle payment-settlement \
  --hint '{"kind":"rename","object":"column","from":["app","users","name"],"to":["app","users","display_name"]}'
```

When the intent is already known, supply the same hint on the first call.
Hints are repeatable; `--hints-file` accepts an array. onwardpg validates every
hint against the exact graph diff and rejects impossible, contradictory, or
unused intent. The agent never copies fingerprints or opaque planner keys.

## Product-specific SQL belongs in SQL

When a safe transition depends on a product-aware cast, backfill, refresh, or
other operation that onwardpg cannot derive, choose `manual_sql`. The hint
contains no SQL:

```sh
onwardpg draft --target primary --bundle payment-settlement \
  --hint '{"kind":"type_change","name":["app","payments","settled_on"],"strategy":"manual_sql"}'
```

The command exits `2` with `needs_sql_edits` and names the phase file containing
an `ONWARDPG TODO`. Replace the TODO with reviewed SQL, optionally add boolean
assertions to `verify.sql`, then run:

```sh
onwardpg verify --target primary --bundle payment-settlement
```

Verification executes only in onwardpg-created disposable databases and
requires an empty residual diff. The developer or coding agent decides how and
when the receipted phase files run in a real environment.

## Review and rollout

Generated SQL is split into `expand`, `migrate`, `manual`, and `contract`
phases with transaction and hazard comments. Run expand before compatible
application code, complete data work while both shapes are supported, and
delay contract until old code is gone. Concurrent index operations remain in
their required non-transactional batches.

Keep rerunning `draft` with the same bundle ID while the feature changes or
after new base migrations arrive. The selected bundle remains the one
cumulative history-head-to-working-schema transition. There is no lock or
finalize command, no down migration, and no automatic application command.

Recovery from a deployed mistake is another reviewed forward migration.
