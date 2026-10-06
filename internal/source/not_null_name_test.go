package source

import (
	"strings"
	"testing"
)

// The expected names below were produced by PostgreSQL 18.4 itself
// (CREATE TABLE ... NOT NULL, then pg_constraint.conname), not by this package.
func TestGeneratedNotNullConstraintNames(t *testing.T) {
	rep := strings.Repeat
	tests := []struct {
		name       string
		relation   string
		column     string
		constraint string
		want       bool
	}{
		{"plain", "accounts", "id", "accounts_id_not_null", true},
		{"truncated column part", "t_with_a_short_name", rep("a", 63),
			"t_with_a_short_name_" + rep("a", 34) + "_not_null", true},
		{"truncated table part", rep("t", 62), "c",
			rep("t", 52) + "_c_not_null", true},
		{"both parts truncated", rep("t", 35), rep("c", 36),
			rep("t", 27) + "_" + rep("c", 26) + "_not_null", true},
		{"multibyte parts clipped on character boundaries", rep("é", 31), rep("å", 31),
			rep("é", 13) + "_" + rep("å", 13) + "_not_null", true},
		{"multibyte three-byte characters", rep("日本語", 7), rep("列", 21),
			"日本語日本語日本語_" + rep("列", 8) + "_not_null", true},
		{"multibyte column clipped to 62 bytes", "a", rep("日本語", 7),
			"a_" + rep("日本語", 5) + "日本_not_null", true},
		{"first collision counter", rep("t", 62) + "2", "c", rep("t", 51) + "_c_not_null1", true},
		{"second collision counter", rep("t", 59) + "3", "c", rep("t", 51) + "_c_not_null2", true},
		{"double digit collision counter", "accounts", "id", "accounts_id_not_null12", true},
		{"short collision counter", "accounts", "id", "accounts_id_not_null1", true},

		{"custom name", "objects", "id", "id_required", false},
		{"custom name that only ends like a default", "objects", "id", "my_not_null", false},
		{"default of a different column", "accounts", "id", "accounts_name_not_null", false},
		{"default of a different table", "accounts", "id", "users_id_not_null", false},
		{"default left behind by a column rename", "accounts", "full_name", "accounts_display_name_not_null", false},
		{"default left behind by a table rename", "users", "id", "accounts_id_not_null", false},
		{"truncated name of a different table", rep("t", 62), "c", rep("t", 51) + "_c_not_null", false},
		{"column part not truncated", rep("t", 62), "c", rep("t", 62) + "_c_not_null", false},
		{"clipped in the middle of a character", rep("é", 31), rep("å", 31),
			rep("é", 13) + "\xc3_" + rep("å", 13) + "_not_null", false},
		{"counter with leading zero", "accounts", "id", "accounts_id_not_null01", false},
		{"zero counter", "accounts", "id", "accounts_id_not_null0", false},
		{"counter without the label", "accounts", "id", "accounts_id1", false},
		{"digits belong to the column, not the counter", "accounts", "id", "accounts_id1_not_null", false},
		{"suffix after the counter", "accounts", "id", "accounts_id_not_null1x", false},
		{"upper case label", "accounts", "id", "accounts_id_NOT_NULL", false},
		{"empty name", "accounts", "id", "", false},
		{"counter too long for any name", "accounts", "id", "accounts_id_not_null" + rep("9", 60), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isGeneratedNotNullConstraintName(test.relation, test.column, test.constraint); got != test.want {
				t.Fatalf("isGeneratedNotNullConstraintName(%q, %q, %q) = %v, want %v", test.relation, test.column, test.constraint, got, test.want)
			}
		})
	}
}

func TestGeneratedNotNullConstraintNamesStayWithinNameDataLen(t *testing.T) {
	for _, relation := range []string{"a", strings.Repeat("r", 63), strings.Repeat("é", 31)} {
		for _, column := range []string{"c", strings.Repeat("c", 63), strings.Repeat("日", 21)} {
			for _, label := range []string{"not_null", "not_null1", "not_null99"} {
				name, ok := postgresMakeObjectName(relation, column, label)
				if !ok || len(name) > postgresNameDataLen-1 {
					t.Fatalf("makeObjectName(%q, %q, %q) = %q (%d bytes), ok=%v", relation, column, label, name, len(name), ok)
				}
			}
		}
	}
}
