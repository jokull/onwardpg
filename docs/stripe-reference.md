# Stripe pg-schema-diff reference boundary

onwardpg pins Stripe `pg-schema-diff` **v1.0.7** at commit
`6208f8f3ceccae8ca634055dc47907a6a864cb76` as an executable, test-only
reference. Stripe's project is MIT licensed; the full notice is preserved in
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md), and the immutable pin is
recorded in
[`references/stripe-pg-schema-diff-v1.0.7.json`](../references/stripe-pg-schema-diff-v1.0.7.json).

It is deliberately absent from `go.mod`, onwardpg production binaries, and the
runtime planning path. CI builds the exact commit, runs selected current→desired
DDL through both planners on disposable PostgreSQL, applies both plans only to
disposable databases, and compares catalog convergence and residual diffs.

## Acceptance-corpus receipt

[`parity/stripe-pg-schema-diff-v1.0.7.json`](../parity/stripe-pg-schema-diff-v1.0.7.json)
indexes all 415 acceptance scenarios from the 24 `*_cases_test.go` files in the
pinned release. Every source file carries a SHA-256 receipt; CI regenerates the
index and rejects drift. Each scenario records five separate comparisons:
catalog coverage, mutation support, online strategy, rejection behavior, and
workflow semantics.

`classified_unverified` is intentionally weaker than parity. It means no
Stripe acceptance case is absent or unclassified, but most family-level
classifications still need scenario-specific onwardpg and differential tests.
The matrix can become `verified` only when every entry links both kinds of
evidence.

## Adopted

- Acceptance testing by applying the generated plan to disposable PostgreSQL,
  comparing the resulting schema, and proving the second diff is empty.
- Continuous same-name index replacement: rename the usable old index, build
  the desired index concurrently under the stable name, and remove the old
  temporary index concurrently.
- Per-statement statement/lock timeout guidance and explicit index-build and
  cleanup hazards.
- Studying operation ordering as a graph problem rather than accumulating
  object-family SQL in incidental order.

## Adapted

- Stripe's flat ordered index replacement is split into onwardpg `EXPAND` and
  `CONTRACT` phases. Cleanup can therefore be reviewed or delayed without
  losing the desired named index.
- Stripe's random temporary index identifiers are replaced with deterministic
  hashes of the complete typed old/new index pair. A collision with any
  PostgreSQL relation name is an explicit unsupported result.
- Timeout values are plan metadata and rendered review comments. onwardpg does
  not set them on a caller session because it has no apply command.
- Differential equality is semantic catalog convergence, not byte-for-byte SQL
  equality. Intent and safety differences remain visible rather than being
  normalized away.
- Stripe's RLS/policy/privilege ordering and role escaping are represented as
  typed onwardpg nodes and dependency edges. Authorization relaxations,
  policy replacement, and grant-option removal additionally require
  fingerprint-bound answers; emitted statements carry timeout guidance but
  are never executed against the caller database.

## Rejected

- **Runtime delegation.** Stripe is an oracle in tests, never a production
  dependency or source of planning truth.
- **Names equal identity.** A credible onwardpg rename remains a
  fingerprint-bound question; it is not silently converted into Stripe's
  create/drop interpretation.
- **Hazards as authorization.** A warning does not approve a drop, cast,
  backfill, refresh, or other absent intent.
- **A flat plan as the public model.** onwardpg retains typed graph nodes and
  edges, questions, phases, transactional batches, bundle receipts, and PR
  restacking.
- **Application behavior.** The Stripe CLI's apply command is outside
  onwardpg's permanent no-apply boundary.
- **Non-snapshot catalog inspection.** onwardpg keeps all catalog reads inside
  one read-only repeatable-read transaction even when a reference implementation
  chooses another tradeoff.

## Current observed result

The continuous ordinary/materialized-view/local-index replacement primitive is
implemented for standalone indexes where PostgreSQL permits concurrent create
and drop. Standalone partitioned parents, including nested trees, retain the
old hierarchy while recursive `ON ONLY` shells and concurrently built leaves
are attached bottom-up. The pinned "Attach an unnattached, invalid index"
case also converges by attaching a structurally matching prebuilt child without
rebuild or drop. The two "to partition using existing index" cases converge by
claiming the local index with a new primary/unique constraint before attachment.
Detach, reparent, and more complex existing local-constraint transitions remain
explicit work except for ordinary primary/unique constraints without
external dependents, which use a concurrently built replacement plus a short
transactional swap. The pinned differential tests
compare ordering, hazards, timeouts, application, idempotence, and empty
residual diff. They also prove policy-expression changes, RLS force
relaxation, table-privilege revocation, grant-option removal, partition-parent
replacement, and primary-constraint swaps converge in both planners. Both
planners converge for Stripe's exact primary-key/type-change acceptance DDL;
Stripe selects a direct cast while onwardpg requires the cast as a
fingerprint-bound answer. onwardpg's extra intent questions are an intentional
workflow difference, not normalized away by the harness. The catalog-family
inventory and the new partition-attachment convergence cases have been
observed on PostgreSQL 14–18. CI remains the authoritative cross-version proof
for every commit; most of the 415-case Stripe corpus is still conservatively
classified `weaker`, not claimed as differential parity.
