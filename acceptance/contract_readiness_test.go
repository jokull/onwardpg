package acceptance_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jokull/onwardpg/internal/testkit"
)

func TestNativeContractReadinessRequiresEvidenceAndReconciliation(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	baseline := []byte("CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY);\n")
	desired := []byte("CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY, status text NOT NULL);\n")
	clients := testkit.ReleaseClientContracts{
		Legacy: testkit.ClientContract{Name: "legacy readiness client", Prepared: []testkit.PreparedAction{
			{Name: "legacy_readiness_insert", SQL: `INSERT INTO app.bookings (id) VALUES ($1)`},
		}},
		New: testkit.ClientContract{Name: "new readiness client", Actions: []testkit.ClientAction{
			{Name: "insert with status", SQL: `INSERT INTO app.bookings (id, status) VALUES ($1, $2) RETURNING id`, Arguments: []any{int64(3), "confirmed"}, ExpectedRow: []any{int64(3)}},
		}},
		Final: testkit.ClientContract{Name: "final readiness client", Actions: []testkit.ClientAction{
			{Name: "insert after readiness", SQL: `INSERT INTO app.bookings (id, status) VALUES ($1, $2) RETURNING id`, Arguments: []any{int64(4), "pending"}, ExpectedRow: []any{int64(4)}},
			{Name: "reject omitted status", SQL: `INSERT INTO app.bookings (id) VALUES ($1)`, Arguments: []any{int64(5)}, ExpectedSQLState: "23502"},
			{Name: "no null status", SQL: `SELECT count(*) FROM app.bookings WHERE status IS NULL`, ExpectedRow: []any{int64(0)}},
		}},
	}
	requireReleaseClientContracts(t, clients)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", "app", "--bundle", "baseline")

	workload, err := testkit.NewPostgres(ctx, adminURL, "accept_contract_readiness")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workload.Close(); err != nil {
			t.Errorf("clean readiness workload: %v", err)
		}
	})
	if err := workload.Apply(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	legacy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.PrepareAll(ctx, legacy, clients.Legacy.Prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_readiness_insert", int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteSchema(desired); err != nil {
		t.Fatal(err)
	}
	questions := runExit(t, ctx, workspace.Root, environment, 2, "plan", "required-status-readiness", "--target", "app", "--output", "json")
	var questionReport testkit.PlanEnvelope
	if err := questions.DecodeJSON(&questionReport); err != nil {
		t.Fatal(err)
	}
	if questionReport.Status != "needs_action" || questionReport.Durable.Status != "needs_input" || questionReport.Durable.PlanID == "" {
		t.Fatalf("required-column decision envelope = %#v", questionReport)
	}
	pending := runExit(t, ctx, workspace.Root, environment, 2,
		"plan", "required-status-readiness", "--target", "app", "--output", "json",
		"--hint", `{"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"}`,
		"--hint", `{"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]}`,
	)
	var pendingReport testkit.PlanEnvelope
	if err := pending.DecodeJSON(&pendingReport); err != nil {
		t.Fatal(err)
	}
	if pendingReport.Status != "needs_action" || pendingReport.Durable.Status != "needs_sql_edits" || pendingReport.Durable.PlanID != questionReport.Durable.PlanID || len(pendingReport.Durable.Edits) != 1 {
		t.Fatalf("required-column SQL handoff = %#v", pendingReport)
	}
	contractPath := workspace.PhasePath("required-status-readiness", "contract")
	pocketIDs, err := testkit.PhasePocketIDs(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pocketIDs) != 1 || pocketIDs[0] != pendingReport.Durable.Edits[0].PocketID {
		t.Fatalf("contract edit pockets = %v, output = %#v", pocketIDs, pendingReport.Durable.Edits)
	}
	if err := testkit.EditPhasePocket(contractPath, pocketIDs[0], []byte(`UPDATE "app"."bookings" SET "status" = 'pending' WHERE "status" IS NULL;`+"\n")); err != nil {
		t.Fatal(err)
	}
	verified := runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", "required-status-readiness")
	var verifyReport struct {
		Status          string `json:"status"`
		ReceiptsUpdated bool   `json:"receipts_updated"`
	}
	if err := verified.DecodeJSON(&verifyReport); err != nil {
		t.Fatal(err)
	}
	if verifyReport.Status != "verified" || !verifyReport.ReceiptsUpdated {
		t.Fatalf("verification did not receipt edited plan: %#v", verifyReport)
	}
	runOK(t, ctx, workspace.Root, environment, "verify", "--target", "app", "--bundle", "required-status-readiness", "--check")

	deploy, err := workload.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deploy.Close(context.Background())
	if _, err := testkit.ApplyPhaseFile(ctx, deploy, workspace.PhasePath("required-status-readiness", "expand")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, "legacy_readiness_insert", int64(2)); err != nil {
		t.Fatalf("prepared legacy writer after expand: %v", err)
	}
	if err := testkit.RunClientActions(ctx, deploy, clients.New.Actions); err != nil {
		t.Fatalf("new client after expand: %v", err)
	}
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}

	observer, err := testkit.NewReadOnlyObserver(ctx, adminURL, workload, "app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := observer.Close(); err != nil {
			t.Errorf("clean readiness observer: %v", err)
		}
	})
	const observerEnv = "ONWARDPG_ACCEPTANCE_OBSERVER_DATABASE_URL"
	contractEnvironment := map[string]string{
		acceptanceDatabaseEnv: adminURL,
		observerEnv:           observer.URL,
	}
	manifestPath := filepath.Join(workspace.BundlePath("required-status-readiness"), "manifest.json")
	evidence, err := testkit.BuildWriterEvidence(manifestPath, "acceptance", "required-status-readiness", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.PlanID != pendingReport.Durable.PlanID {
		t.Fatalf("evidence PlanID = %s, planned PlanID = %s", evidence.PlanID, pendingReport.Durable.PlanID)
	}
	evidencePath := filepath.Join(workspace.Root, "writer-evidence.json")
	if err := testkit.WriteWriterEvidence(evidencePath, evidence); err != nil {
		t.Fatal(err)
	}

	withoutEvidence := runExit(t, ctx, workspace.Root, contractEnvironment, 2,
		"contract", "check", "--target", "app", "--bundle", "required-status-readiness",
		"--environment", "acceptance", "--database-env", observerEnv,
	)
	missing := decodeContractReadiness(t, withoutEvidence)
	if missing.Status != "needs_evidence" || missing.PlanID != evidence.PlanID || missing.BundleEntryDigest != evidence.BundleEntryDigest || missing.Observer.Mode != "dedicated_read_only" {
		t.Fatalf("missing-evidence readiness = %#v", missing)
	}

	beforeCleanup := runExit(t, ctx, workspace.Root, contractEnvironment, 2,
		"contract", "check", "--target", "app", "--bundle", "required-status-readiness",
		"--environment", "acceptance", "--database-env", observerEnv, "--evidence", evidencePath,
	)
	pendingCleanup := decodeContractReadiness(t, beforeCleanup)
	if pendingCleanup.Status != "reconciliation_required" || len(pendingCleanup.Reconciliations) != 1 || pendingCleanup.Reconciliations[0].PhasePath != "phases/contract.sql" {
		t.Fatalf("pre-cleanup readiness = %#v", pendingCleanup)
	}

	batches, err := testkit.ReadPhaseBatches(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || !batches[0].Transactional || !batches[1].Transactional {
		t.Fatalf("required-column contract batches = %#v", batches)
	}
	if err := testkit.ApplyPhaseBatch(ctx, deploy, batches[0]); err != nil {
		t.Fatalf("apply receipted cleanup batch: %v", err)
	}
	readyResult := runOK(t, ctx, workspace.Root, contractEnvironment,
		"contract", "check", "--target", "app", "--bundle", "required-status-readiness",
		"--environment", "acceptance", "--database-env", observerEnv, "--evidence", evidencePath,
	)
	ready := decodeContractReadiness(t, readyResult)
	if ready.Status != "ready" || ready.PlanID != evidence.PlanID || ready.BundleEntryDigest != evidence.BundleEntryDigest || ready.Observer.Mode != "dedicated_read_only" || ready.Digest == "" {
		t.Fatalf("ready contract boundary = %#v", ready)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("remove observer overlay before final drift proof: %v", err)
	}
	if err := testkit.ApplyPhaseBatch(ctx, deploy, batches[1]); err != nil {
		t.Fatalf("apply receipted enforcement batch: %v", err)
	}
	if err := testkit.RunClientActions(ctx, deploy, clients.Final.Actions); err != nil {
		t.Fatalf("final client after contract: %v", err)
	}
	if _, err := testkit.AssertZeroResidual(ctx, adminURL, desired, workload.Config()); err != nil {
		t.Fatalf("final independent zero-drift oracle: %v", err)
	}
}

type acceptanceContractReport struct {
	Status            string `json:"status"`
	PlanID            string `json:"plan_id"`
	BundleEntryDigest string `json:"bundle_entry_digest"`
	Digest            string `json:"digest"`
	Observer          struct {
		Mode string `json:"mode"`
	} `json:"observer"`
	Reconciliations []struct {
		PhasePath string `json:"phase_path"`
	} `json:"reconciliations"`
}

func decodeContractReadiness(t *testing.T, result testkit.CommandResult) acceptanceContractReport {
	t.Helper()
	var report acceptanceContractReport
	if err := result.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	return report
}
