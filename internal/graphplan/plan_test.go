package graphplan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestBuildCreatesDependencyOrderedGraphPlan(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	enum := pgschema.Enum{Schema: "app", Name: "state", Labels: []string{"open", "closed"}}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	id := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true}
	state := pgschema.Column{Table: table.ObjectID(), Name: "state", Position: 2, Type: `app.state`, NotNull: true, Default: stringPointer("'open'::app.state")}
	primary := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_pkey", Type: pgschema.ConstraintPrimary, Definition: `PRIMARY KEY (id)`, Validated: true}
	index := pgschema.Index{Table: table.ObjectID(), Name: "orders_state_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "state", NullsLast: true}}}
	for _, object := range []pgschema.Object{schema, enum, table, id, state, primary, index} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{
		{enum.ObjectID(), schema.ObjectID()}, {table.ObjectID(), schema.ObjectID()},
		{id.ObjectID(), table.ObjectID()}, {state.ObjectID(), table.ObjectID()}, {state.ObjectID(), enum.ObjectID()},
		{primary.ObjectID(), table.ObjectID()}, {index.ObjectID(), table.ObjectID()},
	} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || result.CurrentFingerprint == "" || result.DesiredFingerprint == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
	joined := joinSQL(result)
	for _, fragment := range []string{
		`CREATE SCHEMA "app";`,
		`CREATE TYPE "app"."state" AS ENUM ('open', 'closed');`,
		`CREATE TABLE "app"."orders" ("id" bigint NOT NULL, "state" app.state DEFAULT 'open'::app.state NOT NULL);`,
		`ALTER TABLE "app"."orders" ADD CONSTRAINT "orders_pkey" PRIMARY KEY (id);`,
		`CREATE INDEX "orders_state_idx" ON "app"."orders" USING "btree" ("state" NULLS LAST);`,
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("plan missing %q:\n%s", fragment, joined)
		}
	}
	if strings.Index(joined, "CREATE TYPE") > strings.Index(joined, "CREATE TABLE") {
		t.Fatalf("enum must be created before dependent table:\n%s", joined)
	}
}

func TestBuildRejectsRangeCanonicalFunctionChoreography(t *testing.T) {
	desired := snapshotForTest(t,
		pgschema.Schema{Name: "app"},
		pgschema.Range{Schema: "app", Name: "canonical_range", Subtype: "integer", Canonical: "app.canonical_range_canonical", MultirangeName: "canonical_range_multirange"},
	)
	result, err := Build(pgschema.New(), desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || !hasUnsupportedPrefix(result, "range_canonical_dependency:") {
		t.Fatalf("custom range canonical function must remain explicit: %#v", result)
	}
}

func TestBuildRejectsDependencyOnlyGraphDifference(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{schema, table} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desired.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, "dependency_only_graph_difference") {
		t.Fatalf("dependency-only difference must fail closed: %#v", result)
	}
}

func TestExistingPrimaryConstraintExposesBlockingIndexBuild(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	id := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true}
	primary := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_pkey", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)", Validated: true}
	primaryIndex := pgschema.Index{Table: table.ObjectID(), Name: "orders_pkey", Constraint: "orders_pkey", Unique: true, Primary: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{schema, table, id} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, edge := range [][2]pgschema.ID{{table.ObjectID(), schema.ObjectID()}, {id.ObjectID(), table.ObjectID()}} {
			if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desired.Add(primary); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(primaryIndex); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]pgschema.ID{{primary.ObjectID(), table.ObjectID()}, {primary.ObjectID(), primaryIndex.ObjectID()}, {primaryIndex.ObjectID(), table.ObjectID()}, {primaryIndex.ObjectID(), id.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}

	result := buildWithAssertOnlyReconciliations(t, current, desired, Options{ConcurrentIndexes: true})
	if result.Status != protocol.Planned || len(result.Statements) != 2 {
		t.Fatalf("unexpected constraint result: %#v", result)
	}
	enforcement := result.Statements[len(result.Statements)-1]
	for _, hazard := range []string{"access_exclusive_lock", "blocking_index_build"} {
		if !containsString(enforcement.Hazards, hazard) {
			t.Fatalf("constraint hazards omit %q: %#v", hazard, enforcement.Hazards)
		}
	}
	if strings.Contains(enforcement.SQL, "CONCURRENTLY") {
		t.Fatalf("constraint-backed index build was incorrectly made concurrent: %q", enforcement.SQL)
	}
}

func TestBuildNullableColumnWithoutDefaultDoesNotClaimTableRewrite(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	id := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	displayName := pgschema.Column{Table: table.ObjectID(), Name: "display_name", Position: 2, Type: "text"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{schema, table, id} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, edge := range [][2]pgschema.ID{{table.ObjectID(), schema.ObjectID()}, {id.ObjectID(), table.ObjectID()}} {
			if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desired.Add(displayName); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(displayName.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !reflect.DeepEqual(result.Statements[0].Hazards, []string{"table_lock", "application_row_shape_change"}) {
		t.Fatalf("nullable metadata-only ADD COLUMN hazards = %#v", result.Statements[0].Hazards)
	}
}

func TestBuildWorkspaceModePreservesSurplusObjectsWithoutDropQuestion(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	legacy := pgschema.Table{Schema: "app", Name: "legacy"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(schema); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(legacy); err != nil {
		t.Fatal(err)
	}
	if err := current.AddDependency(legacy.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	strict, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strict.Status != protocol.NeedsInput {
		t.Fatalf("strict result = %#v", strict)
	}
	workspace, err := Build(current, desired, protocol.Answers{}, Options{PreserveSurplus: true})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Status != protocol.Planned || len(workspace.Statements) != 0 || !reflect.DeepEqual(workspace.Preserved, []string{legacy.ObjectID().String()}) {
		t.Fatalf("workspace result = %#v", workspace)
	}
}

func FuzzQuoteIdentifierRoundTrip(f *testing.F) {
	for _, seed := range []string{"orders", `a"b`, "select", "", "emoji_😀", "line\nname"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, identifier string) {
		rendered := quote(identifier)
		if len(rendered) < 2 || rendered[0] != '"' || rendered[len(rendered)-1] != '"' {
			t.Fatalf("quoted identifier lacks delimiters: %q", rendered)
		}
		inner := rendered[1 : len(rendered)-1]
		if got := strings.ReplaceAll(inner, `""`, `"`); got != identifier {
			t.Fatalf("quoted identifier does not round-trip: got %q want %q", got, identifier)
		}
	})
}

func TestBuildOrdersPoliciesBeforeRLSAndQuotesRolesAndPrivileges(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "tenant_id", Position: 1, Type: "bigint"}
	using := `tenant_id = current_setting('app.tenant_id')::bigint`
	policy := pgschema.Policy{Table: table.ObjectID(), Name: `tenant"access`, Permissive: true, Command: "SELECT", Roles: []string{"PUBLIC", `role"; DROP TABLE nope; --`}, Using: &using}
	rls := pgschema.RowSecurity{Table: table.ObjectID(), Enabled: true, Forced: true}
	privilege := pgschema.TablePrivilege{Table: table.ObjectID(), Grantee: `role"; DROP TABLE nope; --`, Grantor: "@owner", Privilege: "SELECT", Grantable: true}
	for _, object := range []pgschema.Object{schema, table, column, policy, rls, privilege} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{
		{table.ObjectID(), schema.ObjectID()}, {column.ObjectID(), table.ObjectID()},
		{policy.ObjectID(), table.ObjectID()}, {policy.ObjectID(), column.ObjectID()},
		{rls.ObjectID(), table.ObjectID()}, {rls.ObjectID(), policy.ObjectID()},
		{privilege.ObjectID(), table.ObjectID()},
	} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned {
		t.Fatalf("result = %#v", result)
	}
	joined := joinSQL(result)
	policySQL := `CREATE POLICY "tenant""access" ON "app"."orders" AS PERMISSIVE FOR SELECT TO PUBLIC, "role""; DROP TABLE nope; --" USING (`
	grantSQL := `GRANT SELECT ON TABLE "app"."orders" TO "role""; DROP TABLE nope; --" WITH GRANT OPTION;`
	for _, fragment := range []string{policySQL, grantSQL, `ENABLE ROW LEVEL SECURITY`, `FORCE ROW LEVEL SECURITY`} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("plan missing %q:\n%s", fragment, joined)
		}
	}
	if strings.Index(joined, "CREATE POLICY") > strings.Index(joined, "ENABLE ROW LEVEL SECURITY") || strings.Index(joined, "ENABLE ROW LEVEL SECURITY") > strings.Index(joined, "FORCE ROW LEVEL SECURITY") {
		t.Fatalf("unsafe RLS order:\n%s", joined)
	}
	for _, item := range result.Statements {
		if (strings.Contains(item.SQL, "POLICY") || strings.Contains(item.SQL, "ROW LEVEL SECURITY")) && item.Phase != protocol.PhaseExpand {
			t.Fatalf("new-table authorization must stay in expand: %#v", item)
		}
	}
}

func TestBuildDefersExistingTableTriggerPolicyAndRLSToContract(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "tenant_id", Position: 1, Type: "bigint"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{schema, table, column} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, edge := range [][2]pgschema.ID{{table.ObjectID(), schema.ObjectID()}, {column.ObjectID(), table.ObjectID()}} {
			if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	using := "tenant_id > 0"
	policy := pgschema.Policy{Table: table.ObjectID(), Name: "tenant", Permissive: false, Command: "SELECT", Roles: []string{"PUBLIC"}, Using: &using}
	rls := pgschema.RowSecurity{Table: table.ObjectID(), Enabled: true}
	trigger := pgschema.Trigger{Table: table.ObjectID(), Name: "audit", Definition: `CREATE TRIGGER audit BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.audit()`, Enabled: "O"}
	for _, object := range []pgschema.Object{policy, rls, trigger} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
		if err := desired.AddDependency(object.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(rls.ObjectID(), policy.ObjectID()); err != nil {
		t.Fatal(err)
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 3 {
		t.Fatalf("unexpected activation plan: %#v", result)
	}
	for _, item := range result.Statements {
		if item.Phase != protocol.PhaseContract || !containsString(item.Hazards, "application_behavior_change") {
			t.Fatalf("existing-table activation was not deferred: %#v", item)
		}
	}
}

func TestBuildOrdersExistingPolicyChangeBeforeRowSecurityTighteningInContract(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	oldUsing, newUsing := "tenant_id > 0", "tenant_id > 10"
	oldPolicy := pgschema.Policy{Table: table.ObjectID(), Name: "tenant", Permissive: true, Command: "SELECT", Roles: []string{"PUBLIC"}, Using: &oldUsing}
	newPolicy := oldPolicy
	newPolicy.Using = &newUsing
	oldRLS := pgschema.RowSecurity{Table: table.ObjectID(), Enabled: true, Forced: false}
	newRLS := pgschema.RowSecurity{Table: table.ObjectID(), Enabled: true, Forced: true}
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		policy   pgschema.Policy
		rls      pgschema.RowSecurity
	}{{current, oldPolicy, oldRLS}, {desired, newPolicy, newRLS}} {
		for _, object := range []pgschema.Object{table, fixture.policy, fixture.rls} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, edge := range [][2]pgschema.ID{{fixture.policy.ObjectID(), table.ObjectID()}, {fixture.rls.ObjectID(), table.ObjectID()}, {fixture.rls.ObjectID(), fixture.policy.ObjectID()}} {
			if err := fixture.snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}

	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "alter_policy" {
		t.Fatalf("policy tightening question = %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint,
		DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{
			Kind: pending.Questions[0].Kind, Key: pending.Questions[0].Key, Value: "alter", QuestionFingerprint: pending.Questions[0].ScopeFingerprint,
		}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil || planned.Status != protocol.Planned {
		t.Fatalf("policy and RLS tightening plan=%#v err=%v", planned, err)
	}
	joined := joinSQL(planned)
	policyAt, forceAt := strings.Index(joined, "ALTER POLICY"), strings.Index(joined, "FORCE ROW LEVEL SECURITY")
	if policyAt < 0 || forceAt < 0 || policyAt > forceAt {
		t.Fatalf("policy must change before RLS tightening:\n%s", joined)
	}
	for _, item := range planned.Statements {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("existing authorization tightening escaped contract: %#v", item)
		}
		if strings.Contains(item.SQL, "FORCE ROW LEVEL SECURITY") && !containsString(item.Hazards, "application_behavior_change") {
			t.Fatalf("RLS tightening omitted behavior hazard: %#v", item)
		}
	}
}

func TestBuildBlocksRoutineSemanticChangesWithRetainedStoredDependents(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "measurements"}
	base := pgschema.Column{Table: table.ObjectID(), Name: "value", Position: 1, Type: "integer"}
	generated := pgschema.Column{Table: table.ObjectID(), Name: "score", Position: 2, Type: "integer", Generated: &pgschema.Generated{Expression: "public.score(value)", Kind: "STORED"}}
	check := pgschema.Constraint{Table: table.ObjectID(), Name: "score_positive", Type: pgschema.ConstraintCheck, Definition: "CHECK (public.score(value) > 0)", Validated: true}
	index := pgschema.Index{Table: table.ObjectID(), Name: "score_idx", Method: "btree", Parts: []pgschema.IndexPart{{Expression: "public.score(value)"}}}
	before := pgschema.Routine{Schema: "public", Name: "score", Signature: "integer", Kind: "function", ReturnType: "integer", Definition: "CREATE OR REPLACE FUNCTION public.score(integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'"}
	after := before
	after.Definition = "CREATE OR REPLACE FUNCTION public.score(integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 2'"
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		routine  pgschema.Routine
	}{{current, before}, {desired, after}} {
		for _, object := range []pgschema.Object{table, base, generated, check, index, fixture.routine} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, dependent := range []pgschema.ID{generated.ObjectID(), check.ObjectID(), index.ObjectID()} {
			if err := fixture.snapshot.AddDependency(dependent, fixture.routine.ObjectID()); err != nil {
				t.Fatal(err)
			}
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []pgschema.ID{generated.ObjectID(), check.ObjectID(), index.ObjectID()} {
		if !hasUnsupportedPrefix(result, "routine_semantic_change_stored_dependent:"+id.String()) {
			t.Fatalf("retained stored dependent %s did not block: %#v", id, result)
		}
	}
}

func TestBuildBlocksSemanticRoutineRenameWithRetainedMaterializedView(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Routine{Schema: "public", Name: "old_score", Signature: "integer", Kind: "function", ReturnType: "integer", Definition: "CREATE FUNCTION public.old_score(integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'"}
	after := pgschema.Routine{Schema: "public", Name: "score", Signature: "integer", Kind: "function", ReturnType: "integer", Definition: "CREATE FUNCTION public.score(integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 2'"}
	oldView := pgschema.View{Schema: "public", Name: "scores", Materialized: true, Populated: true, Definition: "SELECT public.old_score(1) AS score"}
	newView := oldView
	newView.Definition = "SELECT public.score(1) AS score"
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		routine  pgschema.Routine
		view     pgschema.View
	}{{current, before, oldView}, {desired, after, newView}} {
		if err := fixture.snapshot.Add(fixture.routine); err != nil {
			t.Fatal(err)
		}
		if err := fixture.snapshot.Add(fixture.view); err != nil {
			t.Fatal(err)
		}
		if err := fixture.snapshot.AddDependency(fixture.view.ObjectID(), fixture.routine.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_routine" {
		t.Fatalf("semantic rename question=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || !hasUnsupportedPrefix(result, "routine_semantic_change_stored_dependent:"+oldView.ObjectID().String()) {
		t.Fatalf("semantic rename retained stale materialized data: %#v", result)
	}
}

func TestBuildBlocksExtensionUpgradeWithRetainedDependent(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Extension{Schema: "public", Name: "opaque", Version: "1.0"}
	after := before
	after.Version = "2.0"
	table := pgschema.Table{Schema: "public", Name: "events"}
	index := pgschema.Index{Table: table.ObjectID(), Name: "opaque_idx", Method: "btree", Parts: []pgschema.IndexPart{{Expression: "public.opaque_value()"}}}
	for _, fixture := range []struct {
		snapshot  *pgschema.Snapshot
		extension pgschema.Extension
	}{{current, before}, {desired, after}} {
		for _, object := range []pgschema.Object{fixture.extension, table, index} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.snapshot.AddDependency(index.ObjectID(), fixture.extension.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || !hasUnsupportedPrefix(result, "extension_upgrade_retained_dependent:"+index.ObjectID().String()) {
		t.Fatalf("opaque extension upgrade retained dependent: %#v", result)
	}
}

func TestBuildPromotesNewRoutineDependentBehindContractReplacement(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "measurements"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "value", Position: 1, Type: "integer"}
	before := pgschema.Routine{Schema: "public", Name: "score", Signature: "integer", Kind: "function", ReturnType: "integer", Definition: "CREATE OR REPLACE FUNCTION public.score(integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 1'"}
	after := before
	after.Definition = "CREATE OR REPLACE FUNCTION public.score(integer) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT $1 + 2'"
	index := pgschema.Index{Table: table.ObjectID(), Name: "score_idx", Method: "btree", Parts: []pgschema.IndexPart{{Expression: "public.score(value)"}}}
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		routine  pgschema.Routine
	}{{current, before}, {desired, after}} {
		for _, object := range []pgschema.Object{table, column, fixture.routine} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desired.Add(index); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(index.ObjectID(), after.ObjectID()); err != nil {
		t.Fatal(err)
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || result.Status != protocol.Planned {
		t.Fatalf("routine replacement with new dependent plan=%#v err=%v", result, err)
	}
	joined := joinSQL(result)
	routineAt, indexAt := strings.Index(joined, "CREATE OR REPLACE FUNCTION"), strings.Index(joined, "CREATE INDEX")
	if routineAt < 0 || indexAt < 0 || routineAt > indexAt {
		t.Fatalf("new dependent must follow routine replacement:\n%s", joined)
	}
	for _, item := range result.Statements {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("routine-dependent work escaped contract: %#v", item)
		}
	}
}

func TestBuildPromotesNewDependentBehindContractEnumMutation(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Enum{Schema: "public", Name: "state", Labels: []string{"old"}}
	after := pgschema.Enum{Schema: "public", Name: "state", Labels: []string{"fresh"}}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	table := pgschema.Table{Schema: "public", Name: "events"}
	defaultValue := "'fresh'::public.state"
	column := pgschema.Column{Table: table.ObjectID(), Name: "state", Position: 1, Type: "public.state", Default: &defaultValue}
	for _, object := range []pgschema.Object{table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(table.ObjectID(), after.ObjectID()); err != nil {
		t.Fatal(err)
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || result.Status != protocol.Planned {
		t.Fatalf("enum mutation with dependent table plan=%#v err=%v", result, err)
	}
	joined := joinSQL(result)
	renameAt, tableAt := strings.Index(joined, "RENAME VALUE"), strings.Index(joined, "CREATE TABLE")
	if renameAt < 0 || tableAt < 0 || renameAt > tableAt {
		t.Fatalf("new enum-dependent table must follow label mutation:\n%s", joined)
	}
	for _, item := range result.Statements {
		if item.Phase != protocol.PhaseContract {
			t.Fatalf("enum-dependent create escaped contract: %#v", item)
		}
	}
}

func TestBuildBlocksUncoordinatedSharedNamespaceKindTransition(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	if err := current.Add(pgschema.Enum{Schema: "public", Name: "status", Labels: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(pgschema.Domain{Schema: "public", Name: "status", BaseType: "text"}); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnsupportedPrefix(result, "shared_namespace_transition:type:public.status") {
		t.Fatalf("shared type namespace transition did not block: %#v", result)
	}
}

func TestBuildBlocksExplicitMultirangeSharedNamespaceTransition(t *testing.T) {
	current := snapshotForTest(t, pgschema.Enum{Schema: "public", Name: "span_multirange", Labels: []string{"old"}})
	desired := snapshotForTest(t, pgschema.Range{Schema: "public", Name: "span", Subtype: "integer", MultirangeName: "span_multirange"})
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || !hasUnsupportedPrefix(result, "shared_namespace_transition:type:public.span_multirange") {
		t.Fatalf("explicit multirange name escaped type namespace guard: %#v", result)
	}
}

func TestBuildOrdersReplicaIdentityAfterIndexRename(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "events"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "integer", NotNull: true}
	oldIndex := pgschema.Index{Table: table.ObjectID(), Name: "events_old_uidx", Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	newIndex := oldIndex
	newIndex.Name = "events_uidx"
	oldIdentity := pgschema.ReplicaIdentity{Table: table.ObjectID(), Mode: pgschema.ReplicaIdentityDefault}
	newIdentity := pgschema.ReplicaIdentity{Table: table.ObjectID(), Mode: pgschema.ReplicaIdentityIndex, Index: ptrID(newIndex.ObjectID())}
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		index    pgschema.Index
		identity pgschema.ReplicaIdentity
	}{{current, oldIndex, oldIdentity}, {desired, newIndex, newIdentity}} {
		for _, object := range []pgschema.Object{table, column, fixture.index, fixture.identity} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desired.AddDependency(newIdentity.ObjectID(), newIndex.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_index" {
		t.Fatalf("index rename question=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_index", Key: oldIndex.ObjectID().String(), Value: newIndex.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil || planned.Status != protocol.Planned {
		t.Fatalf("index rename plan=%#v err=%v", planned, err)
	}
	joined := joinSQL(planned)
	renameAt, identityAt := strings.Index(joined, `ALTER INDEX "public"."events_old_uidx" RENAME TO "events_uidx";`), strings.Index(joined, `REPLICA IDENTITY USING INDEX "events_uidx";`)
	if renameAt < 0 || identityAt < 0 || renameAt > identityAt {
		t.Fatalf("replica identity preceded its renamed provider:\n%s", joined)
	}
}

func TestBuildOrdersNewIndexAfterColumnRenameBridge(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "events"}
	oldColumn := pgschema.Column{Table: table.ObjectID(), Name: "old_value", Position: 1, Type: "integer"}
	newColumn := oldColumn
	newColumn.Name = "value"
	index := pgschema.Index{Table: table.ObjectID(), Name: "events_value_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "value"}}}
	for _, object := range []pgschema.Object{table, oldColumn} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, newColumn, index} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(index.ObjectID(), newColumn.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("column rename question=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{
		{Kind: "rename_column", Key: oldColumn.ObjectID().String(), Value: newColumn.ObjectID().String()},
		{Kind: "rename_backfill_strategy", Key: oldColumn.ObjectID().String(), Value: "single_transaction"},
	}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil || planned.Status != protocol.Planned {
		t.Fatalf("column rename plan=%#v err=%v", planned, err)
	}
	joined := joinSQL(planned)
	renameAt, indexAt := strings.Index(joined, `RENAME COLUMN "old_value" TO "value";`), strings.Index(joined, `CREATE INDEX "events_value_idx"`)
	if renameAt < 0 || indexAt < 0 || renameAt > indexAt {
		t.Fatalf("new index preceded final column identity:\n%s", joined)
	}
}

func TestBuildOrdersTableRenameBeforeNewDependentAndOldSchemaDrop(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	oldSchema, newSchema := pgschema.Schema{Name: "old_app"}, pgschema.Schema{Name: "app"}
	oldTable := pgschema.Table{Schema: oldSchema.Name, Name: "events"}
	newTable := pgschema.Table{Schema: newSchema.Name, Name: "events"}
	oldColumn := pgschema.Column{Table: oldTable.ObjectID(), Name: "id", Position: 1, Type: "integer"}
	newColumn := pgschema.Column{Table: newTable.ObjectID(), Name: "id", Position: 1, Type: "integer"}
	dependent := pgschema.View{Schema: newSchema.Name, Name: "event_ids", Definition: "SELECT id FROM app.events"}
	for _, object := range []pgschema.Object{oldSchema, newSchema, oldTable, oldColumn} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{newSchema, newTable, newColumn, dependent} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldTable.ObjectID(), oldSchema.ObjectID()}, {oldColumn.ObjectID(), oldTable.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{newTable.ObjectID(), newSchema.ObjectID()}, {newColumn.ObjectID(), newTable.ObjectID()}, {dependent.ObjectID(), newTable.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("table rename question=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{
		{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()},
		{Kind: "drop", Key: oldSchema.ObjectID().String(), Value: "drop"},
	}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil || planned.Status != protocol.Planned {
		t.Fatalf("table rename plan=%#v err=%v", planned, err)
	}
	joined := joinSQL(planned)
	moveAt := strings.Index(joined, `ALTER TABLE "old_app"."events" SET SCHEMA "app";`)
	dependentAt := strings.Index(joined, `CREATE VIEW "app"."event_ids"`)
	dropAt := strings.Index(joined, `DROP SCHEMA "old_app" CASCADE;`)
	if moveAt < 0 || dependentAt < 0 || dropAt < 0 || moveAt > dependentAt || moveAt > dropAt {
		t.Fatalf("table identity was consumed before its provider existed:\n%s", joined)
	}
}

func TestBuildRequiresAuthorizationAnswersForPolicyAndGrantOptionContractions(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	oldUsing, newUsing := "tenant_id > 0", "tenant_id > 10"
	oldPolicy := pgschema.Policy{Table: table.ObjectID(), Name: "tenant", Permissive: true, Command: "SELECT", Roles: []string{"PUBLIC"}, Using: &oldUsing}
	newPolicy := oldPolicy
	newPolicy.Using = &newUsing
	oldRLS := pgschema.RowSecurity{Table: table.ObjectID(), Enabled: true, Forced: true}
	newRLS := pgschema.RowSecurity{Table: table.ObjectID(), Enabled: true, Forced: false}
	oldPrivilege := pgschema.TablePrivilege{Table: table.ObjectID(), Grantee: "reader", Grantor: "@owner", Privilege: "SELECT", Grantable: true}
	newPrivilege := oldPrivilege
	newPrivilege.Grantable = false
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{oldPolicy, oldRLS, oldPrivilege} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{newPolicy, newRLS, newPrivilege} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 3 {
		t.Fatalf("expected three authorization questions: %#v", pending)
	}
	values := map[string]string{"alter_policy": "alter", "relax_row_security": "relax", "revoke_grant_option": "revoke"}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: values[question.Kind], QuestionFingerprint: question.ScopeFingerprint})
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(planned)
	for _, fragment := range []string{"ALTER POLICY", "NO FORCE ROW LEVEL SECURITY", "REVOKE GRANT OPTION FOR SELECT"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("plan missing %q:\n%s", fragment, joined)
		}
	}
}

func TestBuildRequiresPolicyReplacementIntentAndRejectsInvalidExpressions(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	using := "tenant_id > 0"
	before := pgschema.Policy{Table: table.ObjectID(), Name: "tenant", Permissive: true, Command: "ALL", Roles: []string{"PUBLIC"}, Using: &using}
	after := before
	after.Permissive = false
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
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "replace_policy" {
		t.Fatalf("policy replacement must ask: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "replace_policy", Key: pending.Questions[0].Key, Value: "replace", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil || planned.Status != protocol.Planned || strings.Index(joinSQL(planned), "DROP POLICY") > strings.Index(joinSQL(planned), "CREATE POLICY") {
		t.Fatalf("policy replacement plan=%#v err=%v", planned, err)
	}

	invalid := pgschema.New()
	check := "true"
	bad := pgschema.Policy{Table: table.ObjectID(), Name: "bad", Permissive: true, Command: "SELECT", Roles: []string{"PUBLIC"}, Check: &check}
	if err := invalid.Add(table); err != nil {
		t.Fatal(err)
	}
	if err := invalid.Add(bad); err != nil {
		t.Fatal(err)
	}
	result, err := Build(pgschema.New(), invalid, protocol.Answers{}, Options{})
	if err != nil || result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || !strings.HasPrefix(result.Unsupported[0], "policy_check_not_allowed:") {
		t.Fatalf("invalid policy expression must be unsupported: plan=%#v err=%v", result, err)
	}
}

func TestBuildRequiresFingerprintBoundDestructiveAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	if err := current.Add(table); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected destructive question, got %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "drop", Key: table.ObjectID().String(), Value: "drop"}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].Phase != "contract" {
		t.Fatalf("expected approved contract drop, got %#v", planned)
	}
}

func TestBuildRendersApprovedExtensionDrop(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	extension := pgschema.Extension{Schema: "public", Name: "pgcrypto", Version: "1.3"}
	if err := current.Add(extension); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected extension drop confirmation, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: extension.ObjectID().String(), Value: "drop"}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `DROP EXTENSION "pgcrypto";` {
		t.Fatalf("extension drop was not rendered: %#v", planned)
	}
}

func TestBuildRendersExtensionCreate(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	extension := pgschema.Extension{Schema: "public", Name: "pgcrypto", Version: "1.3"}
	if err := desired.Add(extension); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `CREATE EXTENSION "pgcrypto" WITH SCHEMA "public" VERSION '1.3';` {
		t.Fatalf("extension create was not rendered: %#v", planned)
	}
}

func TestBuildRendersExtensionVersionUpdate(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Extension{Schema: "public", Name: "pgcrypto", Version: "1.2"}
	after := before
	after.Version = "1.3"
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `ALTER EXTENSION "pgcrypto" UPDATE TO '1.3';` {
		t.Fatalf("extension update was not rendered: %#v", planned)
	}
}

func TestBuildSelectivelyIgnoresExtensionVersionUpdates(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Extension{Schema: "public", Name: "pgcrypto", Version: "1.2"}
	after := before
	after.Version = "1.3"
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	ignored, err := Build(current, desired, protocol.Answers{}, Options{IgnoreExtensionVersions: []string{"pgcrypto"}})
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Status != protocol.Planned || len(ignored.Statements) != 0 || ignored.CurrentFingerprint != ignored.DesiredFingerprint {
		t.Fatalf("matching extension version ignore was not honored: %#v", ignored)
	}
	if equivalent, err := Equivalent(current, desired, Options{IgnoreExtensionVersions: []string{"pgcrypto"}}); err != nil || !equivalent {
		t.Fatalf("matching extension version ignore was not planner-equivalent: %v, %v", equivalent, err)
	}
	nonMatching, err := Build(current, desired, protocol.Answers{}, Options{IgnoreExtensionVersions: []string{"some_other_extension"}})
	if err != nil {
		t.Fatal(err)
	}
	if nonMatching.Status != protocol.Planned || len(nonMatching.Statements) != 1 || nonMatching.CurrentFingerprint == nonMatching.DesiredFingerprint || nonMatching.Statements[0].SQL != `ALTER EXTENSION "pgcrypto" UPDATE TO '1.3';` {
		t.Fatalf("nonmatching extension version ignore suppressed the update: %#v", nonMatching)
	}
	if equivalent, err := Equivalent(current, desired, Options{IgnoreExtensionVersions: []string{"some_other_extension"}}); err != nil || equivalent {
		t.Fatalf("nonmatching extension version ignore became planner-equivalent: %v, %v", equivalent, err)
	}
}

func TestBuildRendersExtensionSchemaMove(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Extension{Schema: "old_schema", Name: "pgcrypto", Version: "1.3"}
	after := before
	after.Schema = "new_schema"
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `ALTER EXTENSION "pgcrypto" SET SCHEMA "new_schema";` {
		t.Fatalf("extension schema move was not rendered: %#v", planned)
	}
}

func TestBuildRendersSequenceParameterUpdate(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Sequence{Schema: "app", Name: "orders_seq", Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 1}
	after := before
	after.Start, after.Increment, after.Cache, after.Cycle = 10, 5, 4, true
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := `ALTER SEQUENCE "app"."orders_seq" AS bigint START WITH 10 INCREMENT BY 5 MINVALUE 1 MAXVALUE 100 CACHE 4 CYCLE;`
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != want {
		t.Fatalf("sequence update was not rendered: %#v", planned)
	}
}

func TestBuildRendersTablePersistenceChange(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Table{Schema: "app", Name: "orders"}
	after := before
	after.Unlogged = true
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `ALTER TABLE "app"."orders" SET UNLOGGED;` {
		t.Fatalf("table persistence change was not rendered: %#v", planned)
	}
}

func TestBuildRendersUnloggedTableCreate(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders", Unlogged: true}
	if err := desired.Add(table); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `CREATE UNLOGGED TABLE "app"."orders" ();` {
		t.Fatalf("unlogged table create was not rendered: %#v", planned)
	}
}

func TestBuildRendersPartitionAttachDetachAndRejectsReconfiguration(t *testing.T) {
	parent := pgschema.Table{Schema: "app", Name: "events"}.ObjectID()
	base := pgschema.Table{Schema: "app", Name: "events_2026"}
	attached := base
	attached.PartitionOf = &pgschema.PartitionOf{Parent: parent, Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}
	attach, _, unsupported, err := renderModify(base, attached, decisions{}, Options{}, nil, nil)
	if err != nil || len(unsupported) != 0 || len(attach) != 1 || attach[0].SQL != `ALTER TABLE "app"."events" ATTACH PARTITION "app"."events_2026" FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');` || attach[0].Phase != protocol.PhaseContract {
		t.Fatalf("partition attach = %#v, unsupported=%#v, err=%v", attach, unsupported, err)
	}
	detach, _, unsupported, err := renderModify(attached, base, decisions{}, Options{}, nil, nil)
	if err != nil || len(unsupported) != 0 || len(detach) != 1 || detach[0].SQL != `ALTER TABLE "app"."events" DETACH PARTITION "app"."events_2026";` || detach[0].Phase != "contract" {
		t.Fatalf("partition detach = %#v, unsupported=%#v, err=%v", detach, unsupported, err)
	}
	moved := attached
	moved.PartitionOf = &pgschema.PartitionOf{Parent: parent, Bound: "FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')"}
	_, _, unsupported, err = renderModify(attached, moved, decisions{}, Options{}, nil, nil)
	if err != nil || len(unsupported) != 1 || unsupported[0] != "partition_reconfiguration:table:app:events_2026" {
		t.Fatalf("partition reconfiguration = %#v, err=%v", unsupported, err)
	}
}

func TestBuildRequiresFingerprintBoundManualContractForPartitionReconfiguration(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	parent := pgschema.Table{Schema: "app", Name: "events"}
	before := pgschema.Table{Schema: "app", Name: "events_2026", PartitionOf: &pgschema.PartitionOf{Parent: parent.ObjectID(), Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}}
	after := before
	after.PartitionOf = &pgschema.PartitionOf{Parent: parent.ObjectID(), Bound: "FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(parent); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	needed, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if needed.Status != protocol.NeedsInput || len(needed.Questions) != 1 || needed.Questions[0].Kind != "partition_reconfiguration" {
		t.Fatalf("expected a manual-contract question, got %#v", needed)
	}
	if len(needed.Guidance) != 1 || needed.Guidance[0].Kind != "partition_reconfiguration" {
		t.Fatalf("expected a bounded partition runbook with the question, got %#v", needed.Guidance)
	}
	boundRunbook := guidanceSQL(needed.Guidance[0])
	for _, fragment := range []string{"ADD CONSTRAINT \"onwardpg_bound_", "<new-bound-predicate>", "VALIDATE CONSTRAINT", "DETACH PARTITION", "ATTACH PARTITION", "onwardpg_catalog_bound_matches", "later fingerprint-bound onwardpg plan"} {
		if !strings.Contains(boundRunbook, fragment) {
			t.Fatalf("partition-bound runbook omitted %q:\n%s", fragment, boundRunbook)
		}
	}
	answers := protocol.Answers{CurrentFingerprint: needed.CurrentFingerprint, DesiredFingerprint: needed.DesiredFingerprint, Answers: []protocol.Answer{{
		Kind: "partition_reconfiguration", Key: before.ObjectID().String(), Value: "provided",
		Manual: &protocol.ManualWork{Summary: "move the partition window after checking data", ExecutionMode: "non_transactional", Statements: []string{
			`ALTER TABLE "app"."events" DETACH PARTITION "app"."events_2026";`,
			`ALTER TABLE "app"."events" ATTACH PARTITION "app"."events_2026" FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');`,
		}, VerificationSQL: []string{`SELECT 1;`}},
	}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].Phase != protocol.PhaseContract || planned.Statements[0].Manual == nil || len(planned.Batches) != 1 || planned.Batches[0].Transactional {
		t.Fatalf("expected a manual planned statement, got %#v", planned)
	}
}

func TestBuildScaffoldsOrdinaryToPartitionedShadowHierarchy(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	root := pgschema.Table{Schema: "app", Name: "events"}
	partitioned := root
	partitioned.Partition = &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (occurred_at)", Parts: []pgschema.PartitionPart{{Column: "occurred_at"}}}
	child := pgschema.Table{Schema: "archive", Name: "events_2026", PartitionOf: &pgschema.PartitionOf{Parent: root.ObjectID(), Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}}
	columns := []pgschema.Column{
		{Table: root.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true, Default: stringPointer(`nextval('app.events_id_seq'::regclass)`)},
		{Table: root.ObjectID(), Name: "occurred_at", Position: 2, Type: "date", NotNull: true},
	}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, pgschema.Schema{Name: "archive"}, root} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range columns {
		if err := current.Add(column); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, pgschema.Schema{Name: "archive"}, partitioned, child} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range columns {
		if err := desired.Add(column); err != nil {
			t.Fatal(err)
		}
	}
	view := pgschema.View{Schema: "app", Name: "event_ids", Definition: `SELECT events.id FROM app.events`}
	routine := pgschema.Routine{Schema: "app", Name: "audit_event", Signature: "", Kind: "function", ReturnType: "trigger", Definition: `CREATE FUNCTION app.audit_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;`}
	trigger := pgschema.Trigger{Table: root.ObjectID(), Name: "events_audit", Routine: routine.ObjectID(), Definition: `CREATE TRIGGER events_audit AFTER INSERT ON app.events FOR EACH ROW EXECUTE FUNCTION app.audit_event()`, Enabled: "O"}
	policyExpression := "id > 0"
	policy := pgschema.Policy{Table: root.ObjectID(), Name: "events_read", Permissive: true, Command: "SELECT", Roles: []string{"PUBLIC"}, Using: &policyExpression}
	rls := pgschema.RowSecurity{Table: root.ObjectID(), Enabled: true}
	privilege := pgschema.TablePrivilege{Table: root.ObjectID(), Grantee: "PUBLIC", Grantor: "@owner", Privilege: "SELECT"}
	sequence := pgschema.Sequence{Schema: "app", Name: "events_id_seq", Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 9223372036854775807, Cache: 1, OwnedBy: ptrID(columns[0].ObjectID())}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{view, routine, trigger, policy, rls, privilege, sequence} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := snapshot.AddDependency(view.ObjectID(), root.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	key := pgschema.Constraint{Table: root.ObjectID(), Name: "events_pkey", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id, occurred_at)"}
	local := pgschema.Index{Table: child.ObjectID(), Name: "events_2026_id_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	for _, object := range []pgschema.Object{key, local} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.NeedsInput || len(result.Guidance) != 1 {
		t.Fatalf("expected partition scaffold, got %#v", result)
	}
	runbook := guidanceSQL(result.Guidance[0])
	for _, fragment := range []string{
		"current partition tree", "desired partition tree", "PARTITION BY RANGE (occurred_at)",
		"temporary_identity_preflight", "reserved partition handoff identity already exists",
		"PARTITION OF \"app\".\"onwardpg_shadow_", "PRIMARY KEY (id, occurred_at)",
		"PROVED: constraint:app:events:events_pkey includes partition columns (occurred_at)",
		"CREATE INDEX \"onwardpg_idx_", "OVERRIDING SYSTEM VALUE", "EXCEPT ALL",
		"onwardpg_catalog_bound_matches", "dual-write/capture behavior", `DROP VIEW "app"."event_ids"`,
		`CREATE VIEW "app"."event_ids"`, `CREATE TRIGGER events_audit`, `CREATE POLICY "events_read"`,
		`ENABLE ROW LEVEL SECURITY`, `GRANT SELECT ON TABLE "app"."events" TO PUBLIC`,
		`ALTER SEQUENCE "app"."events_id_seq" OWNED BY "app"."events"."id"`,
		"separate, current-catalog fingerprint authorization",
	} {
		if !strings.Contains(runbook, fragment) {
			t.Fatalf("ordinary-to-partitioned runbook omitted %q:\n%s", fragment, runbook)
		}
	}
	again, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || !reflect.DeepEqual(result.Guidance, again.Guidance) {
		t.Fatalf("partition temporary identities are not deterministic: first=%#v second=%#v err=%v", result.Guidance, again.Guidance, err)
	}
}

func TestBuildScaffoldsPartitionedToOrdinaryRetainedDataCutover(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	root := pgschema.Table{Schema: "app", Name: "events", Partition: &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (id)", Parts: []pgschema.PartitionPart{{Column: "id"}}}}
	ordinary := root
	ordinary.Partition = nil
	child := pgschema.Table{Schema: "app", Name: "events_low", PartitionOf: &pgschema.PartitionOf{Parent: root.ObjectID(), Bound: "FOR VALUES FROM (0) TO (100)"}}
	column := pgschema.Column{Table: root.ObjectID(), Name: "id", Position: 1, Type: "integer", NotNull: true}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, root, child, column} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, ordinary, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.NeedsInput || len(result.Guidance) != 1 {
		t.Fatalf("expected partitioned-to-ordinary scaffold, got %#v", result)
	}
	runbook := guidanceSQL(result.Guidance[0])
	for _, fragment := range []string{"table:app:events_low", "CREATE TABLE \"app\".\"onwardpg_shadow_", "INSERT INTO", "RENAME TO \"onwardpg_old_", "DROP TABLE \"app\".\"onwardpg_old_", "Never infer production copy duration"} {
		if !strings.Contains(runbook, fragment) {
			t.Fatalf("partitioned-to-ordinary runbook omitted %q:\n%s", fragment, runbook)
		}
	}
}

func guidanceSQL(guidance protocol.Guidance) string {
	parts := make([]string, 0, len(guidance.Steps))
	for _, step := range guidance.Steps {
		parts = append(parts, step.Stage+"\n"+step.SQL)
	}
	return strings.Join(parts, "\n")
}

func TestBuildRejectsPropagatedPartitionIndexAndConstraintAlteration(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	parentTable := pgschema.Table{Schema: "app", Name: "events"}
	childTable := pgschema.Table{Schema: "app", Name: "events_2026", PartitionOf: &pgschema.PartitionOf{Parent: parentTable.ObjectID(), Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}}
	parentIndex := pgschema.Index{Table: parentTable.ObjectID(), Name: "events_id_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	childIndex := pgschema.Index{Table: childTable.ObjectID(), Name: "events_2026_id_idx", Parent: ptrID(parentIndex.ObjectID()), Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	changedIndex := childIndex
	changedIndex.Parts = []pgschema.IndexPart{{Column: "occurred_at"}}
	parentConstraint := pgschema.Constraint{Table: parentTable.ObjectID(), Name: "events_pkey", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)"}
	childConstraint := pgschema.Constraint{Table: childTable.ObjectID(), Name: "events_2026_pkey", Parent: ptrID(parentConstraint.ObjectID()), Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)"}
	changedConstraint := childConstraint
	changedConstraint.Definition = "PRIMARY KEY (occurred_at)"
	for _, object := range []pgschema.Object{parentTable, childTable, parentIndex, childIndex, parentConstraint, childConstraint} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{parentTable, childTable, parentIndex, changedIndex, parentConstraint, changedConstraint} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, "partitioned_index_modify:"+changedIndex.ObjectID().String()) || !containsString(result.Unsupported, "partitioned_constraint_modify:"+changedConstraint.ObjectID().String()) {
		t.Fatalf("expected propagated hierarchy alteration to be unsupported, got %#v", result)
	}
}

func TestBuildChangesMaterializedViewCommentWithoutRebuild(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	beforeComment, afterComment := "old", "new"
	before := pgschema.View{Schema: "app", Name: "order_ids", Materialized: true, Definition: "SELECT 1", Populated: true, Comment: &beforeComment}
	after := before
	after.Comment = &afterComment
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `COMMENT ON MATERIALIZED VIEW "app"."order_ids" IS 'new';` {
		t.Fatalf("materialized-view comment plan = %#v", planned)
	}
}

func TestBuildDoesNotOfferMaterializedViewRenameWhenAnIndexChanges(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.View{Schema: "app", Name: "order_ids", Materialized: true, Definition: "SELECT 1", Populated: true}
	after := pgschema.View{Schema: "app", Name: "reporting_order_ids", Materialized: true, Definition: "SELECT 1", Populated: true}
	beforeIndex := pgschema.Index{Table: before.ObjectID(), Name: "order_ids_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	afterIndex := pgschema.Index{Table: after.ObjectID(), Name: "order_ids_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "other_id"}}}
	for _, object := range []pgschema.Object{before, beforeIndex} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{after, afterIndex} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput {
		t.Fatalf("expected destructive review, got %#v", pending)
	}
	for _, question := range pending.Questions {
		if question.Kind == "rename_view" {
			t.Fatalf("changed materialized-view index must not offer rename: %#v", pending)
		}
	}
}

func TestBuildSeparatesConcurrentMaterializedViewRebuildIndex(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.View{Schema: "app", Name: "order_ids", Materialized: true, Definition: "SELECT 1", Populated: true}
	after := before
	after.Definition = "SELECT 2"
	index := pgschema.Index{Table: after.ObjectID(), Name: "order_ids_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	for _, object := range []pgschema.Object{after, index} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rebuild_materialized_view" {
		t.Fatalf("expected rebuild question, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rebuild_materialized_view", Key: after.ObjectID().String(), Value: "rebuild"}}}
	planned, err := Build(current, desired, answers, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Batches) < 2 {
		t.Fatalf("expected split materialized-view rebuild batches, got %#v", planned)
	}
	last := planned.Batches[len(planned.Batches)-1]
	if last.Transactional || len(last.Statements) != 1 || !strings.Contains(last.Statements[0].SQL, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("expected non-transactional concurrent materialized-view index batch, got %#v", last)
	}
}

func TestBuildDefaultsToTransactionalQualifiedBatchesAndStructuredUnsupported(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	for _, object := range []pgschema.Object{schema, table} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Batches) != 1 || !result.Batches[0].Transactional || !strings.Contains(joinSQL(result), `CREATE TABLE "app"."orders"`) {
		t.Fatalf("unexpected default transactional qualified plan %#v", result)
	}

	unsupported := pgschema.New()
	if err := unsupported.Add(pgschema.Routine{Schema: "app", Name: "orders_routine", Signature: "", Definition: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	result, err = Build(current, unsupported, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || result.Unsupported[0] != "routine_render:routine:app:orders_routine" {
		t.Fatalf("expected structured unsupported result %#v", result)
	}
	answers := protocol.Answers{
		CurrentFingerprint: result.CurrentFingerprint,
		DesiredFingerprint: result.DesiredFingerprint,
		Answers: []protocol.Answer{{
			Kind: "rename_table", Key: "table:app:old->table:app:new", Value: "rename",
		}},
	}
	if _, err := Build(current, unsupported, answers, Options{}); err == nil || !strings.Contains(err.Error(), "unused answer") {
		t.Fatalf("unsupported planning must reject unused answers, got %v", err)
	}
}

func TestBuildUnsortedDumpPreservesCallerOrder(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	for _, object := range []pgschema.Object{schema, table} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{UnsortedDump: true, UnsortedOrder: []pgschema.ID{table.ObjectID(), schema.ObjectID()}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || joinSQL(result) != "CREATE TABLE \"app\".\"orders\" ();\nCREATE SCHEMA \"app\";" {
		t.Fatalf("unsorted dump order was not preserved: %#v", result)
	}
	for _, statement := range result.Statements {
		if statement.Safety != "manual" || !containsString(statement.Hazards, "unsorted_dump_order") {
			t.Fatalf("unsorted dump statement must be explicit manual review work: %#v", statement)
		}
	}
}

func TestBuildUnsortedDumpRejectsIncompleteOrderAndMutation(t *testing.T) {
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	desired := pgschema.New()
	for _, object := range []pgschema.Object{schema, table} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Build(pgschema.New(), desired, protocol.Answers{}, Options{UnsortedDump: true, UnsortedOrder: []pgschema.ID{schema.ObjectID()}}); err == nil {
		t.Fatal("expected incomplete unsorted dump order rejection")
	}
	current := pgschema.New()
	if err := current.Add(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(current, desired, protocol.Answers{}, Options{UnsortedDump: true, UnsortedOrder: desired.IDs()}); err == nil {
		t.Fatal("expected unsorted dump mutation rejection")
	}
}

func TestBuildRefusesSemanticDefaultSuppressionWithoutFingerprintNormalization(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	beforeDefault, afterDefault := "1 + 1", "2"
	before := pgschema.Column{Table: table.ObjectID(), Name: "value", Type: "integer", Default: &beforeDefault}
	after := before
	after.Default = &afterDefault
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
	result, err := Build(current, desired, protocol.Answers{}, Options{DefaultEquivalent: func(left, right string) (bool, error) {
		if left != beforeDefault || right != afterDefault {
			t.Fatalf("comparator arguments = %q, %q", left, right)
		}
		return true, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnsupportedPrefix(result, "default_equivalence_not_fingerprintable") {
		t.Fatalf("equivalent defaults must not create a fingerprint-inconsistent empty plan: %#v", result)
	}
}

func TestBuildSchemaQualifierOverridesSingleSchemaAndPreservesLiterals(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	enum := pgschema.Enum{Schema: "app", Name: "state", Labels: []string{"open"}}
	table := pgschema.Table{Schema: "app", Name: "orders", Comment: stringPointer("app.orders is documentation")}
	column := pgschema.Column{Table: table.ObjectID(), Name: "state", Position: 1, Type: "app.state"}
	for _, object := range []pgschema.Object{enum, table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(table.ObjectID(), enum.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	qualifier := "tenant_42"
	plan, err := Build(current, desired, protocol.Answers{}, Options{SchemaQualifier: &qualifier})
	if err != nil {
		t.Fatal(err)
	}
	got := joinPlan(plan)
	if strings.Contains(got, `"app".`) || strings.Contains(got, "app.state") {
		t.Fatalf("schema qualifier did not replace catalog schema: %s", got)
	}
	for _, want := range []string{`CREATE TYPE "tenant_42"."state"`, `CREATE TABLE "tenant_42"."orders"`, `"state" "tenant_42".state`, `'app.orders is documentation'`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in qualified plan: %s", want, got)
		}
	}
}

func TestBuildSchemaQualifierEmptyEmitsUnqualifiedNames(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	for _, object := range []pgschema.Object{table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	empty := ""
	plan, err := Build(current, desired, protocol.Answers{}, Options{SchemaQualifier: &empty})
	if err != nil {
		t.Fatal(err)
	}
	got := joinPlan(plan)
	if strings.Contains(got, `"app".`) || !strings.Contains(got, `CREATE TABLE "orders"`) {
		t.Fatalf("expected unqualified plan, got: %s", got)
	}
}

func TestBuildSchemaQualifierRejectsSchemaDDLAndMultiSchemaChanges(t *testing.T) {
	qualifier := "tenant"
	t.Run("schema ddl", func(t *testing.T) {
		_, err := Build(pgschema.New(), snapshotForTest(t, pgschema.Schema{Name: "app"}), protocol.Answers{}, Options{SchemaQualifier: &qualifier})
		if err == nil || !strings.Contains(err.Error(), "does not allow schema changes") {
			t.Fatalf("expected scoped schema DDL error, got %v", err)
		}
	})
	t.Run("multiple schemas", func(t *testing.T) {
		desired := snapshotForTest(t, pgschema.Table{Schema: "app", Name: "orders"}, pgschema.Table{Schema: "audit", Name: "events"})
		_, err := Build(pgschema.New(), desired, protocol.Answers{}, Options{SchemaQualifier: &qualifier})
		if err == nil || !strings.Contains(err.Error(), "found 2 schemas") {
			t.Fatalf("expected scoped multi-schema error, got %v", err)
		}
	})
}

func snapshotForTest(t *testing.T, objects ...pgschema.Object) *pgschema.Snapshot {
	t.Helper()
	snapshot := pgschema.New()
	for _, object := range objects {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func TestBuildOrdersChangedDefaultAfterColumnType(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	oldDefault, newDefault := "0", "'abc'"
	before := pgschema.Column{Table: table.ObjectID(), Name: "value", Type: "integer", Default: &oldDefault}
	after := before
	after.Type, after.Default = "text", &newDefault
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
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || !reflect.DeepEqual(pending.Questions[0].Choices, []string{"manual_sql", "split_plan"}) || pending.Questions[0].AllowsFreeform {
		t.Fatalf("type change must expose the one-deployment handoff or split-plan boundary: %#v", pending)
	}
	answer := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "type_change", Key: after.ObjectID().String(), Value: "manual_sql"}}}
	plan, err := Build(current, desired, answer, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	contractTODO := strings.LastIndex(joined, "ONWARDPG TODO")
	dropDefault := strings.Index(joined, `ALTER COLUMN "value" DROP DEFAULT`)
	analyze := strings.Index(joined, `ANALYZE "app"."orders" ("value")`)
	setDefault := strings.Index(joined, `ALTER COLUMN "value" SET DEFAULT 'abc'`)
	if plan.Status != protocol.NeedsSQLEdits || strings.Count(joined, "ONWARDPG TODO") != 2 ||
		dropDefault < 0 || contractTODO < dropDefault || analyze < contractTODO || setDefault < analyze {
		t.Fatalf("editable type handoff must precede the new default: %#v\n%s", plan, joined)
	}
}

func TestBuildReusesPreservedValidatedNotNullCheck(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	before := pgschema.Column{Table: table.ObjectID(), Name: "amount", Position: 1, Type: "bigint"}
	after := before
	after.NotNull = true
	check := pgschema.Constraint{
		Table: table.ObjectID(), Name: "orders_amount_nn", Type: pgschema.ConstraintCheck,
		Definition: `CHECK ((amount IS NOT NULL))`, Validated: true,
	}
	for index, snapshot := range []*pgschema.Snapshot{current, desired} {
		column := before
		if index == 1 {
			column = after
		}
		for _, object := range []pgschema.Object{table, column, check} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := snapshot.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(check.ObjectID(), column.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("NOT NULL reuse decision=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "set_not_null", Key: after.ObjectID().String(), Value: "staged", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "SET NOT NULL") != 1 || strings.Contains(joined, "ADD CONSTRAINT") || strings.Contains(joined, "VALIDATE CONSTRAINT") || strings.Contains(joined, "DROP CONSTRAINT") {
		t.Fatalf("preserved validated check was not reused: %#v\n%s", plan, joined)
	}
	if len(plan.Statements) != 1 || !containsString(plan.Statements[0].Hazards, "validated_check_reuse:orders_amount_nn") || containsString(plan.Statements[0].Hazards, "table_scan") {
		t.Fatalf("validated-check reuse hazards=%#v", plan.Statements)
	}
}

func TestSimpleNotNullCheckRecognizerIsNarrow(t *testing.T) {
	for _, definition := range []string{
		`CHECK ((amount IS NOT NULL))`,
		`check ("amount" is not null)`,
		`CHECK ((("quoted amount" IS NOT NULL))) NOT VALID`,
	} {
		name := "amount"
		if strings.Contains(definition, "quoted amount") {
			name = "quoted amount"
		}
		if !isSimpleNotNullCheck(definition, name) {
			t.Fatalf("did not recognize %q for %q", definition, name)
		}
	}
	for _, definition := range []string{
		`CHECK ((amount > 0))`,
		`CHECK ((amount IS NOT NULL) AND (amount > 0))`,
		`CHECK ((other IS NOT NULL))`,
	} {
		if isSimpleNotNullCheck(definition, "amount") {
			t.Fatalf("recognized non-equivalent check %q", definition)
		}
	}
}

func TestApprovedSchemaDropConsumesDescendants(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	for _, object := range []pgschema.Object{schema, table, column} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := current.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Questions) != 1 || pending.Questions[0].Key != schema.ObjectID().String() {
		t.Fatalf("expected only schema question, got %#v", pending.Questions)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "drop", Key: schema.ObjectID().String(), Value: "drop"}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Statements) != 1 || planned.Statements[0].SQL != `DROP SCHEMA "app" CASCADE;` {
		t.Fatalf("descendant drops were not consumed: %#v", planned.Statements)
	}
}

func TestBuildRequiresAndRendersColumnMutationChoices(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	before := pgschema.Column{Table: table.ObjectID(), Name: "age", Position: 1, Type: "text"}
	after := pgschema.Column{Table: table.ObjectID(), Name: "age", Position: 1, Type: "integer", NotNull: true}
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
		if err := snapshot.AddDependency((pgschema.Column{Table: table.ObjectID(), Name: "age"}).ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 2 {
		t.Fatalf("expected type and NOT NULL questions, got %#v", pending)
	}
	if !reflect.DeepEqual(pending.Questions[0].Choices, []string{"manual_sql", "split_plan"}) || pending.Questions[0].AllowsFreeform {
		t.Fatalf("type-change choices contain an unusable shortcut: %#v", pending.Questions[0])
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			{Kind: "type_change", Key: after.ObjectID().String(), Value: "manual_sql"},
			{Kind: "set_not_null", Key: after.ObjectID().String(), Value: "staged"},
		},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.NeedsSQLEdits || strings.Count(joinSQL(planned), "ONWARDPG TODO") != 2 || strings.Contains(joinSQL(planned), "ALTER COLUMN \"age\" TYPE") {
		t.Fatalf("same-name type change must become an editable handoff, not direct SQL: %#v", planned)
	}
	splitAnswers := answers
	splitAnswers.Answers = append([]protocol.Answer(nil), answers.Answers...)
	splitAnswers.Answers[0].Value = "split_plan"
	split, err := Build(current, desired, splitAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if split.Status != protocol.Unsupported || !containsString(split.Unsupported, "two_application_deployments_required:"+after.ObjectID().String()) {
		t.Fatalf("split-plan choice must produce an explicit two-deployment boundary: %#v", split)
	}
	if len(split.Guidance) != 1 || split.Guidance[0].Kind != "split_plan" || split.Guidance[0].Key != after.ObjectID().String() || len(split.Guidance[0].Steps) != 3 {
		t.Fatalf("split-plan choice must include a bounded scaffold: %#v", split.Guidance)
	}
	if scaffold := split.Guidance[0].Steps[0]; scaffold.Stage != "plan_a_expand_scaffold" || !strings.Contains(scaffold.SQL, `ALTER TABLE "public"."orders" ADD COLUMN "onwardpg_next_`) || !strings.HasSuffix(scaffold.SQL, `" integer;`) {
		t.Fatalf("split-plan expand scaffold=%#v", scaffold)
	}
	for _, guidance := range split.Guidance {
		for _, step := range guidance.Steps {
			if strings.Contains(step.SQL, `DROP COLUMN "age"`) {
				t.Fatalf("split-plan guidance scheduled destructive work before a later desired schema: %#v", guidance)
			}
		}
	}
}

func TestBuildStagesNewRequiredColumnWithoutDefault(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "customers"}
	id := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	email := pgschema.Column{Table: table.ObjectID(), Name: "email", Position: 2, Type: "text", NotNull: true}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(id); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(id.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.Add(email); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(email.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}

	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "reconcile_contract" {
		t.Fatalf("required new column must capture a reconciliation decision: %#v", pending)
	}
	plan := buildWithManualReconciliation(t, current, desired, Options{}, `UPDATE "app"."customers" SET "email" = 'unknown-' || id::text WHERE "email" IS NULL;`, `SELECT NOT EXISTS (SELECT 1 FROM "app"."customers" WHERE "email" IS NULL);`)
	if plan.Status != protocol.Planned || len(plan.Statements) != 4 {
		t.Fatalf("required new column staged plan: %#v", plan)
	}
	if plan.Statements[0].Phase != "expand" || plan.Statements[0].SQL != `ALTER TABLE "app"."customers" ADD COLUMN "email" text;` ||
		plan.Statements[1].Phase != protocol.PhaseContract || plan.Statements[1].Manual == nil ||
		!strings.Contains(plan.Statements[2].SQL, "DO $onwardpg$") ||
		plan.Statements[3].Phase != "contract" || plan.Statements[3].SQL != `ALTER TABLE "app"."customers" ALTER COLUMN "email" SET NOT NULL;` {
		t.Fatalf("required column phases = %#v", plan.Statements)
	}
	if strings.Contains(plan.Statements[0].SQL, "NOT NULL") {
		t.Fatalf("expand broke old writers: %s", plan.Statements[0].SQL)
	}
	if !containsString(plan.Statements[0].Hazards, "application_row_shape_change") {
		t.Fatalf("required column addition must expose row-shape compatibility risk: %#v", plan.Statements[0].Hazards)
	}
}

func TestBuildAddsRequiredColumnWithDefaultDirectly(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "customers"}
	id := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	defaultValue := "'pending'::text"
	status := pgschema.Column{Table: table.ObjectID(), Name: "status", Position: 2, Type: "text", NotNull: true, Default: &defaultValue}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(id); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(id.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.Add(status); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(status.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}

	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 1 || !strings.Contains(plan.Statements[0].SQL, `ADD COLUMN "status" text DEFAULT 'pending'::text NOT NULL`) {
		t.Fatalf("required column with retained default should be directly addable: %#v", plan)
	}
	if !containsString(plan.Statements[0].Hazards, "application_row_shape_change") {
		t.Fatalf("defaulted column addition must expose row-shape compatibility risk: %#v", plan.Statements[0].Hazards)
	}
}

func TestBuildStagesApplicationBackfillBeforeNotNullContract(t *testing.T) {
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
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	strategyAnswers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "set_not_null", Key: after.ObjectID().String(), Value: "staged_with_backfill"}},
	}
	manualPending, err := Build(current, desired, strategyAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if manualPending.Status != protocol.NeedsInput || len(manualPending.Questions) != 1 || manualPending.Questions[0].Kind != "backfill_not_null" {
		t.Fatalf("manual pending = %#v", manualPending)
	}
	strategyAnswers.Answers = append(strategyAnswers.Answers, protocol.Answer{
		Kind: "backfill_not_null", Key: after.ObjectID().String(), Value: "provided",
		Manual: &protocol.ManualWork{
			Summary: "fill missing event dates", ExecutionMode: "transactional",
			Statements:      []string{`UPDATE "public"."events" SET "occurred_on" = CURRENT_DATE WHERE "occurred_on" IS NULL;`},
			VerificationSQL: []string{`SELECT count(*) = 0 FROM "public"."events" WHERE "occurred_on" IS NULL;`},
		},
	})
	planned, err := Build(current, desired, strategyAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Batches) != 3 {
		t.Fatalf("planned = %#v", planned)
	}
	if planned.Batches[0].Phase != protocol.PhaseContract || planned.Batches[1].Phase != protocol.PhaseContract || planned.Batches[2].Phase != protocol.PhaseContract {
		t.Fatalf("phase order = %#v", planned.Batches)
	}
	if !strings.Contains(planned.Batches[0].Statements[0].SQL, "NOT VALID") || planned.Batches[1].Statements[0].Manual == nil ||
		!strings.Contains(planned.Batches[2].Statements[0].SQL, "DO $onwardpg$") || !strings.Contains(joinSQL(planned), "VALIDATE CONSTRAINT") {
		t.Fatalf("backfill/contract = %#v", planned.Batches)
	}
}

func TestBuildRequiresFingerprintBoundIndexRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	before := pgschema.Index{Table: table.ObjectID(), Name: "orders_old_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}, Definition: "CREATE INDEX orders_old_idx ON public.orders USING btree (id)"}
	after := before
	after.Name, after.Definition = "orders_new_idx", "CREATE INDEX orders_new_idx ON public.orders USING btree (id)"
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		indexID := before.ObjectID()
		if snapshot == desired {
			indexID = after.ObjectID()
		}
		if err := snapshot.AddDependency(indexID, table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_index" {
		t.Fatalf("expected index rename question, got %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_index", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `ALTER INDEX "public"."orders_old_idx" RENAME TO "orders_new_idx";` {
		t.Fatalf("unexpected rename plan %#v", planned)
	}
}

func TestBuildRequiresFingerprintBoundViewRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.View{Schema: "app", Name: "orders_view", Definition: " SELECT 1\n"}
	after := pgschema.View{Schema: "app", Name: "reporting_orders", Definition: " SELECT 1\n"}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_view" {
		t.Fatalf("expected view rename question, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.NeedsInput || len(planned.Questions) != 1 || planned.Questions[0].Kind != "rename_compatibility_bridge" {
		t.Fatalf("view rename must request an editable compatibility bridge: %#v", planned)
	}
}

func TestBuildPlansFingerprintBoundViewRenameWithDependentRewrite(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	oldView := pgschema.View{Schema: "app", Name: "orders_view", Definition: "SELECT 1"}
	newView := pgschema.View{Schema: "app", Name: "reporting_orders", Definition: "SELECT 1"}
	oldDependent := pgschema.View{Schema: "app", Name: "dashboard", Definition: "SELECT * FROM orders_view"}
	newDependent := pgschema.View{Schema: "app", Name: "dashboard", Definition: "SELECT * FROM reporting_orders"}
	for _, object := range []pgschema.Object{oldView, oldDependent} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddDependency(oldDependent.ObjectID(), oldView.ObjectID()); err != nil {
		t.Fatal(err)
	}
	for _, object := range []pgschema.Object{newView, newDependent} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(newDependent.ObjectID(), newView.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_view" {
		t.Fatalf("dependent view must require a rename answer: %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: oldView.ObjectID().String(), Value: newView.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.NeedsInput || len(planned.Questions) != 1 || planned.Questions[0].Kind != "rename_compatibility_bridge" {
		t.Fatalf("dependent view rename must request an editable compatibility bridge: %#v", planned)
	}
}

func TestBuildDoesNotOfferViewRenameWithMaterializedDependent(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	oldView := pgschema.View{Schema: "app", Name: "orders_view", Definition: "SELECT 1"}
	newView := pgschema.View{Schema: "app", Name: "reporting_orders", Definition: "SELECT 1"}
	oldDependent := pgschema.View{Schema: "app", Name: "orders_cache", Materialized: true, Definition: "SELECT * FROM orders_view", Populated: true}
	newDependent := pgschema.View{Schema: "app", Name: "orders_cache", Materialized: true, Definition: "SELECT * FROM reporting_orders", Populated: true}
	for _, object := range []pgschema.Object{oldView, oldDependent} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddDependency(oldDependent.ObjectID(), oldView.ObjectID()); err != nil {
		t.Fatal(err)
	}
	for _, object := range []pgschema.Object{newView, newDependent} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(newDependent.ObjectID(), newView.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range pending.Questions {
		if question.Kind == "rename_view" {
			t.Fatalf("materialized dependent must keep rename conservative: %#v", pending)
		}
	}
}

func TestNormalizeViewRelationReferenceDoesNotRewriteLiterals(t *testing.T) {
	from := pgschema.View{Schema: "app", Name: "old_cache", Materialized: true}
	to := pgschema.View{Schema: "app", Name: "reporting_cache", Materialized: true}
	definition := "SELECT 'FROM app.old_cache' AS note, app.old_cache(1) AS fn, app.old_cache.id FROM app.old_cache JOIN app.old_cache AS second ON true"
	normalized, changed := normalizeViewRelationReference(definition, from, to)
	if !changed {
		t.Fatal("expected catalog relation references to be normalized")
	}
	want := "SELECT 'FROM app.old_cache' AS note, app.old_cache(1) AS fn, app.reporting_cache.id FROM app.reporting_cache JOIN app.reporting_cache AS second ON true"
	if normalized != want {
		t.Fatalf("normalized definition = %q, want %q", normalized, want)
	}
}

func TestBuildRequiresFingerprintBoundRoutineRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Routine{Schema: "app", Name: "old_total", Signature: "integer", Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.old_total(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1';"}
	after := pgschema.Routine{Schema: "app", Name: "total", Signature: "integer", Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.total(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1';"}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_routine" {
		t.Fatalf("expected routine rename question, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.NeedsInput || len(planned.Questions) != 1 || planned.Questions[0].Kind != "rename_compatibility_bridge" {
		t.Fatalf("routine rename must request an editable compatibility bridge: %#v", planned)
	}
}

func TestBuildPlansRoutineRenameWithDependentTriggerRewrite(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	oldRoutine := pgschema.Routine{Schema: "app", Name: "old_audit", Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.old_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"}
	newRoutine := pgschema.Routine{Schema: "app", Name: "audit", Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"}
	for _, object := range []pgschema.Object{table, oldRoutine, pgschema.Trigger{Table: table.ObjectID(), Name: "audit", Routine: oldRoutine.ObjectID(), Enabled: "O", Definition: "CREATE TRIGGER audit BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.old_audit()"}} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, newRoutine, pgschema.Trigger{Table: table.ObjectID(), Name: "audit", Routine: newRoutine.ObjectID(), Enabled: "O", Definition: "CREATE TRIGGER audit BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.audit()"}} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_routine" {
		t.Fatalf("trigger-dependent routine rename must require an answer: %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: oldRoutine.ObjectID().String(), Value: newRoutine.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.NeedsInput || len(planned.Questions) != 1 || planned.Questions[0].Kind != "rename_compatibility_bridge" {
		t.Fatalf("trigger-dependent routine rename must request an editable compatibility bridge: %#v", planned)
	}
}

func TestBuildChangesRoutineCommentWithoutReplacement(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	beforeComment, afterComment := "old", "new"
	before := pgschema.Routine{Schema: "app", Name: "total", Signature: "integer", Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.total(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1';", Comment: &beforeComment}
	after := before
	after.Comment = &afterComment
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	planned, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].SQL != `COMMENT ON FUNCTION "app"."total"(integer) IS 'new';` {
		t.Fatalf("routine comment plan = %#v", planned)
	}
}

func TestBuildRequiresFingerprintBoundTriggerRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}.ObjectID()
	routine := pgschema.Routine{Schema: "app", Name: "audit", Kind: "function"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(pgschema.Table{Schema: "app", Name: "orders"}); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(routine); err != nil {
			t.Fatal(err)
		}
	}
	before := pgschema.Trigger{Table: table, Name: "audit_old", Routine: routine.ObjectID(), Enabled: "O", Definition: "CREATE TRIGGER audit_old BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.audit()"}
	after := pgschema.Trigger{Table: table, Name: "audit", Routine: routine.ObjectID(), Enabled: "O", Definition: "CREATE TRIGGER audit BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.audit()"}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_trigger" {
		t.Fatalf("expected trigger rename question, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_trigger", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || !strings.Contains(joinSQL(planned), `ALTER TRIGGER "audit_old" ON "app"."orders" RENAME TO "audit";`) {
		t.Fatalf("unexpected trigger rename plan %#v", planned)
	}
}

func TestConstraintTriggerDefinitionAndRenameIdentityAreSupported(t *testing.T) {
	table := (pgschema.Table{Schema: "app", Name: "orders"}).ObjectID()
	routine := (pgschema.Routine{Schema: "app", Name: "enforce_orders", Signature: ""}).ObjectID()
	before := pgschema.Trigger{
		Table: table, Name: "orders_guard_old", Routine: routine, Enabled: "O",
		Definition: `CREATE CONSTRAINT TRIGGER orders_guard_old AFTER INSERT ON app.orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.enforce_orders()`,
	}
	after := before
	after.Name = "orders_guard"
	after.Definition = `CREATE CONSTRAINT TRIGGER orders_guard AFTER INSERT ON app.orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.enforce_orders()`

	if sql, unsupported := renderTriggerDefinition(after); unsupported != "" || !strings.HasPrefix(sql, "CREATE CONSTRAINT TRIGGER ") {
		t.Fatalf("constraint trigger render = %q, %q", sql, unsupported)
	}
	if !equivalentTriggerForRename(before, after) {
		t.Fatalf("constraint trigger names should be removable from canonical identity: before=%q after=%q", triggerDefinitionTail(before.Definition), triggerDefinitionTail(after.Definition))
	}

	changed := after
	changed.Definition = `CREATE CONSTRAINT TRIGGER orders_guard AFTER INSERT ON app.orders NOT DEFERRABLE FOR EACH ROW EXECUTE FUNCTION app.enforce_orders()`
	if equivalentTriggerForRename(before, changed) {
		t.Fatal("constraint-trigger deferrability must remain part of rename identity")
	}
}

func TestViewTriggerDefinitionIsSupported(t *testing.T) {
	view := (pgschema.View{Schema: "app", Name: "orders_v"}).ObjectID()
	routine := (pgschema.Routine{Schema: "app", Name: "change_orders", Signature: ""}).ObjectID()
	trigger := pgschema.Trigger{
		Table: view, Name: "orders_v_insert", Routine: routine, Enabled: "O",
		Definition: `CREATE TRIGGER orders_v_insert INSTEAD OF INSERT ON app.orders_v FOR EACH ROW EXECUTE FUNCTION app.change_orders()`,
	}
	if sql, unsupported := renderTriggerDefinition(trigger); unsupported != "" || !strings.HasPrefix(sql, "CREATE TRIGGER ") {
		t.Fatalf("view trigger render = %q, %q", sql, unsupported)
	}
	invalid := trigger
	invalid.Table = (pgschema.View{Schema: "app", Name: "orders_mv", Materialized: true}).ObjectID()
	if _, unsupported := renderTriggerDefinition(invalid); !strings.HasPrefix(unsupported, "trigger_render:") {
		t.Fatalf("materialized-view trigger should remain invalid, got %q", unsupported)
	}
}

func TestTriggerRecreateRestoresMatchingNonDefaultEnableState(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	routine := pgschema.Routine{Schema: "app", Name: "audit", Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$"}
	before := pgschema.Trigger{Table: table.ObjectID(), Name: "audit", Routine: routine.ObjectID(), Enabled: "D", Definition: "CREATE TRIGGER audit BEFORE INSERT ON app.orders FOR EACH ROW EXECUTE FUNCTION app.audit()"}
	after := before
	after.Definition = "CREATE TRIGGER audit BEFORE UPDATE ON app.orders FOR EACH ROW EXECUTE FUNCTION app.audit()"
	for _, object := range []pgschema.Object{table, routine, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, routine, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(joined, "DROP TRIGGER") || !strings.Contains(joined, "CREATE TRIGGER") || !strings.Contains(joined, "DISABLE TRIGGER") || strings.Index(joined, "CREATE TRIGGER") > strings.Index(joined, "DISABLE TRIGGER") {
		t.Fatalf("trigger recreate did not restore disabled state: %#v", plan)
	}
}

func TestBuildRequiresFingerprintBoundTableRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	oldTable := pgschema.Table{Schema: "public", Name: "orders_old"}
	newTable := pgschema.Table{Schema: "public", Name: "orders_new"}
	oldColumn := pgschema.Column{Table: oldTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	newColumn := pgschema.Column{Table: newTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	for _, object := range []pgschema.Object{schema, oldTable, oldColumn} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, newTable, newColumn} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldTable.ObjectID(), schema.ObjectID()}, {oldColumn.ObjectID(), oldTable.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{newTable.ObjectID(), schema.ObjectID()}, {newColumn.ObjectID(), newTable.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("expected table rename question, got %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 3 ||
		planned.Statements[0].Phase != "expand" || !strings.Contains(planned.Statements[0].SQL, `CREATE VIEW "public"."orders_new" WITH (security_invoker = true)`) ||
		planned.Statements[1].Phase != "contract" || planned.Statements[1].SQL != `DROP VIEW "public"."orders_new";` ||
		planned.Statements[2].Phase != "contract" || planned.Statements[2].SQL != `ALTER TABLE "public"."orders_old" RENAME TO "orders_new";` {
		t.Fatalf("unexpected rename plan %#v", planned)
	}
	if len(planned.Batches) != 2 || planned.Batches[1].Phase != "contract" || !planned.Batches[1].Transactional || len(planned.Batches[1].Statements) != 2 {
		t.Fatalf("view replacement and physical rename must be one atomic contract batch: %#v", planned.Batches)
	}
}

func TestTableRenameOffersDeclarativeDerivedChildNames(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	beforeID := pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true}
	afterID := pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true}
	beforeEmail := pgschema.Column{Table: from.ObjectID(), Name: "email", Position: 2, Type: "text"}
	afterEmail := pgschema.Column{Table: to.ObjectID(), Name: "email", Position: 2, Type: "text"}
	beforePrimary := pgschema.Constraint{Table: from.ObjectID(), Name: "accounts_pkey", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)", UsingIndex: "accounts_pkey", Validated: true}
	afterPrimary := pgschema.Constraint{Table: to.ObjectID(), Name: "customers_pkey", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)", UsingIndex: "customers_pkey", Validated: true}
	beforeUnique := pgschema.Constraint{Table: from.ObjectID(), Name: "accounts_email_key", Type: pgschema.ConstraintUnique, Definition: "UNIQUE (email)", UsingIndex: "accounts_email_key", Validated: true}
	afterUnique := pgschema.Constraint{Table: to.ObjectID(), Name: "customers_email_key", Type: pgschema.ConstraintUnique, Definition: "UNIQUE (email)", UsingIndex: "customers_email_key", Validated: true}
	users := pgschema.Table{Schema: "app", Name: "users"}
	beforeFK := pgschema.Constraint{Table: from.ObjectID(), Name: "accounts_owner_id_fkey", Type: pgschema.ConstraintForeign, Definition: "FOREIGN KEY (owner_id) REFERENCES users(id)", Reference: ptrID(users.ObjectID()), Validated: true}
	afterFK := pgschema.Constraint{Table: to.ObjectID(), Name: "customers_owner_id_fkey", Type: pgschema.ConstraintForeign, Definition: "FOREIGN KEY (owner_id) REFERENCES users(id)", Reference: ptrID(users.ObjectID()), Validated: true}
	beforeIndex := pgschema.Index{Table: from.ObjectID(), Name: "accounts_email_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "email"}}}
	afterIndex := pgschema.Index{Table: to.ObjectID(), Name: "customers_email_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "email"}}}
	for _, object := range []pgschema.Object{schema, users, from, beforeID, beforeEmail, beforePrimary, beforeUnique, beforeFK, beforeIndex} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, users, to, afterID, afterEmail, afterPrimary, afterUnique, afterFK, afterIndex} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{
		{users.ObjectID(), schema.ObjectID()}, {from.ObjectID(), schema.ObjectID()}, {beforeID.ObjectID(), from.ObjectID()}, {beforeEmail.ObjectID(), from.ObjectID()}, {beforePrimary.ObjectID(), from.ObjectID()}, {beforeUnique.ObjectID(), from.ObjectID()}, {beforeFK.ObjectID(), from.ObjectID()}, {beforeIndex.ObjectID(), from.ObjectID()},
	} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{
		{users.ObjectID(), schema.ObjectID()}, {to.ObjectID(), schema.ObjectID()}, {afterID.ObjectID(), to.ObjectID()}, {afterEmail.ObjectID(), to.ObjectID()}, {afterPrimary.ObjectID(), to.ObjectID()}, {afterUnique.ObjectID(), to.ObjectID()}, {afterFK.ObjectID(), to.ObjectID()}, {afterIndex.ObjectID(), to.ObjectID()},
	} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("declarative derived names must retain table rename candidacy: %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: from.ObjectID().String(), Value: to.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql := joinSQL(planned)
	for _, statement := range []string{
		`ALTER TABLE "app"."accounts" RENAME TO "customers";`,
		`ALTER TABLE "app"."customers" RENAME CONSTRAINT "accounts_pkey" TO "customers_pkey";`,
		`ALTER TABLE "app"."customers" RENAME CONSTRAINT "accounts_email_key" TO "customers_email_key";`,
		`ALTER TABLE "app"."customers" RENAME CONSTRAINT "accounts_owner_id_fkey" TO "customers_owner_id_fkey";`,
		`ALTER INDEX "app"."accounts_email_idx" RENAME TO "customers_email_idx";`,
	} {
		if !strings.Contains(sql, statement) {
			t.Fatalf("plan missing %q:\n%s", statement, sql)
		}
	}
}

func TestTableRenameDoesNotAbsorbUserNamedChildRename(t *testing.T) {
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	before := pgschema.Constraint{Table: from.ObjectID(), Name: "billing_identity", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)"}
	after := pgschema.Constraint{Table: to.ObjectID(), Name: "customer_identity", Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)"}
	if equivalentChildForTableRename(before, after, from, to) {
		t.Fatal("user-selected child names must remain a material table-rename difference")
	}
}

func TestBuildExplainsRejectedCredibleTableRename(t *testing.T) {
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
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Analysis) != 1 || result.Analysis[0].Kind != "rename_table" || result.Analysis[0].Outcome != "rejected" || !strings.HasPrefix(result.Analysis[0].Reason, "child_identity_mismatch:") {
		t.Fatalf("expected explained near-miss, got %#v", result.Analysis)
	}
}

func TestTableRenameCompatibilityViewCarriesRuntimePrivileges(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	beforeColumn := pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	afterColumn := pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	beforePrivilege := pgschema.TablePrivilege{Table: from.ObjectID(), Grantee: "app_runtime", Grantor: "@owner", Privilege: "SELECT"}
	afterPrivilege := beforePrivilege
	afterPrivilege.Table = to.ObjectID()
	for _, object := range []pgschema.Object{from, beforeColumn, beforePrivilege} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{to, afterColumn, afterPrivilege} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_table", Key: from.ObjectID().String(), Value: to.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || !strings.Contains(joinSQL(planned), `GRANT SELECT ON TABLE "app"."customers" TO "app_runtime";`) {
		t.Fatalf("compatibility view did not retain runtime privilege: %#v", planned)
	}
}

func TestTableRenameRejectsTableOnlyPrivilegeThatAViewCannotPreserve(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	from := pgschema.Table{Schema: "app", Name: "accounts"}
	to := pgschema.Table{Schema: "app", Name: "customers"}
	beforeColumn := pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	afterColumn := pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	beforePrivilege := pgschema.TablePrivilege{Table: from.ObjectID(), Grantee: "app_runtime", Grantor: "@owner", Privilege: "TRUNCATE"}
	afterPrivilege := beforePrivilege
	afterPrivilege.Table = to.ObjectID()
	for _, object := range []pgschema.Object{from, beforeColumn, beforePrivilege} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{to, afterColumn, afterPrivilege} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_table", Key: from.ObjectID().String(), Value: to.ObjectID().String()}},
	}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "table_rename_compatibility_table_only_privilege:" + beforePrivilege.ObjectID().String()
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, want) {
		t.Fatalf("table-only privilege must block view bridge: %#v", result)
	}
}

func TestBuildOffersEveryCredibleTableRenameCandidate(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	oldTable := pgschema.Table{Schema: "public", Name: "accounts"}
	customers := pgschema.Table{Schema: "public", Name: "customers"}
	prospects := pgschema.Table{Schema: "public", Name: "prospects"}
	oldID := pgschema.Column{Table: oldTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	customerID := pgschema.Column{Table: customers.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	prospectID := pgschema.Column{Table: prospects.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	for _, object := range []pgschema.Object{schema, oldTable, oldID} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, customers, customerID, prospects, prospectID} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldTable.ObjectID(), schema.ObjectID()}, {oldID.ObjectID(), oldTable.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{
		{customers.ObjectID(), schema.ObjectID()}, {customerID.ObjectID(), customers.ObjectID()},
		{prospects.ObjectID(), schema.ObjectID()}, {prospectID.ObjectID(), prospects.ObjectID()},
	} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantChoices := []string{customers.ObjectID().String(), prospects.ObjectID().String(), "create"}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || !reflect.DeepEqual(pending.Questions[0].Choices, wantChoices) {
		t.Fatalf("rename choices = %#v, want %#v", pending.Questions, wantChoices)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: prospects.ObjectID().String(), QuestionFingerprint: pending.Questions[0].ScopeFingerprint}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql := joinSQL(planned)
	if planned.Status != protocol.Planned || !strings.Contains(sql, `CREATE TABLE "public"."customers"`) || !strings.Contains(sql, `ALTER TABLE "public"."accounts" RENAME TO "prospects";`) {
		t.Fatalf("selected rename plan = %#v\n%s", planned, sql)
	}
}

func TestBuildKeepsTableRenameIntentStableWhileAddingColumn(t *testing.T) {
	current, desiredBefore, desiredAfter := pgschema.New(), pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	oldTable := pgschema.Table{Schema: "public", Name: "accounts"}
	newTable := pgschema.Table{Schema: "public", Name: "customers"}
	oldID := pgschema.Column{Table: oldTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	newID := pgschema.Column{Table: newTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	timezone := pgschema.Column{Table: newTable.ObjectID(), Name: "timezone", Position: 2, Type: "text"}
	for _, object := range []pgschema.Object{schema, oldTable, oldID} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, snapshot := range []*pgschema.Snapshot{desiredBefore, desiredAfter} {
		for _, object := range []pgschema.Object{schema, newTable, newID} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desiredAfter.Add(timezone); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]pgschema.ID{{oldTable.ObjectID(), schema.ObjectID()}, {oldID.ObjectID(), oldTable.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, snapshot := range []*pgschema.Snapshot{desiredBefore, desiredAfter} {
		for _, edge := range [][2]pgschema.ID{{newTable.ObjectID(), schema.ObjectID()}, {newID.ObjectID(), newTable.ObjectID()}} {
			if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desiredAfter.AddDependency(timezone.ObjectID(), newTable.ObjectID()); err != nil {
		t.Fatal(err)
	}

	pendingBefore, err := Build(current, desiredBefore, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pendingAfter, err := Build(current, desiredAfter, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pendingBefore.Status != protocol.NeedsInput || pendingAfter.Status != protocol.NeedsInput || len(pendingBefore.Questions) != 1 || len(pendingAfter.Questions) != 1 {
		t.Fatalf("rename questions before=%#v after=%#v", pendingBefore, pendingAfter)
	}
	if pendingBefore.Questions[0].ScopeFingerprint != pendingAfter.Questions[0].ScopeFingerprint {
		t.Fatalf("additive column invalidated rename intent: before=%s after=%s", pendingBefore.Questions[0].ScopeFingerprint, pendingAfter.Questions[0].ScopeFingerprint)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pendingAfter.CurrentFingerprint, DesiredFingerprint: pendingAfter.DesiredFingerprint,
		Answers: []protocol.Answer{{
			Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String(),
			QuestionFingerprint: pendingAfter.Questions[0].ScopeFingerprint,
		}},
	}
	planned, err := Build(current, desiredAfter, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql := joinSQL(planned)
	if planned.Status != protocol.Planned || !strings.Contains(sql, `ALTER TABLE "public"."accounts" ADD COLUMN "timezone" text`) || !strings.Contains(sql, `ALTER TABLE "public"."accounts" RENAME TO "customers"`) {
		t.Fatalf("rename plus additive column plan = %#v\n%s", planned, sql)
	}
}

func TestBuildRejectsTableRenameWhenOldColumnShapeCannotRemainCompatible(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	oldTable := pgschema.Table{Schema: "public", Name: "accounts"}
	newTable := pgschema.Table{Schema: "public", Name: "customers"}
	before := pgschema.Column{Table: oldTable.ObjectID(), Name: "occurred_at", Position: 1, Type: "timestamp without time zone"}
	after := pgschema.Column{Table: newTable.ObjectID(), Name: "occurred_at", Position: 1, Type: "date"}
	for _, object := range []pgschema.Object{schema, oldTable, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, newTable, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldTable.ObjectID(), schema.ObjectID()}, {before.ObjectID(), oldTable.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{newTable.ObjectID(), schema.ObjectID()}, {after.ObjectID(), newTable.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}

	renamePending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if renamePending.Status != protocol.NeedsInput || len(renamePending.Questions) != 1 || renamePending.Questions[0].Kind != "rename_table" {
		t.Fatalf("rename stage = %#v", renamePending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: renamePending.CurrentFingerprint, DesiredFingerprint: renamePending.DesiredFingerprint,
		Answers: []protocol.Answer{{
			Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String(),
			QuestionFingerprint: renamePending.Questions[0].ScopeFingerprint,
		}},
	}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "table_rename_compatibility_column_change:" + before.ObjectID().String()
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, want) {
		t.Fatalf("compound rename/type compatibility result = %#v", result)
	}
}

func TestBuildAllowsConfirmedTableRenameReferencedByForeignKey(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	oldTable := pgschema.Table{Schema: "public", Name: "users"}
	newTable := pgschema.Table{Schema: "public", Name: "accounts"}
	orders := pgschema.Table{Schema: "public", Name: "orders"}
	oldID := pgschema.Column{Table: oldTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	newID := pgschema.Column{Table: newTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	orderUser := pgschema.Column{Table: orders.ObjectID(), Name: "user_id", Position: 1, Type: "bigint"}
	beforeFK := pgschema.Constraint{Table: orders.ObjectID(), Name: "orders_user_fkey", Type: pgschema.ConstraintForeign, Definition: "FOREIGN KEY (user_id) REFERENCES users(id)", Reference: ptrID(oldTable.ObjectID())}
	afterFK := beforeFK
	afterFK.Reference = ptrID(newTable.ObjectID())
	for _, object := range []pgschema.Object{schema, oldTable, orders, oldID, orderUser, beforeFK} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, newTable, orders, newID, orderUser, afterFK} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("expected rename question, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 3 ||
		planned.Statements[0].SQL != "CREATE VIEW \"public\".\"accounts\" WITH (security_invoker = true) AS\nSELECT \"id\" FROM \"public\".\"users\";" ||
		planned.Statements[1].SQL != `DROP VIEW "public"."accounts";` ||
		planned.Statements[2].SQL != `ALTER TABLE "public"."users" RENAME TO "accounts";` {
		t.Fatalf("unexpected FK-safe rename plan %#v", planned)
	}
}

func TestBuildAllowsConfirmedTableRenameWithRetainedTrigger(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	oldTable := pgschema.Table{Schema: "public", Name: "orders_old"}
	newTable := pgschema.Table{Schema: "public", Name: "orders_new"}
	oldID := pgschema.Column{Table: oldTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	newID := pgschema.Column{Table: newTable.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	routine := pgschema.Routine{Schema: "public", Name: "audit", Kind: "function", Signature: "", Definition: "CREATE FUNCTION public.audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"}
	oldTrigger := pgschema.Trigger{Table: oldTable.ObjectID(), Name: "audit", Routine: routine.ObjectID(), Enabled: "O", Definition: "CREATE TRIGGER audit BEFORE INSERT ON public.orders_old FOR EACH ROW EXECUTE FUNCTION public.audit()"}
	newTrigger := oldTrigger
	newTrigger.Table = newTable.ObjectID()
	newTrigger.Definition = "CREATE TRIGGER audit BEFORE INSERT ON public.orders_new FOR EACH ROW EXECUTE FUNCTION public.audit()"
	for _, object := range []pgschema.Object{schema, oldTable, oldID, routine, oldTrigger} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, newTable, newID, routine, newTrigger} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldTable.ObjectID(), schema.ObjectID()}, {oldID.ObjectID(), oldTable.ObjectID()}, {routine.ObjectID(), schema.ObjectID()}, {oldTrigger.ObjectID(), oldTable.ObjectID()}, {oldTrigger.ObjectID(), routine.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{newTable.ObjectID(), schema.ObjectID()}, {newID.ObjectID(), newTable.ObjectID()}, {routine.ObjectID(), schema.ObjectID()}, {newTrigger.ObjectID(), newTable.ObjectID()}, {newTrigger.ObjectID(), routine.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("expected table-rename question, got %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 3 ||
		planned.Statements[0].SQL != "CREATE VIEW \"public\".\"orders_new\" WITH (security_invoker = true) AS\nSELECT \"id\" FROM \"public\".\"orders_old\";" ||
		planned.Statements[1].SQL != `DROP VIEW "public"."orders_new";` ||
		planned.Statements[2].SQL != `ALTER TABLE "public"."orders_old" RENAME TO "orders_new";` {
		t.Fatalf("expected retained-trigger table rename plan, got %#v", planned)
	}
}

func TestTableRenameRejectsUnprovenDependentViewTransition(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	oldTable := pgschema.Table{Schema: "app", Name: "orders_old"}
	newTable := pgschema.Table{Schema: "app", Name: "orders_new"}
	before := pgschema.View{Schema: "app", Name: "order_view", Definition: "SELECT id FROM app.orders_old"}
	after := pgschema.View{Schema: "app", Name: "order_view", Definition: "SELECT id + 1 AS id FROM app.orders_new"}
	for _, object := range []pgschema.Object{oldTable, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{newTable, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddDependency(before.ObjectID(), oldTable.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(after.ObjectID(), newTable.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}}}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "table_rename_dependent_view:" + before.ObjectID().String()
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, want) {
		t.Fatalf("expected structured dependent-view rejection, got %#v", result)
	}
}

func ptrID(id pgschema.ID) *pgschema.ID { return &id }

func TestRenderTableRenameAcrossSchemasUsesValidPostgreSQLMove(t *testing.T) {
	from := pgschema.Table{Schema: "s1", Name: "orders"}
	to := pgschema.Table{Schema: "s2", Name: "orders_v2"}
	statements, err := renderTableRename(renameTable{
		from: from, to: to,
		compatibilityColumns: []pgschema.Column{{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(statements))
	for i, statement := range statements {
		got[i] = statement.SQL
	}
	want := []string{
		"CREATE VIEW \"s2\".\"orders_v2\" WITH (security_invoker = true) AS\nSELECT \"id\" FROM \"s1\".\"orders\";",
		`DROP VIEW "s2"."orders_v2";`,
		`ALTER TABLE "s1"."orders" SET SCHEMA "s2";`,
		`ALTER TABLE "s2"."orders" RENAME TO "orders_v2";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cross-schema rename = %#v, want %#v", got, want)
	}
}

func TestBuildRequiresFingerprintBoundColumnRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	before := pgschema.Column{Table: table.ObjectID(), Name: "old_name", Position: 1, Type: "text"}
	after := before
	after.Name = "new_name"
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		columnID := before.ObjectID()
		if snapshot == desired {
			columnID = after.ObjectID()
		}
		if err := snapshot.AddDependency(columnID, table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("expected column rename question, got %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_column", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	strategyPending, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strategyPending.Status != protocol.NeedsInput || len(strategyPending.Questions) != 1 || strategyPending.Questions[0].Kind != "rename_backfill_strategy" {
		t.Fatalf("rename identity alone must not emit an unbounded backfill: %#v", strategyPending)
	}
	answers.Answers = append(answers.Answers, protocol.Answer{Kind: "rename_backfill_strategy", Key: before.ObjectID().String(), Value: "single_transaction"})
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 10 {
		t.Fatalf("column rename must produce a two-phase compatibility bridge: %#v", planned)
	}
	if planned.Statements[0].Phase != protocol.PhaseExpand || planned.Statements[4].Phase != protocol.PhaseContract {
		t.Fatalf("column rename phases = %#v", planned.Statements)
	}
	joined := joinSQL(planned)
	for _, fragment := range []string{
		`ADD COLUMN "new_name" text`,
		`CREATE FUNCTION "public"."onwardpg_sync_column_`,
		`BEFORE INSERT OR UPDATE OF "old_name", "new_name"`,
		`DROP COLUMN "new_name"`,
		`RENAME COLUMN "old_name" TO "new_name"`,
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("column rename bridge missing %q:\n%s", fragment, joined)
		}
	}
	if !containsString(planned.Statements[3].Hazards, "unbounded_update") || !containsString(planned.Statements[4].Hazards, "unbounded_update") {
		t.Fatalf("single-transaction rename must expose operational backfill risk: %#v", planned.Statements)
	}

	manualAnswers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			{Kind: "rename_column", Key: before.ObjectID().String(), Value: after.ObjectID().String()},
			{Kind: "rename_backfill_strategy", Key: before.ObjectID().String(), Value: "manual_sql"},
		},
	}
	manualPending, err := Build(current, desired, manualAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if manualPending.Status != protocol.NeedsInput || len(manualPending.Questions) != 1 || manualPending.Questions[0].Kind != "rename_backfill" {
		t.Fatalf("manual rename strategy must request verifiable work: %#v", manualPending)
	}
	manualAnswers.Answers = append(manualAnswers.Answers, protocol.Answer{
		Kind: "rename_backfill", Key: before.ObjectID().String(), Value: "provided",
		Manual: &protocol.ManualWork{
			Summary: "backfill one reviewed key range", ExecutionMode: "transactional",
			Statements:      []string{`UPDATE "public"."orders" SET "new_name" = "old_name" WHERE "new_name" IS DISTINCT FROM "old_name" AND ctid IN (SELECT ctid FROM "public"."orders" LIMIT 1000);`},
			VerificationSQL: []string{`SELECT count(*) = 0 FROM "public"."orders" WHERE "new_name" IS DISTINCT FROM "old_name";`},
		},
	})
	manualPlan, err := Build(current, desired, manualAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if manualPlan.Status != protocol.Planned || manualPlan.Statements[3].Manual == nil || manualPlan.Statements[3].Phase != protocol.PhaseExpand {
		t.Fatalf("manual rename backfill plan = %#v", manualPlan)
	}
	for _, statement := range manualPlan.Statements {
		if statement.Manual == nil && strings.HasPrefix(statement.SQL, `UPDATE "public"."orders" SET "new_name" = "old_name"`) {
			t.Fatalf("manual strategy emitted an implicit full-table update: %#v", statement)
		}
	}
	equalityGate := gateByKind(t, manualPlan.ContractGates, "data_assertion")
	if !strings.Contains(equalityGate.BooleanSQL, `"new_name" IS DISTINCT FROM "old_name"`) {
		t.Fatalf("rename equality gate = %#v", equalityGate)
	}
	for _, statement := range manualPlan.Statements {
		if statement.Phase == protocol.PhaseContract && !containsString(statement.Hazards, "final_overlap_catchup") && !containsString(statement.RequiresGates, equalityGate.ID) {
			t.Fatalf("rename contract statement lacks equality gate: %#v", statement)
		}
	}

	batchedAnswers := manualAnswers
	batchedAnswers.Answers = append([]protocol.Answer(nil), manualAnswers.Answers...)
	batchedAnswers.Answers[len(batchedAnswers.Answers)-1].Manual = &protocol.ManualWork{
		Summary: "backfill bounded primary-key windows", ExecutionMode: protocol.ManualOperatorBatched,
		Statements:      []string{`UPDATE "public"."orders" SET "new_name" = "old_name" WHERE id > :after_id AND id <= :through_id AND "new_name" IS DISTINCT FROM "old_name";`},
		VerificationSQL: []string{`SELECT count(*) = 0 FROM "public"."orders" WHERE "new_name" IS DISTINCT FROM "old_name";`},
		ProgressKey:     "orders.id", IdempotencyNotes: "rerunning a window writes the same source value",
	}
	batchedPlan, err := Build(current, desired, batchedAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(batchedPlan.Operations) != 1 || batchedPlan.Operations[0].ExecutionMode != protocol.ManualOperatorBatched {
		t.Fatalf("operator-batched rename operation = %#v", batchedPlan.Operations)
	}
	if len(batchedPlan.Reconciliations) != 1 || batchedPlan.Reconciliations[0].Strategy != "manual_sql" {
		t.Fatalf("operator-batched rename reconciliation = %#v", batchedPlan.Reconciliations)
	}
	for _, statement := range batchedPlan.Statements {
		if statement.Manual != nil && statement.Manual.ExecutionMode == protocol.ManualOperatorBatched {
			t.Fatalf("operator-batched work leaked into phase SQL: %#v", statement)
		}
	}

	splitAnswers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			{Kind: "rename_column", Key: before.ObjectID().String(), Value: after.ObjectID().String()},
			{Kind: "rename_backfill_strategy", Key: before.ObjectID().String(), Value: "split_plan"},
		},
	}
	split, err := Build(current, desired, splitAnswers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if split.Status != protocol.Planned || len(split.Statements) != 0 || len(split.Guidance) != 1 || split.Guidance[0].Kind != "split_plan" {
		t.Fatalf("split rename plan = %#v", split)
	}
}

func TestBuildDevelopmentColumnRenameIgnoresPositionAndRendersDirectDDL(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "checkout_quote"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	defaultValue := "'fx'::text"
	before := pgschema.Column{Table: table.ObjectID(), Name: "quote_mode", Position: 33, Type: "text", NotNull: true, Default: &defaultValue}
	after := before
	after.Name, after.Position = "pricing_mode", 9
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	if err := current.AddDependency(before.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(after.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	options := Options{PreserveSurplus: true, DirectColumnRenames: true}
	pending, err := Build(current, desired, protocol.Answers{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("development rename question = %#v", pending)
	}
	if !containsString(pending.Questions[0].Choices, "preserve") || containsString(pending.Questions[0].Choices, "create") {
		t.Fatalf("workspace rename fallback = %#v", pending.Questions[0].Choices)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_column", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, options)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || len(planned.Preserved) != 0 {
		t.Fatalf("development direct rename = %#v", planned)
	}
	if got, want := planned.Statements[0].SQL, `ALTER TABLE "public"."checkout_quote" RENAME COLUMN "quote_mode" TO "pricing_mode";`; got != want {
		t.Fatalf("development direct rename SQL = %q, want %q", got, want)
	}
}

func TestBuildDevelopmentColumnRenamePreservesPostgres18NotNullConstraintName(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	before := pgschema.Column{Table: table.ObjectID(), Name: "quote_mode", Type: "text", NotNull: true, NotNullConstraintName: "accounts_quote_mode_not_null"}
	after := before
	after.Name, after.NotNullConstraintName = "pricing_mode", "accounts_pricing_mode_not_null"
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	if err := current.AddDependency(before.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(after.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{DirectColumnRenames: true})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_column", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, Options{DirectColumnRenames: true})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) != 2 {
		t.Fatalf("PostgreSQL 18 direct rename = %#v", planned)
	}
	if got, want := planned.Statements[1].SQL, `ALTER TABLE "app"."accounts" RENAME CONSTRAINT "accounts_quote_mode_not_null" TO "accounts_pricing_mode_not_null";`; got != want {
		t.Fatalf("PostgreSQL 18 constraint rename = %q, want %q", got, want)
	}
}

func TestColumnRenameRejectsUnmodeledPostgres18NotNullConstraintIdentity(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	before := pgschema.Column{Table: table.ObjectID(), Name: "display_name", Type: "text", NotNull: true}
	after := before
	after.Name = "full_name"
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddUnsupported("not_null_constraint:app.accounts.accounts_display_name_not_null"); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddUnsupported("not_null_constraint:app.accounts.accounts_full_name_not_null"); err != nil {
		t.Fatal(err)
	}
	if reason := columnTriggerBridgeUnsupported(current, desired, before, after); reason != "not_null_constraint_identity" {
		t.Fatalf("PostgreSQL 18 NOT NULL constraint identity reason = %q", reason)
	}
}

func TestBuildRequiresFingerprintBoundEnumRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "public"}
	before := pgschema.Enum{Schema: "public", Name: "old_state", Labels: []string{"open", "closed"}}
	after := pgschema.Enum{Schema: "public", Name: "new_state", Labels: []string{"open", "closed"}}
	for _, object := range []pgschema.Object{schema, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{schema, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddDependency(before.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(after.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_enum" {
		t.Fatalf("expected enum rename question, got %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_enum", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_bridge_required:") {
		t.Fatalf("enum rename must not become a direct cutover: %#v", planned)
	}
}

func TestBuildSeparatesConcurrentIndexIntoNonTransactionalBatch(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	index := pgschema.Index{Table: table.ObjectID(), Name: "orders_id_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id", NullsLast: true}}}
	if err := desired.Add(index); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(index.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Batches) != 1 || result.Batches[0].Transactional {
		t.Fatalf("expected one non-transactional batch, got %#v", result)
	}
	if got := result.Statements[0].SQL; got != `CREATE INDEX CONCURRENTLY "orders_id_idx" ON "public"."orders" USING "btree" ("id" NULLS LAST);` {
		t.Fatalf("concurrent SQL = %q", got)
	}
}

func TestBuildContinuouslyReplacesSameNameIndex(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	before := pgschema.Index{Table: table.ObjectID(), Name: "orders_lookup_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "account_id"}}}
	after := pgschema.Index{Table: table.ObjectID(), Name: "orders_lookup_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "account_id"}, {Column: "created_at", Descending: true}}}
	for _, pair := range []struct {
		snapshot *pgschema.Snapshot
		index    pgschema.Index
	}{{current, before}, {desired, after}} {
		if err := pair.snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.Add(pair.index); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.index.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := replacementIndexName(before, after)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`ALTER INDEX "public"."orders_lookup_idx" RENAME TO "` + temporary + `";`,
		`CREATE INDEX CONCURRENTLY "orders_lookup_idx" ON "public"."orders" USING "btree" ("account_id", "created_at" DESC);`,
		`DROP INDEX CONCURRENTLY "public"."` + temporary + `";`,
	}
	if result.Status != protocol.Planned || len(result.Statements) != len(want) {
		t.Fatalf("unexpected continuous replacement plan: %#v", result)
	}
	for i, sql := range want {
		if result.Statements[i].SQL != sql {
			t.Fatalf("statement %d = %q, want %q", i, result.Statements[i].SQL, sql)
		}
	}
	if result.Statements[0].Phase != "expand" || result.Statements[0].NonTransactional || !result.Statements[1].NonTransactional || result.Statements[2].Phase != "contract" || !result.Statements[2].NonTransactional {
		t.Fatalf("invalid phased transaction boundaries: %#v", result.Statements)
	}
	if result.Statements[0].StatementTimeoutMS != 3000 || result.Statements[0].LockTimeoutMS != 3000 || result.Statements[1].StatementTimeoutMS != 1200000 || result.Statements[1].LockTimeoutMS != 3000 || result.Statements[2].StatementTimeoutMS != 1200000 || result.Statements[2].LockTimeoutMS != 3000 {
		t.Fatalf("invalid timeout guidance: %#v", result.Statements)
	}
	if len(result.Batches) != 3 || !result.Batches[0].Transactional || result.Batches[1].Transactional || result.Batches[2].Transactional {
		t.Fatalf("invalid continuous replacement batches: %#v", result.Batches)
	}
}

func TestBuildContinuouslyReplacesIndexForCollationChange(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	before := pgschema.Index{Table: table.ObjectID(), Name: "orders_name_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "name"}}}
	after := before
	after.Parts = []pgschema.IndexPart{{Column: "name", Collation: `pg_catalog."C"`}}
	for _, pair := range []struct {
		snapshot *pgschema.Snapshot
		index    pgschema.Index
	}{{current, before}, {desired, after}} {
		if err := pair.snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.Add(pair.index); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.index.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := replacementIndexName(before, after)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`ALTER INDEX "public"."orders_name_idx" RENAME TO "` + temporary + `";`,
		`CREATE INDEX CONCURRENTLY "orders_name_idx" ON "public"."orders" USING "btree" ("name" COLLATE pg_catalog."C");`,
		`DROP INDEX CONCURRENTLY "public"."` + temporary + `";`,
	}
	if result.Status != protocol.Planned || len(result.Statements) != len(want) {
		t.Fatalf("unexpected collation replacement plan: %#v", result)
	}
	for index, sql := range want {
		if result.Statements[index].SQL != sql {
			t.Fatalf("statement %d = %q, want %q", index, result.Statements[index].SQL, sql)
		}
	}
}

func TestBuildContinuouslyReplacesEmptyPartitionedParentIndex(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "events", Partition: &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (created_at)"}}
	before := pgschema.Index{Table: table.ObjectID(), Name: "events_lookup_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "account_id"}}}
	after := pgschema.Index{Table: table.ObjectID(), Name: "events_lookup_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "account_id"}, {Column: "created_at"}}}
	for _, pair := range []struct {
		snapshot *pgschema.Snapshot
		index    pgschema.Index
	}{{current, before}, {desired, after}} {
		if err := pair.snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.Add(pair.index); err != nil {
			t.Fatal(err)
		}
		if err := pair.snapshot.AddDependency(pair.index.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 3 || !strings.Contains(result.Statements[1].SQL, ` ON ONLY "public"."events" `) || result.Statements[2].Phase != "contract" {
		t.Fatalf("expected empty partitioned-parent shell replacement, got %#v", result)
	}
}

func TestBuildContinuouslyReplacesNestedEmptyPartitionedIndex(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	parent := pgschema.Table{Schema: "public", Name: "events", Partition: &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (created_at)"}}
	child := pgschema.Table{Schema: "public", Name: "events_2026", Partition: &pgschema.Partition{Strategy: "HASH", Raw: "HASH (account_id)"}, PartitionOf: &pgschema.PartitionOf{Parent: parent.ObjectID(), Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}}
	before := pgschema.Index{Table: parent.ObjectID(), Name: "events_lookup_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "account_id"}}}
	after := before
	after.Parts = []pgschema.IndexPart{{Column: "account_id"}, {Column: "created_at"}}
	beforeParentID, afterParentID := before.ObjectID(), after.ObjectID()
	beforeChild := pgschema.Index{Table: child.ObjectID(), Name: "events_2026_account_id_idx", Parent: &beforeParentID, Method: "btree", Parts: before.Parts}
	afterChild := pgschema.Index{Table: child.ObjectID(), Name: "events_2026_account_id_created_at_idx", Parent: &afterParentID, Method: "btree", Parts: after.Parts}
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		parent   pgschema.Index
		child    pgschema.Index
	}{{current, before, beforeChild}, {desired, after, afterChild}} {
		for _, object := range []pgschema.Object{parent, child, fixture.parent, fixture.child} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.snapshot.AddDependency(fixture.child.ObjectID(), fixture.parent.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || result.Status != protocol.Planned || len(result.Statements) != 6 {
		t.Fatalf("nested hierarchy must plan recursively: plan=%#v err=%v", result, err)
	}
	if !strings.Contains(result.Statements[2].SQL, ` ON ONLY "public"."events" `) ||
		!strings.Contains(result.Statements[3].SQL, ` ON ONLY "public"."events_2026" `) ||
		!strings.Contains(result.Statements[4].SQL, `"events_lookup_idx" ATTACH PARTITION "public"."events_2026_account_id_created_at_idx"`) ||
		result.Statements[5].Phase != "contract" {
		t.Fatalf("nested shell/build/attach/retire order is invalid: %#v", result.Statements)
	}
}

func TestBuildAttachesExistingLocalIndexToPartitionedParent(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	parentTable := pgschema.Table{Schema: "public", Name: "events", Partition: &pgschema.Partition{Strategy: "RANGE", Raw: "RANGE (created_at)"}}
	childTable := pgschema.Table{Schema: "public", Name: "events_2026", PartitionOf: &pgschema.PartitionOf{Parent: parentTable.ObjectID(), Bound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"}}
	parentIndex := pgschema.Index{Table: parentTable.ObjectID(), Name: "events_lookup_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "created_at"}}}
	childBefore := pgschema.Index{Table: childTable.ObjectID(), Name: "events_2026_lookup_idx", Method: "btree", Parts: parentIndex.Parts}
	childAfter := childBefore
	parentID := parentIndex.ObjectID()
	childAfter.Parent = &parentID
	for _, fixture := range []struct {
		snapshot *pgschema.Snapshot
		child    pgschema.Index
	}{{current, childBefore}, {desired, childAfter}} {
		for _, object := range []pgschema.Object{parentTable, childTable, parentIndex, fixture.child} {
			if err := fixture.snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		for _, edge := range [][2]pgschema.ID{
			{childTable.ObjectID(), parentTable.ObjectID()},
			{parentIndex.ObjectID(), parentTable.ObjectID()},
			{fixture.child.ObjectID(), childTable.ObjectID()},
		} {
			if err := fixture.snapshot.AddDependency(edge[0], edge[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := desired.AddDependency(childAfter.ObjectID(), parentID); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || result.Status != protocol.Planned || len(result.Statements) != 1 {
		t.Fatalf("attach plan=%#v err=%v", result, err)
	}
	statement := result.Statements[0]
	if statement.SQL != `ALTER INDEX "public"."events_lookup_idx" ATTACH PARTITION "public"."events_2026_lookup_idx";` || statement.Phase != "expand" || statement.NonTransactional || statement.StatementTimeoutMS != 30000 || statement.LockTimeoutMS != 3000 {
		t.Fatalf("unexpected attach statement: %#v", statement)
	}
}

func TestBuildRejectsPartitionIndexDetachOrStructuralAttach(t *testing.T) {
	parent := pgschema.Index{Table: (pgschema.Table{Schema: "public", Name: "events"}).ObjectID(), Name: "events_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	parentID := parent.ObjectID()
	attached := pgschema.Index{Table: (pgschema.Table{Schema: "public", Name: "events_1"}).ObjectID(), Name: "events_1_idx", Parent: &parentID, Method: "btree", Parts: parent.Parts}
	detached := attached
	detached.Parent = nil
	_, _, unsupported, err := renderModify(attached, detached, decisions{}, Options{}, nil, nil)
	if err != nil || len(unsupported) != 1 || !strings.Contains(unsupported[0], "partitioned_index_modify") {
		t.Fatalf("detach must reject: unsupported=%#v err=%v", unsupported, err)
	}
	structural := detached
	structural.Parent = &parentID
	structural.Parts = []pgschema.IndexPart{{Column: "id"}, {Column: "created_at"}}
	_, _, unsupported, err = renderModify(detached, structural, decisions{}, Options{}, nil, nil)
	if err != nil || len(unsupported) != 1 || !strings.Contains(unsupported[0], "partition_index_attach_rebuild") {
		t.Fatalf("structural attach must reject: unsupported=%#v err=%v", unsupported, err)
	}
}

func TestPartitionConstraintAttachmentRejectsMismatchedExistingIndex(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	parentTable := (pgschema.Table{Schema: "public", Name: "events"}).ObjectID()
	childTable := (pgschema.Table{Schema: "public", Name: "events_1"}).ObjectID()
	parentConstraint := pgschema.Constraint{Table: parentTable, Name: "events_pkey", Type: pgschema.ConstraintPrimary, UsingIndex: "events_pkey"}
	parentConstraintID := parentConstraint.ObjectID()
	parentIndex := pgschema.Index{Table: parentTable, Name: "events_pkey", Unique: true, Primary: true, Constraint: "events_pkey", Method: "btree", Parts: []pgschema.IndexPart{{Column: "bucket"}, {Column: "id"}}}
	parentIndexID := parentIndex.ObjectID()
	before := pgschema.Index{Table: childTable, Name: "events_1_pkey", Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "bucket"}}}
	after := before
	after.Parent = &parentIndexID
	after.Primary = true
	after.Constraint = "events_1_pkey"
	constraint := pgschema.Constraint{Table: childTable, Name: "events_1_pkey", Parent: &parentConstraintID, Type: pgschema.ConstraintPrimary, UsingIndex: "events_1_pkey"}
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	for _, object := range []pgschema.Object{parentConstraint, parentIndex, after, constraint} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	_, _, unsupported, err := renderPartitionConstraintAttachment(constraint, current, desired)
	if err != nil || len(unsupported) != 1 || !strings.Contains(unsupported[0], "partition_constraint_attach_structure") {
		t.Fatalf("mismatched constraint attachment must reject: unsupported=%#v err=%v", unsupported, err)
	}
}

func TestBuildRendersIdempotentSchemaAndTableOptions(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	for _, object := range []pgschema.Object{schema, table} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{IfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || joinSQL(result) != "CREATE SCHEMA IF NOT EXISTS \"app\";\nCREATE TABLE IF NOT EXISTS \"app\".\"orders\" ();" {
		t.Fatalf("unexpected idempotent create plan %#v", result)
	}

	current, desired = pgschema.New(), pgschema.New()
	if err := desired.Add(pgschema.Schema{Name: "public"}); err != nil {
		t.Fatal(err)
	}
	result, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || joinSQL(result) != "CREATE SCHEMA IF NOT EXISTS \"public\";" {
		t.Fatalf("unexpected public schema plan %#v", result)
	}

	current, desired = pgschema.New(), pgschema.New()
	for _, object := range []pgschema.Object{schema, table} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{IfExists: true, CascadeDrops: true})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: schema.ObjectID().String(), Value: "drop"}}}
	result, err = Build(current, desired, answers, Options{IfExists: true, CascadeDrops: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || joinSQL(result) != "DROP SCHEMA IF EXISTS \"app\" CASCADE;" {
		t.Fatalf("unexpected guarded cascade drop plan %#v", result)
	}

	current, desired = pgschema.New(), pgschema.New()
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(schema); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(table); err != nil {
		t.Fatal(err)
	}
	if err := current.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err = Build(current, desired, protocol.Answers{}, Options{IfExists: true, CascadeDrops: true})
	if err != nil {
		t.Fatal(err)
	}
	answers = protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: table.ObjectID().String(), Value: "drop"}}}
	result, err = Build(current, desired, answers, Options{IfExists: true, CascadeDrops: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || joinSQL(result) != "DROP TABLE IF EXISTS \"app\".\"orders\" CASCADE;" {
		t.Fatalf("unexpected guarded table drop plan %#v", result)
	}
}

func TestBuildRendersIfExistsForConstraintDrop(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	constraint := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_account_fkey", Type: pgschema.ConstraintForeign, Definition: "FOREIGN KEY (account_id) REFERENCES accounts(id)"}
	for _, object := range []pgschema.Object{table, constraint} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.Add(table); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{IfExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || !strings.Contains(pending.Questions[0].Message, "expand schema may stop enforcing") {
		t.Fatalf("constraint removal did not ask the enforcement-specific question: %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: constraint.ObjectID().String(), Value: "drop"}}}
	plan, err := Build(current, desired, answers, Options{IfExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinSQL(plan), `DROP CONSTRAINT IF EXISTS "orders_account_fkey"`) {
		t.Fatalf("missing IF EXISTS constraint drop: %#v", plan)
	}
	if len(plan.Statements) != 1 || plan.Statements[0].Phase != protocol.PhaseExpand || containsString(plan.Statements[0].Hazards, "data_loss") || !containsString(plan.Statements[0].Hazards, "referential_enforcement_removed") {
		t.Fatalf("constraint drop was not rendered as a precise expand relaxation: %#v", plan.Statements)
	}
}

func TestBuildSeparatesConcurrentIndexDropIntoNonTransactionalBatch(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	index := pgschema.Index{Table: table.ObjectID(), Name: "orders_id_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(index); err != nil {
		t.Fatal(err)
	}
	if err := current.AddDependency(index.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected destructive index-drop question: %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: index.ObjectID().String(), Value: "drop"}}}
	result, err := Build(current, desired, answers, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Batches) != 1 || result.Batches[0].Transactional {
		t.Fatalf("expected one non-transactional concurrent drop batch, got %#v", result)
	}
	if got := result.Statements[0].SQL; got != `DROP INDEX CONCURRENTLY "public"."orders_id_idx";` {
		t.Fatalf("concurrent drop SQL = %q", got)
	}
}

func TestBuildRendersStructuredIndexWithoutRawDefinition(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	index := pgschema.Index{
		Table: table.ObjectID(), Name: "orders_lookup_idx", Unique: true, Method: "btree",
		Parts:   []pgschema.IndexPart{{Expression: "lower(name)", Descending: true, NullsFirst: true, OpClass: &pgschema.OpClass{Name: "text_pattern_ops"}}},
		Include: []string{"id"}, NullsNotDistinct: true, Predicate: "name IS NOT NULL",
		Storage: pgschema.IndexStorage{Options: []pgschema.Option{{Name: "fillfactor", Value: "70"}}},
		// If planning used this compatibility field, the generated SQL would
		// be invalid. Typed fields must be the source of truth.
		Definition: "not SQL",
	}
	if err := desired.Add(index); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(index.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result := buildWithManualReconciliation(t, current, desired, Options{}, `DELETE FROM "app"."orders" a USING "app"."orders" b WHERE a.ctid < b.ctid AND lower(a.name) = lower(b.name);`, `SELECT NOT EXISTS (SELECT 1 FROM "app"."orders" WHERE name IS NOT NULL GROUP BY lower(name) HAVING count(*) > 1);`)
	if result.Status != protocol.Planned || len(result.Statements) != 3 {
		t.Fatalf("unexpected structured index plan %#v", result)
	}
	want := `CREATE UNIQUE INDEX "orders_lookup_idx" ON "app"."orders" USING "btree" ((lower(name)) "text_pattern_ops" DESC NULLS FIRST) INCLUDE ("id") NULLS NOT DISTINCT WITH ("fillfactor" = 70) WHERE name IS NOT NULL;`
	if got := result.Statements[len(result.Statements)-1].SQL; got != want {
		t.Fatalf("structured index SQL = %q, want %q", got, want)
	}
}

func TestBuildValidatesNotValidConstraintWithoutRebuild(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "public", Name: "orders"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	before := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_positive", Type: pgschema.ConstraintCheck, Definition: "CHECK (value > 0) NOT VALID", Validated: false}
	after := before
	after.Definition, after.Validated = "CHECK (value > 0)", true
	if err := current.Add(before); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(after); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		constraintID := before.ObjectID()
		if err := snapshot.AddDependency(constraintID, table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	result := buildWithAssertOnlyReconciliations(t, current, desired, Options{})
	if result.Status != protocol.Planned || len(result.Statements) != 2 || result.Statements[1].SQL != `ALTER TABLE "public"."orders" VALIDATE CONSTRAINT "orders_positive";` || result.Statements[1].Phase != protocol.PhaseContract {
		t.Fatalf("unexpected validation plan %#v", result)
	}
}

func TestBuildCreatesStandaloneSequenceBeforeDefaultUsingTable(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	sequence := pgschema.Sequence{Schema: "app", Name: "order_number_seq", Type: "bigint", Start: 10, Increment: 2, Min: 1, Max: 999, Cache: 5, Cycle: true}
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "number", Position: 1, Type: "bigint", Default: stringPointer(`nextval('app.order_number_seq'::regclass)`)}
	for _, object := range []pgschema.Object{schema, sequence, table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(schema); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]pgschema.ID{{sequence.ObjectID(), schema.ObjectID()}, {table.ObjectID(), schema.ObjectID()}, {table.ObjectID(), sequence.ObjectID()}, {column.ObjectID(), table.ObjectID()}, {column.ObjectID(), sequence.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 2 {
		t.Fatalf("unexpected sequence create plan %#v", result)
	}
	if got := result.Statements[0].SQL; got != `CREATE SEQUENCE "app"."order_number_seq" AS bigint START WITH 10 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 5 CYCLE;` {
		t.Fatalf("sequence SQL = %q", got)
	}
	if !strings.HasPrefix(result.Statements[1].SQL, `CREATE TABLE "app"."orders"`) {
		t.Fatalf("table did not follow sequence: %q", result.Statements[1].SQL)
	}
}

func TestBuildRejectsNonCanonicalOwnedSerialSequence(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "integer", Serial: &pgschema.Serial{Type: "serial", SequenceName: "custom_order_numbers"}}
	for _, object := range []pgschema.Object{table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.Add(pgschema.Schema{Name: "app"}); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(table.ObjectID(), (pgschema.Schema{Name: "app"}).ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || result.Unsupported[0] != "serial_sequence_name:column:app:orders:id" {
		t.Fatalf("noncanonical owned serial sequence must be explicit unsupported: %#v", result)
	}
}

func TestBuildRendersStandaloneSequenceMutation(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	before := pgschema.Sequence{Schema: "app", Name: "order_number_seq", Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 1}
	after := before
	after.Increment = 2
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(pgschema.Schema{Name: "app"}); err != nil {
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
		if err := snapshot.AddDependency(before.ObjectID(), (pgschema.Schema{Name: "app"}).ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 1 || result.Statements[0].SQL != `ALTER SEQUENCE "app"."order_number_seq" AS bigint START WITH 1 INCREMENT BY 2 MINVALUE 1 MAXVALUE 100 CACHE 1 NO CYCLE;` {
		t.Fatalf("sequence mutation was not rendered, got %#v", result)
	}
}

func TestBuildRequiresManualContractForPartitionKeyMutation(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	beforeTable := pgschema.Table{Schema: "app", Name: "orders"}
	afterTable := beforeTable
	afterTable.Partition = &pgschema.Partition{Strategy: "HASH", Raw: "HASH (id)"}
	beforeColumn := pgschema.Column{Table: beforeTable.ObjectID(), Name: "slug", Position: 1, Type: "text", Generated: &pgschema.Generated{Expression: "lower(name)", Kind: "STORED"}}
	afterColumn := beforeColumn
	afterColumn.Generated = &pgschema.Generated{Expression: "upper(name)", Kind: "STORED"}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, beforeTable, beforeColumn} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, afterTable, afterColumn} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		table := beforeTable
		if snapshot == desired {
			table = afterTable
		}
		if err := snapshot.AddDependency(table.ObjectID(), (pgschema.Schema{Name: "app"}).ObjectID()); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency((pgschema.Column{Table: table.ObjectID(), Name: "slug"}).ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.NeedsInput || len(result.Questions) != 1 || result.Questions[0].Kind != "partition_reconfiguration" || result.Questions[0].Key != afterTable.ObjectID().String() {
		t.Fatalf("expected explicit partition-reconfiguration handoff, got %#v", result)
	}
}

func TestBuildRejectsNullableSerialColumn(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	column := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "integer", Serial: &pgschema.Serial{Type: "serial"}}
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "app"}, table, column} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := desired.AddDependency(table.ObjectID(), (pgschema.Schema{Name: "app"}).ObjectID()); err != nil {
		t.Fatal(err)
	}
	if err := desired.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
		t.Fatal(err)
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || result.Unsupported[0] != "serial_sequence_name:column:app:orders:id" {
		t.Fatalf("nullable serial must be explicit unsupported: %#v", result)
	}
}

func TestBuildRequiresConfirmedEnumLabelRewrite(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
	}{
		{name: "drop", labels: []string{"open"}},
		{name: "reorder", labels: []string{"closed", "open"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current, desired := pgschema.New(), pgschema.New()
			before := pgschema.Enum{Schema: "app", Name: "state", Labels: []string{"open", "closed"}}
			after := pgschema.Enum{Schema: "app", Name: "state", Labels: test.labels}
			for _, snapshot := range []*pgschema.Snapshot{current, desired} {
				if err := snapshot.Add(pgschema.Schema{Name: "app"}); err != nil {
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
				if err := snapshot.AddDependency(before.ObjectID(), (pgschema.Schema{Name: "app"}).ObjectID()); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rewrite_enum" {
				t.Fatalf("enum %s must require confirmation: %#v", test.name, pending)
			}
			question := pending.Questions[0]
			answers := protocol.Answers{
				CurrentFingerprint: pending.CurrentFingerprint,
				DesiredFingerprint: pending.DesiredFingerprint,
				Answers:            []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: "rewrite", QuestionFingerprint: question.ScopeFingerprint}},
			}
			planned, err := Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
			joined := joinSQL(planned)
			if planned.Status != protocol.Planned || !strings.Contains(joined, `ALTER TYPE "app"."state" RENAME TO "onwardpg_tmpenum_`) || !strings.Contains(joined, `CREATE TYPE "app"."state" AS ENUM (`) || !strings.Contains(joined, `DROP TYPE "app"."onwardpg_tmpenum_`) {
				t.Fatalf("enum %s rewrite was not planned: %#v", test.name, planned)
			}
		})
	}
}

func TestRenameEnumValuesAcceptsOnlyPurePositionalRenames(t *testing.T) {
	before := pgschema.Enum{Schema: "app", Name: "state", Labels: []string{"new", "active", "archived"}}
	after := pgschema.Enum{Schema: "app", Name: "state", Labels: []string{"fresh", "enabled", "archived"}}
	statements, ok := renameEnumValues(before, after)
	if !ok || len(statements) != 2 || statements[0].SQL != `ALTER TYPE "app"."state" RENAME VALUE 'new' TO 'fresh';` || statements[1].SQL != `ALTER TYPE "app"."state" RENAME VALUE 'active' TO 'enabled';` {
		t.Fatalf("pure enum rename = %#v, %v", statements, ok)
	}

	for name, labels := range map[string][]string{
		"reorder":          {"active", "new", "archived"},
		"mixed_add":        {"fresh", "active", "archived", "deleted"},
		"existing_target":  {"active", "active", "archived"},
		"unchanged_labels": {"new", "active", "archived"},
	} {
		t.Run(name, func(t *testing.T) {
			if statements, ok := renameEnumValues(before, pgschema.Enum{Schema: "app", Name: "state", Labels: labels}); ok || len(statements) != 0 {
				t.Fatalf("unsafe enum rename accepted: %#v", statements)
			}
		})
	}
}

func TestBuildRequiresFingerprintBoundConstraintRenameAnswer(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	before := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_value_check", Type: pgschema.ConstraintCheck, Definition: "CHECK (value > 0)", Validated: true}
	after := before
	after.Name = "orders_positive"
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(pgschema.Schema{Name: "app"}); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(table.ObjectID(), (pgschema.Schema{Name: "app"}).ObjectID()); err != nil {
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
		constraint := before
		if snapshot == desired {
			constraint = after
		}
		if err := snapshot.AddDependency(constraint.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_constraint" {
		t.Fatalf("constraint rename must require confirmation: %#v", pending)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_constraint", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || joinSQL(result) != `ALTER TABLE "app"."orders" RENAME CONSTRAINT "orders_value_check" TO "orders_positive";` {
		t.Fatalf("confirmed constraint rename = %#v", result)
	}
}

func TestBuildRejectsForeignKeyMatchPartial(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	accounts := pgschema.Table{Schema: "app", Name: "accounts"}
	orders := pgschema.Table{Schema: "app", Name: "orders"}
	foreignKey := pgschema.Constraint{Table: orders.ObjectID(), Name: "orders_account_id_fkey", Type: pgschema.ConstraintForeign, Definition: "FOREIGN KEY (account_id) REFERENCES app.accounts(id) MATCH PARTIAL"}
	for _, object := range []pgschema.Object{schema, accounts, orders, foreignKey} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{accounts.ObjectID(), schema.ObjectID()}, {orders.ObjectID(), schema.ObjectID()}, {foreignKey.ObjectID(), orders.ObjectID()}, {foreignKey.ObjectID(), accounts.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || result.Unsupported[0] != "foreign_key_match_partial:constraint:app:orders:orders_account_id_fkey" {
		t.Fatalf("MATCH PARTIAL must be explicit unsupported: %#v", result)
	}
}

func TestBuildDropsSameNamedIndexBeforeMovingItToAnotherTable(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	from, to := pgschema.Table{Schema: "app", Name: "from_table"}, pgschema.Table{Schema: "app", Name: "to_table"}
	oldIndex := pgschema.Index{Table: from.ObjectID(), Name: "shared_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	newIndex := pgschema.Index{Table: to.ObjectID(), Name: "shared_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}}
	for _, object := range []pgschema.Object{schema, from, to, pgschema.Column{Table: from.ObjectID(), Name: "id", Position: 1, Type: "bigint"}, pgschema.Column{Table: to.ObjectID(), Name: "id", Position: 1, Type: "bigint"}} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(oldIndex); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(newIndex); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected destructive index question: %#v", pending)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: oldIndex.ObjectID().String(), Value: "drop"}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(planned)
	dropAt, createAt := strings.Index(joined, `DROP INDEX "app"."shared_idx";`), strings.Index(joined, `CREATE INDEX "shared_idx" ON "app"."to_table"`)
	if dropAt < 0 || createAt < 0 || dropAt > createAt {
		t.Fatalf("index replacement created before same-name drop: %s", joined)
	}
}

func TestRebuildBatchesRejectsUnknownPhase(t *testing.T) {
	result := protocol.Result{Statements: []protocol.Statement{{SQL: "SELECT 1;", Phase: "typo", Safety: "safe"}}}
	if err := rebuildBatches(&result); err == nil {
		t.Fatal("expected unknown phase to be rejected")
	}
}

func TestConstraintUsingIndexRejectsMismatchedStructure(t *testing.T) {
	table := pgschema.Table{Schema: "app", Name: "orders"}
	constraint := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_key", Type: pgschema.ConstraintUnique, UsingIndex: "orders_key"}
	current, desired := pgschema.New(), pgschema.New()
	existing := pgschema.Index{Table: table.ObjectID(), Name: "orders_key", Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "a"}}}
	expected := existing
	expected.Constraint = constraint.Name
	expected.Parts = []pgschema.IndexPart{{Column: "a"}, {Column: "b"}}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(existing); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(expected); err != nil {
		t.Fatal(err)
	}
	_, _, unsupported, err := renderConstraintCreateUsingExistingIndex(constraint, current, desired)
	if err != nil || len(unsupported) != 1 || unsupported[0] != "constraint_using_index_structure:"+constraint.ObjectID().String() {
		t.Fatalf("expected structural attachment rejection, got %v %v", unsupported, err)
	}
}

func TestColumnRenameIgnoresUnrelatedTypedDependencies(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	oldColumn := pgschema.Column{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"}
	newColumn := oldColumn
	newColumn.Name = "order_id"
	paidAt := pgschema.Column{Table: table.ObjectID(), Name: "paid_at", Position: 2, Type: "timestamptz"}
	index := pgschema.Index{Table: table.ObjectID(), Name: "orders_paid_at_idx", Method: "btree", Parts: []pgschema.IndexPart{{Column: "paid_at"}}}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		for _, object := range []pgschema.Object{table, paidAt, index} {
			if err := snapshot.Add(object); err != nil {
				t.Fatal(err)
			}
		}
		if err := snapshot.AddDependency(index.ObjectID(), paidAt.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := current.Add(oldColumn); err != nil {
		t.Fatal(err)
	}
	if err := desired.Add(newColumn); err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("unrelated paid_at dependency suppressed rename: %#v", pending)
	}
}

func TestColumnRenamePreservesAutomaticallyRewrittenConstraint(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	oldColumn := pgschema.Column{Table: table.ObjectID(), Name: "old_name", Position: 1, Type: "text"}
	newColumn := oldColumn
	newColumn.Name = "new_name"
	oldConstraint := pgschema.Constraint{Table: table.ObjectID(), Name: "orders_name_check", Type: pgschema.ConstraintCheck, Definition: "CHECK ((old_name <> 'old_name'::text))"}
	newConstraint := oldConstraint
	newConstraint.Definition = "CHECK ((new_name <> 'old_name'::text))"
	for _, object := range []pgschema.Object{table, oldColumn, oldConstraint} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, newColumn, newConstraint} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldColumn.ObjectID(), table.ObjectID()}, {oldConstraint.ObjectID(), table.ObjectID()}, {oldConstraint.ObjectID(), oldColumn.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{newColumn.ObjectID(), table.ObjectID()}, {newConstraint.ObjectID(), table.ObjectID()}, {newConstraint.ObjectID(), newColumn.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{
		{Kind: "rename_column", Key: oldColumn.ObjectID().String(), Value: newColumn.ObjectID().String()},
		{Kind: "rename_backfill_strategy", Key: oldColumn.ObjectID().String(), Value: "single_transaction"},
	}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), `RENAME COLUMN "old_name" TO "new_name"`) {
		t.Fatalf("constraint-backed column rename must use the automatic compatibility bridge: %#v", plan)
	}
}

func TestRenameIdentifierTokensPreservesStrings(t *testing.T) {
	got := renameIdentifierTokens(`CHECK ((old_name <> 'old_name') AND "old_name" IS NOT NULL)`, "old_name", "new_name")
	want := `CHECK ((new_name <> 'old_name') AND "new_name" IS NOT NULL)`
	if got != want {
		t.Fatalf("renameIdentifierTokens = %q, want %q", got, want)
	}
}

func TestNormalizeTriggerColumnReferenceChangesOnlyColumnRegions(t *testing.T) {
	definition := "CREATE TRIGGER old_name BEFORE UPDATE OF old_name ON app.orders FOR EACH ROW WHEN (old.old_name IS DISTINCT FROM new.old_name AND audit_old_name() IS NULL) EXECUTE FUNCTION app.old_name()"
	got := normalizeTriggerColumnReference(definition, "old_name", "new_name")
	want := "CREATE TRIGGER old_name BEFORE UPDATE OF new_name ON app.orders FOR EACH ROW WHEN (old.new_name IS DISTINCT FROM new.new_name AND audit_old_name() IS NULL) EXECUTE FUNCTION app.old_name()"
	if got != want {
		t.Fatalf("normalized trigger = %q, want %q", got, want)
	}
}

func TestAutomaticSimpleViewColumnRenameDefinition(t *testing.T) {
	before := " SELECT old_name\n   FROM app.orders;"
	got, ok := automaticSimpleViewColumnRenameDefinition(before, "old_name", "new_name")
	if !ok {
		t.Fatal("expected simple deparsed view definition to be recognized")
	}
	want := " SELECT new_name AS old_name\n   FROM app.orders;"
	if got != want {
		t.Fatalf("automatic view definition = %q, want %q", got, want)
	}
	if _, ok := automaticSimpleViewColumnRenameDefinition(" SELECT old_name || 'x' FROM app.orders;", "old_name", "new_name"); ok {
		t.Fatal("expression must remain outside automatic rewrite proof")
	}
	qualified := " SELECT orders.old_name\n   FROM app.orders;"
	if got, ok := automaticSimpleViewColumnRenameDefinition(qualified, "old_name", "new_name"); !ok || got != " SELECT orders.new_name AS old_name\n   FROM app.orders;" {
		t.Fatalf("qualified automatic definition = %q, ok=%v", got, ok)
	}
}

func TestColumnRenameRejectsUnprovenDependentViewRewrite(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	table := pgschema.Table{Schema: "app", Name: "orders"}
	oldColumn := pgschema.Column{Table: table.ObjectID(), Name: "old_name", Position: 1, Type: "text"}
	newColumn := oldColumn
	newColumn.Name = "new_name"
	before := pgschema.View{Schema: "app", Name: "order_view", Definition: "SELECT old_name || 'x' FROM app.orders"}
	after := pgschema.View{Schema: "app", Name: "order_view", Definition: "SELECT new_name || 'x' FROM app.orders"}
	for _, object := range []pgschema.Object{table, oldColumn, before} {
		if err := current.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []pgschema.Object{table, newColumn, after} {
		if err := desired.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{oldColumn.ObjectID(), table.ObjectID()}, {before.ObjectID(), table.ObjectID()}, {before.ObjectID(), oldColumn.ObjectID()}} {
		if err := current.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{{newColumn.ObjectID(), table.ObjectID()}, {after.ObjectID(), table.ObjectID()}, {after.ObjectID(), newColumn.ObjectID()}} {
		if err := desired.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_column", Key: oldColumn.ObjectID().String(), Value: newColumn.ObjectID().String()}}}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "column_rename_dependent_view:" + before.ObjectID().String()
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, want) {
		t.Fatalf("expected structured unproven-view rejection, got %#v", result)
	}
}

func TestNormalizeRoutineCallReferencesSkipsLiteralsAndBareNames(t *testing.T) {
	from := pgschema.Routine{Schema: "app", Name: "old_double", Signature: "v integer", Kind: "function"}
	to := pgschema.Routine{Schema: "app", Name: "new_double", Signature: "v integer", Kind: "function"}
	definition := "SELECT 'app.old_double(1)' AS literal, app.old_double(1) AS value, old_double(1) AS bare"
	got, changed := normalizeRoutineCallReferences(definition, from, to)
	if !changed {
		t.Fatal("expected qualified routine call to be normalized")
	}
	want := "SELECT 'app.old_double(1)' AS literal, app.new_double(1) AS value, old_double(1) AS bare"
	if got != want {
		t.Fatalf("normalized routine references = %q, want %q", got, want)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasUnsupportedPrefix(result protocol.Result, prefix string) bool {
	for _, value := range result.Unsupported {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func joinSQL(result protocol.Result) string {
	var sql []string
	for _, statement := range result.Statements {
		sql = append(sql, statement.SQL)
	}
	return strings.Join(sql, "\n")
}

func stringPointer(value string) *string { return &value }

func TestPlanReportsUnreachablePhysicalColumnOrderWithoutBlocking(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(schema); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []pgschema.Column{
		{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"},
		{Table: table.ObjectID(), Name: "email", Position: 2, Type: "text"},
	} {
		if err := current.Add(column); err != nil {
			t.Fatal(err)
		}
		if err := current.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []pgschema.Column{
		{Table: table.ObjectID(), Name: "id", Position: 1, Type: "bigint"},
		{Table: table.ObjectID(), Name: "timezone", Position: 2, Type: "text"},
		{Table: table.ObjectID(), Name: "email", Position: 3, Type: "text"},
	} {
		if err := desired.Add(column); err != nil {
			t.Fatal(err)
		}
		if err := desired.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Compatibility) != 1 || len(result.Statements) != 1 {
		t.Fatalf("unreachable column order result = %#v", result)
	}
	joined := strings.Join(result.Compatibility, "\n")
	if !strings.Contains(joined, "inserted_column:timezone:desired=2:last_retained=3") {
		t.Fatalf("unreachable column order diagnostics = %q", joined)
	}
	if !strings.Contains(result.Statements[0].SQL, `ADD COLUMN "timezone" text`) {
		t.Fatalf("unreachable column order plan = %#v", result.Statements)
	}
}

func TestPlanAllowsDenseColumnOrderToCloseAfterMiddleDrop(t *testing.T) {
	current, desired := pgschema.New(), pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	table := pgschema.Table{Schema: "app", Name: "accounts"}
	for _, snapshot := range []*pgschema.Snapshot{current, desired} {
		if err := snapshot.Add(schema); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	for index, name := range []string{"id", "legacy", "email"} {
		column := pgschema.Column{Table: table.ObjectID(), Name: name, Position: index + 1, Type: "text"}
		if err := current.Add(column); err != nil {
			t.Fatal(err)
		}
		if err := current.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	for index, name := range []string{"id", "email"} {
		column := pgschema.Column{Table: table.ObjectID(), Name: name, Position: index + 1, Type: "text"}
		if err := desired.Add(column); err != nil {
			t.Fatal(err)
		}
		if err := desired.AddDependency(column.ObjectID(), table.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == protocol.Unsupported && strings.Contains(strings.Join(result.Unsupported, "\n"), "column_physical_order") {
		t.Fatalf("middle-column drop was incorrectly treated as a physical reorder: %#v", result)
	}
}
