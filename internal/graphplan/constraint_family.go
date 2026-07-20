package graphplan

import (
	"sort"

	"github.com/jokull/onwardpg/internal/change"
	"github.com/jokull/onwardpg/pgschema"
)

// correlateCrossNameCheckChanges groups an unambiguous CHECK drop/create pair
// into one acceptance transition. This is not rename inference: both durable
// identities remain exactly as declared. It only prevents independent object
// renderers from putting the relaxation and restoration in incompatible
// rollout phases.
//
// The proof is deliberately narrow. Both constraints must be on the same table
// and share at least one catalog-proven constrained column, and the match must
// be one-to-one in both directions. Ambiguous families stay as ordinary
// drop/create changes and therefore still require explicit decisions.
func correlateCrossNameCheckChanges(changes []change.Change, current, desired *pgschema.Snapshot) []change.Change {
	var drops, creates []change.Change
	for _, item := range changes {
		switch {
		case item.Kind == change.Drop:
			constraint, ok := item.Before.(pgschema.Constraint)
			if ok && constraint.Type == pgschema.ConstraintCheck && constraint.Parent == nil {
				drops = append(drops, item)
			}
		case item.Kind == change.Create:
			constraint, ok := item.After.(pgschema.Constraint)
			if ok && constraint.Type == pgschema.ConstraintCheck && constraint.Parent == nil {
				creates = append(creates, item)
			}
		}
	}
	if len(drops) == 0 || len(creates) == 0 {
		return changes
	}

	dropCandidates := make(map[pgschema.ID][]pgschema.ID)
	createCandidates := make(map[pgschema.ID][]pgschema.ID)
	for _, drop := range drops {
		before := drop.Before.(pgschema.Constraint)
		beforeColumns := constraintColumnSet(current, drop.ID)
		if len(beforeColumns) == 0 {
			continue
		}
		for _, create := range creates {
			after := create.After.(pgschema.Constraint)
			if before.Table != after.Table || before.Name == after.Name || !setsOverlap(beforeColumns, constraintColumnSet(desired, create.ID)) {
				continue
			}
			dropCandidates[drop.ID] = append(dropCandidates[drop.ID], create.ID)
			createCandidates[create.ID] = append(createCandidates[create.ID], drop.ID)
		}
	}

	byID := make(map[pgschema.ID]change.Change, len(changes))
	for _, item := range changes {
		byID[item.ID] = item
	}
	consumed := make(map[pgschema.ID]bool)
	var correlated []change.Change
	for _, drop := range drops {
		candidates := dropCandidates[drop.ID]
		if len(candidates) != 1 || len(createCandidates[candidates[0]]) != 1 {
			continue
		}
		create := byID[candidates[0]]
		consumed[drop.ID], consumed[create.ID] = true, true
		correlated = append(correlated, change.Change{
			Kind: change.Modify, ID: drop.ID, Before: drop.Before, After: create.After,
		})
	}
	if len(correlated) == 0 {
		return changes
	}
	result := make([]change.Change, 0, len(changes)-len(consumed)+len(correlated))
	for _, item := range changes {
		if !consumed[item.ID] {
			result = append(result, item)
		}
	}
	result = append(result, correlated...)
	sort.Slice(result, func(left, right int) bool {
		if result[left].ID.String() == result[right].ID.String() {
			return result[left].Kind < result[right].Kind
		}
		return result[left].ID.String() < result[right].ID.String()
	})
	return result
}

func constraintColumnSet(snapshot *pgschema.Snapshot, id pgschema.ID) map[pgschema.ID]bool {
	result := make(map[pgschema.ID]bool)
	if snapshot == nil {
		return result
	}
	for _, dependency := range snapshot.Dependencies(id) {
		if dependency.Kind == pgschema.KindColumn {
			result[dependency] = true
		}
	}
	return result
}

func setsOverlap(left, right map[pgschema.ID]bool) bool {
	for id := range left {
		if right[id] {
			return true
		}
	}
	return false
}
