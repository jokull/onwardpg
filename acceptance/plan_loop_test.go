package acceptance_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/testkit"
)

func TestPlanLoopRestackMatrix(t *testing.T) {
	requireAcceptance(t)
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	cases := []struct {
		name             string
		baseline         string
		feature          string
		upstream         string
		combined         string
		wantFeatureSQL   []string
		forbidFeatureSQL []string
		wantAbsorbed     bool
	}{
		{
			name:             "unrelated upstream additive retains feature",
			baseline:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`,
			feature:          `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, booking_date date);`,
			upstream:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY); CREATE TABLE app.audit_log (id bigint PRIMARY KEY);`,
			combined:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, booking_date date); CREATE TABLE app.audit_log (id bigint PRIMARY KEY);`,
			wantFeatureSQL:   []string{`ADD COLUMN "booking_date" date`},
			forbidFeatureSQL: []string{`CREATE TABLE "app"."audit_log"`},
		},
		{
			name:             "same table upstream additive retains feature",
			baseline:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`,
			feature:          `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, booking_date date);`,
			upstream:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);`,
			combined:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text, booking_date date);`,
			wantFeatureSQL:   []string{`ADD COLUMN "booking_date" date`},
			forbidFeatureSQL: []string{`ADD COLUMN "timezone" text`},
		},
		{
			name:             "partial absorption shrinks remaining feature",
			baseline:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`,
			feature:          `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text, booking_date date);`,
			upstream:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);`,
			combined:         `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text, booking_date date);`,
			wantFeatureSQL:   []string{`ADD COLUMN "booking_date" date`},
			forbidFeatureSQL: []string{`ADD COLUMN "timezone" text`},
		},
		{
			name:         "full absorption retires active feature",
			baseline:     `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`,
			feature:      `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);`,
			upstream:     `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);`,
			combined:     `CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);`,
			wantAbsorbed: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			workspace, upstreamRoot, environment := planLoopBaseline(t, ctx, adminURL, []byte(test.baseline))

			if err := workspace.WriteSchema([]byte(test.feature)); err != nil {
				t.Fatal(err)
			}
			initial := planLoopPlan(t, ctx, workspace.Root, environment, "feature")
			if initial.Durable.Status != "planned" || initial.Durable.PlanID == "" || initial.Durable.Generation < 1 {
				t.Fatalf("initial feature plan = %#v", initial.Durable)
			}

			planLoopGenerateUpstream(t, ctx, upstreamRoot, environment, []byte(test.upstream), "upstream")
			planLoopCopyBundle(t, upstreamRoot, workspace.Root, workspace.Target, "upstream")
			if err := workspace.WriteSchema([]byte(test.combined)); err != nil {
				t.Fatal(err)
			}

			result := runOK(t, ctx, workspace.Root, environment, "plan", "--target", workspace.Target, "--output", "json")
			var restacked testkit.PlanEnvelope
			if err := result.DecodeJSON(&restacked); err != nil {
				t.Fatal(err)
			}
			featurePath := workspace.BundlePath("feature")
			if test.wantAbsorbed {
				if restacked.Durable.Status != "absorbed" {
					t.Fatalf("full absorption status = %#v\n%s", restacked.Durable, result.Failure())
				}
				if _, err := os.Stat(featurePath); !os.IsNotExist(err) {
					t.Fatalf("fully absorbed feature bundle remains: %v", err)
				}
				return
			}

			if restacked.Durable.Status != "planned" {
				t.Fatalf("restack status = %#v\n%s", restacked.Durable, result.Failure())
			}
			if restacked.Durable.PlanID != initial.Durable.PlanID {
				t.Fatalf("PlanID changed across restack: before=%s after=%s", initial.Durable.PlanID, restacked.Durable.PlanID)
			}
			if restacked.Durable.Generation <= initial.Durable.Generation {
				t.Fatalf("generation did not advance across restack: before=%d after=%d", initial.Durable.Generation, restacked.Durable.Generation)
			}
			phase := planLoopReadPhase(t, workspace.PhasePath("feature", "expand"))
			for _, fragment := range test.wantFeatureSQL {
				if !strings.Contains(phase, fragment) {
					t.Errorf("restacked feature missing %q:\n%s", fragment, phase)
				}
			}
			for _, fragment := range test.forbidFeatureSQL {
				if strings.Contains(phase, fragment) {
					t.Errorf("restacked feature absorbed upstream SQL %q:\n%s", fragment, phase)
				}
			}
			runOK(t, ctx, workspace.Root, environment, "verify", "--target", workspace.Target, "--bundle", "feature")
		})
	}
}

func TestPlanLoopRestackRetainsSafeEditedPocket(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	baseline := []byte(`CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY);`)
	feature := []byte(`CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY, status text NOT NULL);`)
	upstream := []byte(`CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY); CREATE TABLE app.audit_log (id bigint PRIMARY KEY);`)
	combined := []byte(`CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY, status text NOT NULL); CREATE TABLE app.audit_log (id bigint PRIMARY KEY);`)
	workspace, upstreamRoot, environment := planLoopBaseline(t, ctx, adminURL, baseline)
	if err := workspace.WriteSchema(feature); err != nil {
		t.Fatal(err)
	}
	pending := runExit(t, ctx, workspace.Root, environment, 2,
		"plan", "required-status", "--target", workspace.Target, "--output", "json",
		"--hint", `{"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"}`,
		"--hint", `{"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]}`,
	)
	var pendingReport testkit.PlanEnvelope
	if err := pending.DecodeJSON(&pendingReport); err != nil {
		t.Fatal(err)
	}
	if pendingReport.Durable.Status != "needs_sql_edits" || len(pendingReport.Durable.Edits) != 1 {
		t.Fatalf("required-column handoff = %#v", pendingReport.Durable)
	}
	contractPath := workspace.PhasePath("required-status", "contract")
	const authoredMarker = "onwardpg acceptance: preserve this reviewed cleanup across unrelated restack"
	authoredSQL := []byte("-- " + authoredMarker + "\nUPDATE \"app\".\"bookings\" SET \"status\" = 'pending' WHERE \"status\" IS NULL;\n")
	if err := testkit.EditPhasePocket(contractPath, pendingReport.Durable.Edits[0].PocketID, authoredSQL); err != nil {
		t.Fatal(err)
	}
	before := planLoopPlainPlan(t, ctx, workspace.Root, environment)
	if before.Durable.Status != "planned" {
		t.Fatalf("edited feature did not become planned: %#v", before.Durable)
	}
	planLoopGenerateUpstream(t, ctx, upstreamRoot, environment, upstream, "upstream-audit")
	planLoopCopyBundle(t, upstreamRoot, workspace.Root, workspace.Target, "upstream-audit")
	if err := workspace.WriteSchema(combined); err != nil {
		t.Fatal(err)
	}
	after := planLoopPlainPlan(t, ctx, workspace.Root, environment)
	if after.Durable.Status != "planned" || after.Durable.PlanID != before.Durable.PlanID || after.Durable.Generation <= before.Durable.Generation {
		t.Fatalf("edited restack identity = before %#v, after %#v", before.Durable, after.Durable)
	}
	contract := planLoopReadPhase(t, contractPath)
	if strings.Count(contract, authoredMarker) != 1 || !strings.Contains(contract, string(authoredSQL)) {
		t.Fatalf("reviewed edit was not retained byte-for-byte once:\n%s", contract)
	}
	if strings.Contains(contract, `CREATE TABLE "app"."audit_log"`) {
		t.Fatalf("edited feature absorbed upstream table creation:\n%s", contract)
	}
	runOK(t, ctx, workspace.Root, environment, "verify", "--target", workspace.Target, "--bundle", "required-status")
}

func TestPlanLoopDevelopmentAheadWorkIsNotAbsorbed(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	baseline := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`)
	feature := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, booking_date date);`)
	upstream := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text);`)
	combined := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, timezone text, booking_date date);`)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	devURL, cleanDev := planLoopNewAdminDatabase(t, ctx, adminURL)
	t.Cleanup(cleanDev)
	fixtureConnection, err := pgx.Connect(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureConnection.Exec(ctx, string(baseline)+` ALTER TABLE app.accounts ADD COLUMN abandoned_branch_value text;`); err != nil {
		fixtureConnection.Close(context.Background())
		t.Fatal(err)
	}
	var aheadColumn bool
	if err := fixtureConnection.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'app' AND table_name = 'accounts' AND column_name = 'abandoned_branch_value'
	)`).Scan(&aheadColumn); err != nil {
		fixtureConnection.Close(context.Background())
		t.Fatal(err)
	}
	if err := fixtureConnection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !aheadColumn {
		t.Fatal("development fixture did not retain its ahead-only column")
	}
	const devEnv = "ONWARDPG_RESTACK_DEV_DATABASE_URL"
	planLoopWriteConfig(t, workspace.Root, workspace.Target, acceptanceDatabaseEnv, devEnv)
	environment := map[string]string{acceptanceDatabaseEnv: adminURL, devEnv: devURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", workspace.Target, "--bundle", "baseline")
	upstreamRoot := t.TempDir()
	planLoopCopyTree(t, workspace.Root, upstreamRoot)
	planLoopWriteConfig(t, upstreamRoot, workspace.Target, acceptanceDatabaseEnv, "")

	if err := workspace.WriteSchema(feature); err != nil {
		t.Fatal(err)
	}
	initial := planLoopPlan(t, ctx, workspace.Root, environment, "feature")
	workspaceSQL := runOK(t, ctx, workspace.Root, environment, "plan", "--target", workspace.Target, "--output", "sql").Stdout
	if !strings.Contains(workspaceSQL, `ADD COLUMN "booking_date" date`) || strings.Contains(workspaceSQL, `DROP COLUMN "abandoned_branch_value"`) {
		t.Fatalf("unsafe first workspace reconciliation:\n%s", workspaceSQL)
	}
	devConnection, err := pgx.Connect(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := devConnection.Exec(ctx, workspaceSQL); err != nil {
		devConnection.Close(context.Background())
		t.Fatalf("apply development SQL externally: %v\n%s", err, workspaceSQL)
	}
	if err := devConnection.Close(ctx); err != nil {
		t.Fatal(err)
	}

	upstreamEnvironment := map[string]string{acceptanceDatabaseEnv: adminURL}
	planLoopGenerateUpstream(t, ctx, upstreamRoot, upstreamEnvironment, upstream, "upstream")
	planLoopCopyBundle(t, upstreamRoot, workspace.Root, workspace.Target, "upstream")
	if err := workspace.WriteSchema(combined); err != nil {
		t.Fatal(err)
	}
	pending := runExit(t, ctx, workspace.Root, environment, 2, "plan", "--target", workspace.Target, "--output", "json")
	var pendingReport testkit.PlanEnvelope
	if err := pending.DecodeJSON(&pendingReport); err != nil {
		t.Fatal(err)
	}
	if pendingReport.Durable.PlanID != initial.Durable.PlanID || pendingReport.Durable.Generation <= initial.Durable.Generation {
		t.Fatalf("ahead-work restack identity = before %#v after %#v", initial.Durable, pendingReport.Durable)
	}
	if !planLoopChoiceContains(pendingReport.NextActions, `"kind":"preserve"`, `"abandoned_branch_value"`) {
		t.Fatalf("development ambiguity did not offer a scoped preserve choice: %#v", pendingReport.NextActions)
	}
	resolvedResult := runOK(t, ctx, workspace.Root, environment,
		"plan", "--target", workspace.Target, "--output", "json",
		"--dev-hint", `{"kind":"preserve","object":"column","name":["app","accounts","abandoned_branch_value"]}`,
	)
	var restacked testkit.PlanEnvelope
	if err := resolvedResult.DecodeJSON(&restacked); err != nil {
		t.Fatal(err)
	}
	if restacked.Durable.PlanID != initial.Durable.PlanID || restacked.Durable.Generation != pendingReport.Durable.Generation {
		t.Fatalf("development choice churned durable restack: pending %#v resolved %#v", pendingReport.Durable, restacked.Durable)
	}
	action := planLoopAction(restacked.NextActions, "workspace_fast_forward")
	if action == nil {
		t.Fatalf("restack did not expose accepted-history fast-forward: %#v", restacked.NextActions)
	}
	if action.Reason != "accepted_history_changed" && action.Reason != "development_database_behind_desired_schema" {
		t.Fatalf("unexpected workspace fast-forward reason after scoped preserve choice: %#v", action)
	}
	if !strings.Contains(action.SQL, `ADD COLUMN "timezone" text`) || strings.Contains(action.SQL, `booking_date`) || strings.Contains(action.SQL, `abandoned_branch_value`) {
		t.Fatalf("fast-forward absorbed feature or ahead work: %#v", action)
	}
	featurePhase := planLoopReadPhase(t, workspace.PhasePath("feature", "expand"))
	if !strings.Contains(featurePhase, `ADD COLUMN "booking_date" date`) || strings.Contains(featurePhase, `timezone`) || strings.Contains(featurePhase, `abandoned_branch_value`) {
		t.Fatalf("durable feature absorbed upstream/development state:\n%s", featurePhase)
	}
}

func TestPlanLoopDevelopmentManualSQLHasAction(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	baseline := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`)
	desired := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, status text NOT NULL);`)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	devURL, cleanDev := planLoopNewAdminDatabase(t, ctx, adminURL)
	t.Cleanup(cleanDev)
	development, err := pgx.Connect(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := development.Exec(ctx, string(baseline)); err != nil {
		_ = development.Close(context.Background())
		t.Fatal(err)
	}
	if err := development.Close(ctx); err != nil {
		t.Fatal(err)
	}
	const devEnv = "ONWARDPG_MANUAL_SQL_DEV_DATABASE_URL"
	planLoopWriteConfig(t, workspace.Root, workspace.Target, acceptanceDatabaseEnv, devEnv)
	environment := map[string]string{acceptanceDatabaseEnv: adminURL, devEnv: devURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", workspace.Target, "--bundle", "baseline")
	if err := workspace.WriteSchema(desired); err != nil {
		t.Fatal(err)
	}
	hints := []string{
		"--hint", `{"kind":"reconcile","object":"column","name":["app","accounts","status"],"strategy":"manual_sql"}`,
		"--hint", `{"kind":"manual_sql","object":"column","name":["app","accounts","status"],"action":"reconcile_contract_sql"}`,
		"--dev-hint", `{"kind":"reconcile","object":"column","name":["app","accounts","status"],"strategy":"manual_sql"}`,
		"--dev-hint", `{"kind":"manual_sql","object":"column","name":["app","accounts","status"],"action":"reconcile_contract_sql"}`,
	}
	arguments := append([]string{"plan", "manual-development-status", "--target", workspace.Target, "--output", "json"}, hints...)
	result := runExit(t, ctx, workspace.Root, environment, 2, arguments...)
	var report testkit.PlanEnvelope
	if err := result.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	action := planLoopAction(report.NextActions, "inspect_manual_sql_handoff")
	if report.Status != "needs_action" || report.Development.Status != "needs_sql_edits" || action == nil {
		t.Fatalf("development manual-SQL handoff = %#v", report)
	}
	if action.Scope != "development" || len(action.Argv) < 5 || !reflect.DeepEqual(action.Argv[:3], []string{"onwardpg", "dev", "plan"}) || action.Resolution == "" {
		t.Fatalf("development manual-SQL action = %#v", action)
	}
	textArguments := append([]string{"plan", "--target", workspace.Target, "--output", "text"}, hints...)
	textResult := runExit(t, ctx, workspace.Root, environment, 2, textArguments...)
	if !strings.Contains(textResult.Stdout, "development SQL handoff:") || !strings.Contains(textResult.Stdout, "'onwardpg' 'dev' 'plan'") {
		t.Fatalf("development manual-SQL text handoff:\n%s", textResult.Stdout)
	}
}

func TestPlanLoopBranchSwitchRestoresParkedPlanIdentity(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	baseline := []byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY);`)
	workspace, _, environment := planLoopBaseline(t, ctx, adminURL, baseline)
	if err := workspace.WriteSchema([]byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, branch_a text);`)); err != nil {
		t.Fatal(err)
	}
	branchA := planLoopPlan(t, ctx, workspace.Root, environment, "branch-a")
	parkedA := filepath.Join(workspace.Root, "branch-a.checkout")
	if err := os.Rename(workspace.BundlePath("branch-a"), parkedA); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteSchema([]byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, branch_b text);`)); err != nil {
		t.Fatal(err)
	}
	branchB := planLoopPlan(t, ctx, workspace.Root, environment, "branch-b")
	if branchB.Durable.PlanID == branchA.Durable.PlanID {
		t.Fatalf("distinct checkout plans reused PlanID %s", branchA.Durable.PlanID)
	}
	parkedB := filepath.Join(workspace.Root, "branch-b.checkout")
	if err := os.Rename(workspace.BundlePath("branch-b"), parkedB); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parkedA, workspace.BundlePath("branch-a")); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteSchema([]byte(`CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint PRIMARY KEY, branch_a text);`)); err != nil {
		t.Fatal(err)
	}
	restored := planLoopPlan(t, ctx, workspace.Root, environment, "branch-a")
	if restored.Durable.PlanID != branchA.Durable.PlanID {
		t.Fatalf("returning checkout lost parked PlanID: original=%s restored=%s", branchA.Durable.PlanID, restored.Durable.PlanID)
	}
	if restored.Durable.Generation != branchA.Durable.Generation {
		t.Fatalf("unchanged returning checkout churned generation: original=%d restored=%d", branchA.Durable.Generation, restored.Durable.Generation)
	}
}

func planLoopBaseline(t *testing.T, ctx context.Context, adminURL string, baseline []byte) (*testkit.Workspace, string, map[string]string) {
	t.Helper()
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	if err := workspace.WriteSchema(baseline); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "config", "check")
	runOK(t, ctx, workspace.Root, environment, "init", "--target", workspace.Target, "--bundle", "baseline")
	upstreamRoot := t.TempDir()
	planLoopCopyTree(t, workspace.Root, upstreamRoot)
	return workspace, upstreamRoot, environment
}

func planLoopGenerateUpstream(t *testing.T, ctx context.Context, root string, environment map[string]string, desired []byte, bundleID string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "schema.sql"), desired, 0o600); err != nil {
		t.Fatal(err)
	}
	report := planLoopPlan(t, ctx, root, environment, bundleID)
	if report.Durable.Status != "planned" {
		t.Fatalf("upstream bundle %s = %#v", bundleID, report.Durable)
	}
	runOK(t, ctx, root, environment, "verify", "--target", "app", "--bundle", bundleID)
}

func planLoopPlan(t *testing.T, ctx context.Context, root string, environment map[string]string, bundleID string) testkit.PlanEnvelope {
	t.Helper()
	result := runOK(t, ctx, root, environment, "plan", bundleID, "--target", "app", "--output", "json")
	var report testkit.PlanEnvelope
	if err := result.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	return report
}

func planLoopPlainPlan(t *testing.T, ctx context.Context, root string, environment map[string]string) testkit.PlanEnvelope {
	t.Helper()
	result := runOK(t, ctx, root, environment, "plan", "--target", "app", "--output", "json")
	var report testkit.PlanEnvelope
	if err := result.DecodeJSON(&report); err != nil {
		t.Fatal(err)
	}
	return report
}

func planLoopCopyBundle(t *testing.T, fromRoot, toRoot, target, bundleID string) {
	t.Helper()
	from := filepath.Join(fromRoot, "migrations", "onward", target, bundleID)
	to := filepath.Join(toRoot, "migrations", "onward", target, bundleID)
	planLoopCopyTree(t, from, to)
}

func planLoopCopyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := filepath.Walk(from, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(to, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		target, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(target, source)
		closeErr := target.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}); err != nil {
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
}

func planLoopWriteConfig(t *testing.T, root, target, scratchEnv, devEnv string) {
	t.Helper()
	development := ""
	if devEnv != "" {
		development = fmt.Sprintf("dev_database_env = %q\ndev_mode = \"workspace\"\n", devEnv)
	}
	configuration := fmt.Sprintf("version = 1\nbundle_root = \"migrations/onward\"\n\n[targets.%s]\nschema_file = \"schema.sql\"\n%sscratch_database_env = %q\n", target, development, scratchEnv)
	if err := os.WriteFile(filepath.Join(root, ".onwardpg.toml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
}

func planLoopNewAdminDatabase(t *testing.T, ctx context.Context, adminURL string) (string, func()) {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	name := "onwardpg_accept_restack_" + hex.EncodeToString(random)
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`" TEMPLATE template0`); err != nil {
		admin.Close(context.Background())
		t.Fatal(err)
	}
	config, err := url.Parse(adminURL)
	if err != nil {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
		admin.Close(context.Background())
		t.Fatal(err)
	}
	config.Path = "/" + name
	return config.String(), func() {
		if _, err := admin.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("drop plan-loop database %s: %v", name, err)
		}
		if err := admin.Close(context.Background()); err != nil {
			t.Errorf("close plan-loop admin connection: %v", err)
		}
	}
}

func planLoopReadPhase(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func planLoopAction(actions []testkit.NextAction, kind string) *testkit.NextAction {
	for index := range actions {
		if actions[index].Kind == kind {
			return &actions[index]
		}
	}
	return nil
}

func planLoopChoiceContains(actions []testkit.NextAction, fragments ...string) bool {
	for _, action := range actions {
		for _, choice := range action.Choices {
			joined := strings.Join(choice.Argv, "\x00")
			matches := true
			for _, fragment := range fragments {
				if !strings.Contains(joined, fragment) {
					matches = false
					break
				}
			}
			if matches {
				return true
			}
		}
	}
	return false
}
