package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/source"
)

const earthDistanceSchema = `CREATE SCHEMA IF NOT EXISTS "extensions";
CREATE EXTENSION IF NOT EXISTS "cube" WITH SCHEMA "extensions";
CREATE EXTENSION IF NOT EXISTS "earthdistance" WITH SCHEMA "extensions";
SET search_path = public, extensions;
CREATE TABLE public.place (id bigint PRIMARY KEY, lat double precision NOT NULL, lon double precision NOT NULL);
CREATE INDEX place_location_idx ON public.place USING gist (extensions.ll_to_earth(lat, lon));
`

// earthDistanceExtensionsOnly omits the expression index: a generated bundle
// carries no search_path, which PostgreSQL 15 needs to resolve earth.
const earthDistanceExtensionsOnly = `CREATE SCHEMA IF NOT EXISTS "extensions";
CREATE EXTENSION IF NOT EXISTS "cube" WITH SCHEMA "extensions";
CREATE EXTENSION IF NOT EXISTS "earthdistance" WITH SCHEMA "extensions";
CREATE TABLE public.place (id bigint PRIMARY KEY, lat double precision NOT NULL, lon double precision NOT NULL);
`

func TestScratchAdminExtensionsAcrossTheLifecycleOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var untrusted bool
	if err := admin.QueryRow(ctx, `
SELECT v.superuser AND NOT v.trusted
FROM pg_available_extensions a JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = a.default_version
WHERE a.name = 'earthdistance'`).Scan(&untrusted); err != nil || !untrusted {
		t.Skipf("earthdistance is not an untrusted extension on this server (%v)", err)
	}
	repository := t.TempDir()
	const setting = `scratch_admin_extensions = [{ name = "earthdistance", schema = "extensions" }]`
	config := func(extra string) string {
		return `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
` + extra + "\n"
	}
	writeTestFile(t, repository, "schema.sql", earthDistanceSchema)

	t.Run("without the setting the refusal names it", func(t *testing.T) {
		writeTestFile(t, repository, ".onwardpg.toml", config(""))
		for label, run := range map[string]func() int{
			"config check": func() int {
				return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
			},
			"init": func() int { return runInitAt(nil, repository) },
		} {
			result := captureStdout(t, run)
			if result.code == 0 || !strings.Contains(result.stdout, `permission denied to create extension`) || !strings.Contains(result.stdout, "scratch_admin_extensions") {
				t.Fatalf("%s: %d %s", label, result.code, result.stdout)
			}
		}
		if _, err := os.Stat(filepath.Join(repository, "onward-bundles/primary/baseline/manifest.json")); !os.IsNotExist(err) {
			t.Fatalf("a refused init wrote a bundle: %v", err)
		}
	})

	writeTestFile(t, repository, ".onwardpg.toml", config(setting))
	t.Run("config check reports what the administrator installed", func(t *testing.T) {
		result := captureStdout(t, func() int {
			return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
		})
		if result.code != 0 || !strings.Contains(result.stdout, `"scratch_admin_installed":["cube","earthdistance"]`) ||
			!strings.Contains(result.stdout, `"scratch_admin_extensions":[{"name":"earthdistance","schema":"extensions"}]`) {
			t.Fatalf("config check: %d %s", result.code, result.stdout)
		}
	})
	t.Run("config check rejects an extension the server lacks", func(t *testing.T) {
		writeTestFile(t, repository, ".onwardpg.toml", config(`scratch_admin_extensions = [{ name = "not_a_real_extension", schema = "public" }]`))
		defer writeTestFile(t, repository, ".onwardpg.toml", config(setting))
		result := captureStdout(t, func() int {
			return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
		})
		if result.code == 0 || !strings.Contains(result.stdout, "does not provide") {
			t.Fatalf("config check accepted a missing extension: %d %s", result.code, result.stdout)
		}
	})
	t.Run("config check notes a trusted extension instead of failing", func(t *testing.T) {
		writeTestFile(t, repository, ".onwardpg.toml", config(`scratch_admin_extensions = [{ name = "earthdistance", schema = "extensions" }, { name = "cube", schema = "extensions" }]`))
		defer writeTestFile(t, repository, ".onwardpg.toml", config(setting))
		result := captureStdout(t, func() int {
			return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
		})
		if result.code != 0 || !strings.Contains(result.stdout, `"notes":["scratch_admin_extensions entry \"cube\" is not needed`) {
			t.Fatalf("config check: %d %s", result.code, result.stdout)
		}
	})

	initialized := captureStdout(t, func() int { return runInitAt(nil, repository) })
	if initialized.code != 0 || !strings.Contains(initialized.stdout, `"status":"verified"`) {
		t.Fatalf("init: %d %s", initialized.code, initialized.stdout)
	}
	baseline, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/baseline"))
	if err != nil {
		t.Fatal(err)
	}
	receipted := baseline.Manifest.Planner.ScratchAdminExtensions
	if len(receipted) != 1 || receipted[0].Name != "earthdistance" || receipted[0].Schema != "extensions" {
		t.Fatalf("allowlist was not receipted: %#v", baseline.Manifest.Planner)
	}

	t.Run("receipted fingerprint equals the owner-created catalog", func(t *testing.T) {
		reference, cleanup := createTestDatabase(t, adminURL)
		defer cleanup()
		connection, err := pgx.Connect(ctx, reference)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(ctx)
		if _, err := connection.Exec(ctx, earthDistanceSchema); err != nil {
			t.Fatal(err)
		}
		config, err := pgx.ParseConfig(reference)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := source.LoadDatabaseGraphForComparison(ctx, config, nil)
		if err != nil {
			t.Fatal(err)
		}
		want, err := graph.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if baseline.Manifest.DesiredSource.Fingerprint != want {
			t.Fatalf("receipted desired fingerprint %s != owner-created %s", baseline.Manifest.DesiredSource.Fingerprint, want)
		}
		// The same live database is drift-free against the replayed history: the
		// replay goes through the scratch administrator path.
		drift := captureStdout(t, func() int {
			return runDriftAt([]string{"check", "--database", reference}, repository)
		})
		if drift.code != 0 || !strings.Contains(drift.stdout, `"status":"drift_free"`) {
			t.Fatalf("drift check: %d %s", drift.code, drift.stdout)
		}
	})

	verify := func() captured {
		return captureStdout(t, func() int { return runVerifyAt([]string{"--bundle", "baseline", "--check"}, repository) })
	}
	t.Run("verify --check reuses the receipted allowlist", func(t *testing.T) {
		if result := verify(); result.code != 0 || !strings.Contains(result.stdout, `"status":"verified"`) {
			t.Fatalf("verify: %d %s", result.code, result.stdout)
		}
	})
	t.Run("verify --check reports a configuration that differs from the receipt", func(t *testing.T) {
		for label, extra := range map[string]string{
			"removed":  "",
			"widened":  `scratch_admin_extensions = [{ name = "earthdistance", schema = "extensions" }, { name = "dblink", schema = "public" }]`,
			"reschema": `scratch_admin_extensions = [{ name = "earthdistance", schema = "public" }]`,
		} {
			writeTestFile(t, repository, ".onwardpg.toml", config(extra))
			result := verify()
			if result.code == 0 || !strings.Contains(result.stdout, `"code":"scratch_admin_extensions_changed"`) {
				t.Fatalf("%s: %d %s", label, result.code, result.stdout)
			}
		}
		// Only a read-only check of one bundle compares the two. Drift check replays
		// each accepted bundle under its own receipt, so the changed configuration
		// does not matter to it.
		live, cleanup := createTestDatabase(t, adminURL)
		defer cleanup()
		connection, err := pgx.Connect(ctx, live)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(ctx)
		if _, err := connection.Exec(ctx, earthDistanceSchema); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, repository, ".onwardpg.toml", config(""))
		drift := captureStdout(t, func() int { return runDriftAt([]string{"check", "--database", live}, repository) })
		if drift.code != 0 || !strings.Contains(drift.stdout, `"status":"drift_free"`) {
			t.Fatalf("drift check under a changed configuration: %d %s", drift.code, drift.stdout)
		}
		writeTestFile(t, repository, ".onwardpg.toml", config(setting))
	})

	t.Run("a generated bundle verifies when the planner emits idempotent statements", func(t *testing.T) {
		// Start from an extension-free history in a second repository so the next
		// bundle itself must create the untrusted extension.
		second := t.TempDir()
		writeTestFile(t, second, ".onwardpg.toml", config(setting+"\nignore = [\"domain:extensions.earth\"]"))
		writeTestFile(t, second, "schema.sql", `CREATE TABLE public.place (id bigint PRIMARY KEY, lat double precision NOT NULL, lon double precision NOT NULL);`)
		if result := captureStdout(t, func() int { return runInitAt(nil, second) }); result.code != 0 {
			t.Fatalf("init: %d %s", result.code, result.stdout)
		}
		writeTestFile(t, second, "schema.sql", earthDistanceExtensionsOnly)
		bare := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"add-geo"}, second) })
		if bare.code == 0 || !strings.Contains(bare.stdout, "--if-not-exists") {
			t.Fatalf("a bare CREATE EXTENSION must fail with a hint naming --if-not-exists: %d %s", bare.code, bare.stdout)
		}
		planned := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"add-geo", "--if-not-exists"}, second) })
		if planned.code != 0 {
			t.Fatalf("plan: %d %s", planned.code, planned.stdout)
		}
		verified := captureStdout(t, func() int { return runVerifyAt([]string{"--bundle", "add-geo", "--check"}, second) })
		if verified.code != 0 || !strings.Contains(verified.stdout, `"status":"verified"`) {
			t.Fatalf("verify: %d %s", verified.code, verified.stdout)
		}
		artifact, err := bundle.Read(filepath.Join(second, "onward-bundles/primary/add-geo"))
		if err != nil {
			t.Fatal(err)
		}
		if got := artifact.Manifest.Planner.ScratchAdminExtensions; len(got) != 1 || got[0].Name != "earthdistance" {
			t.Fatalf("generated bundle did not receipt the allowlist: %#v", artifact.Manifest.Planner)
		}
	})

	t.Run("diff honors --scratch-admin-extension", func(t *testing.T) {
		from := filepath.Join(repository, "empty.sql")
		writeTestFile(t, repository, "empty.sql", "")
		to := filepath.Join(repository, "schema.sql")
		without := captureStdout(t, func() int {
			return runLowLevelPlan("diff", []string{"--from", "file://" + from, "--to", "file://" + to, "--dev-url", adminURL})
		})
		if without.code == 0 || !strings.Contains(without.stdout, "scratch_admin_extensions") {
			t.Fatalf("diff without the flag: %d %s", without.code, without.stdout)
		}
		with := captureStdout(t, func() int {
			return runLowLevelPlan("diff", []string{"--from", "file://" + from, "--to", "file://" + to, "--dev-url", adminURL, "--scratch-admin-extension", "earthdistance=extensions", "--if-not-exists"})
		})
		if with.code != 0 || !strings.Contains(with.stdout, "CREATE EXTENSION IF NOT EXISTS") {
			t.Fatalf("diff with the flag: %d %s", with.code, with.stdout)
		}
		bad := captureStdout(t, func() int {
			return runLowLevelPlan("diff", []string{"--from", "file://" + from, "--to", "file://" + to, "--dev-url", adminURL, "--scratch-admin-extension", "earthdistance"})
		})
		if bad.code == 0 || !strings.Contains(bad.stdout, "NAME=SCHEMA") {
			t.Fatalf("diff with a malformed flag: %d %s", bad.code, bad.stdout)
		}
	})
}

// Accepted bundles replay under the allowlist each one receipted. Changing the
// configuration later must not break the replay of an old bundle.
func TestHistoryReplaysEachBundleUnderItsOwnAllowlistOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	for _, name := range []string{"pg_prewarm", "pgstattuple"} {
		var untrusted bool
		if err := admin.QueryRow(ctx, `
SELECT v.superuser AND NOT v.trusted
FROM pg_available_extensions a JOIN pg_available_extension_versions v ON v.name = a.name AND v.version = a.default_version
WHERE a.name = $1`, name).Scan(&untrusted); err != nil || !untrusted {
			t.Skipf("%s is not an untrusted extension on this server (%v)", name, err)
		}
	}
	const table = `CREATE TABLE public.place (id bigint PRIMARY KEY, lat double precision NOT NULL, lon double precision NOT NULL);
`
	const first = `CREATE SCHEMA IF NOT EXISTS "tools";
CREATE EXTENSION IF NOT EXISTS "pg_prewarm" WITH SCHEMA "tools";
`
	const second = `CREATE EXTENSION IF NOT EXISTS "pgstattuple" WITH SCHEMA "tools";
`
	const firstEntry = `{ name = "pg_prewarm", schema = "tools" }`
	const secondEntry = `{ name = "pgstattuple", schema = "tools" }`
	repository := t.TempDir()
	config := func(entries ...string) string {
		list := ""
		if len(entries) > 0 {
			list = "scratch_admin_extensions = [" + strings.Join(entries, ", ") + "]\n"
		}
		return `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
` + list
	}
	run := func(name string, call func() int) captured {
		t.Helper()
		result := captureStdout(t, call)
		if result.code != 0 {
			t.Fatalf("%s: %d %s", name, result.code, result.stdout)
		}
		return result
	}

	writeTestFile(t, repository, ".onwardpg.toml", config(firstEntry))
	writeTestFile(t, repository, "schema.sql", first+table)
	run("init", func() int { return runInitAt(nil, repository) })

	t.Run("a later bundle adds a second allowlisted extension", func(t *testing.T) {
		writeTestFile(t, repository, ".onwardpg.toml", config(firstEntry, secondEntry))
		writeTestFile(t, repository, "schema.sql", first+second+table)
		run("plan", func() int { return runWorkflowPlanAt([]string{"add-second", "--if-not-exists"}, repository) })
		run("verify --check", func() int { return runVerifyAt([]string{"--bundle", "add-second", "--check"}, repository) })
		artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/add-second"))
		if err != nil {
			t.Fatal(err)
		}
		if got := artifact.Manifest.Planner.ScratchAdminExtensions; len(got) != 2 {
			t.Fatalf("second bundle receipt = %#v", got)
		}
		baseline, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/baseline"))
		if err != nil {
			t.Fatal(err)
		}
		if got := baseline.Manifest.Planner.ScratchAdminExtensions; len(got) != 1 || got[0].Name != "pg_prewarm" {
			t.Fatalf("the accepted baseline's own receipt changed: %#v", got)
		}
	})

	t.Run("removing the extensions from DDL and configuration still plans and verifies the drop", func(t *testing.T) {
		// Neither the DDL nor the configuration mentions either extension now, yet
		// the accepted bundles that created them must replay under their receipts.
		// The previous plan counts as merged: forget the local active-plan anchor.
		if err := os.RemoveAll(filepath.Join(repository, ".onwardpg")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, repository, ".onwardpg.toml", config())
		writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA IF NOT EXISTS "tools";`+"\n"+table)
		run("plan drop", func() int {
			return runWorkflowPlanAt([]string{"drop-extensions",
				"--hint", `{"kind":"drop","object":"extension","name":["tools","pg_prewarm"]}`,
				"--hint", `{"kind":"drop","object":"extension","name":["tools","pgstattuple"]}`}, repository)
		})
		run("verify --check", func() int { return runVerifyAt([]string{"--bundle", "drop-extensions", "--check"}, repository) })
		artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/drop-extensions"))
		if err != nil {
			t.Fatal(err)
		}
		if got := artifact.Manifest.Planner.ScratchAdminExtensions; len(got) != 0 {
			t.Fatalf("the drop bundle must receipt the configuration it ran under (none), got %#v", got)
		}
		phase := string(artifact.Files[artifact.Manifest.Phases["expand"].Path]) + string(artifact.Files[artifact.Manifest.Phases["contract"].Path])
		if !strings.Contains(phase, `DROP EXTENSION "pg_prewarm"`) || !strings.Contains(phase, `DROP EXTENSION "pgstattuple"`) {
			t.Fatalf("the drop bundle does not drop the extensions:\n%s", phase)
		}
		// Drift check also replays the whole history without the configuration.
		live, cleanup := createTestDatabase(t, adminURL)
		defer cleanup()
		connection, err := pgx.Connect(ctx, live)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(ctx)
		if _, err := connection.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS "tools";`+"\n"+table); err != nil {
			t.Fatal(err)
		}
		drift := captureStdout(t, func() int { return runDriftAt([]string{"check", "--database", live}, repository) })
		if drift.code != 0 || !strings.Contains(drift.stdout, `"status":"drift_free"`) {
			t.Fatalf("drift check: %d %s", drift.code, drift.stdout)
		}
	})
}

// The version in the allowlist entry, not a VERSION clause in project DDL,
// decides what the administrator installs.
func TestScratchAdminExtensionVersionFlagOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, "empty.sql", "")
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA IF NOT EXISTS tools;\nCREATE EXTENSION IF NOT EXISTS pg_prewarm WITH SCHEMA tools VERSION '1.1';\n")
	diff := func(flag string) captured {
		return captureStdout(t, func() int {
			return runLowLevelPlan("diff", []string{"--from", "file://" + filepath.Join(repository, "empty.sql"), "--to", "file://" + filepath.Join(repository, "schema.sql"), "--dev-url", adminURL, "--scratch-admin-extension", flag, "--output", "text"})
		})
	}
	pinned := diff("pg_prewarm=tools@1.1")
	if pinned.code != 0 || !strings.Contains(pinned.stdout, `VERSION '1.1'`) {
		t.Fatalf("pinned: %d %s", pinned.code, pinned.stdout)
	}
	unpinned := diff("pg_prewarm=tools")
	if unpinned.code != 0 || strings.Contains(unpinned.stdout, `VERSION '1.1'`) || !strings.Contains(unpinned.stdout, `VERSION '1.2'`) {
		t.Fatalf("without a version the default is installed, whatever the DDL says: %d %s", unpinned.code, unpinned.stdout)
	}
	if bad := diff("pg_prewarm=tools@1 1"); bad.code == 0 {
		t.Fatalf("a malformed version was accepted: %s", bad.stdout)
	}
}
