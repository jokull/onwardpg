package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/change"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestBuildCrossNameCheckFamilyUsesLooseAcceptanceEnvelope(t *testing.T) {
	current, desired, _, _ := crossNameCheckGraphs(t,
		`CHECK (settlement_currency = 'jpy')`,
		`CHECK ((quote_mode = 'fx' AND settlement_currency = 'jpy') OR (quote_mode = 'fixed_market' AND settlement_currency = presentment_currency))`,
		[]string{"settlement_currency"},
		[]string{"quote_mode", "settlement_currency", "presentment_currency"},
	)
	plan := buildWithAssertOnlyReconciliations(t, current, desired, Options{})
	if plan.Status != protocol.Planned || len(plan.Statements) != 4 {
		t.Fatalf("cross-name CHECK was not correlated: %#v", plan)
	}
	if plan.Statements[0].Phase != protocol.PhaseExpand || !strings.Contains(plan.Statements[0].SQL, `DROP CONSTRAINT "checkout_quote_settlement_currency_jpy"`) {
		t.Fatalf("old cross-name enforcement was not relaxed in expand: %#v", plan.Statements)
	}
	for _, item := range plan.Statements[1:] {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("unknown desired enforcement was installed before contract: %#v", item)
		}
	}
	if !strings.Contains(joinSQL(plan), `ADD CONSTRAINT "checkout_quote_settlement_currency_by_mode"`) {
		t.Fatalf("desired cross-name constraint missing: %s", joinSQL(plan))
	}
}

func TestBuildCrossNameCheckWideningConvergesInExpand(t *testing.T) {
	current, desired, _, _ := crossNameCheckGraphs(t,
		`CHECK (currency IN ('usd', 'eur', 'jpy', 'krw'))`,
		`CHECK (currency ~ '^[a-z]{3}$')`,
		[]string{"currency"},
		[]string{"currency"},
	)
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 3 {
		t.Fatalf("cross-name widening plan=%#v", plan)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseExpand {
			t.Fatalf("proven cross-name widening retained contract work: %#v", item)
		}
	}
}

func TestCrossNameCheckCorrelationRejectsAmbiguousFamilies(t *testing.T) {
	current, desired, _, after := crossNameCheckGraphs(t,
		`CHECK (currency = 'jpy')`, `CHECK (currency ~ '^[a-z]{3}$')`,
		[]string{"currency"}, []string{"currency"},
	)
	second := after
	second.Name = "checkout_quote_currency_alternate"
	if err := desired.Add(second); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"currency"} {
		if err := desired.AddDependency(second.ObjectID(), (pgschema.Column{Table: second.Table, Name: column}).ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	changes := correlateCrossNameCheckChanges(change.Between(current, desired), current, desired)
	for _, item := range changes {
		if item.Kind == change.Modify {
			t.Fatalf("ambiguous enforcement family was correlated: %#v", changes)
		}
	}
}

func crossNameCheckGraphs(t *testing.T, beforeDefinition, afterDefinition string, beforeColumns, afterColumns []string) (*pgschema.Snapshot, *pgschema.Snapshot, pgschema.Constraint, pgschema.Constraint) {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "checkout_quote"}
	allColumns := map[string]bool{}
	for _, name := range append(append([]string(nil), beforeColumns...), afterColumns...) {
		allColumns[name] = true
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		for name := range allColumns {
			column := pgschema.Column{Table: table.ObjectID(), Name: name, Type: "text"}
			if err := snapshot.Add(column); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := pgschema.Constraint{Table: table.ObjectID(), Name: "checkout_quote_settlement_currency_jpy", Type: pgschema.ConstraintCheck, Definition: beforeDefinition, Validated: true}
	after := pgschema.Constraint{Table: table.ObjectID(), Name: "checkout_quote_settlement_currency_by_mode", Type: pgschema.ConstraintCheck, Definition: afterDefinition, Validated: true}
	for _, pair := range []struct {
		snapshot   *pgschema.Snapshot
		constraint pgschema.Constraint
		columns    []string
	}{{current, before, beforeColumns}, {desired, after, afterColumns}} {
		if err := pair.snapshot.Add(pair.constraint); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		for _, name := range pair.columns {
			if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), (pgschema.Column{Table: table.ObjectID(), Name: name}).ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
	}
	return current, desired, before, after
}
