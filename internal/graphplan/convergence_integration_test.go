package graphplan

import (
	"context"
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER VIEW") {
		t.Fatalf("expected view-rename plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

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
	if plan.Status != protocol.Planned || len(plan.Batches) != 2 || plan.Batches[1].Phase != "migrate" || !strings.Contains(joinSQL(plan), "REFRESH MATERIALIZED VIEW") {
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

func TestDependentViewRenameConvergesOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER VIEW") || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE VIEW") {
		t.Fatalf("expected dependent view transition, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
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
	if plan.Status != protocol.Planned || len(plan.Batches) != 1 || plan.Batches[0].Phase != "migrate" {
		t.Fatalf("expected manual plan, got %#v", plan)
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
	if plan.Status != protocol.Planned || len(plan.Batches) != 1 || plan.Batches[0].Phase != "migrate" {
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

func TestRoutineRenameConvergesOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER FUNCTION") {
		t.Fatalf("expected routine rename plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestRoutineRenameRetainsMaterializedViewDependentOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER FUNCTION") || strings.Contains(joinSQL(plan), "DROP MATERIALIZED VIEW") || strings.Contains(joinSQL(plan), "CREATE MATERIALIZED VIEW") {
		t.Fatalf("expected native materialized dependent retention, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	var value int
	if err := conn.QueryRow(ctx, "SELECT value FROM "+quote(schemaName)+".doubled_cache").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 2 {
		t.Fatalf("retained materialized view changed stored data: %d", value)
	}
	assertGraphConverges(t, ctx, url, desired)
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE FUNCTION") || !strings.Contains(joinSQL(plan), "REFRESH MATERIALIZED VIEW") || len(plan.Batches) != 2 || plan.Batches[1].Phase != "migrate" {
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

func TestRoutineRenameWithDependentTriggerConvergesOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER FUNCTION") || !strings.Contains(joinSQL(plan), "CREATE OR REPLACE TRIGGER") {
		t.Fatalf("expected routine/trigger transition, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
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
		"CREATE TRIGGER audit_old BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
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
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_trigger" {
		t.Fatalf("expected trigger rename question, got %#v", pending)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "rename_trigger", Key: pending.Questions[0].Key, Value: pending.Questions[0].Choices[0]}}}
	plan, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER TRIGGER") {
		t.Fatalf("expected trigger rename plan, got %#v", plan)
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

func TestMaterializedViewRenameConvergesOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || !strings.Contains(joinSQL(plan), "ALTER MATERIALIZED VIEW") || !strings.Contains(joinSQL(plan), "COMMENT ON INDEX") || strings.Contains(joinSQL(plan), "DROP INDEX") || strings.Contains(joinSQL(plan), "CREATE INDEX") {
		t.Fatalf("expected materialized-view rename plan, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
}

func TestMaterializedViewRenameRetainsMaterializedDependentOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || strings.Count(joinSQL(plan), "ALTER MATERIALIZED VIEW") != 1 || strings.Contains(joinSQL(plan), "DROP MATERIALIZED VIEW") || strings.Contains(joinSQL(plan), "CREATE MATERIALIZED VIEW") {
		t.Fatalf("expected native retained-dependent materialized-view rename, got %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
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
	if plan.Status != protocol.Planned {
		t.Fatalf("expected extension-move plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("extension schema move did not converge: got %s want %s", actualFingerprint, desiredFingerprint)
	}
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
	currentDDL := "CREATE SCHEMA " + quote(schemaName) + "; CREATE SEQUENCE " + quote(schemaName) + ".orders_seq AS bigint START WITH 1 INCREMENT BY 1 MINVALUE 1 MAXVALUE 100 CACHE 1 NO CYCLE;"
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + "; CREATE SEQUENCE " + quote(schemaName) + ".orders_seq AS bigint START WITH 10 INCREMENT BY 5 MINVALUE 1 MAXVALUE 100 CACHE 4 CYCLE;"
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
	if plan.Status != protocol.Planned {
		t.Fatalf("expected sequence-update plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("sequence update did not converge: got %s want %s", actualFingerprint, desiredFingerprint)
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
  age integer,
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
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 2 {
		t.Fatalf("expected two mutation questions: %#v", pending)
	}
	ageID := (pgschema.Column{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "age"}).ObjectID().String()
	idID := (pgschema.Column{Table: (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID(), Name: "id"}).ObjectID().String()
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			{Kind: "type_change", Key: ageID, Value: "age::integer"},
			{Kind: "set_not_null", Key: idID, Value: "staged"},
		},
	}
	plan, err := Build(current, desired, answers, Options{})
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
		t.Fatalf("residual graph diff after mutation plan\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}

	dropNotNullDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TYPE "` + schemaName + `".state AS ENUM ('open', 'pending', 'closed');
CREATE TABLE "` + schemaName + `".orders (
  id bigint,
  age integer,
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
	actual, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ = desired.Fingerprint()
	actualFingerprint, _ = actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		t.Fatalf("residual graph diff after DROP NOT NULL: got %s want %s", actualFingerprint, desiredFingerprint)
	}
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
CREATE TABLE "` + schemaName + `".orders (id integer);
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
	base := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint);
CREATE INDEX orders_id_idx ON "` + schemaName + `".orders (id);`
	if _, err := conn.Exec(ctx, base); err != nil {
		t.Fatal(err)
	}
	withOne := base + `
COMMENT ON SCHEMA "` + schemaName + `" IS 'schema one';
COMMENT ON TABLE "` + schemaName + `".orders IS 'table one';
COMMENT ON COLUMN "` + schemaName + `".orders.id IS 'column one';
COMMENT ON INDEX "` + schemaName + `".orders_id_idx IS 'index one';`
	withTwo := base + `
COMMENT ON SCHEMA "` + schemaName + `" IS 'schema two';
COMMENT ON TABLE "` + schemaName + `".orders IS 'table two';
COMMENT ON COLUMN "` + schemaName + `".orders.id IS 'column two';
COMMENT ON INDEX "` + schemaName + `".orders_id_idx IS 'index two';`
	for i, ddl := range []string{withOne, withTwo, base} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("comments-%d.sql", i))
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
			t.Fatalf("expected comment transition plan: %#v", plan)
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
COMMENT ON INDEX "` + schemaName + `".orders_value_idx IS 'old lookup';`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders (id bigint, value integer CONSTRAINT value_positive CHECK (value >= 0));
CREATE INDEX orders_value_idx ON "` + schemaName + `".orders (value) WHERE value >= 0;
COMMENT ON INDEX "` + schemaName + `".orders_value_idx IS 'new lookup';`
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
CREATE INDEX orders_new_idx ON "` + schemaName + `".orders (id);
COMMENT ON INDEX "` + schemaName + `".orders_new_idx IS 'new';`
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
	if plan.Status != protocol.Planned {
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
CREATE TABLE "` + schemaName + `".keep (id bigint, old_column text);
CREATE INDEX keep_old_column_idx ON "` + schemaName + `".keep (old_column);
CREATE TABLE "` + schemaName + `".remove_me (id bigint);`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".keep (id bigint);`
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
CREATE TABLE "` + schemaName + `".orders_old (id bigint, name text);
CREATE INDEX stable_orders_idx ON "` + schemaName + `".orders_old (name);
CREATE FUNCTION "` + schemaName + `".audit_orders() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER audit_orders BEFORE INSERT ON "` + schemaName + `".orders_old FOR EACH ROW EXECUTE FUNCTION "` + schemaName + `".audit_orders();
CREATE VIEW "` + schemaName + `".order_names AS SELECT name FROM "` + schemaName + `".orders_old;
CREATE MATERIALIZED VIEW "` + schemaName + `".order_names_cache AS SELECT name FROM "` + schemaName + `".orders_old;`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
		t.Fatal(err)
	}
	desiredDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".orders_new (id bigint, name text);
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
	if plan.Status != protocol.Planned || len(plan.Statements) != 1 || !strings.Contains(joinSQL(plan), "ALTER TABLE") {
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
		t.Fatalf("residual graph diff after table rename\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
}

func TestColumnRenameConvergesOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned || len(plan.Statements) != 1 || !strings.Contains(joinSQL(plan), "RENAME COLUMN") {
		t.Fatalf("expected native column/trigger rename plan: %#v", plan)
	}
	applyPlan(t, ctx, conn, plan)
	var retained string
	if err := conn.QueryRow(ctx, `SELECT new_name FROM "`+schemaName+`".orders`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != "kept" {
		t.Fatalf("renamed column lost data: %q", retained)
	}
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, _ := desired.Fingerprint()
	actualFingerprint, _ := actual.Fingerprint()
	if desiredFingerprint != actualFingerprint {
		desiredJSON, _ := desired.CanonicalJSON()
		actualJSON, _ := actual.CanonicalJSON()
		t.Fatalf("residual graph diff after column rename\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
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

func TestEnumRenameConvergesOnPostgreSQL(t *testing.T) {
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
	if plan.Status != protocol.Planned {
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
		t.Fatalf("residual graph diff after enum rename\ndesired: %s\nactual:  %s", desiredJSON, actualJSON)
	}
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
	schemaName := "onwardpg_seq_owned_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()
	currentDDL := `CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".old_owner (id bigint);
CREATE TABLE "` + schemaName + `".new_owner (id bigint);
CREATE SEQUENCE "` + schemaName + `".ticket_seq AS bigint START WITH 10 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 3 NO CYCLE;
ALTER SEQUENCE "` + schemaName + `".ticket_seq OWNED BY "` + schemaName + `".old_owner.id;`
	if _, err := conn.Exec(ctx, currentDDL); err != nil {
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
CREATE TABLE "` + schemaName + `".old_owner (id bigint);
CREATE TABLE "` + schemaName + `".new_owner (id bigint);
CREATE SEQUENCE "` + schemaName + `".ticket_seq AS bigint START WITH 10 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 3 NO CYCLE;
ALTER SEQUENCE "` + schemaName + `".ticket_seq OWNED BY "` + schemaName + `".new_owner.id;`)
	current, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), `OWNED BY "`+schemaName+`"."new_owner"."id"`) {
		t.Fatalf("owned-by move plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)

	desired = loadDesired(`CREATE SCHEMA "` + schemaName + `";
CREATE TABLE "` + schemaName + `".old_owner (id bigint);
CREATE TABLE "` + schemaName + `".new_owner (id bigint);
CREATE SEQUENCE "` + schemaName + `".ticket_seq AS bigint START WITH 10 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 3 NO CYCLE OWNED BY NONE;`)
	current, err = source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = Build(current, desired, protocol.Answers{}, Options{})
	if err != nil || plan.Status != protocol.Planned || !strings.Contains(joinPlan(plan), "OWNED BY NONE") {
		t.Fatalf("owned-by-none plan=%#v err=%v", plan, err)
	}
	applyPlan(t, ctx, conn, plan)
	assertGraphConverges(t, ctx, url, desired)
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

func joinPlan(plan protocol.Result) string {
	var statements []string
	for _, statement := range plan.Statements {
		statements = append(statements, statement.SQL)
	}
	return strings.Join(statements, "\n")
}
