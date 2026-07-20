package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestContractReconciliationAssertOnlyCreatesOrderedGates(t *testing.T) {
	current, desired := checkReconciliationSnapshots(t)
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "reconcile_contract" {
		t.Fatalf("reconciliation question=%#v", pending)
	}
	question := pending.Questions[0]
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: "assert_only", QuestionFingerprint: question.ScopeFingerprint}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.ContractGates) != 2 || len(plan.Reconciliations) != 1 {
		t.Fatalf("gated plan=%#v", plan)
	}
	dataGate := gateByKind(t, plan.ContractGates, "data_assertion")
	if !strings.Contains(dataGate.BooleanSQL, `WHERE (tier = 'open') IS FALSE`) {
		t.Fatalf("CHECK gate does not preserve CHECK false-only semantics: %#v", dataGate)
	}
	addAt, assertionAt, validateAt := -1, -1, -1
	for index, statement := range plan.Statements {
		switch {
		case strings.Contains(statement.SQL, "ADD CONSTRAINT"):
			addAt = index
			if !containsString(statement.RequiresGates, "writers:legacy") || containsString(statement.RequiresGates, dataGate.ID) {
				t.Fatalf("NOT VALID fence gate requirements=%#v", statement)
			}
		case strings.Contains(statement.SQL, "DO $onwardpg$"):
			assertionAt = index
		case strings.Contains(statement.SQL, "VALIDATE CONSTRAINT"):
			validateAt = index
			if !containsString(statement.RequiresGates, dataGate.ID) || !containsString(statement.RequiresGates, "writers:legacy") {
				t.Fatalf("validation gate requirements=%#v", statement)
			}
		}
	}
	if addAt < 0 || assertionAt <= addAt || validateAt <= assertionAt {
		t.Fatalf("contract fence/assert/validate order=%#v", plan.Statements)
	}
}

func TestContractReconciliationManualRequiresBooleanVerification(t *testing.T) {
	current, desired := checkReconciliationSnapshots(t)
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := pending.Questions[0]
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: first.Kind, Key: first.Key, Value: "manual_sql", QuestionFingerprint: first.ScopeFingerprint}},
	}
	pending, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "reconcile_contract_sql" {
		t.Fatalf("manual reconciliation question=%#v", pending)
	}
	manual := pending.Questions[0]
	answers.Answers = append(answers.Answers, protocol.Answer{
		Kind: manual.Kind, Key: manual.Key, Value: "provided", QuestionFingerprint: manual.ScopeFingerprint,
		Manual: &protocol.ManualWork{Summary: "normalize legacy tier", ExecutionMode: "transactional", Statements: []string{"UPDATE public.delivery SET tier = 'open' WHERE tier = 'legacy';"}},
	})
	if _, err := Build(current, desired, answers, Options{}); err == nil || !strings.Contains(err.Error(), "requires at least one boolean verification") {
		t.Fatalf("missing reconciliation verification error=%v", err)
	}
}

func TestForeignKeyReadinessBooleanPreservesMatchSemantics(t *testing.T) {
	referenced := (pgschema.Table{Schema: "app", Name: "parents"}).ObjectID()
	constraint := pgschema.Constraint{
		Table: (pgschema.Table{Schema: "app", Name: "children"}).ObjectID(), Name: "children_parent_fkey", Type: pgschema.ConstraintForeign,
		Reference: &referenced, ForeignKeyColumns: []string{"tenant_id", "parent_id"}, ReferencedColumns: []string{"tenant_id", "id"},
		ForeignKeyEqualityOperators: []pgschema.ForeignKeyOperator{{Schema: "pg_catalog", Name: "="}, {Schema: "pg_catalog", Name: "="}},
		ForeignKeyMatch:             pgschema.ForeignKeyMatchSimple,
	}
	simple, ok := foreignKeyReadinessBoolean(constraint)
	if !ok || !strings.Contains(simple, `local_row."tenant_id" IS NULL OR local_row."parent_id" IS NULL`) ||
		!strings.Contains(simple, `referenced_row."id" OPERATOR("pg_catalog".=) local_row."parent_id"`) {
		t.Fatalf("MATCH SIMPLE probe=%q ok=%v", simple, ok)
	}
	constraint.ForeignKeyMatch = pgschema.ForeignKeyMatchFull
	full, ok := foreignKeyReadinessBoolean(constraint)
	if !ok || !strings.Contains(full, "AND NOT") || !strings.Contains(full, "OR (NOT") {
		t.Fatalf("MATCH FULL probe=%q ok=%v", full, ok)
	}
	constraint.ForeignKeyMatch = pgschema.ForeignKeyMatchPartial
	if query, ok := foreignKeyReadinessBoolean(constraint); ok || query != "" {
		t.Fatalf("MATCH PARTIAL must remain manual: %q %v", query, ok)
	}
}

func TestReconciliationScopeSurvivesUnrelatedChangeButRejectsDependencyChange(t *testing.T) {
	current, desired := checkReconciliationSnapshots(t)
	old, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	question := old.Questions[0]
	previous := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: old.CurrentFingerprint, DesiredFingerprint: old.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: "assert_only", QuestionFingerprint: question.ScopeFingerprint}},
	}
	unrelated := pgschema.Table{Schema: "public", Name: "audit_log"}
	if err := current.Add(unrelated); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(unrelated); err != nil {
		t.Fatal(err)
	}
	withUnrelated, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rebound, err := protocol.RebindAnswers(previous, old.Questions, withUnrelated.Questions, withUnrelated.CurrentFingerprint, withUnrelated.DesiredFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(rebound.Carried) != 1 || len(rebound.Invalidated) != 0 {
		t.Fatalf("unrelated object invalidated reconciliation: %#v", rebound)
	}
	dependency := pgschema.Column{Table: (pgschema.Table{Schema: "public", Name: "delivery"}).ObjectID(), Name: "region", Type: "text"}
	if err := desired.Add(dependency); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(dependency.ObjectID(), dependency.Table); err != nil {
		t.Fatal(err)
	}
	constraintID := (pgschema.Constraint{Table: dependency.Table, Name: "delivery_tier_check"}).ObjectID()
	if err := desired.AddDependency(constraintID, dependency.ObjectID()); err != nil {
		t.Fatal(err)
	}
	withDependency, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rebound, err = protocol.RebindAnswers(previous, old.Questions, withDependency.Questions, withDependency.CurrentFingerprint, withDependency.DesiredFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(rebound.Carried) != 0 || len(rebound.Invalidated) != 1 || rebound.Invalidated[0].Reason != "question_scope_changed" {
		t.Fatalf("dependency change retained stale reconciliation: %#v", rebound)
	}
}

func gateByKind(t *testing.T, gates []protocol.ContractGate, kind string) protocol.ContractGate {
	t.Helper()
	for _, gate := range gates {
		if gate.Kind == kind {
			return gate
		}
	}
	t.Fatalf("gate kind %q absent from %#v", kind, gates)
	return protocol.ContractGate{}
}

func checkReconciliationSnapshots(t *testing.T) (*pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "delivery"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "tier", Type: "text"}
	before := pgschema.Constraint{Table: table.ObjectID(), Name: "delivery_tier_check", Type: pgschema.ConstraintCheck, Definition: `CHECK (tier IN ('open', 'legacy'))`, CheckExpression: `tier IN ('open', 'legacy')`, Validated: true}
	after := before
	after.Definition = `CHECK (tier = 'open')`
	after.CheckExpression = `tier = 'open'`
	for _, pair := range []struct {
		snapshot   *pgschema.Snapshot
		constraint pgschema.Constraint
	}{{current, before}, {desired, after}} {
		for _, object := range []pgschema.Object{table, column, pair.constraint} {
			if err := pair.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := pair.snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), column.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	return current, desired
}

func buildWithAssertOnlyReconciliations(t *testing.T, current, desired *pgschema.Snapshot, options Options) protocol.Result {
	t.Helper()
	answers := protocol.Answers{}
	for attempt := 0; attempt < 4; attempt++ {
		plan, err := Build(current, desired, answers, options)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != protocol.NeedsInput {
			return plan
		}
		if answers.ProtocolVersion == "" {
			answers.ProtocolVersion, answers.CurrentFingerprint, answers.DesiredFingerprint = protocol.Version, plan.CurrentFingerprint, plan.DesiredFingerprint
		}
		for _, question := range plan.Questions {
			if question.Kind != "reconcile_contract" || !containsString(question.Choices, "assert_only") {
				t.Fatalf("question requires an explicit non-assert reconciliation: %#v", question)
			}
			answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "assert_only", QuestionFingerprint: question.ScopeFingerprint})
		}
	}
	t.Fatal("reconciliation questions did not converge")
	return protocol.Result{}
}

func buildWithManualReconciliation(t *testing.T, current, desired *pgschema.Snapshot, options Options, cleanupSQL, verificationSQL string) protocol.Result {
	t.Helper()
	answers := protocol.Answers{}
	for attempt := 0; attempt < 5; attempt++ {
		plan, err := Build(current, desired, answers, options)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != protocol.NeedsInput {
			return plan
		}
		if answers.ProtocolVersion == "" {
			answers.ProtocolVersion, answers.CurrentFingerprint, answers.DesiredFingerprint = protocol.Version, plan.CurrentFingerprint, plan.DesiredFingerprint
		}
		for _, question := range plan.Questions {
			switch question.Kind {
			case "reconcile_contract":
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "manual_sql", QuestionFingerprint: question.ScopeFingerprint})
			case "reconcile_contract_sql":
				answers.Answers = append(answers.Answers, protocol.Answer{
					Kind: question.Kind, Key: question.Key, Value: "provided", QuestionFingerprint: question.ScopeFingerprint,
					Manual: &protocol.ManualWork{Summary: "reconcile overlap rows", ExecutionMode: "transactional", Statements: []string{cleanupSQL}, VerificationSQL: []string{verificationSQL}},
				})
			default:
				t.Fatalf("unexpected question during manual reconciliation: %#v", question)
			}
		}
	}
	t.Fatal("manual reconciliation questions did not converge")
	return protocol.Result{}
}
