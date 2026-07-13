# Adapter API

The public `adapter` package is the hand-off point for Django, Drizzle, Prisma,
SQLAlchemy, or a custom declarative schema tool. It deliberately accepts schema
state, never migration SQL.

```go
type Source interface {
	Load(context.Context) (Artifact, error)
}

type Artifact struct {
	DDL         []byte
	Snapshot    *pgschema.Snapshot
	Provenance  string
	ObjectOrder []pgschema.ID
}
```

An artifact must contain exactly one of:

- `DDL`: PostgreSQL `CREATE`-statement input. onwardpg materializes it in a
  disposable PostgreSQL database and uses the resulting catalog graph.
- `Snapshot`: a complete `pgschema.Snapshot` produced by an adapter that can
  faithfully model PostgreSQL semantics.

`Provenance` is required and must be stable and non-secret, such as
`drizzle:packages/db/schema.ts`. It belongs in diagnostics/reproducibility
metadata, not in credentials or a full database URL.

## Explicit unsorted schema dumps

An adapter that has a real declaration/dump order may return an
`OrderedSnapshot`. `ObjectOrder` must be a complete, duplicate-free
permutation of every object ID in its typed snapshot—schemas, tables, columns,
constraints, indexes, and so on. Validate it before passing the order to the
planner:

```go
artifact := adapter.OrderedSnapshot("drizzle:schema.ts", snapshot, order)
if err := artifact.ValidateDumpOrder(); err != nil {
	return err
}
```

Unsorted mode is restricted to an empty current snapshot and create-only
output. It is a reviewable schema dump, not an executable migration: every
statement is marked `safety: "manual"` with the
`unsorted_dump_order` hazard. The planner rejects missing, duplicate, stale,
or partial orders instead of falling back to an arbitrary sort. DDL artifacts
cannot request this mode because materialization/catalog inspection does not
preserve input statement order.

Adapters must not infer renames, casts, backfills, destructive approval, or
executable migration ordering. Those decisions remain planner questions and
batches. An adapter that cannot faithfully model an object must emit DDL for
catalog inspection or fail explicitly; it must not omit the object from a
typed snapshot.

The CLI currently accepts URLs and DDL files. Wiring arbitrary adapter sources
into the CLI is an integration task still tracked in `PLAN.md`; library users
can use the adapter boundary directly.
