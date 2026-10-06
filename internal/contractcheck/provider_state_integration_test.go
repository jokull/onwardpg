package contractcheck

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/driftcheck"
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
	return newProviderFixtureWithRole(t, "_provider")
}

// newProviderFixtureWithRole names the provider role after the scratch role
// plus suffix, so a test can give it a name that needs quoting.
func newProviderFixtureWithRole(t *testing.T, suffix string) *providerFixture {
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
	fixture := &providerFixture{t: t, Role: scratch.Role + suffix, URL: restrictedScratchURL(scratch.Config)}
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

func TestLiveIgnoreAcknowledgesProviderStateWithoutHidingAnythingElse(t *testing.T) {
	fixture := newProviderFixture(t)
	ctx := context.Background()
	fixture.ownExtension("citext")
	fixture.ownSchema("provider_ext")
	fixture.grantParameter("session_replication_role")
	acknowledged := []string{
		"ownership:extension:citext=" + fixture.Role,
		"ownership:schema:provider_ext=" + fixture.Role,
		"parameter_acl:session_replication_role",
	}

	t.Run("without live_ignore the state blocks", func(t *testing.T) {
		snapshot, _, finding, err := InspectObserverCatalog(ctx, fixture.URL, nil, nil, 5*time.Second)
		if err != nil || finding != nil {
			t.Fatalf("finding = %#v, err = %v", finding, err)
		}
		if strings.Join(snapshot.Unsupported(), ",") != strings.Join(acknowledged, ",") {
			t.Fatalf("unsupported = %#v", snapshot.Unsupported())
		}
	})

	t.Run("live_ignore removes the markers, keeps the modeled objects, and reports what it removed", func(t *testing.T) {
		snapshot, projection, finding, err := InspectObserverCatalog(ctx, fixture.URL, nil, acknowledged, 5*time.Second)
		if err != nil || finding != nil {
			t.Fatalf("finding = %#v, err = %v", finding, err)
		}
		if len(snapshot.Unsupported()) != 0 || strings.Join(projection.LiveIgnored, ",") != strings.Join(acknowledged, ",") {
			t.Fatalf("unsupported = %#v, live ignored = %#v", snapshot.Unsupported(), projection.LiveIgnored)
		}
		if len(snapshot.Ignored()) != 0 {
			t.Fatalf("live_ignore must not leave ignore receipts: %#v", snapshot.Ignored())
		}
		for _, id := range snapshot.IDs() {
			if id.Name == "provider_ext" || id.Name == "citext" {
				return
			}
		}
		t.Fatalf("modeled provider objects were removed: %#v", snapshot.IDs())
	})

	t.Run("an ownership selector is pinned to its owner", func(t *testing.T) {
		wrongOwner := []string{"ownership:schema:provider_ext=somebody_else", "ownership:extension:citext=" + fixture.Role, "parameter_acl:session_replication_role"}
		snapshot, projection, _, err := InspectObserverCatalog(ctx, fixture.URL, nil, wrongOwner, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(snapshot.Unsupported(), ",") != acknowledged[1] || len(projection.LiveIgnored) != 2 {
			t.Fatalf("unsupported = %#v, live ignored = %#v", snapshot.Unsupported(), projection.LiveIgnored)
		}
	})

	t.Run("a new parameter grant is not covered by an older list", func(t *testing.T) {
		fixture.grantParameter("work_mem")
		snapshot, _, _, err := InspectObserverCatalog(ctx, fixture.URL, nil, acknowledged, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(snapshot.Unsupported(), ",") != "parameter_acl:work_mem" {
			t.Fatalf("unsupported = %#v", snapshot.Unsupported())
		}
	})

	t.Run("an event trigger survives the same list", func(t *testing.T) {
		fixture.exec("CREATE FUNCTION public.audit_ddl() RETURNS event_trigger LANGUAGE plpgsql AS 'BEGIN NULL; END'; CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION public.audit_ddl()")
		defer fixture.exec("DROP EVENT TRIGGER audit_ddl")
		snapshot, _, _, err := InspectObserverCatalog(ctx, fixture.URL, nil, acknowledged, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, selector := range snapshot.Unsupported() {
			found = found || selector == "event_trigger:audit_ddl"
		}
		if !found {
			t.Fatalf("unsupported = %#v", snapshot.Unsupported())
		}
	})

	t.Run("a genuine blocker survives the same list", func(t *testing.T) {
		fixture.exec("CREATE SEQUENCE public.manual_seq; ALTER SEQUENCE public.manual_seq OWNER TO " + pgx.Identifier{fixture.Role}.Sanitize())
		snapshot, _, _, err := InspectObserverCatalog(ctx, fixture.URL, nil, acknowledged, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		want := "ownership:relation:public.manual_seq=" + fixture.Role
		found := false
		for _, selector := range snapshot.Unsupported() {
			found = found || selector == want
		}
		if !found {
			t.Fatalf("unsupported = %#v", snapshot.Unsupported())
		}
	})
}

func TestLiveIgnoreDoesNotCoverACustomNamedNotNullConstraint(t *testing.T) {
	fixture := newProviderFixture(t)
	var major int
	if err := fixture.database.QueryRow(context.Background(), "SELECT current_setting('server_version_num')::integer / 10000").Scan(&major); err != nil {
		t.Fatal(err)
	}
	if major < 18 {
		t.Skip("named NOT NULL constraints are catalog state from PostgreSQL 18")
	}
	fixture.ownSchema("provider_ext")
	fixture.exec("CREATE TABLE public.users (id bigint CONSTRAINT users_id_present NOT NULL)")
	acknowledged := []string{"ownership:schema:provider_ext=" + fixture.Role}
	snapshot, projection, _, err := InspectObserverCatalog(context.Background(), fixture.URL, nil, acknowledged, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(snapshot.Unsupported(), ",") != "not_null_constraint:public.users.users_id_present" || len(projection.LiveIgnored) != 1 {
		t.Fatalf("unsupported = %#v, live ignored = %#v", snapshot.Unsupported(), projection.LiveIgnored)
	}
}

func TestContractCheckAppliesLiveIgnoreToUnsupportedStateOnly(t *testing.T) {
	fixture := newProviderFixture(t)
	ctx := context.Background()
	fixture.ownExtension("citext")
	fixture.ownSchema("provider_ext")
	fixture.grantParameter("session_replication_role")
	// The receipted post-expand catalog already contains the provider's modeled
	// objects, as it does when the project DDL creates the extension.
	input := fixture.readinessInput()
	acknowledged := []string{
		"ownership:extension:citext=" + fixture.Role,
		"ownership:schema:provider_ext=" + fixture.Role,
		"parameter_acl:session_replication_role",
	}
	report, err := Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "unsupported" || len(report.Unsupported) != 3 || report.Observer.ObservedFingerprint != "" {
		t.Fatalf("without live_ignore: %#v", report)
	}
	observed := report.ActualFingerprint

	input.LiveIgnore = acknowledged
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ready" || len(report.Unsupported) != 0 || strings.Join(report.Observer.LiveIgnored, ",") != strings.Join(acknowledged, ",") {
		t.Fatalf("with live_ignore: %#v", report)
	}
	// The comparison fingerprint is taken after the markers are removed, so the
	// catalog matches the receipted checkpoint; the raw one is reported apart.
	if report.ActualFingerprint != report.ExpectedFingerprint || report.Observer.ObservedFingerprint == "" || report.Observer.ObservedFingerprint == report.ActualFingerprint {
		t.Fatalf("fingerprints: expected %s, actual %s, observed %s", report.ExpectedFingerprint, report.ActualFingerprint, report.Observer.ObservedFingerprint)
	}

	if report.Observer.ObservedFingerprint != observed {
		t.Fatalf("observed fingerprint %s is not the unprojected catalog %s", report.Observer.ObservedFingerprint, observed)
	}

	// live_ignore cannot hide a modeled difference from the receipted checkpoint.
	fixture.exec("CREATE TABLE public.manual_table (id bigint)")
	report, err = Run(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "blocked" || len(report.Findings) != 1 || report.Findings[0].Code != "catalog_drift" {
		t.Fatalf("modeled drift under live_ignore: %#v", report)
	}
}

// Replayed history and the live catalog are identical except for state the
// provider owns: the project DDL creates the extension and the schema, and in
// the live cluster a provider role owns both and holds a parameter grant.
func TestDriftCheckIsInSyncWhenOnlyAcknowledgedProviderStateDiffers(t *testing.T) {
	fixture := newProviderFixture(t)
	ctx := context.Background()
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	fixture.ownExtension("citext")
	fixture.ownSchema("provider_ext")
	// The project's own role, the database owner, creates the application table.
	owner, err := pgx.Connect(ctx, fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, "CREATE TABLE public.users (id bigint, email citext)"); err != nil {
		t.Fatal(err)
	}
	fixture.grantParameter("session_replication_role")
	acknowledged := []string{
		"ownership:extension:citext=" + fixture.Role,
		"ownership:schema:provider_ext=" + fixture.Role,
		"parameter_acl:session_replication_role",
	}

	expected, err := source.LoadDDLGraphForComparison(ctx, []byte("CREATE EXTENSION citext; CREATE SCHEMA provider_ext; CREATE TABLE public.users (id bigint, email citext);"), "provider-state-test", adminURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Replay happens in a scratch cluster of its own in practice; here it shares
	// this cluster, so drop the cluster-wide parameter marker it also observes.
	expected, err = expected.Project(nil, func(selector string) bool { return !strings.HasPrefix(selector, "parameter_acl:") })
	if err != nil {
		t.Fatal(err)
	}
	compare := func(liveIgnore []string) (driftcheck.Report, ObserverProjection) {
		t.Helper()
		actual, projection, finding, err := InspectObserverCatalog(ctx, fixture.URL, nil, liveIgnore, 5*time.Second)
		if err != nil || finding != nil {
			t.Fatalf("finding = %#v, err = %v", finding, err)
		}
		report, err := driftcheck.Compare("primary", "head", expected, actual)
		if err != nil {
			t.Fatal(err)
		}
		return report, projection
	}

	report, projection := compare(nil)
	if report.Outcome != "unsupported" || strings.Join(report.Unsupported, ",") != strings.Join(acknowledged, ",") || len(report.Differences) != 0 || projection.ObservedFingerprint != "" {
		t.Fatalf("without live_ignore: %#v", report)
	}
	rawFingerprint := report.ActualFingerprint

	report, projection = compare(acknowledged)
	if report.Outcome != "drift_free" || len(report.Differences) != 0 || len(report.Unsupported) != 0 {
		t.Fatalf("with live_ignore: %#v", report)
	}
	if report.ExpectedFingerprint != report.ActualFingerprint {
		t.Fatalf("fingerprints differ: expected %s, actual %s", report.ExpectedFingerprint, report.ActualFingerprint)
	}
	if strings.Join(projection.LiveIgnored, ",") != strings.Join(acknowledged, ",") {
		t.Fatalf("live ignored = %#v", projection.LiveIgnored)
	}
	if projection.ObservedFingerprint != rawFingerprint || rawFingerprint == report.ActualFingerprint {
		t.Fatalf("observed fingerprint = %s, raw = %s, compared = %s", projection.ObservedFingerprint, rawFingerprint, report.ActualFingerprint)
	}
}

// Every selector the catalog queries really emit must pass live_ignore
// validation and be acknowledged by itself, including identifiers that
// quote_ident has to quote.
func TestLiveIgnoreAcceptsEveryProviderSelectorTheInspectorEmits(t *testing.T) {
	fixture := newProviderFixtureWithRole(t, `_Provider "Admin"=1`)
	ctx := context.Background()
	fixture.ownExtension(`"uuid-ossp"`)
	fixture.exec(`CREATE SCHEMA "Provider ""Schema""=x" AUTHORIZATION ` + pgx.Identifier{fixture.Role}.Sanitize())
	fixture.grantParameter("my.custom_param")
	fixture.grantParameter("session_replication_role")

	snapshot, _, finding, err := InspectObserverCatalog(ctx, fixture.URL, nil, nil, 5*time.Second)
	if err != nil || finding != nil {
		t.Fatalf("finding = %#v, err = %v", finding, err)
	}
	var emitted []string
	for _, selector := range snapshot.Unsupported() {
		if strings.HasPrefix(selector, "ownership:") || strings.HasPrefix(selector, "parameter_acl:") {
			emitted = append(emitted, selector)
		}
	}
	if len(emitted) != 4 {
		t.Fatalf("emitted selectors = %#v", emitted)
	}
	for _, selector := range emitted {
		if err := source.ValidateLiveIgnoreSelectors([]string{selector}); err != nil {
			t.Errorf("generated selector rejected: %v", err)
		}
	}
	projected, projection, _, err := InspectObserverCatalog(ctx, fixture.URL, nil, emitted, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Unsupported()) != 0 || len(projection.LiveIgnored) != 4 {
		t.Fatalf("unsupported = %#v, live ignored = %#v", projected.Unsupported(), projection.LiveIgnored)
	}
}
