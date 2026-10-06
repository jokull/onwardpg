package source

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/scratchdb"
)

// earthdistance and dblink are contrib extensions that PostgreSQL marks as
// superuser-only (not trusted), so the restricted scratch role cannot create
// them. These tests skip on a server where an administrator has made them
// trusted by editing the control file, because nothing is then left to prove.
// The search_path line keeps the unqualified earth type visible to earthdistance's
// SQL functions, which PostgreSQL 15 resolves with the caller's search_path.
const earthDistanceDDL = `
CREATE SCHEMA IF NOT EXISTS "extensions";
CREATE EXTENSION IF NOT EXISTS "cube" WITH SCHEMA "extensions";
CREATE EXTENSION IF NOT EXISTS "earthdistance" WITH SCHEMA "extensions";
SET search_path = public, extensions;
CREATE TABLE public.place (id bigint PRIMARY KEY, lat double precision NOT NULL, lon double precision NOT NULL);
CREATE INDEX place_location_idx ON public.place USING gist (extensions.ll_to_earth(lat, lon));
CREATE FUNCTION public.place_distance(a double precision, b double precision, c double precision, d double precision)
RETURNS double precision LANGUAGE sql STABLE AS $$ SELECT extensions.earth_distance(extensions.ll_to_earth(a, b), extensions.ll_to_earth(c, d)) $$;
`

func requireUntrustedExtension(t *testing.T, url, name string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	var untrusted bool
	err = connection.QueryRow(ctx, `
SELECT v.superuser AND NOT v.trusted
FROM pg_available_extensions a JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = a.default_version
WHERE a.name = $1`, name).Scan(&untrusted)
	if err != nil {
		t.Skipf("extension %s is not available on this server: %v", name, err)
	}
	if !untrusted {
		t.Skipf("extension %s is trusted on this server", name)
	}
}

func scratchTestURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	return url
}

func TestUntrustedExtensionWithoutAllowlistFailsWithHint(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "earthdistance")
	_, err := LoadDDLGraphForComparison(context.Background(), []byte(earthDistanceDDL), "test-ddl", url, nil)
	if err == nil {
		t.Fatal("restricted scratch role unexpectedly created an untrusted extension")
	}
	for _, fragment := range []string{`permission denied to create extension "earthdistance"`, "scratch_admin_extensions"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q omits %q", err, fragment)
		}
	}
}

func TestAllowlistedExtensionMatchesOwnerCreatedFingerprint(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "earthdistance")
	ctx := context.Background()
	allow := scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "earthdistance", Schema: "extensions"}})
	graph, err := LoadDDLGraphForComparison(ctx, []byte(earthDistanceDDL), "test-ddl", url, nil, allow)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := graph.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("administrator-owned extensions must not be blockers, got %v", unsupported)
	}
	fingerprint, err := graph.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	// A database whose owner is a superuser is the model of "the owner created
	// the extension itself": nothing is foreign-owned and nothing is exempted.
	reference := ownerCreatedFingerprint(t, url, earthDistanceDDL)
	if fingerprint != reference {
		t.Fatalf("allowlisted fingerprint %s differs from owner-created %s", fingerprint, reference)
	}
	again, err := LoadDDLGraphForComparison(ctx, []byte(earthDistanceDDL), "test-ddl", url, nil, allow)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, _ := again.Fingerprint(); repeated != fingerprint {
		t.Fatalf("repeated materialization changed fingerprint: %s vs %s", repeated, fingerprint)
	}
}

func TestAllowlistIsDemandDriven(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "earthdistance")
	ctx := context.Background()
	allow := scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "earthdistance", Schema: "extensions"}})
	const withoutExtension = `CREATE TABLE public.plain (id bigint PRIMARY KEY);`
	graph, err := LoadDDLGraphForComparison(ctx, []byte(withoutExtension), "test-ddl", url, nil, allow)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := LoadDDLGraphForComparison(ctx, []byte(withoutExtension), "test-ddl", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	withList, _ := graph.Fingerprint()
	without, _ := plain.Fingerprint()
	if withList != without {
		t.Fatalf("an allowlisted extension the DDL never asked for changed the graph: %s vs %s", withList, without)
	}
	// The empty catalog is the planner's baseline: it must not contain the
	// allowlisted extensions, or a first bundle would never create them.
	empty, err := LoadDDLGraphForComparison(ctx, nil, "empty-postgresql", url, nil, allow)
	if err != nil {
		t.Fatal(err)
	}
	emptyPlain, err := LoadDDLGraphForComparison(ctx, nil, "empty-postgresql", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	withList, _ = empty.Fingerprint()
	without, _ = emptyPlain.Fingerprint()
	if withList != without {
		t.Fatalf("the empty catalog changed under an allowlist: %s vs %s", withList, without)
	}
}

func TestAllowlistDoesNotAuthorizeOtherUntrustedExtensions(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "earthdistance")
	requireUntrustedExtension(t, url, "dblink")
	allow := scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "earthdistance", Schema: "extensions"}})
	_, err := LoadDDLGraphForComparison(context.Background(), []byte(`CREATE EXTENSION dblink;`), "test-ddl", url, nil, allow)
	if err == nil || !strings.Contains(err.Error(), `permission denied to create extension "dblink"`) || !strings.Contains(err.Error(), "scratch_admin_extensions") {
		t.Fatalf("an unlisted untrusted extension must keep the refusal and hint, got %v", err)
	}
}

func TestAllowlistedExtensionWithoutDependenciesInPublicSchema(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "dblink")
	ctx := context.Background()
	const ddl = `CREATE EXTENSION IF NOT EXISTS dblink WITH SCHEMA public;
CREATE VIEW public.remote_ping AS SELECT 1 AS ok WHERE length(public.dblink_get_connections()::text) >= 0;`
	allow := scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "dblink", Schema: "public"}})
	graph, err := LoadDDLGraphForComparison(ctx, []byte(ddl), "test-ddl", url, nil, allow)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := graph.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("unexpected blockers %v", unsupported)
	}
	fingerprint, err := graph.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if reference := ownerCreatedFingerprint(t, url, ddl); fingerprint != reference {
		t.Fatalf("fingerprint %s differs from owner-created %s", fingerprint, reference)
	}
}

func TestAllowlistedSchemaMustBeCreatedIdempotently(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "earthdistance")
	allow := scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "earthdistance", Schema: "extensions"}})
	const ddl = `CREATE SCHEMA extensions; CREATE EXTENSION cube WITH SCHEMA extensions; CREATE EXTENSION earthdistance WITH SCHEMA extensions;`
	_, err := LoadDDLGraphForComparison(context.Background(), []byte(ddl), "test-ddl", url, nil, allow)
	if err == nil || !strings.Contains(err.Error(), "CREATE SCHEMA IF NOT EXISTS") {
		t.Fatalf("expected an actionable IF NOT EXISTS hint, got %v", err)
	}
}

func TestAdministratorOwnershipExemptionIsExact(t *testing.T) {
	url := scratchTestURL(t)
	requireUntrustedExtension(t, url, "earthdistance")
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, url, "onwardpg_exempt_test", scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "earthdistance", Schema: "extensions"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	err = database.Retrying(ctx, func() error {
		_, execErr := owner.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS extensions; CREATE EXTENSION IF NOT EXISTS earthdistance WITH SCHEMA extensions CASCADE;`)
		return execErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed := strings.Join(database.InstalledByAdministrator(), ","); installed != "cube,earthdistance" {
		t.Fatalf("administrator installed %q, want the extension and its dependency", installed)
	}
	// The same database inspected without the exemption reports the ownership,
	// so the exemption is what keeps these extensions out of the graph.
	plain, err := inspectGraphConfig(ctx, database.Config, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plain.Unsupported(), ","); !strings.Contains(got, "ownership:extension:cube=") || !strings.Contains(got, "ownership:extension:earthdistance=") {
		t.Fatalf("without the exemption the administrator ownership must be reported, got %q", got)
	}
	exempt, err := InspectScratchGraph(ctx, database, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := exempt.Unsupported(); len(got) != 0 {
		t.Fatalf("exempted graph still has blockers %v", got)
	}
}

func ownerCreatedFingerprint(t *testing.T, url, ddl string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("onwardpg_owner_ref_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)+" TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE "+quote(name)+" WITH (FORCE)") }()
	config := admin.Config().Copy()
	config.Database = name
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, ddl); err != nil {
		connection.Close(ctx)
		t.Fatal(err)
	}
	connection.Close(ctx)
	graph, err := inspectGraphConfig(ctx, config, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := graph.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("reference database reported blockers %v", unsupported)
	}
	fingerprint, err := graph.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}
