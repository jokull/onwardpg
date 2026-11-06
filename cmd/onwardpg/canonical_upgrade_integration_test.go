package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
)

// The fixture was initialized and verified by preview.14, whose CHECK catalog
// definition retained a nested AND group that current inspection flattens.
func TestVerifyOldCanonicalReceiptWithoutRewritingItOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	major, err := source.PostgresMajor(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if major != 18 {
		t.Skip("preview.14 fixture was receipted on PostgreSQL 18")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	fixture := filepath.Join("testdata", "canonical-upgrade-preview14")
	for _, name := range []string{"schema.sql", "baseline/manifest.json", "baseline/plan.json", "baseline/phases/expand.sql"} {
		body, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		destination := name
		if name != "schema.sql" {
			destination = filepath.Join("onward-bundles", "primary", name)
		}
		writeTestFile(t, repository, destination, string(body))
	}
	manifestPath := filepath.Join(repository, "onward-bundles", "primary", "baseline", "manifest.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	check := func() (int, verify.Report) {
		t.Helper()
		result := captureStdout(t, func() int {
			return runVerifyAt([]string{"--target", "primary", "--bundle", "baseline", "--check"}, repository)
		})
		var report verify.Report
		if err := json.Unmarshal([]byte(result.stdout), &report); err != nil {
			t.Fatalf("verify: %d %s: %v", result.code, result.stdout, err)
		}
		return result.code, report
	}
	if code, report := check(); code != 0 || report.Outcome != "verified" {
		t.Fatalf("unchanged old bundle: %d %#v", code, report)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("verify --check rewrote the old receipt: %v", err)
	}
	ddl, err := os.ReadFile(filepath.Join(repository, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", strings.Replace(string(ddl), "y > 0", "y > 1", 1))
	if code, report := check(); code != 4 || report.Outcome != "stale" || len(report.Findings) != 1 || report.Findings[0].Code != "working_schema_changed" {
		t.Fatalf("real CHECK change: %d %#v", code, report)
	}
}
