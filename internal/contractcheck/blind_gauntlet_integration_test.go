package contractcheck

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// TestBlindGauntletCompatibilityWindowKeepsPreparedLegacySQLAliveOnPostgreSQL
// is the durable receipt for blind field run C. In particular, the legacy SQL
// is parsed and prepared before expand. Reconnecting after expand would miss
// the compatibility promise made to already-running application processes.
func TestBlindGauntletCompatibilityWindowKeepsPreparedLegacySQLAliveOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "onwardpg_blind_c")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ownerURL := restrictedScratchURL(database.Config)

	baselineDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  display_name text NOT NULL,
  delivery_tier text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT accounts_delivery_tier_check CHECK (delivery_tier IN ('email', 'slack'))
);`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  full_name text NOT NULL,
  account_status text NOT NULL,
  delivery_tier text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT accounts_delivery_tier_check CHECK (delivery_tier IN ('slack', 'push'))
);`

	setup, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, baselineDDL+`
INSERT INTO app.accounts (display_name, delivery_tier)
SELECT 'Account ' || value, CASE WHEN value % 3 = 0 THEN 'email' ELSE 'slack' END
FROM generate_series(1, 100) AS value;`); err != nil {
		setup.Close(ctx)
		t.Fatal(err)
	}
	if err := setup.Close(ctx); err != nil {
		t.Fatal(err)
	}

	current, err := source.LoadGraphForComparison(ctx, source.Parse(ownerURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "blind gauntlet C desired schema", adminURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := buildBlindGauntletCPlan(t, current, desired)
	if plan.Status != protocol.Planned {
		t.Fatalf("gauntlet plan is not executable: %#v", plan)
	}
	assertBlindGauntletCPlanShape(t, plan)

	legacy, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacyClosed := false
	defer func() {
		if !legacyClosed {
			_ = legacy.Close(context.Background())
		}
	}()
	if _, err := legacy.Prepare(ctx, "legacy_insert", `
INSERT INTO app.accounts (display_name, delivery_tier)
VALUES ($1, $2)
RETURNING id, display_name, delivery_tier`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Prepare(ctx, "legacy_select", `
SELECT id, display_name, delivery_tier
FROM app.accounts
WHERE id = $1`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Prepare(ctx, "legacy_update", `
UPDATE app.accounts
SET display_name = $2
WHERE id = $1
RETURNING id, display_name`); err != nil {
		t.Fatal(err)
	}
	var legacyPID int32
	if err := legacy.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&legacyPID); err != nil {
		t.Fatal(err)
	}
	var preparedCount int
	if err := legacy.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE name LIKE 'legacy_%'`).Scan(&preparedCount); err != nil {
		t.Fatal(err)
	}
	if preparedCount != 3 {
		t.Fatalf("prepared legacy statements = %d, want 3", preparedCount)
	}

	deploy, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deploy.Close(context.Background())
	var deployPID int32
	if err := deploy.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&deployPID); err != nil {
		t.Fatal(err)
	}
	if deployPID == legacyPID {
		t.Fatal("expand must be applied through a connection distinct from the prepared legacy session")
	}
	applyBlindGauntletPhase(t, ctx, deploy, plan, protocol.PhaseExpand)

	var legacyID int64
	var displayName, tier string
	if err := legacy.QueryRow(ctx, "legacy_insert", "Legacy post-expand", "email").Scan(&legacyID, &displayName, &tier); err != nil {
		t.Fatalf("prepared legacy INSERT failed after expand: %v", err)
	}
	if displayName != "Legacy post-expand" || tier != "email" {
		t.Fatalf("prepared legacy INSERT returned display_name=%q tier=%q", displayName, tier)
	}
	if err := legacy.QueryRow(ctx, "legacy_select", legacyID).Scan(&legacyID, &displayName, &tier); err != nil {
		t.Fatalf("prepared legacy SELECT failed after expand: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_update", legacyID, "Legacy renamed").Scan(&legacyID, &displayName); err != nil {
		t.Fatalf("prepared legacy UPDATE failed after expand: %v", err)
	}
	if displayName != "Legacy renamed" {
		t.Fatalf("prepared legacy UPDATE returned display_name=%q", displayName)
	}

	var newID int64
	var oldProjection, newProjection, status string
	if err := deploy.QueryRow(ctx, `
INSERT INTO app.accounts (full_name, account_status, delivery_tier)
VALUES ('New post-expand', 'active', 'push')
RETURNING id, display_name, full_name, account_status, delivery_tier`).Scan(
		&newID, &oldProjection, &newProjection, &status, &tier,
	); err != nil {
		t.Fatalf("new INSERT failed against expanded schema: %v", err)
	}
	if oldProjection != "New post-expand" || newProjection != oldProjection || status != "active" || tier != "push" {
		t.Fatalf("new INSERT bridge result old=%q new=%q status=%q tier=%q", oldProjection, newProjection, status, tier)
	}
	if _, err := deploy.Exec(ctx, `UPDATE app.accounts SET full_name = 'New renamed' WHERE id = $1`, newID); err != nil {
		t.Fatalf("new UPDATE failed against expanded schema: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_select", newID).Scan(&newID, &displayName, &tier); err != nil {
		t.Fatalf("prepared legacy SELECT could not see a new writer's row: %v", err)
	}
	if displayName != "New renamed" || tier != "push" {
		t.Fatalf("new-to-legacy synchronization display_name=%q tier=%q", displayName, tier)
	}
	var nullableStatus *string
	if err := deploy.QueryRow(ctx, `SELECT display_name, full_name, account_status FROM app.accounts WHERE id = $1`, legacyID).Scan(
		&oldProjection, &newProjection, &nullableStatus,
	); err != nil {
		t.Fatal(err)
	}
	if oldProjection != "Legacy renamed" || newProjection != oldProjection || nullableStatus != nil {
		t.Fatalf("legacy-to-new synchronization old=%q new=%q status=%v", oldProjection, newProjection, nullableStatus)
	}

	_, err = deploy.Exec(ctx, `
INSERT INTO app.accounts (display_name, full_name, account_status, delivery_tier)
VALUES ('contradictory old value', 'contradictory new value', 'active', 'slack')`)
	var conflict *pgconn.PgError
	if !errors.As(err, &conflict) || conflict.Code != "23514" {
		t.Fatalf("contradictory dual write error = %v, want SQLSTATE 23514", err)
	}

	var legacyBackendAlive bool
	if err := deploy.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, legacyPID).Scan(&legacyBackendAlive); err != nil {
		t.Fatal(err)
	}
	if !legacyBackendAlive {
		t.Fatal("legacy backend disappeared before contract readiness was checked")
	}
	expandSnapshot, err := source.LoadGraphForComparison(ctx, source.Parse(ownerURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := buildBlindGauntletArtifact(t, current, desired, plan)
	expandFingerprint, err := expandSnapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err = bundle.WithExpandCheckpoint(artifact, expandFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	readiness, err := Run(ctx, Input{
		Artifact: artifact, ExpectedHead: artifact.Manifest.History.EntryDigest,
		DatabaseURL: ownerURL, Environment: "gauntlet", Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Status != "needs_evidence" {
		t.Fatalf("contract readiness without writer-drain evidence = %#v", readiness)
	}
	writerGateFound := false
	for _, gate := range plan.ContractGates {
		if gate.Kind == "writer_attestation" {
			writerGateFound = true
			break
		}
	}
	if !writerGateFound {
		t.Fatalf("compatibility-removing contract has no writer attestation gate: %#v", plan.ContractGates)
	}

	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	legacyClosed = true
	waitForBlindGauntletBackendDrain(t, ctx, deploy, legacyPID)
	applyBlindGauntletPhase(t, ctx, deploy, plan, protocol.PhaseContract)

	actual, err := source.LoadGraphForComparison(ctx, source.Parse(ownerURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantFingerprint, err := desired.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	gotFingerprint, err := actual.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if gotFingerprint != wantFingerprint {
		wantJSON, _ := desired.CanonicalJSON()
		gotJSON, _ := actual.CanonicalJSON()
		t.Fatalf("contract did not reach strict desired catalog\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
	var nullStatuses, legacyTiers int
	if err := deploy.QueryRow(ctx, `SELECT count(*) FILTER (WHERE account_status IS NULL), count(*) FILTER (WHERE delivery_tier = 'email') FROM app.accounts`).Scan(&nullStatuses, &legacyTiers); err != nil {
		t.Fatal(err)
	}
	if nullStatuses != 0 || legacyTiers != 0 {
		t.Fatalf("contract cleanup left null statuses=%d legacy tiers=%d", nullStatuses, legacyTiers)
	}
	if err := deploy.QueryRow(ctx, `
INSERT INTO app.accounts (full_name, account_status, delivery_tier)
VALUES ('Final writer', 'active', 'push')
RETURNING full_name, account_status, delivery_tier`).Scan(&newProjection, &status, &tier); err != nil {
		t.Fatalf("final new SQL failed: %v", err)
	}
	if newProjection != "Final writer" || status != "active" || tier != "push" {
		t.Fatalf("final new SQL returned name=%q status=%q tier=%q", newProjection, status, tier)
	}
	_, err = deploy.Exec(ctx, `SELECT display_name FROM app.accounts LIMIT 1`)
	var missingLegacyColumn *pgconn.PgError
	if !errors.As(err, &missingLegacyColumn) || missingLegacyColumn.Code != "42703" {
		t.Fatalf("legacy-only column after contract error = %v, want SQLSTATE 42703", err)
	}
}

func waitForBlindGauntletBackendDrain(t *testing.T, ctx context.Context, conn *pgx.Conn, pid int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var drained bool
		if err := conn.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&drained); err != nil {
			t.Fatal(err)
		}
		if drained {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy backend %d still exists after the bounded writer-drain wait", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func buildBlindGauntletCPlan(t *testing.T, current, desired *pgschema.Snapshot) protocol.Result {
	t.Helper()
	tableID := (pgschema.Table{Schema: "app", Name: "accounts"}).ObjectID()
	oldColumnID := (pgschema.Column{Table: tableID, Name: "display_name"}).ObjectID().String()
	newColumnID := (pgschema.Column{Table: tableID, Name: "full_name"}).ObjectID().String()
	answers := protocol.Answers{}
	for attempt := 0; attempt < 10; attempt++ {
		result, err := graphplan.Build(current, desired, answers, graphplan.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != protocol.NeedsInput {
			return result
		}
		if answers.CurrentFingerprint == "" {
			answers.CurrentFingerprint = result.CurrentFingerprint
			answers.DesiredFingerprint = result.DesiredFingerprint
		}
		before := len(answers.Answers)
		for _, question := range result.Questions {
			answer := protocol.Answer{Kind: question.Kind, Key: question.Key, QuestionFingerprint: question.ScopeFingerprint}
			switch question.Kind {
			case "rename_column":
				if question.Key != oldColumnID {
					t.Fatalf("unexpected rename question: %#v", question)
				}
				answer.Value = newColumnID
			case "rename_backfill_strategy":
				answer.Value = "single_transaction"
			case "reconcile_contract":
				answer.Value = "manual_sql"
			case "reconcile_contract_sql":
				answer.Value = "provided"
				switch {
				case strings.Contains(question.Key, "account_status"):
					answer.Manual = &protocol.ManualWork{
						Summary: "mark accounts written before drain active", ExecutionMode: protocol.ManualTransactionalOnce,
						Statements:      []string{`UPDATE app.accounts SET account_status = 'active' WHERE account_status IS NULL;`},
						VerificationSQL: []string{`SELECT NOT EXISTS (SELECT 1 FROM app.accounts WHERE account_status IS NULL);`},
					}
				case strings.Contains(question.Key, "accounts_delivery_tier_check"):
					answer.Manual = &protocol.ManualWork{
						Summary: "map the retired email tier to push", ExecutionMode: protocol.ManualTransactionalOnce,
						Statements:      []string{`UPDATE app.accounts SET delivery_tier = 'push' WHERE delivery_tier = 'email';`},
						VerificationSQL: []string{`SELECT NOT EXISTS (SELECT 1 FROM app.accounts WHERE (delivery_tier IN ('slack', 'push')) IS FALSE);`},
					}
				default:
					t.Fatalf("unexpected manual reconciliation target: %#v", question)
				}
			default:
				t.Fatalf("unexpected blind gauntlet decision: %#v", question)
			}
			answers.Answers = append(answers.Answers, answer)
		}
		if len(answers.Answers) == before {
			t.Fatalf("blind gauntlet planning made no progress: %#v", result)
		}
	}
	t.Fatal("blind gauntlet decisions did not converge")
	return protocol.Result{}
}

func assertBlindGauntletCPlanShape(t *testing.T, plan protocol.Result) {
	t.Helper()
	var expandSQL, contractSQL strings.Builder
	for _, batch := range plan.Batches {
		for _, statement := range batch.Statements {
			switch batch.Phase {
			case protocol.PhaseExpand:
				expandSQL.WriteString(statement.SQL)
				expandSQL.WriteByte('\n')
			case protocol.PhaseContract:
				contractSQL.WriteString(statement.SQL)
				contractSQL.WriteByte('\n')
			}
		}
	}
	expand := expandSQL.String()
	contract := contractSQL.String()
	for _, fragment := range []string{`ADD COLUMN "full_name" text`, `ADD COLUMN "account_status" text`, `DROP CONSTRAINT "accounts_delivery_tier_check"`, "CREATE TRIGGER"} {
		if !strings.Contains(expand, fragment) {
			t.Fatalf("expand is missing %q:\n%s", fragment, expand)
		}
	}
	if strings.Contains(expand, `"account_status" text NOT NULL`) || strings.Contains(expand, `ALTER COLUMN "account_status" SET NOT NULL`) {
		t.Fatalf("expand prematurely enforces the future-required column:\n%s", expand)
	}
	for _, fragment := range []string{`ALTER COLUMN "account_status" SET NOT NULL`, `UPDATE app.accounts SET account_status = 'active'`, `UPDATE app.accounts SET delivery_tier = 'push'`, `VALIDATE CONSTRAINT "accounts_delivery_tier_check"`} {
		if !strings.Contains(contract, fragment) {
			t.Fatalf("contract is missing %q:\n%s", fragment, contract)
		}
	}
}

func applyBlindGauntletPhase(t *testing.T, ctx context.Context, conn *pgx.Conn, plan protocol.Result, phase string) {
	t.Helper()
	for _, batch := range plan.Batches {
		if batch.Phase != phase {
			continue
		}
		if !batch.Transactional {
			for _, statement := range batch.Statements {
				if _, err := conn.Exec(ctx, statement.SQL); err != nil {
					t.Fatalf("apply %s statement %q: %v", phase, statement.SQL, err)
				}
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range batch.Statements {
			if _, err := tx.Exec(ctx, statement.SQL); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("apply %s statement %q: %v", phase, statement.SQL, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func buildBlindGauntletArtifact(t *testing.T, current, desired *pgschema.Snapshot, plan protocol.Result) bundle.Artifact {
	t.Helper()
	currentFingerprint, err := current.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	desiredFingerprint, err := desired.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: bundle.Metadata{
		BundleID: "blind-gauntlet-c", PlanID: "plan_0123456789abcdef0123456789abcdef", Generation: 1,
		Target: "primary", Purpose: "feature",
		BaselineSource: bundle.SourceReceipt{Kind: "database", Description: "blind gauntlet C baseline", Fingerprint: currentFingerprint},
		DesiredSource:  bundle.SourceReceipt{Kind: "database", Description: "blind gauntlet C desired", Fingerprint: desiredFingerprint},
		Planner:        bundle.PlannerReceipt{Version: "integration-test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}, Result: plan})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
