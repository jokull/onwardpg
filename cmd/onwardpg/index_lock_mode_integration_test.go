package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

const indexLockModeBaseSchema = `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, email text NOT NULL, org bigint, name text);
CREATE INDEX accounts_org_idx ON app.accounts (org);
`

type indexPlanResult struct {
	Status  string `json:"status"`
	Durable struct {
		Status        string                  `json:"status"`
		Generation    int                     `json:"generation"`
		IndexLockMode *protocol.IndexLockMode `json:"index_lock_mode"`
		Findings      []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"findings"`
	} `json:"durable"`
	NextActions []workflowNextAction `json:"next_actions"`
	Warnings    []protocol.Warning   `json:"warnings"`
}

func (r indexPlanResult) warningCodes() []string {
	codes := make([]string, 0, len(r.Warnings))
	for _, warning := range r.Warnings {
		codes = append(codes, warning.Code)
	}
	return codes
}

func indexLockModeRepository(t *testing.T, targetLines string) string {
	t.Helper()
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`+targetLines)
	writeTestFile(t, repository, "schema.sql", indexLockModeBaseSchema)
	return repository
}

func runIndexPlan(t *testing.T, repository string, wantCode int, arguments ...string) indexPlanResult {
	t.Helper()
	output := captureStdout(t, func() int {
		return runWorkflowPlanAt(append(arguments, "--target", "primary"), repository)
	})
	if output.code != wantCode {
		t.Fatalf("plan %v = %d, want %d: %s", arguments, output.code, wantCode, output.stdout)
	}
	var result indexPlanResult
	if err := json.Unmarshal([]byte(output.stdout), &result); err != nil {
		t.Fatalf("plan %v: %v\n%s", arguments, err, output.stdout)
	}
	return result
}

func bundlePhaseSQL(t *testing.T, repository, bundleID string) string {
	t.Helper()
	var sql strings.Builder
	for _, phase := range []string{"expand.sql", "contract.sql"} {
		data, err := os.ReadFile(filepath.Join(repository, "onward-bundles", "primary", bundleID, "phases", phase))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		sql.Write(data)
	}
	return sql.String()
}

func storedConcurrentIndexes(t *testing.T, repository, bundleID string) bool {
	t.Helper()
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", bundleID))
	if err != nil {
		t.Fatal(err)
	}
	return artifact.Manifest.Planner.Options.ConcurrentIndexes
}

func requireIndexLockMode(t *testing.T, step string, result indexPlanResult, concurrent bool, source string, warnings ...string) {
	t.Helper()
	mode := result.Durable.IndexLockMode
	if result.Durable.Status != "planned" || mode == nil || mode.ConcurrentIndexes != concurrent || mode.Source != source {
		t.Fatalf("%s: durable = %#v, mode = %#v, want concurrent=%t from %s", step, result.Durable, mode, concurrent, source)
	}
	if strings.Join(result.warningCodes(), ",") != strings.Join(warnings, ",") {
		t.Fatalf("%s: warnings = %#v, want codes %v", step, result.Warnings, warnings)
	}
}

func requireSQLMode(t *testing.T, step, sql string, concurrent bool) {
	t.Helper()
	hasConcurrent := strings.Contains(sql, "CONCURRENTLY") || strings.Contains(sql, "-- onwardpg:batch nontransactional")
	hasPlain := strings.Contains(sql, "CREATE INDEX \"") || strings.Contains(sql, "DROP INDEX \"")
	if concurrent && (hasPlain || !strings.Contains(sql, "CREATE INDEX CONCURRENTLY") || !strings.Contains(sql, "DROP INDEX CONCURRENTLY")) {
		t.Fatalf("%s: SQL is not concurrent:\n%s", step, sql)
	}
	if !concurrent && (hasConcurrent || !hasPlain) {
		t.Fatalf("%s: SQL is not plain:\n%s", step, sql)
	}
}

// The lock mode of a bundle is the flag of the run, then the choice that the
// bundle stored, then the target configuration. A restack with a plain `plan`
// keeps it, and every change is a warning in the result.
func TestIndexLockModeFollowsFlagThenBundleThenConfigOnPostgreSQL(t *testing.T) {
	if os.Getenv("ONWARDPG_TEST_DATABASE_URL") == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := indexLockModeRepository(t, "")
	if output := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); output.code != 0 {
		t.Fatalf("init = %d: %s", output.code, output.stdout)
	}
	schema := `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, email text NOT NULL, org bigint, name text);
CREATE INDEX accounts_name_idx ON app.accounts (name);
`
	writeTestFile(t, repository, "schema.sql", schema)

	// One index is dropped and one is created. The drop asks no decision.
	result := runIndexPlan(t, repository, 0, "indexes", "--concurrent-indexes")
	requireIndexLockMode(t, "first plan with the flag", result, true, "flag")
	requireSQLMode(t, "first plan with the flag", bundlePhaseSQL(t, repository, "indexes"), true)
	if !storedConcurrentIndexes(t, repository, "indexes") {
		t.Fatal("the manifest did not store the concurrent choice")
	}

	// The documented restack is a plain plan. It keeps the mode.
	schema += "CREATE INDEX accounts_email_idx ON app.accounts (email);\n"
	writeTestFile(t, repository, "schema.sql", schema)
	result = runIndexPlan(t, repository, 0)
	requireIndexLockMode(t, "plain restack", result, true, "bundle")
	requireSQLMode(t, "plain restack", bundlePhaseSQL(t, repository, "indexes"), true)
	if result.Durable.Generation != 2 || !storedConcurrentIndexes(t, repository, "indexes") {
		t.Fatalf("plain restack: generation %d, stored %t", result.Durable.Generation, storedConcurrentIndexes(t, repository, "indexes"))
	}

	// The flag overrides the stored choice, and the result says so.
	result = runIndexPlan(t, repository, 0, "--concurrent-indexes=false")
	requireIndexLockMode(t, "explicit false", result, false, "flag", protocol.IndexLockModeChangedCode)
	requireSQLMode(t, "explicit false", bundlePhaseSQL(t, repository, "indexes"), false)
	if mode := result.Durable.IndexLockMode; mode.PreviousConcurrentIndexes == nil || !*mode.PreviousConcurrentIndexes {
		t.Fatalf("explicit false: mode = %#v, want the previous mode", mode)
	}
	if !strings.Contains(result.Warnings[0].Message, "blocking") || storedConcurrentIndexes(t, repository, "indexes") {
		t.Fatalf("explicit false: warning = %#v", result.Warnings[0])
	}

	// The stored blocking choice also stays, without a warning.
	result = runIndexPlan(t, repository, 0)
	requireIndexLockMode(t, "plain plan after explicit false", result, false, "bundle")

	// The configuration does not change an existing bundle. The result says
	// that the bundle stays blocking against the configuration.
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
concurrent_indexes = true
`)
	result = runIndexPlan(t, repository, 0)
	requireIndexLockMode(t, "config set after the bundle", result, false, "bundle", protocol.IndexLockModeDiffersFromConfigCode)
	requireSQLMode(t, "config set after the bundle", bundlePhaseSQL(t, repository, "indexes"), false)

	// One run with the flag moves the bundle, with a warning for the change.
	result = runIndexPlan(t, repository, 0, "--concurrent-indexes")
	requireIndexLockMode(t, "flag after config", result, true, "flag", protocol.IndexLockModeChangedCode)
	requireSQLMode(t, "flag after config", bundlePhaseSQL(t, repository, "indexes"), true)
	result = runIndexPlan(t, repository, 0)
	requireIndexLockMode(t, "plain plan in the configured mode", result, true, "bundle")

	if output := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "indexes", "--check"}, repository)
	}); output.code != 0 || !strings.Contains(output.stdout, `"status":"verified"`) {
		t.Fatalf("verify --check = %d: %s", output.code, output.stdout)
	}
}

// With concurrent_indexes = true a new bundle is concurrent without the flag,
// and --concurrent-indexes=false overrides the configuration. init does not
// read the key.
func TestConfiguredConcurrentIndexesApplyToPlanAndNotToInitOnPostgreSQL(t *testing.T) {
	if os.Getenv("ONWARDPG_TEST_DATABASE_URL") == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	changed := `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, email text NOT NULL, org bigint, name text);
CREATE INDEX accounts_name_idx ON app.accounts (name);
`
	repository := indexLockModeRepository(t, "concurrent_indexes = true\n")
	if output := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); output.code != 0 {
		t.Fatalf("init = %d: %s", output.code, output.stdout)
	}
	if baseline := bundlePhaseSQL(t, repository, "baseline"); strings.Contains(baseline, "CONCURRENTLY") || storedConcurrentIndexes(t, repository, "baseline") {
		t.Fatalf("a baseline must not read concurrent_indexes:\n%s", baseline)
	}
	writeTestFile(t, repository, "schema.sql", changed)
	result := runIndexPlan(t, repository, 0, "configured")
	requireIndexLockMode(t, "new bundle with the configuration", result, true, "config")
	requireSQLMode(t, "new bundle with the configuration", bundlePhaseSQL(t, repository, "configured"), true)

	repository = indexLockModeRepository(t, "concurrent_indexes = true\n")
	if output := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--concurrent-indexes"}, repository)
	}); output.code != 0 {
		t.Fatalf("init --concurrent-indexes = %d: %s", output.code, output.stdout)
	}
	// A baseline is the exported DDL as written; the flag is only recorded.
	if !storedConcurrentIndexes(t, repository, "baseline") {
		t.Fatal("init --concurrent-indexes must record the flag")
	}
	writeTestFile(t, repository, "schema.sql", changed)
	result = runIndexPlan(t, repository, 0, "overridden", "--concurrent-indexes=false")
	requireIndexLockMode(t, "flag against the configuration", result, false, "flag")
	requireSQLMode(t, "flag against the configuration", bundlePhaseSQL(t, repository, "overridden"), false)
	// The stored choice of the flag stays; the result repeats the difference.
	result = runIndexPlan(t, repository, 0)
	requireIndexLockMode(t, "restack of the override", result, false, "bundle", protocol.IndexLockModeDiffersFromConfigCode)
}

// A hint that an earlier version asked for and that no decision needs now is
// accepted. Decisions with one choice each are offered as one hints file.
func TestIndexDropHintsAreNotNeededAndSingleChoiceDecisionsShareOneFileOnPostgreSQL(t *testing.T) {
	if os.Getenv("ONWARDPG_TEST_DATABASE_URL") == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := indexLockModeRepository(t, "")
	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, email text NOT NULL, org bigint, name text, legacy_a text, legacy_b text);
CREATE INDEX accounts_org_idx ON app.accounts (org);
CREATE UNIQUE INDEX accounts_id_copy_idx ON app.accounts (id);
CREATE UNIQUE INDEX accounts_email_idx ON app.accounts (email);
`)
	if output := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); output.code != 0 {
		t.Fatalf("init = %d: %s", output.code, output.stdout)
	}
	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY, email text NOT NULL, org bigint, name text);
`)
	dropHint := func(object, name string) string {
		return `{"kind":"drop","object":"` + object + `","name":["app","accounts","` + name + `"]}`
	}
	// Four decisions stay: two columns (data loss) and the two unique indexes
	// (enforcement). The plain index asks nothing.
	pending := runIndexPlan(t, repository, 2, "cleanup")
	var single []workflowNextAction
	var file *workflowNextAction
	for index, action := range pending.NextActions {
		switch action.Kind {
		case "semantic_hint":
			single = append(single, action)
		case "semantic_hints_file":
			file = &pending.NextActions[index]
		}
	}
	if len(single) != 4 || file == nil || file.DecisionCount != 4 || len(file.Hints) != 4 {
		t.Fatalf("next actions = %#v", pending.NextActions)
	}
	hazards := map[string][]string{}
	for _, action := range single {
		hazards[action.Choices[0].Hint.Name[2]] = action.Choices[0].Hazards
	}
	if strings.Join(hazards["legacy_a"], ",") != "data_loss" || strings.Join(hazards["legacy_b"], ",") != "data_loss" ||
		strings.Join(hazards["accounts_email_idx"], ",") != "unique_index_enforcement_removed,duplicate_rows_possible" ||
		strings.Join(hazards["accounts_id_copy_idx"], ",") != "unique_index_enforcement_removed,duplicate_rows_possible" {
		t.Fatalf("decision hazards = %#v", hazards)
	}
	if strings.Join(file.Hazards, ",") != "data_loss,duplicate_rows_possible,unique_index_enforcement_removed" ||
		strings.Join(file.Argv, " ") != "onwardpg plan --target primary --hints-file "+hintsFileName {
		t.Fatalf("hints file action = %#v", file)
	}
	encoded, err := json.Marshal(file.Hints)
	if err != nil {
		t.Fatal(err)
	}
	hintsPath := filepath.Join(repository, hintsFileName)
	if err := os.WriteFile(hintsPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// The file answers every decision. The extra hint is one that preview.7
	// asked for; it is accepted and reported.
	planned := runIndexPlan(t, repository, 0, "--hints-file", hintsPath, "--hint", dropHint("index", "accounts_org_idx"))
	if planned.Durable.Status != "planned" {
		t.Fatalf("planned = %#v", planned)
	}
	notNeeded := 0
	for _, finding := range planned.Durable.Findings {
		if finding.Code == "hint_not_needed" {
			notNeeded++
		}
	}
	if notNeeded != 1 {
		t.Fatalf("findings = %#v, want one hint_not_needed", planned.Durable.Findings)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := bundle.SemanticHints(artifact)
	if err != nil || len(stored) != 4 {
		t.Fatalf("stored hints = %#v (%v), want only the four that answer a decision", stored, err)
	}
	var plan protocol.Result
	if err := json.Unmarshal(artifact.Files["plan.json"], &plan); err != nil {
		t.Fatal(err)
	}
	found := map[string]protocol.Statement{}
	for _, statement := range plan.Statements {
		found[statement.SQL] = statement
	}
	for sql, want := range map[string]struct{ phase, hazard, absent string }{
		`DROP INDEX "app"."accounts_org_idx";`:     {protocol.PhaseContract, "slower_queries_possible", "data_loss"},
		`DROP INDEX "app"."accounts_id_copy_idx";`: {protocol.PhaseExpand, "unique_index_enforcement_removed", "data_loss"},
		`DROP INDEX "app"."accounts_email_idx";`:   {protocol.PhaseExpand, "unique_index_enforcement_removed", "data_loss"},
	} {
		statement, exists := found[sql]
		if !exists || statement.Phase != want.phase || statement.Safety != "review" {
			t.Fatalf("%s = %#v", sql, statement)
		}
		if strings.Join(statement.Hazards, ",") == "" || !strings.Contains(","+strings.Join(statement.Hazards, ",")+",", ","+want.hazard+",") ||
			strings.Contains(","+strings.Join(statement.Hazards, ",")+",", ","+want.absent+",") {
			t.Fatalf("%s hazards = %v", sql, statement.Hazards)
		}
	}
	column := found[`ALTER TABLE "app"."accounts" DROP COLUMN "legacy_a";`]
	if column.Safety != "dangerous" || !strings.Contains(strings.Join(column.Hazards, ","), "data_loss") {
		t.Fatalf("a column drop must stay dangerous data loss: %#v", column)
	}
}

// A bundle that preview.7 wrote is accepted history for this build: it
// verifies with no change to a byte, and a restack of the bundle itself keeps
// its stored lock mode and the answers that still have a question.
func TestPreview7BundleVerifiesUnchangedAndRestacksOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	// The fixture was planned on PostgreSQL 18. History is bound to its major.
	if major, err := source.PostgresMajor(context.Background(), url); err != nil {
		t.Fatal(err)
	} else if major != 18 {
		t.Skipf("the preview.7 fixture is PostgreSQL 18 history; the server is PostgreSQL %d", major)
	}
	fixture := filepath.Join("testdata", "preview7-index-bundle")
	original := map[string][]byte{}
	copyFixture := func() string {
		repository := t.TempDir()
		err := filepath.WalkDir(fixture, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			relative, err := filepath.Rel(fixture, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if relative == "onwardpg.toml" {
				relative = ".onwardpg.toml"
			} else if strings.HasPrefix(relative, "onward-bundles") {
				original[relative] = data
			}
			writeTestFile(t, repository, relative, string(data))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return repository
	}
	requireUnchanged := func(step, repository string, directories ...string) {
		t.Helper()
		for relative, want := range original {
			selected := false
			for _, directory := range directories {
				selected = selected || strings.HasPrefix(relative, filepath.Join("onward-bundles", "primary", directory)+string(filepath.Separator))
			}
			if !selected {
				continue
			}
			got, err := os.ReadFile(filepath.Join(repository, relative))
			if err != nil || string(got) != string(want) {
				t.Fatalf("%s: %s changed (%v)", step, relative, err)
			}
		}
	}

	// Accepted history: verify changes nothing.
	repository := copyFixture()
	if len(original) != 12 {
		t.Fatalf("fixture has %d bundle files, want 12", len(original))
	}
	for _, bundleID := range []string{"drop-indexes"} {
		output := captureStdout(t, func() int {
			return runVerifyAt([]string{"--target", "primary", "--bundle", bundleID, "--check"}, repository)
		})
		if output.code != 0 || !strings.Contains(output.stdout, `"status":"verified"`) {
			t.Fatalf("verify --check %s = %d: %s", bundleID, output.code, output.stdout)
		}
	}
	requireUnchanged("verify --check", repository, "baseline", "drop-indexes")
	// In-flight bundle: a plain plan with this build restacks it. The stored
	// mode stays. The answers for index drops that need no decision now are
	// dropped without an error; the answers for the unique indexes stay.
	repository = copyFixture()
	schema, err := os.ReadFile(filepath.Join(repository, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", string(schema)+"CREATE INDEX accounts_name_org_idx ON app.accounts (name, org);\n")
	restacked := runIndexPlan(t, repository, 0, "--bundle", "drop-indexes")
	requireIndexLockMode(t, "restack of a preview.7 bundle", restacked, true, "bundle")
	requireUnchanged("restack of a preview.7 bundle", repository, "baseline")
	sql := bundlePhaseSQL(t, repository, "drop-indexes")
	requireSQLMode(t, "restack of a preview.7 bundle", sql, true)
	for _, statement := range []string{
		`DROP INDEX CONCURRENTLY "app"."accounts_org_idx";`, `DROP INDEX CONCURRENTLY "app"."accounts_name_idx";`,
		`DROP INDEX CONCURRENTLY "app"."accounts_id_copy_idx";`, `DROP INDEX CONCURRENTLY "app"."accounts_email_idx";`,
		`CREATE INDEX CONCURRENTLY "accounts_name_org_idx"`,
	} {
		if !strings.Contains(sql, statement) {
			t.Fatalf("restack lost %s:\n%s", statement, sql)
		}
	}
	if strings.Contains(sql, "data_loss") {
		t.Fatalf("restacked index drops are still data loss:\n%s", sql)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "drop-indexes"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := bundle.SemanticHints(artifact)
	if err != nil || len(stored) != 2 || stored[0].Name[2] != "accounts_email_idx" || stored[1].Name[2] != "accounts_id_copy_idx" {
		t.Fatalf("stored hints after the restack = %#v (%v)", stored, err)
	}
}

// PostgreSQL has no concurrent form for an index of a partitioned table. The
// concurrent mode builds a new one partition by partition and drops an old
// one with the plain statement; the bundle verifies in PostgreSQL.
func TestConcurrentIndexesOnPartitionedTableVerifyOnPostgreSQL(t *testing.T) {
	if os.Getenv("ONWARDPG_TEST_DATABASE_URL") == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	tables := `CREATE SCHEMA app;
CREATE TABLE app.events (id bigint, at timestamptz NOT NULL, kind text) PARTITION BY RANGE (at);
CREATE TABLE app.events_2026 PARTITION OF app.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE app.events_2027 PARTITION OF app.events FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');
`
	repository := indexLockModeRepository(t, "concurrent_indexes = true\n")
	writeTestFile(t, repository, "schema.sql", tables+"CREATE INDEX events_old_idx ON app.events (kind, id);\n")
	if output := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary"}, repository) }); output.code != 0 {
		t.Fatalf("init = %d: %s", output.code, output.stdout)
	}
	writeTestFile(t, repository, "schema.sql", tables+"CREATE INDEX events_kind_idx ON app.events (kind);\n")
	result := runIndexPlan(t, repository, 0, "partitioned")
	requireIndexLockMode(t, "partitioned table", result, true, "config")
	sql := bundlePhaseSQL(t, repository, "partitioned")
	for _, statement := range []string{
		`CREATE INDEX "events_kind_idx" ON ONLY "app"."events"`,
		`CREATE INDEX CONCURRENTLY "events_2026_kind_idx" ON "app"."events_2026"`,
		`CREATE INDEX CONCURRENTLY "events_2027_kind_idx" ON "app"."events_2027"`,
		`ALTER INDEX "app"."events_kind_idx" ATTACH PARTITION "app"."events_2027_kind_idx";`,
		`DROP INDEX "app"."events_old_idx";`,
		"partitioned_index_not_concurrent",
	} {
		if !strings.Contains(sql, statement) {
			t.Fatalf("missing %s:\n%s", statement, sql)
		}
	}
	if output := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "partitioned"}, repository)
	}); output.code != 0 || !strings.Contains(output.stdout, `"status":"verified"`) {
		t.Fatalf("verify = %d: %s", output.code, output.stdout)
	}
}
