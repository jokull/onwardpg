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
