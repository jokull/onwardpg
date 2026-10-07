package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/contractcheck"
	"github.com/jokull/onwardpg/internal/driftcheck"
)

// Drift check reads the live database through a role that the team can
// create on a managed provider: a login role that only inherits
// pg_read_all_data, or a login role with nothing at all. This test runs the
// command as such roles against a database that also holds provider state
// they cannot read, and proves three rules:
//
//   - the observer needs no privilege on an application or provider object,
//     so no access error names an object, ignored or not;
//   - live_ignore acknowledges who owns provider state and removes no object
//     from the comparison; only --ignore removes an object;
//   - a role that the guard refuses stops the command before the history
//     replay.
func TestDriftCheckRunsAsReadOnlyRolesAndRefusesAWrongRoleBeforeReplayOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	cluster, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close(context.Background())
	live, err := pgx.Connect(ctx, liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())

	const password = "drift-observer-test-password"
	const plain = " NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
	prefix := "onwardpg_drift_" + randomToken(t)[:12] + "_"
	role := func(suffix string) string { return pgx.Identifier{prefix + suffix}.Sanitize() }
	suffixes := []string{"provider", "bare", "reader", "bypass", "writer"}
	defer func() {
		// The provider owns objects in the live database. Drop them first.
		_, _ = live.Exec(context.Background(), "DROP SCHEMA IF EXISTS provider_ext CASCADE")
		for _, suffix := range suffixes {
			_, _ = cluster.Exec(context.Background(), "DROP ROLE IF EXISTS "+role(suffix))
		}
	}()
	if _, err := cluster.Exec(ctx,
		"CREATE ROLE "+role("provider")+" NOLOGIN"+plain+";"+
			"CREATE ROLE "+role("bare")+" LOGIN PASSWORD '"+password+"'"+plain+";"+
			"CREATE ROLE "+role("reader")+" LOGIN PASSWORD '"+password+"'"+plain+" IN ROLE pg_read_all_data;"+
			// The role of a team whose tables have row-level security with
			// no policy for a reader: read-only memberships and BYPASSRLS.
			"CREATE ROLE "+role("bypass")+" LOGIN PASSWORD '"+password+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS IN ROLE pg_read_all_data, pg_read_all_stats;"+
			"CREATE ROLE "+role("writer")+" LOGIN PASSWORD '"+password+"'"+plain+" IN ROLE pg_read_all_data, pg_write_all_data;"); err != nil {
		t.Fatal(err)
	}
	urlFor := func(suffix string) string {
		parsed, err := url.Parse(liveURL)
		if err != nil {
			t.Fatal(err)
		}
		parsed.User = url.UserPassword(prefix+suffix, password)
		return parsed.String()
	}

	ownership := "ownership:schema:provider_ext=" + prefix + "provider"
	config := func(scratchEnv string, liveIgnore bool) string {
		body := `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "` + scratchEnv + `"
`
		if liveIgnore {
			body += "live_ignore = ['" + ownership + "']\n"
		}
		return body
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", config("ONWARDPG_TEST_DATABASE_URL", false))
	writeTestFile(t, repository, "acknowledged.toml", config("ONWARDPG_TEST_DATABASE_URL", true))
	// No server listens here: a command that reaches the replay fails on it.
	t.Setenv("ONWARDPG_TEST_NO_SCRATCH_URL", "postgres://onwardpg@127.0.0.1:1/onwardpg?sslmode=disable&connect_timeout=2")
	writeTestFile(t, repository, "no-scratch.toml", config("ONWARDPG_TEST_NO_SCRATCH_URL", true))
	ddl := "CREATE SCHEMA app;\nCREATE TABLE app.users (id bigint PRIMARY KEY, email text);\nALTER TABLE app.users ENABLE ROW LEVEL SECURITY;\n"
	writeTestFile(t, repository, "schema.sql", ddl)
	if initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	}); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	// The application schema is the history. The provider schema is not in
	// it, belongs to another role, and gives the observers no access.
	if _, err := live.Exec(ctx, ddl+
		"CREATE SCHEMA provider_ext AUTHORIZATION "+role("provider")+";"+
		"CREATE TABLE provider_ext.listing (id integer);"+
		"ALTER TABLE provider_ext.listing OWNER TO "+role("provider")+";"); err != nil {
		t.Fatal(err)
	}
	var usable bool
	if err := live.QueryRow(ctx, "SELECT has_schema_privilege($1, 'app', 'USAGE') OR has_schema_privilege($1, 'provider_ext', 'USAGE') OR has_table_privilege($1, 'app.users', 'SELECT')", prefix+"bare").Scan(&usable); err != nil || usable {
		t.Fatalf("the bare observer has access to an application or provider object: %t, %v", usable, err)
	}

	// Row-level security without a policy hides each row from a plain
	// reader, with no error. BYPASSRLS shows them. Neither changes what the
	// catalog holds.
	if _, err := live.Exec(ctx, "INSERT INTO app.users VALUES (1, 'a@example.com')"); err != nil {
		t.Fatal(err)
	}
	for suffix, want := range map[string]int{"reader": 0, "bypass": 1} {
		connection, err := pgx.Connect(ctx, urlFor(suffix))
		if err != nil {
			t.Fatal(err)
		}
		var visible int
		err = connection.QueryRow(ctx, "SELECT count(*) FROM app.users").Scan(&visible)
		_ = connection.Close(ctx)
		if err != nil || visible != want {
			t.Fatalf("%s reads %d rows of app.users, want %d: %v", suffix, visible, want, err)
		}
	}

	type result struct {
		code   int
		report driftcheck.Report
		stdout string
	}
	check := func(suffix, configName string, arguments ...string) result {
		t.Helper()
		output := captureStdout(t, func() int {
			return runDriftAt(append([]string{"check", "--target", "primary", "--config", configName, "--database", urlFor(suffix)}, arguments...), repository)
		})
		var report driftcheck.Report
		if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
			t.Fatalf("drift check as %s = %d, %s", suffix, output.code, output.stdout)
		}
		return result{code: output.code, report: report, stdout: output.stdout}
	}
	// providerObjects reports that the provider schema and its table are
	// differences, and that nothing else is.
	providerObjects := func(report driftcheck.Report) bool {
		t.Helper()
		found := make(map[string]bool)
		for _, difference := range report.Differences {
			if difference.Kind != "unexpected_in_actual" || !strings.Contains(difference.ObjectID, "provider_ext") {
				t.Fatalf("difference outside the provider schema: %#v", report.Differences)
			}
			found[difference.ObjectID] = true
		}
		return found["schema:provider_ext"] && found["table:provider_ext:listing"]
	}

	// Without access to anything, the role reads the complete catalog: the
	// provider objects are differences, and their owner is unsupported state.
	// No result is an access error.
	first := check("bare", ".onwardpg.toml")
	if first.code != 3 || first.report.Outcome != "unsupported" || !slices.Contains(first.report.Unsupported, ownership) || !providerObjects(first.report) {
		t.Fatalf("bare observer without acknowledgement = %d, %s", first.code, first.stdout)
	}
	if first.report.Observer == nil || first.report.Observer.Mode != "dedicated_read_only" || first.report.Observer.Role != prefix+"bare" {
		t.Fatalf("bare observer = %#v", first.report.Observer)
	}

	// live_ignore acknowledges the owner. The objects stay in the comparison.
	acknowledged := check("bare", "acknowledged.toml")
	if acknowledged.code != 4 || acknowledged.report.Outcome != "drifted" || len(acknowledged.report.Unsupported) != 0 ||
		!providerObjects(acknowledged.report) || !slices.Contains(acknowledged.report.Observer.LiveIgnored, ownership) {
		t.Fatalf("bare observer with live_ignore = %d, %s", acknowledged.code, acknowledged.stdout)
	}

	// --ignore removes the objects. Nothing asks for access to them.
	ignores := []string{"--ignore", "schema:provider_ext", "--ignore", "table:provider_ext.listing"}
	for suffix, mode := range map[string]string{"bare": "dedicated_read_only", "reader": "predefined_read_role", "bypass": "predefined_read_role"} {
		ignored := check(suffix, "acknowledged.toml", ignores...)
		if ignored.code != 0 || ignored.report.Outcome != "drift_free" || len(ignored.report.Differences) != 0 ||
			ignored.report.ExpectedFingerprint != ignored.report.ActualFingerprint ||
			!slices.Contains(ignored.report.Ignored, "schema:provider_ext") || !slices.Contains(ignored.report.Ignored, "table:provider_ext.listing") {
			t.Fatalf("%s observer with --ignore = %d, %s", suffix, ignored.code, ignored.stdout)
		}
		if ignored.report.Observer == nil || ignored.report.Observer.Mode != mode || ignored.report.Observer.Role != prefix+suffix ||
			ignored.report.Observer.BypassRLS != (suffix == "bypass") || strings.Contains(ignored.stdout, `"bypass_rls":true`) != (suffix == "bypass") {
			t.Fatalf("%s observer = %s, want mode %s", suffix, ignored.stdout, mode)
		}
	}

	// The same role with one direct INSERT grant can change rows that
	// policies protect. The guard refuses it, also before the replay.
	if _, err := live.Exec(ctx, "GRANT INSERT ON app.users TO "+role("bypass")); err != nil {
		t.Fatal(err)
	}
	withInsert := captureStdout(t, func() int {
		return runDriftAt(append([]string{"check", "--target", "primary", "--config", "no-scratch.toml", "--database", urlFor("bypass")}, ignores...), repository)
	})
	if withInsert.code != 1 || !strings.Contains(withInsert.stdout, `"code":"drift_observer_access_policy_unsafe"`) ||
		!strings.Contains(withInsert.stdout, "observer is not read-only: ") || !strings.Contains(withInsert.stdout, "can write to relation app.users") ||
		!strings.Contains(withInsert.stdout, `"bypass_rls":true`) {
		t.Fatalf("BYPASSRLS role with an INSERT grant = %d, %s", withInsert.code, withInsert.stdout)
	}
	if _, err := live.Exec(ctx, "REVOKE INSERT ON app.users FROM "+role("bypass")); err != nil {
		t.Fatal(err)
	}

	// A role that can write is refused. The scratch server of this
	// configuration does not exist, so the answer proves that the guard ran
	// before the replay; a valid role gets as far as the replay and fails there.
	refused := captureStdout(t, func() int {
		return runDriftAt(append([]string{"check", "--target", "primary", "--config", "no-scratch.toml", "--database", urlFor("writer")}, ignores...), repository)
	})
	var diagnostic struct {
		Status      string                     `json:"status"`
		Code        string                     `json:"code"`
		Message     string                     `json:"message"`
		Observer    *driftcheck.Observer       `json:"observer"`
		NextActions []contractcheck.NextAction `json:"next_actions"`
	}
	if err := json.Unmarshal([]byte(refused.stdout), &diagnostic); err != nil {
		t.Fatalf("refused role = %d, %s", refused.code, refused.stdout)
	}
	if refused.code != 1 || diagnostic.Status != "error" || diagnostic.Code != "drift_observer_role_elevated" ||
		!strings.Contains(diagnostic.Message, "inherits unsafe role pg_write_all_data") ||
		diagnostic.Observer == nil || diagnostic.Observer.Role != prefix+"writer" || diagnostic.Observer.Mode != "refused" {
		t.Fatalf("refused role = %d, %s", refused.code, refused.stdout)
	}
	if len(diagnostic.NextActions) != 2 || diagnostic.NextActions[0].Kind != "create_observer_role" ||
		!strings.Contains(diagnostic.NextActions[0].SQL, "IN ROLE pg_read_all_data;") ||
		!strings.Contains(diagnostic.NextActions[1].SQL, "CREATE ROLE onwardpg_observer LOGIN") ||
		strings.Contains(diagnostic.NextActions[1].SQL, "GRANT SELECT") {
		t.Fatalf("next actions of the refused role = %#v", diagnostic.NextActions)
	}
	afterGuard := captureStdout(t, func() int {
		return runDriftAt(append([]string{"check", "--target", "primary", "--config", "no-scratch.toml", "--database", urlFor("reader")}, ignores...), repository)
	})
	if afterGuard.code != 1 || !strings.Contains(afterGuard.stdout, `"code":"source_error"`) || !strings.Contains(afterGuard.stdout, "replay expected history") {
		t.Fatalf("valid role without a scratch server = %d, %s", afterGuard.code, afterGuard.stdout)
	}
}
