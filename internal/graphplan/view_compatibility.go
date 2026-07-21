package graphplan

import (
	"github.com/jokull/onwardpg/pgschema"
)

type viewReplacementLegality string

const (
	viewReplacementUnknown viewReplacementLegality = "unknown"
	viewReplacementLegal   viewReplacementLegality = "legal"
	viewReplacementIllegal viewReplacementLegality = "illegal"
)

// classifyViewReplacement applies PostgreSQL's CREATE OR REPLACE VIEW output
// contract to typed catalog signatures. Existing columns must retain their
// name, order, and type identity; only new columns appended to the target list
// are legal. Unknown is deliberately separate from legal so callers never use
// missing catalog evidence as permission to rewrite a live view.
func classifyViewReplacement(before, after pgschema.View) (viewReplacementLegality, string) {
	if before.Materialized || after.Materialized {
		return viewReplacementIllegal, "materialized_view"
	}
	if before.ObjectID() != after.ObjectID() {
		return viewReplacementIllegal, "identity_changed"
	}
	if len(before.OutputColumns) == 0 || len(after.OutputColumns) == 0 {
		return viewReplacementUnknown, "output_signature_unavailable"
	}
	if len(after.OutputColumns) < len(before.OutputColumns) {
		return viewReplacementIllegal, "output_removed"
	}
	for index, existing := range before.OutputColumns {
		replacement := after.OutputColumns[index]
		if existing.Name != replacement.Name {
			return viewReplacementIllegal, "output_name_changed"
		}
		if !sameViewColumnType(existing, replacement) {
			return viewReplacementIllegal, "output_type_changed"
		}
	}
	return viewReplacementLegal, ""
}

func sameViewColumnType(before, after pgschema.ViewColumn) bool {
	return before.Type == after.Type &&
		before.TypeMod == after.TypeMod &&
		before.TypeSchema == after.TypeSchema &&
		before.TypeName == after.TypeName &&
		before.TypeKind == after.TypeKind &&
		before.Collation == after.Collation
}
