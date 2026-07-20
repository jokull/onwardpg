package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestClassifyFiniteCheckAcceptance(t *testing.T) {
	tests := []struct {
		name     string
		before   string
		after    string
		relation checkAcceptanceRelation
	}{
		{
			name:     "catalog any widening",
			before:   `("tier" = ANY (ARRAY['shift_push'::text, 'assigned_email'::text]))`,
			after:    `("tier" = ANY (ARRAY['slack_new_conversation'::text, 'shift_push'::text, 'assigned_email'::text]))`,
			relation: checkAcceptanceWider,
		},
		{
			name:     "declarative in narrowing",
			before:   `tier IN ('open', 'closed')`,
			after:    `tier IN ('open')`,
			relation: checkAcceptanceNarrower,
		},
		{
			name:     "incomparable sets",
			before:   `tier IN ('open', 'closed')`,
			after:    `tier IN ('open', 'pending')`,
			relation: checkAcceptanceUnknown,
		},
		{
			name:     "different columns",
			before:   `tier IN ('open')`,
			after:    `state IN ('open', 'closed')`,
			relation: checkAcceptanceUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := pgschema.Constraint{CheckExpression: test.before}
			after := pgschema.Constraint{CheckExpression: test.after}
			if got := classifyCheckAcceptance(before, after); got != test.relation {
				t.Fatalf("relation=%v want %v", got, test.relation)
			}
		})
	}
}

func TestClassifyTripCheckWidenings(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{
			name: "manual amount gains a dato hotel escape hatch",
			before: `((booking_mode = 'manual' AND manual_amount IS NOT NULL AND manual_amount > 0) OR
				(booking_mode = 'expedia' AND manual_amount IS NULL) OR
				(booking_mode = 'no_hotel' AND manual_amount IS NULL))`,
			after: `((booking_mode = 'manual' AND (manual_amount > 0 OR (manual_amount IS NULL AND dato_hotel_id IS NOT NULL))) OR
				(booking_mode = 'expedia' AND manual_amount IS NULL) OR
				(booking_mode = 'no_hotel' AND manual_amount IS NULL))`,
		},
		{
			name:   "known currencies become any lowercase ISO-shaped code",
			before: `currency IN ('usd', 'eur', 'jpy', 'krw')`,
			after:  `currency ~ '^[a-z]{3}$'`,
		},
		{
			name: "one branch widens its finite field set",
			before: `(source_table = 'trip_template' AND field_name IN ('title', 'description')) OR
				(source_table = 'package' AND field_name IN ('title', 'creator_bio'))`,
			after: `(source_table = 'trip_template' AND field_name IN ('title', 'description')) OR
				(source_table = 'package' AND field_name IN ('title', 'creator_bio', 'co_host_bio'))`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := pgschema.Constraint{CheckExpression: test.before}
			after := pgschema.Constraint{CheckExpression: test.after}
			if got := classifyCheckAcceptance(before, after); got != checkAcceptanceWider {
				t.Fatalf("relation=%v want wider", got)
			}
		})
	}
}

func TestClassifyCheckWideningUsesPreservedNotNullColumns(t *testing.T) {
	table := pgschema.Table{Schema: "public", Name: "translation_value"}
	current, desired := pgschema.New(), pgschema.New()
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"source_table", "field_name"} {
			column := pgschema.Column{Table: table.ObjectID(), Name: name, Type: "text", NotNull: true}
			if err := snapshot.Add(column); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := pgschema.Constraint{Table: table.ObjectID(), CheckExpression: `source_table = 'tour' AND field_name = 'name'`}
	after := pgschema.Constraint{Table: table.ObjectID(), CheckExpression: `(source_table IS NOT NULL AND field_name IS NOT NULL AND source_table = 'tour' AND field_name = 'name') OR (source_table = 'package' AND field_name = 'title')`}
	if got := classifyCheckAcceptance(before, after); got != checkAcceptanceUnknown {
		t.Fatalf("without catalog invariants relation=%v want unknown", got)
	}
	if got := classifyCheckAcceptanceWithSnapshots(before, after, current, desired); got != checkAcceptanceWider {
		t.Fatalf("with preserved NOT NULL relation=%v want wider", got)
	}
}

func TestFiniteCheckDoesNotProveUnsafeRegexWidening(t *testing.T) {
	tests := []string{
		`currency ~ '^[a-z]+$'`,
		`currency ~ '^[a-z]{2}$'`,
		`currency ~ '^[A-Z]{3}$'`,
	}
	for _, afterExpression := range tests {
		before := pgschema.Constraint{CheckExpression: `currency IN ('usd', 'eur')`}
		after := pgschema.Constraint{CheckExpression: afterExpression}
		if got := classifyCheckAcceptance(before, after); got != checkAcceptanceUnknown {
			t.Fatalf("after=%q relation=%v want unknown", afterExpression, got)
		}
	}
}

func TestBuildCheckWideningIsExpandOnly(t *testing.T) {
	before := `CHECK ("tier" IN ('shift_push', 'assigned_email'))`
	after := `CHECK ("tier" IN ('slack_new_conversation', 'shift_push', 'assigned_email'))`
	plan := buildCheckTransition(t, before, after)
	if plan.Status != protocol.Planned {
		t.Fatalf("status=%s plan=%#v", plan.Status, plan)
	}
	if len(plan.Statements) != 3 || len(plan.Batches) != 2 {
		t.Fatalf("widening statements/batches=%#v", plan)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseExpand {
			t.Fatalf("widening emitted non-expand statement: %#v", item)
		}
	}
	joined := joinSQL(plan)
	for _, wanted := range []string{
		`DROP CONSTRAINT "support_escalation_delivery_tier_check"`,
		`CHECK ("tier" IN ('slack_new_conversation', 'shift_push', 'assigned_email')) NOT VALID`,
		`VALIDATE CONSTRAINT "support_escalation_delivery_tier_check"`,
	} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("widening missing %q:\n%s", wanted, joined)
		}
	}
	if !plan.Batches[0].Transactional || len(plan.Batches[0].Statements) != 2 || len(plan.Batches[1].Statements) != 1 {
		t.Fatalf("widening replacement/validation boundaries=%#v", plan.Batches)
	}
}

func TestBuildCheckNarrowingKeepsOldEnforcementUntilContract(t *testing.T) {
	plan := buildCheckTransition(t,
		`CHECK ("tier" IN ('open', 'closed'))`,
		`CHECK ("tier" IN ('open'))`,
	)
	if plan.Status != protocol.Planned || len(plan.Statements) != 4 {
		t.Fatalf("narrowing plan=%#v", plan)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("narrowing changed expand state: %#v", item)
		}
	}
	joined := joinSQL(plan)
	if !strings.Contains(joined, `ADD CONSTRAINT "onwardpg_tmpcheck_`) ||
		!strings.Contains(joined, `VALIDATE CONSTRAINT "onwardpg_tmpcheck_`) ||
		!strings.Contains(joined, `DROP CONSTRAINT "support_escalation_delivery_tier_check"`) ||
		!strings.Contains(joined, `RENAME CONSTRAINT "onwardpg_tmpcheck_`) {
		t.Fatalf("narrowing did not stage and atomically swap desired enforcement:\n%s", joined)
	}
}

func TestBuildUnknownCheckChangeUsesLooseExpandEnvelope(t *testing.T) {
	plan := buildCheckTransition(t,
		`CHECK ((amount > 0))`,
		`CHECK ((amount <> 10))`,
	)
	if plan.Status != protocol.Planned || len(plan.Statements) != 3 {
		t.Fatalf("unknown check plan=%#v", plan)
	}
	if plan.Statements[0].Phase != protocol.PhaseExpand || !strings.Contains(plan.Statements[0].SQL, "DROP CONSTRAINT") {
		t.Fatalf("unknown change did not relax in expand: %#v", plan.Statements)
	}
	for _, item := range plan.Statements[1:] {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("unknown change restored enforcement before contract: %#v", item)
		}
	}
	if !containsString(plan.Statements[0].Hazards, "temporary_check_enforcement_removed") {
		t.Fatalf("missing temporary enforcement hazard: %#v", plan.Statements[0])
	}
}

func TestBuildNewCheckOnExistingTableStagesValidationInContract(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "amount", Type: "integer"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(column); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	check := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_amount_positive", Type: pgschema.ConstraintCheck, Definition: "CHECK (amount > 0)", Validated: true}
	if err := desired.Add(check); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(check.ObjectID(), column.ObjectID()); err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 || len(plan.Batches) != 2 {
		t.Fatalf("new CHECK validation plan=%#v", plan)
	}
	if !strings.Contains(plan.Statements[0].SQL, "NOT VALID") || !strings.Contains(plan.Statements[1].SQL, "VALIDATE CONSTRAINT") {
		t.Fatalf("new CHECK did not stage validation: %#v", plan.Statements)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("new enforcement escaped contract: %#v", item)
		}
	}
}

func buildCheckTransition(t *testing.T, beforeDefinition, afterDefinition string) protocol.Result {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "support_escalation_delivery"}
	before := pgschema.Constraint{
		Table: table.ObjectID(), Name: "support_escalation_delivery_tier_check",
		Type: pgschema.ConstraintCheck, Definition: beforeDefinition, Validated: true,
	}
	after := before
	after.Definition = afterDefinition
	for _, pair := range []struct {
		snapshot   *pgschema.Snapshot
		constraint pgschema.Constraint
	}{{current, before}, {desired, after}} {
		if err := pair.snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.Add(pair.constraint); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
