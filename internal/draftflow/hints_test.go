package draftflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestRefreshRebindAfterHintsReportsFinalResolvedState(t *testing.T) {
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version,
		Answers:         []protocol.Answer{{Kind: "rename_table", Key: "app:accounts", Value: "app:customers"}},
	}
	report := &protocol.RebindReport{
		ProtocolVersion: protocol.RebindVersion,
		Unanswered:      []string{"rename_table:app:accounts"},
		Deferred:        []string{"rename_table:app:accounts", "type_change:app:accounts:created_at"},
	}
	refreshRebindAfterHints(report, answers, protocol.Result{Status: protocol.Planned})
	if !reflect.DeepEqual(report.Answers, answers) || len(report.Unanswered) != 0 || len(report.Deferred) != 0 {
		t.Fatalf("resolved rebind retained pending state: %#v", report)
	}
	wantInvalidated := []protocol.RebindFinding{{Decision: "type_change:app:accounts:created_at", Reason: "question_no_longer_present"}}
	if !reflect.DeepEqual(report.Invalidated, wantInvalidated) {
		t.Fatalf("resolved rebind invalidations = %#v", report.Invalidated)
	}
}

func TestBuildPlanConsumesAheadOfTimeRenameHint(t *testing.T) {
	current, desired := columnRenameSnapshots(t)
	hint := protocol.Hint{
		Kind: "rename", Object: "column",
		From: []string{"public", "users", "name"}, To: []string{"public", "users", "display_name"},
	}
	plan, _, answers, questions, hints, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{hint}, HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsInput || len(plan.Questions) != 1 || plan.Questions[0].Kind != "rename_compatibility_bridge" {
		t.Fatalf("plan = %#v", plan)
	}
	if answers == nil || len(answers.Answers) != 1 || len(questions) != 2 || len(hints) != 1 {
		t.Fatalf("answers=%#v questions=%#v hints=%#v", answers, questions, hints)
	}
	if answers.Answers[0].QuestionFingerprint == "" {
		t.Fatal("semantic answer lost its narrow scope fingerprint")
	}
}

func TestBuildPlanSelectsOneOfMultipleRenameCandidatesBeforeOrAfterQuestion(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	accounts := pgschema.Table{Schema: "public", Name: "accounts"}
	customers := pgschema.Table{Schema: "public", Name: "customers"}
	prospects := pgschema.Table{Schema: "public", Name: "prospects"}
	for _, object := range []pgschema.Object{schema, accounts, pgschema.Column{Table: accounts.ObjectID(), Name: "id", Position: 1, Type: "bigint"}} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []pgschema.Table{customers, prospects} {
		if err := desired.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := desired.Add(pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.Add(schema); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]pgschema.ID{
		{accounts.ObjectID(), schema.ObjectID()},
		{(pgschema.Column{Table: accounts.ObjectID(), Name: "id"}).ObjectID(), accounts.ObjectID()},
	} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []pgschema.Table{customers, prospects} {
		if err := desired.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := desired.AddDependency((pgschema.Column{Table: table.ObjectID(), Name: "id"}).ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	pending, _, _, _, _, err := buildPlan(current, desired, Input{PlannerOptions: graphplan.Options{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || len(pending.Questions[0].Choices) != 3 {
		t.Fatalf("multiple-candidate question = %#v", pending)
	}
	hint := protocol.Hint{Kind: "rename", Object: "table", From: []string{"public", "accounts"}, To: []string{"public", "prospects"}}
	planned, _, _, _, hints, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{hint}, HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(hints) != 1 || !planContains(planned, `RENAME TO "prospects"`) || !planContains(planned, `CREATE TABLE "public"."customers"`) {
		t.Fatalf("selected multiple-candidate rename = %#v", planned)
	}
}

func TestBuildPlanAcceptsResendingAnAlreadyReceiptedHint(t *testing.T) {
	current, desired := columnRenameSnapshots(t)
	hint := protocol.Hint{
		Kind: "rename", Object: "column",
		From: []string{"public", "users", "name"}, To: []string{"public", "users", "display_name"},
	}
	plan, rebind, answers, questions, previous, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{hint}, HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, answers, _, hints, err := buildWithSemanticHints(
		current, desired, plan, rebind, answers, questions, previous, []protocol.Hint{hint}, graphplan.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsInput || len(plan.Questions) != 1 || plan.Questions[0].Kind != "rename_compatibility_bridge" || answers == nil || len(answers.Answers) != 1 || len(hints) != 1 {
		t.Fatalf("plan=%#v answers=%#v hints=%#v", plan, answers, hints)
	}
}

func TestBuildPlanUsesOneDropHintForRenameRejectionAndRemoval(t *testing.T) {
	current, desired := columnRenameSnapshots(t)
	hint := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}}
	plan, _, answers, questions, hints, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{hint}, HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !planContains(plan, "ADD COLUMN") || !planContains(plan, "DROP COLUMN") {
		t.Fatalf("plan = %#v", plan)
	}
	if answers == nil || len(answers.Answers) != 2 {
		t.Fatalf("answers = %#v", answers)
	}
	if len(questions) != 2 || len(hints) != 1 {
		t.Fatalf("questions=%#v hints=%#v", questions, hints)
	}
}

func TestBuildPlanRejectsUnusedAheadOfTimeHint(t *testing.T) {
	current, desired := columnRenameSnapshots(t)
	_, _, _, _, _, err := buildPlan(current, desired, Input{
		Hints:      []protocol.Hint{{Kind: "drop", Object: "column", Name: []string{"public", "other", "legacy"}}},
		HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "unused semantic hints") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildPlanRejectsContradictorySemanticIntent(t *testing.T) {
	current, desired := columnRenameSnapshots(t)
	_, _, _, _, _, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{
			{Kind: "rename", Object: "column", From: []string{"public", "users", "name"}, To: []string{"public", "users", "display_name"}},
			{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}},
		},
		HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "contradictory hints") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildPlanTurnsManualTypeIntentIntoEditableIncompleteSQL(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "events"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(pgschema.Column{Table: table.ObjectID(), Name: "occurred_on", Type: "text"}); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(pgschema.Column{Table: table.ObjectID(), Name: "occurred_on", Type: "date"}); err != nil {
		t.Fatal(err)
	}
	hint := protocol.Hint{
		Kind: "type_change", Name: []string{"public", "events", "occurred_on"}, Strategy: "manual_sql",
	}
	plan, _, answers, _, hints, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{hint}, HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsSQLEdits || !planContains(plan, "ONWARDPG TODO") {
		t.Fatalf("plan = %#v", plan)
	}
	if answers == nil || answers.Answers[0].Value != "manual_sql" || len(hints) != 1 {
		t.Fatalf("answers=%#v hints=%#v", answers, hints)
	}
}

func TestBuildPlanConsumesAheadOfTimeNotNullAndBackfillHintsAcrossStages(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "events"}
	before := pgschema.Column{Table: table.ObjectID(), Name: "occurred_on", Position: 1, Type: "date"}
	after := before
	after.NotNull = true
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.AddDependency(before.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	name := []string{"public", "events", "occurred_on"}
	plan, _, answers, questions, hints, err := buildPlan(current, desired, Input{
		Hints: []protocol.Hint{
			{Kind: "rollout", Name: name, Strategy: "staged_with_backfill"},
			{Kind: "manual_sql", Action: "backfill_not_null", Object: "column", Name: name},
		},
		HintsGiven: true, PlannerOptions: graphplan.Options{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsSQLEdits || !planContains(plan, "ONWARDPG TODO") {
		t.Fatalf("plan = %#v", plan)
	}
	if answers == nil || len(answers.Answers) != 2 || len(questions) != 2 || len(hints) != 2 {
		t.Fatalf("answers=%#v questions=%#v hints=%#v", answers, questions, hints)
	}
}

func columnRenameSnapshots(t *testing.T) (*pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	table := pgschema.Table{Schema: "public", Name: "users"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(schema); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(pgschema.Column{Table: table.ObjectID(), Name: "name", Type: "text"}); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(pgschema.Column{Table: table.ObjectID(), Name: "display_name", Type: "text"}); err != nil {
		t.Fatal(err)
	}
	return current, desired
}

func planContains(plan protocol.Result, fragment string) bool {
	for _, statement := range plan.Statements {
		if strings.Contains(statement.SQL, fragment) {
			return true
		}
	}
	return false
}
