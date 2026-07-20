package contractcheck

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/internal/source"
)

func TestContractCheckUsesOneReadOnlySnapshotAndRequiresWriterEvidence(t *testing.T) {
	databaseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, databaseURL, "onwardpg_contract_check")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	databaseURL = restrictedScratchURL(database.Config)
	snapshot, err := source.LoadGraphForComparison(ctx, source.Parse(databaseURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	dataGate := protocol.ContractGate{ID: "data:readonly", Kind: "data_assertion", ScopeFingerprint: fingerprint, Reason: "readiness transaction must be read only", BooleanSQL: `SELECT current_setting('transaction_read_only') = 'on';`}
	writerGate := protocol.ContractGate{ID: "writers:legacy", Kind: "writer_attestation", ScopeFingerprint: fingerprint, Reason: "legacy writers must drain", RequiredEvidence: []string{"ad_hoc_writers", "connection_pools", "previews", "queues", "scheduled_jobs", "web", "workers"}}
	statement := protocol.Statement{SQL: "SELECT 1;", Phase: protocol.PhaseContract, Safety: "review", RequiresGates: []string{dataGate.ID, writerGate.ID}, TransitionID: "test:readiness"}
	statement.ID = protocol.StableStatementID(statement)
	result := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: fingerprint, DesiredFingerprint: fingerprint, Status: protocol.Planned,
		Statements: []protocol.Statement{statement}, Batches: []protocol.Batch{{ID: "batch-contract-001", Phase: protocol.PhaseContract, Transactional: true, Statements: []protocol.Statement{statement}}},
		ContractGates: []protocol.ContractGate{dataGate, writerGate}, Reconciliations: []protocol.Reconciliation{{TransitionID: "test:readiness", Strategy: "assert_only", GateIDs: []string{dataGate.ID}}},
	}
	metadata := bundle.Metadata{
		BundleID: "readiness-test", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 1, Target: "primary", Purpose: "contract",
		BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "integration baseline", Fingerprint: fingerprint},
		DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "integration desired", Fingerprint: fingerprint},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err = bundle.WithExpandCheckpoint(artifact, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Artifact: artifact, ExpectedHead: artifact.Manifest.History.EntryDigest, DatabaseURL: databaseURL, Environment: "production", Now: time.Now().UTC()}
	report, err := Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "needs_evidence" || len(report.GateResults) != 1 || !report.GateResults[0].Passed {
		t.Fatalf("missing evidence report=%#v", report)
	}
	evidence := Evidence{
		ProtocolVersion: EvidenceVersion, Target: "primary", Environment: "production", PlanID: metadata.PlanID,
		BundleEntryDigest: artifact.Manifest.History.EntryDigest, DesiredFingerprint: fingerprint, Generation: 1, Release: "integration-release",
		ObservedAt: input.Now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: input.Now.Add(time.Hour).Format(time.RFC3339),
	}
	for _, category := range writerGate.RequiredEvidence {
		evidence.Cohorts = append(evidence.Cohorts, Cohort{Category: category, Name: category, Status: "drained", SourceKind: "manual", Source: "integration-test"})
	}
	input.Evidence, err = json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ready" || report.Digest == "" || len(report.GateResults) != 2 {
		t.Fatalf("ready report=%#v", report)
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA contract_check_drift"); err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "catalog_drift" {
		t.Fatalf("post-expand drift report=%#v", report)
	}
}

func TestContractCheckProjectsDedicatedObserverAccessAndFailsClosed(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "onwardpg_observer_check")
	if err != nil {
		t.Fatal(err)
	}
	observerRole := database.Role + "_observer"
	observerGrantRole := database.Role + "_observer_grants"
	observerPassword := "observer-test-password"
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{observerGrantRole}.Sanitize()+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE "+pgx.Identifier{observerRole}.Sanitize()+" LOGIN PASSWORD '"+observerPassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; GRANT "+pgx.Identifier{observerGrantRole}.Sanitize()+" TO "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
		admin.Close(ctx)
		database.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = admin.Close(context.Background())
		_ = database.Close()
		cleanup, cleanupErr := pgx.Connect(context.Background(), adminURL)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{observerRole}.Sanitize())
			_, _ = cleanup.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{observerGrantRole}.Sanitize())
			_ = cleanup.Close(context.Background())
		}
	}()

	owner, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, "CREATE SCHEMA app; CREATE TABLE app.orders (id bigint PRIMARY KEY, status text NOT NULL); INSERT INTO app.orders VALUES (1, 'new')"); err != nil {
		t.Fatal(err)
	}
	ownerURL := restrictedScratchURL(database.Config)
	baseline, err := source.LoadGraphForComparison(ctx, source.Parse(ownerURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := baseline.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	dataGate := protocol.ContractGate{ID: "data:observer", Kind: "data_assertion", ScopeFingerprint: fingerprint, Reason: "observer can prove data", BooleanSQL: "SELECT count(*) = 1 FROM app.orders WHERE status = 'new';"}
	statement := protocol.Statement{SQL: "SELECT 1;", Phase: protocol.PhaseContract, Safety: "review", RequiresGates: []string{dataGate.ID}, TransitionID: "test:observer"}
	statement.ID = protocol.StableStatementID(statement)
	result := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: fingerprint, DesiredFingerprint: fingerprint, Status: protocol.Planned,
		Statements: []protocol.Statement{statement}, Batches: []protocol.Batch{{ID: "batch-contract-001", Phase: protocol.PhaseContract, Transactional: true, Statements: []protocol.Statement{statement}}},
		ContractGates: []protocol.ContractGate{dataGate}, Reconciliations: []protocol.Reconciliation{{TransitionID: "test:observer", Strategy: "assert_only", GateIDs: []string{dataGate.ID}}},
	}
	metadata := bundle.Metadata{
		BundleID: "observer-test", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 1, Target: "primary", Purpose: "contract",
		BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "integration baseline", Fingerprint: fingerprint},
		DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "integration desired", Fingerprint: fingerprint},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err = bundle.WithExpandCheckpoint(artifact, fingerprint)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := admin.Exec(ctx, "GRANT CONNECT ON DATABASE "+pgx.Identifier{database.Name}.Sanitize()+" TO "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA app TO "+pgx.Identifier{observerGrantRole}.Sanitize()+"; GRANT SELECT ON ALL TABLES IN SCHEMA app TO "+pgx.Identifier{observerGrantRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	observerConfig := database.Config.Copy()
	observerConfig.User = observerRole
	observerConfig.Password = observerPassword
	input := Input{
		Artifact: artifact, ExpectedHead: artifact.Manifest.History.EntryDigest,
		DatabaseURL: restrictedScratchURL(observerConfig), Environment: "production", Now: time.Now().UTC(),
	}
	report, err := Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ready" || report.Observer.Mode != "dedicated_read_only" || len(report.Observer.ProjectedAccess) < 3 {
		t.Fatalf("dedicated observer report=%#v", report)
	}
	if _, err := owner.Exec(ctx, "GRANT SELECT ON app.orders TO PUBLIC"); err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "catalog_drift" {
		t.Fatalf("unrelated application grant report=%#v", report)
	}
	if _, err := owner.Exec(ctx, "REVOKE SELECT ON app.orders FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}

	if _, err := owner.Exec(ctx, "GRANT INSERT ON app.orders TO "+pgx.Identifier{observerGrantRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "observer_access_policy_unsafe" {
		t.Fatalf("unsafe observer report=%#v", report)
	}
	if _, err := owner.Exec(ctx, "REVOKE INSERT ON app.orders FROM "+pgx.Identifier{observerGrantRole}.Sanitize()+"; ALTER TABLE app.orders ENABLE ROW LEVEL SECURITY"); err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "observer_rls_incomplete" {
		t.Fatalf("RLS observer report=%#v", report)
	}
}

func TestContractCheckOrdersWriterDrainBeforeManualReconciliation(t *testing.T) {
	databaseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, databaseURL, "onwardpg_reconciliation_check")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, "CREATE TABLE public.deliveries (id bigint PRIMARY KEY, tier text NOT NULL); INSERT INTO public.deliveries VALUES (1, 'legacy')"); err != nil {
		t.Fatal(err)
	}
	ownerURL := restrictedScratchURL(database.Config)
	snapshot, err := source.LoadGraphForComparison(ctx, source.Parse(ownerURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	manualGate := protocol.ContractGate{
		ID: "data:tiers", Kind: "manual_reconciliation", ScopeFingerprint: fingerprint,
		Reason: "legacy tiers must be normalized", BooleanSQL: "SELECT NOT EXISTS (SELECT 1 FROM public.deliveries WHERE tier = 'legacy');",
	}
	writerGate := protocol.ContractGate{
		ID: "writers:legacy", Kind: "writer_attestation", ScopeFingerprint: fingerprint,
		Reason: "legacy writers must drain", RequiredEvidence: []string{"web"},
	}
	work := &protocol.ManualWork{
		Summary: "normalize legacy delivery tiers in bounded windows", ExecutionMode: protocol.ManualOperatorBatched,
		Statements:      []string{"UPDATE public.deliveries SET tier = 'new' WHERE tier = 'legacy';"},
		VerificationSQL: []string{manualGate.BooleanSQL},
		ProgressKey:     "deliveries.id", IdempotencyNotes: "normalizing an already-normalized row is a no-op",
	}
	transitionID := "constraint:public:deliveries:tier"
	result := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: fingerprint, DesiredFingerprint: fingerprint, Status: protocol.Planned,
		ContractGates:   []protocol.ContractGate{manualGate, writerGate},
		Reconciliations: []protocol.Reconciliation{{TransitionID: transitionID, Strategy: "manual_sql", Work: work, GateIDs: []string{manualGate.ID, writerGate.ID}}},
		Operations: []protocol.Operation{{
			ID: "operation-normalize-tiers", TransitionID: transitionID, Timing: "after_expand_before_contract",
			ExecutionMode: protocol.ManualOperatorBatched, Summary: work.Summary, BatchTemplate: work.Statements,
			ProgressKey: work.ProgressKey, CompletionSQL: manualGate.BooleanSQL, IdempotencyNotes: work.IdempotencyNotes, CompletionGateIDs: []string{manualGate.ID},
		}},
	}
	metadata := bundle.Metadata{
		BundleID: "reconciliation-test", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 1, Target: "primary", Purpose: "contract",
		BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "integration baseline", Fingerprint: fingerprint},
		DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "integration desired", Fingerprint: fingerprint},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err = bundle.WithExpandCheckpoint(artifact, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := Input{Artifact: artifact, ExpectedHead: artifact.Manifest.History.EntryDigest, DatabaseURL: ownerURL, Environment: "production", Now: now}
	report, err := Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "needs_evidence" || len(report.GateResults) != 1 || report.GateResults[0].Passed {
		t.Fatalf("pre-drain report=%#v", report)
	}
	evidence := Evidence{
		ProtocolVersion: EvidenceVersion, Target: "primary", Environment: "production", PlanID: metadata.PlanID,
		BundleEntryDigest: artifact.Manifest.History.EntryDigest, DesiredFingerprint: fingerprint, Generation: 1, Release: "release-42",
		ObservedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Cohorts: []Cohort{{Category: "web", Name: "web", Status: "drained", SourceKind: "manual", Source: "deploy-42"}},
	}
	input.Evidence, err = json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "reconciliation_required" || len(report.Reconciliations) != 1 || report.Reconciliations[0].TransitionID != transitionID || report.Reconciliations[0].PhasePath != "operations/operation-normalize-tiers.json" {
		t.Fatalf("post-drain report=%#v", report)
	}
	if _, err := owner.Exec(ctx, work.Statements[0]); err != nil {
		t.Fatal(err)
	}
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ready" {
		t.Fatalf("post-reconciliation report=%#v", report)
	}
}

func restrictedScratchURL(config *pgx.ConnConfig) string {
	connection := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(config.User, config.Password),
		Host:   net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))),
		Path:   config.Database,
	}
	query := connection.Query()
	query.Set("sslmode", "disable")
	connection.RawQuery = query.Encode()
	return connection.String()
}
