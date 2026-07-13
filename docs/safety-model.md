# Safety model

onwardpg is a planner, not a deployment agent. It writes a plan and exits; it
does not connect to a target to execute the plan.

The planner's core safety rules are:

- inspect live catalog state in a single `REPEATABLE READ, READ ONLY`
  transaction;
- materialize DDL in a disposable PostgreSQL database rather than parse a
  subset of SQL;
- block unknown or unmodeled catalog state unless a validated narrow ignore
  selector explicitly accounts for it;
- bind ambiguity answers to both source and desired graph fingerprints;
- reject stale, invalid, contradictory, duplicate, and unused answers;
- require an explicit, fingerprint-bound manual-work contract—including its
  transactional/non-transactional execution mode—whenever schema state cannot
  prove the required data work;
- emit no plan when status is `needs_input` or `unsupported`;
- preserve execution constraints through explicit transactional batches; and
- surface destructive, lock, rewrite, validation, and availability concerns as
statement safety/hazard metadata for review.

Manual-work SQL is operator-owned and is never invented from catalog state.
Only a question that explicitly requests manual work may carry that payload;
ordinary answer types reject it. Summaries and verification queries are
one-line non-executable metadata, while the reviewed statement list is placed
in its own `MANUAL` batch with the declared execution boundary.

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
