package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/driftcheck"
)

func TestLiveObjectIgnoreAppliesOnlyToObservedCatalogOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	repository := t.TempDir()
	config := `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`
	writeTestFile(t, repository, ".onwardpg.toml", config)
	ddl := "CREATE TABLE public.trip (id bigint PRIMARY KEY); CREATE FUNCTION public.trip_capture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;"
	writeTestFile(t, repository, "schema.sql", ddl)
	initialized := captureStdout(t, func() int { return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository) })
	if initialized.code != 0 {
		t.Fatalf("init: %d %s", initialized.code, initialized.stdout)
	}
	connection, err := pgx.Connect(context.Background(), liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), ddl+" CREATE TRIGGER s1a_write BEFORE INSERT ON public.trip FOR EACH ROW EXECUTE FUNCTION public.trip_capture();"); err != nil {
		t.Fatal(err)
	}
	check := func() (int, driftcheck.Report) {
		t.Helper()
		output := captureStdout(t, func() int {
			return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
		})
		var report driftcheck.Report
		if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
			t.Fatalf("drift: %d %s: %v", output.code, output.stdout, err)
		}
		return output.code, report
	}
	code, report := check()
	if code != 4 || report.Outcome != "drifted" || len(report.Differences) != 1 || !strings.Contains(report.Differences[0].ObjectID, "s1a_write") {
		t.Fatalf("without live_ignore: %d %#v", code, report)
	}
	selector := "trigger:public.*.s1a_write"
	writeTestFile(t, repository, ".onwardpg.toml", config+"[targets.primary.ignore]\nlive = [\""+selector+"\"]\n")
	code, report = check()
	if code != 0 || report.Outcome != "drift_free" || report.Observer == nil || len(report.Observer.LiveIgnored) != 1 || report.Observer.LiveIgnored[0] != selector {
		t.Fatalf("with live_ignore: %d %#v", code, report)
	}
	verified := captureStdout(t, func() int { return runVerifyAt([]string{"--bundle", "baseline"}, repository) })
	if verified.code != 0 {
		t.Fatalf("verify: %d %s", verified.code, verified.stdout)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles/primary/baseline"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Manifest.Planner.IgnoreSelectors) != 0 || len(artifact.Manifest.Planner.ObserverIgnoreSelectors) != 0 {
		t.Fatalf("live selector entered planner boundary: %#v", artifact.Manifest.Planner)
	}
	writeTestFile(t, repository, "schema.sql", ddl+" CREATE TABLE public.extra (id bigint PRIMARY KEY);")
	planned := captureStdout(t, func() int { return runWorkflowPlanAt([]string{"add-extra"}, repository) })
	if planned.code != 0 {
		t.Fatalf("plan: %d %s", planned.code, planned.stdout)
	}
	artifact, err = bundle.Read(filepath.Join(repository, "onward-bundles/primary/add-extra"))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Manifest.Planner.IgnoreSelectors) != 0 || len(artifact.Manifest.Planner.ObserverIgnoreSelectors) != 0 {
		t.Fatalf("planned bundle captured live selector: %#v", artifact.Manifest.Planner)
	}
}
