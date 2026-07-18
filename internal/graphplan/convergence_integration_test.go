package graphplan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

func TestCreatePlanConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	const integrationLock int64 = 731095702114
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()
	schemaName := "onwardpg_plan_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	ddl := `CREATE SCHEMA "` + schemaName + `";
COMMENT ON SCHEMA "` + schemaName + `" IS 'application';
CREATE TYPE "` + schemaName + `".state AS ENUM ('open', 'closed');
CREATE SEQUENCE "` + schemaName + `".orders_external_id_seq AS bigint START WITH 41 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 3 CYCLE;
CREATE TABLE "` + schemaName + `".orders (
  id bigint GENERATED ALWAYS AS IDENTITY (START WITH 10 INCREMENT BY 5),
  legacy_id bigserial,
  external_id bigint NOT NULL DEFAULT nextval('"` + schemaName + `".orders_external_id_seq'::regclass),
  state "` + schemaName + `".state NOT NULL DEFAULT 'open',
  note text,
  sort_key text COLLATE "C",
  CONSTRAINT orders_pkey PRIMARY KEY (id)
);
COMMENT ON TABLE "` + schemaName + `".orders IS 'orders';
COMMENT ON COLUMN "` + schemaName + `".orders.note IS 'optional note';
CREATE INDEX orders_state_idx ON "` + schemaName + `".orders (state);`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected planned result: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after applying plan\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestSchemaAndSearchPathConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_schema_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func(currentURL string) *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(currentURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyWithDrops := func(label string, desired *pgschema.Snapshot) protocol.Result {
		t.Helper()
		current := loadCurrent(url)
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				if question.Kind != "drop" {
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan
	}

	fullDDL := `CREATE SCHEMA "` + schemaName + `";
COMMENT ON SCHEMA "` + schemaName + `" IS 'the store';
CREATE TYPE "` + schemaName + `".mood AS ENUM ('happy', 'sad');
CREATE TABLE "` + schemaName + `".person (m "` + schemaName + `".mood);`
	full := loadDesired(fullDDL)
	created := applyWithDrops("schema create", full)
	createdSQL := joinPlan(created)
	if !strings.Contains(createdSQL, `CREATE SCHEMA "`+schemaName+`"`) || !strings.Contains(createdSQL, `COMMENT ON SCHEMA "`+schemaName+`" IS 'the store'`) || !strings.Contains(createdSQL, schemaName+".mood") {
		t.Fatalf("schema creation, comment, or qualified type was lost:\n%s", createdSQL)
	}

	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	emptySearchPathURL := url + separator + "options=-csearch_path%3D"
	normal := loadCurrent(url)
	hardened := loadCurrent(emptySearchPathURL)
	normalFingerprint, _ := normal.Fingerprint()
	hardenedFingerprint, _ := hardened.Fingerprint()
	if normalFingerprint != hardenedFingerprint {
		t.Fatalf("catalog introspection changed under an empty search_path: normal=%s hardened=%s", normalFingerprint, hardenedFingerprint)
	}
	unchanged, err := Build(hardened, full, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("empty-search_path identical graph plan=%#v err=%v", unchanged, err)
	}

	schemaOnly := loadDesired(`CREATE SCHEMA "` + schemaName + `"; COMMENT ON SCHEMA "` + schemaName + `" IS 'the store';`)
	applyWithDrops("schema contents dropped", schemaOnly)
	dropped := applyWithDrops("schema dropped", loadDesired(""))
	if !strings.Contains(joinPlan(dropped), `DROP SCHEMA "`+schemaName+`"`) {
		t.Fatalf("schema drop missing:\n%s", joinPlan(dropped))
	}
	empty := loadDesired("")
	noWork, err := Build(loadCurrent(url), empty, protocol.Answers{}, Options{})
	if err != nil || noWork.Status != protocol.Planned || len(noWork.Statements) != 0 {
		t.Fatalf("identical empty databases generated work: plan=%#v err=%v", noWork, err)
	}
}

func TestViewCreateAndReplaceConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_view_plan_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, amount bigint NOT NULL);"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	createDDL := baseDDL +
		"CREATE VIEW " + quote(schemaName) + ".order_amounts WITH (security_barrier=true) AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"COMMENT ON VIEW " + quote(schemaName) + ".order_amounts IS 'orders visible to reporting';"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(createDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE VIEW") {
		t.Fatalf("expected view-create plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	renameDDL := baseDDL +
		"CREATE VIEW " + quote(schemaName) + ".reporting_orders WITH (security_barrier=true) AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"COMMENT ON VIEW " + quote(schemaName) + ".reporting_orders IS 'orders visible to reporting';"
	if err := os.WriteFile(path, []byte(renameDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_view" {
		t.Fatalf("expected view rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)

	// Keep the independent CREATE OR REPLACE coverage below. A direct rename is
	// fixture setup here, not SQL onwardpg is allowed to emit as a successful
	// expand/contract plan.
	if _, err := conn.Exec(ctx, "ALTER VIEW "+quote(schemaName)+".order_amounts RENAME TO reporting_orders"); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, "CREATE OR REPLACE VIEW "+quote(schemaName)+".reporting_orders WITH (security_barrier=false) AS SELECT id, amount * 100 AS amount FROM "+quote(schemaName)+".orders;"); err != nil {
		t.Fatal(err)
	}
	changedDDL := baseDDL +
		"CREATE VIEW " + quote(schemaName) + ".reporting_orders WITH (security_barrier=true) AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"COMMENT ON VIEW " + quote(schemaName) + ".reporting_orders IS 'orders visible to reporting';"
	if err := os.WriteFile(path, []byte(changedDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE VIEW") {
		t.Fatalf("expected view-replace plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestViewOptionsAndDependencyChainsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	dataSchema := "onwardpg_view_data_" + suffix
	apiSchema := "onwardpg_view_api_" + suffix
	defer func() {
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+apiSchema+`" CASCADE`)
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+dataSchema+`" CASCADE`)
	}()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA "`+dataSchema+`"; CREATE SCHEMA "`+apiSchema+`";`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				if question.Kind != "drop" {
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}

	prefix := `CREATE SCHEMA "` + dataSchema + `"; CREATE SCHEMA "` + apiSchema + `";
CREATE TABLE "` + dataSchema + `".t (x integer);`
	views := func(baseOptions, baseExpression, checkedSuffix string) string {
		options := ""
		if baseOptions != "" {
			options = " WITH (" + baseOptions + ")"
		}
		return prefix + `
CREATE VIEW "` + dataSchema + `".base` + options + ` AS SELECT ` + baseExpression + ` AS x FROM "` + dataSchema + `".t;
COMMENT ON VIEW "` + dataSchema + `".base IS 'base view';
CREATE VIEW "` + apiSchema + `".derived AS SELECT x FROM "` + dataSchema + `".base;
CREATE VIEW "` + apiSchema + `".top AS SELECT x FROM "` + apiSchema + `".derived;
CREATE VIEW "` + apiSchema + `".checked AS SELECT x FROM "` + dataSchema + `".t` + checkedSuffix + `;`
	}
	initial := views("security_invoker=true, security_barrier=true", "x", "")
	created, desired := applyDesired("view dependency creation", initial)
	createdSQL := joinPlan(created)
	baseAt := strings.Index(createdSQL, `CREATE VIEW "`+dataSchema+`"."base"`)
	derivedAt := strings.Index(createdSQL, `CREATE VIEW "`+apiSchema+`"."derived"`)
	topAt := strings.Index(createdSQL, `CREATE VIEW "`+apiSchema+`"."top"`)
	if baseAt < 0 || derivedAt < 0 || topAt < 0 || baseAt > derivedAt || derivedAt > topAt || !strings.Contains(createdSQL, "security_barrier") || !strings.Contains(createdSQL, "COMMENT ON VIEW") {
		t.Fatalf("view creation/dependency order was incomplete:\n%s", createdSQL)
	}

	reorderedOptions := views("security_barrier=true, security_invoker=true", "x", "")
	reordered := loadDesired(reorderedOptions)
	unchanged, err := Build(loadCurrent(), reordered, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("view option ordering or unchanged chain generated work: plan=%#v err=%v", unchanged, err)
	}
	_ = desired

	removed, _ := applyDesired("view option removed", views("", "x", ""))
	if !strings.Contains(joinPlan(removed), "CREATE OR REPLACE VIEW") {
		t.Fatalf("view option removal did not replace the view:\n%s", joinPlan(removed))
	}
	added, _ := applyDesired("view options added", views("security_barrier=true", "x", " WITH CASCADED CHECK OPTION"))
	if !strings.Contains(joinPlan(added), "security_barrier") || !strings.Contains(joinPlan(added), "check_option") || !strings.Contains(joinPlan(added), "cascaded") {
		t.Fatalf("view option additions missing:\n%s", joinPlan(added))
	}
	replaced, _ := applyDesired("base view definition changed", views("security_barrier=true", "x + 1", " WITH CASCADED CHECK OPTION"))
	if !strings.Contains(joinPlan(replaced), `CREATE OR REPLACE VIEW "`+dataSchema+`"."base"`) {
		t.Fatalf("view definition replacement missing:\n%s", joinPlan(replaced))
	}

	dropped, _ := applyDesired("view chain dropped", prefix)
	dropSQL := joinPlan(dropped)
	baseDrop := strings.Index(dropSQL, `DROP VIEW "`+dataSchema+`"."base"`)
	derivedDrop := strings.Index(dropSQL, `DROP VIEW "`+apiSchema+`"."derived"`)
	topDrop := strings.Index(dropSQL, `DROP VIEW "`+apiSchema+`"."top"`)
	if baseDrop < 0 || derivedDrop < 0 || topDrop < 0 || topDrop > derivedDrop || derivedDrop > baseDrop {
		t.Fatalf("dependent view drop order was unsafe:\n%s", dropSQL)
	}
}

func TestViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	dataSchema := "onwardpg_view_type_data_" + suffix
	apiSchema := "onwardpg_view_type_api_" + suffix
	defer func() {
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+apiSchema+`" CASCADE`)
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+dataSchema+`" CASCADE`)
	}()
	currentDDL := `CREATE SCHEMA "` + dataSchema + `"; CREATE SCHEMA "` + apiSchema + `";
CREATE TABLE "` + dataSchema + `".t (keep integer, val integer);
CREATE VIEW "` + apiSchema + `".base AS SELECT val FROM "` + dataSchema + `".t;
CREATE VIEW "` + apiSchema + `".derived AS SELECT val FROM "` + apiSchema + `".base;
CREATE VIEW "` + apiSchema + `".keep_view AS SELECT keep FROM "` + dataSchema + `".t;`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := strings.Replace(currentDDL, "val integer", "val bigint", 1)
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("view-column type change must enter the reviewed handoff: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "type_change", Key: pending.Questions[0].Key, Value: "manual_sql", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.NeedsSQLEdits {
		t.Fatalf("view-column type change handoff plan=%#v err=%v", plan, err)
	}
	sql := joinPlan(plan)
	if !strings.Contains(sql, "ONWARDPG TODO") || !strings.Contains(sql, "view:"+apiSchema+":base") || !strings.Contains(sql, "view:"+apiSchema+":derived") || strings.Contains(sql, "view:"+apiSchema+":keep_view") {
		t.Fatalf("view-column dependency handoff scope/order was incorrect:\n%s", sql)
	}
}

func TestMaterializedViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_matview_type_%d", time.Now().UTC().UnixNano())
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + q + ";" +
		"CREATE TABLE " + q + ".t (keep integer, val integer);" +
		"CREATE MATERIALIZED VIEW " + q + ".direct AS SELECT val FROM " + q + ".t WITH NO DATA;" +
		"CREATE UNIQUE INDEX direct_val_idx ON " + q + ".direct (val);" +
		"CREATE MATERIALIZED VIEW " + q + ".derived AS SELECT val FROM " + q + ".direct WITH NO DATA;" +
		"CREATE MATERIALIZED VIEW " + q + ".whole_row AS SELECT t FROM " + q + ".t WITH NO DATA;" +
		"CREATE MATERIALIZED VIEW " + q + ".keep_only AS SELECT keep FROM " + q + ".t WITH NO DATA;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := strings.Replace(currentDDL, "val integer", "val bigint", 1)
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("materialized-view column type change must enter the reviewed handoff: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "type_change", Key: pending.Questions[0].Key, Value: "manual_sql", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.NeedsSQLEdits {
		t.Fatalf("materialized-view type-change handoff plan=%#v err=%v", plan, err)
	}
	sql := joinPlan(plan)
	for _, id := range []string{"materialized_view:" + schemaName + ":direct", "materialized_view:" + schemaName + ":derived", "materialized_view:" + schemaName + ":whole_row", "index:" + schemaName + ":direct:direct_val_idx"} {
		if !strings.Contains(sql, id) {
			t.Fatalf("materialized-view handoff omitted %s:\n%s", id, sql)
		}
	}
	if strings.Contains(sql, "materialized_view:"+schemaName+":keep_only") {
		t.Fatalf("materialized-view handoff included an unaffected column dependency:\n%s", sql)
	}
}

func TestChangedViewRequiresManualDependentMaterializedViewRefresh(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_matview_refresh_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, amount bigint NOT NULL);" +
		"INSERT INTO " + quote(schemaName) + ".orders VALUES (1, 10);"
	currentDDL := baseDDL +
		"CREATE VIEW " + quote(schemaName) + ".order_totals AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_cache AS SELECT * FROM " + quote(schemaName) + ".order_totals;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"CREATE VIEW " + quote(schemaName) + ".order_totals AS SELECT id, amount * 2 AS amount FROM " + quote(schemaName) + ".orders;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_cache AS SELECT * FROM " + quote(schemaName) + ".order_totals;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "refresh_materialized_view" {
		t.Fatalf("expected dependent materialized-view refresh contract, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{
		Kind: pending.Questions[0].Kind, Key: pending.Questions[0].Key, Value: "provided",
		Manual: &protocol.ManualWork{Summary: "refresh order cache after replacing its source view", ExecutionMode: "transactional", Statements: []string{
			"REFRESH MATERIALIZED VIEW " + quote(schemaName) + ".order_cache;",
		}, VerificationSQL: []string{"SELECT amount = 20 FROM " + quote(schemaName) + ".order_cache WHERE id = 1;"}},
	}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Batches) != 2 || plan.Batches[1].Phase != protocol.PhaseContract || !strings.Contains(joinSQL(plan), "REFRESH MATERIALIZED VIEW") {
		t.Fatalf("expected view replacement followed by a manual refresh batch, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	var refreshed bool
	if err := conn.QueryRow(ctx, "SELECT amount = 20 FROM "+quote(schemaName)+".order_cache WHERE id = 1").Scan(&refreshed); err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Fatal("manual refresh did not materialize the changed view data")
	}
	assertGraphConverges(t, ctx, url, desired)
}

func TestDependentViewRenameRequiresCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_dependent_view_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, amount bigint NOT NULL);" +
		"CREATE VIEW " + quote(schemaName) + ".orders_view AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"CREATE VIEW " + quote(schemaName) + ".dashboard AS SELECT id FROM " + quote(schemaName) + ".orders_view;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, amount bigint NOT NULL);" +
		"CREATE VIEW " + quote(schemaName) + ".reporting_orders AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"CREATE VIEW " + quote(schemaName) + ".dashboard AS SELECT id FROM " + quote(schemaName) + ".reporting_orders;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_view" {
		t.Fatalf("expected dependent view rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)
}

func TestPartitionAttachAndDetachConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_attach_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 (id bigint NOT NULL, occurred_at date NOT NULL);" +
		"ALTER TABLE " + quote(schemaName) + ".events_2026 ADD CONSTRAINT events_2026_range CHECK (occurred_at >= DATE '2026-01-01' AND occurred_at < DATE '2027-01-01');"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	attachDDL := baseDDL +
		"ALTER TABLE " + quote(schemaName) + ".events ATTACH PARTITION " + quote(schemaName) + ".events_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if err := os.WriteFile(path, []byte(attachDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ATTACH PARTITION") {
		t.Fatalf("expected partition-attach plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DETACH PARTITION") {
		t.Fatalf("expected partition-detach plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestManualPartitionReconfigurationContractConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_manual_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');"
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "partition_reconfiguration" {
		t.Fatalf("expected manual-contract question, got %#v", pending)
	}
	childID := pgschema.Table{Schema: schemaName, Name: "events_2026"}.ObjectID().String()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{
		Kind: "partition_reconfiguration", Key: childID, Value: "provided",
		Manual: &protocol.ManualWork{Summary: "move the empty partition window", ExecutionMode: "transactional", Statements: []string{
			"ALTER TABLE " + quote(schemaName) + ".events DETACH PARTITION " + quote(schemaName) + ".events_2026;",
			"ALTER TABLE " + quote(schemaName) + ".events ATTACH PARTITION " + quote(schemaName) + ".events_2026 FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');",
		}, VerificationSQL: []string{"SELECT 1;"}},
	}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Batches) != 1 || plan.Batches[0].Phase != protocol.PhaseContract {
		t.Fatalf("expected manual plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestManualPartitionStrategyChangePreservesPrimaryKeyOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_partition_strategy_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "events") + " (id integer NOT NULL) PARTITION BY RANGE (id);" +
		"ALTER TABLE " + qualified(schemaName, "events") + " ADD CONSTRAINT events_pkey PRIMARY KEY (id);"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "events") + " (id integer NOT NULL) PARTITION BY HASH (id);" +
		"ALTER TABLE " + qualified(schemaName, "events") + " ADD CONSTRAINT events_pkey PRIMARY KEY (id);"
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "partition_reconfiguration" {
		t.Fatalf("expected partition-strategy manual contract, got %#v", pending)
	}
	tableID := pgschema.Table{Schema: schemaName, Name: "events"}.ObjectID().String()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{
		Kind: "partition_reconfiguration", Key: tableID, Value: "provided", QuestionFingerprint: pending.Questions[0].ScopeFingerprint,
		Manual: &protocol.ManualWork{Summary: "replace the empty partitioned parent while preserving its primary key", ExecutionMode: "transactional", Statements: []string{
			"DROP TABLE " + qualified(schemaName, "events") + ";",
			"CREATE TABLE " + qualified(schemaName, "events") + " (id integer NOT NULL) PARTITION BY HASH (id);",
			"ALTER TABLE " + qualified(schemaName, "events") + " ADD CONSTRAINT events_pkey PRIMARY KEY (id);",
		}, VerificationSQL: []string{"SELECT 1;"}},
	}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Batches) != 1 || plan.Batches[0].Phase != protocol.PhaseContract || !strings.Contains(joinPlan(plan), "ADD CONSTRAINT events_pkey PRIMARY KEY (id)") {
		t.Fatalf("expected executable partition-strategy handoff preserving the primary key, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestManualDefaultPartitionReconfigurationContractConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_default_manual_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_other PARTITION OF " + quote(schemaName) + ".events DEFAULT;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_other PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "partition_reconfiguration" {
		t.Fatalf("expected default-partition manual question, got %#v", pending)
	}
	childID := pgschema.Table{Schema: schemaName, Name: "events_other"}.ObjectID().String()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{
		Kind: "partition_reconfiguration", Key: childID, Value: "provided",
		Manual: &protocol.ManualWork{Summary: "validate the default partition is empty, then narrow its bound", ExecutionMode: "transactional", Statements: []string{
			"ALTER TABLE " + quote(schemaName) + ".events DETACH PARTITION " + quote(schemaName) + ".events_other;",
			"ALTER TABLE " + quote(schemaName) + ".events ATTACH PARTITION " + quote(schemaName) + ".events_other FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');",
		}, VerificationSQL: []string{"SELECT 1;"}},
	}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Batches) != 1 || plan.Batches[0].Phase != protocol.PhaseContract {
		t.Fatalf("expected manual default-partition plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedIndexAndConstraintCreateConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_hierarchy_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');" +
		"ALTER TABLE " + quote(schemaName) + ".events ADD CONSTRAINT events_pkey PRIMARY KEY (id, occurred_at);" +
		"CREATE INDEX events_id_idx ON " + quote(schemaName) + ".events (id);"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "CREATE INDEX") != 1 {
		t.Fatalf("expected a parent index statement, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedIndexAndConstraintDropConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_hierarchy_drop_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	currentDDL := baseDDL +
		"ALTER TABLE " + quote(schemaName) + ".events ADD CONSTRAINT events_pkey PRIMARY KEY (id, occurred_at);" +
		"CREATE INDEX events_id_idx ON " + quote(schemaName) + ".events (id);"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 2 {
		t.Fatalf("expected exactly parent destructive questions, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop"})
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP INDEX") != 1 || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 {
		t.Fatalf("expected parent-only drop plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedInheritedCheckDropConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_check_drop_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint, kind text) PARTITION BY RANGE (id);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM (0) TO (100);"
	if _, err := conn.Exec(ctx, baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_kind_check CHECK (kind <> 'bad');"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected exactly the parent destructive question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: pending.Questions[0].Kind, Key: pending.Questions[0].Key, Value: "drop"}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 {
		t.Fatalf("expected only the parent CHECK drop, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedInheritedCheckCreateAndRebuildConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_check_create_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint, kind text) PARTITION BY RANGE (id);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM (0) TO (100);"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	firstDDL := baseDDL + "ALTER TABLE " + quote(schemaName) + ".events ADD CONSTRAINT events_kind_check CHECK (kind <> 'bad');"
	firstPath := filepath.Join(t.TempDir(), "first.sql")
	if err := os.WriteFile(firstPath, []byte(firstDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.LoadGraph(ctx, source.Parse("file://"+firstPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, first, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "ADD CONSTRAINT") != 1 {
		t.Fatalf("expected only parent CHECK creation, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, first)

	secondDDL := baseDDL + "ALTER TABLE " + quote(schemaName) + ".events ADD CONSTRAINT events_kind_check CHECK (kind <> 'blocked');"
	secondPath := filepath.Join(t.TempDir(), "second.sql")
	if err := os.WriteFile(secondPath, []byte(secondDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.LoadGraph(ctx, source.Parse("file://"+secondPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, second, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 || strings.Count(joinSQL(plan), "ADD CONSTRAINT") != 1 {
		t.Fatalf("expected parent-only CHECK rebuild, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, second)
}

func TestPartitionedParentIndexRebuildConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_index_rebuild_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, baseDDL+"CREATE INDEX events_lookup_idx ON "+quote(schemaName)+".events (id);"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL+"CREATE INDEX events_lookup_idx ON "+quote(schemaName)+".events (occurred_at);"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP INDEX") != 1 || strings.Count(joinSQL(plan), "CREATE INDEX") != 1 {
		t.Fatalf("expected parent-only partitioned index rebuild, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestContinuousMaterializedViewAndLocalPartitionIndexReplacementConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_continuous_special_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint, tenant_id bigint);
CREATE MATERIALIZED VIEW "` + schemaName + `".account_cache AS SELECT id, tenant_id FROM "` + schemaName + `".accounts;
CREATE INDEX account_cache_lookup_idx ON "` + schemaName + `".account_cache (id);
CREATE TABLE "` + schemaName + `".events (id bigint, created_at timestamptz) PARTITION BY RANGE (created_at);
CREATE TABLE "` + schemaName + `".events_2026 PARTITION OF "` + schemaName + `".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE INDEX events_2026_lookup_idx ON "` + schemaName + `".events_2026 (id);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint, tenant_id bigint);
CREATE MATERIALIZED VIEW "` + schemaName + `".account_cache AS SELECT id, tenant_id FROM "` + schemaName + `".accounts;
CREATE INDEX account_cache_lookup_idx ON "` + schemaName + `".account_cache (id, tenant_id);
CREATE TABLE "` + schemaName + `".events (id bigint, created_at timestamptz) PARTITION BY RANGE (created_at);
CREATE TABLE "` + schemaName + `".events_2026 PARTITION OF "` + schemaName + `".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE INDEX events_2026_lookup_idx ON "` + schemaName + `".events_2026 (id, created_at DESC);`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("continuous special-index plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	for _, index := range []string{"account_cache_lookup_idx", "events_2026_lookup_idx"} {
		if !strings.Contains(joined, "ALTER INDEX "+qualified(schemaName, index)+" RENAME TO \"onwardpg_tmpidx_") ||
			!strings.Contains(joined, "CREATE INDEX CONCURRENTLY "+quote(index)) ||
			!strings.Contains(joined, "DROP INDEX CONCURRENTLY "+quote(schemaName)+".\"onwardpg_tmpidx_") {
			t.Fatalf("index %s did not receive continuous replacement:\n%s", index, joined)
		}
	}
	for _, statement := range plan.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("continuous statement lacks timeout guidance: %#v", statement)
		}
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestContinuousPartitionedParentIndexReplacementConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_continuous_parent_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".events (account_id bigint, created_at timestamptz) PARTITION BY RANGE (created_at);
CREATE TABLE "` + schemaName + `".events_2025 PARTITION OF "` + schemaName + `".events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE "` + schemaName + `".events_2026 PARTITION OF "` + schemaName + `".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');`
	currentDDL := base + `CREATE INDEX events_lookup_idx ON "` + schemaName + `".events (account_id);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := base + `CREATE INDEX events_lookup_idx ON "` + schemaName + `".events (account_id, created_at DESC);`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || plan.Status != protocol.Planned || len(plan.Statements) != 9 {
		t.Fatalf("continuous partition-parent plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	for _, fragment := range []string{
		`CREATE INDEX "events_lookup_idx" ON ONLY "` + schemaName + `"."events"`,
		`CREATE INDEX CONCURRENTLY "events_2025_account_id_created_at_idx"`,
		`CREATE INDEX CONCURRENTLY "events_2026_account_id_created_at_idx"`,
		`ALTER INDEX "` + schemaName + `"."events_lookup_idx" ATTACH PARTITION`,
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("partition-parent plan missing %q:\n%s", fragment, joined)
		}
	}
	relations := []string{"events", "events_2025", "events_2026"}
	assertUsable := func(queryer rowQuerier) {
		for _, relation := range relations {
			var count int
			if err := queryer.QueryRow(ctx, `
SELECT count(*) FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND i.indisvalid AND i.indisready`, schemaName, relation).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count < 1 {
				t.Fatalf("committed replacement boundary left %s.%s without a usable index", schemaName, relation)
			}
		}
	}
	assertUsable(conn)
	for _, batch := range plan.Batches {
		if !batch.Transactional {
			for _, statement := range batch.Statements {
				if _, err := conn.Exec(ctx, statement.SQL); err != nil {
					t.Fatalf("apply %q: %v", statement.SQL, err)
				}
				assertUsable(conn)
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range batch.Statements {
			if _, err := tx.Exec(ctx, statement.SQL); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("apply %q: %v", statement.SQL, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		assertUsable(conn)
	}
	assertGraphConverges(t, ctx, url, desired)
}

func TestContinuousNestedPartitionedIndexReplacementConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_nested_index_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".events (account_id bigint, created_at timestamptz) PARTITION BY RANGE (created_at);
CREATE TABLE "` + schemaName + `".events_2026 PARTITION OF "` + schemaName + `".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') PARTITION BY HASH (account_id);
CREATE TABLE "` + schemaName + `".events_2026_0 PARTITION OF "` + schemaName + `".events_2026 FOR VALUES WITH (MODULUS 2, REMAINDER 0);
CREATE TABLE "` + schemaName + `".events_2026_1 PARTITION OF "` + schemaName + `".events_2026 FOR VALUES WITH (MODULUS 2, REMAINDER 1);`
	if _, err := conn.Exec(ctx, base+`CREATE INDEX events_lookup_idx ON "`+schemaName+`".events (account_id);`); err != nil {
		t.Fatal(err)
	}
	desiredDDL := base + `CREATE INDEX events_lookup_idx ON "` + schemaName + `".events (account_id, created_at DESC);`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("nested continuous plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	if strings.Count(joined, " ON ONLY ") != 2 || strings.Count(joined, "CREATE INDEX CONCURRENTLY") != 2 || strings.Count(joined, "ATTACH PARTITION") != 3 {
		t.Fatalf("nested hierarchy did not receive recursive shell/build/attach planning:\n%s", joined)
	}
	for _, statement := range plan.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("nested replacement statement lacks timeout guidance: %#v", statement)
		}
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

// Adapted from Stripe pg-schema-diff v1.0.7's MIT-licensed
// "Attach an unnattached, invalid index" acceptance case. The typo is retained
// in the source-case name only; onwardpg's assertion is semantic convergence.
func TestAttachExistingPartitionIndexConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_attach_index_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `"."Foobar" (id integer, foo varchar(255), PRIMARY KEY (foo)) PARTITION BY LIST (foo);
CREATE TABLE "` + schemaName + `".foobar_1 PARTITION OF "` + schemaName + `"."Foobar" FOR VALUES IN ('foo_1');
CREATE TABLE "` + schemaName + `".foobar_2 PARTITION OF "` + schemaName + `"."Foobar" FOR VALUES IN ('foo_2');
CREATE TABLE "` + schemaName + `"."Foobar_3" PARTITION OF "` + schemaName + `"."Foobar" FOR VALUES IN ('foo_3');
CREATE INDEX "Partitioned_Idx" ON ONLY "` + schemaName + `"."Foobar" (foo);
CREATE INDEX foobar_1_part ON "` + schemaName + `".foobar_1 (foo);
ALTER INDEX "` + schemaName + `"."Partitioned_Idx" ATTACH PARTITION "` + schemaName + `".foobar_1_part;
CREATE INDEX foobar_2_part ON "` + schemaName + `".foobar_2 (foo);
ALTER INDEX "` + schemaName + `"."Partitioned_Idx" ATTACH PARTITION "` + schemaName + `".foobar_2_part;
CREATE INDEX "Foobar_3_Part" ON "` + schemaName + `"."Foobar_3" (foo);`
	if _, err := conn.Exec(ctx, base); err != nil {
		t.Fatal(err)
	}
	desiredDDL := base + `ALTER INDEX "` + schemaName + `"."Partitioned_Idx" ATTACH PARTITION "` + schemaName + `"."Foobar_3_Part";`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := current.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("incomplete partitioned index must be represented by attachment topology, not blocked: %#v", unsupported)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || plan.Status != protocol.Planned || len(plan.Statements) != 1 {
		t.Fatalf("partition attach plan=%#v err=%v", plan, err)
	}
	if got := plan.Statements[0].SQL; got != `ALTER INDEX "`+schemaName+`"."Partitioned_Idx" ATTACH PARTITION "`+schemaName+`"."Foobar_3_Part";` {
		t.Fatalf("attach SQL=%q", got)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

// Adapted from Stripe pg-schema-diff v1.0.7's MIT-licensed "Add primary key
// to partition using existing index" and unique-constraint sibling cases.
func TestAttachExistingPartitionConstraintIndexesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_attach_constraint_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".pk_events (id integer NOT NULL, bucket text NOT NULL) PARTITION BY LIST (bucket);
CREATE TABLE "` + schemaName + `".pk_events_1 PARTITION OF "` + schemaName + `".pk_events FOR VALUES IN ('one');
ALTER TABLE ONLY "` + schemaName + `".pk_events ADD CONSTRAINT pk_events_pkey PRIMARY KEY (bucket, id);
CREATE UNIQUE INDEX pk_events_1_pkey ON "` + schemaName + `".pk_events_1 (bucket, id);
CREATE TABLE "` + schemaName + `".uq_events (id integer NOT NULL, bucket text NOT NULL) PARTITION BY LIST (bucket);
CREATE TABLE "` + schemaName + `".uq_events_1 PARTITION OF "` + schemaName + `".uq_events FOR VALUES IN ('one');
ALTER TABLE ONLY "` + schemaName + `".uq_events ADD CONSTRAINT uq_events_key UNIQUE (bucket, id);
CREATE UNIQUE INDEX uq_events_1_key ON "` + schemaName + `".uq_events_1 (bucket, id);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := currentDDL + `
ALTER TABLE "` + schemaName + `".pk_events_1 ADD CONSTRAINT pk_events_1_pkey PRIMARY KEY USING INDEX pk_events_1_pkey;
ALTER INDEX "` + schemaName + `".pk_events_pkey ATTACH PARTITION "` + schemaName + `".pk_events_1_pkey;
ALTER TABLE "` + schemaName + `".uq_events_1 ADD CONSTRAINT uq_events_1_key UNIQUE USING INDEX uq_events_1_key;
ALTER INDEX "` + schemaName + `".uq_events_key ATTACH PARTITION "` + schemaName + `".uq_events_1_key;`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || plan.Status != protocol.Planned || len(plan.Statements) != 4 {
		t.Fatalf("partition constraint attachment plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	if strings.Count(joined, "ADD CONSTRAINT") != 2 || strings.Count(joined, "ATTACH PARTITION") != 2 {
		t.Fatalf("constraint claim/attach ordering is invalid:\n%s", joined)
	}
	for i := 0; i < len(plan.Statements); i += 2 {
		if !strings.Contains(plan.Statements[i].SQL, " ADD CONSTRAINT ") || !strings.Contains(plan.Statements[i+1].SQL, " ATTACH PARTITION ") {
			t.Fatalf("constraint claim must immediately precede its attachment: %#v", plan.Statements)
		}
	}
	for _, statement := range plan.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("constraint attachment lacks timeout guidance: %#v", statement)
		}
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func TestContinuousPrimaryAndUniqueConstraintReplacementConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_continuous_constraint_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (
  id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  email text NOT NULL,
  CONSTRAINT accounts_pkey PRIMARY KEY (id),
  CONSTRAINT accounts_email_key UNIQUE (email)
);
COMMENT ON CONSTRAINT accounts_pkey ON "` + schemaName + `".accounts IS 'stable primary identity';`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (
  id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  email text NOT NULL,
  CONSTRAINT accounts_pkey PRIMARY KEY (id, tenant_id),
  CONSTRAINT accounts_email_key UNIQUE (email, tenant_id)
);
COMMENT ON CONSTRAINT accounts_pkey ON "` + schemaName + `".accounts IS 'stable primary identity';`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("continuous constraint plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	if strings.Count(joined, "CREATE UNIQUE INDEX CONCURRENTLY") != 2 || strings.Count(joined, "DROP CONSTRAINT") != 2 || strings.Count(joined, "USING INDEX \"onwardpg_tmpidx_") != 2 {
		t.Fatalf("constraints did not receive continuous index swaps:\n%s", joined)
	}
	for _, statement := range plan.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("continuous constraint statement lacks timeout guidance: %#v", statement)
		}
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestContinuousConstraintReplacementRejectsForeignKeyDependents(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_constraint_dependent_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint NOT NULL, tenant_id bigint NOT NULL, CONSTRAINT accounts_pkey PRIMARY KEY (id));
CREATE TABLE "` + schemaName + `".orders (account_id bigint REFERENCES "` + schemaName + `".accounts(id));`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint NOT NULL, tenant_id bigint NOT NULL, CONSTRAINT accounts_pkey PRIMARY KEY (id, tenant_id));
CREATE TABLE "` + schemaName + `".orders (account_id bigint);`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || pending.Status != protocol.NeedsInput {
		t.Fatalf("foreign-key drop should ask before strategy rejection: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		if question.Kind != "drop" {
			t.Fatalf("unexpected dependent-constraint question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
	}
	result, err := Build(current, desired, answers, Options{ConcurrentIndexes: true})
	if err != nil || result.Status != protocol.Unsupported || len(result.Unsupported) == 0 || !strings.Contains(strings.Join(result.Unsupported, ","), "continuous_constraint_replacement_dependents") {
		t.Fatalf("foreign-key dependent swap must reject explicitly: plan=%#v err=%v", result, err)
	}
}

func TestPartitionedParentPrimaryKeyRebuildConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_pk_rebuild_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL, shard integer NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_pkey PRIMARY KEY (id, occurred_at);"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_pkey PRIMARY KEY (id, occurred_at, shard);"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 || strings.Count(joinSQL(plan), "ADD CONSTRAINT") != 1 {
		t.Fatalf("expected parent-only partitioned primary-key rebuild, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedParentUniqueRebuildConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_unique_rebuild_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL, shard integer NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_unique UNIQUE (id, occurred_at);"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_unique UNIQUE (id, occurred_at, shard);"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 || strings.Count(joinSQL(plan), "ADD CONSTRAINT") != 1 {
		t.Fatalf("expected parent-only partitioned unique rebuild, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedParentForeignKeyRebuildConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_fk_rebuild_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".accounts (id bigint PRIMARY KEY);" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, account_id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_account_fkey FOREIGN KEY (account_id) REFERENCES "+quote(schemaName)+".accounts(id);"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_account_fkey FOREIGN KEY (account_id) REFERENCES "+quote(schemaName)+".accounts(id) ON DELETE CASCADE;"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 || strings.Count(joinSQL(plan), "ADD CONSTRAINT") != 1 {
		t.Fatalf("expected parent-only partitioned foreign-key rebuild, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestPartitionedParentExclusionRebuildConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	requirePostgresVersion(t, ctx, conn, 170000, "partitioned exclusion constraints")
	schemaName := "onwardpg_partition_exclusion_rebuild_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL, shard integer NOT NULL) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_excl EXCLUDE USING btree (id WITH =, occurred_at WITH =);"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL+"ALTER TABLE "+quote(schemaName)+".events ADD CONSTRAINT events_excl EXCLUDE USING btree (id WITH =, occurred_at WITH =, shard WITH =);"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP CONSTRAINT") != 1 || strings.Count(joinSQL(plan), "ADD CONSTRAINT") != 1 {
		t.Fatalf("expected parent-only partitioned exclusion rebuild, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestDefaultPartitionAttachDetachConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_default_partition_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL) PARTITION BY LIST (id);" +
		"CREATE TABLE " + quote(schemaName) + ".events_default (id bigint NOT NULL);"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	attachDDL := baseDDL + "ALTER TABLE " + quote(schemaName) + ".events ATTACH PARTITION " + quote(schemaName) + ".events_default DEFAULT;"
	if err := os.WriteFile(path, []byte(attachDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ATTACH PARTITION") || !strings.Contains(joinSQL(plan), " DEFAULT;") {
		t.Fatalf("expected default partition attach plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DETACH PARTITION") {
		t.Fatalf("expected default partition detach plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_plan_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, note text);"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	fullDDL := baseDDL +
		"CREATE FUNCTION " + quote(schemaName) + ".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.note := coalesce(NEW.note, 'created'); RETURN NEW; END $$;" +
		"CREATE TRIGGER audit_orders BEFORE INSERT OR UPDATE OF note ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit_orders();"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(fullDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE FUNCTION") || !strings.Contains(joinSQL(plan), "CREATE TRIGGER") {
		t.Fatalf("expected routine/trigger create plan, got %#v", plan)
	}
	if strings.Index(joinSQL(plan), "CREATE OR REPLACE FUNCTION") > strings.Index(joinSQL(plan), "CREATE TRIGGER") {
		t.Fatalf("routine must precede trigger: %s", joinSQL(plan))
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	commentedDDL := fullDDL + "COMMENT ON FUNCTION " + quote(schemaName) + ".audit_orders() IS 'maintains order notes';"
	if err := os.WriteFile(path, []byte(commentedDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 1 || !strings.Contains(joinSQL(plan), "COMMENT ON FUNCTION") {
		t.Fatalf("expected routine comment plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if _, err := conn.Exec(ctx, "CREATE OR REPLACE FUNCTION "+quote(schemaName)+".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.note := 'changed'; RETURN NEW; END $$;"); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE FUNCTION") {
		t.Fatalf("expected routine replacement plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if _, err := conn.Exec(ctx, "ALTER TABLE "+quote(schemaName)+".orders DISABLE TRIGGER audit_orders;"); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ENABLE TRIGGER") {
		t.Fatalf("expected trigger-enable plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 2 {
		t.Fatalf("expected routine and trigger drop confirmations, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop"})
	}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Index(joinSQL(plan), "DROP TRIGGER") > strings.Index(joinSQL(plan), "DROP FUNCTION") {
		t.Fatalf("expected trigger before routine drop, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestTriggerEnableStatesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_trigger_states_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE FUNCTION " + quote(schemaName) + ".audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"
	triggerDDL := baseDDL +
		"CREATE TRIGGER audit BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();"
	if _, err := conn.Exec(ctx, triggerDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	writeDDL := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return desired
	}
	build := func(desired *pgschema.Snapshot) protocol.Result {
		t.Helper()
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	modes := []struct {
		ddl      string
		expected string
	}{
		{"DISABLE", "DISABLE TRIGGER"},
		{"ENABLE REPLICA", "ENABLE REPLICA TRIGGER"},
		{"ENABLE ALWAYS", "ENABLE ALWAYS TRIGGER"},
		{"ENABLE", "ENABLE TRIGGER"},
	}
	for _, mode := range modes {
		desired := writeDDL(triggerDDL + "ALTER TABLE " + quote(schemaName) + ".orders " + mode.ddl + " TRIGGER audit;")
		plan := build(desired)
		if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), mode.expected) {
			t.Fatalf("expected %s plan, got %#v", mode.ddl, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		if residual := build(desired); residual.Status != protocol.Planned || len(residual.Statements) != 0 {
			t.Fatalf("unchanged %s trigger state produced work: %#v", mode.ddl, residual)
		}
	}

	if _, err := conn.Exec(ctx, "DROP TRIGGER audit ON "+quote(schemaName)+".orders"); err != nil {
		t.Fatal(err)
	}
	desired := writeDDL(triggerDDL + "ALTER TABLE " + quote(schemaName) + ".orders DISABLE TRIGGER audit;")
	plan := build(desired)
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(joined, "CREATE TRIGGER") || !strings.Contains(joined, "DISABLE TRIGGER") || strings.Index(joined, "CREATE TRIGGER") > strings.Index(joined, "DISABLE TRIGGER") {
		t.Fatalf("expected create-disabled trigger plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	recreatedDDL := baseDDL +
		"CREATE TRIGGER audit BEFORE UPDATE ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();" +
		"ALTER TABLE " + quote(schemaName) + ".orders DISABLE TRIGGER audit;"
	desired = writeDDL(recreatedDDL)
	plan = build(desired)
	joined = joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(joined, "DROP TRIGGER") || !strings.Contains(joined, "CREATE TRIGGER") || !strings.Contains(joined, "DISABLE TRIGGER") || strings.Index(joined, "CREATE TRIGGER") > strings.Index(joined, "DISABLE TRIGGER") {
		t.Fatalf("expected disabled-state-preserving trigger recreate, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineDependenciesAndReturnTypeChangesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_deps_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	q := quote(schemaName)
	baseDDL := "CREATE SCHEMA " + q + ";"
	currentDDL := baseDDL +
		"CREATE FUNCTION " + q + ".f() RETURNS integer LANGUAGE sql AS $$SELECT 1$$;" +
		"CREATE TABLE " + q + ".t_default (x bigint DEFAULT " + q + ".f());" +
		"CREATE FUNCTION " + q + ".amount(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$SELECT v$$;" +
		"CREATE TABLE " + q + ".t_check (x integer, CONSTRAINT t_check_positive CHECK (" + q + ".amount(x) > 0));" +
		"CREATE FUNCTION " + q + ".dbl(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$SELECT v * 2$$;" +
		"CREATE TABLE " + q + ".t_index (x integer);" +
		"CREATE INDEX t_index_dbl ON " + q + ".t_index (" + q + ".dbl(x));" +
		"CREATE FUNCTION " + q + ".multi() RETURNS integer LANGUAGE sql IMMUTABLE AS $$SELECT 1$$;" +
		"CREATE TABLE " + q + ".t_multi (x bigint DEFAULT " + q + ".multi(), CONSTRAINT t_multi_positive CHECK (" + q + ".multi() > 0));" +
		"CREATE FUNCTION " + q + ".calc() RETURNS integer LANGUAGE sql AS $$SELECT 1$$;" +
		"COMMENT ON FUNCTION " + q + ".calc() IS 'same';" +
		"CREATE FUNCTION " + q + ".drop_default() RETURNS integer LANGUAGE sql AS $$SELECT 1$$;" +
		"CREATE TABLE " + q + ".drop_default_table (x integer DEFAULT " + q + ".drop_default());" +
		"CREATE FUNCTION " + q + ".drop_check(v integer) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$SELECT v > 0$$;" +
		"CREATE TABLE " + q + ".drop_check_table (x integer, CONSTRAINT drop_check_constraint CHECK (" + q + ".drop_check(x)));" +
		"CREATE FUNCTION " + q + ".drop_index(v text) RETURNS text LANGUAGE sql IMMUTABLE AS $$SELECT lower(v)$$;" +
		"CREATE TABLE " + q + ".drop_index_table (x text);" +
		"CREATE INDEX drop_index_expression ON " + q + ".drop_index_table (" + q + ".drop_index(x));" +
		"CREATE FUNCTION " + q + ".leaf() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT 1; END;" +
		"CREATE FUNCTION " + q + ".mid() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT " + q + ".leaf(); END;" +
		"CREATE FUNCTION " + q + ".top() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT " + q + ".mid(); END;" +
		"CREATE FUNCTION " + q + ".leaf_diamond() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT 1; END;" +
		"CREATE FUNCTION " + q + ".mid1_diamond() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT " + q + ".leaf_diamond(); END;" +
		"CREATE FUNCTION " + q + ".mid2_diamond() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT " + q + ".leaf_diamond(); END;" +
		"CREATE FUNCTION " + q + ".top_diamond() RETURNS integer LANGUAGE sql BEGIN ATOMIC SELECT " + q + ".mid1_diamond() + " + q + ".mid2_diamond(); END;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"CREATE FUNCTION " + q + ".f() RETURNS bigint LANGUAGE sql AS $$SELECT 1::bigint$$;" +
		"CREATE TABLE " + q + ".t_default (x bigint DEFAULT " + q + ".f());" +
		"CREATE FUNCTION " + q + ".amount(v integer) RETURNS bigint LANGUAGE sql IMMUTABLE AS $$SELECT v::bigint$$;" +
		"CREATE TABLE " + q + ".t_check (x integer, CONSTRAINT t_check_positive CHECK (" + q + ".amount(x) > 0));" +
		"CREATE FUNCTION " + q + ".dbl(v integer) RETURNS bigint LANGUAGE sql IMMUTABLE AS $$SELECT (v * 2)::bigint$$;" +
		"CREATE TABLE " + q + ".t_index (x integer);" +
		"CREATE INDEX t_index_dbl ON " + q + ".t_index (" + q + ".dbl(x));" +
		"CREATE FUNCTION " + q + ".multi() RETURNS bigint LANGUAGE sql IMMUTABLE AS $$SELECT 1::bigint$$;" +
		"CREATE TABLE " + q + ".t_multi (x bigint DEFAULT " + q + ".multi(), CONSTRAINT t_multi_positive CHECK (" + q + ".multi() > 0));" +
		"CREATE FUNCTION " + q + ".calc() RETURNS bigint LANGUAGE sql AS $$SELECT 1::bigint$$;" +
		"COMMENT ON FUNCTION " + q + ".calc() IS 'same';" +
		"CREATE TABLE " + q + ".drop_default_table (x integer);" +
		"CREATE TABLE " + q + ".drop_check_table (x integer);" +
		"CREATE TABLE " + q + ".drop_index_table (x text);"
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) == 0 {
		t.Fatalf("routine and dependent drops must require confirmation: %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	for beforeSQL, afterSQL := range map[string]string{
		"ALTER COLUMN \"x\" DROP DEFAULT":                              "DROP FUNCTION " + qualified(schemaName, "drop_default"),
		"DROP CONSTRAINT \"drop_check_constraint\"":                    "DROP FUNCTION " + qualified(schemaName, "drop_check"),
		"DROP INDEX " + qualified(schemaName, "drop_index_expression"): "DROP FUNCTION " + qualified(schemaName, "drop_index"),
		"DROP FUNCTION " + qualified(schemaName, "top"):                "DROP FUNCTION " + qualified(schemaName, "mid"),
		"DROP FUNCTION " + qualified(schemaName, "mid"):                "DROP FUNCTION " + qualified(schemaName, "leaf"),
		"DROP FUNCTION " + qualified(schemaName, "top_diamond"):        "DROP FUNCTION " + qualified(schemaName, "mid1_diamond"),
		"DROP FUNCTION " + qualified(schemaName, "mid1_diamond"):       "DROP FUNCTION " + qualified(schemaName, "leaf_diamond"),
		"DROP FUNCTION " + qualified(schemaName, "mid2_diamond"):       "DROP FUNCTION " + qualified(schemaName, "leaf_diamond"),
	} {
		if beforeAt, afterAt := strings.Index(joined, beforeSQL), strings.Index(joined, afterSQL); beforeAt < 0 || afterAt < 0 || beforeAt > afterAt {
			t.Fatalf("routine dependency order %q before %q is missing: %#v", beforeSQL, afterSQL, plan)
		}
	}
	if plan.Status != protocol.Planned || strings.Count(joined, "DROP FUNCTION ") < 15 || strings.Count(joined, "CREATE OR REPLACE FUNCTION ") != 5 || !strings.Contains(joined, "COMMENT ON FUNCTION "+qualified(schemaName, "calc")+"() IS 'same';") || strings.Count(joined, "DROP DEFAULT") < 3 || strings.Count(joined, "SET DEFAULT") < 2 {
		t.Fatalf("routine return-type recreation is incomplete: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineDependencyChangesRejectUnsafeShapesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	tests := []struct {
		name, current, desired, unsupported string
	}{
		{
			name: "dropped dependent",
			current: "CREATE FUNCTION %s.f() RETURNS integer LANGUAGE sql AS $$SELECT 1$$;" +
				"CREATE TABLE %s.t (x bigint DEFAULT %s.f());",
			desired:     "CREATE FUNCTION %s.f() RETURNS bigint LANGUAGE sql AS $$SELECT 1::bigint$$;",
			unsupported: "routine_return_type_changed_dependent:column:",
		},
		{
			name: "changed dependent",
			current: "CREATE FUNCTION %s.amount(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$SELECT v$$;" +
				"CREATE TABLE %s.t (x integer, CONSTRAINT t_chk CHECK (%s.amount(x) > 0));",
			desired: "CREATE FUNCTION %s.amount(v integer) RETURNS bigint LANGUAGE sql IMMUTABLE AS $$SELECT v::bigint$$;" +
				"CREATE TABLE %s.t (x integer, CONSTRAINT t_chk CHECK (%s.amount(x) > 1));",
			unsupported: "routine_return_type_changed_dependent:constraint:",
		},
		{
			name: "routine dependent",
			current: "CREATE FUNCTION %s.leaf() RETURNS integer LANGUAGE sql AS $$SELECT 1$$;" +
				"CREATE FUNCTION %s.caller() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT %s.leaf(); END;",
			desired: "CREATE FUNCTION %s.leaf() RETURNS bigint LANGUAGE sql AS $$SELECT 1::bigint$$;" +
				"CREATE FUNCTION %s.caller() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT %s.leaf(); END;",
			unsupported: "routine_return_type_dependent:routine:",
		},
		{
			name: "circular dropped relations",
			current: "CREATE TABLE %s.readings (a integer);" +
				"CREATE FUNCTION %s.reader() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT count(*) FROM %s.readings; END;" +
				"CREATE TABLE %s.t2 (n bigint DEFAULT %s.reader());",
			desired:     "",
			unsupported: "routine_drop_circular_dependency:routine:",
		},
	}
	for testIndex, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemaName := fmt.Sprintf("onwardpg_routine_reject_%d_%d", testIndex, time.Now().UTC().UnixNano())
			q := quote(schemaName)
			defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
			format := func(value string) string {
				arguments := make([]any, strings.Count(value, "%s"))
				for index := range arguments {
					arguments[index] = q
				}
				return fmt.Sprintf(value, arguments...)
			}
			baseDDL := "CREATE SCHEMA " + q + ";"
			if _, err := conn.Exec(ctx, baseDDL+format(test.current)); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "desired.sql")
			if err := os.WriteFile(path, []byte(baseDDL+format(test.desired)), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status == protocol.NeedsInput {
				answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
				for _, question := range plan.Questions {
					answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: question.Choices[0], QuestionFingerprint: question.ScopeFingerprint})
				}
				plan, err = Build(current, desired, answers, Options{})
				if err != nil {
					t.Fatal(err)
				}
			}
			assertUnsupportedPrefix(t, plan, test.unsupported)
		})
	}
}

func TestRoutineViewDependencyOrderingConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_view_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	fullDDL := baseDDL +
		"CREATE FUNCTION " + quote(schemaName) + ".double_value(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT v * 2 $$;" +
		"CREATE VIEW " + quote(schemaName) + ".doubled AS SELECT " + quote(schemaName) + ".double_value(1) AS value;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache AS SELECT " + quote(schemaName) + ".double_value(1) AS value;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(fullDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql := joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(sql, "CREATE OR REPLACE FUNCTION") || !strings.Contains(sql, "CREATE VIEW") || !strings.Contains(sql, "CREATE MATERIALIZED VIEW") || strings.Index(sql, "CREATE OR REPLACE FUNCTION") > strings.Index(sql, "CREATE VIEW") || strings.Index(sql, "CREATE OR REPLACE FUNCTION") > strings.Index(sql, "CREATE MATERIALIZED VIEW") {
		t.Fatalf("expected routine before dependent views, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 3 {
		t.Fatalf("expected destructive confirmations for views and routine, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop"})
	}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql = joinSQL(plan)
	functionDrop := strings.Index(sql, "DROP FUNCTION")
	if plan.Status != protocol.Planned || functionDrop < 0 || !strings.Contains(sql, "DROP VIEW") || !strings.Contains(sql, "DROP MATERIALIZED VIEW") || functionDrop < strings.Index(sql, "DROP VIEW") || functionDrop < strings.Index(sql, "DROP MATERIALIZED VIEW") {
		t.Fatalf("expected dependent views before routine drop, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestEnumViewDependencyDropOrderingConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_view_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	fullDDL := baseDDL +
		"CREATE TYPE " + quote(schemaName) + ".state AS ENUM ('open', 'closed');" +
		"CREATE VIEW " + quote(schemaName) + ".states AS SELECT 'open'::" + quote(schemaName) + ".state AS state;"
	if _, err := conn.Exec(ctx, fullDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop"})
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sql := joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(sql, "DROP VIEW") || !strings.Contains(sql, "DROP TYPE") || strings.Index(sql, "DROP VIEW") > strings.Index(sql, "DROP TYPE") {
		t.Fatalf("expected view before enum drop, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineRenameRequiresCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".old_total(value integer) RETURNS integer LANGUAGE sql AS 'SELECT value';"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".total(value integer) RETURNS integer LANGUAGE sql AS 'SELECT value';"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_routine" {
		t.Fatalf("expected routine rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)
}

func TestRoutineRenameWithMaterializedViewRequiresCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_matview_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".old_double(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT v * 2 $$;" +
		"CREATE VIEW " + quote(schemaName) + ".doubled AS SELECT " + quote(schemaName) + ".old_double(1) AS value;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache AS SELECT " + quote(schemaName) + ".old_double(1) AS value;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".new_double(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT v * 2 $$;" +
		"CREATE VIEW " + quote(schemaName) + ".doubled AS SELECT " + quote(schemaName) + ".new_double(1) AS value;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache AS SELECT " + quote(schemaName) + ".new_double(1) AS value;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_routine" {
		t.Fatalf("expected only routine-rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: pending.Questions[0].Kind, Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)
}

func TestRoutineReplacementRequiresMaterializedViewRefreshContract(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_refresh_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".double_value(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT v * 2 $$;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache AS SELECT " + quote(schemaName) + ".double_value(1) AS value;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".double_value(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT v * 3 $$;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache AS SELECT " + quote(schemaName) + ".double_value(1) AS value;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "refresh_materialized_view" {
		t.Fatalf("expected routine-dependent refresh contract, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{
		Kind: pending.Questions[0].Kind, Key: pending.Questions[0].Key, Value: "provided",
		Manual: &protocol.ManualWork{Summary: "refresh cache after changing double_value", ExecutionMode: "transactional", Statements: []string{
			"REFRESH MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache;",
		}, VerificationSQL: []string{"SELECT value = 3 FROM " + quote(schemaName) + ".doubled_cache;"}},
	}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE FUNCTION") || !strings.Contains(joinSQL(plan), "REFRESH MATERIALIZED VIEW") || len(plan.Batches) != 2 || plan.Batches[1].Phase != protocol.PhaseContract {
		t.Fatalf("expected routine replacement followed by manual refresh, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	var value int
	if err := conn.QueryRow(ctx, "SELECT value FROM "+quote(schemaName)+".doubled_cache").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 3 {
		t.Fatalf("routine refresh did not update stored data: %d", value)
	}
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineRenameWithDependentTriggerRequiresCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_routine_trigger_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY);" +
		"CREATE FUNCTION " + quote(schemaName) + ".old_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE TRIGGER audit BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".old_audit();"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY);" +
		"CREATE FUNCTION " + quote(schemaName) + ".audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE TRIGGER audit BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_routine" {
		t.Fatalf("expected routine rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_routine", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)
}

func TestTriggerRenameConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_trigger_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE FUNCTION " + quote(schemaName) + ".audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE TRIGGER audit_old BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();" +
		"ALTER TABLE " + quote(schemaName) + ".orders DISABLE TRIGGER audit_old;" +
		"COMMENT ON TRIGGER audit_old ON " + quote(schemaName) + ".orders IS 'old comment';"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE FUNCTION " + quote(schemaName) + ".audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE TRIGGER audit BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();" +
		"ALTER TABLE " + quote(schemaName) + ".orders DISABLE TRIGGER audit;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_trigger" {
		t.Fatalf("expected trigger rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_trigger", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER TRIGGER") || !strings.Contains(joinSQL(plan), "COMMENT ON TRIGGER") || !strings.Contains(joinSQL(plan), "IS NULL") {
		t.Fatalf("expected trigger rename plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestConstraintTriggerLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_constraint_trigger_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE FUNCTION " + quote(schemaName) + ".enforce_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	deferredDDL := baseDDL +
		"CREATE CONSTRAINT TRIGGER orders_guard AFTER INSERT ON " + quote(schemaName) + ".orders DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".enforce_orders();"
	writeDDL := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return desired
	}
	build := func(desired *pgschema.Snapshot, answers protocol.Answers) protocol.Result {
		t.Helper()
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, answers, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	desired := writeDDL(deferredDDL)
	plan := build(desired, protocol.Answers{})
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE CONSTRAINT TRIGGER") {
		t.Fatalf("expected constraint-trigger create plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	if residual := build(desired, protocol.Answers{}); residual.Status != protocol.Planned || len(residual.Statements) != 0 {
		t.Fatalf("unchanged constraint trigger produced work: %#v", residual)
	}

	immediateDDL := baseDDL +
		"CREATE CONSTRAINT TRIGGER orders_guard AFTER INSERT ON " + quote(schemaName) + ".orders NOT DEFERRABLE FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".enforce_orders();"
	desired = writeDDL(immediateDDL)
	plan = build(desired, protocol.Answers{})
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DROP TRIGGER") || !strings.Contains(joinSQL(plan), "CREATE CONSTRAINT TRIGGER") {
		t.Fatalf("expected constraint-trigger deferrability replacement, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	renamedDDL := baseDDL +
		"CREATE CONSTRAINT TRIGGER orders_guard_v2 AFTER INSERT ON " + quote(schemaName) + ".orders NOT DEFERRABLE FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".enforce_orders();"
	desired = writeDDL(renamedDDL)
	pending := build(desired, protocol.Answers{})
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_trigger" {
		t.Fatalf("expected constraint-trigger rename question, got %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_trigger", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}},
	}
	plan = build(desired, answers)
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER TRIGGER") {
		t.Fatalf("expected constraint-trigger rename plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = writeDDL(baseDDL)
	pending = build(desired, protocol.Answers{})
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "drop" {
		t.Fatalf("expected constraint-trigger drop confirmation, got %#v", pending)
	}
	answers = protocol.Answers{
		ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "drop", Key: pending.Questions[0].Key, Value: "drop"}},
	}
	plan = build(desired, answers)
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DROP TRIGGER") {
		t.Fatalf("expected constraint-trigger drop plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestTriggerCommentsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_trigger_comments_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE FUNCTION " + quote(schemaName) + ".table_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE FUNCTION " + quote(schemaName) + ".view_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE TRIGGER orders_audit BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".table_change();" +
		"CREATE VIEW " + quote(schemaName) + ".orders_v AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE TRIGGER orders_v_insert INSTEAD OF INSERT ON " + quote(schemaName) + ".orders_v FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".view_change();"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	writeDDL := func(tableComment, viewComment *string) *pgschema.Snapshot {
		t.Helper()
		ddl := baseDDL
		if tableComment != nil {
			ddl += "COMMENT ON TRIGGER orders_audit ON " + quote(schemaName) + ".orders IS " + literal(*tableComment) + ";"
		}
		if viewComment != nil {
			ddl += "COMMENT ON TRIGGER orders_v_insert ON " + quote(schemaName) + ".orders_v IS " + literal(*viewComment) + ";"
		}
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return desired
	}
	build := func(desired *pgschema.Snapshot) protocol.Result {
		t.Helper()
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	first, viewFirst := "table audit", "view audit"
	desired := writeDDL(&first, &viewFirst)
	plan := build(desired)
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "COMMENT ON TRIGGER") != 2 {
		t.Fatalf("expected table/view trigger comments, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	if residual := build(desired); residual.Status != protocol.Planned || len(residual.Statements) != 0 {
		t.Fatalf("unchanged trigger comments produced work: %#v", residual)
	}

	second, viewSecond := "table audit v2", "view audit v2"
	desired = writeDDL(&second, &viewSecond)
	plan = build(desired)
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "COMMENT ON TRIGGER") != 2 {
		t.Fatalf("expected changed table/view trigger comments, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = writeDDL(nil, nil)
	plan = build(desired)
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "COMMENT ON TRIGGER") != 2 || strings.Count(joinSQL(plan), "IS NULL") != 2 {
		t.Fatalf("expected removed table/view trigger comments, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineOverloadsProceduresAndPartitionTriggersConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_routine_matrix_%d", time.Now().UTC().UnixNano())
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				if question.Kind != "drop" {
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}

	relations := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".team (id integer PRIMARY KEY);
CREATE TABLE "` + schemaName + `".person (team_id integer REFERENCES "` + schemaName + `".team(id));
CREATE TABLE "` + schemaName + `".events (id integer NOT NULL) PARTITION BY RANGE (id);
CREATE TABLE "` + schemaName + `".events_2024 PARTITION OF "` + schemaName + `".events FOR VALUES FROM (1) TO (100);`
	routines := `
CREATE FUNCTION "` + schemaName + `".add(a integer, b integer) RETURNS integer LANGUAGE sql AS $$SELECT a + b$$;
COMMENT ON FUNCTION "` + schemaName + `".add(integer, integer) IS 'adds';
CREATE FUNCTION "` + schemaName + `".f(a integer) RETURNS integer LANGUAGE sql AS $$SELECT a$$;
CREATE FUNCTION "` + schemaName + `".f(a text) RETURNS text LANGUAGE sql AS $$SELECT a$$;
CREATE PROCEDURE "` + schemaName + `".noop() LANGUAGE sql AS $$SELECT 1$$;
CREATE FUNCTION "` + schemaName + `".log_change() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END;$$;`
	trigger := `CREATE TRIGGER events_audit AFTER INSERT ON "` + schemaName + `".events FOR EACH ROW EXECUTE FUNCTION "` + schemaName + `".log_change();`
	full := relations + routines + trigger
	created, desired := applyDesired("routine and parent-trigger create", full)
	createdSQL := joinPlan(created)
	if strings.Count(createdSQL, "CREATE TRIGGER") != 1 || strings.Count(createdSQL, "CREATE OR REPLACE FUNCTION") != 4 || strings.Count(createdSQL, "CREATE OR REPLACE PROCEDURE") != 1 || !strings.Contains(createdSQL, "COMMENT ON FUNCTION") {
		t.Fatalf("routine/trigger create matrix was incomplete or leaked internal/partition triggers:\n%s", createdSQL)
	}
	unchanged, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged routine matrix plan=%#v err=%v", unchanged, err)
	}

	changed := strings.Replace(full, "SELECT a + b", "SELECT a + b + 1", 1)
	replaced, _ := applyDesired("function body changed", changed)
	if strings.Count(joinPlan(replaced), "CREATE OR REPLACE FUNCTION") != 1 {
		t.Fatalf("function body replacement was not isolated:\n%s", joinPlan(replaced))
	}

	withoutTrigger := strings.Replace(changed, trigger, "", 1)
	droppedTrigger, _ := applyDesired("partition-parent trigger dropped", withoutTrigger)
	if strings.Count(joinPlan(droppedTrigger), "DROP TRIGGER") != 1 || !strings.Contains(joinPlan(droppedTrigger), `ON "`+schemaName+`"."events"`) {
		t.Fatalf("partition-parent trigger drop was not singular:\n%s", joinPlan(droppedTrigger))
	}

	withoutAddAndProcedure := strings.Replace(strings.Replace(withoutTrigger,
		`CREATE FUNCTION "`+schemaName+`".add(a integer, b integer) RETURNS integer LANGUAGE sql AS $$SELECT a + b + 1$$;`, "", 1),
		`CREATE PROCEDURE "`+schemaName+`".noop() LANGUAGE sql AS $$SELECT 1$$;`, "", 1)
	withoutAddAndProcedure = strings.Replace(withoutAddAndProcedure, `COMMENT ON FUNCTION "`+schemaName+`".add(integer, integer) IS 'adds';`, "", 1)
	droppedRoutines, _ := applyDesired("function and procedure dropped", withoutAddAndProcedure)
	if !strings.Contains(joinPlan(droppedRoutines), "DROP FUNCTION") || !strings.Contains(joinPlan(droppedRoutines), "DROP PROCEDURE") {
		t.Fatalf("function/procedure drops missing:\n%s", joinPlan(droppedRoutines))
	}
}

func TestViewTriggerLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_view_trigger_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE FUNCTION " + quote(schemaName) + ".change_view() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	viewDDL := "CREATE VIEW " + quote(schemaName) + ".person_v AS SELECT 1 AS id;"
	insertTrigger := "CREATE TRIGGER person_v_ins INSTEAD OF INSERT ON " + quote(schemaName) + ".person_v FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".change_view();"
	path := filepath.Join(t.TempDir(), "schema.sql")
	writeDDL := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return desired
	}
	build := func(desired *pgschema.Snapshot, answers protocol.Answers) protocol.Result {
		t.Helper()
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, answers, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	desired := writeDDL(baseDDL + viewDDL + insertTrigger)
	plan := build(desired, protocol.Answers{})
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(joined, "CREATE VIEW") || !strings.Contains(joined, "CREATE TRIGGER") || strings.Index(joined, "CREATE VIEW") > strings.Index(joined, "CREATE TRIGGER") {
		t.Fatalf("expected view before INSTEAD OF trigger create, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	if residual := build(desired, protocol.Answers{}); residual.Status != protocol.Planned || len(residual.Statements) != 0 {
		t.Fatalf("unchanged view trigger produced work: %#v", residual)
	}

	updateTrigger := "CREATE TRIGGER person_v_ins INSTEAD OF UPDATE ON " + quote(schemaName) + ".person_v FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".change_view();"
	desired = writeDDL(baseDDL + viewDDL + updateTrigger)
	plan = build(desired, protocol.Answers{})
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DROP TRIGGER") || !strings.Contains(joinSQL(plan), "INSTEAD OF UPDATE") {
		t.Fatalf("expected view-trigger definition replacement, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	renamedTrigger := "CREATE TRIGGER person_v_update INSTEAD OF UPDATE ON " + quote(schemaName) + ".person_v FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".change_view();"
	desired = writeDDL(baseDDL + viewDDL + renamedTrigger)
	pending := build(desired, protocol.Answers{})
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_trigger" {
		t.Fatalf("expected view-trigger rename question, got %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_trigger", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}},
	}
	plan = build(desired, answers)
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER TRIGGER") {
		t.Fatalf("expected view-trigger rename plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	changedViewDDL := "CREATE VIEW " + quote(schemaName) + ".person_v AS SELECT 2 AS id;"
	desired = writeDDL(baseDDL + changedViewDDL + renamedTrigger)
	plan = build(desired, protocol.Answers{})
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE VIEW") || strings.Contains(joinSQL(plan), "DROP TRIGGER") {
		t.Fatalf("expected identity-preserving view replacement without trigger loss, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = writeDDL(baseDDL + changedViewDDL)
	pending = build(desired, protocol.Answers{})
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "drop" {
		t.Fatalf("expected view-trigger drop confirmation, got %#v", pending)
	}
	answers = protocol.Answers{
		ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "drop", Key: pending.Questions[0].Key, Value: "drop"}},
	}
	plan = build(desired, answers)
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DROP TRIGGER") {
		t.Fatalf("expected view-trigger drop plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestTransactionalBatchFailureRollsBackOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_tx_rollback_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quote(schemaName)); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE "+quote(schemaName)+".should_rollback (id bigint)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 / 0"); err == nil {
		t.Fatal("expected planned transactional batch failure")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var exists *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass($1)", schemaName+".should_rollback").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != nil {
		t.Fatalf("failed transactional batch left relation behind: %q", *exists)
	}
}

func TestMaterializedViewRenameRequiresCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_matview_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY);" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_ids AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE UNIQUE INDEX order_ids_id_idx ON " + quote(schemaName) + ".order_ids (id);" +
		"COMMENT ON INDEX " + quote(schemaName) + ".order_ids_id_idx IS 'old index';"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY);" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".reporting_order_ids AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE UNIQUE INDEX order_ids_id_idx ON " + quote(schemaName) + ".reporting_order_ids (id);" +
		"COMMENT ON INDEX " + quote(schemaName) + ".order_ids_id_idx IS 'new index';"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_view" {
		t.Fatalf("expected materialized-view rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_view", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)
}

func TestMaterializedViewRenameWithDependentRequiresCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_matview_dependent_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_cache AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".report_cache AS SELECT id FROM " + quote(schemaName) + ".order_cache;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".reporting_cache AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".report_cache AS SELECT id FROM " + quote(schemaName) + ".reporting_cache;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_view" {
		t.Fatalf("expected direct materialized-view rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: pending.Questions[0].Kind, Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertRenameCompatibilityBridge(t, plan)
}

func TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_matview_plan_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY);"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	initialDDL := baseDDL +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_ids AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE UNIQUE INDEX order_ids_id_idx ON " + quote(schemaName) + ".order_ids (id);"
	if err := os.WriteFile(path, []byte(initialDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE MATERIALIZED VIEW") {
		t.Fatalf("expected materialized-view create plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	unchanged, err := Build(func() *pgschema.Snapshot {
		snapshot, loadErr := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		return snapshot
	}(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged materialized view/index generated work: plan=%#v err=%v", unchanged, err)
	}

	commentedDDL := initialDDL + "COMMENT ON MATERIALIZED VIEW " + quote(schemaName) + ".order_ids IS 'reporting cache';"
	if err := os.WriteFile(path, []byte(commentedDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 1 || !strings.Contains(joinSQL(plan), "COMMENT ON MATERIALIZED VIEW") {
		t.Fatalf("expected non-destructive materialized-view comment plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	changedDDL := baseDDL +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_ids AS SELECT id FROM " + quote(schemaName) + ".orders WHERE id > 0;" +
		"CREATE UNIQUE INDEX order_ids_id_idx ON " + quote(schemaName) + ".order_ids (id);" +
		"COMMENT ON MATERIALIZED VIEW " + quote(schemaName) + ".order_ids IS 'reporting cache';"
	if err := os.WriteFile(path, []byte(changedDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rebuild_materialized_view" {
		t.Fatalf("expected materialized-view rebuild question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rebuild_materialized_view", Key: pending.Questions[0].Key, Value: "rebuild"}}}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DROP MATERIALIZED VIEW") || !strings.Contains(joinSQL(plan), "CREATE MATERIALIZED VIEW") || !strings.Contains(joinSQL(plan), "CREATE UNIQUE INDEX") {
		t.Fatalf("expected materialized-view rebuild plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if err := os.WriteFile(path, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "drop" {
		t.Fatalf("expected materialized-view drop confirmation, got %#v", pending)
	}
	answers = protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: pending.Questions[0].Key, Value: "drop"}}}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "DROP MATERIALIZED VIEW") {
		t.Fatalf("expected materialized-view drop plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestMaterializedViewsOverExternalViewsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_matview_external_%d", time.Now().UTC().UnixNano())
	defer func() {
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`)
		_, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS pg_stat_statements")
	}()
	if _, err := conn.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatal(err)
	}
	ddl := `CREATE EXTENSION pg_stat_statements;
CREATE SCHEMA "` + schemaName + `";
CREATE MATERIALIZED VIEW "` + schemaName + `".active AS SELECT pid FROM pg_catalog.pg_stat_activity WITH NO DATA;
CREATE MATERIALIZED VIEW "` + schemaName + `".stats AS SELECT userid FROM public.pg_stat_statements WITH NO DATA;`
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned || strings.Count(joinPlan(plan), "CREATE MATERIALIZED VIEW") != 2 {
		t.Fatalf("external-view materialized plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestMaterializedViewIndexLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_matview_index_%d", time.Now().UTC().UnixNano())
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	base := "CREATE SCHEMA " + q + "; CREATE MATERIALIZED VIEW " + q + ".report AS SELECT 1 AS x, 2 AS y WITH NO DATA;"
	if _, err := conn.Exec(ctx, base); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				value := ""
				switch question.Kind {
				case "drop":
					value = "drop"
				case "rename_index":
					value = question.Choices[0]
				default:
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{ConcurrentIndexes: true})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}

	indexes := base + " CREATE INDEX report_idx ON " + q + ".report (x); CREATE UNIQUE INDEX report_y_uidx ON " + q + ".report (y);"
	created, _ := applyDesired("matview indexes created concurrently", indexes)
	if strings.Count(joinPlan(created), "CREATE INDEX CONCURRENTLY") != 1 || strings.Count(joinPlan(created), "CREATE UNIQUE INDEX CONCURRENTLY") != 1 {
		t.Fatalf("materialized-view concurrent index creates missing:\n%s", joinPlan(created))
	}
	commented := indexes + " COMMENT ON INDEX " + q + ".report_idx IS 'by x';"
	commentAdded, desired := applyDesired("matview index comment added", commented)
	if !strings.Contains(joinPlan(commentAdded), "COMMENT ON INDEX") || !strings.Contains(joinPlan(commentAdded), "IS 'by x'") {
		t.Fatalf("materialized-view index comment add missing:\n%s", joinPlan(commentAdded))
	}
	unchanged, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged materialized-view indexes plan=%#v err=%v", unchanged, err)
	}
	commentRemoved, _ := applyDesired("matview index comment removed", indexes)
	if !strings.Contains(joinPlan(commentRemoved), "COMMENT ON INDEX") || !strings.Contains(joinPlan(commentRemoved), "IS NULL") {
		t.Fatalf("materialized-view index comment removal missing:\n%s", joinPlan(commentRemoved))
	}

	renamedDDL := strings.Replace(indexes, "report_idx", "report_new", 1)
	renamed, _ := applyDesired("matview index renamed", renamedDDL)
	if !strings.Contains(joinPlan(renamed), "ALTER INDEX") || !strings.Contains(joinPlan(renamed), `RENAME TO "report_new"`) {
		t.Fatalf("materialized-view index rename missing:\n%s", joinPlan(renamed))
	}
	changedDDL := strings.Replace(renamedDDL, ".report (x)", ".report (x, y)", 1)
	changed, _ := applyDesired("matview index definition changed", changedDDL)
	changedSQL := joinPlan(changed)
	if !strings.Contains(changedSQL, "CREATE INDEX CONCURRENTLY") || !strings.Contains(changedSQL, "DROP INDEX CONCURRENTLY") {
		t.Fatalf("materialized-view continuous index replacement missing:\n%s", changedSQL)
	}
	droppedDDL := strings.Replace(changedDDL, " CREATE INDEX report_new ON "+q+".report (x, y);", "", 1)
	dropped, _ := applyDesired("matview index dropped concurrently", droppedDDL)
	if !strings.Contains(joinPlan(dropped), `DROP INDEX CONCURRENTLY "`+schemaName+`"."report_new"`) {
		t.Fatalf("materialized-view concurrent index drop missing:\n%s", joinPlan(dropped))
	}
	_ = desired
}

func TestMaterializedViewDependencyChainsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_matview_deps_%d", time.Now().UTC().UnixNano())
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				value := ""
				switch question.Kind {
				case "drop":
					value = "drop"
				case "rebuild_materialized_view":
					value = "rebuild"
				default:
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}

	prefix := "CREATE SCHEMA " + q + ";"
	chain := prefix +
		" CREATE VIEW " + q + ".v AS SELECT 1 AS x;" +
		" CREATE MATERIALIZED VIEW " + q + ".z_base AS SELECT x FROM " + q + ".v WITH NO DATA;" +
		" CREATE MATERIALIZED VIEW " + q + ".m_mid AS SELECT x FROM " + q + ".z_base WITH NO DATA;" +
		" CREATE MATERIALIZED VIEW " + q + ".a_top AS SELECT x FROM " + q + ".m_mid WITH NO DATA;" +
		" CREATE VIEW " + q + ".final_view AS SELECT x FROM " + q + ".a_top;"
	created, desired := applyDesired("materialized dependency chain created", chain)
	createdSQL := joinPlan(created)
	positions := []int{
		strings.Index(createdSQL, `CREATE VIEW "`+schemaName+`"."v"`),
		strings.Index(createdSQL, `CREATE MATERIALIZED VIEW "`+schemaName+`"."z_base"`),
		strings.Index(createdSQL, `CREATE MATERIALIZED VIEW "`+schemaName+`"."m_mid"`),
		strings.Index(createdSQL, `CREATE MATERIALIZED VIEW "`+schemaName+`"."a_top"`),
		strings.Index(createdSQL, `CREATE VIEW "`+schemaName+`"."final_view"`),
	}
	for i, position := range positions {
		if position < 0 || i > 0 && positions[i-1] > position {
			t.Fatalf("materialized dependency creation order was unsafe:\n%s", createdSQL)
		}
	}
	unchanged, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged materialized dependency chain plan=%#v err=%v", unchanged, err)
	}
	dropped, _ := applyDesired("materialized dependency chain dropped", prefix)
	dropSQL := joinPlan(dropped)
	dropPositions := []int{
		strings.Index(dropSQL, `DROP VIEW "`+schemaName+`"."final_view"`),
		strings.Index(dropSQL, `DROP MATERIALIZED VIEW "`+schemaName+`"."a_top"`),
		strings.Index(dropSQL, `DROP MATERIALIZED VIEW "`+schemaName+`"."m_mid"`),
		strings.Index(dropSQL, `DROP MATERIALIZED VIEW "`+schemaName+`"."z_base"`),
		strings.Index(dropSQL, `DROP VIEW "`+schemaName+`"."v"`),
	}
	for i, position := range dropPositions {
		if position < 0 || i > 0 && dropPositions[i-1] > position {
			t.Fatalf("materialized dependency drop order was unsafe:\n%s", dropSQL)
		}
	}

	baseChain := prefix + " CREATE MATERIALIZED VIEW " + q + ".base AS SELECT 1 AS x WITH NO DATA; CREATE MATERIALIZED VIEW " + q + ".derived AS SELECT x FROM " + q + ".base WITH NO DATA;"
	applyDesired("materialized rebuild chain created", baseChain)
	changedChain := strings.Replace(baseChain, "SELECT 1 AS x", "SELECT 2 AS x", 1)
	rebuilt, _ := applyDesired("materialized rebuild cascaded", changedChain)
	rebuiltSQL := joinPlan(rebuilt)
	if strings.Index(rebuiltSQL, `DROP MATERIALIZED VIEW "`+schemaName+`"."derived"`) > strings.Index(rebuiltSQL, `DROP MATERIALIZED VIEW "`+schemaName+`"."base"`) || strings.LastIndex(rebuiltSQL, `CREATE MATERIALIZED VIEW "`+schemaName+`"."base"`) > strings.LastIndex(rebuiltSQL, `CREATE MATERIALIZED VIEW "`+schemaName+`"."derived"`) {
		t.Fatalf("materialized rebuild cascade order was unsafe:\n%s", rebuiltSQL)
	}
}

func TestExtensionCreateConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	if _, err := conn.Exec(ctx, "DROP EXTENSION IF EXISTS pg_trgm"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS pg_trgm") }()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte("CREATE EXTENSION pg_trgm;"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected extension-create plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("extension create did not converge: got %s want %s", actualFingerprint, desiredFingerprint)
	}
}

func TestExtensionDropConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS pg_trgm") }()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected extension-drop confirmation: %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: pending.Questions[0].Key, Value: "drop"}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected extension-drop plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("extension drop did not converge: got %s want %s", actualFingerprint, desiredFingerprint)
	}
}

func TestExtensionAtomicMembersAndTypeDependenciesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_ext_dep_" + time.Now().UTC().Format("20060102150405")
	defer func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE")
		_, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS pg_trgm CASCADE")
		_, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS citext CASCADE")
	}()
	if _, err := conn.Exec(ctx, "DROP EXTENSION IF EXISTS pg_trgm CASCADE; DROP EXTENSION IF EXISTS citext CASCADE"); err != nil {
		t.Fatal(err)
	}
	memberDDL := `CREATE EXTENSION pg_trgm;
CREATE TABLE spatial_ref_sys (srid integer PRIMARY KEY, name text);
CREATE INDEX spatial_ref_sys_name_idx ON spatial_ref_sys (name);
CREATE FUNCTION spatial_audit() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
CREATE TRIGGER spatial_audit_trg BEFORE INSERT ON spatial_ref_sys FOR EACH ROW EXECUTE FUNCTION spatial_audit();
CREATE SEQUENCE ext_seq;
CREATE SCHEMA ext_schema;
CREATE FUNCTION ext_schema.helper() RETURNS integer LANGUAGE sql AS 'SELECT 1';
CREATE TYPE ext_schema.mood AS ENUM ('happy', 'sad');
ALTER EXTENSION pg_trgm ADD TABLE spatial_ref_sys;
ALTER EXTENSION pg_trgm ADD FUNCTION spatial_audit();
ALTER EXTENSION pg_trgm ADD SEQUENCE ext_seq;
ALTER EXTENSION pg_trgm ADD SCHEMA ext_schema;`
	path := filepath.Join(t.TempDir(), "extension-members.sql")
	if err := os.WriteFile(path, []byte(memberDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned || strings.Count(joinPlan(plan), "CREATE EXTENSION") != 1 || strings.Contains(joinPlan(plan), "spatial_ref_sys") || strings.Contains(joinPlan(plan), "ext_schema") || strings.Contains(joinPlan(plan), "ext_seq") {
		t.Fatalf("extension members escaped the atomic boundary: plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	if _, err := conn.Exec(ctx, "DROP EXTENSION pg_trgm"); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, "CREATE EXTENSION citext; CREATE SCHEMA "+quote(schemaName)+"; CREATE TABLE "+quote(schemaName)+".users (email citext);"); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "drop-extension-dependency.sql")
	if err := os.WriteFile(path, []byte("CREATE SCHEMA "+quote(schemaName)+";"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput {
		t.Fatalf("dependent extension/table drops must ask: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		if question.Kind != "drop" {
			t.Fatalf("unexpected extension dependency question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("dependent extension/table drop plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	tableDropAt := strings.Index(joined, "DROP TABLE")
	extensionDropAt := strings.Index(joined, "DROP EXTENSION")
	if tableDropAt < 0 || extensionDropAt < 0 || tableDropAt > extensionDropAt {
		t.Fatalf("extension was not dropped after its consuming table:\n%s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestExtensionSchemaMoveConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_ext_move_" + time.Now().UTC().Format("20060102150405")
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "ALTER EXTENSION pg_trgm SET SCHEMA public")
		_, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS pg_trgm")
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE")
	}()
	path := filepath.Join(t.TempDir(), "schema.sql")
	ddl := "CREATE SCHEMA " + quote(schemaName) + "; CREATE EXTENSION pg_trgm WITH SCHEMA " + quote(schemaName) + ";"
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "ALTER EXTENSION \"pg_trgm\" SET SCHEMA "+quote(schemaName)+";") {
		t.Fatalf("expected extension schema-move plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestExtensionVersionIgnoreBehaviorOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	if _, err := conn.Exec(ctx, "DROP EXTENSION IF EXISTS hstore"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE EXTENSION hstore VERSION '1.4'"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS hstore") }()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte("CREATE EXTENSION hstore VERSION '1.8';"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	ignored, err := Build(current, desired, protocol.Answers{}, Options{IgnoreExtensionVersions: []string{"hstore"}})
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Status != protocol.Planned || len(ignored.Statements) != 0 {
		t.Fatalf("matching extension version ignore was not honored: %#v", ignored)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{IgnoreExtensionVersions: []string{"some_other_extension"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `ALTER EXTENSION "hstore" UPDATE TO '1.8';`) {
		t.Fatalf("nonmatching extension version ignore suppressed the update: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestSequenceParameterUpdateConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_sequence_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		path := filepath.Join(t.TempDir(), "schema.sql")
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		desired := loadDesired(ddl)
		plan, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{})
		if err != nil || plan.Status != protocol.Planned {
			t.Fatalf("sequence lifecycle plan=%#v err=%v", plan, err)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return desired
	}
	createdDDL := baseDDL + " CREATE SEQUENCE " + quote(schemaName) + ".orders_seq AS integer START WITH 1 INCREMENT BY 1 MINVALUE 1 MAXVALUE 100 CACHE 1 NO CYCLE; COMMENT ON SEQUENCE " + quote(schemaName) + ".orders_seq IS 'order numbers';"
	created := applyDesired(createdDDL)
	unchanged, err := Build(loadCurrent(), created, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged sequence generated work: plan=%#v err=%v", unchanged, err)
	}
	mutatedDDL := baseDDL + " CREATE SEQUENCE " + quote(schemaName) + ".orders_seq AS bigint START WITH 10 INCREMENT BY 5 MINVALUE -10 MAXVALUE 1000 CACHE 4 CYCLE; COMMENT ON SEQUENCE " + quote(schemaName) + ".orders_seq IS 'order numbers';"
	applyDesired(mutatedDDL)
	noCycleDDL := baseDDL + " CREATE SEQUENCE " + quote(schemaName) + ".orders_seq AS bigint START WITH 10 INCREMENT BY 5 MINVALUE -10 MAXVALUE 1000 CACHE 4 NO CYCLE; COMMENT ON SEQUENCE " + quote(schemaName) + ".orders_seq IS 'order numbers';"
	applyDesired(noCycleDDL)
	desired := loadDesired(baseDDL)
	current := loadCurrent()
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "drop" {
		t.Fatalf("sequence drop must ask: plan=%#v err=%v", pending, err)
	}
	question := pending.Questions[0]
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "DROP SEQUENCE") {
		t.Fatalf("sequence drop plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestSequencePersistenceConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_sequence_persistence_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	definition := " SEQUENCE " + quote(schemaName) + ".counter AS integer INCREMENT BY 1 MINVALUE 1 MAXVALUE 100 START WITH 1 CACHE 1;"
	steps := []struct {
		name string
		ddl  string
		want string
	}{
		{name: "create unlogged", ddl: baseDDL + "CREATE UNLOGGED" + definition, want: "CREATE UNLOGGED SEQUENCE"},
		{name: "flip to logged", ddl: baseDDL + "CREATE" + definition, want: "SET LOGGED;"},
		{name: "flip to unlogged", ddl: baseDDL + "CREATE UNLOGGED" + definition, want: "SET UNLOGGED;"},
		{name: "unchanged unlogged", ddl: baseDDL + "CREATE UNLOGGED" + definition},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("expected sequence persistence plan: %#v", plan)
			}
			if step.want == "" {
				if len(plan.Statements) != 0 {
					t.Fatalf("unchanged sequence persistence generated work: %#v", plan.Statements)
				}
			} else if !strings.Contains(joinSQL(plan), step.want) {
				t.Fatalf("sequence persistence plan missing %q: %#v", step.want, plan.Statements)
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}
}

func TestReplicaIdentityConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_replica_identity_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	tableDDL := func(mode string, includeNewTable bool) string {
		ddl := baseDDL +
			"CREATE TABLE " + quote(schemaName) + ".events (a integer NOT NULL, b integer NOT NULL);" +
			"CREATE UNIQUE INDEX events_a_idx ON " + quote(schemaName) + ".events (a);" +
			"CREATE UNIQUE INDEX events_b_idx ON " + quote(schemaName) + ".events (b);"
		if mode != "" {
			ddl += "ALTER TABLE " + quote(schemaName) + ".events REPLICA IDENTITY " + mode + ";"
		}
		if includeNewTable {
			ddl += "CREATE TABLE " + quote(schemaName) + ".new_events (id integer NOT NULL);" +
				"CREATE UNIQUE INDEX new_events_uidx ON " + quote(schemaName) + ".new_events (id);" +
				"ALTER TABLE " + quote(schemaName) + ".new_events REPLICA IDENTITY USING INDEX new_events_uidx;"
		}
		return ddl
	}
	steps := []struct {
		name             string
		mode             string
		includeNewTable  bool
		want             string
		unchanged        bool
		checkCreateOrder bool
	}{
		{name: "create table full", mode: "FULL", want: "REPLICA IDENTITY FULL;"},
		{name: "unchanged full", mode: "FULL", unchanged: true},
		{name: "full to default", want: "REPLICA IDENTITY DEFAULT;"},
		{name: "default to nothing", mode: "NOTHING", want: "REPLICA IDENTITY NOTHING;"},
		{name: "nothing to default", want: "REPLICA IDENTITY DEFAULT;"},
		{name: "using index set", mode: "USING INDEX events_a_idx", want: `REPLICA IDENTITY USING INDEX "events_a_idx";`},
		{name: "using index changed", mode: "USING INDEX events_b_idx", want: `REPLICA IDENTITY USING INDEX "events_b_idx";`},
		{name: "using index dropped back", want: "REPLICA IDENTITY DEFAULT;"},
		{name: "create table using index", includeNewTable: true, want: `REPLICA IDENTITY USING INDEX "new_events_uidx";`, checkCreateOrder: true},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(tableDDL(step.mode, step.includeNewTable)), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("expected replica identity plan: %#v", plan)
			}
			joined := joinSQL(plan)
			if step.unchanged {
				if len(plan.Statements) != 0 {
					t.Fatalf("unchanged replica identity generated work: %#v", plan.Statements)
				}
			} else if !strings.Contains(joined, step.want) {
				t.Fatalf("replica identity plan missing %q: %#v", step.want, plan.Statements)
			}
			if step.checkCreateOrder {
				indexAt := strings.Index(joined, `CREATE UNIQUE INDEX "new_events_uidx"`)
				identityAt := strings.Index(joined, step.want)
				if indexAt < 0 || identityAt < 0 || indexAt > identityAt {
					t.Fatalf("replica identity was not ordered after its index:\n%s", joined)
				}
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}
}

func TestTablePersistenceChangeConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_unlogged_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quote(schemaName)+"; CREATE TABLE "+quote(schemaName)+".orders (id bigint)"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + "; CREATE UNLOGGED TABLE " + quote(schemaName) + ".orders (id bigint);"
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected persistence-change plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("table persistence change did not converge: got %s want %s", actualFingerprint, desiredFingerprint)
	}
}

func TestTablePersistenceRestoreLoggedConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_logged_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quote(schemaName)+"; CREATE UNLOGGED TABLE "+quote(schemaName)+".orders (id bigint)"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + "; CREATE TABLE " + quote(schemaName) + ".orders (id bigint);"
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected restore-logged plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("table persistence restore did not converge: got %s want %s", actualFingerprint, desiredFingerprint)
	}
}

func TestTableLifecycleAndPersistenceConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_table_lifecycle_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) protocol.Result {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				if question.Kind != "drop" {
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan
	}

	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".person (name text);
CREATE TABLE "` + schemaName + `"."we""ird" ("c""ol" text);
CREATE TABLE "` + schemaName + `".marker ();
CREATE UNLOGGED TABLE "` + schemaName + `".cache (id integer);
CREATE TABLE "` + schemaName + `".events (id integer) PARTITION BY RANGE (id);
CREATE UNLOGGED TABLE "` + schemaName + `".events_lo PARTITION OF "` + schemaName + `".events FOR VALUES FROM (0) TO (10);`
	created := applyDesired("table creation", base)
	createdSQL := joinPlan(created)
	for _, fragment := range []string{`CREATE TABLE "` + schemaName + `"."we""ird" ("c""ol" text)`, `CREATE TABLE "` + schemaName + `"."marker" ()`, `CREATE UNLOGGED TABLE "` + schemaName + `"."cache"`, `CREATE UNLOGGED TABLE "` + schemaName + `"."events_lo" PARTITION OF`} {
		if !strings.Contains(createdSQL, fragment) {
			t.Fatalf("table creation plan missing %q:\n%s", fragment, createdSQL)
		}
	}
	unchanged, err := Build(loadCurrent(), loadDesired(base), protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged table/persistence plan=%#v err=%v", unchanged, err)
	}

	logged := strings.Replace(strings.Replace(base, "CREATE UNLOGGED TABLE \""+schemaName+"\".cache", "CREATE TABLE \""+schemaName+"\".cache", 1), "CREATE UNLOGGED TABLE \""+schemaName+"\".events_lo", "CREATE TABLE \""+schemaName+"\".events_lo", 1)
	toLogged := applyDesired("persistence to logged", logged)
	if !strings.Contains(joinPlan(toLogged), `ALTER TABLE "`+schemaName+`"."cache" SET LOGGED`) || !strings.Contains(joinPlan(toLogged), `ALTER TABLE "`+schemaName+`"."events_lo" SET LOGGED`) {
		t.Fatalf("logged persistence transitions missing:\n%s", joinPlan(toLogged))
	}
	toUnlogged := applyDesired("persistence to unlogged", base)
	if !strings.Contains(joinPlan(toUnlogged), `ALTER TABLE "`+schemaName+`"."cache" SET UNLOGGED`) {
		t.Fatalf("unlogged persistence transition missing:\n%s", joinPlan(toUnlogged))
	}

	retained := `CREATE SCHEMA "` + schemaName + `";
CREATE UNLOGGED TABLE "` + schemaName + `".cache (id integer);
CREATE TABLE "` + schemaName + `".events (id integer) PARTITION BY RANGE (id);
CREATE UNLOGGED TABLE "` + schemaName + `".events_lo PARTITION OF "` + schemaName + `".events FOR VALUES FROM (0) TO (10);`
	dropped := applyDesired("ordinary tables dropped", retained)
	if strings.Count(joinPlan(dropped), "DROP TABLE") < 3 {
		t.Fatalf("ordinary table drops missing:\n%s", joinPlan(dropped))
	}
}

func TestMutationPlanConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_mutate_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TYPE "` + schemaName + `".state AS ENUM ('open', 'closed');
CREATE TABLE "` + schemaName + `".orders (id bigint, age text, state "` + schemaName + `".state NOT NULL DEFAULT 'open');`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TYPE "` + schemaName + `".state AS ENUM ('open', 'pending', 'closed');
CREATE TABLE "` + schemaName + `".orders (
  id bigint NOT NULL,
  age text,
  state "` + schemaName + `".state NOT NULL DEFAULT 'open',
  note text DEFAULT 'new'
);
COMMENT ON TABLE "` + schemaName + `".orders IS 'migrated';
CREATE INDEX orders_age_idx ON "` + schemaName + `".orders (age);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected staged NOT NULL question: %#v", pending)
	}
	idID := (pgschema.Column{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "id"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			{Kind: "set_not_null", Key: idID, Value: "staged"},
		},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected mutation plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	dropNotNullDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TYPE "` + schemaName + `".state AS ENUM ('open', 'pending', 'closed');
CREATE TABLE "` + schemaName + `".orders (
  id bigint,
  age text,
  state "` + schemaName + `".state NOT NULL DEFAULT 'open',
  note text DEFAULT 'new'
);
COMMENT ON TABLE "` + schemaName + `".orders IS 'migrated';
CREATE INDEX orders_age_idx ON "` + schemaName + `".orders (age);`
	dropPath := filepath.Join(t.TempDir(), "drop-not-null.sql")
	if err := os.WriteFile(dropPath, []byte(dropNotNullDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+dropPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `ALTER TABLE "`+schemaName+`"."orders" ALTER COLUMN "id" DROP NOT NULL;`) {
		t.Fatalf("expected DROP NOT NULL plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestTableColumnLifecycleMatrixConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_column_matrix_%d", time.Now().UTC().UnixNano())
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		answers := protocol.Answers{}
		var plan protocol.Result
		for attempt := 0; attempt < 4; attempt++ {
			var err error
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s plan attempt %d: %v", label, attempt, err)
			}
			if plan.Status != protocol.NeedsInput {
				break
			}
			if answers.ProtocolVersion == "" {
				answers = protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			}
			for _, question := range plan.Questions {
				value := ""
				switch question.Kind {
				case "drop":
					value = "drop"
				case "rename_column":
					value = "create"
				case "set_not_null":
					value = "direct"
				default:
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}

	initial := "CREATE SCHEMA " + q + "; CREATE TABLE " + q + ".person (zebra text, apple text, mango text, age integer NOT NULL DEFAULT 0, id integer PRIMARY KEY);"
	created, desired := applyDesired("column creation and physical order", initial)
	createdSQL := joinPlan(created)
	if strings.Index(createdSQL, `"zebra" text`) > strings.Index(createdSQL, `"apple" text`) || strings.Index(createdSQL, `"apple" text`) > strings.Index(createdSQL, `"mango" text`) || !strings.Contains(createdSQL, `"age" integer DEFAULT 0 NOT NULL`) {
		t.Fatalf("column physical order or inline attributes were lost:\n%s", createdSQL)
	}
	unchanged, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged column attributes generated work: plan=%#v err=%v", unchanged, err)
	}

	changed := "CREATE SCHEMA " + q + "; CREATE TABLE " + q + ".person (zebra text, apple text, email text, age integer NOT NULL DEFAULT 1, id integer, score integer NOT NULL DEFAULT 0);"
	mutated, _ := applyDesired("columns added dropped and changed", changed)
	mutatedSQL := joinPlan(mutated)
	if strings.Index(mutatedSQL, "DROP CONSTRAINT") > strings.Index(mutatedSQL, `ALTER COLUMN "id" DROP NOT NULL`) || !strings.Contains(mutatedSQL, `DROP COLUMN "mango"`) || !strings.Contains(mutatedSQL, `ADD COLUMN "email" text`) || !strings.Contains(mutatedSQL, `ADD COLUMN "score" integer DEFAULT 0 NOT NULL`) || !strings.Contains(mutatedSQL, `ALTER COLUMN "age" SET DEFAULT 1`) {
		t.Fatalf("column mutation ordering/attributes were incomplete:\n%s", mutatedSQL)
	}

	withoutDefaults := "CREATE SCHEMA " + q + "; CREATE TABLE " + q + ".person (zebra text, apple text, email text DEFAULT 'known', age integer, id integer, score integer NOT NULL);"
	defaults, _ := applyDesired("column defaults and nullability changed", withoutDefaults)
	defaultsSQL := joinPlan(defaults)
	for _, fragment := range []string{`ALTER COLUMN "email" SET DEFAULT 'known'`, `ALTER COLUMN "age" DROP DEFAULT`, `ALTER COLUMN "age" DROP NOT NULL`, `ALTER COLUMN "score" DROP DEFAULT`} {
		if !strings.Contains(defaultsSQL, fragment) {
			t.Fatalf("column default/nullability transition omitted %q:\n%s", fragment, defaultsSQL)
		}
	}
	final := strings.Replace(withoutDefaults, "email text DEFAULT 'known'", "email text", 1)
	droppedDefault, desired := applyDesired("column default removed", final)
	if !strings.Contains(joinPlan(droppedDefault), `ALTER COLUMN "email" DROP DEFAULT`) {
		t.Fatalf("column default removal missing:\n%s", joinPlan(droppedDefault))
	}
	unchanged, err = Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("final unchanged columns generated work: plan=%#v err=%v", unchanged, err)
	}
}

func TestTypeMutationProducesEditableExpandContractHandoffOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_type_bridge_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".orders (age text);`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(`CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".orders (age integer);`), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("expected type-change decision: %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "type_change", Key: pending.Questions[0].Key, Value: "manual_sql"}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsSQLEdits || !strings.Contains(joinSQL(plan), "ONWARDPG TODO") || strings.Contains(joinSQL(plan), `ALTER COLUMN "age" TYPE`) {
		t.Fatalf("type change must be handed to editable SQL: %#v", plan)
	}
}

func TestRequiredColumnStagingConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_required_column_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	baseDDL := `CREATE SCHEMA "` + schemaName + `"; CREATE TABLE "` + schemaName + `".customers (id bigint);`
	if _, err := conn.Exec(ctx, baseDDL+` INSERT INTO "`+schemaName+`".customers (id) VALUES (1);`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "desired.sql")
	desiredDDL := `CREATE SCHEMA "` + schemaName + `"; CREATE TABLE "` + schemaName + `".customers (id bigint, email text NOT NULL);`
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.NeedsSQLEdits {
		t.Fatalf("required column must wait for agent-owned data work: %#v", plan)
	}
	applyPlanPhase(t, ctx, conn, plan, "expand")
	if _, err := conn.Exec(ctx, `INSERT INTO "`+schemaName+`".customers (id) VALUES (2)`); err != nil {
		t.Fatalf("old writer broke during expand: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE "`+schemaName+`".customers SET email = 'customer-' || id::text || '@example.test' WHERE email IS NULL`); err != nil {
		t.Fatal(err)
	}
	applyPlanPhase(t, ctx, conn, plan, "contract")
	assertGraphConverges(t, ctx, url, desired)
}

func TestIdentityGenerationStartAndIncrementConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_identity_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (
  id bigint GENERATED BY DEFAULT AS IDENTITY (START WITH 1 INCREMENT BY 1)
);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (
  id bigint GENERATED ALWAYS AS IDENTITY (START WITH 10 INCREMENT BY 2)
);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected identity mutation plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("identity mutation left residual graph diff: got %s want %s", got, want)
	}
}

func TestSerialTypeTransitionsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_serial_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	integerDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
COMMENT ON COLUMN "` + schemaName + `".orders.id IS 'external identifier';`
	if _, err := conn.Exec(ctx, integerDDL); err != nil {
		t.Fatal(err)
	}
	serialDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigserial);`
	serialPath := filepath.Join(t.TempDir(), "serial.sql")
	integerPath := filepath.Join(t.TempDir(), "integer.sql")
	if err := os.WriteFile(serialPath, []byte(serialDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(integerPath, []byte(integerDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	applyTo := func(path string) {
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				if question.Kind != "set_not_null" {
					t.Fatalf("unexpected serial transition question: %#v", question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "direct"})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected serial transition plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("serial transition left residual graph diff: got %s want %s", got, want)
		}
	}
	applyTo(serialPath)
	applyTo(integerPath)
}

func TestGeneratedColumnCreateAndDropExpressionConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_generated_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	baseDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (name text, normalized text);`
	if _, err := conn.Exec(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		t.Fatal(err)
	}
	generatedDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (
  name text,
  normalized text GENERATED ALWAYS AS (lower(name)) STORED
);`
	paths := make([]string, 0, 2)
	for i, ddl := range []string{generatedDDL, baseDDL} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("generated-%d.sql", i))
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for i, path := range paths {
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != protocol.Planned || (i == 1 && !strings.Contains(joinPlan(plan), "DROP EXPRESSION")) {
			t.Fatalf("expected generated-column transition plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("generated-column transition left residual graph diff: got %s want %s", got, want)
		}
	}
}

func TestSemanticDefaultEquivalenceAvoidsMigrationOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_default_semantic_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer DEFAULT (1 + 1));`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer DEFAULT 2);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{DefaultEquivalent: func(left, right string) (bool, error) {
		var same bool
		err := conn.QueryRow(ctx, "SELECT ("+left+") IS NOT DISTINCT FROM ("+right+")").Scan(&same)
		return same, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 0 {
		t.Fatalf("equivalent defaults should not migrate: %#v", plan)
	}
}

func TestColumnDefaultTransitionsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_defaults_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	withoutDefault := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer);`
	if _, err := conn.Exec(ctx, withoutDefault); err != nil {
		t.Fatal(err)
	}
	withOne := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer DEFAULT 1);`
	withTwo := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer DEFAULT 2);`
	paths := make([]string, 0, 3)
	for i, ddl := range []string{withOne, withTwo, withoutDefault} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("desired-%d.sql", i))
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected default transition plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("default transition left residual graph diff: got %s want %s", got, want)
		}
	}
}

func TestCommentTransitionsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_comments_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		t.Fatal(err)
	}
	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
CREATE INDEX orders_id_idx ON "` + schemaName + `".orders (id);`
	withOne := base + `
COMMENT ON SCHEMA "` + schemaName + `" IS 'schema one';
COMMENT ON TABLE "` + schemaName + `".orders IS 'table one';
COMMENT ON COLUMN "` + schemaName + `".orders.id IS 'column one';
COMMENT ON INDEX "` + schemaName + `".orders_id_idx IS 'index one';`
	baseWithNote := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint, note text);
CREATE INDEX orders_id_idx ON "` + schemaName + `".orders (id);`
	withQuoted := baseWithNote + `
COMMENT ON SCHEMA "` + schemaName + `" IS 'schema one';
COMMENT ON TABLE "` + schemaName + `".orders IS 'it''s a table';
COMMENT ON COLUMN "` + schemaName + `".orders.id IS 'column one';
COMMENT ON COLUMN "` + schemaName + `".orders.note IS 'it''s a note';
COMMENT ON INDEX "` + schemaName + `".orders_id_idx IS 'index one';`
	withTwo := baseWithNote + `
COMMENT ON SCHEMA "` + schemaName + `" IS 'schema two';
COMMENT ON TABLE "` + schemaName + `".orders IS 'table two';
COMMENT ON COLUMN "` + schemaName + `".orders.id IS 'column two';
COMMENT ON INDEX "` + schemaName + `".orders_id_idx IS 'index two';`
	steps := []struct {
		name  string
		ddl   string
		check func(string, protocol.Result)
	}{
		{name: "create with comments", ddl: withOne, check: func(sql string, _ protocol.Result) {
			if strings.Index(sql, "CREATE TABLE") > strings.Index(sql, "COMMENT ON TABLE") || strings.Index(sql, "CREATE TABLE") > strings.Index(sql, "COMMENT ON COLUMN") {
				t.Fatalf("creation comments were not ordered after their objects:\n%s", sql)
			}
		}},
		{name: "comments unchanged", ddl: withOne, check: func(_ string, plan protocol.Result) {
			if len(plan.Statements) != 0 {
				t.Fatalf("unchanged comments generated work: %#v", plan.Statements)
			}
		}},
		{name: "column added with quoted comments", ddl: withQuoted, check: func(sql string, _ protocol.Result) {
			if !strings.Contains(sql, "ADD COLUMN \"note\"") || strings.Index(sql, "ADD COLUMN \"note\"") > strings.Index(sql, "COMMENT ON COLUMN") || !strings.Contains(sql, "'it''s a table'") || !strings.Contains(sql, "'it''s a note'") {
				t.Fatalf("added-column or quoted comments were rendered incorrectly:\n%s", sql)
			}
		}},
		{name: "comments changed", ddl: withTwo, check: func(sql string, _ protocol.Result) {
			if !strings.Contains(sql, "IS 'table two'") || !strings.Contains(sql, "IS 'column two'") || !strings.Contains(sql, `orders"."note" IS NULL`) {
				t.Fatalf("comment changes/removal were incomplete:\n%s", sql)
			}
		}},
		{name: "comments removed", ddl: baseWithNote, check: func(sql string, _ protocol.Result) {
			if strings.Count(sql, "IS NULL") < 4 {
				t.Fatalf("comment removals were incomplete:\n%s", sql)
			}
		}},
	}
	for i, step := range steps {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("comments-%d.sql", i))
		if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected comment transition plan: %#v", plan)
		}
		if step.check != nil {
			step.check(joinPlan(plan), plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("comment transition left residual graph diff: got %s want %s", got, want)
		}
	}
}

func TestExpressionIndexWithOpClassConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_expr_index_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (name text);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := currentDDL + `
CREATE INDEX orders_name_pattern_idx ON "` + schemaName + `".orders USING btree ((lower(name)) text_pattern_ops);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `(lower(name)) "text_pattern_ops"`) {
		t.Fatalf("expected expression/opclass index plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("expression index left residual graph diff: got %s want %s", got, want)
	}
}

func TestIndexCollationReplacementConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_index_collation_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (name text);
CREATE INDEX orders_name_idx ON "` + schemaName + `".orders USING btree (name);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (name text);
CREATE INDEX orders_name_idx ON "` + schemaName + `".orders USING btree (name COLLATE "C");`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 3 || !strings.Contains(joinPlan(plan), `COLLATE pg_catalog."C"`) {
		t.Fatalf("expected continuous collation replacement plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("collation replacement left residual graph diff: got %s want %s", got, want)
	}
}

func TestBRINIndexStorageConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_brin_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".events (id bigint, tags text[], bloom_value integer);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := currentDDL + `
CREATE INDEX events_id_brin ON "` + schemaName + `".events USING brin (id) WITH (pages_per_range = 32, autosummarize = true);
CREATE INDEX events_tags_gin ON "` + schemaName + `".events USING gin (tags);
CREATE INDEX events_bloom_brin ON "` + schemaName + `".events USING brin (bloom_value int4_bloom_ops (n_distinct_per_range = 100));`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `USING "brin"`) || !strings.Contains(joinPlan(plan), `"pages_per_range" = 32`) || !strings.Contains(joinPlan(plan), `USING "gin"`) || !strings.Contains(joinPlan(plan), `"int4_bloom_ops" ("n_distinct_per_range" = 100)`) {
		t.Fatalf("expected BRIN storage plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("BRIN index left residual graph diff: got %s want %s", got, want)
	}
}

func TestConstraintDropRetainsStandaloneIndexConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_detach_index_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_id_key UNIQUE (id);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
CREATE UNIQUE INDEX orders_id_key ON "` + schemaName + `".orders (id);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected constraint-drop confirmation: %#v", pending)
	}
	constraintID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "orders_id_key"}).ObjectID()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: constraintID.String(), Value: "drop"}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinPlan(plan)
	if plan.Status != protocol.Planned || strings.Index(joined, `DROP CONSTRAINT "orders_id_key"`) > strings.Index(joined, `CREATE UNIQUE INDEX "orders_id_key"`) {
		t.Fatalf("expected constraint drop before standalone index recreation: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("constraint index detach left residual graph diff: got %s want %s", got, want)
	}
}

func TestPrimaryKeyAddModifyAndDropConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_primary_key_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	baseDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint NOT NULL, external_id bigint NOT NULL);`
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	firstDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (
  id bigint NOT NULL,
  external_id bigint NOT NULL,
  CONSTRAINT orders_pkey PRIMARY KEY (id)
);`
	secondDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (
  id bigint NOT NULL,
  external_id bigint NOT NULL,
  CONSTRAINT orders_pkey PRIMARY KEY (external_id)
);`
	paths := make([]string, 0, 3)
	for i, ddl := range []string{firstDDL, secondDDL, baseDDL} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("primary-%d.sql", i))
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for i, path := range paths {
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		plan := pending
		if i == 2 {
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
				t.Fatalf("expected primary-key drop confirmation: %#v", pending)
			}
			constraintID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "orders_pkey"}).ObjectID()
			answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: constraintID.String(), Value: "drop"}}}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected primary-key plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("primary-key transition left residual graph diff: got %s want %s", got, want)
		}
	}
}

func TestUniqueNullsNotDistinctConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	requirePostgresVersion(t, ctx, conn, 150000, "NULLS NOT DISTINCT")
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_unique_nulls_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (
  id bigint,
  CONSTRAINT orders_id_key UNIQUE NULLS NOT DISTINCT (id)
);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "NULLS NOT DISTINCT") {
		t.Fatalf("expected unique NULLS NOT DISTINCT plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("unique NULLS NOT DISTINCT left residual graph diff: got %s want %s", got, want)
	}
}

func TestNullsNotDistinctScenarioMatrixConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	requirePostgresVersion(t, ctx, conn, 150000, "NULLS NOT DISTINCT")
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_nulls_matrix_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "person") + " (email text, alt text, third text, fourth text);"
	currentDDL := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "person") + " ADD CONSTRAINT person_email_key UNIQUE (email);" +
		"CREATE UNIQUE INDEX person_alt_idx ON " + qualified(schemaName, "person") + " (alt);"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "person") + " ADD CONSTRAINT person_email_key UNIQUE NULLS NOT DISTINCT (email);" +
		"ALTER TABLE " + qualified(schemaName, "person") + " ADD CONSTRAINT person_third_key UNIQUE NULLS NOT DISTINCT (third);" +
		"CREATE UNIQUE INDEX person_alt_idx ON " + qualified(schemaName, "person") + " (alt) NULLS NOT DISTINCT;" +
		"CREATE UNIQUE INDEX person_fourth_idx ON " + qualified(schemaName, "person") + " (fourth) NULLS NOT DISTINCT;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return desired
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired := loadDesired(desiredDDL)
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinPlan(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "NULLS NOT DISTINCT") != 4 || !strings.Contains(joined, "DROP CONSTRAINT") || !strings.Contains(joined, "DROP INDEX") {
		t.Fatalf("expected NULLS NOT DISTINCT add and toggle plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("NULLS NOT DISTINCT state was not stable: plan=%#v err=%v", unchanged, err)
	}

	renamedDDL := strings.Replace(desiredDDL, "person_fourth_idx", "person_fourth_renamed", 1)
	renamedDesired := loadDesired(renamedDDL)
	pending, err := Build(current, renamedDesired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_index" {
		t.Fatalf("expected NULLS NOT DISTINCT index rename question, got %#v", pending)
	}
	oldIndex := pgschema.Index{Table: (pgschema.Table{Schema: schemaName, Name: "person"}).ObjectID(), Name: "person_fourth_idx"}.ObjectID().String()
	newIndex := pgschema.Index{Table: (pgschema.Table{Schema: schemaName, Name: "person"}).ObjectID(), Name: "person_fourth_renamed"}.ObjectID().String()
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_index", Key: oldIndex, Value: newIndex, QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
	plan, err = Build(current, renamedDesired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "ALTER INDEX") {
		t.Fatalf("NULLS NOT DISTINCT index rename plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, renamedDesired)

	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	baseDesired := loadDesired(baseDDL)
	pending, err = Build(current, baseDesired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers = protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		if question.Kind != "drop" {
			t.Fatalf("unexpected NULLS NOT DISTINCT removal question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err = Build(current, baseDesired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "DROP CONSTRAINT") || !strings.Contains(joinPlan(plan), "DROP INDEX") {
		t.Fatalf("NULLS NOT DISTINCT removal plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, baseDesired)
}

func requirePostgresVersion(t *testing.T, ctx context.Context, conn *pgx.Conn, minimum int, feature string) {
	t.Helper()
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < minimum {
		t.Skipf("%s requires PostgreSQL %d or newer; server is %d", feature, minimum/10000, version/10000)
	}
}

func TestPrimaryKeyUsingExistingIndexConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_pk_using_index_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint NOT NULL);
CREATE UNIQUE INDEX orders_pkey ON "` + schemaName + `".orders (id);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := currentDDL + `
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey;`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `ADD CONSTRAINT "orders_pkey" PRIMARY KEY USING INDEX "orders_pkey"`) {
		t.Fatalf("expected primary-key attachment using existing index: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("primary key using index left residual graph diff: got %s want %s", got, want)
	}
}

func TestConstraintUsingExistingIndexConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_using_index_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
CREATE UNIQUE INDEX orders_id_key ON "` + schemaName + `".orders (id);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := currentDDL + `
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_id_key UNIQUE USING INDEX orders_id_key;`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `ADD CONSTRAINT "orders_id_key" UNIQUE USING INDEX "orders_id_key"`) {
		t.Fatalf("expected constraint attachment using existing index: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("using-index constraint left residual graph diff: got %s want %s", got, want)
	}
}

func TestConstraintAndIndexRebuildConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_rebuild_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint, value integer CONSTRAINT value_positive CHECK (value > 0));
CREATE INDEX orders_value_idx ON "` + schemaName + `".orders (value) WHERE value > 0;
COMMENT ON CONSTRAINT value_positive ON "` + schemaName + `".orders IS 'stable check';
COMMENT ON INDEX "` + schemaName + `".orders_value_idx IS 'stable lookup';`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint, value integer CONSTRAINT value_positive CHECK (value >= 0));
CREATE INDEX orders_value_idx ON "` + schemaName + `".orders (value) WHERE value >= 0;
COMMENT ON CONSTRAINT value_positive ON "` + schemaName + `".orders IS 'stable check';
COMMENT ON INDEX "` + schemaName + `".orders_value_idx IS 'stable lookup';`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected planned rebuild: %#v", plan)
	}
	joined := joinPlan(plan)
	if strings.Index(joined, `DROP CONSTRAINT "value_positive"`) > strings.Index(joined, `ADD CONSTRAINT "value_positive"`) {
		t.Fatalf("constraint replacement is not ordered: %s", joined)
	}
	if strings.Index(joined, `DROP INDEX "`+schemaName+`"."orders_value_idx"`) > strings.Index(joined, `CREATE INDEX "orders_value_idx"`) {
		t.Fatalf("index rebuild is not ordered: %s", joined)
	}
	if !strings.Contains(joined, `COMMENT ON CONSTRAINT "value_positive" ON "`+schemaName+`"."orders" IS 'stable check'`) || !strings.Contains(joined, `COMMENT ON INDEX "`+schemaName+`"."orders_value_idx" IS 'stable lookup'`) {
		t.Fatalf("rebuild did not restore identical comments: %s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after rebuild plan\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestIndexRenameConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
CREATE INDEX orders_old_idx ON "` + schemaName + `".orders (id);
COMMENT ON INDEX "` + schemaName + `".orders_old_idx IS 'old';`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
CREATE INDEX orders_new_idx ON "` + schemaName + `".orders (id);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_index" {
		t.Fatalf("expected index rename question: %#v", pending)
	}
	oldID := (pgschema.Index{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "orders_old_idx"}).ObjectID().String()
	newID := (pgschema.Index{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "orders_new_idx"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_index", Key: oldID, Value: newID}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `COMMENT ON INDEX "`+schemaName+`"."orders_new_idx" IS NULL;`) {
		t.Fatalf("expected rename plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after index rename\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestIndexRenameAndReusedNameCommentsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_reused_index_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "items") + " (a integer, b integer);" +
		"CREATE INDEX shared_idx ON " + qualified(schemaName, "items") + " (a);" +
		"COMMENT ON INDEX " + qualified(schemaName, "shared_idx") + " IS 'kept';"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "items") + " (a integer, b integer);" +
		"CREATE INDEX shared_idx ON " + qualified(schemaName, "items") + " (b);" +
		"COMMENT ON INDEX " + qualified(schemaName, "shared_idx") + " IS 'kept';" +
		"CREATE INDEX renamed_idx ON " + qualified(schemaName, "items") + " (a);" +
		"COMMENT ON INDEX " + qualified(schemaName, "renamed_idx") + " IS 'kept';"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan := pending
	joined := joinPlan(plan)
	if plan.Status != protocol.Planned || !strings.Contains(joined, `CREATE INDEX "renamed_idx"`) || !strings.Contains(joined, `CREATE INDEX "shared_idx"`) || !strings.Contains(joined, `COMMENT ON INDEX "`+schemaName+`"."shared_idx" IS 'kept';`) {
		t.Fatalf("reused index name lost its create/comment lifecycle:\n%s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestApprovedDestructiveDropsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_drop_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".keep (id bigint, old_column text, retained_column text);
CREATE INDEX keep_old_column_idx ON "` + schemaName + `".keep (old_column);
CREATE TABLE "` + schemaName + `".remove_me (id bigint);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".keep (id bigint, retained_column text);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 3 {
		t.Fatalf("expected table, column, and index drop questions: %#v", pending)
	}
	keepTable := (pgschema.Table{Schema: schemaName, Name: "keep"}).ObjectID()
	removeTable := (pgschema.Table{Schema: schemaName, Name: "remove_me"}).ObjectID()
	oldColumn := (pgschema.Column{Table: keepTable, Name: "old_column"}).ObjectID()
	oldIndex := (pgschema.Index{Table: keepTable, Name: "keep_old_column_idx"}).ObjectID()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			{Kind: "drop", Key: removeTable.String(), Value: "drop"},
			{Kind: "drop", Key: oldColumn.String(), Value: "drop"},
			{Kind: "drop", Key: oldIndex.String(), Value: "drop"},
		},
	}
	plan, err := Build(current, desired, answers, Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected destructive plan: %#v", plan)
	}
	if !strings.Contains(joinPlan(plan), `DROP INDEX CONCURRENTLY "`+schemaName+`"."keep_old_column_idx";`) {
		t.Fatalf("expected concurrent index drop: %s", joinPlan(plan))
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after approved drops\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestPartitionedTableScenarioMatrixConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_partition_matrix_%d", time.Now().UTC().UnixNano())
	partsSchema := schemaName + "_parts"
	defer func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(partsSchema)+" CASCADE")
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE")
	}()
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return desired
	}
	applyDesired := func(label, ddl string) protocol.Result {
		t.Helper()
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatalf("%s current graph: %v", label, err)
		}
		desired := loadDesired(ddl)
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				if question.Kind != "drop" {
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan
	}

	prefix := `CREATE SCHEMA "` + schemaName + `";
CREATE SCHEMA "` + partsSchema + `";
CREATE TABLE "` + schemaName + `".events (id integer NOT NULL, region text) PARTITION BY RANGE (id);
CREATE TABLE "` + schemaName + `".events_2024 PARTITION OF "` + schemaName + `".events FOR VALUES FROM (1) TO (100);
CREATE TABLE "` + schemaName + `".events_default PARTITION OF "` + schemaName + `".events DEFAULT;
ALTER TABLE "` + schemaName + `".events ADD CONSTRAINT events_pkey PRIMARY KEY (id);
CREATE INDEX events_region_idx ON "` + schemaName + `".events (region);
CREATE TABLE "` + schemaName + `".listed (id integer, region text) PARTITION BY LIST (region);
CREATE TABLE "` + schemaName + `".listed_us PARTITION OF "` + schemaName + `".listed FOR VALUES IN ('us', 'ca');
CREATE TABLE "` + schemaName + `".listed_default PARTITION OF "` + schemaName + `".listed DEFAULT;
CREATE TABLE "` + schemaName + `".hashed (id integer) PARTITION BY HASH (id);
CREATE TABLE "` + schemaName + `".hashed_0 PARTITION OF "` + schemaName + `".hashed FOR VALUES WITH (MODULUS 4, REMAINDER 0);
CREATE TABLE "` + schemaName + `".subroot (id integer, region text) PARTITION BY RANGE (id);
CREATE TABLE "` + schemaName + `".submid PARTITION OF "` + schemaName + `".subroot FOR VALUES FROM (1) TO (100) PARTITION BY LIST (region);
CREATE TABLE "` + schemaName + `".subleaf PARTITION OF "` + schemaName + `".submid FOR VALUES IN ('us');
CREATE TABLE "` + schemaName + `".crossroot (id integer) PARTITION BY RANGE (id);
CREATE TABLE "` + partsSchema + `".crosschild PARTITION OF "` + schemaName + `".crossroot FOR VALUES FROM (1) TO (100);`
	created := applyDesired("create hierarchy", prefix)
	createdSQL := joinPlan(created)
	for _, fragment := range []string{
		"PARTITION BY RANGE (id)",
		"PARTITION BY LIST (region)",
		"PARTITION BY HASH (id)",
		" DEFAULT;",
		"FOR VALUES WITH (modulus 4, remainder 0)",
		"PARTITION OF " + qualified(schemaName, "subroot"),
		"PARTITION OF " + qualified(schemaName, "submid"),
		"CREATE TABLE " + qualified(partsSchema, "crosschild") + " PARTITION OF " + qualified(schemaName, "crossroot"),
		"ADD CONSTRAINT \"events_pkey\" PRIMARY KEY (id)",
		"CREATE INDEX \"events_region_idx\" ON " + qualified(schemaName, "events"),
	} {
		if !strings.Contains(createdSQL, fragment) {
			t.Fatalf("partition creation plan missing %q:\n%s", fragment, createdSQL)
		}
	}

	withAddedPartition := prefix + `
CREATE TABLE "` + schemaName + `".events_2025 PARTITION OF "` + schemaName + `".events FOR VALUES FROM (100) TO (200);`
	added := applyDesired("add partition", withAddedPartition)
	if !strings.Contains(joinPlan(added), "CREATE TABLE "+qualified(schemaName, "events_2025")+" PARTITION OF "+qualified(schemaName, "events")) {
		t.Fatalf("partition add was not rendered directly:\n%s", joinPlan(added))
	}

	removed := applyDesired("remove partition", prefix)
	if !strings.Contains(joinPlan(removed), "DROP TABLE "+qualified(schemaName, "events_2025")+";") {
		t.Fatalf("partition removal was not rendered directly:\n%s", joinPlan(removed))
	}

	withoutHashSubtree := strings.ReplaceAll(prefix,
		"CREATE TABLE \""+schemaName+"\".hashed (id integer) PARTITION BY HASH (id);\n"+
			"CREATE TABLE \""+schemaName+"\".hashed_0 PARTITION OF \""+schemaName+"\".hashed FOR VALUES WITH (MODULUS 4, REMAINDER 0);\n", "")
	dropped := applyDesired("drop partitioned subtree", withoutHashSubtree)
	droppedSQL := joinPlan(dropped)
	if !strings.Contains(droppedSQL, "DROP TABLE "+qualified(schemaName, "hashed")+";") || strings.Contains(droppedSQL, "DROP TABLE "+qualified(schemaName, "hashed_0")+";") {
		t.Fatalf("partitioned subtree should be dropped through its parent only:\n%s", droppedSQL)
	}
}

func TestPartitionedTableCreateConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_partition_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".events (id bigint, occurred_at timestamptz) PARTITION BY RANGE (occurred_at);
CREATE TABLE "` + schemaName + `".events_2026 PARTITION OF "` + schemaName + `".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE "` + schemaName + `".jobs (id bigint, queue text) PARTITION BY LIST (queue);
CREATE TABLE "` + schemaName + `".shards (id bigint) PARTITION BY HASH (id);
CREATE TABLE "` + schemaName + `".daily_events (occurred_at timestamptz) PARTITION BY RANGE ((date_trunc('day', occurred_at AT TIME ZONE 'UTC')));
CREATE TABLE "` + schemaName + `".names (name text) PARTITION BY RANGE (name COLLATE "C");`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "PARTITION BY RANGE (occurred_at)") || !strings.Contains(joinPlan(plan), `PARTITION OF "`+schemaName+`"."events" FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2027-01-01 00:00:00+00')`) || !strings.Contains(joinPlan(plan), "PARTITION BY LIST (queue)") || !strings.Contains(joinPlan(plan), "PARTITION BY HASH (id)") || !strings.Contains(joinPlan(plan), "PARTITION BY RANGE (date_trunc(") || !strings.Contains(joinPlan(plan), `COLLATE "C"`) {
		t.Fatalf("expected partitioned-table plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("partitioned table left residual graph diff: got %s want %s", got, want)
	}
}

func TestTableRenameConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_table_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders_old (id bigint, name text, CONSTRAINT stable_orders_id_key UNIQUE (id));
CREATE INDEX stable_orders_idx ON "` + schemaName + `".orders_old (name);
CREATE FUNCTION "` + schemaName + `".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER audit_orders BEFORE INSERT ON "` + schemaName + `".orders_old FOR EACH ROW EXECUTE FUNCTION "` + schemaName + `".audit_orders();
CREATE VIEW "` + schemaName + `".order_names AS SELECT name FROM "` + schemaName + `".orders_old;
CREATE MATERIALIZED VIEW "` + schemaName + `".order_names_cache AS SELECT name FROM "` + schemaName + `".orders_old;`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders_new (id bigint, name text, CONSTRAINT stable_orders_id_key UNIQUE (id));
CREATE INDEX stable_orders_idx ON "` + schemaName + `".orders_new (name);
CREATE FUNCTION "` + schemaName + `".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER audit_orders BEFORE INSERT ON "` + schemaName + `".orders_new FOR EACH ROW EXECUTE FUNCTION "` + schemaName + `".audit_orders();
CREATE VIEW "` + schemaName + `".order_names AS SELECT name FROM "` + schemaName + `".orders_new;
CREATE MATERIALIZED VIEW "` + schemaName + `".order_names_cache AS SELECT name FROM "` + schemaName + `".orders_new;`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("expected table rename question: %#v", pending)
	}
	oldID := (pgschema.Table{Schema: schemaName, Name: "orders_old"}).ObjectID().String()
	newID := (pgschema.Table{Schema: schemaName, Name: "orders_new"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_table", Key: oldID, Value: newID}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 3 ||
		!strings.Contains(joinSQL(plan), "ALTER TABLE") ||
		!strings.Contains(joinSQL(plan), "CREATE VIEW") ||
		!strings.Contains(joinSQL(plan), "DROP VIEW") {
		t.Fatalf("expected rename plan: %#v", plan)
	}
	applyPlanPhase(t, ctx, conn, plan, "expand")
	var oldKind, newKind string
	if err := conn.QueryRow(ctx, `SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = 'orders_old'`, schemaName).Scan(&oldKind); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = 'orders_new'`, schemaName).Scan(&newKind); err != nil {
		t.Fatal(err)
	}
	if oldKind != "r" || newKind != "v" {
		t.Fatalf("expand must preserve old physical table and expose new compatibility view: old=%q new=%q", oldKind, newKind)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO "`+schemaName+`".orders_old (id, name) VALUES (1, 'old-client') ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`); err != nil {
		t.Fatalf("old application contract is not writable after expand: %v", err)
	}
	var value string
	if err := conn.QueryRow(ctx, `SELECT name FROM "`+schemaName+`".orders_new WHERE id = 1`).Scan(&value); err != nil || value != "old-client" {
		t.Fatalf("new application contract cannot observe old-client write: value=%q err=%v", value, err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO "`+schemaName+`".orders_new (id, name) VALUES (2, 'new-client')`); err != nil {
		t.Fatalf("new application contract is not writable after expand: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT name FROM "`+schemaName+`".orders_old WHERE id = 2`).Scan(&value); err != nil || value != "new-client" {
		t.Fatalf("old application contract cannot observe new-client write: value=%q err=%v", value, err)
	}
	applyPlanPhase(t, ctx, conn, plan, "contract")
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after table rename\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestTableRenameWithDeclarativeDerivedNamesConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_derived_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint PRIMARY KEY, email text UNIQUE);
CREATE INDEX accounts_email_idx ON "` + schemaName + `".accounts (email);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".customers (id bigint PRIMARY KEY, email text UNIQUE);
CREATE INDEX customers_email_idx ON "` + schemaName + `".customers (email);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_table" {
		t.Fatalf("declarative export must offer table rename: %#v", pending)
	}
	from := (pgschema.Table{Schema: schemaName, Name: "accounts"}).ObjectID().String()
	to := (pgschema.Table{Schema: schemaName, Name: "customers"}).ObjectID().String()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_table", Key: from, Value: to}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), `RENAME CONSTRAINT "accounts_pkey" TO "customers_pkey"`) || !strings.Contains(joinSQL(plan), `RENAME TO "customers_email_idx"`) {
		t.Fatalf("expected explicit conventional child renames: %#v", plan)
	}
	if strings.HasPrefix(conn.PgConn().ParameterStatus("server_version"), "18.") && !strings.Contains(joinSQL(plan), `RENAME CONSTRAINT "accounts_id_not_null" TO "customers_id_not_null"`) {
		t.Fatalf("expected PostgreSQL 18 generated NOT NULL constraint rename: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("derived-name rename left residual graph diff: got %s want %s", got, want)
	}
}

func TestColumnRenameWithExistingTriggerIsRejectedOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_column_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (old_name text CONSTRAINT orders_name_check CHECK (old_name <> 'reserved'));
CREATE FUNCTION "` + schemaName + `".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER old_name_audit BEFORE UPDATE OF old_name ON "` + schemaName + `".orders FOR EACH ROW WHEN (OLD.old_name IS DISTINCT FROM NEW.old_name) EXECUTE FUNCTION "` + schemaName + `".audit_orders();
INSERT INTO "` + schemaName + `".orders (old_name) VALUES ('kept');
CREATE VIEW "` + schemaName + `".order_view AS SELECT old_name FROM "` + schemaName + `".orders;
CREATE MATERIALIZED VIEW "` + schemaName + `".order_cache AS SELECT old_name FROM "` + schemaName + `".orders;`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (new_name text CONSTRAINT orders_name_check CHECK (new_name <> 'reserved'));
CREATE FUNCTION "` + schemaName + `".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER old_name_audit BEFORE UPDATE OF new_name ON "` + schemaName + `".orders FOR EACH ROW WHEN (OLD.new_name IS DISTINCT FROM NEW.new_name) EXECUTE FUNCTION "` + schemaName + `".audit_orders();
CREATE VIEW "` + schemaName + `".order_view AS SELECT new_name AS old_name FROM "` + schemaName + `".orders;
CREATE MATERIALIZED VIEW "` + schemaName + `".order_cache AS SELECT new_name AS old_name FROM "` + schemaName + `".orders;`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("expected column rename question: %#v", pending)
	}
	tableID := (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID()
	oldID := (pgschema.Column{Table: tableID, Name: "old_name"}).ObjectID().String()
	newID := (pgschema.Column{Table: tableID, Name: "new_name"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_column", Key: oldID, Value: newID}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertUnsupportedPrefix(t, plan, "single_deployment_column_bridge_required:"+oldID+":existing_trigger_order")
}

func TestColumnRenameOverlapBridgeConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_column_overlap_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint PRIMARY KEY, old_name text);
INSERT INTO "` + schemaName + `".orders (id, old_name) VALUES (1, 'existing');`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint PRIMARY KEY, new_name text);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("expected column rename question: %#v", pending)
	}
	tableID := (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID()
	oldID := (pgschema.Column{Table: tableID, Name: "old_name"}).ObjectID().String()
	newID := (pgschema.Column{Table: tableID, Name: "new_name"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_column", Key: oldID, Value: newID}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 9 {
		t.Fatalf("expected automatic two-phase overlap bridge: %#v", plan)
	}
	applyPlanPhase(t, ctx, conn, plan, protocol.PhaseExpand)

	var oldValue, newValue string
	if err := conn.QueryRow(ctx, `SELECT old_name, new_name FROM "`+schemaName+`".orders WHERE id = 1`).Scan(&oldValue, &newValue); err != nil || oldValue != "existing" || newValue != "existing" {
		t.Fatalf("initial backfill mismatch: old=%q new=%q err=%v", oldValue, newValue, err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO "`+schemaName+`".orders (id, old_name) VALUES (2, 'old-writer')`); err != nil {
		t.Fatalf("old application write failed: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO "`+schemaName+`".orders (id, new_name) VALUES (3, 'new-writer')`); err != nil {
		t.Fatalf("new application write failed: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT old_name, new_name FROM "`+schemaName+`".orders WHERE id = 3`).Scan(&oldValue, &newValue); err != nil || oldValue != "new-writer" || newValue != "new-writer" {
		t.Fatalf("new application write did not reach old contract: old=%q new=%q err=%v", oldValue, newValue, err)
	}
	if _, err := conn.Exec(ctx, `UPDATE "`+schemaName+`".orders SET old_name = 'old-update' WHERE id = 2`); err != nil {
		t.Fatalf("old application update failed: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT old_name, new_name FROM "`+schemaName+`".orders WHERE id = 2`).Scan(&oldValue, &newValue); err != nil || oldValue != "old-update" || newValue != "old-update" {
		t.Fatalf("old application update did not reach new contract: old=%q new=%q err=%v", oldValue, newValue, err)
	}
	if _, err := conn.Exec(ctx, `UPDATE "`+schemaName+`".orders SET old_name = 'conflict-old', new_name = 'conflict-new' WHERE id = 2`); err == nil {
		t.Fatal("conflicting simultaneous old/new write must fail")
	}

	applyPlanPhase(t, ctx, conn, plan, protocol.PhaseContract)
	if err := conn.QueryRow(ctx, `SELECT new_name FROM "`+schemaName+`".orders WHERE id = 3`).Scan(&newValue); err != nil || newValue != "new-writer" {
		t.Fatalf("contract lost bridged value: new=%q err=%v", newValue, err)
	}
	assertGraphConverges(t, ctx, url, desired)
}

func TestConstraintRenamesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	tests := []struct {
		name       string
		oldName    string
		newName    string
		currentDDL func(string) string
		desiredDDL func(string) string
		want       string
	}{
		{
			name: "unique and comment clearing", oldName: "person_email_old", newName: "person_email_new", want: "COMMENT ON CONSTRAINT",
			currentDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".person (email text);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT person_email_old UNIQUE (email);COMMENT ON CONSTRAINT person_email_old ON " + quote(schema) + ".person IS 'unique email';"
			},
			desiredDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".person (email text);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT person_email_new UNIQUE (email);"
			},
		},
		{
			name: "check", oldName: "person_value_old", newName: "person_value_new",
			currentDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".person (value integer);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT person_value_old CHECK (value > 0);"
			},
			desiredDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".person (value integer);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT person_value_new CHECK (value > 0);"
			},
		},
		{
			name: "exclusion", oldName: "period_old", newName: "period_new",
			currentDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".periods (during int4range);ALTER TABLE " + quote(schema) + ".periods ADD CONSTRAINT period_old EXCLUDE USING gist (during WITH &&);"
			},
			desiredDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".periods (during int4range);ALTER TABLE " + quote(schema) + ".periods ADD CONSTRAINT period_new EXCLUDE USING gist (during WITH &&);"
			},
		},
		{
			name: "nulls not distinct", oldName: "email_old", newName: "email_new",
			currentDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".person (email text);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT email_old UNIQUE NULLS NOT DISTINCT (email);"
			},
			desiredDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".person (email text);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT email_new UNIQUE NULLS NOT DISTINCT (email);"
			},
		},
		{
			name: "foreign key", oldName: "account_old", newName: "account_new",
			currentDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".accounts (id integer PRIMARY KEY);CREATE TABLE " + quote(schema) + ".person (account_id integer);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT account_old FOREIGN KEY (account_id) REFERENCES " + quote(schema) + ".accounts(id);"
			},
			desiredDDL: func(schema string) string {
				return "CREATE TABLE " + quote(schema) + ".accounts (id integer PRIMARY KEY);CREATE TABLE " + quote(schema) + ".person (account_id integer);ALTER TABLE " + quote(schema) + ".person ADD CONSTRAINT account_new FOREIGN KEY (account_id) REFERENCES " + quote(schema) + ".accounts(id);"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemaName := "onwardpg_constraint_rename_" + strings.ReplaceAll(test.name, " ", "_") + "_" + time.Now().UTC().Format("150405")
			baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
			if _, err := conn.Exec(ctx, baseDDL+test.currentDDL(schemaName)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") })
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(baseDDL+test.desiredDDL(schemaName)), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_constraint" {
				t.Fatalf("expected constraint rename question: %#v", pending)
			}
			answers := protocol.Answers{
				ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
				Answers: []protocol.Answer{{Kind: "rename_constraint", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}},
			}
			plan, err := Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
			joined := joinSQL(plan)
			if plan.Status != protocol.Planned || !strings.Contains(joined, "RENAME CONSTRAINT "+quote(test.oldName)+" TO "+quote(test.newName)) {
				t.Fatalf("expected confirmed constraint rename: %#v", plan)
			}
			if test.want != "" && (!strings.Contains(joined, test.want) || !strings.Contains(joined, "IS NULL;")) {
				t.Fatalf("constraint rename did not clear comment: %#v", plan.Statements)
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}
}

func TestDomainLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_domain_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	age := func(suffix string) string {
		return "CREATE DOMAIN " + quote(schemaName) + ".age AS integer" + suffix + ";"
	}
	positive := func(baseType string) string {
		return "CREATE DOMAIN " + quote(schemaName) + ".positive_int AS " + baseType + " DEFAULT 1 NOT NULL CONSTRAINT positive_int_check CHECK (VALUE > 0);"
	}
	steps := []struct {
		name      string
		ddl       string
		want      string
		unchanged bool
		drop      bool
	}{
		{name: "create", ddl: baseDDL + age(""), want: "CREATE DOMAIN"},
		{name: "unchanged", ddl: baseDDL + age(""), unchanged: true},
		{name: "add default", ddl: baseDDL + age(" DEFAULT 0"), want: "SET DEFAULT 0;"},
		{name: "change default", ddl: baseDDL + age(" DEFAULT 18"), want: "SET DEFAULT 18;"},
		{name: "drop default", ddl: baseDDL + age(""), want: "DROP DEFAULT;"},
		{name: "set not null", ddl: baseDDL + age(" NOT NULL"), want: "SET NOT NULL;"},
		{name: "drop not null", ddl: baseDDL + age(""), want: "DROP NOT NULL;"},
		{name: "add check", ddl: baseDDL + age(" CONSTRAINT age_positive CHECK (VALUE > 0)"), want: `ADD CONSTRAINT "age_positive"`},
		{name: "rename check", ddl: baseDDL + age(" CONSTRAINT age_new CHECK (VALUE > 0)"), want: `RENAME CONSTRAINT "age_positive" TO "age_new";`},
		{name: "drop check", ddl: baseDDL + age(""), want: `DROP CONSTRAINT "age_new";`},
		{name: "add old comment", ddl: baseDDL + age("") + "COMMENT ON DOMAIN " + quote(schemaName) + ".age IS 'old';", want: "IS 'old';"},
		{name: "change comment", ddl: baseDDL + age("") + "COMMENT ON DOMAIN " + quote(schemaName) + ".age IS 'new';", want: "IS 'new';"},
		{name: "create fully attributed", ddl: baseDDL + age("") + "COMMENT ON DOMAIN " + quote(schemaName) + ".age IS 'new';" + positive("integer"), want: `ADD CONSTRAINT "positive_int_check"`},
		{name: "drop", ddl: baseDDL + positive("integer"), want: "DROP DOMAIN", drop: true},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if step.drop {
				if plan.Status != protocol.NeedsInput || len(plan.Questions) != 1 || plan.Questions[0].Kind != "drop" {
					t.Fatalf("expected domain drop confirmation: %#v", plan)
				}
				answers := protocol.Answers{
					ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint,
					Answers: []protocol.Answer{{Kind: "drop", Key: plan.Questions[0].Key, Value: "drop"}},
				}
				plan, err = Build(current, desired, answers, Options{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("expected domain lifecycle plan: %#v", plan)
			}
			if step.unchanged {
				if len(plan.Statements) != 0 {
					t.Fatalf("unchanged domain generated work: %#v", plan.Statements)
				}
			} else if !strings.Contains(joinSQL(plan), step.want) {
				t.Fatalf("domain plan missing %q: %#v", step.want, plan.Statements)
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}

	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "base-type-change.sql")
	if err := os.WriteFile(path, []byte(baseDDL+positive("bigint")), 0o600); err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertUnsupportedPrefix(t, plan, "domain_base_type_change:")
}

func TestDomainDependencyOrderingConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_domain_dependency_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ('sad', 'happy');" +
		"CREATE DOMAIN " + quote(schemaName) + ".mood_value AS " + quote(schemaName) + ".mood DEFAULT 'sad';" +
		"CREATE TABLE " + quote(schemaName) + ".feelings (value " + quote(schemaName) + ".mood_value);"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	enumAt, domainAt, tableAt := strings.Index(joined, "CREATE TYPE"), strings.Index(joined, "CREATE DOMAIN"), strings.Index(joined, "CREATE TABLE")
	if plan.Status != protocol.Planned || enumAt < 0 || domainAt <= enumAt || tableAt <= domainAt {
		t.Fatalf("domain dependency order is wrong: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestCompositeTypeLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_composite_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	typeDDL := func(attributes string) string {
		return "CREATE TYPE " + quote(schemaName) + ".pair AS (" + attributes + ");CREATE TYPE " + quote(schemaName) + ".empty AS ();COMMENT ON TYPE " + quote(schemaName) + ".pair IS 'hi';"
	}
	steps := []struct {
		name      string
		ddl       string
		want      []string
		unchanged bool
	}{
		{name: "create and comment", ddl: baseDDL + typeDDL("a integer, b integer"), want: []string{"CREATE TYPE", "COMMENT ON TYPE"}},
		{name: "unchanged", ddl: baseDDL + typeDDL("a integer, b integer"), unchanged: true},
		{name: "add attribute", ddl: baseDDL + typeDDL("a integer, b integer, c integer"), want: []string{`ADD ATTRIBUTE "c" integer CASCADE;`}},
		{name: "alter attribute", ddl: baseDDL + typeDDL("a integer, b bigint, c integer"), want: []string{`ALTER ATTRIBUTE "b" TYPE bigint CASCADE;`}},
		{name: "combined", ddl: baseDDL + typeDDL("a bigint, c integer, d integer"), want: []string{`DROP ATTRIBUTE "b" CASCADE;`, `ALTER ATTRIBUTE "a" TYPE bigint CASCADE;`, `ADD ATTRIBUTE "d" integer CASCADE;`}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("expected composite plan: %#v", plan)
			}
			if step.unchanged && len(plan.Statements) != 0 {
				t.Fatalf("unchanged composite generated work: %#v", plan.Statements)
			}
			joined := joinSQL(plan)
			for _, want := range step.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("composite plan missing %q: %#v", want, plan.Statements)
				}
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}

	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	reorderPath := filepath.Join(t.TempDir(), "reorder.sql")
	if err := os.WriteFile(reorderPath, []byte(baseDDL+typeDDL("c integer, a bigint, d integer")), 0o600); err != nil {
		t.Fatal(err)
	}
	reordered, err := source.LoadGraph(ctx, source.Parse("file://"+reorderPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, reordered, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertUnsupportedPrefix(t, plan, "composite_attribute_reorder:")

	dropPath := filepath.Join(t.TempDir(), "drop.sql")
	if err := os.WriteFile(dropPath, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	dropped, err := source.LoadGraph(ctx, source.Parse("file://"+dropPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, dropped, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: "drop", Key: question.Key, Value: "drop"})
	}
	plan, err = Build(current, dropped, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "DROP TYPE") != 2 {
		t.Fatalf("expected two composite drops: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, dropped)
}

func TestCompositeTypeDependenciesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_composite_dependency_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";CREATE TYPE " + quote(schemaName) + ".pair AS (a integer);CREATE TABLE " + quote(schemaName) + ".items (value " + quote(schemaName) + ".pair);"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"CREATE TYPE " + quote(schemaName) + ".z_new AS (v integer);" +
		"ALTER TYPE " + quote(schemaName) + ".pair ADD ATTRIBUTE b " + quote(schemaName) + ".z_new;" +
		"CREATE TYPE " + quote(schemaName) + ".z_coord AS (v integer);" +
		"CREATE TYPE " + quote(schemaName) + ".a_point AS (c " + quote(schemaName) + ".z_coord);" +
		"CREATE TYPE " + quote(schemaName) + ".a_poly AS (pts " + quote(schemaName) + ".z_coord[]);" +
		"CREATE TYPE " + quote(schemaName) + ".a_inner AS (v integer);" +
		"CREATE TYPE " + quote(schemaName) + ".z_outer AS (c " + quote(schemaName) + ".a_inner);"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || strings.Index(joined, `."z_coord" AS`) > strings.Index(joined, `."a_point" AS`) || strings.Index(joined, `."z_coord" AS`) > strings.Index(joined, `."a_poly" AS`) || strings.Index(joined, `."z_new" AS`) > strings.Index(joined, `ADD ATTRIBUTE "b"`) {
		t.Fatalf("composite dependency order is wrong: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	retainedDDL := baseDDL +
		"CREATE TYPE " + quote(schemaName) + ".z_new AS (v integer);" +
		"ALTER TYPE " + quote(schemaName) + ".pair ADD ATTRIBUTE b " + quote(schemaName) + ".z_new;"
	dropPath := filepath.Join(t.TempDir(), "drop-dependencies.sql")
	if err := os.WriteFile(dropPath, []byte(retainedDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	dropDesired, err := source.LoadGraph(ctx, source.Parse("file://"+dropPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, dropDesired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: "drop", Key: question.Key, Value: "drop"})
	}
	plan, err = Build(current, dropDesired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined = joinSQL(plan)
	outerAt, innerAt := strings.Index(joined, `."z_outer";`), strings.Index(joined, `."a_inner";`)
	pointAt, coordAt := strings.Index(joined, `."a_point";`), strings.Index(joined, `."z_coord";`)
	polyAt := strings.Index(joined, `."a_poly";`)
	if plan.Status != protocol.Planned || outerAt < 0 || innerAt < 0 || outerAt > innerAt || pointAt < 0 || polyAt < 0 || coordAt < 0 || pointAt > coordAt || polyAt > coordAt {
		t.Fatalf("composite drop dependency order is wrong: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, dropDesired)
}

func TestTableOwnershipChangesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	suffix := fmt.Sprint(time.Now().UTC().UnixNano())
	schemaName := "onwardpg_owner_" + suffix
	roleA, roleB := "onwardpg_owner_a_"+suffix, "onwardpg_owner_b_"+suffix
	q := quote(schemaName)
	defer func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
		_, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+quote(roleA))
		_, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+quote(roleB))
	}()
	if _, err := conn.Exec(ctx, "CREATE ROLE "+quote(roleA)+"; CREATE ROLE "+quote(roleB)+"; CREATE SCHEMA "+q+"; CREATE TABLE "+q+".person (name text); ALTER TABLE "+q+".person OWNER TO "+quote(roleA)+";"); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + q + "; CREATE TABLE " + q + ".person (name text); ALTER TABLE " + q + ".person OWNER TO " + quote(roleB) + ";"
	path := filepath.Join(t.TempDir(), "owner.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "change_table_owner" {
		t.Fatalf("table owner transfer must require authorization: %#v", pending)
	}
	question := pending.Questions[0]
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: "authorize", QuestionFingerprint: question.ScopeFingerprint}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER TABLE "+qualified(schemaName, "person")+" OWNER TO "+quote(roleB)) {
		t.Fatalf("table owner transfer was not planned: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged table owner generated work: %#v", unchanged)
	}
	if _, err := conn.Exec(ctx, "ALTER TABLE "+q+".person OWNER TO "+quote(roleA)); err != nil {
		t.Fatal(err)
	}
	selector := "table_owner:" + schemaName + ".person"
	ignoredCurrent, err := source.LoadGraph(ctx, source.Parse(url), "", []string{selector})
	if err != nil {
		t.Fatal(err)
	}
	ignoredDesired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, []string{selector})
	if err != nil {
		t.Fatal(err)
	}
	ignored, err := Build(ignoredCurrent, ignoredDesired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Status != protocol.Planned || len(ignored.Statements) != 0 {
		t.Fatalf("ignored table owner generated work: %#v", ignored)
	}
}

func TestColumnCollationChangesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_collation_" + time.Now().UTC().Format("20060102150405")
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + q + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	for index, step := range []struct {
		ddl, want, absent string
		unchanged         bool
	}{
		{ddl: baseDDL + "CREATE TABLE " + q + `.t (c text COLLATE "C");`, want: `COLLATE pg_catalog."C"`},
		{ddl: baseDDL + "CREATE TABLE " + q + `.t (c text COLLATE "C");`, unchanged: true},
		{ddl: baseDDL + "CREATE TABLE " + q + `.t (c text COLLATE "POSIX");`, want: `COLLATE pg_catalog."POSIX"`},
		{ddl: baseDDL + "CREATE TABLE " + q + ".t (c text);", want: `ALTER COLUMN "c" TYPE text USING`, absent: "COLLATE"},
	} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("collation-%d.sql", index))
		if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		joined := joinSQL(plan)
		if plan.Status != protocol.Planned || step.unchanged && len(plan.Statements) != 0 || !step.unchanged && !strings.Contains(joined, step.want) || step.absent != "" && strings.Contains(joined, step.absent) {
			t.Fatalf("column collation transition is incomplete: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
	}
}

func TestGeneratedColumnExpressionChangesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	schemaName := "onwardpg_generated_" + time.Now().UTC().Format("20060102150405")
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + q + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	tableDDL := func(storedType string, storedFactor, virtualFactor int, added bool) string {
		columns := "b integer"
		if version >= 180000 {
			columns += fmt.Sprintf(", virtual_one integer GENERATED ALWAYS AS (b * %d) VIRTUAL", virtualFactor)
		}
		columns += fmt.Sprintf(", stored_one %s GENERATED ALWAYS AS (b * %d) STORED", storedType, storedFactor)
		if version >= 180000 && added {
			columns += ", virtual_added integer GENERATED ALWAYS AS (b + 1) VIRTUAL"
		}
		return baseDDL + "CREATE TABLE " + q + ".item (" + columns + ");"
	}
	applyDesired := func(name, ddl string, approveDrops bool) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		path := filepath.Join(t.TempDir(), name+".sql")
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if approveDrops && plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("generated-column %s plan failed: %#v", name, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}
	created, _ := applyDesired("create", tableDDL("integer", 2, 2, false), false)
	if version >= 180000 && !strings.Contains(joinSQL(created), " VIRTUAL") {
		t.Fatalf("virtual generated column was not rendered: %#v", created)
	}
	typeChanged, _ := applyDesired("type-change", tableDDL("bigint", 2, 2, false), false)
	if sql := joinSQL(typeChanged); !strings.Contains(sql, `ALTER COLUMN "stored_one" TYPE bigint;`) || strings.Contains(sql, " USING ") {
		t.Fatalf("generated-column type change must omit USING: %#v", typeChanged)
	}
	changed, _ := applyDesired("change", tableDDL("bigint", 3, 5, false), false)
	joined := joinSQL(changed)
	if !strings.Contains(joined, `DROP COLUMN "stored_one"`) || !strings.Contains(joined, `ADD COLUMN "stored_one"`) {
		t.Fatalf("stored generated expression was not rebuilt: %#v", changed)
	}
	if version >= 180000 && !strings.Contains(joined, `ALTER COLUMN "virtual_one" SET EXPRESSION AS`) {
		t.Fatalf("virtual generated expression was not changed in place: %#v", changed)
	}
	if version >= 180000 {
		added, _ := applyDesired("add", tableDDL("bigint", 3, 5, true), false)
		if !strings.Contains(joinSQL(added), `ADD COLUMN "virtual_added"`) || !strings.Contains(joinSQL(added), " VIRTUAL") {
			t.Fatalf("virtual generated column was not added: %#v", added)
		}
		dropped, _ := applyDesired("drop", tableDDL("bigint", 3, 5, false), true)
		if !strings.Contains(joinSQL(dropped), `DROP COLUMN "virtual_added"`) {
			t.Fatalf("virtual generated column was not dropped: %#v", dropped)
		}
	}
}

func TestRangeTypeLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_range_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	commonDDL := func(changeSubtype, diffOption string) string {
		return baseDDL +
			"CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ('sad', 'ok', 'glad');" +
			"CREATE TYPE " + quote(schemaName) + ".r_mood AS RANGE (SUBTYPE = " + quote(schemaName) + ".mood);" +
			"CREATE TYPE " + quote(schemaName) + ".r_int AS RANGE (SUBTYPE = integer);" +
			"COMMENT ON TYPE " + quote(schemaName) + ".r_int IS 'hi';" +
			"CREATE TYPE " + quote(schemaName) + ".r_float AS RANGE (SUBTYPE = float8, SUBTYPE_DIFF = float8mi);" +
			"CREATE TYPE " + quote(schemaName) + ".r_text AS RANGE (SUBTYPE = text, SUBTYPE_OPCLASS = text_pattern_ops, COLLATION = \"C\");" +
			"CREATE TYPE " + quote(schemaName) + ".r_change AS RANGE (SUBTYPE = " + changeSubtype + ");" +
			"COMMENT ON TYPE " + quote(schemaName) + ".r_change IS 'same';" +
			"CREATE TYPE " + quote(schemaName) + ".r_diff AS RANGE (SUBTYPE = float8" + diffOption + ");" +
			"CREATE TABLE " + quote(schemaName) + ".events (span " + quote(schemaName) + ".r_int);"
	}
	path := filepath.Join(t.TempDir(), "create.sql")
	if err := os.WriteFile(path, []byte(commonDDL("integer", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || !strings.Contains(joined, "SUBTYPE_DIFF = float8mi") || !strings.Contains(joined, "SUBTYPE_OPCLASS = pg_catalog.text_pattern_ops") || !strings.Contains(joined, `COLLATION = pg_catalog."C"`) || strings.Index(joined, "CREATE TYPE "+qualified(schemaName, "mood")) > strings.Index(joined, "CREATE TYPE "+qualified(schemaName, "r_mood")) || strings.Index(joined, "CREATE TYPE "+qualified(schemaName, "r_int")) > strings.Index(joined, "CREATE TABLE") {
		t.Fatalf("range create plan is incomplete or misordered: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged ranges generated work: %#v", unchanged)
	}

	recreatePath := filepath.Join(t.TempDir(), "recreate.sql")
	if err := os.WriteFile(recreatePath, []byte(commonDDL("bigint", ", SUBTYPE_DIFF = float8mi")), 0o600); err != nil {
		t.Fatal(err)
	}
	recreated, err := source.LoadGraph(ctx, source.Parse("file://"+recreatePath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, recreated, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined = joinSQL(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "DROP TYPE") != 2 || strings.Count(joined, "COMMENT ON TYPE") != 1 || !strings.Contains(joined, "SUBTYPE = bigint") || !strings.Contains(joined, "SUBTYPE_DIFF = float8mi") {
		t.Fatalf("range recreation did not preserve properties/comments: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, recreated)

	dependentPath := filepath.Join(t.TempDir(), "dependent-rewrite.sql")
	dependentDDL := strings.Replace(commonDDL("bigint", ", SUBTYPE_DIFF = float8mi"), "r_int AS RANGE (SUBTYPE = integer)", "r_int AS RANGE (SUBTYPE = bigint)", 1)
	if err := os.WriteFile(dependentPath, []byte(dependentDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	dependentDesired, err := source.LoadGraph(ctx, source.Parse("file://"+dependentPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, dependentDesired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertUnsupportedPrefix(t, plan, "range_rewrite_dependents:")

	dropDDL := commonDDL("bigint", ", SUBTYPE_DIFF = float8mi")
	dropDDL = strings.Replace(dropDDL,
		"CREATE TYPE "+quote(schemaName)+".r_change AS RANGE (SUBTYPE = bigint);"+
			"COMMENT ON TYPE "+quote(schemaName)+".r_change IS 'same';", "", 1)
	dropDDL = strings.Replace(dropDDL,
		"CREATE TYPE "+quote(schemaName)+".r_diff AS RANGE (SUBTYPE = float8, SUBTYPE_DIFF = float8mi);", "", 1)
	dropPath := filepath.Join(t.TempDir(), "drop.sql")
	if err := os.WriteFile(dropPath, []byte(dropDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	dropDesired, err := source.LoadGraph(ctx, source.Parse("file://"+dropPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, dropDesired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: "drop", Key: question.Key, Value: "drop"})
	}
	plan, err = Build(current, dropDesired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined = joinSQL(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "DROP TYPE") != 2 || !strings.Contains(joined, qualified(schemaName, "r_change")) || !strings.Contains(joined, qualified(schemaName, "r_diff")) {
		t.Fatalf("approved range drops were not planned: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, dropDesired)
}

func TestEnumCreateAndDropConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_lifecycle_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	baseDDL := `CREATE SCHEMA "` + schemaName + `";`
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	withEnum := baseDDL + `
CREATE TYPE "` + schemaName + `".state AS ENUM ('open', 'closed');`
	paths := make([]string, 0, 2)
	for i, ddl := range []string{withEnum, baseDDL} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("enum-%d.sql", i))
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for i, path := range paths {
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		plan := pending
		if i == 1 {
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
				t.Fatalf("expected enum-drop confirmation: %#v", pending)
			}
			enumID := (pgschema.Enum{Schema: schemaName, Name: "state"}).ObjectID()
			answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: enumID.String(), Value: "drop"}}}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected enum lifecycle plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("enum lifecycle left residual graph diff: got %s want %s", got, want)
		}
	}
}

func TestEnumBasicLifecycleConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_basic_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	ddl := func(labels string) string {
		return baseDDL + "CREATE TYPE " + quote(schemaName) + ".state AS ENUM (" + labels + ");" +
			"CREATE TABLE " + quote(schemaName) + ".events (state " + quote(schemaName) + ".state);"
	}
	steps := []struct {
		name, ddl, contains string
	}{
		{name: "create", ddl: ddl("'open', 'closed'"), contains: "CREATE TABLE"},
		{name: "append", ddl: ddl("'open', 'closed', 'archived'"), contains: "ADD VALUE 'archived'"},
		{name: "insert", ddl: ddl("'open', 'pending', 'closed', 'archived'"), contains: "ADD VALUE 'pending'"},
	}
	for _, step := range steps {
		path := filepath.Join(t.TempDir(), step.name+".sql")
		if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		joined := joinSQL(plan)
		if plan.Status != protocol.Planned || !strings.Contains(joined, step.contains) {
			t.Fatalf("enum %s plan is incomplete: %#v", step.name, plan)
		}
		if step.name == "create" && strings.Index(joined, "CREATE TYPE") > strings.Index(joined, "CREATE TABLE") {
			t.Fatalf("typed enum column was created before its type: %s", joined)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		unchanged, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
			t.Fatalf("unchanged enum %s generated work: %#v", step.name, unchanged)
		}
	}
}

func TestEnumCommentsAndEmptyEnumConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_comment_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		name string
		ddl  string
		want string
	}{
		{name: "create empty with comment", ddl: baseDDL + "CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ();COMMENT ON TYPE " + quote(schemaName) + ".mood IS 'feelings';", want: "AS ENUM ();"},
		{name: "change comment", ddl: baseDDL + "CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ();COMMENT ON TYPE " + quote(schemaName) + ".mood IS 'new feelings';", want: "IS 'new feelings';"},
		{name: "remove comment", ddl: baseDDL + "CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ();", want: "IS NULL;"},
		{name: "add comment", ddl: baseDDL + "CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ();COMMENT ON TYPE " + quote(schemaName) + ".mood IS 'restored';", want: "IS 'restored';"},
		{name: "unchanged comment", ddl: baseDDL + "CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ();COMMENT ON TYPE " + quote(schemaName) + ".mood IS 'restored';"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("expected enum comment plan: %#v", plan)
			}
			if step.want == "" {
				if len(plan.Statements) != 0 {
					t.Fatalf("unchanged enum comment generated work: %#v", plan.Statements)
				}
			} else if !strings.Contains(joinSQL(plan), step.want) {
				t.Fatalf("enum comment plan missing %q: %#v", step.want, plan.Statements)
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}
}

func TestEnumRewriteConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_rewrite_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	currentDDL := baseDDL +
		"CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ('sad', 'ok', 'happy');" +
		"COMMENT ON TYPE " + quote(schemaName) + ".mood IS 'feelings';" +
		"CREATE TYPE " + quote(schemaName) + ".mixed AS ENUM ('sad', 'ok');" +
		"CREATE TYPE " + quote(schemaName) + ".ordered AS ENUM ('sad', 'happy');" +
		"CREATE TABLE " + quote(schemaName) + ".t1 (c " + quote(schemaName) + ".mood DEFAULT 'sad');" +
		"CREATE TABLE " + quote(schemaName) + ".t2 (c " + quote(schemaName) + ".mood[]);" +
		"CREATE TABLE " + quote(schemaName) + ".t3 (c " + quote(schemaName) + ".mood);" +
		"INSERT INTO " + quote(schemaName) + ".t1 DEFAULT VALUES;" +
		"INSERT INTO " + quote(schemaName) + ".t2 VALUES (ARRAY['happy']::" + quote(schemaName) + ".mood[]);" +
		"INSERT INTO " + quote(schemaName) + ".t3 VALUES ('sad');"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"CREATE TYPE " + quote(schemaName) + ".mood AS ENUM ('sad', 'happy');" +
		"COMMENT ON TYPE " + quote(schemaName) + ".mood IS 'feelings';" +
		"CREATE TYPE " + quote(schemaName) + ".mixed AS ENUM ('sad', 'fine', 'happy');" +
		"CREATE TYPE " + quote(schemaName) + ".ordered AS ENUM ('happy', 'sad');" +
		"CREATE TABLE " + quote(schemaName) + ".t1 (c " + quote(schemaName) + ".mood DEFAULT 'sad');" +
		"CREATE TABLE " + quote(schemaName) + ".t2 (c " + quote(schemaName) + ".mood[]);" +
		"CREATE TABLE " + quote(schemaName) + ".t3 (c " + quote(schemaName) + ".mood);"
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 3 {
		t.Fatalf("enum rewrites must require three confirmations: %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "rewrite", QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "RENAME TO \"onwardpg_tmpenum_") != 3 || strings.Count(joined, "CREATE TYPE ") != 3 || strings.Count(joined, " ALTER COLUMN \"c\" TYPE ") != 3 || !strings.Contains(joined, `::text[]::`+schemaName+`.mood[]`) || !strings.Contains(joined, ` ALTER COLUMN "c" DROP DEFAULT;`) || !strings.Contains(joined, ` ALTER COLUMN "c" SET DEFAULT 'sad'::`+schemaName+`.mood`) || !strings.Contains(joined, "COMMENT ON TYPE "+qualified(schemaName, "mood")+" IS 'feelings';") || strings.Count(joined, "DROP TYPE ") != 3 {
		t.Fatalf("enum rewrite plan is incomplete: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	var scalar, arrayValue string
	if err := conn.QueryRow(ctx, "SELECT c::text FROM "+qualified(schemaName, "t1")).Scan(&scalar); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, "SELECT c::text FROM "+qualified(schemaName, "t2")).Scan(&arrayValue); err != nil {
		t.Fatal(err)
	}
	if scalar != "sad" || arrayValue != "{happy}" {
		t.Fatalf("enum rewrite changed retained data: scalar=%q array=%q", scalar, arrayValue)
	}
}

func TestEnumRewriteRejectsUnsafeDependentsOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	tests := []struct {
		name, extra, unsupported string
	}{
		{name: "generated", extra: "CREATE TABLE %s.t (c %s.mood, d %s.mood GENERATED ALWAYS AS (c) STORED);", unsupported: "enum_rewrite_generated_column:"},
		{name: "indexed", extra: "CREATE TABLE %s.t (c %s.mood); CREATE INDEX t_c_idx ON %s.t (c);", unsupported: "enum_rewrite_column_dependent:index:"},
		{name: "constrained", extra: "CREATE TABLE %s.t (c %s.mood, CONSTRAINT t_c_not_x CHECK (c <> 'happy'));", unsupported: "enum_rewrite_column_dependent:constraint:"},
		{name: "domain", extra: "CREATE DOMAIN %s.feeling AS %s.mood;", unsupported: "enum_rewrite_dependent:domain:"},
		{name: "view", extra: "CREATE TABLE %s.t (c %s.mood); CREATE VIEW %s.v AS SELECT c FROM %s.t;", unsupported: "enum_rewrite_column_dependent:view:"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemaName := fmt.Sprintf("onwardpg_enum_reject_%d_%d", index, time.Now().UTC().UnixNano())
			defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
			quoted := quote(schemaName)
			arguments := make([]any, strings.Count(test.extra, "%s"))
			for index := range arguments {
				arguments[index] = quoted
			}
			extra := fmt.Sprintf(test.extra, arguments...)
			baseDDL := "CREATE SCHEMA " + quoted + ";"
			currentDDL := baseDDL + "CREATE TYPE " + quoted + ".mood AS ENUM ('sad', 'ok', 'happy');" + extra
			if _, err := conn.Exec(ctx, currentDDL); err != nil {
				t.Fatal(err)
			}
			desiredDDL := baseDDL + "CREATE TYPE " + quoted + ".mood AS ENUM ('sad', 'happy');" + extra
			path := filepath.Join(t.TempDir(), "desired.sql")
			if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
				t.Fatalf("enum rewrite did not request confirmation: %#v", pending)
			}
			question := pending.Questions[0]
			answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: question.Kind, Key: question.Key, Value: "rewrite", QuestionFingerprint: question.ScopeFingerprint}}}
			plan, err := Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
			assertUnsupportedPrefix(t, plan, test.unsupported)
		})
	}
}

func TestEnumValueRenameConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_value_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TYPE " + quote(schemaName) + ".state AS ENUM ('new', 'active', 'archived');" +
		"CREATE TABLE " + quote(schemaName) + ".orders (state " + quote(schemaName) + ".state NOT NULL);" +
		"INSERT INTO " + quote(schemaName) + ".orders VALUES ('new'), ('active'), ('archived');"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TYPE " + quote(schemaName) + ".state AS ENUM ('fresh', 'enabled', 'archived');" +
		"CREATE TABLE " + quote(schemaName) + ".orders (state " + quote(schemaName) + ".state NOT NULL);"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinSQL(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "RENAME VALUE") != 2 || !strings.Contains(joined, "'new' TO 'fresh'") || !strings.Contains(joined, "'active' TO 'enabled'") {
		t.Fatalf("expected two enum label renames, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	var values []string
	rows, err := conn.Query(ctx, "SELECT state::text FROM "+quote(schemaName)+".orders ORDER BY state::text")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if strings.Join(values, ",") != "archived,enabled,fresh" {
		t.Fatalf("renamed enum data = %#v", values)
	}
}

func TestExtensionCommentsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	var extensionName, extensionVersion string
	err = conn.QueryRow(ctx, `
SELECT available.name, available.default_version
FROM pg_available_extensions available
JOIN pg_available_extension_versions version
  ON version.name = available.name AND version.version = available.default_version
WHERE version.relocatable
  AND NOT EXISTS (SELECT 1 FROM pg_extension installed WHERE installed.extname = available.name)
ORDER BY available.name
LIMIT 1`).Scan(&extensionName, &extensionVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Skip("no uninstalled relocatable extension is available")
	}
	if err != nil {
		t.Fatal(err)
	}
	schemaName := "onwardpg_extension_comment_" + time.Now().UTC().Format("20060102150405")
	defer func() {
		_, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS "+quote(extensionName)+" CASCADE")
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE")
	}()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";CREATE EXTENSION " + quote(extensionName) + " WITH SCHEMA " + quote(schemaName) + " VERSION " + literal(extensionVersion) + ";"
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		ddl  string
		want string
	}{
		{name: "custom comment", ddl: baseDDL + "COMMENT ON EXTENSION " + quote(extensionName) + " IS 'custom';", want: "IS 'custom';"},
		{name: "changed comment", ddl: baseDDL + "COMMENT ON EXTENSION " + quote(extensionName) + " IS 'new custom';", want: "IS 'new custom';"},
		{name: "remove custom comment", ddl: baseDDL, want: "IS NULL;"},
		{name: "default comment unchanged", ddl: baseDDL},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.sql")
			if err := os.WriteFile(path, []byte(step.ddl), 0o600); err != nil {
				t.Fatal(err)
			}
			current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Build(current, desired, protocol.Answers{}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("expected extension comment plan: %#v", plan)
			}
			if step.want == "" {
				if len(plan.Statements) != 0 {
					t.Fatalf("default extension comment generated work: %#v", plan.Statements)
				}
			} else if !strings.Contains(joinSQL(plan), step.want) {
				t.Fatalf("extension comment plan missing %q: %#v", step.want, plan.Statements)
			}
			applyPlan(t, ctx, conn, plan)
			assertGraphConverges(t, ctx, url, desired)
		})
	}
}

func TestEnumRenameRequiresAnotherDeploymentWithoutCompatibilityBridgeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_enum_rename_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TYPE "` + schemaName + `".old_state AS ENUM ('open', 'closed');
CREATE TABLE "` + schemaName + `".orders (state "` + schemaName + `".old_state NOT NULL DEFAULT 'open');`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TYPE "` + schemaName + `".new_state AS ENUM ('open', 'closed');
CREATE TABLE "` + schemaName + `".orders (state "` + schemaName + `".new_state NOT NULL DEFAULT 'open');`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_enum" {
		t.Fatalf("expected enum rename question: %#v", pending)
	}
	oldID := (pgschema.Enum{Schema: schemaName, Name: "old_state"}).ObjectID().String()
	newID := (pgschema.Enum{Schema: schemaName, Name: "new_state"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "rename_enum", Key: oldID, Value: newID}},
	}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertUnsupportedPrefix(t, plan, "expand_contract_bridge_required:")
}

func TestCheckNoInheritNotValidConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_check_attrs_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	baseDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer);`
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	notValidDDL := baseDDL + `
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_value_positive CHECK (value > 0) NO INHERIT NOT VALID;`
	validatedDDL := baseDDL + `
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_value_positive CHECK (value > 0) NO INHERIT;`
	for i, ddl := range []string{notValidDDL, validatedDDL} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("check-%d.sql", i))
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected check attribute plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("check attribute transition left residual graph diff: got %s want %s", got, want)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(t.TempDir(), "base.sql")
	if err := os.WriteFile(basePath, []byte(baseDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+basePath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected check-drop confirmation: %#v", pending)
	}
	constraintID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "orders_value_positive"}).ObjectID()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: constraintID.String(), Value: "drop"}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected check-drop plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("check drop left residual graph diff: got %s want %s", got, want)
	}
}

func TestNotValidForeignKeyCreateConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_notvalid_fk_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint PRIMARY KEY);
CREATE TABLE "` + schemaName + `".orders (account_id bigint);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := currentDDL + `
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES "` + schemaName + `".accounts(id) NOT VALID;`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "NOT VALID") {
		t.Fatalf("expected NOT VALID foreign-key plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("NOT VALID foreign key left residual graph diff: got %s want %s", got, want)
	}
}

func TestNotValidConstraintValidationConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_validate_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer);
ALTER TABLE "` + schemaName + `".orders ADD CONSTRAINT orders_positive CHECK (value > 0) NOT VALID;`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (value integer CONSTRAINT orders_positive CHECK (value > 0));`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 1 || !strings.Contains(plan.Statements[0].SQL, "VALIDATE CONSTRAINT") {
		t.Fatalf("expected validation plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after constraint validation\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestConstraintsBecomeNotValidConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_unvalidate_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "teams") + " (id integer PRIMARY KEY);" +
		"CREATE TABLE " + qualified(schemaName, "people") + " (age integer, team_id integer);"
	currentDDL := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_age_check CHECK (age > 0);" +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_team_fkey FOREIGN KEY (team_id) REFERENCES " + qualified(schemaName, "teams") + " (id);"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_age_check CHECK (age > 0) NOT VALID;" +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_team_fkey FOREIGN KEY (team_id) REFERENCES " + qualified(schemaName, "teams") + " (id) NOT VALID;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	joined := joinPlan(plan)
	if plan.Status != protocol.Planned || strings.Count(joined, "DROP CONSTRAINT") != 2 || strings.Count(joined, "NOT VALID") != 2 {
		t.Fatalf("expected check and foreign-key NOT VALID rebuilds, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestExclusionConstraintCreateAndDropConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_exclusion_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	baseDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".bookings (period tstzrange);`
	if _, err := conn.Exec(ctx, baseDDL); err != nil {
		t.Fatal(err)
	}
	withExclusion := baseDDL + `
ALTER TABLE "` + schemaName + `".bookings ADD CONSTRAINT bookings_period_excl EXCLUDE USING gist (period WITH &&);`
	withAdjacency := baseDDL + `
ALTER TABLE "` + schemaName + `".bookings ADD CONSTRAINT bookings_period_excl EXCLUDE USING gist (period WITH -|-);`
	paths := make([]string, 0, 3)
	for i, ddl := range []string{withExclusion, withAdjacency, baseDDL} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("exclusion-%d.sql", i))
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for i, path := range paths {
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		plan := pending
		if i == 2 {
			if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
				t.Fatalf("expected exclusion-drop confirmation: %#v", pending)
			}
			constraintID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "bookings"}).ObjectID(), Name: "bookings_period_excl"}).ObjectID()
			answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: constraintID.String(), Value: "drop"}}}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("expected exclusion plan: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := desired.Fingerprint()
		got, _ := actual.Fingerprint()
		if got != want {
			t.Fatalf("exclusion constraint transition left residual graph diff: got %s want %s", got, want)
		}
	}
}

func TestForeignKeyActionsAndDeferrabilityConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_fk_options_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint PRIMARY KEY);
CREATE TABLE "` + schemaName + `".orders (
  id bigint PRIMARY KEY,
  account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES "` + schemaName + `".accounts(id)
);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint PRIMARY KEY);
CREATE TABLE "` + schemaName + `".orders (
  id bigint PRIMARY KEY,
  account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES "` + schemaName + `".accounts(id)
    MATCH FULL ON DELETE SET NULL ON UPDATE CASCADE DEFERRABLE INITIALLY DEFERRED
);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `DROP CONSTRAINT "orders_account_id_fkey"`) || !strings.Contains(joinPlan(plan), "MATCH FULL") {
		t.Fatalf("expected foreign-key rebuild: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := desired.Fingerprint()
	got, _ := actual.Fingerprint()
	if got != want {
		t.Fatalf("foreign-key options left residual graph diff: got %s want %s", got, want)
	}

	dropDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (id bigint PRIMARY KEY);
CREATE TABLE "` + schemaName + `".orders (id bigint PRIMARY KEY, account_id bigint);`
	dropPath := filepath.Join(t.TempDir(), "drop.sql")
	if err := os.WriteFile(dropPath, []byte(dropDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+dropPath), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 {
		t.Fatalf("expected foreign-key drop confirmation: %#v", pending)
	}
	foreignKeyID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "orders_account_id_fkey"}).ObjectID()
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop", Key: foreignKeyID.String(), Value: "drop"}}}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected foreign-key drop plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ = desired.Fingerprint()
	got, _ = actual.Fingerprint()
	if got != want {
		t.Fatalf("foreign-key drop left residual graph diff: got %s want %s", got, want)
	}
}

func TestConstraintDeferrabilityScenarioMatrixConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_deferrability_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	baseDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + qualified(schemaName, "teams") + " (id integer PRIMARY KEY);" +
		"CREATE TABLE " + qualified(schemaName, "people") + " (email text, team_id integer);"
	notDeferrable := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_email_key UNIQUE (email);" +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_team_fkey FOREIGN KEY (team_id) REFERENCES " + qualified(schemaName, "teams") + " (id);"
	if _, err := conn.Exec(ctx, notDeferrable); err != nil {
		t.Fatal(err)
	}
	deferred := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_email_key UNIQUE (email) DEFERRABLE INITIALLY DEFERRED;" +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_team_fkey FOREIGN KEY (team_id) REFERENCES " + qualified(schemaName, "teams") + " (id) DEFERRABLE INITIALLY DEFERRED;"
	immediate := baseDDL +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_email_key UNIQUE (email) DEFERRABLE INITIALLY DEFERRED;" +
		"ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_team_fkey FOREIGN KEY (team_id) REFERENCES " + qualified(schemaName, "teams") + " (id) DEFERRABLE INITIALLY IMMEDIATE;"
	path := filepath.Join(t.TempDir(), "schema.sql")
	applyDesired := func(label, ddl string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil || plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v err=%v", label, plan, err)
		}
		if label != "unchanged" && (!strings.Contains(joinPlan(plan), "DROP CONSTRAINT") || !strings.Contains(joinPlan(plan), "ADD CONSTRAINT")) {
			t.Fatalf("%s did not rebuild changed deferrability:\n%s", label, joinPlan(plan))
		}
		if label == "unchanged" && len(plan.Statements) != 0 {
			t.Fatalf("unchanged deferrability planned work: %#v", plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
	}
	applyDesired("make deferred", deferred)
	applyDesired("deferred to immediate", immediate)
	applyDesired("make not deferrable", notDeferrable)
	applyDesired("unchanged", notDeferrable)
}

func TestForeignKeyDropOrderingScenarioMatrixConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_fk_drop_order_%d", time.Now().UTC().UnixNano())
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()
	schemaDDL := "CREATE SCHEMA " + quote(schemaName) + ";"
	peopleDDL := "CREATE TABLE " + qualified(schemaName, "people") + " (team_id integer);"
	teamDDL := "CREATE TABLE " + qualified(schemaName, "teams") + " (id integer PRIMARY KEY);"
	fkDDL := "ALTER TABLE " + qualified(schemaName, "people") + " ADD CONSTRAINT people_team_fkey FOREIGN KEY (team_id) REFERENCES " + qualified(schemaName, "teams") + " (id);"
	if _, err := conn.Exec(ctx, schemaDDL+peopleDDL+teamDDL+fkDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	planWithDrops := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		answers := protocol.Answers{ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
		for _, question := range pending.Questions {
			if question.Kind != "drop" {
				t.Fatalf("%s unexpected question: %#v", label, question)
			}
			answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
		}
		plan, err := Build(current, desired, answers, Options{})
		if err != nil || plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v err=%v", label, plan, err)
		}
		return plan, desired
	}

	plan, desired := planWithDrops("drop referenced table", schemaDDL+peopleDDL)
	joined := joinPlan(plan)
	constraintDropAt := strings.Index(joined, "DROP CONSTRAINT")
	teamDropAt := strings.Index(joined, "DROP TABLE "+qualified(schemaName, "teams"))
	if constraintDropAt < 0 || teamDropAt < 0 || constraintDropAt > teamDropAt {
		t.Fatalf("foreign key was not dropped before its referenced table:\n%s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	if _, err := conn.Exec(ctx, teamDDL+fkDDL); err != nil {
		t.Fatal(err)
	}
	plan, desired = planWithDrops("drop both tables", schemaDDL)
	joined = joinPlan(plan)
	constraintAt := strings.Index(joined, "DROP CONSTRAINT")
	peopleAt := strings.Index(joined, "DROP TABLE "+qualified(schemaName, "people"))
	teamsAt := strings.Index(joined, "DROP TABLE "+qualified(schemaName, "teams"))
	if constraintAt < 0 || peopleAt < 0 || teamsAt < 0 || constraintAt > peopleAt || constraintAt > teamsAt {
		t.Fatalf("foreign key must be dropped before both its own and referenced tables:\n%s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestForeignKeyCycleCreateConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_fk_cycle_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".accounts (
  id bigint PRIMARY KEY,
  profile_id bigint
);
CREATE TABLE "` + schemaName + `".profiles (
  id bigint PRIMARY KEY,
  account_id bigint
);
ALTER TABLE "` + schemaName + `".accounts ADD CONSTRAINT accounts_profile_fkey FOREIGN KEY (profile_id) REFERENCES "` + schemaName + `".profiles(id);
ALTER TABLE "` + schemaName + `".profiles ADD CONSTRAINT profiles_account_fkey FOREIGN KEY (account_id) REFERENCES "` + schemaName + `".accounts(id);`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("expected cycle-safe plan: %#v", plan)
	}
	joined := joinPlan(plan)
	if strings.Index(joined, `CREATE TABLE "`+schemaName+`"."profiles"`) > strings.Index(joined, `ADD CONSTRAINT "accounts_profile_fkey"`) || strings.Index(joined, `CREATE TABLE "`+schemaName+`"."accounts"`) > strings.Index(joined, `ADD CONSTRAINT "profiles_account_fkey"`) {
		t.Fatalf("foreign keys were not detached from table creation: %s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after FK cycle create\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestTemporalAndEnforcementConstraintsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 {
		t.Skip("temporal and NOT ENFORCED constraints require PostgreSQL 18")
	}
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_temporal_" + time.Now().UTC().Format("20060102150405")
	var extensionPreexisting bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')").Scan(&extensionPreexisting); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS btree_gist"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE")
		if !extensionPreexisting {
			_, _ = conn.Exec(context.Background(), "DROP EXTENSION IF EXISTS btree_gist")
		}
	}()
	base := `CREATE EXTENSION btree_gist;
CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".room (id integer NOT NULL, code integer NOT NULL, valid_at daterange NOT NULL);
CREATE TABLE "` + schemaName + `".booking (id integer NOT NULL, valid_at daterange NOT NULL);
CREATE TABLE "` + schemaName + `".team (id integer PRIMARY KEY);
CREATE TABLE "` + schemaName + `".person (team_id integer, age integer);
ALTER TABLE "` + schemaName + `".person ADD CONSTRAINT person_age_check CHECK (age > 0);
ALTER TABLE "` + schemaName + `".person ADD CONSTRAINT person_team_fkey FOREIGN KEY (team_id) REFERENCES "` + schemaName + `".team(id);`
	if _, err := conn.Exec(ctx, strings.Replace(base, "CREATE EXTENSION btree_gist;", "", 1)); err != nil {
		t.Fatal(err)
	}
	loadDesired := func(extra string) *pgschema.Snapshot {
		t.Helper()
		path := filepath.Join(t.TempDir(), "desired.sql")
		if err := os.WriteFile(path, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	temporal := `
ALTER TABLE "` + schemaName + `".room ADD CONSTRAINT room_pkey PRIMARY KEY (id, valid_at WITHOUT OVERLAPS);
ALTER TABLE "` + schemaName + `".room ADD CONSTRAINT room_uq UNIQUE (id, valid_at WITHOUT OVERLAPS);
ALTER TABLE "` + schemaName + `".booking ADD CONSTRAINT booking_room_fkey FOREIGN KEY (id, PERIOD valid_at) REFERENCES "` + schemaName + `".room(id, PERIOD valid_at);`
	unenforced := strings.Replace(strings.Replace(base, "CHECK (age > 0);", "CHECK (age > 0) NOT ENFORCED;", 1), "REFERENCES \""+schemaName+"\".team(id);", "REFERENCES \""+schemaName+"\".team(id) NOT ENFORCED;", 1)
	// Replace the two ordinary constraints with their NOT ENFORCED forms while
	// adding all three temporal constraint kinds.
	path := filepath.Join(t.TempDir(), "unenforced.sql")
	if err := os.WriteFile(path, []byte(unenforced+temporal), 0o600); err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	current := loadCurrent()
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("temporal/enforcement add plan=%#v err=%v", plan, err)
	}
	joined := joinPlan(plan)
	if !strings.Contains(joined, "WITHOUT OVERLAPS") || !strings.Contains(joined, "PERIOD valid_at") || !strings.Contains(joined, "NOT ENFORCED") || strings.Index(joined, `ADD CONSTRAINT "room_pkey"`) > strings.Index(joined, `ADD CONSTRAINT "booking_room_fkey"`) {
		t.Fatalf("temporal/enforcement syntax or ordering was lost:\n%s", joined)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
	unchanged, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("temporal/enforcement unchanged plan=%#v err=%v", unchanged, err)
	}

	renamedDDL := strings.Replace(unenforced+temporal, "CONSTRAINT room_uq UNIQUE", "CONSTRAINT room_uq_new UNIQUE", 1)
	path = filepath.Join(t.TempDir(), "renamed.sql")
	if err := os.WriteFile(path, []byte(renamedDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	current = loadCurrent()
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_constraint" {
		t.Fatalf("temporal unique rename must ask: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_constraint", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("temporal unique rename plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	changedTemporal := strings.Replace(temporal, "CONSTRAINT room_uq UNIQUE (id, valid_at", "CONSTRAINT room_uq_new UNIQUE (id, code, valid_at", 1)
	desired = loadDesired(changedTemporal)
	current = loadCurrent()
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("temporal definition/enforcement reversal plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = loadDesired("")
	current = loadCurrent()
	pending, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput {
		t.Fatalf("temporal drops must ask: plan=%#v err=%v", pending, err)
	}
	answers = protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		if question.Kind != "drop" {
			t.Fatalf("unexpected temporal-drop question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("temporal drop plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestSequenceOwnedByTransitionsConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	schemaName := "onwardpg_seq_owned_" + suffix
	identitySchema := "onwardpg_seq_identity_" + suffix
	serialSchema := "onwardpg_seq_serial_" + suffix
	defer func() {
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`)
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+identitySchema+`" CASCADE`)
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+serialSchema+`" CASCADE`)
	}()
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		path := filepath.Join(t.TempDir(), "desired.sql")
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string, check func(string)) *pgschema.Snapshot {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				value := ""
				switch question.Kind {
				case "drop":
					value = "drop"
				case "rename_column":
					value = "create"
				default:
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		if check != nil {
			check(joinPlan(plan))
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return desired
	}

	base := `CREATE SCHEMA "` + schemaName + `";`
	tables := base + ` CREATE TABLE "` + schemaName + `".owner (a bigint, b bigint);`
	ownedA := tables + ` CREATE SEQUENCE "` + schemaName + `".ticket_seq; ALTER SEQUENCE "` + schemaName + `".ticket_seq OWNED BY "` + schemaName + `".owner.a;`
	ownedB := tables + ` CREATE SEQUENCE "` + schemaName + `".ticket_seq; ALTER SEQUENCE "` + schemaName + `".ticket_seq OWNED BY "` + schemaName + `".owner.b;`
	none := tables + ` CREATE SEQUENCE "` + schemaName + `".ticket_seq OWNED BY NONE;`

	created := applyDesired("owned sequence create", ownedA, func(sql string) {
		if !strings.Contains(sql, "CREATE SEQUENCE") || !strings.Contains(sql, `OWNED BY "`+schemaName+`"."owner"."a"`) || strings.Index(sql, "CREATE TABLE") > strings.Index(sql, "OWNED BY") {
			t.Fatalf("owned sequence create order was lost:\n%s", sql)
		}
	})
	unchanged, err := Build(loadCurrent(), created, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged owned sequence plan=%#v err=%v", unchanged, err)
	}
	applyDesired("ownership removed", none, func(sql string) {
		if !strings.Contains(sql, "OWNED BY NONE") {
			t.Fatalf("ownership removal missing:\n%s", sql)
		}
	})
	applyDesired("ownership added", ownedA, func(sql string) {
		if !strings.Contains(sql, `OWNED BY "`+schemaName+`"."owner"."a"`) {
			t.Fatalf("ownership addition missing:\n%s", sql)
		}
	})
	applyDesired("ownership retargeted", ownedB, func(sql string) {
		if !strings.Contains(sql, `OWNED BY "`+schemaName+`"."owner"."b"`) {
			t.Fatalf("ownership retarget missing:\n%s", sql)
		}
	})
	applyDesired("owned sequence dropped", tables, func(sql string) {
		if !strings.Contains(sql, "DROP SEQUENCE") {
			t.Fatalf("owned sequence drop missing:\n%s", sql)
		}
	})
	applyDesired("owned sequence recreated", ownedA, nil)
	columnBOnly := base + ` CREATE TABLE "` + schemaName + `".owner (b bigint);`
	applyDesired("owning column dropped", columnBOnly, nil)
	applyDesired("owned sequence restored after column drop", ownedA, nil)
	applyDesired("owning table dropped", base, nil)
	applyDesired("owned sequence restored before schema drop", ownedA, nil)
	applyDesired("owning schema dropped", "", nil)

	identityDDL := `CREATE SCHEMA "` + identitySchema + `";
CREATE TABLE "` + identitySchema + `".items (id bigint GENERATED BY DEFAULT AS IDENTITY);
CREATE SEQUENCE "` + identitySchema + `".manual_identity_owned;
ALTER SEQUENCE "` + identitySchema + `".manual_identity_owned OWNED BY "` + identitySchema + `".items.id;`
	applyDesired("identity-owned sequence excluded", identityDDL, func(sql string) {
		if strings.Contains(sql, "manual_identity_owned") {
			t.Fatalf("identity-owned sequence leaked into standalone planning:\n%s", sql)
		}
	})
	applyDesired("identity exclusion cleanup", "", nil)

	serialDDL := `CREATE SCHEMA "` + serialSchema + `";
CREATE TABLE "` + serialSchema + `".items (id bigserial);
CREATE SEQUENCE "` + serialSchema + `".manual_serial_owned;
ALTER SEQUENCE "` + serialSchema + `".manual_serial_owned OWNED BY "` + serialSchema + `".items.id;`
	applyDesired("serial backing sequence excluded", serialDDL, func(sql string) {
		if strings.Count(sql, "CREATE SEQUENCE") != 1 || !strings.Contains(sql, "manual_serial_owned") {
			t.Fatalf("serial/manual sequence filtering was not atomic:\n%s", sql)
		}
	})
}

func TestIdentityAddAllOptionsAndConfirmedDropConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := "onwardpg_identity_full_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".items (id bigint NOT NULL DEFAULT 5);`); err != nil {
		t.Fatal(err)
	}
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		path := filepath.Join(t.TempDir(), "desired.sql")
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	desired := loadDesired(`CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".items (
  id bigint GENERATED ALWAYS AS IDENTITY (MINVALUE 2 MAXVALUE 90 START WITH 3 INCREMENT BY 4 CACHE 5 NO CYCLE)
);`)
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned || strings.Index(joinPlan(plan), "DROP DEFAULT") > strings.Index(joinPlan(plan), "ADD GENERATED ALWAYS AS IDENTITY") {
		t.Fatalf("identity-add plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = loadDesired(`CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".items (
  id bigint GENERATED BY DEFAULT AS IDENTITY (MINVALUE 1 MAXVALUE 900 START WITH 30 INCREMENT BY 40 CACHE 50 CYCLE)
);`)
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("identity-options plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = loadDesired(`CREATE SCHEMA "` + schemaName + `"; CREATE TABLE "` + schemaName + `".items (id bigint NOT NULL DEFAULT 9);`)
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "drop_identity" {
		t.Fatalf("identity drop must ask explicit intent: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "drop_identity", Key: pending.Questions[0].Key, Value: "drop", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned || strings.Index(joinPlan(plan), "DROP IDENTITY") > strings.Index(joinPlan(plan), "SET DEFAULT 9") {
		t.Fatalf("identity-drop plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestIdentityAndSerialScenarioMatrixConvergesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	schemaName := fmt.Sprintf("onwardpg_identity_matrix_%d", time.Now().UTC().UnixNano())
	q := quote(schemaName)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	loadCurrent := func() *pgschema.Snapshot {
		t.Helper()
		snapshot, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	applyDesired := func(label, ddl string) (protocol.Result, *pgschema.Snapshot) {
		t.Helper()
		desired := loadDesired(ddl)
		current := loadCurrent()
		plan, err := Build(current, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatalf("%s initial plan: %v", label, err)
		}
		if plan.Status == protocol.NeedsInput {
			answers := protocol.Answers{ProtocolVersion: plan.ProtocolVersion, CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint}
			for _, question := range plan.Questions {
				value := ""
				switch question.Kind {
				case "set_not_null":
					value = "direct"
				case "drop_identity":
					value = "drop"
				default:
					t.Fatalf("%s unexpected question: %#v", label, question)
				}
				answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
			}
			plan, err = Build(current, desired, answers, Options{})
			if err != nil {
				t.Fatalf("%s answered plan: %v", label, err)
			}
		}
		if plan.Status != protocol.Planned {
			t.Fatalf("%s plan=%#v", label, plan)
		}
		applyPlan(t, ctx, conn, plan)
		assertGraphConverges(t, ctx, url, desired)
		return plan, desired
	}

	prefix := "CREATE SCHEMA " + q + ";"
	initial := prefix +
		" CREATE TABLE " + q + ".serials (id serial PRIMARY KEY, small_id smallserial, big_id bigserial, name text);" +
		" CREATE TABLE " + q + ".identities (" +
		"ascending integer GENERATED ALWAYS AS IDENTITY (START WITH 100 INCREMENT BY 5 CACHE 10)," +
		"max_cycle integer GENERATED ALWAYS AS IDENTITY (MAXVALUE 500 CYCLE)," +
		"descending integer GENERATED ALWAYS AS IDENTITY (INCREMENT BY -1 MINVALUE -100));" +
		" CREATE TABLE " + q + ".plain (id integer);"
	created, desired := applyDesired("serial and identity creation", initial)
	createdSQL := joinPlan(created)
	for _, fragment := range []string{"smallserial", "serial", "bigserial", "START WITH 100", "MAXVALUE 500", "INCREMENT BY -1", "MINVALUE -100"} {
		if !strings.Contains(createdSQL, fragment) {
			t.Fatalf("serial/identity creation omitted %q:\n%s", fragment, createdSQL)
		}
	}
	unchanged, err := Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("unchanged serial/identity matrix plan=%#v err=%v", unchanged, err)
	}

	plainIdentity := strings.Replace(initial, ".plain (id integer)", ".plain (id integer GENERATED ALWAYS AS IDENTITY (INCREMENT BY 5))", 1)
	added, _ := applyDesired("identity added to nullable column", plainIdentity)
	if strings.Index(joinPlan(added), "SET NOT NULL") > strings.Index(joinPlan(added), "ADD GENERATED ALWAYS AS IDENTITY") {
		t.Fatalf("identity add did not establish NOT NULL first:\n%s", joinPlan(added))
	}
	droppedIdentity, _ := applyDesired("identity dropped to nullable column", initial)
	if strings.Index(joinPlan(droppedIdentity), "DROP IDENTITY") > strings.Index(joinPlan(droppedIdentity), "DROP NOT NULL") {
		t.Fatalf("identity drop did not remove identity before nullability:\n%s", joinPlan(droppedIdentity))
	}

	serialIdentity := strings.Replace(initial, "id serial PRIMARY KEY", "id integer GENERATED BY DEFAULT AS IDENTITY (INCREMENT BY 5) PRIMARY KEY", 1)
	serialConverted, _ := applyDesired("serial converted to identity with options", serialIdentity)
	if strings.Index(joinPlan(serialConverted), "DROP DEFAULT") > strings.Index(joinPlan(serialConverted), "ADD GENERATED BY DEFAULT AS IDENTITY") || !strings.Contains(joinPlan(serialConverted), "INCREMENT BY 5") {
		t.Fatalf("serial-to-identity ordering/options missing:\n%s", joinPlan(serialConverted))
	}

	mutated := strings.Replace(serialIdentity,
		"ascending integer GENERATED ALWAYS AS IDENTITY (START WITH 100 INCREMENT BY 5 CACHE 10)",
		"ascending integer GENERATED BY DEFAULT AS IDENTITY (START WITH 1 INCREMENT BY 1 MINVALUE -100 MAXVALUE 500 CACHE 1 CYCLE)", 1)
	optionsChanged, _ := applyDesired("identity generation and options changed", mutated)
	optionsSQL := joinPlan(optionsChanged)
	for _, fragment := range []string{"SET GENERATED BY DEFAULT", "SET START WITH 1", "SET INCREMENT BY 1", "SET MINVALUE -100", "SET MAXVALUE 500", "SET CACHE 1", "SET CYCLE"} {
		if !strings.Contains(optionsSQL, fragment) {
			t.Fatalf("identity option mutation omitted %q:\n%s", fragment, optionsSQL)
		}
	}
	backToDefaults := strings.Replace(mutated,
		"ascending integer GENERATED BY DEFAULT AS IDENTITY (START WITH 1 INCREMENT BY 1 MINVALUE -100 MAXVALUE 500 CACHE 1 CYCLE)",
		"ascending integer GENERATED BY DEFAULT AS IDENTITY", 1)
	defaults, desired := applyDesired("identity options restored to defaults", backToDefaults)
	if !strings.Contains(joinPlan(defaults), "SET NO CYCLE") || !strings.Contains(joinPlan(defaults), "SET MINVALUE 1") || !strings.Contains(joinPlan(defaults), "SET MAXVALUE 2147483647") {
		t.Fatalf("identity default restoration missing:\n%s", joinPlan(defaults))
	}
	unchanged, err = Build(loadCurrent(), desired, protocol.Answers{}, Options{})
	if err != nil || unchanged.Status != protocol.Planned || len(unchanged.Statements) != 0 {
		t.Fatalf("default identity options generated residual work: plan=%#v err=%v", unchanged, err)
	}
}

func lockGraphPlanIntegration(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	const integrationLock int64 = 731095702114
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) })
}

func TestRowSecurityPoliciesAndTablePrivilegesConvergeOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	suffix := time.Now().UTC().Format("20060102150405")
	schemaName := "onwardpg_rls_" + suffix
	roleName := `onwardpg_reader_` + suffix
	if _, err := conn.Exec(ctx, "CREATE ROLE "+quote(roleName)); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+quote(roleName)) }()
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quote(schemaName)+" CASCADE") }()

	loadDesired := func(ddl string) *pgschema.Snapshot {
		t.Helper()
		path := filepath.Join(t.TempDir(), "schema.sql")
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, amount bigint NOT NULL);`
	desired := loadDesired(base + `
CREATE POLICY tenant_access ON "` + schemaName + `".orders AS PERMISSIVE FOR ALL TO PUBLIC, "` + roleName + `" USING (tenant_id > 0);
ALTER TABLE "` + schemaName + `".orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE "` + schemaName + `".orders FORCE ROW LEVEL SECURITY;
GRANT SELECT ON TABLE "` + schemaName + `".orders TO "` + roleName + `" WITH GRANT OPTION;
GRANT INSERT ON TABLE "` + schemaName + `".orders TO PUBLIC;`)
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("initial authorization plan=%#v err=%v", plan, err)
	}
	initialSQL := joinPlan(plan)
	if strings.Index(initialSQL, "CREATE POLICY") > strings.Index(initialSQL, "ENABLE ROW LEVEL SECURITY") || strings.Index(initialSQL, "ENABLE ROW LEVEL SECURITY") > strings.Index(initialSQL, "FORCE ROW LEVEL SECURITY") {
		t.Fatalf("unsafe initial RLS order:\n%s", initialSQL)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = loadDesired(base + `
CREATE POLICY tenant_access ON "` + schemaName + `".orders AS PERMISSIVE FOR ALL TO "` + roleName + `" USING (tenant_id > 10) WITH CHECK (amount >= 0);
ALTER TABLE "` + schemaName + `".orders ENABLE ROW LEVEL SECURITY;
GRANT SELECT ON TABLE "` + schemaName + `".orders TO "` + roleName + `";`)
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput {
		t.Fatalf("authorization contraction must ask: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	answerValues := map[string]string{"alter_policy": "alter", "relax_row_security": "relax", "revoke_grant_option": "revoke", "drop": "drop"}
	for _, question := range pending.Questions {
		value, exists := answerValues[question.Kind]
		if !exists {
			t.Fatalf("unexpected authorization question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("answered authorization plan=%#v err=%v", plan, err)
	}
	for _, fragment := range []string{"ALTER POLICY", "NO FORCE ROW LEVEL SECURITY", "REVOKE GRANT OPTION FOR SELECT", "REVOKE INSERT"} {
		if !strings.Contains(joinPlan(plan), fragment) {
			t.Fatalf("authorization plan missing %q:\n%s", fragment, joinPlan(plan))
		}
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = loadDesired(base)
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || pending.Status != protocol.NeedsInput {
		t.Fatalf("RLS removal must ask: plan=%#v err=%v", pending, err)
	}
	answers = protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		if question.Kind != "drop" {
			t.Fatalf("unexpected RLS-removal question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: "drop", QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err = Build(current, desired, answers, Options{})
	if err != nil || plan.Status != protocol.Planned {
		t.Fatalf("RLS-removal plan=%#v err=%v", plan, err)
	}
	removalSQL := joinPlan(plan)
	if !strings.Contains(removalSQL, "DISABLE ROW LEVEL SECURITY") || !strings.Contains(removalSQL, "DROP POLICY") || strings.Index(removalSQL, "DISABLE ROW LEVEL SECURITY") > strings.Index(removalSQL, "DROP POLICY") {
		t.Fatalf("RLS must be disabled before policy removal:\n%s", removalSQL)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func applyPlan(t *testing.T, ctx context.Context, conn *pgx.Conn, plan protocol.Result) {
	t.Helper()
	for _, batch := range plan.Batches {
		if !batch.Transactional {
			for _, statement := range batch.Statements {
				if _, err := conn.Exec(ctx, statement.SQL); err != nil {
					t.Fatalf("apply %q: %v", statement.SQL, err)
				}
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range batch.Statements {
			if _, err := tx.Exec(ctx, statement.SQL); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("apply %q: %v", statement.SQL, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func applyPlanPhase(t *testing.T, ctx context.Context, conn *pgx.Conn, plan protocol.Result, phase string) {
	t.Helper()
	filtered := plan
	filtered.Batches = nil
	for _, batch := range plan.Batches {
		if batch.Phase == phase {
			filtered.Batches = append(filtered.Batches, batch)
		}
	}
	applyPlan(t, ctx, conn, filtered)
}

func assertGraphConverges(t *testing.T, ctx context.Context, url string, desired *pgschema.Snapshot) {
	t.Helper()
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, err := desired.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	actualFingerprint, err := actual.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after applying plan\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func assertUnsupportedPrefix(t *testing.T, result protocol.Result, prefix string) {
	t.Helper()
	if result.Status != protocol.Unsupported {
		t.Fatalf("expected unsupported result with %q, got %#v", prefix, result)
	}
	for _, reason := range result.Unsupported {
		if strings.HasPrefix(reason, prefix) {
			return
		}
	}
	t.Fatalf("expected unsupported reason with %q, got %#v", prefix, result.Unsupported)
}

func assertRenameCompatibilityBridge(t *testing.T, result protocol.Result) {
	t.Helper()
	if result.Status != protocol.NeedsInput || len(result.Questions) != 1 || result.Questions[0].Kind != "rename_compatibility_bridge" {
		t.Fatalf("expected editable rename compatibility bridge, got %#v", result)
	}
}

func joinPlan(plan protocol.Result) string {
	var statements []string
	for _, statement := range plan.Statements {
		statements = append(statements, statement.SQL)
	}
	return strings.Join(statements, "\n")
}
