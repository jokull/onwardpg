package pgschema

import "testing"

func TestCanonicalExpressionMatchesPostgreSQLFlatDeparse(t *testing.T) {
	// Each want is what PostgreSQL 18 deparses after it parses got's text
	// again, as a dump and restore does.
	cases := []struct{ name, got, want string }{
		{"between then and", "CHECK ((((x >= 1) AND (x <= 5)) AND (y > 0)))", "CHECK (((x >= 1) AND (x <= 5) AND (y > 0)))"},
		{"right nested", "CHECK (((a > 0) AND ((b > 0) AND (c > 0))))", "CHECK (((a > 0) AND (b > 0) AND (c > 0)))"},
		{"nested or", "CHECK ((((a > 0) OR (b > 0)) OR (c > 0)))", "CHECK (((a > 0) OR (b > 0) OR (c > 0)))"},
		{"already flat", "CHECK (((a > 0) AND (b > 0) AND (c > 0)))", "CHECK (((a > 0) AND (b > 0) AND (c > 0)))"},
		{
			"or of and keeps its groups",
			"CHECK ((((a > 0) OR (b > 0)) AND (((c >= 1) AND (c <= 2)) OR (d = ANY (ARRAY['x'::text, 'y'::text])))))",
			"CHECK ((((a > 0) OR (b > 0)) AND (((c >= 1) AND (c <= 2)) OR (d = ANY (ARRAY['x'::text, 'y'::text])))))",
		},
		{"literal holds a keyword", "CHECK ((((d <> 'a AND (b'::text) AND (a > 0)) AND (\"OR\" > 0)))", "CHECK (((d <> 'a AND (b'::text) AND (a > 0) AND (\"OR\" > 0)))"},
		{"not is an operand", "CHECK (((NOT ((a > 0) AND (b > 0))) AND (c > 0)))", "CHECK (((NOT ((a > 0) AND (b > 0))) AND (c > 0)))"},
		{"inside a function argument", "CHECK (f((((a > 0) AND (b > 0)) AND (c > 0)), 1))", "CHECK (f(((a > 0) AND (b > 0) AND (c > 0)), 1))"},
		{"suffix", "CHECK ((((a > 0) AND (b > 0)) AND (c > 0))) NOT VALID", "CHECK (((a > 0) AND (b > 0) AND (c > 0))) NOT VALID"},
		{"quote escape", "CHECK ((((d <> 'it''s ('::text) AND (a > 0)) AND (b > 0)))", "CHECK (((d <> 'it''s ('::text) AND (a > 0) AND (b > 0)))"},
		{
			"case keeps the newline PostgreSQL prints",
			"CHECK ((((x >= 1) AND (x <= 2)) AND\nCASE\n    WHEN (x > 0) THEN true\n    ELSE false\nEND))",
			"CHECK (((x >= 1) AND (x <= 2) AND\nCASE\n    WHEN (x > 0) THEN true\n    ELSE false\nEND))",
		},
		{"index predicate", "(((x >= 1) AND (x <= 5)) AND (y > 0))", "((x >= 1) AND (x <= 5) AND (y > 0))"},
		{"index expression", "((((x >= 1) AND (x <= 5)) AND (y > 0)))", "(((x >= 1) AND (x <= 5) AND (y > 0)))"},
		{"unbalanced is left alone", "CHECK ((a > 0) AND (b > 0)", "CHECK ((a > 0) AND (b > 0)"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := CanonicalExpression(test.got); got != test.want {
				t.Fatalf("CanonicalExpression(%q)\n got %q\nwant %q", test.got, got, test.want)
			}
			if again := CanonicalExpression(test.want); again != test.want {
				t.Fatalf("canonical form is not a fixed point: %q", again)
			}
		})
	}
}
