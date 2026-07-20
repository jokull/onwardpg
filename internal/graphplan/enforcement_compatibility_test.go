package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestBuildUniqueKeyRelaxationIsExpandOnly(t *testing.T) {
	current, desired := uniqueRelaxationSnapshots(t)
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 {
		t.Fatalf("unique relaxation plan=%#v", plan)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseExpand {
			t.Fatalf("unique relaxation emitted contract work: %#v", item)
		}
	}
	joined := joinSQL(plan)
	if !strings.Contains(joined, `DROP CONSTRAINT "idx_package_component_flight_segment_unique"`) ||
		!strings.Contains(joined, `UNIQUE (package_component_id, direction, sequence)`) {
		t.Fatalf("unique relaxation SQL:\n%s", joined)
	}
}

func TestBuildPrimaryKeyRelaxationIsExpandOnly(t *testing.T) {
	uniqueCurrent, uniqueDesired := uniqueRelaxationSnapshots(t)
	convert := func(source *pgschema.Snapshot) *pgschema.Snapshot {
		result := pgschema.New()
		for _, object := range source.Objects() {
			switch value := object.(type) {
			case pgschema.Constraint:
				if value.Name == "idx_package_component_flight_segment_unique" {
					value.Type = pgschema.ConstraintPrimary
					value.Definition = strings.Replace(value.Definition, "UNIQUE", "PRIMARY KEY", 1)
					object = value
				}
			case pgschema.Index:
				if value.Name == "idx_package_component_flight_segment_unique" {
					value.Primary = true
					object = value
				}
			}
			if err := result.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range source.IDs() {
			for _, dependency := range source.Dependencies(id) {
				if err := result.AddDependency(id, dependency); err != nil {
					t.Fatal(err)
				}
			}
		}
		return result
	}
	plan, err := Build(convert(uniqueCurrent), convert(uniqueDesired), protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 {
		t.Fatalf("primary-key relaxation plan=%#v", plan)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseExpand {
			t.Fatalf("primary-key relaxation emitted contract work: %#v", item)
		}
	}
}

func TestUniqueKeyRelaxationRejectsStrengtheningAndNullTightening(t *testing.T) {
	current, desired := uniqueRelaxationSnapshots(t)
	beforeObject, _ := current.Object((pgschema.Constraint{Table: (pgschema.Table{Schema: "public", Name: "package_component_flight_segment"}).ObjectID(), Name: "idx_package_component_flight_segment_unique"}).ObjectID())
	afterObject, _ := desired.Object(beforeObject.ObjectID())
	before := beforeObject.(pgschema.Constraint)
	after := afterObject.(pgschema.Constraint)
	if !uniqueConstraintAcceptanceWider(before, after, current, desired) {
		t.Fatal("expected added key part to prove a unique relaxation")
	}
	if uniqueConstraintAcceptanceWider(after, before, desired, current) {
		t.Fatal("removing a unique key part was classified as a relaxation")
	}

	newIndexObject, _ := desired.Object((pgschema.Index{Table: after.Table, Name: after.Name}).ObjectID())
	newIndex := newIndexObject.(pgschema.Index)
	newIndex.NullsNotDistinct = true
	replacement := pgschema.New()
	for _, object := range desired.Objects() {
		if object.ObjectID() == newIndex.ObjectID() {
			object = newIndex
		}
		if err := replacement.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range desired.IDs() {
		for _, dependency := range desired.Dependencies(id) {
			if err := replacement.AddDependency(id, dependency); err != nil {
				t.Fatal(err)
			}
		}
	}
	if uniqueConstraintAcceptanceWider(before, after, current, replacement) {
		t.Fatal("NULLS NOT DISTINCT tightening was classified as a relaxation")
	}
}

func TestBuildIncomparableUniqueConstraintUsesLooseEnvelope(t *testing.T) {
	current, widerDesired := uniqueRelaxationSnapshots(t)
	desired := pgschema.New()
	for _, object := range widerDesired.Objects() {
		switch value := object.(type) {
		case pgschema.Constraint:
			if value.Name == "idx_package_component_flight_segment_unique" {
				value.Definition = `UNIQUE (package_component_id, direction)`
				object = value
			}
		case pgschema.Index:
			if value.Name == "idx_package_component_flight_segment_unique" {
				value.Parts = []pgschema.IndexPart{{Column: "package_component_id"}, {Column: "direction"}}
				object = value
			}
		}
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range widerDesired.IDs() {
		for _, dependency := range widerDesired.Dependencies(id) {
			if err := desired.AddDependency(id, dependency); err != nil {
				t.Fatal(err)
			}
		}
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 || plan.Statements[0].Phase != protocol.PhaseExpand || plan.Statements[1].Phase != protocol.PhaseContract {
		t.Fatalf("incomparable unique constraint did not use loose envelope: %#v", plan)
	}
	if !containsString(plan.Statements[0].Hazards, "temporary_uniqueness_unenforced") || !strings.Contains(plan.Statements[1].SQL, `UNIQUE (package_component_id, direction)`) {
		t.Fatalf("incomparable unique envelope SQL/hazards=%#v", plan.Statements)
	}
}

func TestBuildCapturesPreparationForUniqueIndexOnNewColumn(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "otp"}
	email := pgschema.Column{Table: table.ObjectID(), Name: "email", Type: "text", NotNull: true}
	defaultPurpose := "'login'::text"
	purpose := pgschema.Column{Table: table.ObjectID(), Name: "purpose", Type: "text", NotNull: true, Default: &defaultPurpose}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(email); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(email.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.Add(purpose); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(purpose.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	index := pgschema.Index{
		Table: table.ObjectID(), Name: "idx_otp_email_purpose_unique", Unique: true, Method: "btree",
		Parts: []pgschema.IndexPart{{Column: "email"}, {Column: "purpose"}},
	}
	if err := desired.Add(index); err != nil {
		t.Fatal(err)
	}
	for _, dependency := range []pgschema.ID{table.ObjectID(), email.ObjectID(), purpose.ObjectID()} {
		if err := desired.AddDependency(index.ObjectID(), dependency); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "prepare_unique" {
		t.Fatalf("OTP-style uniqueness did not request preparation: %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "prepare_unique", Key: index.ObjectID().String(), Value: "manual_sql", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}},
	}
	pending, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "prepare_unique_sql" {
		t.Fatalf("OTP-style uniqueness did not request SQL: %#v", pending)
	}
	answers.Answers = append(answers.Answers, protocol.Answer{
		Kind: "prepare_unique_sql", Key: index.ObjectID().String(), Value: "provided", QuestionFingerprint: pending.Questions[0].ScopeFingerprint,
		Manual: &protocol.ManualWork{
			Summary: "dedupe expired OTP rows", ExecutionMode: "transactional",
			Statements:      []string{`DELETE FROM "public"."otp" a USING "public"."otp" b WHERE a.email = b.email AND a.purpose = b.purpose AND a.ctid < b.ctid;`},
			VerificationSQL: []string{`SELECT NOT EXISTS (SELECT 1 FROM "public"."otp" GROUP BY email, purpose HAVING count(*) > 1);`},
		},
	})
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("prepared unique plan=%#v", plan)
	}
	manualAt, indexAt := -1, -1
	for position, item := range plan.Statements {
		if item.Manual != nil {
			manualAt = position
			if item.Phase != protocol.PhaseContract {
				t.Fatalf("unique cleanup ran before legacy drain: %#v", item)
			}
		}
		if strings.Contains(item.SQL, `CREATE UNIQUE INDEX "idx_otp_email_purpose_unique"`) {
			indexAt = position
		}
	}
	if manualAt < 0 || indexAt < 0 || manualAt > indexAt {
		t.Fatalf("unique preparation did not precede enforcement: %#v", plan.Statements)
	}
}

func TestBuildUnknownUniqueIndexReplacementUsesLooseEnvelope(t *testing.T) {
	current, desired := standaloneUniqueIndexSnapshots(t,
		[]pgschema.IndexPart{{Column: "trip_line_item_id"}},
		[]pgschema.IndexPart{{Column: "trip_line_item_id"}},
		`scope = 'trip_addon' AND stripe_payment_intent_id IS NULL`,
		`scope = 'trip_addon' AND completed_at IS NULL`,
	)
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 {
		t.Fatalf("partial unique replacement=%#v", plan)
	}
	if plan.Statements[0].Phase != protocol.PhaseExpand || !strings.Contains(plan.Statements[0].SQL, "DROP INDEX CONCURRENTLY") ||
		plan.Statements[1].Phase != protocol.PhaseContract || !strings.Contains(plan.Statements[1].SQL, "CREATE UNIQUE INDEX CONCURRENTLY") {
		t.Fatalf("unknown uniqueness did not use a loose overlap envelope: %#v", plan.Statements)
	}
	if containsString(plan.Statements[0].Hazards, "data_loss") || !containsString(plan.Statements[0].Hazards, "temporary_uniqueness_unenforced") {
		t.Fatalf("unique relaxation hazards=%#v", plan.Statements[0])
	}
}

func TestBuildStandaloneUniqueRelaxationConvergesInExpand(t *testing.T) {
	current, desired := standaloneUniqueIndexSnapshots(t,
		[]pgschema.IndexPart{{Column: "component_id"}, {Column: "sequence"}},
		[]pgschema.IndexPart{{Column: "component_id"}, {Column: "direction"}, {Column: "sequence"}},
		"", "",
	)
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 {
		t.Fatalf("standalone unique relaxation=%#v", plan)
	}
	for _, item := range plan.Statements {
		if item.Phase != protocol.PhaseExpand {
			t.Fatalf("proven standalone unique relaxation retained contract work: %#v", item)
		}
	}
}

func TestBuildForeignKeyMutationUsesLooseAcceptanceEnvelope(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	orders := pgschema.Table{Schema: "public", Name: "orders"}
	accounts := pgschema.Table{Schema: "public", Name: "accounts"}
	users := pgschema.Table{Schema: "public", Name: "users"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{orders, accounts, users} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	accountID, userID := accounts.ObjectID(), users.ObjectID()
	before := pgschema.Constraint{Table: orders.ObjectID(), Name: "orders_owner_fkey", Type: pgschema.ConstraintForeign, Definition: `FOREIGN KEY (owner_id) REFERENCES accounts(id) ON DELETE CASCADE`, Reference: &accountID, Validated: true}
	after := before
	after.Definition = `FOREIGN KEY (owner_id) REFERENCES users(id) ON DELETE SET NULL`
	after.Reference = &userID
	for _, pair := range []struct {
		snapshot   *pgschema.Snapshot
		constraint pgschema.Constraint
	}{{current, before}, {desired, after}} {
		if err := pair.snapshot.Add(pair.constraint); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), orders.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), *pair.constraint.Reference); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 3 {
		t.Fatalf("foreign-key envelope=%#v", plan)
	}
	if plan.Statements[0].Phase != protocol.PhaseExpand || !strings.Contains(plan.Statements[0].SQL, "DROP CONSTRAINT") {
		t.Fatalf("old foreign key was not relaxed in expand: %#v", plan.Statements)
	}
	for _, item := range plan.Statements[1:] {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("desired foreign key restored before contract: %#v", item)
		}
	}
	if !strings.Contains(plan.Statements[1].SQL, "NOT VALID") || !strings.Contains(plan.Statements[2].SQL, "VALIDATE CONSTRAINT") {
		t.Fatalf("foreign-key restoration was not staged: %#v", plan.Statements)
	}
}

func TestBuildExclusionMutationUsesLooseAcceptanceEnvelope(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "bookings"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "period", Type: "tstzrange"}
	before := pgschema.Constraint{Table: table.ObjectID(), Name: "bookings_period_excl", Type: pgschema.ConstraintExclusion, Definition: `EXCLUDE USING gist (period WITH &&)`, Validated: true}
	after := before
	after.Definition = `EXCLUDE USING gist (period WITH -|-)`
	for _, pair := range []struct {
		snapshot   *pgschema.Snapshot
		constraint pgschema.Constraint
	}{{current, before}, {desired, after}} {
		index := pgschema.Index{Table: table.ObjectID(), Name: before.Name, Unique: true, Exclusion: true, Constraint: before.Name, Method: "gist", Parts: []pgschema.IndexPart{{Column: "period"}}}
		for _, object := range []pgschema.Object{table, column, pair.constraint, index} {
			if err := pair.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, edge := range [][2]pgschema.ID{
			{column.ObjectID(), table.ObjectID()},
			{pair.constraint.ObjectID(), table.ObjectID()},
			{pair.constraint.ObjectID(), column.ObjectID()},
			{pair.constraint.ObjectID(), index.ObjectID()},
			{index.ObjectID(), table.ObjectID()},
			{index.ObjectID(), column.ObjectID()},
		} {
			if err := pair.snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 ||
		plan.Statements[0].Phase != protocol.PhaseExpand || plan.Statements[1].Phase != protocol.PhaseContract {
		t.Fatalf("exclusion mutation did not use loose envelope: %#v", plan)
	}
	if !containsString(plan.Statements[0].Hazards, "temporary_exclusion_unenforced") ||
		!strings.Contains(plan.Statements[1].SQL, `period WITH -|-`) {
		t.Fatalf("exclusion envelope SQL/hazards=%#v", plan.Statements)
	}
}

func TestBuildConstraintKindChangeUsesLooseAcceptanceEnvelope(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "jobs"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "state", Type: "text"}
	before := pgschema.Constraint{Table: table.ObjectID(), Name: "jobs_state_guard", Type: pgschema.ConstraintCheck, Definition: `CHECK (state IN ('queued', 'done'))`, CheckExpression: `state IN ('queued', 'done')`, Validated: true}
	after := pgschema.Constraint{Table: table.ObjectID(), Name: before.Name, Type: pgschema.ConstraintUnique, Definition: `UNIQUE (state)`, Validated: true}
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
	index := pgschema.Index{Table: table.ObjectID(), Name: after.Name, Constraint: after.Name, Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "state"}}}
	if err := desired.Add(index); err != nil {
		t.Fatal(err)
	}
	for _, dependency := range []pgschema.ID{table.ObjectID(), column.ObjectID()} {
		if err := desired.AddDependency(index.ObjectID(), dependency); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(after.ObjectID(), index.ObjectID()); err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 2 ||
		plan.Statements[0].Phase != protocol.PhaseExpand || plan.Statements[1].Phase != protocol.PhaseContract {
		t.Fatalf("constraint-kind change did not use loose envelope: %#v", plan)
	}
	if !strings.Contains(plan.Statements[0].SQL, `DROP CONSTRAINT "jobs_state_guard"`) ||
		!strings.Contains(plan.Statements[1].SQL, `UNIQUE (state)`) {
		t.Fatalf("constraint-kind envelope SQL=%#v", plan.Statements)
	}
}

func TestBuildColumnDefaultPolaritySupportsBothWriters(t *testing.T) {
	table := pgschema.Table{Schema: "public", Name: "jobs"}
	without := pgschema.Column{Table: table.ObjectID(), Name: "status", Type: "text", NotNull: true}
	defaultStatus := "'queued'::text"
	with := without
	with.Default = &defaultStatus
	build := func(before, after pgschema.Column) protocol.Result {
		current, desired := pgschema.New(), pgschema.New()
		for _, pair := range []struct {
			snapshot *pgschema.Snapshot
			column   pgschema.Column
		}{{current, before}, {desired, after}} {
			if err := pair.snapshot.Add(table); err != nil {
				t.Fatal(err)
			}
			if err := pair.snapshot.Add(pair.column); err != nil {
				t.Fatal(err)
			}
			if err := pair.snapshot.AddDependency(pair.column.ObjectID(), table.ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	added := build(without, with)
	if added.Status != protocol.Planned || len(added.Statements) != 1 || added.Statements[0].Phase != protocol.PhaseExpand || !strings.Contains(added.Statements[0].SQL, "SET DEFAULT") {
		t.Fatalf("added default did not support the new omitting writer in expand: %#v", added)
	}
	removed := build(with, without)
	if removed.Status != protocol.Planned || len(removed.Statements) != 1 || removed.Statements[0].Phase != protocol.PhaseContract || !strings.Contains(removed.Statements[0].SQL, "DROP DEFAULT") {
		t.Fatalf("removed default did not preserve the legacy omitting writer through expand: %#v", removed)
	}
}

func standaloneUniqueIndexSnapshots(t *testing.T, beforeParts, afterParts []pgschema.IndexPart, beforePredicate, afterPredicate string) (*pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	columnNames := map[string]bool{"scope": true, "stripe_payment_intent_id": true, "completed_at": true, "component_id": true, "direction": true, "sequence": true, "trip_line_item_id": true}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		for name := range columnNames {
			column := pgschema.Column{Table: table.ObjectID(), Name: name, Type: "text"}
			if err := snapshot.Add(column); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := pgschema.Index{Table: table.ObjectID(), Name: "idx_order_trip_addon_open_unique", Unique: true, Method: "btree", Parts: beforeParts, Predicate: beforePredicate}
	after := before
	after.Parts, after.Predicate = afterParts, afterPredicate
	for _, pair := range []struct {
		snapshot *pgschema.Snapshot
		index    pgschema.Index
	}{{current, before}, {desired, after}} {
		if err := pair.snapshot.Add(pair.index); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.index.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		for _, part := range pair.index.Parts {
			if part.Column != "" {
				if err := pair.snapshot.AddDependency(pair.index.ObjectID(), (pgschema.Column{Table: table.ObjectID(), Name: part.Column}).ObjectID()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return current, desired
}

func uniqueRelaxationSnapshots(t *testing.T) (*pgschema.Snapshot, *pgschema.Snapshot) {
	t.Helper()
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "package_component_flight_segment"}
	columns := []pgschema.Column{
		{Table: table.ObjectID(), Name: "package_component_id", Position: 1, Type: "text", NotNull: true},
		{Table: table.ObjectID(), Name: "direction", Position: 2, Type: "text", NotNull: true},
		{Table: table.ObjectID(), Name: "sequence", Position: 3, Type: "integer", NotNull: true},
	}
	before := pgschema.Constraint{
		Table: table.ObjectID(), Name: "idx_package_component_flight_segment_unique", Type: pgschema.ConstraintUnique,
		Definition: `UNIQUE (package_component_id, sequence)`, Validated: true, UsingIndex: "idx_package_component_flight_segment_unique",
	}
	after := before
	after.Definition = `UNIQUE (package_component_id, direction, sequence)`
	oldIndex := pgschema.Index{
		Table: table.ObjectID(), Name: before.Name, Constraint: before.Name, Unique: true, Method: "btree",
		Parts: []pgschema.IndexPart{{Column: "package_component_id"}, {Column: "sequence"}},
	}
	newIndex := oldIndex
	newIndex.Parts = []pgschema.IndexPart{{Column: "package_component_id"}, {Column: "direction"}, {Column: "sequence"}}

	for _, pair := range []struct {
		snapshot   *pgschema.Snapshot
		constraint pgschema.Constraint
		index      pgschema.Index
	}{{current, before, oldIndex}, {desired, after, newIndex}} {
		if err := pair.snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		for _, column := range columns {
			if err := pair.snapshot.Add(column); err != nil {
				t.Fatal(err)
			}
			if err := pair.snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
		for _, object := range []pgschema.Object{pair.index, pair.constraint} {
			if err := pair.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := pair.snapshot.AddDependency(pair.index.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		for _, part := range pair.index.Parts {
			columnID := (pgschema.Column{Table: table.ObjectID(), Name: part.Column}).ObjectID()
			if err := pair.snapshot.AddDependency(pair.index.ObjectID(), columnID); err != nil {
				t.Fatal(err)
			}
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.constraint.ObjectID(), pair.index.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	return current, desired
}
