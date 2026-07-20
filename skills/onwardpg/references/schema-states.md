# Schema states and comparisons

Keep these states distinct whenever reading onwardpg output:

| Symbol | State | Authority | Relationship |
| --- | --- | --- | --- |
| H | Accepted onwardpg history replayed in disposable PostgreSQL | Durable migration base | `H -> W` produces the feature bundle |
| W | Declarative DDL exported from the working tree and ingested by PostgreSQL | Intended destination | Shared destination for durable and local comparisons |
| D | Long-lived development database inspected read-only | Local convenience | `D -> W` produces optional development SQL |
| E | Verified selected bundle replayed through expand | Receipted artifact evidence | `P -> E` checks the live post-expand catalog before contract |
| P | Production database or replica | Explicit read-only evidence | Drift against H, contract readiness against E, or bounded data shapes |
| V | Independent disposable verification replays | Artifact evidence | Proves exact bundle bytes converge to W |

## H -> W is durable

The reviewed migration bundle always starts from the accepted content-addressed history and reaches the materialized working schema. An unaccepted draft shape is not history. If a feature changes again before merge, onwardpg recalculates the same PlanID rather than preserving every intermediate draft.

When another migration lands, Git moves the feature branch and `onwardpg plan` replays the new accepted head. Still-valid decisions and owned SQL pockets can move forward; changed meaning must stop for a new decision.

## D -> W is local

The development database may contain an earlier draft, another branch's objects, manual experiments, or an accepted migration whose application data work has not run locally. Its diff must never contaminate H -> W.

Workspace mode preserves surplus D-only objects. Strict cleanup is an explicit local choice. `--dev-hint` answers belong only to D -> W and are never durable migration intent.

## P is outside ordinary planning

Production is not an implicit plan input. Use `onwardpg drift check` for an
explicit read-only comparison of P with accepted history H. If a coding agent has
separate restricted read-only access, it may gather bounded aggregates or
classifications as product evidence. After expand, `onwardpg contract check`
may inspect P read-only against the bundle's receipted expand checkpoint E and
evaluate contract gates; `plan` and `verify` still do not query production.

## V proves the artifact, not the environment

Verification replays exact accepted history and exact edited bundle bytes in onwardpg-created disposable databases, runs read-only assertions, and requires an empty final residual catalog diff. It cannot prove live row distribution, workload behavior, lock duration, replica lag, application compatibility, or traffic drain.
