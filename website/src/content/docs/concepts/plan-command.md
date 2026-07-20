---
title: The plan command
description: One evolving migration reconciles accepted history, working DDL, and a branch-worn development database.
---

`onwardpg plan` is the core product. It does more than print the difference between two schemas. It keeps one feature migration alive while both the feature and the accepted migration chain change underneath it.

```sh
# Start the migration once.
onwardpg plan checkout-preferences

# Repeat after every model edit, decision, and rebase.
onwardpg plan
```

The first call creates one worktree-local `PlanID` and one durable bundle. Every later call revises that same logical migration. There is no trail of speculative fixups. When accepted history absorbs the verified bundle, the next `plan` retires this checkout's authoring anchor automatically; it does not claim merge or deployment.

## Start easy. Keep the same command when it gets ugly.

The useful test of a planner is not whether it can print `ALTER TABLE`. It is
whether the same workflow continues to help as compatibility, product meaning,
and PostgreSQL dependencies accumulate. These four examples are generated from
checked-in DDL fixtures by the current CLI; the displayed statements are
selected directly from those phase files.

### Easy: add a nullable column

The desired schema adds `status text`. No old write becomes invalid, no data
meaning is missing, and no dependent object must move. `plan` exits successfully
with one expand statement:

```sql
ALTER TABLE "app"."bookings" ADD COLUMN "status" text;
```

There is no empty `contract.sql` for ceremony's sake. The planner writes only
the non-empty phase. This is the familiar diff-tool case—and it uses the same
bundle, hazard, history, and verification machinery as every harder rung.

### Medium: make the new column required

Change the destination to `status text NOT NULL` and the same SQL is no longer
enough. Old code can still insert without `status` after expand. Actual output
therefore asks whether to `assert_only`, supply `manual_sql`, or `split_plan`.
With reviewed cleanup selected, it adds the nullable shape first, reports
`needs_sql_edits`, and puts cleanup plus an exact gate before enforcement:

```sql
-- expand.sql
ALTER TABLE "app"."bookings" ADD COLUMN "status" text;

-- contract.sql
-- PRODUCT-SPECIFIC SQL: Provide reviewed reconcile_contract_sql SQL for app.bookings.status
-- Verify: SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL);
DO $onwardpg$ BEGIN IF NOT COALESCE((SELECT NOT EXISTS (SELECT 1 FROM "app"."bookings" WHERE "status" IS NULL)), false) THEN RAISE EXCEPTION 'onwardpg contract gate failed: data:1c16b884027de910'; END IF; END $onwardpg$;
ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;
```

Now the output answers the question a raw diff cannot: *when is each operation
compatible?* The application deploy and drain sit between those files. The
developer supplies the historical value; onwardpg keeps that edit ahead of its
receipted data assertion and the constraint. The complete generated and edited
files live in the
[`required-column` receipt](https://github.com/jokull/onwardpg/tree/main/docs/receipts/required-column).

### Hard: rename a column while both releases are alive

Two snapshots cannot prove that `display_name` and `full_name` are one product
field, so `plan` first asks for a semantic rename decision. It then asks how to
backfill existing rows. With `single_transaction` explicitly selected for a
known-small table, the current receipt contains this choreography:

```sql
-- expand.sql, selected statements in emitted order
ALTER TABLE "app"."accounts" ADD COLUMN "full_name" text;
CREATE TRIGGER "onwardpg_sync_column_4cff936be08db67c" BEFORE INSERT OR UPDATE OF "display_name", "full_name" ON "app"."accounts" FOR EACH ROW EXECUTE FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();
UPDATE "app"."accounts" SET "full_name" = "display_name" WHERE "full_name" IS DISTINCT FROM "display_name";

-- contract.sql, after the old release drains
UPDATE "app"."accounts" SET "full_name" = "display_name" WHERE "full_name" IS DISTINCT FROM "display_name";
DROP TRIGGER "onwardpg_sync_column_4cff936be08db67c" ON "app"."accounts";
DROP FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();
ALTER TABLE "app"."accounts" DROP COLUMN "full_name";
ALTER TABLE "app"."accounts" RENAME COLUMN "display_name" TO "full_name";
```

The omitted generated function is not hand-waved: it handles inserts, one-sided
updates, and conflicting dual writes, while contract performs a final catch-up
and equality assertion before cleanup. The
[`rename` receipt](https://github.com/jokull/onwardpg/tree/main/docs/receipts/rename)
contains every emitted byte and is clone-verified by the documentation test.
For a large table, provide `operator_batched` work instead. onwardpg writes a
separate receipted `operations/<id>.json` containing the bounded batch template,
progress key, idempotency notes, and generated equality completion query. It is
not rendered into a phase transaction. `contract check` reports that exact
operation as `reconciliation_required` until the equality gate is true.

### Nightmare: change a type beneath views and indexes

Now change `app.facts.val integer` to `bigint` while it feeds an ordinary view,
a materialized view, and that materialized view's unique index. This is where a
coding agent becomes genuinely useful: it can inspect application readers,
production value shapes, and compose the compatibility bridge and conversion.
It should not have to rediscover PostgreSQL's dependency graph.

After the agent supplies the `type_change: manual_sql` decision, onwardpg names
the exact dependency scope and emits this order around the editable conversion:

```sql
-- contract.sql, selected statements in emitted order
DROP MATERIALIZED VIEW "app"."fact_cache";
DROP VIEW "app"."fact_view";

-- ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL for column:app:facts:val (integer -> bigint).
ANALYZE "app"."facts" ("val");

CREATE VIEW "app"."fact_view" AS SELECT val
   FROM app.facts;
CREATE MATERIALIZED VIEW "app"."fact_cache" AS SELECT val
   FROM app.fact_view WITH DATA;

-- onwardpg:batch nontransactional
CREATE UNIQUE INDEX CONCURRENTLY "fact_cache_val_idx" ON "app"."fact_cache" USING "btree" ("val" NULLS LAST);
```

This division of labor is the point. The agent owns whether values should cast,
map, reject, or be backfilled through a shadow interface. onwardpg owns the
catalog-proven closure: reverse dependency order for destruction, desired
dependency order for recreation, materialized-view population state, index
definition, transaction boundary, hazards, and the final empty residual diff.

The proof is executable. The
[`dependency-type-change` receipt](https://github.com/jokull/onwardpg/tree/main/docs/receipts/dependency-type-change)
is regenerated through the CLI. In
`TestPinnedStripeRejectsDerivedViewTypeClosureOnwardGeneratesIt`, the repository
also edits the conversion pocket, executes the whole plan on PostgreSQL, compares
the final typed fingerprint, and requires no residual. The pinned Stripe
reference fails that same combined view/materialized-view case, while onwardpg
converges. Independent PostgreSQL integration tests
`TestViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL` and
`TestMaterializedViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL`
prove the ordinary-view and materialized-view closures, including retained
privileges, population state, comments, indexes, and transitive output types.

The fourth rung is the payoff: onwardpg does not become less useful when an
agent enters the loop. The agent supplies facts PostgreSQL cannot know; the
planner keeps those facts inside a dependency-correct, evolving, verifiable
migration.

## First question: what is “the current schema”?

During feature development, there is no single current schema. There are several states with different authority:

```text
framework models ── export SQL ── materialize in PostgreSQL ── W (working intent)

accepted bundles ── replay in PostgreSQL ─────────────────── H (accepted history)

developer database ── read-only catalog inspection ───────── D (local reality)

production ── explicit read-only drift audit only ────────── P (deployed reality)

accepted history + selected bundle ── disposable replay ──── V (verification)
```

These states are not interchangeable:

| State | What it answers | How onwardpg uses it |
| --- | --- | --- |
| **W — working** | What schema does this checkout declare now? | Framework DDL is executed in disposable PostgreSQL and catalog-inspected. |
| **H — history** | What will a clean environment have before this feature? | Accepted onwardpg bundles are replayed from their content-addressed chain. |
| **D — development** | What has this developer already applied locally? | Inspected read-only and reconciled separately for convenience. |
| **P — production** | Did deployed reality drift from accepted history? | Consulted only by explicit `drift check`, never as permission to generate a PR migration. |
| **V — verification** | Do the exact bundle bytes replay and converge? | Independent disposable clones execute the prefix and full continuation. |

Here is the first “a-ha”: the durable PR migration is always **H → W**. A developer database cannot accidentally become migration history, and production state cannot silently authorize a repair.

## So why does `plan` inspect my development database?

Because useful development ergonomics need a second comparison: **D → W**.

One invocation produces two deliberately separate answers:

```text
H → W   durable bundle reviewed, merged, and deployed by the team
D → W   direct local reconciliation printed for the developer
```

JSON output keeps both reports in separate fields. `onwardpg plan --output sql` prints only the D → W SQL so you may choose to pipe it to your local database:

```sh
onwardpg plan --output sql | psql "$ONWARDPG_DEV_DATABASE_URL"
```

onwardpg itself still applies nothing to D, P, staging, or production.

When D → W has safe statements, ordinary JSON output also adds a
`workspace_fast_forward` next action containing the SQL and exact argv. After a
rebase its reason is `accepted_history_changed`, so an agent can distinguish
“bring my dev database across the new history head” from feature migration SQL.
Any D-only objects that workspace mode preserved are named on the action; they
are neither dropped nor absorbed into H → W. If generating that SQL consumed a
local-only `--dev-hint`, the action repeats it so `--output sql` does not ask the
same question again.

`ready` means the durable migration is ready; onwardpg never mutates the
caller-owned development database as part of that claim. Agents should inspect
`next_actions` even on a successful response and may execute the optional
fast-forward when they want D to catch up. If H already equals W and no durable
bundle remains active, the emitted argv names the plan explicitly so it still
replays successfully.

## What if I rename something before the feature merges?

Suppose accepted history H has neither `quote_mode` nor `pricing_mode`.

1. Your first draft adds `quote_mode`; H → W correctly plans `ADD quote_mode`.
2. You apply that development SQL locally, so D now contains `quote_mode`.
3. Before merge, you rename the model field to `pricing_mode`.
4. H → W forgets the abandoned draft name and cleanly plans `ADD pricing_mode`.
5. D → W notices the local-only shape and asks whether to rename `quote_mode` directly or preserve it.

The production bundle never inherits a rename that exists only because of local iteration. That is the second “a-ha”: the same diff engine understands *which relationship* it is planning, not merely that two catalogs differ.

## What if I switch branches and D is ahead?

A long-lived development database accumulates objects from several branches. In the default `workspace` mode, onwardpg preserves D-only objects instead of proposing their removal. Absence from the current checkout is not treated as evidence that another branch’s table should be dropped.

If D contains a genuine incompatible transition, `plan` may ask a separate development question. Answer it with `--dev-hint`, never the durable `--hint`:

```sh
onwardpg plan \
  --dev-hint '{"kind":"rename","object":"column","from":["app","quotes","quote_mode"],"to":["app","quotes","pricing_mode"]}'
```

The scopes cannot leak into each other. A local shortcut does not authorize production SQL.

When a checkout switch removes the active bundle, onwardpg parks its local `PlanID`. Naming the other branch’s plan selects it; returning later restores the original identity. If the current bundle is still present, onwardpg blocks rather than inventing a second mutable plan in the same checkout.

## What if `main` moves while I am working?

Rebase with Git, then run the same command:

```sh
git rebase origin/main
onwardpg plan
```

onwardpg does not use branch names or commit SHAs as schema proof. It validates the remaining accepted hash chain, replays its new head as H, and rebuilds the existing `PlanID` as **H(new) → W**.

At the same time it recomputes D → W. If the rebase added a safe upstream
change that the branch-worn dev database lacks, the response contains the
small fast-forward SQL immediately. If parallel-branch state is merely surplus,
it is preserved. If it conflicts with W, onwardpg asks or blocks instead of
silently overwriting or importing it.

The plan already knows the intent you captured:

- decisions whose participating object scope is unchanged are carried forward;
- decisions whose meaning changed are rejected as stale;
- edits inside stable SQL pockets are transplanted;
- generator-owned conflicts stop with the old, new, and edited versions for review; and
- generated work already absorbed by incoming history disappears from the active bundle.

Developer-owned data work is never discarded merely because a catalog diff became empty. That needs explicit reconciliation.

This is the third “a-ha”: a rebase does not mean throwing away a carefully reviewed migration and starting the diff again. It means hill-climbing the same migration on firmer ground.

## Where do decisions enter?

PostgreSQL can prove object shape and dependency. It cannot prove that two names mean the same product field, that data loss is intended, or how a string becomes an integer.

`plan` exits `2` with the smallest semantic question it needs. Supply the answer and rerun:

```sh
onwardpg plan \
  --hint '{"kind":"rename","object":"column","from":["app","accounts","display_name"],"to":["app","accounts","full_name"]}'
```

Consumed hints are written automatically to `decisions.json` with their fingerprint-bound evidence. You do not copy internal question IDs or fingerprints, and you do not need to resubmit accepted hints. An agent that already understands the feature may provide several hints on the first invocation. A valid hint hidden behind an earlier dependency-ordered question is returned as `deferred` and re-emitted in `next_actions` without being receipted; irrelevant or impossible intent is rejected.

When product semantics require SQL, the answer selects `manual_sql`. Small
one-shot work can go into a stable phase edit pocket. Agents can instead attach
typed work directly to the hint; large repeatable work becomes an operation
artifact. This keeps intent, executable work, completion proof, and verification
evidence distinct and reviewable.

Durable H → W planning always runs. If the development database is unavailable,
`plan` preserves and reports that durable result with development marked
`not_available`; it does not make developers select a mode. Supplying a
`--dev-hint` is an intentional request to reconcile D → W and therefore
requires the development database.

## One plan, one merge, one deployment

A team workflow becomes straightforward:

1. Export W from the models in the feature checkout.
2. Run `plan NAME` once, then repeat it as the feature evolves.
3. Capture semantic decisions and edit product-owned SQL in the bundle.
4. Rebase and rerun `plan`; review only what the new base invalidated.
5. Run `verify --check` in CI.
6. Merge the application change and its one bundle together.
7. The release system applies expand, deploys the application, supplies
   expiring evidence for every potential writer, requires `contract check` to
   pass, then applies the assertion-bearing contract.
8. The merged bundle becomes H for the next feature.

The new application must work on both sides of contract. If it cannot, keep the compatibility shape and remove it in a later feature plan. One plan is tied to one merge and one application deployment—not to every keystroke that happened before them.

## Scenario map

| What the developer encounters | What `plan` does |
| --- | --- |
| Models edited repeatedly before merge | Replaces the same bundle generation. |
| Accepted migrations arrive during the feature | Restacks the same `PlanID` on the new H. |
| A valid rename/drop/backfill decision still applies | Carries its scoped evidence forward. |
| The participating schema meaning changed | Invalidates the stale decision and asks again. |
| A draft-only name was already applied to D | Keeps H → W clean and offers a separate D → W reconciliation. |
| Branch switching leaves extra objects in D | Preserves them in workspace mode. |
| Incoming history absorbs generated feature work | Removes the redundant generated bundle. |
| Incoming history collides with edited phase SQL | Stops with an explicit three-way handoff. |
| Production differs from accepted history | Reports drift separately; never smuggles a repair into the feature plan. |

The output is still SQL. The product is the continuously reconciled understanding that produced it.
