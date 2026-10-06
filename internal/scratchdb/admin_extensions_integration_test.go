package scratchdb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func requireUntrusted(t *testing.T, url, name, version string) {
	t.Helper()
	connection, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var untrusted bool
	err = connection.QueryRow(context.Background(), `
SELECT v.superuser AND NOT v.trusted
FROM pg_available_extensions a JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = COALESCE(NULLIF($2, ''), a.default_version)
WHERE a.name = $1`, name, version).Scan(&untrusted)
	if err != nil || !untrusted {
		t.Skipf("%s %s is not an available untrusted extension here (%v)", name, version, err)
	}
}

// The recovery trigger relies on the non-localized source location PostgreSQL
// attaches to its own refusal. This test records those values on every major in
// CI and proves that a refusal raised by project SQL looks different.
func TestRefusalSourceLocationOnPostgreSQL(t *testing.T) {
	url := testAdminURL(t)
	requireUntrusted(t, url, "dblink", "")
	ctx := context.Background()
	database, err := Create(ctx, url, "onwardpg_refusal", WithAdminExtensions([]AdminExtension{{Name: "dblink", Schema: "public"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	connection, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)

	_, refused := connection.Exec(ctx, "CREATE EXTENSION dblink")
	var pgErr *pgconn.PgError
	if !errors.As(refused, &pgErr) || pgErr.Code != "42501" || pgErr.File != "extension.c" || pgErr.Routine != "execute_extension_script" {
		t.Fatalf("PostgreSQL's refusal has an unexpected source location: %#v", refused)
	}
	if name, ok := deniedExtension(refused); !ok || name != "dblink" {
		t.Fatalf("deniedExtension(%v) = %q, %v", refused, name, ok)
	}

	// A user-raised error with the same code and text, from a DO block.
	_, crafted := connection.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION 'permission denied to create extension "dblink"' USING ERRCODE = '42501'; END $$`)
	if !errors.As(crafted, &pgErr) || pgErr.Code != "42501" || pgErr.File == "extension.c" {
		t.Fatalf("a user-raised error must not look like PostgreSQL's refusal: %#v", crafted)
	}
	if _, ok := deniedExtension(crafted); ok {
		t.Fatal("a user-raised 42501 was accepted as an extension refusal")
	}
	recovered, err := database.Recover(ctx, crafted)
	if recovered || err != nil || len(database.InstalledByAdministrator()) != 0 {
		t.Fatalf("a crafted error triggered installation: %v %v %v", recovered, err, database.InstalledByAdministrator())
	}
	if retried := database.Retrying(ctx, func() error { return crafted }); retried != crafted {
		t.Fatalf("the original error must come back unchanged, got %v", retried)
	}
	var count int
	if err := connection.QueryRow(ctx, "SELECT count(*) FROM pg_extension WHERE extname = 'dblink'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("dblink exists after a crafted error: %d %v", count, err)
	}

	// The same crafted block through the retry loop, as project DDL would run it.
	err = database.Retrying(ctx, func() error {
		_, execErr := connection.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION 'permission denied for table "dblink"' USING ERRCODE = '42501'; END $$`)
		return execErr
	})
	if err == nil || strings.Contains(err.Error(), "scratch_admin_extensions") || len(database.InstalledByAdministrator()) != 0 {
		t.Fatalf("crafted DDL error handling: %v", err)
	}

	// A genuine refusal is recovered.
	err = database.Retrying(ctx, func() error {
		_, execErr := connection.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS dblink WITH SCHEMA public")
		return execErr
	})
	if err != nil || strings.Join(database.InstalledByAdministrator(), ",") != "dblink" {
		t.Fatalf("genuine refusal was not recovered: %v %v", err, database.InstalledByAdministrator())
	}
}

func TestAdministratorInstallsTheEntryVersion(t *testing.T) {
	url := testAdminURL(t)
	// pg_prewarm ships 1.1 and 1.2 on PostgreSQL 15 through 18; 1.2 is the default.
	requireUntrusted(t, url, "pg_prewarm", "1.1")
	ctx := context.Background()
	for version, want := range map[string]string{"1.1": "1.1", "": ""} {
		database, err := Create(ctx, url, "onwardpg_version", WithAdminExtensions([]AdminExtension{{Name: "pg_prewarm", Schema: "tools", Version: version}}))
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer database.Close()
			connection, err := database.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close(ctx)
			// The project line asks for 1.2; for an allowlisted extension the entry decides.
			err = database.Retrying(ctx, func() error {
				_, execErr := connection.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS tools; CREATE EXTENSION IF NOT EXISTS pg_prewarm WITH SCHEMA tools VERSION '1.2'`)
				return execErr
			})
			if err != nil {
				t.Fatal(err)
			}
			var installed, def string
			if err := connection.QueryRow(ctx, "SELECT e.extversion, a.default_version FROM pg_extension e JOIN pg_available_extensions a ON a.name = e.extname WHERE e.extname = 'pg_prewarm'").Scan(&installed, &def); err != nil {
				t.Fatal(err)
			}
			if want == "" {
				want = def
			}
			if installed != want {
				t.Fatalf("entry version %q installed %s, want %s", version, installed, want)
			}
		}()
	}
}

func TestCheckAdminExtensionsRequiresTheVersionToExist(t *testing.T) {
	url := testAdminURL(t)
	requireUntrusted(t, url, "pg_prewarm", "1.1")
	ctx := context.Background()
	if _, err := CheckAdminExtensions(ctx, url, []AdminExtension{{Name: "pg_prewarm", Schema: "tools", Version: "1.1"}}); err != nil {
		t.Fatal(err)
	}
	_, err := CheckAdminExtensions(ctx, url, []AdminExtension{{Name: "pg_prewarm", Schema: "tools", Version: "9.9"}})
	if err == nil || !strings.Contains(err.Error(), `version "9.9"`) {
		t.Fatalf("a version the server lacks must be an error, got %v", err)
	}
}

func TestCheckAdminExtensionNotesAreDependencyAware(t *testing.T) {
	url := testAdminURL(t)
	requireUntrusted(t, url, "earthdistance", "")
	ctx := context.Background()
	check := func(entries ...AdminExtension) []string {
		t.Helper()
		notes, err := CheckAdminExtensions(ctx, url, entries)
		if err != nil {
			t.Fatal(err)
		}
		return notes
	}
	// cube is trusted but earthdistance requires it: the entry decides where
	// the administrator installs it, so it is not "unnecessary".
	notes := check(AdminExtension{Name: "earthdistance", Schema: "extensions"}, AdminExtension{Name: "cube", Schema: "geo", Version: "1.5"})
	if len(notes) != 1 || !strings.Contains(notes[0], `entry "cube" fixes the schema and version of a dependency of "earthdistance"`) || strings.Contains(notes[0], "not needed") {
		t.Fatalf("dependency note = %q", notes)
	}
	// A trusted extension listed alone, or next to an untrusted one that does not
	// depend on it, has no effect.
	for _, entries := range [][]AdminExtension{
		{{Name: "cube", Schema: "extensions"}},
		{{Name: "pg_trgm", Schema: "extensions"}, {Name: "earthdistance", Schema: "extensions"}},
	} {
		notes := check(entries...)
		if len(notes) != 1 || !strings.Contains(notes[0], "is not needed on this scratch server") {
			t.Fatalf("entries %v: notes = %q", entries, notes)
		}
	}
	if notes := check(AdminExtension{Name: "earthdistance", Schema: "extensions"}); len(notes) != 0 {
		t.Fatalf("an entry the restricted role cannot create needs no note: %q", notes)
	}
}
