package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/driftcheck"
	"github.com/jokull/onwardpg/internal/protocol"
)

// providerRole creates a NOLOGIN role that stands in for a managed-PostgreSQL
// provider's administrative role, allowed to create objects in the live
// database, and removes it when the test ends.
func providerRole(t *testing.T, adminURL, liveURL string) (role string, admin *pgx.Conn) {
	t.Helper()
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	role = "onwardpg_provider_" + hex.EncodeToString(random)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+quoted+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	liveConfig, err := pgx.ParseConfig(liveURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{liveConfig.Database}.Sanitize()+" TO "+quoted); err != nil {
		t.Fatal(err)
	}
	// The caller's deferred database cleanup runs first, so nothing is left for
	// the role to own by the time it is dropped.
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quoted)
		_ = admin.Close(context.Background())
	})
	return role, admin
}

// providerShape reproduces what a managed provider leaves in a production
// database: an extension and a schema owned by the provider's administrative
// role instead of the database owner.
func providerShape(t *testing.T, liveURL, role string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := connection.Exec(ctx, "SET ROLE "+quoted+"; CREATE EXTENSION citext; CREATE SCHEMA provider_ext; RESET ROLE"); err != nil {
		t.Fatal(err)
	}
}

func sortedUnsupported(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func TestLiveCommandsReportUnsupportedCatalogStateOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	role, _ := providerRole(t, adminURL, liveURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	ddl := "CREATE EXTENSION citext;\nCREATE TABLE public.users (id bigint, email citext);\n"
	writeTestFile(t, repository, "schema.sql", ddl)
	if initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	}); initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	providerShape(t, liveURL, role)
	connection, err := pgx.Connect(context.Background(), liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), "CREATE TABLE public.users (id bigint, email citext); CREATE INDEX users_email_manual_idx ON public.users (email)"); err != nil {
		t.Fatal(err)
	}
	wantUnsupported := []string{"ownership:extension:citext=" + role, "ownership:schema:provider_ext=" + role}

	var drift driftcheck.Report
	t.Run("drift check names the unsupported state and still lists differences", func(t *testing.T) {
		result := captureStdout(t, func() int {
			return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
		})
		if result.code != 3 {
			t.Fatalf("exit = %d, stdout = %s", result.code, result.stdout)
		}
		if err := json.Unmarshal([]byte(result.stdout), &drift); err != nil {
			t.Fatal(err)
		}
		if drift.Outcome != "unsupported" || !reflect.DeepEqual(sortedUnsupported(drift.Unsupported), wantUnsupported) {
			t.Fatalf("drift report = %#v", drift)
		}
		foundIndex := false
		for _, difference := range drift.Differences {
			foundIndex = foundIndex || strings.Contains(difference.ObjectID, "users_email_manual_idx")
		}
		if !foundIndex {
			t.Fatalf("differences were dropped: %#v", drift.Differences)
		}
	})

	t.Run("diff refuses exactly the same catalog state", func(t *testing.T) {
		scratch := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
		result := captureStdout(t, func() int {
			return runLowLevelPlan("diff", []string{"--from", liveURL, "--to", "file://" + repository + "/schema.sql", "--dev-url", scratch})
		})
		if result.code != 3 {
			t.Fatalf("diff exit = %d, stdout = %s", result.code, result.stdout)
		}
		var planned protocol.Result
		if err := json.Unmarshal([]byte(result.stdout), &planned); err != nil {
			t.Fatal(err)
		}
		if planned.Status != protocol.Unsupported || !reflect.DeepEqual(sortedUnsupported(planned.Unsupported), sortedUnsupported(drift.Unsupported)) {
			t.Fatalf("diff = %#v, drift = %#v", planned.Unsupported, drift.Unsupported)
		}
	})

	t.Run("a genuine blocker is reported next to provider state", func(t *testing.T) {
		if _, err := connection.Exec(context.Background(), "CREATE SEQUENCE public.manual_seq; ALTER SEQUENCE public.manual_seq OWNER TO "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		result := captureStdout(t, func() int {
			return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
		})
		var report driftcheck.Report
		if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, selector := range report.Unsupported {
			found = found || selector == "ownership:relation:public.manual_seq="+role
		}
		if result.code != 3 || report.Outcome != "unsupported" || !found {
			t.Fatalf("exit = %d, report = %#v", result.code, report)
		}
	})
}
