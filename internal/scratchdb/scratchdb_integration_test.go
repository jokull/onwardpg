package scratchdb

import (
	"context"
	"os"
	"strings"
	"testing"

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
