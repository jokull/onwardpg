package contractcheck

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// TestBlindGauntletRunADependentViewRenameOnPostgreSQL is the durable receipt
// for blind field run A. It keeps both table and view SQL prepared on a legacy
// connection while the generated rename bridge and a reviewed dependent-view
// overlap are installed through a separate deployment connection.
func TestBlindGauntletRunADependentViewRenameOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "onwardpg_blind_a")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ownerURL := restrictedScratchURL(database.Config)

	// region represents an additive migration which landed upstream while the
	// feature plan was in flight. It is part of both the restacked baseline and
	// destination and must survive the compatibility transition unchanged.
	baselineDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  display_name text NOT NULL,
  delivery_tier text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  region text,
  CONSTRAINT accounts_delivery_tier_check CHECK (delivery_tier IN ('email', 'slack'))
);
CREATE VIEW app.account_directory AS
SELECT id, display_name, delivery_tier, region
FROM app.accounts;`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  full_name text NOT NULL,
  account_status text NOT NULL,
  delivery_tier text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  region text,
  CONSTRAINT accounts_delivery_tier_check CHECK (delivery_tier IN ('slack', 'push'))
);
CREATE VIEW app.account_directory AS
SELECT id, full_name, account_status, delivery_tier, region
FROM app.accounts;`

	setup, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, baselineDDL+`
INSERT INTO app.accounts (display_name, delivery_tier, region)
SELECT 'Account ' || value,
       CASE WHEN value % 3 = 0 THEN 'email' ELSE 'slack' END,
       CASE WHEN value % 2 = 0 THEN 'eu' ELSE 'us' END
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
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "blind gauntlet A desired schema", adminURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	generated := buildBlindGauntletRunAPlan(t, current, desired)
	if generated.Status != protocol.NeedsSQLEdits {
		t.Fatalf("dependent-view transition status = %s, want %s: %#v", generated.Status, protocol.NeedsSQLEdits, generated)
	}
	plan := reviewBlindGauntletRunAViewPockets(t, generated)
	assertBlindGauntletRunAPlanShape(t, plan)

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
	if _, err := legacy.Prepare(ctx, "legacy_account_insert", `
INSERT INTO app.accounts (display_name, delivery_tier, region)
VALUES ($1, $2, $3)
RETURNING id, display_name, delivery_tier, region`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Prepare(ctx, "legacy_account_update", `
UPDATE app.accounts SET display_name = $2 WHERE id = $1
RETURNING id, display_name`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Prepare(ctx, "legacy_directory_select", `
SELECT id, display_name, delivery_tier, region
FROM app.account_directory
WHERE id = $1`); err != nil {
		t.Fatal(err)
	}
	var legacyPID int32
	if err := legacy.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&legacyPID); err != nil {
		t.Fatal(err)
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
		t.Fatal("expand must use a connection distinct from the prepared legacy session")
	}
	applyBlindGauntletRunAPhase(t, ctx, deploy, plan, protocol.PhaseExpand)

	var legacyID int64
	var displayName, tier, region string
	if err := legacy.QueryRow(ctx, "legacy_account_insert", "Legacy after expand", "email", "apac").Scan(
		&legacyID, &displayName, &tier, &region,
	); err != nil {
		t.Fatalf("prepared legacy table INSERT failed after expand: %v", err)
	}
	if displayName != "Legacy after expand" || tier != "email" || region != "apac" {
		t.Fatalf("legacy insert returned name=%q tier=%q region=%q", displayName, tier, region)
	}
	if err := legacy.QueryRow(ctx, "legacy_directory_select", legacyID).Scan(&legacyID, &displayName, &tier, &region); err != nil {
		t.Fatalf("prepared legacy view SELECT failed after expand: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_account_update", legacyID, "Legacy renamed").Scan(&legacyID, &displayName); err != nil {
		t.Fatalf("prepared legacy table UPDATE failed after expand: %v", err)
	}

	var newID int64
	var oldProjection, newProjection, status string
	if err := deploy.QueryRow(ctx, `
INSERT INTO app.accounts (full_name, account_status, delivery_tier, region)
VALUES ('New after expand', 'active', 'push', 'eu')
RETURNING id, display_name, full_name, account_status, delivery_tier, region`).Scan(
		&newID, &oldProjection, &newProjection, &status, &tier, &region,
	); err != nil {
		t.Fatalf("new table INSERT failed after expand: %v", err)
	}
	if oldProjection != "New after expand" || newProjection != oldProjection || status != "active" || tier != "push" || region != "eu" {
		t.Fatalf("new insert bridge old=%q new=%q status=%q tier=%q region=%q", oldProjection, newProjection, status, tier, region)
	}
	if err := deploy.QueryRow(ctx, `
SELECT display_name, full_name, account_status, delivery_tier, region
FROM app.account_directory WHERE id = $1`, newID).Scan(
		&oldProjection, &newProjection, &status, &tier, &region,
	); err != nil {
		t.Fatalf("new view SELECT failed after expand: %v", err)
	}
	if oldProjection != newProjection || newProjection != "New after expand" || status != "active" || tier != "push" || region != "eu" {
		t.Fatalf("expanded view old=%q new=%q status=%q tier=%q region=%q", oldProjection, newProjection, status, tier, region)
	}
	var nullableStatus *string
	if err := deploy.QueryRow(ctx, `SELECT display_name, full_name, account_status FROM app.accounts WHERE id = $1`, legacyID).Scan(
		&oldProjection, &newProjection, &nullableStatus,
	); err != nil {
		t.Fatal(err)
	}
	if oldProjection != "Legacy renamed" || newProjection != oldProjection || nullableStatus != nil {
		t.Fatalf("legacy-to-new bridge old=%q new=%q status=%v", oldProjection, newProjection, nullableStatus)
	}
	if err := legacy.QueryRow(ctx, "legacy_directory_select", newID).Scan(&newID, &displayName, &tier, &region); err != nil {
		t.Fatalf("prepared legacy view could not read a new writer's row: %v", err)
	}
	if displayName != "New after expand" || tier != "push" || region != "eu" {
		t.Fatalf("new-to-legacy view result name=%q tier=%q region=%q", displayName, tier, region)
	}

	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	legacyClosed = true
	waitForBlindGauntletBackendDrain(t, ctx, deploy, legacyPID)
	applyBlindGauntletRunAPhase(t, ctx, deploy, plan, protocol.PhaseContract)

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
		t.Fatalf("contract did not converge to the desired catalog\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
	var nullStatuses, legacyTiers int
	if err := deploy.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE account_status IS NULL),
       count(*) FILTER (WHERE delivery_tier = 'email')
FROM app.accounts`).Scan(&nullStatuses, &legacyTiers); err != nil {
		t.Fatal(err)
	}
	if nullStatuses != 0 || legacyTiers != 0 {
		t.Fatalf("contract cleanup left null statuses=%d legacy tiers=%d", nullStatuses, legacyTiers)
	}
	if err := deploy.QueryRow(ctx, `
SELECT full_name, account_status, delivery_tier, region
FROM app.account_directory WHERE id = $1`, legacyID).Scan(
		&newProjection, &status, &tier, &region,
	); err != nil {
		t.Fatalf("final view query failed: %v", err)
	}
	if newProjection != "Legacy renamed" || status != "active" || tier != "push" || region != "apac" {
		t.Fatalf("final view result name=%q status=%q tier=%q region=%q", newProjection, status, tier, region)
	}
	var regionColumns int
	if err := deploy.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
WHERE table_schema = 'app' AND table_name = 'accounts' AND column_name = 'region'`).Scan(&regionColumns); err != nil {
		t.Fatal(err)
	}
	if regionColumns != 1 {
		t.Fatalf("restacked upstream region columns = %d, want 1", regionColumns)
	}
}

func buildBlindGauntletRunAPlan(t *testing.T, current, desired *pgschema.Snapshot) protocol.Result {
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
						Summary: "mark pre-drain accounts active", ExecutionMode: protocol.ManualTransactionalOnce,
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
				t.Fatalf("unexpected run-A decision: %#v", question)
			}
			answers.Answers = append(answers.Answers, answer)
		}
		if len(answers.Answers) == before {
			t.Fatalf("run-A planning made no progress: %#v", result)
		}
	}
	t.Fatal("run-A decisions did not converge")
	return protocol.Result{}
}

func reviewBlindGauntletRunAViewPockets(t *testing.T, generated protocol.Result) protocol.Result {
	t.Helper()
	replacements := map[string]string{
		"reviewed EXPAND SQL for the dependent catalog closure": `CREATE OR REPLACE VIEW app.account_directory AS
SELECT accounts.id,
       accounts.display_name,
       accounts.delivery_tier,
       accounts.region,
       accounts.full_name,
       accounts.account_status
FROM app.accounts AS accounts;`,
		"reviewed CONTRACT SQL that removes the overlap views": `DROP VIEW app.account_directory;`,
		"reviewed CONTRACT SQL that recreates the exact desired dependency closure": `CREATE VIEW app.account_directory AS
SELECT id, full_name, account_status, delivery_tier, region
FROM app.accounts;`,
	}
	counts := make(map[string]int, len(replacements))
	review := func(statement protocol.Statement) protocol.Statement {
		if !strings.Contains(statement.SQL, "ONWARDPG TODO") {
			return statement
		}
		for marker, sql := range replacements {
			if strings.Contains(statement.SQL, marker) {
				statement.SQL = sql
				counts[marker]++
				return statement
			}
		}
		t.Fatalf("unexpected generated TODO pocket: %s", statement.SQL)
		return statement
	}
	for index := range generated.Statements {
		generated.Statements[index] = review(generated.Statements[index])
	}
	// Statements and Batches are two projections of the same generated plan;
	// execute the reviewed batch projection exactly as a release runner would.
	for batchIndex := range generated.Batches {
		for statementIndex := range generated.Batches[batchIndex].Statements {
			generated.Batches[batchIndex].Statements[statementIndex] = review(generated.Batches[batchIndex].Statements[statementIndex])
		}
	}
	for marker := range replacements {
		if counts[marker] != 2 {
			t.Fatalf("reviewed pocket %q appeared %d times across result projections, want 2", marker, counts[marker])
		}
	}
	return generated
}

func assertBlindGauntletRunAPlanShape(t *testing.T, plan protocol.Result) {
	t.Helper()
	var expand, contract strings.Builder
	for _, batch := range plan.Batches {
		for _, statement := range batch.Statements {
			if strings.Contains(statement.SQL, "ONWARDPG TODO") {
				t.Fatalf("reviewed plan retains TODO: %s", statement.SQL)
			}
			switch batch.Phase {
			case protocol.PhaseExpand:
				expand.WriteString(statement.SQL)
				expand.WriteByte('\n')
			case protocol.PhaseContract:
				contract.WriteString(statement.SQL)
				contract.WriteByte('\n')
			}
		}
	}
	for _, fragment := range []string{
		`ADD COLUMN "full_name" text`,
		`ADD COLUMN "account_status" text`,
		`DROP CONSTRAINT "accounts_delivery_tier_check"`,
		`CREATE OR REPLACE VIEW app.account_directory`,
		`accounts.account_status`,
	} {
		if !strings.Contains(expand.String(), fragment) {
			t.Fatalf("expand is missing %q:\n%s", fragment, expand.String())
		}
	}
	addStatusAt := strings.Index(expand.String(), `ADD COLUMN "account_status" text`)
	overlapViewAt := strings.Index(expand.String(), "CREATE OR REPLACE VIEW app.account_directory")
	if addStatusAt < 0 || overlapViewAt < 0 || addStatusAt >= overlapViewAt {
		t.Fatalf("dependent-view overlap precedes its base-column provider:\n%s", expand.String())
	}
	if strings.Contains(expand.String(), `ALTER COLUMN "account_status" SET NOT NULL`) {
		t.Fatalf("expand prematurely enforced the final required column:\n%s", expand.String())
	}
	dropViewAt := strings.Index(contract.String(), "DROP VIEW app.account_directory")
	columnCutoverAt := strings.Index(contract.String(), `DROP COLUMN "full_name"`)
	recreateViewAt := strings.Index(contract.String(), "CREATE VIEW app.account_directory")
	if dropViewAt < 0 || columnCutoverAt < 0 || recreateViewAt < 0 || !(dropViewAt < columnCutoverAt && columnCutoverAt < recreateViewAt) {
		t.Fatalf("dependent-view contract order is unsafe:\n%s", contract.String())
	}
	for _, fragment := range []string{
		`UPDATE app.accounts SET account_status = 'active'`,
		`ALTER COLUMN "account_status" SET NOT NULL`,
		`UPDATE app.accounts SET delivery_tier = 'push'`,
		`VALIDATE CONSTRAINT "accounts_delivery_tier_check"`,
	} {
		if !strings.Contains(contract.String(), fragment) {
			t.Fatalf("contract is missing %q:\n%s", fragment, contract.String())
		}
	}
}

func applyBlindGauntletRunAPhase(t *testing.T, ctx context.Context, conn *pgx.Conn, plan protocol.Result, phase string) {
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
