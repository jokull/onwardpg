// Package change calculates typed, semantic changes between schema graphs.
package change

import (
	"reflect"
	"sort"

	"github.com/jokull/onwardpg/pgschema"
)

type Kind string

const (
	Create Kind = "create"
	Drop   Kind = "drop"
	Modify Kind = "modify"
)

// Change deliberately retains typed before/after objects. SQL rendering and
// hazard policy can therefore make decisions from PostgreSQL semantics instead
// of reparsing strings emitted by a previous stage.
type Change struct {
	Kind   Kind
	ID     pgschema.ID
	Before pgschema.Object
	After  pgschema.Object
}

// Batch is an execution-order group. Creation and modification follow desired
// dependencies; drops use the reverse of current dependencies. Cyclic batches
// are preserved for the PostgreSQL planner to split with an explicit strategy
// (for example, create foreign keys after their tables).
type Batch struct {
	Changes []Change
	Cyclic  bool
}

// Between returns a deterministic, name-based semantic diff. Rename detection
// is intentionally not part of this function: a rename is ambiguous schema
// intent and is resolved by the answer protocol above this layer.
func Between(current, desired *pgschema.Snapshot) []Change {
	var changes []Change
	seen := make(map[pgschema.ID]bool)
	for _, id := range current.IDs() {
		seen[id] = true
		before, _ := current.Object(id)
		after, exists := desired.Object(id)
		switch {
		case !exists:
			changes = append(changes, Change{Kind: Drop, ID: id, Before: before})
		case !objectsEqual(before, after):
			changes = append(changes, Change{Kind: Modify, ID: id, Before: before, After: after})
		}
	}
	for _, id := range desired.IDs() {
		if seen[id] {
			continue
		}
		after, _ := desired.Object(id)
		changes = append(changes, Change{Kind: Create, ID: id, After: after})
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].ID.String() == changes[j].ID.String() {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].ID.String() < changes[j].ID.String()
	})
	return changes
}

func objectsEqual(before, after pgschema.Object) bool {
	beforeIndex, beforeIsIndex := before.(pgschema.Index)
	afterIndex, afterIsIndex := after.(pgschema.Index)
	if beforeIsIndex && afterIsIndex {
		beforeIndex.Definition, afterIndex.Definition = "", ""
		return reflect.DeepEqual(beforeIndex, afterIndex)
	}
	return reflect.DeepEqual(before, after)
}

func Schedule(current, desired *pgschema.Snapshot, changes []Change) []Batch {
	createsAndModifies := make(map[pgschema.ID]Change)
	currentIdentityModifies := make(map[pgschema.ID]Change)
	drops := make(map[pgschema.ID]Change)
	for _, item := range changes {
		switch item.Kind {
		case Drop:
			drops[item.ID] = item
		default:
			// A confirmed parent rename can turn a drop/create child pair into
			// a modification against the current identity. Schedule that work
			// from the current dependency graph so it executes before the parent
			// receives its desired identity.
			if item.Kind == Modify {
				_, inCurrent := current.Object(item.ID)
				_, inDesired := desired.Object(item.ID)
				if inCurrent && !inDesired {
					currentIdentityModifies[item.ID] = item
					continue
				}
			}
			createsAndModifies[item.ID] = item
		}
	}
	var batches []Batch
	appendBatches := func(snapshot *pgschema.Snapshot, selected map[pgschema.ID]Change, reverse bool) {
		ordered := snapshot.Batches()
		if reverse {
			for left, right := 0, len(ordered)-1; left < right; left, right = left+1, right-1 {
				ordered[left], ordered[right] = ordered[right], ordered[left]
			}
		}
		for _, batch := range ordered {
			changes := make([]Change, 0, len(batch.Objects))
			for _, object := range batch.Objects {
				if change, exists := selected[object.ObjectID()]; exists {
					changes = append(changes, change)
				}
			}
			if len(changes) > 0 {
				batches = append(batches, Batch{Changes: changes, Cyclic: batch.Cyclic})
			}
		}
	}
	appendBatches(desired, createsAndModifies, false)
	appendBatches(current, currentIdentityModifies, false)
	appendBatches(current, drops, true)
	return batches
}
