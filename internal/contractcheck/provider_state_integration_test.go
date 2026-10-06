package contractcheck

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/internal/source"
)

// providerFixture reproduces what a managed PostgreSQL provider leaves in a
// production database: objects owned by the provider's administrative role
// instead of the database owner. The inspecting role is the database owner,
// as it is for a project's own read-only production URL.
type providerFixture struct {
	t        *testing.T
	Role     string
	URL      string
	database *pgx.Conn
	cluster  *pgx.Conn
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	scratch, err := scratchdb.Create(ctx, adminURL, "onwardpg_provider_state")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &providerFixture{t: t, Role: scratch.Role + "_provider", URL: restrictedScratchURL(scratch.Config)}
	if fixture.cluster, err = pgx.Connect(ctx, adminURL); err != nil {
		_ = scratch.Close()
		t.Fatal(err)
	}
	adminConfig := fixture.cluster.Config().Copy()
	adminConfig.Database = scratch.Name
	if fixture.database, err = pgx.ConnectConfig(ctx, adminConfig); err != nil {
		_ = fixture.cluster.Close(ctx)
		_ = scratch.Close()
		t.Fatal(err)
	}
	role := pgx.Identifier{fixture.Role}.Sanitize()
	if _, err := fixture.cluster.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN; GRANT CREATE ON DATABASE "+pgx.Identifier{scratch.Name}.Sanitize()+" TO "+role); err != nil {
		_ = fixture.database.Close(ctx)
		_ = fixture.cluster.Close(ctx)
		_ = scratch.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		background := context.Background()
		_ = fixture.database.Close(background)
		_ = scratch.Close()
		_, _ = fixture.cluster.Exec(background, "DROP ROLE IF EXISTS "+role)
		_ = fixture.cluster.Close(background)
	})
	return fixture
}

func (f *providerFixture) exec(sql string) {
	f.t.Helper()
	if _, err := f.database.Exec(context.Background(), sql); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// ownExtension installs a trusted extension the way the provider's role does:
// the extension is owned by that role, not by the database owner.
func (f *providerFixture) ownExtension(name string) {
	f.exec("SET ROLE " + pgx.Identifier{f.Role}.Sanitize() + "; CREATE EXTENSION " + name + "; RESET ROLE")
}

func (f *providerFixture) ownSchema(name string) {
	f.exec("CREATE SCHEMA " + pgx.Identifier{name}.Sanitize() + " AUTHORIZATION " + pgx.Identifier{f.Role}.Sanitize())
}

// grantParameter records a pg_parameter_acl entry. The catalog is cluster
// wide, so it is revoked before the role is dropped and before any later test
// inspects the cluster.
func (f *providerFixture) grantParameter(parameter string) {
	f.t.Helper()
	role := pgx.Identifier{f.Role}.Sanitize()
	if _, err := f.cluster.Exec(context.Background(), "GRANT SET ON PARAMETER "+parameter+" TO "+role); err != nil {
		f.t.Skipf("GRANT SET ON PARAMETER needs PostgreSQL 15 or later: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.cluster.Exec(context.Background(), "REVOKE SET ON PARAMETER "+parameter+" FROM "+role)
	})
}

// readinessInput builds a one-gate contract bundle whose receipted post-expand
// catalog is the database as it stands now.
func (f *providerFixture) readinessInput() Input {
	f.t.Helper()
	ctx := context.Background()
	snapshot, err := source.LoadGraphForComparison(ctx, source.Parse(f.URL), "", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	// A receipted checkpoint is planned from a graph without unsupported state.
	snapshot, err = snapshot.Project(nil, func(string) bool { return false })
	if err != nil {
		f.t.Fatal(err)
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		f.t.Fatal(err)
	}
	gate := protocol.ContractGate{ID: "data:ready", Kind: "data_assertion", ScopeFingerprint: fingerprint, Reason: "always true", BooleanSQL: "SELECT true;"}
	statement := protocol.Statement{SQL: "SELECT 1;", Phase: protocol.PhaseContract, Safety: "review", RequiresGates: []string{gate.ID}, TransitionID: "test:ready"}
	statement.ID = protocol.StableStatementID(statement)
	result := protocol.Result{
		CurrentFingerprint: fingerprint, DesiredFingerprint: fingerprint, Status: protocol.Planned,
		Statements: []protocol.Statement{statement}, Batches: []protocol.Batch{{ID: "batch-contract-001", Phase: protocol.PhaseContract, Transactional: true, Statements: []protocol.Statement{statement}}},
		ContractGates: []protocol.ContractGate{gate}, Reconciliations: []protocol.Reconciliation{{TransitionID: "test:ready", Strategy: "assert_only", GateIDs: []string{gate.ID}}},
	}
	metadata := bundle.Metadata{
		BundleID: "provider-state", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 1, Target: "primary", Purpose: "contract",
		BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "integration baseline", Fingerprint: fingerprint},
		DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "integration desired", Fingerprint: fingerprint},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		f.t.Fatal(err)
	}
	artifact, err = bundle.WithExpandCheckpoint(artifact, fingerprint)
	if err != nil {
		f.t.Fatal(err)
	}
	return Input{Artifact: artifact, ExpectedHead: artifact.Manifest.History.EntryDigest, DatabaseURL: f.URL, Environment: "production", Now: time.Now().UTC()}
}

func TestContractCheckNamesUnsupportedCatalogStateInsteadOfCatalogDrift(t *testing.T) {
	fixture := newProviderFixture(t)
	ctx := context.Background()
	input := fixture.readinessInput()
	report, err := Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ready" || len(report.Unsupported) != 0 {
		t.Fatalf("clean catalog report = %#v", report)
	}

	fixture.ownExtension("citext")
	fixture.ownSchema("provider_ext")
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// citext and provider_ext are modeled objects the receipted checkpoint does
	// not contain, so catalog drift is real here; the point is that the
	// unsupported state is named next to it instead of being folded into it.
	want := []string{"ownership:extension:citext=" + fixture.Role, "ownership:schema:provider_ext=" + fixture.Role}
	if report.Status != "unsupported" || strings.Join(report.Unsupported, ",") != strings.Join(want, ",") {
		t.Fatalf("unsupported report = %#v", report)
	}
	codes := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		codes = append(codes, finding.Code)
	}
	if strings.Join(codes, ",") != "unsupported_catalog_state,catalog_drift" || len(report.GateResults) != 0 {
		t.Fatalf("findings = %#v, gates = %#v", report.Findings, report.GateResults)
	}
}

func TestContractCheckDoesNotCallUnsupportedStateCatalogDrift(t *testing.T) {
	fixture := newProviderFixture(t)
	fixture.ownSchema("provider_ext")
	// Receipt a checkpoint that already contains the provider schema as a
	// modeled object: the only thing left unexplained is the ownership marker.
	input := fixture.readinessInput()
	report, err := Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "unsupported" || len(report.Findings) != 1 || report.Findings[0].Code != "unsupported_catalog_state" {
		t.Fatalf("report = %#v", report)
	}
}
