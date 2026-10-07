package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

var indexDropTable = pgschema.Table{Schema: "app", Name: "accounts"}

func indexDropSnapshots(t *testing.T, retained []pgschema.Object, onlyCurrent []pgschema.Object, onlyDesired []pgschema.Object) (*pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	base := []pgschema.Object{
		pgschema.Schema{Name: "app"}, indexDropTable,
		pgschema.Column{Table: indexDropTable.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true},
		pgschema.Column{Table: indexDropTable.ObjectID(), Name: "email", Position: 2, Type: "text"},
		pgschema.Column{Table: indexDropTable.ObjectID(), Name: "org", Position: 3, Type: "bigint"},
	}
	current := snapshotForTest(t, append(append(append([]pgschema.Object(nil), base...), retained...), onlyCurrent...)...)
	desired := snapshotForTest(t, append(append(append([]pgschema.Object(nil), base...), retained...), onlyDesired...)...)
	return current, desired
}

func uniqueIndexForTest(name string, parts ...pgschema.IndexPart) pgschema.Index {
	return pgschema.Index{Table: indexDropTable.ObjectID(), Name: name, Unique: true, Method: "btree", Parts: parts}
}

func primaryKeyForTest(columns ...string) []pgschema.Object {
	parts := make([]pgschema.IndexPart, len(columns))
	for index, column := range columns {
		parts[index] = pgschema.IndexPart{Column: column}
	}
	return []pgschema.Object{
		pgschema.Constraint{Table: indexDropTable.ObjectID(), Name: "accounts_pkey", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (" + strings.Join(columns, ", ") + ")", Validated: true},
		pgschema.Index{Table: indexDropTable.ObjectID(), Name: "accounts_pkey", Constraint: "accounts_pkey", Unique: true, Primary: true, Method: "btree", Parts: parts},
	}
}

func buildForTest(t *testing.T, current, desired *pgschema.Snapshot, answers []protocol.Answer, options Options) protocol.Result {
	t.Helper()
	result, err := Build(current, desired, protocol.Answers{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) == 0 {
		return result
	}
	answers = append([]protocol.Answer(nil), answers...)
	for index := range answers {
		for _, question := range result.Questions {
			if question.Kind == answers[index].Kind && question.Key == answers[index].Key {
				answers[index].QuestionFingerprint = question.ScopeFingerprint
			}
		}
	}
	result, err = Build(current, desired, protocol.Answers{
		CurrentFingerprint: result.CurrentFingerprint, DesiredFingerprint: result.DesiredFingerprint, Answers: answers,
	}, options)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNonUniqueIndexDropNeedsNoDecisionAndIsNotDataLoss(t *testing.T) {
	index := pgschema.Index{Table: indexDropTable.ObjectID(), Name: "accounts_org_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "org"}}}
	current, desired := indexDropSnapshots(t, nil, []pgschema.Object{index}, nil)
	for _, mode := range []struct {
		options       Options
		sql, lock     string
		transactional bool
	}{
		{Options{}, `DROP INDEX "app"."accounts_org_idx";`, "blocking_lock", true},
		{Options{ConcurrentIndexes: true}, `DROP INDEX CONCURRENTLY "app"."accounts_org_idx";`, "concurrent_index_drop", false},
	} {
		result := buildForTest(t, current, desired, nil, mode.options)
		if result.Status != protocol.Planned || len(result.Questions) != 0 || len(result.Statements) != 1 {
			t.Fatalf("a non-unique index drop must plan with no decision and one statement: %#v", result)
		}
		statement := result.Statements[0]
		if statement.SQL != mode.sql || statement.Phase != protocol.PhaseContract || statement.Safety != "review" || statement.NonTransactional == mode.transactional {
			t.Fatalf("statement = %#v", statement)
		}
		if containsString(statement.Hazards, "data_loss") || !containsString(statement.Hazards, HazardSlowerQueries) || !containsString(statement.Hazards, mode.lock) {
			t.Fatalf("hazards = %v", statement.Hazards)
		}
		if len(result.Batches) != 1 || result.Batches[0].Transactional != mode.transactional {
			t.Fatalf("batches = %#v", result.Batches)
		}
	}
}

// Every drop of a unique index or of a constraint keeps one decision, also
// when another index enforces the same uniqueness. The decision names the
// enforcement, not data loss.
func TestUniqueIndexAndConstraintDropsKeepOneEnforcementDecision(t *testing.T) {
	copyOfKey := uniqueIndexForTest("accounts_id_copy_idx", pgschema.IndexPart{Column: "id"})
	partial := uniqueIndexForTest("accounts_email_idx", pgschema.IndexPart{Expression: "lower(email)"})
	partial.Predicate = "(org IS NOT NULL)"
	uniqueConstraint := pgschema.Constraint{Table: indexDropTable.ObjectID(), Name: "accounts_id_key", Type: pgschema.ConstraintUnique, Definition: "UNIQUE (id)", Validated: true}
	uniqueBacking := pgschema.Index{Table: indexDropTable.ObjectID(), Name: "accounts_id_key", Constraint: "accounts_id_key", Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	for name, fixture := range map[string]struct {
		dropped  []pgschema.Object
		key      pgschema.Object
		message  string
		hazard   string
		sql      string
		lockMode string
	}{
		"unique index that copies the primary key": {
			[]pgschema.Object{copyOfKey}, copyOfKey, "unique-index enforcement", "unique_index_enforcement_removed", `DROP INDEX "app"."accounts_id_copy_idx";`, "blocking_lock",
		},
		"partial expression unique index": {
			[]pgschema.Object{partial}, partial, "unique-index enforcement", "unique_index_enforcement_removed", `DROP INDEX "app"."accounts_email_idx";`, "blocking_lock",
		},
		"unique constraint that copies the primary key": {
			[]pgschema.Object{uniqueConstraint, uniqueBacking}, uniqueConstraint, "unique enforcement", "temporary_or_permanent_uniqueness_unenforced",
			`ALTER TABLE "app"."accounts" DROP CONSTRAINT "accounts_id_key";`, "access_exclusive_lock",
		},
	} {
		t.Run(name, func(t *testing.T) {
			current, desired := indexDropSnapshots(t, primaryKeyForTest("id"), fixture.dropped, nil)
			pending := buildForTest(t, current, desired, nil, Options{})
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "drop" ||
				pending.Questions[0].Key != fixture.key.ObjectID().String() || !strings.Contains(pending.Questions[0].Message, fixture.message) {
				t.Fatalf("a unique drop must ask exactly one enforcement decision: %#v", pending)
			}
			if hazards := DropDecisionHazards(fixture.key); containsString(hazards, "data_loss") || !containsString(hazards, fixture.hazard) || !containsString(hazards, "duplicate_rows_possible") {
				t.Fatalf("decision hazards = %v", hazards)
			}
			if DropNeedsNoDecision(fixture.key.ObjectID(), current, desired) {
				t.Fatal("DropNeedsNoDecision must agree with the question")
			}
			planned := buildForTest(t, current, desired, []protocol.Answer{{Kind: "drop", Key: fixture.key.ObjectID().String(), Value: "drop"}}, Options{})
			if planned.Status != protocol.Planned || len(planned.Statements) != 1 {
				t.Fatalf("answered plan = %#v", planned)
			}
			statement := planned.Statements[0]
			if statement.SQL != fixture.sql || statement.Phase != protocol.PhaseExpand || statement.Safety != "review" || containsString(statement.Hazards, "data_loss") ||
				!containsString(statement.Hazards, fixture.hazard) || !containsString(statement.Hazards, fixture.lockMode) {
				t.Fatalf("statement = %#v", statement)
			}
		})
	}
	// A primary key drop keeps its decision too.
	current, desired := indexDropSnapshots(t, nil, primaryKeyForTest("id"), nil)
	if pending := buildForTest(t, current, desired, nil, Options{}); pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("a primary key drop must keep its decision: %#v", pending)
	}
}

// An index that the table is clustered on, or that is its replica identity,
// is more than a performance index: its drop keeps a decision.
func TestClusteredAndReplicaIdentityIndexDropsKeepADecision(t *testing.T) {
	index := pgschema.Index{Table: indexDropTable.ObjectID(), Name: "accounts_org_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "org"}}}
	current, desired := indexDropSnapshots(t, nil, []pgschema.Object{index}, nil)
	if !DropNeedsNoDecision(index.ObjectID(), current, desired) {
		t.Fatal("a plain index drop needs no decision")
	}
	for _, selector := range []string{"clustered_index:app.accounts_org_idx", `clustered_index:"app"."accounts_org_idx"`} {
		clustered, _ := indexDropSnapshots(t, nil, []pgschema.Object{index}, nil)
		if err := clustered.AddIgnored(selector); err != nil {
			t.Fatal(err)
		}
		if DropNeedsNoDecision(index.ObjectID(), clustered, desired) {
			t.Fatalf("%s: a clustered index drop must keep its decision", selector)
		}
	}
	other, _ := indexDropSnapshots(t, nil, []pgschema.Object{index}, nil)
	if err := other.AddIgnored("clustered_index:app.another_idx"); err != nil {
		t.Fatal(err)
	}
	if !DropNeedsNoDecision(index.ObjectID(), other, desired) {
		t.Fatal("clustering on another index does not change this drop")
	}
	id := index.ObjectID()
	identity, _ := indexDropSnapshots(t, nil, []pgschema.Object{index, pgschema.ReplicaIdentity{Table: indexDropTable.ObjectID(), Mode: pgschema.ReplicaIdentityIndex, Index: &id}}, nil)
	if DropNeedsNoDecision(index.ObjectID(), identity, desired) {
		t.Fatal("a replica identity index drop must keep its decision")
	}
}

func TestDataDropsKeepTheDataLossDecision(t *testing.T) {
	column := pgschema.Column{Table: indexDropTable.ObjectID(), Name: "legacy", Position: 4, Type: "text"}
	index := pgschema.Index{Table: indexDropTable.ObjectID(), Name: "accounts_legacy_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "legacy"}}}
	current, desired := indexDropSnapshots(t, nil, []pgschema.Object{column, index}, nil)
	pending := buildForTest(t, current, desired, nil, Options{})
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Key != column.ObjectID().String() {
		t.Fatalf("a column drop must ask exactly its own decision: %#v", pending)
	}
	if hazards := DropDecisionHazards(column); len(hazards) != 1 || hazards[0] != "data_loss" {
		t.Fatalf("column decision hazards = %v", hazards)
	}
	if DropNeedsNoDecision(column.ObjectID(), current, desired) || !DropNeedsNoDecision(index.ObjectID(), current, desired) {
		t.Fatal("only the index drop is free of a decision")
	}
}

func TestConcurrentModeOnPartitionedTableUsesOnlineBuildAndPlainDrop(t *testing.T) {
	parent := pgschema.Table{Schema: "app", Name: "events", Partition: &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (at)"}}
	child := pgschema.Table{Schema: "app", Name: "events_2026", PartitionOf: &pgschema.PartitionOf{Parent: parent.ObjectID(), Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}}
	parentID := func(index pgschema.Index) *pgschema.ID { id := index.ObjectID(); return &id }
	oldParent := pgschema.Index{Table: parent.ObjectID(), Name: "events_old_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	oldChild := pgschema.Index{Table: child.ObjectID(), Name: "events_2026_id_idx", Parent: parentID(oldParent), Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	newParent := pgschema.Index{Table: parent.ObjectID(), Name: "events_kind_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "kind"}}}
	newChild := pgschema.Index{Table: child.ObjectID(), Name: "events_2026_kind_idx", Parent: parentID(newParent), Method: "btree", Parts: []pgschema.IndexPart{{Column: "kind"}}}
	base := []pgschema.Object{pgschema.Schema{Name: "app"}, parent, child}
	current := snapshotForTest(t, append(append([]pgschema.Object(nil), base...), oldParent, oldChild)...)
	desired := snapshotForTest(t, append(append([]pgschema.Object(nil), base...), newParent, newChild)...)

	result := buildForTest(t, current, desired, nil, Options{ConcurrentIndexes: true})
	if result.Status != protocol.Planned {
		t.Fatalf("plan = %#v", result)
	}
	want := []struct {
		sql              string
		phase            string
		nonTransactional bool
	}{
		{`CREATE INDEX "events_kind_idx" ON ONLY "app"."events" USING "btree" ("kind");`, protocol.PhaseExpand, false},
		{`CREATE INDEX CONCURRENTLY "events_2026_kind_idx" ON "app"."events_2026" USING "btree" ("kind");`, protocol.PhaseExpand, true},
		{`ALTER INDEX "app"."events_kind_idx" ATTACH PARTITION "app"."events_2026_kind_idx";`, protocol.PhaseExpand, false},
		{`DROP INDEX "app"."events_old_idx";`, protocol.PhaseContract, false},
	}
	if len(result.Statements) != len(want) {
		t.Fatalf("statements = %s", joinSQL(result))
	}
	for index, expected := range want {
		statement := result.Statements[index]
		if statement.SQL != expected.sql || statement.Phase != expected.phase || statement.NonTransactional != expected.nonTransactional {
			t.Fatalf("statement %d = %#v, want %#v", index, statement, expected)
		}
	}
	if drop := result.Statements[3]; !containsString(drop.Hazards, HazardPartitionedIndexNotConcurrent) || !containsString(drop.Hazards, "blocking_lock") {
		t.Fatalf("partitioned drop hazards = %v", drop.Hazards)
	}

	// A plan that also removes a partition keeps the one plain statement:
	// the removed partition stays attached until contract.
	removed := pgschema.Table{Schema: "app", Name: "events_2025", PartitionOf: &pgschema.PartitionOf{Parent: parent.ObjectID(), Bound: "FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')"}}
	withRemoved := snapshotForTest(t, append(append([]pgschema.Object(nil), base...), removed)...)
	shrinking := buildForTest(t, withRemoved, snapshotForTest(t, append(append([]pgschema.Object(nil), base...), newParent, newChild)...),
		[]protocol.Answer{{Kind: "drop", Key: removed.ObjectID().String(), Value: "drop"}}, Options{ConcurrentIndexes: true})
	if sql := joinSQL(shrinking); shrinking.Status != protocol.Planned || strings.Contains(sql, "ON ONLY") || strings.Contains(sql, "CONCURRENTLY") ||
		!strings.Contains(sql, `CREATE INDEX "events_kind_idx" ON "app"."events"`) {
		t.Fatalf("plan with a removed partition = %s (%#v)", sql, shrinking)
	}

	// A unique index on a partitioned table keeps the one plain statement.
	uniqueParent := newParent
	uniqueParent.Unique = true
	uniqueChild := newChild
	uniqueChild.Unique = true
	desired = snapshotForTest(t, append(append([]pgschema.Object(nil), base...), oldParent, oldChild, uniqueParent, uniqueChild)...)
	unique := buildForTest(t, current, desired, []protocol.Answer{{Kind: "reconcile_contract", Key: uniqueParent.ObjectID().String(), Value: "assert_only"}}, Options{ConcurrentIndexes: true})
	sql := joinSQL(unique)
	if strings.Contains(sql, "CONCURRENTLY") || strings.Contains(sql, "ON ONLY") || !strings.Contains(sql, `CREATE UNIQUE INDEX "events_kind_idx" ON "app"."events"`) {
		t.Fatalf("unique partitioned index plan = %s (%#v)", sql, unique)
	}
}

func TestConcurrentModeOnPartitionedTableWithNoPartitionUsesThePlainStatement(t *testing.T) {
	parent := pgschema.Table{Schema: "app", Name: "events", Partition: &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (at)"}}
	index := pgschema.Index{Table: parent.ObjectID(), Name: "events_kind_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "kind"}}}
	current := snapshotForTest(t, pgschema.Schema{Name: "app"}, parent)
	desired := snapshotForTest(t, pgschema.Schema{Name: "app"}, parent, index)
	result := buildForTest(t, current, desired, nil, Options{ConcurrentIndexes: true})
	if result.Status != protocol.Planned || len(result.Statements) != 1 || result.Statements[0].NonTransactional ||
		result.Statements[0].SQL != `CREATE INDEX "events_kind_idx" ON "app"."events" USING "btree" ("kind");` ||
		!containsString(result.Statements[0].Hazards, HazardPartitionedIndexNotConcurrent) {
		t.Fatalf("plan = %#v", result)
	}
}
