# Decisions, edits, rebases, and branch switches

## Semantic hints

PostgreSQL catalogs cannot prove whether differently named objects share identity, whether data loss is intended, or which product conversion is correct. onwardpg exits `2` with the smallest typed decision it needs.

Hints may be supplied proactively, but they must describe semantic intent only. They contain no SQL, question IDs, fingerprints, or transaction rules. Consumed hints are captured in `decisions.json` with internal scoped evidence; the agent does not copy or resubmit those receipts.

Use `--hint` or `--hints-file` for durable H -> W intent. Use `--dev-hint` only for an independent D -> W ambiguity. Contradictory, stale, invalid, impossible, and unused hints fail.

## Product SQL handoff

Some catalog transitions require application-aware data work. onwardpg writes a blocking `ONWARDPG TODO` into the relevant phase and reports the exact files to edit.

- Initial synchronization that must be safe while old code is live belongs in expand.
- Final catch-up and enforcement after old writers drain belongs in contract.
- Read-only Boolean assertions belong in `verify.sql`.
- Never put test fixtures in `expand.sql` or `contract.sql`; they are deployment artifacts.

Preserve generated markers and edit boundaries. After edits, `onwardpg verify` executes the exact artifact. A later `plan` may transplant an owned pocket when its scope remains valid; generator-owned conflicts stop with a three-way handoff.

## Rebase and restack

Git is responsible for rebasing. After accepted history changes, rerun `onwardpg plan` for the same active PlanID. onwardpg validates the remaining content-addressed chain, replays its current head, and rebuilds H(new) -> W.

Still-valid scoped decisions survive. Decisions whose participating schema meaning changed become stale. Generated work already absorbed upstream disappears. Developer-owned SQL is never silently discarded or declared safe.

## Branch switching

An active plan is worktree-local. If its bundle disappears during a normal checkout, onwardpg can park the PlanID. Naming a plan whose bundle is present can restore it when returning to that branch.

Do not delete local anchors merely to bypass an active-plan error. Do not create a second mutable plan while the current bundle remains present. Report which plan and checkout conflict and let the developer choose the intended feature.
