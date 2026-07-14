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
	if result.Status != protocol.Planned || result.ProtocolVersion != protocol.Version || result.CurrentFingerprint == "" || result.DesiredFingerprint == "" {
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
	if !reflect.DeepEqual(result.Statements[0].Hazards, []string{"table_lock"}) {
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "replace_policy", Key: pending.Questions[0].Key, Value: "replace", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
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
		ProtocolVersion:    protocol.Version,
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: extension.ObjectID().String(), Value: "drop"}}}
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

func TestBuildBlocksExtensionSchemaMoveWithoutCompatibilityBridge(t *testing.T) {
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
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_extension_move_required:") {
		t.Fatalf("extension schema move must not become a direct cutover: %#v", planned)
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
	if err != nil || len(unsupported) != 0 || len(attach) != 1 || attach[0].SQL != `ALTER TABLE "app"."events" ATTACH PARTITION "app"."events_2026" FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');` || attach[0].Phase != "migrate" {
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: needed.CurrentFingerprint, DesiredFingerprint: needed.DesiredFingerprint, Answers: []protocol.Answer{{
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
	if planned.Status != protocol.Planned || len(planned.Statements) != 1 || planned.Statements[0].Phase != "migrate" || planned.Statements[0].Manual == nil || len(planned.Batches) != 1 || planned.Batches[0].Transactional {
		t.Fatalf("expected a manual planned statement, got %#v", planned)
	}
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rebuild_materialized_view", Key: after.ObjectID().String(), Value: "rebuild"}}}
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
		ProtocolVersion:    protocol.Version,
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

func TestBuildUsesSemanticDefaultComparator(t *testing.T) {
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
	if result.Status != protocol.Planned || len(result.Statements) != 0 {
		t.Fatalf("equivalent defaults should not plan a mutation: %#v", result)
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
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || !reflect.DeepEqual(pending.Questions[0].Choices, []string{"manual_sql"}) || pending.Questions[0].AllowsFreeform {
		t.Fatalf("type change must expose only the editable SQL handoff: %#v", pending)
	}
	answer := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "type_change", Key: after.ObjectID().String(), Value: "manual_sql"}}}
	plan, err := Build(current, desired, answer, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.NeedsSQLEdits || strings.Index(joined, "ONWARDPG TODO") > strings.Index(joined, "SET DEFAULT 'abc'") {
		t.Fatalf("editable type handoff must precede the new default: %#v\n%s", plan, joined)
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
		ProtocolVersion:    protocol.Version,
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
	if !reflect.DeepEqual(pending.Questions[0].Choices, []string{"manual_sql"}) || pending.Questions[0].AllowsFreeform {
		t.Fatalf("type-change choices contain an unusable shortcut: %#v", pending.Questions[0])
	}
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
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
	if planned.Status != protocol.NeedsSQLEdits || !strings.Contains(joinSQL(planned), "ONWARDPG TODO") || strings.Contains(joinSQL(planned), "ALTER COLUMN \"age\" TYPE") {
		t.Fatalf("same-name type change must become an editable handoff, not direct SQL: %#v", planned)
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

	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsSQLEdits || len(plan.Statements) != 3 {
		t.Fatalf("required new column must create an editable staged plan: %#v", plan)
	}
	if plan.Statements[0].Phase != "expand" || plan.Statements[0].SQL != `ALTER TABLE "app"."customers" ADD COLUMN "email" text;` ||
		plan.Statements[1].Phase != "migrate" || !strings.Contains(plan.Statements[1].SQL, "ONWARDPG TODO") ||
		plan.Statements[2].Phase != "contract" || plan.Statements[2].SQL != `ALTER TABLE "app"."customers" ALTER COLUMN "email" SET NOT NULL;` {
		t.Fatalf("required column phases = %#v", plan.Statements)
	}
	if strings.Contains(plan.Statements[0].SQL, "NOT NULL") {
		t.Fatalf("expand broke old writers: %s", plan.Statements[0].SQL)
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
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
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
	if planned.Batches[0].Phase != "migrate" || planned.Batches[1].Phase != "migrate" || planned.Batches[2].Phase != "contract" {
		t.Fatalf("phase order = %#v", planned.Batches)
	}
	if !strings.Contains(planned.Batches[0].Statements[0].SQL, "NOT VALID") || planned.Batches[1].Statements[0].Manual == nil || !strings.Contains(planned.Batches[2].Statements[0].SQL, "VALIDATE CONSTRAINT") {
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
		ProtocolVersion:    protocol.Version,
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_bridge_required:") {
		t.Fatalf("view rename must not become a direct cutover: %#v", planned)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: oldView.ObjectID().String(), Value: newView.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_bridge_required:") {
		t.Fatalf("dependent view rename must wait for a compatibility bridge: %#v", planned)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_bridge_required:") {
		t.Fatalf("routine rename must not become a direct cutover: %#v", planned)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: oldRoutine.ObjectID().String(), Value: newRoutine.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_bridge_required:") {
		t.Fatalf("trigger-dependent routine rename must wait for a compatibility bridge: %#v", planned)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_trigger", Key: before.ObjectID().String(), Value: after.ObjectID().String()}}}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || !strings.Contains(joinSQL(planned), `ALTER TRIGGER "audit_old" ON "app"."orders" RENAME TO "audit";`) {
		t.Fatalf("unexpected trigger rename plan %#v", planned)
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
		ProtocolVersion:    protocol.Version,
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
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
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
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
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
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
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
		ProtocolVersion: protocol.Version, CurrentFingerprint: pendingAfter.CurrentFingerprint, DesiredFingerprint: pendingAfter.DesiredFingerprint,
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
		ProtocolVersion: protocol.Version, CurrentFingerprint: renamePending.CurrentFingerprint, DesiredFingerprint: renamePending.DesiredFingerprint,
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}}}
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}}}
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: oldTable.ObjectID().String(), Value: newTable.ObjectID().String()}}}
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
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_column", Key: before.ObjectID().String(), Value: after.ObjectID().String()}},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Unsupported || !hasUnsupportedPrefix(planned, "expand_contract_bridge_required:") {
		t.Fatalf("column rename must not become a direct cutover: %#v", planned)
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
		ProtocolVersion:    protocol.Version,
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: schema.ObjectID().String(), Value: "drop"}}}
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
	answers = protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: table.ObjectID().String(), Value: "drop"}}}
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: constraint.ObjectID().String(), Value: "drop"}}}
	plan, err := Build(current, desired, answers, Options{IfExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinSQL(plan), `DROP CONSTRAINT IF EXISTS "orders_account_fkey"`) {
		t.Fatalf("missing IF EXISTS constraint drop: %#v", plan)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: index.ObjectID().String(), Value: "drop"}}}
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
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 1 {
		t.Fatalf("unexpected structured index plan %#v", result)
	}
	want := `CREATE UNIQUE INDEX "orders_lookup_idx" ON "app"."orders" USING "btree" ((lower(name)) "text_pattern_ops" DESC NULLS FIRST) INCLUDE ("id") NULLS NOT DISTINCT WITH ("fillfactor" = 70) WHERE name IS NOT NULL;`
	if got := result.Statements[0].SQL; got != want {
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
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned || len(result.Statements) != 1 || result.Statements[0].SQL != `ALTER TABLE "public"."orders" VALIDATE CONSTRAINT "orders_positive";` || result.Statements[0].Phase != "migrate" {
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

func TestBuildRejectsAtlasUnsupportedGeneratedAndPartitionMutations(t *testing.T) {
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
	if result.Status != protocol.Unsupported || !containsString(result.Unsupported, "partition_rewrite:table:app:orders") || !containsString(result.Unsupported, "generated_column_rewrite:column:app:orders:slug") {
		t.Fatalf("expected explicit rejected mutations, got %#v", result)
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

func TestBuildRejectsEnumLabelDropAndReorder(t *testing.T) {
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
			result, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || result.Unsupported[0] != "enum_rewrite:enum:app:state" {
				t.Fatalf("enum %s must be explicit unsupported: %#v", test.name, result)
			}
		})
	}
}

func TestBuildRejectsConstraintRename(t *testing.T) {
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
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 || result.Unsupported[0] != "constraint_rename:constraint:app:orders:orders_value_check" {
		t.Fatalf("constraint rename must be rejected: %#v", result)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: oldIndex.ObjectID().String(), Value: "drop"}}}
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_column", Key: oldColumn.ObjectID().String(), Value: newColumn.ObjectID().String()}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Unsupported || !hasUnsupportedPrefix(plan, "expand_contract_bridge_required:") {
		t.Fatalf("constraint-backed column rename must wait for a compatibility bridge: %#v", plan)
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
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_column", Key: oldColumn.ObjectID().String(), Value: newColumn.ObjectID().String()}}}
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

func TestPlanRejectsUnreachablePhysicalColumnOrder(t *testing.T) {
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
	if result.Status != protocol.Unsupported || len(result.Unsupported) != 1 {
		t.Fatalf("unreachable column order result = %#v", result)
	}
	joined := strings.Join(result.Unsupported, "\n")
	if !strings.Contains(joined, "inserted_column:timezone:desired=2:last_retained=3") {
		t.Fatalf("unreachable column order diagnostics = %q", joined)
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
