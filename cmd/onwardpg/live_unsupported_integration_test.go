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
	"github.com/jokull/onwardpg/internal/contractcheck"
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

func TestLiveIgnoreAcknowledgesProviderOwnedStateAcrossLiveCommandsOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	role, _ := providerRole(t, adminURL, liveURL)
	t.Setenv("ONWARDPG_PROVIDER_LIVE_URL", liveURL)
	repository := t.TempDir()
	writeConfig := func(liveIgnore ...string) {
		quoted := make([]string, len(liveIgnore))
		for index, selector := range liveIgnore {
			quoted[index] = `"` + selector + `"`
		}
		writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
live_ignore = [`+strings.Join(quoted, ", ")+`]
`)
	}
	extension, schema := "ownership:extension:citext="+role, "ownership:schema:provider_ext="+role
	writeConfig(extension, schema)
	// The project's own DDL creates the extension and the schema; in the live
	// cluster the provider's role owns both.
	ddl := "CREATE EXTENSION citext;\nCREATE SCHEMA provider_ext;\nCREATE TABLE public.users (id bigint, email citext);\n"
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
	if _, err := connection.Exec(context.Background(), "CREATE TABLE public.users (id bigint, email citext)"); err != nil {
		t.Fatal(err)
	}
	drift := func() (int, driftcheck.Report) {
		t.Helper()
		result := captureStdout(t, func() int {
			return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
		})
		var report driftcheck.Report
		if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
			t.Fatalf("drift response: %s: %v", result.stdout, err)
		}
		return result.code, report
	}
	diff := func(arguments ...string) captured {
		t.Helper()
		base := []string{"--from", liveURL, "--to", "file://" + repository + "/schema.sql", "--dev-url", adminURL}
		return captureStdout(t, func() int { return runLowLevelPlan("diff", append(base, arguments...)) })
	}

	t.Run("drift check passes and shows the acknowledged state", func(t *testing.T) {
		code, report := drift()
		if code != 0 || report.Outcome != "drift_free" || len(report.Unsupported) != 0 || len(report.Differences) != 0 {
			t.Fatalf("exit = %d, report = %#v", code, report)
		}
		if report.Observer == nil || !reflect.DeepEqual(report.Observer.LiveIgnored, []string{extension, schema}) {
			t.Fatalf("observer = %#v", report.Observer)
		}
		// Replayed history and the live catalog compare equal once the
		// acknowledged markers are removed; the raw catalog is reported apart.
		if report.ExpectedFingerprint != report.ActualFingerprint || report.Observer.ObservedFingerprint == "" || report.Observer.ObservedFingerprint == report.ActualFingerprint {
			t.Fatalf("fingerprints: expected %s, actual %s, observed %s", report.ExpectedFingerprint, report.ActualFingerprint, report.Observer.ObservedFingerprint)
		}
		observed := report.Observer.ObservedFingerprint
		writeConfig()
		defer writeConfig(extension, schema)
		code, report = drift()
		if code != 3 || report.Outcome != "unsupported" || len(report.Differences) != 0 || !reflect.DeepEqual(sortedUnsupported(report.Unsupported), []string{extension, schema}) ||
			report.ActualFingerprint != observed || report.Observer.ObservedFingerprint != "" {
			t.Fatalf("without live_ignore: exit = %d, report = %#v", code, report)
		}
	})

	t.Run("diff agrees only when it is given the target", func(t *testing.T) {
		if refused := diff(); refused.code != 3 || !strings.Contains(refused.stdout, extension) {
			t.Fatalf("diff without target: %d %s", refused.code, refused.stdout)
		}
		accepted := diff("--target", "primary", "--config", repository+"/.onwardpg.toml")
		var result protocol.Result
		if err := json.Unmarshal([]byte(accepted.stdout), &result); err != nil {
			t.Fatal(err)
		}
		if accepted.code != 0 || result.Status != protocol.Planned || len(result.Statements) != 0 ||
			!reflect.DeepEqual(result.Compatibility, []string{"live_ignored:" + extension, "live_ignored:" + schema}) {
			t.Fatalf("diff with target: %d %s", accepted.code, accepted.stdout)
		}
		if invalid := diff("--config", repository+"/.onwardpg.toml"); invalid.code == 0 || !strings.Contains(invalid.stdout, "--config requires --target") {
			t.Fatalf("--config without --target: %d %s", invalid.code, invalid.stdout)
		}
		if unknown := diff("--target", "absent", "--config", repository+"/.onwardpg.toml"); unknown.code == 0 || !strings.Contains(unknown.stdout, "invalid_config") {
			t.Fatalf("unknown target: %d %s", unknown.code, unknown.stdout)
		}
	})

	t.Run("a configured selector that matches nothing is reported and changes nothing", func(t *testing.T) {
		const unmatched = "parameter_acl:user" // PostgreSQL prints "user", quoted
		writeConfig(extension, schema, unmatched)
		defer writeConfig(extension, schema)
		code, report := drift()
		if code != 0 || report.Outcome != "drift_free" || report.Observer == nil ||
			!reflect.DeepEqual(report.Observer.LiveIgnored, []string{extension, schema}) ||
			!reflect.DeepEqual(report.Observer.LiveIgnoreUnmatched, []string{unmatched}) {
			t.Fatalf("exit = %d, report = %#v", code, report)
		}
		accepted := diff("--target", "primary", "--config", repository+"/.onwardpg.toml")
		var result protocol.Result
		if err := json.Unmarshal([]byte(accepted.stdout), &result); err != nil {
			t.Fatal(err)
		}
		if accepted.code != 0 || result.Status != protocol.Planned || !reflect.DeepEqual(result.Compatibility,
			[]string{"live_ignored:" + extension, "live_ignored:" + schema, "live_ignore_unmatched:" + unmatched}) {
			t.Fatalf("diff: %d %s", accepted.code, accepted.stdout)
		}
	})

	t.Run("the legacy plan spelling never consults live_ignore", func(t *testing.T) {
		arguments := []string{"--from", liveURL, "--to", "file://" + repository + "/schema.sql", "--dev-url", adminURL}
		// Without a target the provider state stops the plan, as before.
		refused := captureStdout(t, func() int { return runPlan(arguments) })
		if refused.code != 3 || !strings.Contains(refused.stdout, `"status":"unsupported"`) || !strings.Contains(refused.stdout, extension) {
			t.Fatalf("plan: %d %s", refused.code, refused.stdout)
		}
		// plan never accepted --target, and must not start applying the list.
		withTarget := captureStdout(t, func() int {
			return runPlan(append(append([]string(nil), arguments...), "--target", "primary", "--config", repository+"/.onwardpg.toml"))
		})
		if withTarget.code == 0 || strings.Contains(withTarget.stdout, `"status":"planned"`) || !strings.Contains(withTarget.stdout, "target") {
			t.Fatalf("plan --target: %d %s", withTarget.code, withTarget.stdout)
		}
	})

	t.Run("a genuine blocker is still reported", func(t *testing.T) {
		if _, err := connection.Exec(context.Background(), "CREATE SEQUENCE public.manual_seq; ALTER SEQUENCE public.manual_seq OWNER TO "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		code, report := drift()
		if code != 3 || report.Outcome != "unsupported" || !reflect.DeepEqual(report.Unsupported, []string{"ownership:relation:public.manual_seq=" + role}) {
			t.Fatalf("exit = %d, report = %#v", code, report)
		}
	})

	t.Run("an acknowledged marker never hides a modeled object", func(t *testing.T) {
		if _, err := connection.Exec(context.Background(), "DROP SEQUENCE public.manual_seq; CREATE SCHEMA provider_only AUTHORIZATION "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		writeConfig(extension, schema, "ownership:schema:provider_only="+role)
		code, report := drift()
		found := false
		for _, difference := range report.Differences {
			found = found || difference.Kind == "unexpected_in_actual" && strings.Contains(difference.ObjectID, "provider_only")
		}
		if code != 4 || report.Outcome != "drifted" || len(report.Unsupported) != 0 || !found {
			t.Fatalf("exit = %d, report = %#v", code, report)
		}
	})
	t.Run("contract check applies the same acknowledgement", func(t *testing.T) {
		if _, err := connection.Exec(context.Background(), "DROP SCHEMA provider_only"); err != nil {
			t.Fatal(err)
		}
		writeConfig(extension, schema)
		writeTestFile(t, repository, "schema.sql", strings.Replace(ddl, "email citext", "email citext, note text", 1))
		if planned := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"add-note", "--target", "primary"}, repository) }); planned.code != 0 {
			t.Fatalf("plan: %d %s", planned.code, planned.stdout)
		}
		if verified := captureStdout(t, func() int { return runVerifyAt([]string{"--target", "primary", "--bundle", "add-note"}, repository) }); verified.code != 0 {
			t.Fatalf("verify: %d %s", verified.code, verified.stdout)
		}
		if _, err := connection.Exec(context.Background(), "ALTER TABLE public.users ADD COLUMN note text"); err != nil {
			t.Fatal(err)
		}
		check := func() (int, contractcheck.Report) {
			t.Helper()
			result := captureStdout(t, func() int {
				return runContractAt([]string{"check", "--target", "primary", "--environment", "test", "--database-env", "ONWARDPG_PROVIDER_LIVE_URL"}, repository)
			})
			var report contractcheck.Report
			if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
				t.Fatalf("contract response: %s: %v", result.stdout, err)
			}
			return result.code, report
		}
		code, report := check()
		if code != 0 || report.Status != "ready" || !reflect.DeepEqual(report.Observer.LiveIgnored, []string{extension, schema}) {
			t.Fatalf("with live_ignore: %d %#v", code, report)
		}
		writeConfig()
		code, report = check()
		if code != 3 || report.Status != "unsupported" || !reflect.DeepEqual(sortedUnsupported(report.Unsupported), []string{extension, schema}) {
			t.Fatalf("without live_ignore: %d %#v", code, report)
		}
	})
}

func TestDiffReadsLiveURLFromEnvironmentOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	connection, err := pgx.Connect(context.Background(), liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), "CREATE TABLE public.users (id bigint)"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONWARDPG_DIFF_LIVE_URL", liveURL)
	schema := t.TempDir() + "/schema.sql"
	if err := os.WriteFile(schema, []byte("CREATE TABLE public.users (id bigint);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	byValue := captureStdout(t, func() int {
		return runLowLevelPlan("diff", []string{"--from", liveURL, "--to", "file://" + schema, "--dev-url", adminURL})
	})
	byEnvironment := captureStdout(t, func() int {
		return runLowLevelPlan("diff", []string{"--from-env", "ONWARDPG_DIFF_LIVE_URL", "--to", "file://" + schema, "--dev-url", adminURL})
	})
	if byEnvironment.code != 0 || byEnvironment.code != byValue.code || byEnvironment.stdout != byValue.stdout {
		t.Fatalf("--from-env: %d %s\n--from: %d %s", byEnvironment.code, byEnvironment.stdout, byValue.code, byValue.stdout)
	}
}
