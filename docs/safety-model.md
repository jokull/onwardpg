# Safety model

onwardpg is a planner, not a deployment agent. It never executes a plan on a
caller-supplied target. Clone verification is the only execution surface, and
it creates and destroys its own randomly named disposable databases.

The planner's core safety rules are:

- inspect live catalog state in a single `REPEATABLE READ, READ ONLY`
  transaction;
- materialize DDL in a disposable PostgreSQL database rather than parse a
  subset of SQL;
- block catalog state in the preview's explicit unsupported-family inventory
  unless a validated narrow ignore selector accounts for it;
- bind ambiguity answers to both source and desired graph fingerprints;
- reject stale, invalid, contradictory, duplicate, and unused answers;
- require an explicit, fingerprint-bound manual-work contract—including its
  transactional/non-transactional execution mode—whenever schema state cannot
  prove the required data work;
- emit no plan when status is `needs_input` or `unsupported`;
- preserve execution constraints through explicit transactional batches; and
- surface destructive, lock, rewrite, validation, and availability concerns as
  statement safety/hazard metadata for review.

Every PostgreSQL 14–18 `pg_catalog` table is classified in the developer
preview inventory. The attribute-level audit remains in progress. The current
blockers include domains, composites, aggregates, standalone collations,
range/multirange types, foreign tables, explicit ownership deviations,
non-table and column ACL/default-privilege state, non-owner grant chains,
replica identity, clustered or invalid indexes, relation and column physical
options, explicit relation tablespaces, traditional inheritance, rules,
text-search objects, event triggers, publications and subscriptions, extended
statistics, FDWs/servers/user mappings, custom access methods/operators/casts/
conversions/languages/transforms, security labels, and comments whose typed
object does not yet retain them. PostgreSQL 18's canonical generated `NOT
NULL` identities normalize to the existing column flag; custom/noncanonical
or commented `NOT NULL`, unenforced and period constraints, and virtual
generated columns are version-gated blockers.
Subscription connection strings and security-label values are never included
in diagnostics.
Extension-owned members are represented atomically by the typed extension
name/version/schema boundary and are not independently planned. The
machine-readable [catalog-family inventory](../parity/postgres-catalog-families.json)
records the per-major catalog-table evidence separately from its still-open
attribute audit. “No unsupported result” is not a catalog-completeness
certification until that second milestone closes.

RLS enable/force state, policies, and table privileges are modeled rather than
ignored. Graph edges place policies before RLS enable and RLS disable before
policy removal. Policy replacement, policy alteration, RLS relaxation,
privilege revocation, and removal of grant options remain reviewable and, when
destructive or authorization-relaxing, fingerprint-bound. Every emitted
authorization statement carries lock/statement timeout guidance; onwardpg
does not set those values on a caller session.

Manual-work SQL is operator-owned and is never invented from catalog state.
Only a question that explicitly requests manual work may carry that payload;
ordinary answer types reject it. Summaries are one-line metadata. Verification
queries are one-line postconditions that must each return one boolean `true`
row during clone verification. The reviewed statement list is placed in its
own `MANUAL` batch with the declared execution boundary; a failed transactional
postcondition rolls that batch back.

Transactional batches are intended to be atomic execution boundaries. The real
PostgreSQL integration suite includes a failure case that proves an earlier
statement is rolled back when a later statement in the same batch fails.

An ignore selector is acceptance of a blind spot, not a declaration that the
ignored object is equivalent. It is validated against both compared snapshots
and returned in the result's `ignored` field.

The planner cannot prove data validity, safe casts, backfills, lock duration,
or application compatibility. Reviewers own those operational decisions. Test
the generated plan on a clone and require an empty residual diff after it is
applied.
