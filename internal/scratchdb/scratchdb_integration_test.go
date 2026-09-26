package scratchdb

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRestrictedDatabaseCannotCreateClusterGlobalState(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	database, err := Create(ctx, url, "onwardpg_authority_test")
	if err != nil {
		t.Fatal(err)
	}
	name, role := database.Name, database.Role
	connection, err := database.Connect(ctx)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	sourceConnection, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	sourceEnvironment, err := readDatabaseEnvironment(ctx, sourceConnection)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceConnection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	scratchEnvironment, err := readDatabaseEnvironment(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	if scratchEnvironment != sourceEnvironment {
		t.Fatalf("scratch environment = %#v, source = %#v", scratchEnvironment, sourceEnvironment)
	}
	var current string
	var superuser, createDB, createRole, replication, bypassRLS bool
	if err := connection.QueryRow(ctx, `
SELECT current_user, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls
FROM pg_roles WHERE rolname = current_user`).Scan(&current, &superuser, &createDB, &createRole, &replication, &bypassRLS); err != nil {
		t.Fatal(err)
	}
	if current != role || superuser || createDB || createRole || replication || bypassRLS {
		t.Fatalf("scratch execution authority = %s super=%v createdb=%v createrole=%v replication=%v bypassrls=%v", current, superuser, createDB, createRole, replication, bypassRLS)
	}
	if _, err := connection.Exec(ctx, "CREATE TABLE local_object (id bigint)"); err != nil {
		t.Fatalf("database-local DDL failed: %v", err)
	}
	for _, sql := range []string{
		"CREATE ROLE onwardpg_must_not_exist",
		"CREATE DATABASE onwardpg_must_not_exist",
		"CREATE TABLESPACE onwardpg_must_not_exist LOCATION '/tmp/onwardpg-must-not-exist'",
	} {
		if _, err := connection.Exec(ctx, sql); err == nil {
			t.Fatalf("restricted execution unexpectedly accepted %q", sql)
		}
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var databases, roles int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", name).Scan(&databases); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname = $1", role).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if databases != 0 || roles != 0 {
		t.Fatalf("scratch cleanup left database=%d role=%d", databases, roles)
	}
	var leaked int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname = 'onwardpg_must_not_exist'").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("restricted DDL leaked a cluster-global role")
	}
}

func TestSanitizePrefix(t *testing.T) {
	if got := sanitizePrefix("OnwardPG verify-1!"); got != "onwardpgverify1" {
		t.Fatalf("sanitizePrefix = %q", got)
	}
	if got := sanitizePrefix(strings.Repeat("!", 3)); got != "onwardpg" {
		t.Fatalf("empty sanitizePrefix = %q", got)
	}
}

func TestCreateDatabaseSQLPreservesLocaleProvider(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		environment databaseEnvironment
		want        []string
	}{
		{name: "libc", environment: databaseEnvironment{Encoding: "UTF8", Collate: "C", CType: "C", Provider: "c"}, want: []string{"ENCODING 'UTF8'", "LC_COLLATE 'C'", "LC_CTYPE 'C'", "LOCALE_PROVIDER libc"}},
		{name: "icu", environment: databaseEnvironment{Encoding: "UTF8", Collate: "und-x-icu", CType: "und-x-icu", Provider: "i", Locale: "und"}, want: []string{"LOCALE_PROVIDER icu", "ICU_LOCALE 'und'"}},
		{name: "builtin", environment: databaseEnvironment{Encoding: "UTF8", Collate: "C.UTF-8", CType: "C.UTF-8", Provider: "b", Locale: "C.UTF-8"}, want: []string{"LOCALE_PROVIDER builtin", "BUILTIN_LOCALE 'C.UTF-8'"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			sql := createDatabaseSQL("scratch", "runner", fixture.environment)
			for _, fragment := range fixture.want {
				if !strings.Contains(sql, fragment) {
					t.Fatalf("create SQL %q omits %q", sql, fragment)
				}
			}
		})
	}
}

func TestCreateFailureCleansCreatedRole(t *testing.T) {
	url := testAdminURL(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	login := "onwardpg_cleanup_creator"
	if _, err := admin.Exec(ctx, "CREATE ROLE "+quoteIdentifier(login)+" LOGIN CREATEROLE PASSWORD 'temporary-test-password'"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+quoteIdentifier(login)); err != nil {
			t.Errorf("drop test administrator: %v", err)
		}
	}()
	parsedURL, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	parsedURL.User = neturl.UserPassword(login, "temporary-test-password")
	database, err := Create(ctx, parsedURL.String(), "onwardpg_cleanup_partial")
	if database != nil {
		defer database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "create restricted scratch database") {
		t.Fatalf("expected database creation failure, got %v", err)
	}
	var count int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname LIKE 'onwardpg_cleanup_partial_role_%'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed Create left %d restricted roles: %v", count, err)
	}
}

func TestCloseReconnectsAfterAdministratorTerminated(t *testing.T) {
	url := testAdminURL(t)
	ctx := context.Background()
	database, err := Create(ctx, url, "onwardpg_cleanup_reconnect")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", database.admin.PgConn().PID()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, admin, database.Name, database.Role)
	if err := database.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCloseTimeoutReportsExactResourcesAndRetries(t *testing.T) {
	previousTimeout := cleanupOperationTimeout
	cleanupOperationTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cleanupOperationTimeout = previousTimeout })
	url := testAdminURL(t)
	ctx := context.Background()
	database, err := Create(ctx, url, "onwardpg_cleanup_lock")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	locker, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close(ctx)
	transaction, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, "LOCK TABLE pg_database IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = database.Close()
	if err == nil {
		t.Fatal("expected cleanup failure while pg_database is locked")
	}
	if elapsed := time.Since(start); elapsed > 2*cleanupOperationTimeout+2*cleanupCloseTimeout+3*time.Second {
		t.Fatalf("Close exceeded bounded cleanup time: %s", elapsed)
	}
	for _, name := range []string{database.Name, database.Role} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("cleanup diagnostic missing exact resource %q: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), database.Config.Password) || strings.Contains(err.Error(), url) {
		t.Fatalf("cleanup diagnostic contains credential: %v", err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	assertAbsent(t, admin, database.Name, database.Role)
}

func TestCloseAfterCallerCancellationForcesActiveLogin(t *testing.T) {
	url := testAdminURL(t)
	ctx, cancel := context.WithCancel(context.Background())
	database, err := Create(ctx, url, "onwardpg_cleanup_cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	active, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close(context.Background())
	cancel()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	assertAbsent(t, admin, database.Name, database.Role)
}

func testAdminURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	return url
}

func assertAbsent(t *testing.T, admin *pgx.Conn, name, role string) {
	t.Helper()
	for _, fixture := range []struct{ catalog, column, value string }{
		{"pg_database", "datname", name},
		{"pg_roles", "rolname", role},
	} {
		var count int
		query := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = $1", fixture.catalog, fixture.column)
		if err := admin.QueryRow(context.Background(), query, fixture.value).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("cleanup left %s %q", fixture.catalog, fixture.value)
		}
	}
}
