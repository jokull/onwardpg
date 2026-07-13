package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

// This is intentionally opt-in: it verifies catalog extraction against a real
// PostgreSQL server without making the regular unit suite require Docker.
func TestLoadGraphForeignKeyIntegration(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_graph_" + time.Now().UTC().Format("20060102150405")
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quote(schemaName)); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	ddl := "CREATE TABLE " + quote(schemaName) + ".accounts (id bigint PRIMARY KEY);" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, account_id bigint NOT NULL REFERENCES " + quote(schemaName) + ".accounts(id) DEFERRABLE INITIALLY DEFERRED);"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}

	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	orders := pgschema.Table{Schema: schemaName, Name: "orders"}.ObjectID()
	accounts := pgschema.Table{Schema: schemaName, Name: "accounts"}.ObjectID()
	foreignKey := pgschema.Constraint{Table: orders, Name: "orders_account_id_fkey"}.ObjectID()
	object, ok := snapshot.Object(foreignKey)
	if !ok {
		t.Fatalf("foreign key %s missing from graph", foreignKey)
	}
	constraint, ok := object.(pgschema.Constraint)
	if !ok || constraint.Reference == nil || *constraint.Reference != accounts || !constraint.Deferrable || !constraint.Deferred {
		t.Fatalf("unexpected typed foreign key: %#v", object)
	}
	dependencies := snapshot.Dependencies(foreignKey)
	primaryKey := pgschema.Constraint{Table: accounts, Name: "accounts_pkey"}.ObjectID()
	accountID := pgschema.Column{Table: orders, Name: "account_id"}.ObjectID()
	if len(dependencies) != 4 || dependencies[0] != accountID || dependencies[1] != primaryKey || dependencies[2] != accounts || dependencies[3] != orders {
		t.Fatalf("foreign key dependencies = %#v", dependencies)
	}
}

func TestLoadGraphNormalizesEquivalentTimestampDefaults(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_default_equiv_" + time.Now().UTC().Format("20060102150405")
	liveDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (created_at timestamptz DEFAULT now());"
	if _, err := conn.Exec(ctx, liveDDL); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	desiredDDL := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (created_at timestamptz DEFAULT CURRENT_TIMESTAMP);"
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	declarative, err := LoadGraph(ctx, Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	liveFingerprint, _ := live.Fingerprint()
	declarativeFingerprint, _ := declarative.Fingerprint()
	if liveFingerprint != declarativeFingerprint {
		t.Fatalf("equivalent timestamp defaults differ: %s != %s", liveFingerprint, declarativeFingerprint)
	}
}

func TestLoadGraphDDLEquivalentToLiveDatabase(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_equiv_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TYPE " + quote(schemaName) + ".state AS ENUM ('open', 'closed');" +
		"CREATE TABLE " + quote(schemaName) + ".accounts (id bigint PRIMARY KEY);" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, account_id bigint REFERENCES " + quote(schemaName) + ".accounts(id), state " + quote(schemaName) + ".state NOT NULL DEFAULT 'open');" +
		"CREATE INDEX orders_state_idx ON " + quote(schemaName) + ".orders (state);"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}

	live, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	declarative, err := LoadGraph(ctx, Parse("file://"+path), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	liveFingerprint, err := live.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	declarativeFingerprint, err := declarative.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if liveFingerprint != declarativeFingerprint {
		liveJSON, _ := live.CanonicalJSON()
		declarativeJSON, _ := declarative.CanonicalJSON()
		t.Fatalf("source graphs differ\nlive: %s\ndeclarative: %s", liveJSON, declarativeJSON)
	}
}

func TestLoadGraphPreservesAtlasCoreCatalogSemantics(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_core_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"COMMENT ON SCHEMA " + quote(schemaName) + " IS 'application schema';" +
		"CREATE SEQUENCE " + quote(schemaName) + ".account_number_seq AS bigint START WITH 10 INCREMENT BY 3 MINVALUE 1 MAXVALUE 999 CACHE 5 CYCLE;" +
		"CREATE TABLE " + quote(schemaName) + ".accounts (" +
		"id bigint GENERATED ALWAYS AS IDENTITY (START WITH 10 INCREMENT BY 5)," +
		"legacy_id serial," +
		"account_number bigint DEFAULT nextval('" + schemaName + ".account_number_seq'::regclass)," +
		"name text COLLATE \"C\", " +
		"lower_name text GENERATED ALWAYS AS (lower(name)) STORED" +
		") PARTITION BY HASH (id);" +
		"COMMENT ON TABLE " + quote(schemaName) + ".accounts IS 'customer accounts';" +
		"COMMENT ON COLUMN " + quote(schemaName) + ".accounts.name IS 'display name';" +
		"CREATE INDEX accounts_name_idx ON " + quote(schemaName) + ".accounts USING btree (name DESC NULLS LAST) INCLUDE (id) WHERE name IS NOT NULL;" +
		"COMMENT ON INDEX " + quote(schemaName) + ".accounts_name_idx IS 'name lookup';"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()

	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := snapshot.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("core catalog state was classified unsupported: %#v", unsupported)
	}
	sequenceID := (pgschema.Sequence{Schema: schemaName, Name: "account_number_seq"}).ObjectID()
	sequenceObject, ok := snapshot.Object(sequenceID)
	if !ok {
		t.Fatal("standalone sequence missing")
	}
	sequence := sequenceObject.(pgschema.Sequence)
	if sequence.Type != "bigint" || sequence.Start != 10 || sequence.Increment != 3 || sequence.Min != 1 || sequence.Max != 999 || sequence.Cache != 5 || !sequence.Cycle {
		t.Fatalf("sequence semantics missing: %#v", sequence)
	}
	schemaObject, ok := snapshot.Object((pgschema.Schema{Name: schemaName}).ObjectID())
	if !ok || schemaObject.(pgschema.Schema).Comment == nil || *schemaObject.(pgschema.Schema).Comment != "application schema" {
		t.Fatalf("schema comment missing: %#v", schemaObject)
	}
	tableID := (pgschema.Table{Schema: schemaName, Name: "accounts"}).ObjectID()
	tableObject, ok := snapshot.Object(tableID)
	if !ok {
		t.Fatal("partitioned table missing")
	}
	table := tableObject.(pgschema.Table)
	if table.Comment == nil || *table.Comment != "customer accounts" || table.Partition == nil || table.Partition.Strategy != "HASH" || table.Partition.Raw != "HASH (id)" {
		t.Fatalf("table semantics missing: %#v", table)
	}
	if dependencies := snapshot.Dependencies(tableID); len(dependencies) < 2 || dependencies[0] != sequenceID && dependencies[1] != sequenceID {
		t.Fatalf("table does not depend on default sequence: %#v", dependencies)
	}
	idObject, _ := snapshot.Object((pgschema.Column{Table: tableID, Name: "id"}).ObjectID())
	id := idObject.(pgschema.Column)
	if id.Identity == nil || id.Identity.Generation != "ALWAYS" || id.Identity.Start != 10 || id.Identity.Increment != 5 {
		t.Fatalf("identity semantics missing: %#v", id)
	}
	serialObject, _ := snapshot.Object((pgschema.Column{Table: tableID, Name: "legacy_id"}).ObjectID())
	serial := serialObject.(pgschema.Column)
	if serial.Serial == nil || serial.Serial.Type != "serial" || serial.Default != nil {
		t.Fatalf("serial semantics missing: %#v", serial)
	}
	nameObject, _ := snapshot.Object((pgschema.Column{Table: tableID, Name: "name"}).ObjectID())
	name := nameObject.(pgschema.Column)
	if name.Comment == nil || *name.Comment != "display name" || name.Collation == "" {
		t.Fatalf("column semantics missing: %#v", name)
	}
	generatedObject, _ := snapshot.Object((pgschema.Column{Table: tableID, Name: "lower_name"}).ObjectID())
	generated := generatedObject.(pgschema.Column)
	if generated.Generated == nil || generated.Generated.Expression == "" {
		t.Fatalf("generated expression missing: %#v", generated)
	}
	indexObject, _ := snapshot.Object((pgschema.Index{Table: tableID, Name: "accounts_name_idx"}).ObjectID())
	index := indexObject.(pgschema.Index)
	if index.Comment == nil || *index.Comment != "name lookup" || index.Predicate == "" || len(index.Parts) != 1 || !index.Parts[0].Descending || len(index.Include) != 1 || index.Include[0] != "id" {
		t.Fatalf("index semantics missing: %#v", index)
	}
	indexDependencies := snapshot.Dependencies(index.ObjectID())
	nameColumn := (pgschema.Column{Table: tableID, Name: "name"}).ObjectID()
	idColumn := (pgschema.Column{Table: tableID, Name: "id"}).ObjectID()
	if !containsID(indexDependencies, nameColumn) || !containsID(indexDependencies, idColumn) {
		t.Fatalf("index column dependencies missing: %#v", indexDependencies)
	}
}

func containsID(ids []pgschema.ID, target pgschema.ID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestLoadGraphReportsNarrowlyIgnoredCatalogState(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_ignore_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (tenant_id bigint);" +
		"ALTER TABLE " + quote(schemaName) + ".orders ENABLE ROW LEVEL SECURITY;" +
		"CREATE POLICY tenant ON " + quote(schemaName) + ".orders USING (tenant_id > 0);"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	policy := "policy:" + schemaName + ".orders.tenant"
	rls := "row_level_security:" + schemaName + ".orders"
	snapshot, err := LoadGraph(ctx, Parse(url), "", []string{policy, rls})
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := snapshot.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("ignored state remains unsupported: %#v", unsupported)
	}
	ignored := snapshot.Ignored()
	if len(ignored) != 2 || ignored[0] != policy || ignored[1] != rls {
		t.Fatalf("ignored = %#v", ignored)
	}
}

func TestLoadGraphCapturesRoutineAndTriggerDependencies(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_trigger_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint);" +
		"CREATE FUNCTION " + quote(schemaName) + ".audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;" +
		"CREATE TRIGGER audit BEFORE INSERT ON " + quote(schemaName) + ".orders FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".audit();" +
		"CREATE FUNCTION " + quote(schemaName) + ".double_value(v integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT v * 2 $$;" +
		"CREATE VIEW " + quote(schemaName) + ".doubled AS SELECT " + quote(schemaName) + ".double_value(1) AS value;" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".doubled_cache AS SELECT " + quote(schemaName) + ".double_value(1) AS value;"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	table := (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID()
	routine := (pgschema.Routine{Schema: schemaName, Name: "audit", Signature: ""}).ObjectID()
	trigger := (pgschema.Trigger{Table: table, Name: "audit"}).ObjectID()
	routineObject, exists := snapshot.Object(routine)
	if !exists {
		t.Fatalf("routine %s missing from graph", routine)
	}
	if value, ok := routineObject.(pgschema.Routine); !ok || value.Kind != "function" || value.Definition == "" {
		t.Fatalf("routine catalog semantics missing: %#v", routineObject)
	}
	triggerObject, exists := snapshot.Object(trigger)
	if !exists {
		t.Fatalf("trigger %s missing from graph", trigger)
	}
	value, ok := triggerObject.(pgschema.Trigger)
	if !ok || value.Routine != routine || value.Definition == "" || value.Enabled != "O" {
		t.Fatalf("trigger catalog semantics missing: %#v", triggerObject)
	}
	dependencies := snapshot.Dependencies(trigger)
	if !containsID(dependencies, table) || !containsID(dependencies, routine) {
		t.Fatalf("trigger dependencies = %#v", dependencies)
	}
	valueRoutine := (pgschema.Routine{Schema: schemaName, Name: "double_value", Signature: "v integer"}).ObjectID()
	for _, view := range []pgschema.ID{
		(pgschema.View{Schema: schemaName, Name: "doubled"}).ObjectID(),
		(pgschema.View{Schema: schemaName, Name: "doubled_cache", Materialized: true}).ObjectID(),
	} {
		if !containsID(snapshot.Dependencies(view), valueRoutine) {
			t.Fatalf("view routine dependencies for %s = %#v, want %s", view, snapshot.Dependencies(view), valueRoutine)
		}
	}
}

func TestLoadGraphCapturesViewsAndTypedDependencies(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_view_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TYPE " + quote(schemaName) + ".state AS ENUM ('open', 'closed');" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY, amount bigint NOT NULL);" +
		"CREATE VIEW " + quote(schemaName) + ".order_amounts WITH (security_barrier=true) AS SELECT id, amount FROM " + quote(schemaName) + ".orders;" +
		"CREATE VIEW " + quote(schemaName) + ".states AS SELECT 'open'::" + quote(schemaName) + ".state AS state;" +
		"COMMENT ON VIEW " + quote(schemaName) + ".order_amounts IS 'public order totals';"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()

	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	viewID := (pgschema.View{Schema: schemaName, Name: "order_amounts"}).ObjectID()
	object, exists := snapshot.Object(viewID)
	if !exists {
		t.Fatalf("view %s missing from graph", viewID)
	}
	view, ok := object.(pgschema.View)
	if !ok || view.Materialized || view.Definition == "" || view.Comment == nil || *view.Comment != "public order totals" || len(view.Options) != 1 || view.Options[0] != (pgschema.Option{Name: "security_barrier", Value: "true"}) {
		t.Fatalf("view catalog semantics missing: %#v", object)
	}
	orders := (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID()
	amount := (pgschema.Column{Table: orders, Name: "amount"}).ObjectID()
	if !containsID(snapshot.Dependencies(viewID), amount) {
		t.Fatalf("view dependencies = %#v, want column %s", snapshot.Dependencies(viewID), amount)
	}
	stateView := (pgschema.View{Schema: schemaName, Name: "states"}).ObjectID()
	state := (pgschema.Enum{Schema: schemaName, Name: "state"}).ObjectID()
	if !containsID(snapshot.Dependencies(stateView), state) {
		t.Fatalf("enum view dependencies = %#v, want enum %s", snapshot.Dependencies(stateView), state)
	}
}

func TestLoadGraphCapturesMaterializedViewIndexes(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_matview_index_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".orders (id bigint PRIMARY KEY);" +
		"CREATE MATERIALIZED VIEW " + quote(schemaName) + ".order_ids AS SELECT id FROM " + quote(schemaName) + ".orders;" +
		"CREATE UNIQUE INDEX order_ids_id_idx ON " + quote(schemaName) + ".order_ids (id);"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view := (pgschema.View{Schema: schemaName, Name: "order_ids", Materialized: true}).ObjectID()
	indexID := (pgschema.Index{Table: view, Name: "order_ids_id_idx"}).ObjectID()
	object, exists := snapshot.Object(indexID)
	if !exists {
		t.Fatalf("materialized-view index %s missing from graph", indexID)
	}
	index, ok := object.(pgschema.Index)
	if !ok || index.Table != view || !index.Unique || len(index.Parts) != 1 || index.Parts[0].Column != "id" {
		t.Fatalf("materialized-view index semantics missing: %#v", object)
	}
	if !containsID(snapshot.Dependencies(indexID), view) {
		t.Fatalf("materialized-view index dependencies = %#v", snapshot.Dependencies(indexID))
	}
}

func TestLoadGraphModelsPartitionedIndexPropagation(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_partition_index_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL) PARTITION BY RANGE (id);" +
		"CREATE TABLE " + quote(schemaName) + ".events_1 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM (0) TO (100);" +
		"CREATE INDEX events_id_idx ON " + quote(schemaName) + ".events (id);"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := (pgschema.Index{Table: (pgschema.Table{Schema: schemaName, Name: "events"}).ObjectID(), Name: "events_id_idx"}).ObjectID()
	var child pgschema.Index
	for _, object := range snapshot.Objects() {
		index, ok := object.(pgschema.Index)
		if ok && index.Table == (pgschema.Table{Schema: schemaName, Name: "events_1"}).ObjectID() {
			child = index
			break
		}
	}
	if child.Parent == nil || *child.Parent != parent || !containsID(snapshot.Dependencies(child.ObjectID()), parent) {
		t.Fatalf("partitioned index propagation missing typed parent edge: child=%#v deps=%#v", child, snapshot.Dependencies(child.ObjectID()))
	}
}

func TestLoadGraphModelsPartitionedConstraintPropagation(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_partition_constraint_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint NOT NULL, occurred_at date NOT NULL, PRIMARY KEY (id, occurred_at)) PARTITION BY RANGE (occurred_at);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "events"}).ObjectID(), Name: "events_pkey"}).ObjectID()
	var child pgschema.Constraint
	for _, object := range snapshot.Objects() {
		constraint, ok := object.(pgschema.Constraint)
		if ok && constraint.Table == (pgschema.Table{Schema: schemaName, Name: "events_2026"}).ObjectID() && constraint.Type == pgschema.ConstraintPrimary {
			child = constraint
			break
		}
	}
	if child.Parent == nil || *child.Parent != parent || !containsID(snapshot.Dependencies(child.ObjectID()), parent) {
		t.Fatalf("partitioned constraint propagation missing typed parent edge: child=%#v deps=%#v", child, snapshot.Dependencies(child.ObjectID()))
	}
}

func TestLoadGraphModelsInheritedPartitionCheckConstraint(t *testing.T) {
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
	lockIntegrationDatabase(t, ctx, conn)
	schemaName := "onwardpg_partition_check_" + time.Now().UTC().Format("20060102150405")
	ddl := "CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".events (id bigint, kind text) PARTITION BY RANGE (id);" +
		"CREATE TABLE " + quote(schemaName) + ".events_2026 PARTITION OF " + quote(schemaName) + ".events FOR VALUES FROM (0) TO (100);" +
		"ALTER TABLE " + quote(schemaName) + ".events ADD CONSTRAINT events_kind_check CHECK (kind <> 'bad');"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "events"}).ObjectID(), Name: "events_kind_check"}).ObjectID()
	childID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schemaName, Name: "events_2026"}).ObjectID(), Name: "events_kind_check"}).ObjectID()
	object, ok := snapshot.Object(childID)
	if !ok {
		t.Fatalf("partition check %s missing from graph", childID)
	}
	child, ok := object.(pgschema.Constraint)
	if !ok || child.Parent == nil || *child.Parent != parent || !containsID(snapshot.Dependencies(childID), parent) {
		t.Fatalf("partitioned inherited CHECK missing typed parent edge: child=%#v deps=%#v", object, snapshot.Dependencies(childID))
	}
	if unsupported := snapshot.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("unexpected unsupported catalog state: %#v", unsupported)
	}
}

func lockIntegrationDatabase(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	const key int64 = 731095702114
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key) })
}
