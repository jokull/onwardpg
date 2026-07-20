package semantichint

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestResolveConsumesSemanticDropAcrossPlannerStages(t *testing.T) {
	current, desired, _ := renameFixture(t)
	hint := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "name"}}
	resolution, err := Resolve(current, desired, []protocol.Hint{hint}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Result.Status != protocol.Planned || len(resolution.Answers.Answers) != 2 || len(resolution.Questions) != 2 || len(resolution.Hints) != 1 {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestResolveRejectsUnconsumedIntent(t *testing.T) {
	current, desired, _ := renameFixture(t)
	_, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "drop", Object: "column", Name: []string{"public", "missing", "name"},
	}}, graphplan.Options{})
	if err == nil || !strings.Contains(err.Error(), "unused semantic hints") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveWorkspaceRequiresExplicitRenameOrPreserveIntent(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	beforeColumn := pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	afterColumn := pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	for _, object := range []pgschema.Object{from, beforeColumn} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{to, afterColumn} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	options := graphplan.Options{PreserveSurplus: true}
	pending, err := Resolve(current, desired, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Result.Status != protocol.NeedsInput || len(pending.Result.Questions) != 1 ||
		!contains(pending.Result.Questions[0].Choices, "preserve") {
		t.Fatalf("workspace ambiguity = %#v", pending)
	}
	preserved, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "preserve", Object: "table", Name: []string{"app", "accounts"},
	}}, options)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Result.Status != protocol.Planned || !contains(preserved.Result.Preserved, from.ObjectID().String()) ||
		strings.Contains(joinResolutionSQL(preserved), "RENAME TO") {
		t.Fatalf("preserved workspace object = %#v", preserved)
	}
	renamed, err := Resolve(current, desired, []protocol.Hint{{
		Kind: "rename", Object: "table", From: []string{"app", "accounts"}, To: []string{"app", "customers"},
	}}, options)
	if err != nil {
		t.Fatal(err)
	}
	sql := joinResolutionSQL(renamed)
	if renamed.Result.Status != protocol.Planned || len(renamed.Result.Preserved) != 0 ||
		!strings.Contains(sql, `CREATE VIEW "app"."customers" WITH (security_invoker = true)`) ||
		!strings.Contains(sql, `DROP VIEW "app"."customers";`) {
		t.Fatalf("workspace rename bridge = %#v", renamed)
	}
}

func TestResolveRequiresBackfillStrategyAfterConfirmedColumnRename(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	before := pgschema.Column{Table: table.ObjectID(), Name: "display_name", Position: 1, Type: "text"}
	after := pgschema.Column{Table: table.ObjectID(), Name: "full_name", Position: 1, Type: "text"}
	for _, object := range []pgschema.Object{table, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	hints := []protocol.Hint{{Kind: "rename", Object: "column", From: []string{"app", "accounts", "display_name"}, To: []string{"app", "accounts", "full_name"}}}
	resolution, err := Resolve(current, desired, hints, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Result.Status != protocol.NeedsInput || len(resolution.Result.Questions) != 1 || resolution.Result.Questions[0].Kind != "rename_backfill_strategy" || len(resolution.Result.Statements) != 0 {
		t.Fatalf("confirmed rename must require an explicit backfill strategy: %#v", resolution)
	}
	hints = append(hints, protocol.Hint{Kind: "rename_backfill", Name: []string{"app", "accounts", "display_name"}, Strategy: "single_transaction"})
	resolution, err = Resolve(current, desired, hints, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql := joinResolutionSQL(resolution)
	if resolution.Result.Status != protocol.Planned || !strings.Contains(sql, "CREATE TRIGGER") || !strings.Contains(sql, `RENAME COLUMN "display_name" TO "full_name"`) {
		t.Fatalf("explicit backfill strategy must produce the overlap bridge: %#v", resolution)
	}
	manualHints := append(hints[:1],
		protocol.Hint{Kind: "rename_backfill", Name: []string{"app", "accounts", "display_name"}, Strategy: "manual_sql"},
		protocol.Hint{Kind: "manual_sql", Object: "column", Name: []string{"app", "accounts", "display_name"}, Action: "rename_backfill"},
	)
	manual, err := Resolve(current, desired, manualHints, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if manual.Result.Status != protocol.NeedsSQLEdits || !strings.Contains(joinResolutionSQL(manual), "ONWARDPG TODO") {
		t.Fatalf("manual backfill strategy must create an editable, unverifiable-until-edited handoff: %#v", manual)
	}
	for _, item := range manual.Result.Statements {
		if item.Manual != nil && (len(item.Manual.VerificationSQL) != 0 || strings.Contains(item.SQL, "SELECT false")) {
			t.Fatalf("rename backfill retained a hidden placeholder verifier: %#v", item)
		}
	}
}

func TestResolveDefersValidHintBehindEarlierQuestionWithoutReceiptingIt(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	before := pgschema.Column{Table: table.ObjectID(), Name: "display_name", Position: 1, Type: "text"}
	after := pgschema.Column{Table: table.ObjectID(), Name: "full_name", Position: 1, Type: "text"}
	for _, object := range []pgschema.Object{table, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	later := protocol.Hint{Kind: "rename_backfill", Name: []string{"app", "accounts", "display_name"}, Strategy: "single_transaction"}
	resolution, err := Resolve(current, desired, []protocol.Hint{later}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Result.Status != protocol.NeedsInput || len(resolution.Deferred) != 1 || len(resolution.Hints) != 0 || len(resolution.Answers.Answers) != 0 {
		t.Fatalf("deferred resolution = %#v", resolution)
	}
}

func TestResolveIdentityHintReachesTableCandidacyAndProducesManualBridge(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	beforeID := pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	afterID := pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	beforeKey := pgschema.Constraint{Table: from.ObjectID(), Name: "billing_identity", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)"}
	afterKey := pgschema.Constraint{Table: to.ObjectID(), Name: "customer_identity", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)"}
	for _, object := range []pgschema.Object{from, beforeID, beforeKey} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{to, afterID, afterKey} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	hints := []protocol.Hint{
		{Kind: "identity", Object: "table", From: []string{"app", "accounts"}, To: []string{"app", "customers"}},
		{Kind: "manual_sql", Object: "table", Name: []string{"app", "accounts"}, Action: "rename_compatibility_bridge"},
	}
	resolution, err := Resolve(current, desired, hints, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Result.Status != protocol.NeedsSQLEdits || !strings.Contains(joinResolutionSQL(resolution), "ONWARDPG TODO") || !strings.Contains(joinResolutionSQL(resolution), "explicitly asserted") {
		t.Fatalf("identity assertion must produce a bounded manual bridge: %#v", resolution)
	}
}

func joinResolutionSQL(resolution Resolution) string {
	statements := make([]string, len(resolution.Result.Statements))
	for index, statement := range resolution.Result.Statements {
		statements[index] = statement.SQL
	}
	return strings.Join(statements, "\n")
}
