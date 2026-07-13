// Package adapter defines the stable boundary for declarative schema tools.
//
// An adapter may return PostgreSQL CREATE-statement DDL, which onwardpg
// materializes in a disposable database, or a fully typed schema snapshot.
// Adapters never return migration SQL: onwardpg remains responsible for
// inspection, diffing, questions, hazards, and planning.
package adapter

import (
	"context"
	"errors"

	"github.com/jokull/onwardpg/pgschema"
)

type Source interface {
	Load(context.Context) (Artifact, error)
}

type SourceFunc func(context.Context) (Artifact, error)

func (f SourceFunc) Load(ctx context.Context) (Artifact, error) { return f(ctx) }

type Artifact struct {
	// DDL contains declarative CREATE statements. It is never parsed by the
	// adapter API; the engine executes it in disposable PostgreSQL.
	DDL []byte
	// Snapshot is a complete typed graph produced by an adapter that can model
	// PostgreSQL semantics without going through DDL.
	Snapshot *pgschema.Snapshot
	// Provenance is a stable, non-secret description used in diagnostics and
	// reproducible-plan metadata (for example "drizzle:packages/db/schema.ts").
	Provenance string
	// ObjectOrder is an optional complete order for a typed Snapshot. It is
	// intended solely for an explicit unsorted schema dump: callers must never
	// treat such output as a dependency-ordered executable migration. The order
	// must be a permutation of every object in Snapshot; ValidateDumpOrder
	// enforces that invariant before a planner accepts it.
	//
	// DDL artifacts cannot carry this field. SQL files are materialized and
	// catalog inspection deliberately does not preserve statement order.
	ObjectOrder []pgschema.ID
}

func (a Artifact) Validate() error {
	hasDDL, hasSnapshot := len(a.DDL) > 0, a.Snapshot != nil
	if hasDDL == hasSnapshot {
		return errors.New("adapter artifact must contain exactly one of DDL or Snapshot")
	}
	if a.Provenance == "" {
		return errors.New("adapter artifact provenance is required")
	}
	if len(a.ObjectOrder) > 0 && a.Snapshot == nil {
		return errors.New("adapter object order requires a typed snapshot")
	}
	return nil
}

// ValidateDumpOrder validates the explicit object order required by an
// unsorted dump. It is intentionally separate from Validate: most adapters
// do not support dump ordering and remain valid schema sources.
func (a Artifact) ValidateDumpOrder() error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Snapshot == nil {
		return errors.New("unsorted dump requires a typed snapshot")
	}
	if len(a.ObjectOrder) == 0 {
		return errors.New("unsorted dump requires an explicit complete object order")
	}
	return pgschema.ValidateObjectOrder(a.Snapshot, a.ObjectOrder)
}

func DDL(provenance string, ddl []byte) Artifact {
	return Artifact{DDL: append([]byte(nil), ddl...), Provenance: provenance}
}

func Snapshot(provenance string, snapshot *pgschema.Snapshot) Artifact {
	return Artifact{Snapshot: snapshot, Provenance: provenance}
}

// OrderedSnapshot constructs a typed artifact whose explicit order can be
// used for a review-only unsorted schema dump after ValidateDumpOrder passes.
func OrderedSnapshot(provenance string, snapshot *pgschema.Snapshot, order []pgschema.ID) Artifact {
	return Artifact{Snapshot: snapshot, Provenance: provenance, ObjectOrder: append([]pgschema.ID(nil), order...)}
}
