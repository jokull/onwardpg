package acceptance_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jokull/onwardpg/internal/activeplan"
	"github.com/jokull/onwardpg/internal/testkit"
)

// A cutover window can run several planned bundles back to back behind a
// write barrier: expand 1, contract 1, expand 2, contract 2, ... The contract
// of a bundle that is not the history head must then be checkable, but only
// in chain order, and one evidence file must be able to attest the barrier
// for the whole pending prefix.
func TestNativeContractReadinessChecksNonHeadBundleInChainOrder(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	schema := func(accounts, bookings, notes string) []byte {
		return []byte("CREATE SCHEMA app;\n" +
			"CREATE TABLE app.accounts (id bigint PRIMARY KEY, " + accounts + " text NOT NULL);\n" +
			"CREATE TABLE app.bookings (id bigint PRIMARY KEY, " + bookings + " text NOT NULL);\n" +
			"CREATE TABLE app.notes (id bigint PRIMARY KEY, " + notes + " text NOT NULL);\n")
	}
	baseline := schema("display_name", "guest", "body")
	steps := []struct {
		bundle, table, from, to string
		desired                 []byte
	}{
		{"rename-account-name", "accounts", "display_name", "full_name", schema("full_name", "guest", "body")},
		{"rename-booking-guest", "bookings", "guest", "guest_name", schema("full_name", "guest_name", "body")},
		{"rename-note-body", "notes", "body", "content", schema("full_name", "guest_name", "content")},
	}
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", "app", "--bundle", "baseline")
	for _, step := range steps {
		if err := workspace.WriteSchema(step.desired); err != nil {
			t.Fatal(err)
		}
		runOK(t, ctx, workspace.Root, environment,
			"plan", step.bundle, "--target", "app", "--output", "json",
			"--hint", `{"kind":"rename","object":"column","from":["app","`+step.table+`","`+step.from+`"],"to":["app","`+step.table+`","`+step.to+`"]}`,
			"--hint", `{"kind":"rename_backfill","name":["app","`+step.table+`","`+step.from+`"],"strategy":"single_transaction"}`,
		)
		runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", step.bundle)
		// The bundle lands; a fresh checkout of the repository has no local
		// active-plan anchor, so the next bundle can be planned on top.
		if err := activeplan.Clear(workspace.Root, "app"); err != nil {
			t.Fatal(err)
		}
	}

	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_contract_chain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean chain workload: %v", err)
		}
	})
	if err := workload.Apply(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	deploy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deploy.Close(context.Background())
	observer, err := testkit.NewReadOnlyObserver(ctx, adminURL, workload, "app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := observer.Close(); err != nil {
			t.Errorf("clean chain observer: %v", err)
		}
	})
	const observerEnv = "ONWARDPG_ACCEPTANCE_OBSERVER_DATABASE_URL"
	contractEnvironment := map[string]string{acceptanceDatabaseEnv: adminURL, observerEnv: observer.URL}

	// One file attests the write barrier for the first two bundles; the head
	// gets a separate single-object file.
	var prefix []testkit.WriterEvidence
	for _, step := range steps[:2] {
		evidence, err := testkit.BuildWriterEvidence(filepath.Join(workspace.BundlePath(step.bundle), "manifest.json"), "acceptance", "cutover-window", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		prefix = append(prefix, evidence)
	}
	prefixBody, err := json.Marshal(prefix)
	if err != nil {
		t.Fatal(err)
	}
	prefixPath := filepath.Join(workspace.Root, "cutover-evidence.json")
	if err := os.WriteFile(prefixPath, prefixBody, 0o600); err != nil {
		t.Fatal(err)
	}
	headEvidence, err := testkit.BuildWriterEvidence(filepath.Join(workspace.BundlePath(steps[2].bundle), "manifest.json"), "acceptance", "cutover-window", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	headPath := filepath.Join(workspace.Root, "head-evidence.json")
	if err := testkit.WriteWriterEvidence(headPath, headEvidence); err != nil {
		t.Fatal(err)
	}
	check := func(exitCode int, bundle, evidencePath string) chainContractReport {
		t.Helper()
		arguments := []string{"contract", "check", "--target", "app", "--bundle", bundle, "--environment", "acceptance", "--database-env", observerEnv}
		if evidencePath != "" {
			arguments = append(arguments, "--evidence", evidencePath)
		}
		var report chainContractReport
		if err := runExit(t, ctx, workspace.Root, contractEnvironment, exitCode, arguments...).DecodeJSON(&report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	requireFinding := func(report chainContractReport, code string) {
		t.Helper()
		if report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != code {
			t.Fatalf("want blocked %s, got %#v", code, report)
		}
	}
	apply := func(bundle, phase string) {
		t.Helper()
		if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath(bundle, phase)); err != nil {
			t.Fatalf("apply %s %s: %v", bundle, phase, err)
		}
	}

	apply(steps[0].bundle, "expand")
	requireFinding(check(4, steps[1].bundle, prefixPath), "predecessor_contract_pending")
	first := check(0, steps[0].bundle, prefixPath)
	if first.Status != "ready" || first.HistoryHead || first.ChainPosition != 2 || first.ChainLength != 4 || first.Evidence == nil || first.Evidence.Release != "cutover-window" || len(first.Evidence.Cohorts) != 7 {
		t.Fatalf("non-head contract readiness = %#v", first)
	}
	apply(steps[0].bundle, "contract")
	requireFinding(check(4, steps[0].bundle, prefixPath), "contract_already_applied")
	requireFinding(check(4, steps[2].bundle, headPath), "predecessor_contract_pending")
	requireFinding(check(4, steps[1].bundle, prefixPath), "expand_not_applied")

	apply(steps[1].bundle, "expand")
	middle := check(0, steps[1].bundle, prefixPath)
	if middle.Status != "ready" || middle.HistoryHead || middle.ChainPosition != 3 || middle.Evidence == nil {
		t.Fatalf("middle-bundle contract readiness = %#v", middle)
	}
	if stale := check(4, steps[1].bundle, headPath); stale.Status != "stale" || len(stale.Findings) != 1 || stale.Findings[0].Code != "writer_evidence_binding_mismatch" {
		t.Fatalf("evidence for another bundle must not satisfy the gate: %#v", stale)
	}
	apply(steps[1].bundle, "contract")

	apply(steps[2].bundle, "expand")
	if missing := check(2, steps[2].bundle, ""); missing.Status != "needs_evidence" || !missing.HistoryHead {
		t.Fatalf("head without evidence = %#v", missing)
	}
	if uncovered := check(4, steps[2].bundle, prefixPath); uncovered.Status != "stale" {
		t.Fatalf("the prefix file must not cover the head: %#v", uncovered)
	}
	if head := check(0, steps[2].bundle, headPath); head.Status != "ready" || !head.HistoryHead || head.ChainPosition != 4 {
		t.Fatalf("head contract readiness = %#v", head)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("remove observer overlay before final drift proof: %v", err)
	}
	apply(steps[2].bundle, "contract")
	if _, err := testkit.AssertZeroResidual(ctx, adminURL, steps[2].desired, workload.Config()); err != nil {
		t.Fatalf("final independent zero-drift oracle: %v", err)
	}
}

type chainContractReport struct {
	Status        string `json:"status"`
	HistoryHead   bool   `json:"history_head"`
	ChainPosition int    `json:"chain_position"`
	ChainLength   int    `json:"chain_length"`
	Evidence      *struct {
		Release string            `json:"release"`
		Cohorts []json.RawMessage `json:"cohorts"`
	} `json:"evidence"`
	Findings []struct {
		Code string `json:"code"`
	} `json:"findings"`
}
