package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/verify"
)

// One command verifies the expand checkpoint and then the complete bundle.
// The two verifications must report exactly what two unshared verifications
// report, and must not replay the same SQL more often than their checks need.
func TestSharedExecutionsReportWhatSeparateVerificationsReport(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY); CREATE TABLE app.obsolete (id bigint PRIMARY KEY);\n"

	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, repository string) (bundleID string)
		// started is the number of scratch replays the shared pair needs.
		started int
	}{
		{
			// Expand is the last work of this bundle. Both verifications
			// compare the same two separate executions.
			name: "bundle that ends with expand",
			prepare: func(t *testing.T, repository string) string {
				writeHistoryTransitionFixture(t, repository, url, "add-timezone",
					"CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text); CREATE TABLE app.obsolete (id bigint PRIMARY KEY);\n")
				return "add-timezone"
			},
			started: 2,
		},
		{
			// The expand prefix and the complete bundle run different SQL. The
			// second verification reuses the complete execution of the first
			// and adds one separate complete execution to compare it with.
			name: "bundle with a contract phase",
			prepare: func(t *testing.T, repository string) string {
				writeHistoryContractDropFixture(t, repository, url, "remove-all")
				return "remove-all"
			},
			started: 3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			writeHistoryFixture(t, repository, url, "genesis", baseDDL)
			bundleID := test.prepare(t, repository)
			chain, err := history.Load(repository, "onward-bundles", "primary")
			if err != nil {
				t.Fatal(err)
			}
			before := disposableDatabaseCount(t, url)
			run := func(through string, executions *verify.Executions) string {
				t.Helper()
				report, err := verify.Run(ctx, verify.Input{
					AdminURL: url, Chain: chain, BundleID: bundleID, ThroughPhase: through, Executions: executions,
				})
				if err != nil {
					t.Fatal(err)
				}
				if report.Outcome != "verified" && report.Outcome != "partial_verified" {
					t.Fatalf("verification through %s = %#v", through, report)
				}
				body, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				return string(body)
			}
			separateExpand := run(protocol.PhaseExpand, nil)
			separateContract := run(protocol.PhaseContract, nil)

			shared := verify.NewExecutions()
			sharedExpand := run(protocol.PhaseExpand, shared)
			sharedContract := run(protocol.PhaseContract, shared)
			if sharedExpand != separateExpand {
				t.Errorf("shared expand report differs:\nshared:   %s\nseparate: %s", sharedExpand, separateExpand)
			}
			if sharedContract != separateContract {
				t.Errorf("shared contract report differs:\nshared:   %s\nseparate: %s", sharedContract, separateContract)
			}
			if got := shared.Started(); got != test.started {
				t.Errorf("shared verifications started %d scratch replays, want %d", got, test.started)
			}
			// A repeat of the complete verification has all the evidence it needs.
			if again := run(protocol.PhaseContract, shared); again != separateContract || shared.Started() != test.started {
				t.Errorf("a repeated verification started more replays (%d) or changed its report", shared.Started())
			}
			if after := disposableDatabaseCount(t, url); after != before {
				t.Fatalf("disposable database count = %d, want %d", after, before)
			}
		})
	}
}

// A SQL failure is reported the same way with and without sharing, and both
// scratch databases of the pair are dropped.
func TestSharedExecutionsReportAFailingBundleOnce(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	repository := t.TempDir()
	writeHistoryFixture(t, repository, url, "genesis", "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);\n")
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	// Replace the planned statements with SQL that fails. The identity of the
	// execution follows the plan, so no earlier result can answer for it.
	var plan protocol.Result
	if err := json.Unmarshal(chain.Entries[0].Artifact.Files["plan.json"], &plan); err != nil {
		t.Fatal(err)
	}
	plan.Batches[0].Statements = []protocol.Statement{{SQL: "CREATE TABLE missing_schema.accounts (id bigint);"}}
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	chain.Entries[0].Artifact.Files["plan.json"] = body

	before := disposableDatabaseCount(t, url)
	shared := verify.NewExecutions()
	for range 2 {
		report, err := verify.Run(ctx, verify.Input{AdminURL: url, Chain: chain, BundleID: "genesis", Executions: shared})
		if err != nil {
			t.Fatal(err)
		}
		if report.Outcome != "failed" || report.Failure == nil || report.Failure.BundleID != "genesis" {
			t.Fatalf("failure report = %#v", report)
		}
	}
	if got := shared.Started(); got != 2 {
		t.Fatalf("a failing bundle started %d scratch replays, want the 2 of one pair", got)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("disposable database count after failure = %d, want %d", after, before)
	}
}
