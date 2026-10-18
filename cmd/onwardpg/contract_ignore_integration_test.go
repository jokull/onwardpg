package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/contractcheck"
)

// A cutover database holds objects that are not drift for the bundle: a schema
// of archive tables that the operator created, a column of another team's
// pending change, and on a restored copy foreign keys that the restore could
// not create. contract check --ignore applies the selectors of drift check
// --ignore to both sides of the checkpoint comparison. The expected side is
// the bundle's post-expand catalog, replayed in disposable PostgreSQL.
func TestContractCheckIgnoreAppliesSelectorsToBothSidesOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	t.Setenv("ONWARDPG_TEST_IGNORE_LIVE_URL", liveURL)
	live, err := pgx.Connect(ctx, liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	exec := func(sql string) {
		t.Helper()
		if _, err := live.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", nontransactionalHistoryConfig)
	const tables = "CREATE TABLE public.parent (id bigint PRIMARY KEY);\n" +
		"CREATE TABLE public.child (id bigint PRIMARY KEY, parent_id bigint CONSTRAINT child_parent_fkey REFERENCES public.parent (id));\n" +
		"CREATE TABLE public.audit (id bigint PRIMARY KEY, parent_id bigint CONSTRAINT \"audit_Mixed_fkey\" REFERENCES public.parent (id));\n"
	writeTestFile(t, repository, "schema.sql", tables+"CREATE TABLE public.draft (id bigint PRIMARY KEY, status text DEFAULT 'new');\n")
	if initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary"}, repository)
	}); initialized.code != 0 {
		t.Fatalf("init = %d, %s", initialized.code, initialized.stdout)
	}
	// Expand adds a column; contract drops a default, so the post-expand
	// checkpoint is not the desired catalog.
	writeTestFile(t, repository, "schema.sql", tables+"CREATE TABLE public.draft (id bigint PRIMARY KEY, status text, note text);\n")
	if planned := captureStdout(t, func() int {
		return runWorkflowPlanAt([]string{"draft-note", "--target", "primary"}, repository)
	}); planned.code != 0 {
		t.Fatalf("plan = %d, %s", planned.code, planned.stdout)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "draft-note"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifact.Files["phases/contract.sql"]), "DROP DEFAULT") {
		t.Fatalf("the plan has no contract DROP DEFAULT:\n%s", artifact.Files["phases/contract.sql"])
	}
	applyBundlePhase(t, live, repository, "baseline", "expand")
	applyBundlePhase(t, live, repository, "draft-note", "expand")

	manifest := artifact.Manifest
	now := time.Now().UTC()
	evidence := contractcheck.Evidence{
		Target: "primary", Environment: "test", PlanID: manifest.PlanID, BundleEntryDigest: manifest.History.EntryDigest,
		DesiredFingerprint: manifest.DesiredSource.Fingerprint, Generation: manifest.Generation, Release: "isolated-test",
		ObservedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	for _, category := range []string{"web", "workers", "scheduled_jobs", "queues", "connection_pools", "previews", "ad_hoc_writers"} {
		evidence.Cohorts = append(evidence.Cohorts, contractcheck.Cohort{Category: category, Name: category, Status: "isolated", SourceKind: "manual", Source: "disposable regression database"})
	}
	evidenceBody, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(t.TempDir(), "readiness.json")
	if err := os.WriteFile(evidencePath, evidenceBody, 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(ignores ...string) (captured, contractcheck.Report) {
		t.Helper()
		arguments := []string{"check", "--target", "primary", "--bundle", "draft-note", "--environment", "test",
			"--database-env", "ONWARDPG_TEST_IGNORE_LIVE_URL", "--evidence", evidencePath}
		for _, selector := range ignores {
			arguments = append(arguments, "--ignore", selector)
		}
		result := captureStdout(t, func() int { return runContractAt(arguments, repository) })
		var report contractcheck.Report
		_ = json.Unmarshal([]byte(result.stdout), &report)
		return result, report
	}
	if result, report := check(); result.code != 0 || report.Status != "ready" || report.Ignored != nil {
		t.Fatalf("clean post-expand catalog: %d %s", result.code, result.stdout)
	}

	exec(`CREATE SCHEMA legacy;
CREATE TABLE legacy.order_snapshot (id bigint PRIMARY KEY, parent_id bigint REFERENCES public.parent (id), body jsonb NOT NULL);
CREATE INDEX order_snapshot_parent_idx ON legacy.order_snapshot (parent_id);
CREATE SEQUENCE legacy.archive_seq;
CREATE VIEW legacy.order_snapshot_ids AS SELECT id FROM legacy.order_snapshot;
ALTER TABLE public.draft ADD COLUMN preview_token_hash bytea;
ALTER TABLE public.audit DROP CONSTRAINT "audit_Mixed_fkey";`)
	const (
		legacy = "schema:legacy"
		column = "column:public.draft.preview_token_hash"
		fkey   = "constraint:public.audit.audit_Mixed_fkey"
	)

	t.Run("without selectors the extras are catalog drift", func(t *testing.T) {
		result, report := check()
		if result.code != 4 || report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "catalog_drift" {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
	})

	t.Run("selectors on both sides make the check ready and are recorded", func(t *testing.T) {
		result, report := check(legacy, column, fkey)
		if result.code != 0 || report.Status != "ready" || report.Ignored == nil {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
		ignored := report.Ignored
		if ignored.ReplayedFingerprint != report.ExpectedFingerprint || ignored.ExpectedFingerprint != ignored.ActualFingerprint ||
			report.ActualFingerprint == ignored.ActualFingerprint {
			t.Fatalf("fingerprints: %s", result.stdout)
		}
		want := []contractcheck.IgnoredSelector{
			{Selector: column, Actual: []string{column}},
			{Selector: fkey, Expected: []string{fkey}},
			// The columns, constraints, and indexes of an ignored table have no
			// receipt of their own; they are in actual_masked.
			{Selector: legacy, Actual: []string{
				"schema:legacy", "sequence:legacy.archive_seq", "table:legacy.order_snapshot", "view:legacy.order_snapshot_ids",
			}},
		}
		if !reflect.DeepEqual(ignored.Selectors, want) {
			t.Fatalf("selectors = %#v", ignored.Selectors)
		}
		if !reflect.DeepEqual(ignored.ExpectedMasked, []string{"constraint:public:audit:audit_Mixed_fkey"}) {
			t.Fatalf("expected masked = %v", ignored.ExpectedMasked)
		}
		for _, id := range []string{
			"schema:legacy", "table:legacy:order_snapshot", "column:legacy:order_snapshot:body",
			"constraint:legacy:order_snapshot:order_snapshot_parent_id_fkey", "index:legacy:order_snapshot:order_snapshot_pkey",
			"sequence:legacy:archive_seq", "view:legacy:order_snapshot_ids", "column:public:draft:preview_token_hash",
		} {
			found := false
			for _, masked := range ignored.ActualMasked {
				found = found || masked == id
			}
			if !found {
				t.Fatalf("actual masked %v lacks %s", ignored.ActualMasked, id)
			}
		}
		for _, masked := range ignored.ActualMasked {
			if !strings.Contains(masked, ":legacy") && masked != "column:public:draft:preview_token_hash" {
				t.Fatalf("a selector masked an object it does not name: %s", masked)
			}
		}
	})

	t.Run("drift check takes the same selectors", func(t *testing.T) {
		output := captureStdout(t, func() int {
			return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL, "--ignore", legacy, "--ignore", column, "--ignore", fkey}, repository)
		})
		// The history head is post-contract; the live database is post-expand,
		// so the only difference left is the default that contract drops.
		if output.code != 4 || strings.Count(output.stdout, `"kind":"changed_in_actual"`) != 1 || !strings.Contains(output.stdout, "column:public:draft:status") {
			t.Fatalf("drift check = %d, %s", output.code, output.stdout)
		}
	})

	t.Run("a selector that matches nothing is an error", func(t *testing.T) {
		result, report := check(legacy, column, fkey, "table:public.absent")
		if result.code == 0 || report.Status == "ready" || !strings.Contains(result.stdout, "invalid_ignore") || !strings.Contains(result.stdout, "table:public.absent") {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
	})

	t.Run("a missing selector leaves the difference visible", func(t *testing.T) {
		result, report := check(legacy, column)
		if result.code != 4 || report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "catalog_drift" ||
			len(report.Differences) != 1 || report.Differences[0].Kind != "missing_in_actual" || report.Differences[0].ObjectID != "constraint:public:audit:audit_Mixed_fkey" {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
	})

	// An ignored table must not hide drift in a table that is not ignored:
	// the foreign key that another table holds to it is still compared.
	t.Run("an ignored table does not mask a missing foreign key that points at it", func(t *testing.T) {
		exec("ALTER TABLE public.child DROP CONSTRAINT child_parent_fkey")
		defer exec("ALTER TABLE public.child ADD CONSTRAINT child_parent_fkey FOREIGN KEY (parent_id) REFERENCES public.parent (id)")
		result, report := check(legacy, column, fkey, "table:public.parent")
		if result.code == 0 || report.Status == "ready" || !strings.Contains(result.stdout, "invalid_ignore") ||
			!strings.Contains(result.stdout, "child_parent_fkey") || !strings.Contains(result.stdout, "references ignored table") {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
		// Without the table selector the missing key is drift.
		result, report = check(legacy, column, fkey)
		if result.code != 4 || len(report.Differences) != 1 || report.Differences[0].ObjectID != "constraint:public:child:child_parent_fkey" {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
	})

	t.Run("an ignored schema does not mask a foreign key that points into it", func(t *testing.T) {
		exec("ALTER TABLE public.child ADD COLUMN snapshot_id bigint REFERENCES legacy.order_snapshot (id)")
		defer exec("ALTER TABLE public.child DROP COLUMN snapshot_id")
		result, report := check(legacy, column, fkey)
		if result.code == 0 || report.Status == "ready" || !strings.Contains(result.stdout, "invalid_ignore") || !strings.Contains(result.stdout, "references ignored table") {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
	})

	t.Run("the check is still ready after the probes", func(t *testing.T) {
		if result, report := check(legacy, column, fkey); result.code != 0 || report.Status != "ready" {
			t.Fatalf("%d %s", result.code, result.stdout)
		}
	})
}
