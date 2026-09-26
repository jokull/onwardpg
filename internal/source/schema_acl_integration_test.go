package source

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

func TestLoadGraphSchemaACLDefaultAfterGrantRevoke(t *testing.T) {
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

	suffix := time.Now().UTC().UnixNano()
	schemaName := fmt.Sprintf("onwardpg_schema_acl_%d", suffix)
	observerName := fmt.Sprintf("onwardpg_schema_observer_%d", suffix)
	schema := quote(schemaName)
	observer := quote(observerName)
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE ROLE " + observer)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+observer) }()
	exec("CREATE SCHEMA " + schema)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") }()

	load := func() *pgschema.Snapshot {
		t.Helper()
		graph, err := LoadGraph(ctx, Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return graph
	}
	baseline := load()
	if unsupported := baseline.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("unexpected baseline blockers: %#v", unsupported)
	}
	baselineFingerprint, err := baseline.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + observer)
	exec("REVOKE USAGE ON SCHEMA " + schema + " FROM " + observer)
	exec("DROP OWNED BY " + observer)
	exec("DROP ROLE " + observer)
	var explicit, defaultACL bool
	if err := conn.QueryRow(ctx, `SELECT nspacl IS NOT NULL, nspacl = acldefault('n', nspowner)
FROM pg_namespace WHERE nspname = $1`, schemaName).Scan(&explicit, &defaultACL); err != nil {
		t.Fatal(err)
	}
	if !explicit || !defaultACL {
		t.Fatalf("grant and revoke did not leave an explicit default ACL: explicit=%t default=%t", explicit, defaultACL)
	}
	got := load()
	if unsupported := got.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("default-equivalent schema ACL was blocked: %#v", unsupported)
	}
	gotFingerprint, err := got.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if gotFingerprint != baselineFingerprint {
		t.Fatalf("grant and revoke changed graph fingerprint: got %s, want %s", gotFingerprint, baselineFingerprint)
	}

	selector := "acl:schema:" + schemaName
	exec("CREATE ROLE " + observer)
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + observer)
	if blockers := load().Unsupported(); !slices.Contains(blockers, selector) {
		t.Fatalf("genuine grantee ACL missing %s blocker in %#v", selector, blockers)
	}
	exec("REVOKE USAGE ON SCHEMA " + schema + " FROM " + observer)
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + observer + " WITH GRANT OPTION")
	if blockers := load().Unsupported(); !slices.Contains(blockers, selector) {
		t.Fatalf("grant option missing %s blocker in %#v", selector, blockers)
	}
	exec("REVOKE USAGE ON SCHEMA " + schema + " FROM " + observer)
	exec("REVOKE CREATE ON SCHEMA " + schema + " FROM CURRENT_USER")
	if blockers := load().Unsupported(); !slices.Contains(blockers, selector) {
		t.Fatalf("owner privilege revocation missing %s blocker in %#v", selector, blockers)
	}
	exec("GRANT CREATE ON SCHEMA " + schema + " TO CURRENT_USER")
	exec("ALTER SCHEMA " + schema + " OWNER TO " + observer)
	ownership := "ownership:schema:" + schemaName + "=" + observerName
	if blockers := load().Unsupported(); !slices.Contains(blockers, ownership) {
		t.Fatalf("foreign ownership missing %s blocker in %#v", ownership, blockers)
	}
}

func TestLoadGraphPublicSchemaStillRequiresPublicUsage(t *testing.T) {
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
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "REVOKE USAGE ON SCHEMA public FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	var genericDefault bool
	if err := tx.QueryRow(ctx, `SELECT nspacl = acldefault('n', nspowner)
FROM pg_namespace WHERE nspname = 'public'`).Scan(&genericDefault); err != nil {
		t.Fatal(err)
	}
	if !genericDefault {
		t.Fatal("public ACL did not become the generic owner-only default")
	}
	graph, err := InspectGraphTransaction(ctx, tx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if blockers := graph.Unsupported(); !slices.Contains(blockers, "acl:schema:public") {
		t.Fatalf("revoked public USAGE missing acl:schema:public blocker in %#v", blockers)
	}
}

func TestLoadGraphRetainsExtensionSchemaPrivilegeChanges(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockIntegrationDatabase(t, ctx, conn)
	name := fmt.Sprintf("onwardpg_extension_acl_%d", time.Now().UnixNano())
	schema := quote(name)
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA " + schema)
	defer func() {
		_, _ = conn.Exec(context.Background(), "ALTER EXTENSION plpgsql DROP SCHEMA "+schema)
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	}()
	exec("GRANT USAGE ON SCHEMA " + schema + " TO PUBLIC")
	// PostgreSQL records the member object's initial ACL when it joins an extension.
	exec("ALTER EXTENSION plpgsql ADD SCHEMA " + schema)
	load := func() *pgschema.Snapshot {
		t.Helper()
		graph, err := LoadGraph(ctx, Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return graph
	}
	selector := "acl:schema:" + name
	if slices.Contains(load().Unsupported(), selector) {
		t.Fatal("unchanged extension ACL was blocked")
	}
	exec("REVOKE USAGE ON SCHEMA " + schema + " FROM PUBLIC")
	var genericDefault, changedFromExtension bool
	if err := conn.QueryRow(ctx, `SELECT n.nspacl = acldefault('n', n.nspowner), i.initprivs IS DISTINCT FROM n.nspacl
 FROM pg_namespace n JOIN pg_init_privs i ON i.classoid='pg_namespace'::regclass AND i.objoid=n.oid AND i.objsubid=0
 WHERE n.nspname=$1`, name).Scan(&genericDefault, &changedFromExtension); err != nil {
		t.Fatal(err)
	}
	if !genericDefault || !changedFromExtension {
		t.Fatal("fixture must match generic default while differing from extension defaults")
	}
	if !slices.Contains(load().Unsupported(), selector) {
		t.Fatal("extension privilege revocation was hidden by generic ACL normalization")
	}
}
