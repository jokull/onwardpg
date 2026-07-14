package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

func TestPostgresMajorDiscoversTheConnectedServer(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	major, err := PostgresMajor(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	if major < 15 || major > 18 {
		t.Fatalf("PostgresMajor = %d", major)
	}
}

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

func TestLoadGraphBlocksPreviouslyBlindCatalogFamilies(t *testing.T) {
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
	suffix := time.Now().UTC().Format("20060102150405")
	schemaName := "onwardpg_blockers_" + suffix
	roleName := "onwardpg_owner_" + suffix
	eventName := "onwardpg_event_" + suffix
	publicationName := "onwardpg_pub_" + suffix
	fdwName := "onwardpg_fdw_" + suffix
	serverName := "onwardpg_server_" + suffix
	accessMethodName := "onwardpg_am_" + suffix
	languageName := "onwardpg_lang_" + suffix
	subscriptionName := "onwardpg_sub_" + suffix
	cleanup := func(sql string) { _, _ = conn.Exec(context.Background(), sql) }
	defer cleanup("DROP ROLE IF EXISTS " + quote(roleName))
	defer cleanup("DROP SCHEMA IF EXISTS " + quote(schemaName) + " CASCADE")
	defer func() {
		_, _ = conn.Exec(context.Background(), "ALTER SUBSCRIPTION "+quote(subscriptionName)+" SET (slot_name = NONE)")
		_, _ = conn.Exec(context.Background(), "DROP SUBSCRIPTION IF EXISTS "+quote(subscriptionName))
	}()
	defer cleanup("DROP LANGUAGE IF EXISTS " + quote(languageName))
	defer cleanup("DROP ACCESS METHOD IF EXISTS " + quote(accessMethodName))
	defer cleanup("DROP FOREIGN DATA WRAPPER IF EXISTS " + quote(fdwName) + " CASCADE")
	defer cleanup("DROP PUBLICATION IF EXISTS " + quote(publicationName))
	defer cleanup("DROP EVENT TRIGGER IF EXISTS " + quote(eventName))

	ddl := "CREATE ROLE " + quote(roleName) + ";" +
		"CREATE SCHEMA " + quote(schemaName) + ";" +
		"CREATE TABLE " + quote(schemaName) + ".objects (id bigint, other bigint, label text);" +
		"CREATE TABLE " + quote(schemaName) + ".inherited (extra bigint) INHERITS (" + quote(schemaName) + ".objects);" +
		"ALTER TABLE " + quote(schemaName) + ".objects ALTER COLUMN label SET STORAGE EXTERNAL;" +
		"ALTER TABLE " + quote(schemaName) + ".objects ALTER COLUMN label SET STATISTICS 500;" +
		"CREATE INDEX objects_label_c_idx ON " + quote(schemaName) + ".objects (label COLLATE \"C\");" +
		"CREATE INDEX objects_cluster_idx ON " + quote(schemaName) + ".objects (id);" +
		"CLUSTER " + quote(schemaName) + ".objects USING objects_cluster_idx;" +
		"ALTER TABLE " + quote(schemaName) + ".objects REPLICA IDENTITY FULL;" +
		"ALTER TABLE " + quote(schemaName) + ".objects SET (fillfactor = 70);" +
		"GRANT SELECT ON " + quote(schemaName) + ".objects TO PUBLIC;" +
		"ALTER DEFAULT PRIVILEGES IN SCHEMA " + quote(schemaName) + " GRANT SELECT ON TABLES TO PUBLIC;" +
		"CREATE TABLE " + quote(schemaName) + ".owned (id bigint);" +
		"ALTER TABLE " + quote(schemaName) + ".owned OWNER TO " + quote(roleName) + ";" +
		"CREATE RULE no_delete AS ON DELETE TO " + quote(schemaName) + ".objects DO INSTEAD NOTHING;" +
		"CREATE TEXT SEARCH DICTIONARY " + quote(schemaName) + ".simple_dict (TEMPLATE = pg_catalog.simple);" +
		"CREATE TEXT SEARCH CONFIGURATION " + quote(schemaName) + ".simple_cfg (COPY = pg_catalog.simple);" +
		"CREATE CONVERSION " + quote(schemaName) + ".latin1_to_utf8 FOR 'LATIN1' TO 'UTF8' FROM iso8859_1_to_utf8;" +
		"CREATE OPERATOR FAMILY " + quote(schemaName) + ".int_family USING btree;" +
		"CREATE TYPE " + quote(schemaName) + ".code AS ENUM ('x');" +
		"COMMENT ON TYPE " + quote(schemaName) + ".code IS 'must not disappear';" +
		"CREATE FUNCTION " + quote(schemaName) + ".code_text(" + quote(schemaName) + ".code) RETURNS text LANGUAGE SQL IMMUTABLE AS 'SELECT $1::text';" +
		"CREATE CAST (" + quote(schemaName) + ".code AS text) WITH FUNCTION " + quote(schemaName) + ".code_text(" + quote(schemaName) + ".code);" +
		"CREATE FUNCTION " + quote(schemaName) + ".equals(bigint, bigint) RETURNS boolean LANGUAGE SQL IMMUTABLE AS 'SELECT $1 = $2';" +
		"CREATE OPERATOR " + quote(schemaName) + ".=== (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = " + quote(schemaName) + ".equals);" +
		"CREATE FUNCTION " + quote(schemaName) + ".trigger_sink() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';" +
		"CREATE TRIGGER objects_trigger BEFORE INSERT ON " + quote(schemaName) + ".objects FOR EACH ROW EXECUTE FUNCTION " + quote(schemaName) + ".trigger_sink();" +
		"COMMENT ON TRIGGER objects_trigger ON " + quote(schemaName) + ".objects IS 'must not disappear';" +
		"CREATE POLICY objects_policy ON " + quote(schemaName) + ".objects USING (true);" +
		"COMMENT ON POLICY objects_policy ON " + quote(schemaName) + ".objects IS 'must not disappear';" +
		"CREATE VIEW " + quote(schemaName) + ".objects_view AS SELECT id FROM " + quote(schemaName) + ".objects;" +
		"COMMENT ON COLUMN " + quote(schemaName) + ".objects_view.id IS 'must not disappear';" +
		"CREATE STATISTICS " + quote(schemaName) + ".objects_stats ON id, other FROM " + quote(schemaName) + ".objects;" +
		"CREATE FUNCTION " + quote(schemaName) + ".event_sink() RETURNS event_trigger LANGUAGE plpgsql AS 'BEGIN END';" +
		"CREATE EVENT TRIGGER " + quote(eventName) + " ON ddl_command_end EXECUTE FUNCTION " + quote(schemaName) + ".event_sink();" +
		"CREATE PUBLICATION " + quote(publicationName) + " FOR TABLE " + quote(schemaName) + ".objects;" +
		"CREATE FOREIGN DATA WRAPPER " + quote(fdwName) + ";" +
		"CREATE SERVER " + quote(serverName) + " FOREIGN DATA WRAPPER " + quote(fdwName) + ";" +
		"CREATE USER MAPPING FOR PUBLIC SERVER " + quote(serverName) + ";" +
		"CREATE ACCESS METHOD " + quote(accessMethodName) + " TYPE TABLE HANDLER heap_tableam_handler;" +
		"CREATE LANGUAGE " + quote(languageName) + " HANDLER plpgsql_call_handler INLINE plpgsql_inline_handler VALIDATOR plpgsql_validator;"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SUBSCRIPTION "+quote(subscriptionName)+" CONNECTION 'host=invalid dbname=invalid' PUBLICATION fake WITH (connect=false, create_slot=false, enabled=false)"); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version >= 150000 {
		if _, err := conn.Exec(ctx, "GRANT SET ON PARAMETER work_mem TO "+quote(roleName)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = conn.Exec(context.Background(), "REVOKE ALL ON PARAMETER work_mem FROM "+quote(roleName))
			_, _ = conn.Exec(context.Background(), "REVOKE ALL ON PARAMETER work_mem FROM "+quote(conn.Config().User))
		}()
	}

	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := snapshot.Unsupported()
	for _, prefix := range []string{
		"ownership:relation:", "acl:default:", "rule:",
		"replica_identity:", "clustered_index:", "table_options:",
		"text_search_configuration:", "text_search_dictionary:", "event_trigger:",
		"publication:", "extended_statistics:", "foreign_data_wrapper:",
		"foreign_server:", "user_mapping:", "table_inheritance:",
		"column_storage:", "column_statistics:", "index_collation:",
		"access_method:", "operator:", "operator_family:", "cast:",
		"conversion:", "procedural_language:", "subscription:",
		"comment:enum:", "comment:trigger:", "comment:policy:", "comment:view_column:",
	} {
		found := false
		for _, selector := range unsupported {
			if strings.HasPrefix(selector, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s blocker in %#v", prefix, unsupported)
		}
	}
	if version >= 150000 {
		found := false
		for _, selector := range unsupported {
			if strings.HasPrefix(selector, "parameter_acl:") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing parameter_acl blocker in %#v", unsupported)
		}
	}
	ignoredSnapshot, err := LoadGraph(ctx, Parse(url), "", unsupported)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := ignoredSnapshot.Unsupported(); len(remaining) != 0 {
		t.Fatalf("exact blocker ignores left unsupported state: %#v", remaining)
	}
	ignored := ignoredSnapshot.Ignored()
	if len(ignored) != len(unsupported) {
		t.Fatalf("ignore receipt has %d selectors, want %d: %#v", len(ignored), len(unsupported), ignored)
	}
	for i := range unsupported {
		if ignored[i] != unsupported[i] {
			t.Fatalf("ignore receipt differs at %d: got %q want %q", i, ignored[i], unsupported[i])
		}
	}
}

func TestPostgres18NamedNotNullConstraintsBlock(t *testing.T) {
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
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 180000 {
		t.Skip("named NOT NULL constraints are a PostgreSQL 18 catalog family")
	}
	schemaName := "onwardpg_not_null_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quote(schemaName)+"; CREATE TABLE "+quote(schemaName)+".objects (id bigint CONSTRAINT id_required NOT NULL, doubled bigint GENERATED ALWAYS AS (id * 2) VIRTUAL)"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"not_null_constraint:" + schemaName + ".objects.id_required",
		"virtual_generated_column:" + schemaName + ".objects.doubled",
	} {
		found := false
		for _, selector := range snapshot.Unsupported() {
			if selector == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q blocker in %#v", want, snapshot.Unsupported())
		}
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
		"CREATE FUNCTION " + quote(schemaName) + ".tenant_allowed(value bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT value > 0 $$;" +
		"ALTER TABLE " + quote(schemaName) + ".orders ENABLE ROW LEVEL SECURITY;" +
		"ALTER TABLE " + quote(schemaName) + ".orders FORCE ROW LEVEL SECURITY;" +
		"CREATE POLICY tenant ON " + quote(schemaName) + ".orders USING (" + quote(schemaName) + ".tenant_allowed(tenant_id));" +
		"GRANT SELECT ON " + quote(schemaName) + ".orders TO PUBLIC;"
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+quote(schemaName)+" CASCADE") }()
	tableID := (pgschema.Table{Schema: schemaName, Name: "orders"}).ObjectID()
	policyID := (pgschema.Policy{Table: tableID, Name: "tenant"}).ObjectID()
	rlsID := (pgschema.RowSecurity{Table: tableID}).ObjectID()
	privilegeID := (pgschema.TablePrivilege{Table: tableID, Grantee: "PUBLIC", Privilege: "SELECT"}).ObjectID()
	typed, err := LoadGraph(ctx, Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	policyObject, exists := typed.Object(policyID)
	policyValue, ok := policyObject.(pgschema.Policy)
	if !exists || !ok || policyValue.Command != "ALL" || !policyValue.Permissive || len(policyValue.Roles) != 1 || policyValue.Roles[0] != "PUBLIC" || policyValue.Using == nil {
		t.Fatalf("policy catalog state missing: %#v", policyObject)
	}
	if !containsID(typed.Dependencies(policyID), (pgschema.Column{Table: tableID, Name: "tenant_id"}).ObjectID()) {
		t.Fatalf("policy column dependency missing: %#v", typed.Dependencies(policyID))
	}
	if !containsID(typed.Dependencies(policyID), (pgschema.Routine{Schema: schemaName, Name: "tenant_allowed", Signature: "value bigint"}).ObjectID()) {
		t.Fatalf("policy routine dependency missing: %#v", typed.Dependencies(policyID))
	}
	rlsObject, exists := typed.Object(rlsID)
	if value, ok := rlsObject.(pgschema.RowSecurity); !exists || !ok || !value.Enabled || !value.Forced || !containsID(typed.Dependencies(rlsID), policyID) {
		t.Fatalf("row-security state/dependency missing: object=%#v deps=%#v", rlsObject, typed.Dependencies(rlsID))
	}
	privilegeObject, exists := typed.Object(privilegeID)
	if value, ok := privilegeObject.(pgschema.TablePrivilege); !exists || !ok || value.Grantor != "@owner" || value.Grantable {
		t.Fatalf("table privilege state missing: %#v", privilegeObject)
	}
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
