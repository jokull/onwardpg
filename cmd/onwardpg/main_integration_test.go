package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/driftcheck"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/historyinit"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
)

func TestDriftCheckComparesReplayedHeadReadOnlyOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	liveURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	ddl := "CREATE SCHEMA app; CREATE TABLE app.users (id bigint, email text);\n"
	writeTestFile(t, repository, "schema.sql", ddl)
	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	connection, err := pgx.Connect(context.Background(), liveURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), ddl+"CREATE INDEX users_email_manual_idx ON app.users (email);"); err != nil {
		t.Fatal(err)
	}

	driftedOutput := captureStdout(t, func() int {
		return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
	})
	if driftedOutput.code != 4 {
		t.Fatalf("drifted exit = %d, stdout = %s", driftedOutput.code, driftedOutput.stdout)
	}
	var drifted driftcheck.Report
	if err := json.Unmarshal([]byte(driftedOutput.stdout), &drifted); err != nil {
		t.Fatal(err)
	}
	if drifted.Outcome != "drifted" || len(drifted.Differences) == 0 {
		t.Fatalf("drift report = %#v", drifted)
	}
	foundIndex := false
	for _, difference := range drifted.Differences {
		if difference.Kind == "unexpected_in_actual" && strings.Contains(difference.ObjectID, "users_email_manual_idx") {
			foundIndex = true
		}
	}
	if !foundIndex {
		t.Fatalf("drift did not report the unexpected index: %#v", drifted.Differences)
	}
	var stillExists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass('app.users_email_manual_idx') IS NOT NULL`).Scan(&stillExists); err != nil {
		t.Fatal(err)
	}
	if !stillExists {
		t.Fatal("drift check modified the live database")
	}
	if _, err := connection.Exec(context.Background(), "DROP INDEX app.users_email_manual_idx;"); err != nil {
		t.Fatal(err)
	}
	cleanOutput := captureStdout(t, func() int {
		return runDriftAt([]string{"check", "--target", "primary", "--database", liveURL}, repository)
	})
	if cleanOutput.code != 0 {
		t.Fatalf("clean drift exit = %d, stdout = %s", cleanOutput.code, cleanOutput.stdout)
	}
	var clean driftcheck.Report
	if err := json.Unmarshal([]byte(cleanOutput.stdout), &clean); err != nil {
		t.Fatal(err)
	}
	if clean.Outcome != "drift_free" || len(clean.Differences) != 0 {
		t.Fatalf("clean drift report = %#v", clean)
	}
}

func TestGitFreeInitDraftHintVerifyAndRestackOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanupDev := createTestDatabase(t, adminURL)
	defer cleanupDev()
	t.Setenv("ONWARDPG_ITERATION_DEV_DATABASE_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_ITERATION_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);\n"
	writeTestFile(t, repository, "schema.sql", baseDDL)

	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	if _, err := os.Stat(filepath.Join(repository, ".git")); !os.IsNotExist(err) {
		t.Fatalf("workflow unexpectedly created or required Git metadata: %v", err)
	}

	featureDDL := "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint);\n"
	writeTestFile(t, repository, "schema.sql", featureDDL)
	pendingOutput := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if pendingOutput.code != 2 {
		t.Fatalf("pending draft exit = %d, stdout = %s", pendingOutput.code, pendingOutput.stdout)
	}
	var pending struct {
		Protocol  string              `json:"protocol"`
		Status    string              `json:"status"`
		Decisions []protocol.Decision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(pendingOutput.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Protocol != draftflow.Version || pending.Status != "needs_decisions" || len(pending.Decisions) == 0 {
		t.Fatalf("pending draft = %#v", pending)
	}
	var renameHint *protocol.Hint
	for _, decision := range pending.Decisions {
		for _, choice := range decision.Choices {
			if choice.Hint.Kind == "rename" && choice.Hint.Object == "table" {
				hint := choice.Hint
				renameHint = &hint
			}
		}
	}
	if renameHint == nil {
		t.Fatalf("draft did not offer a semantic table rename: %#v", pending.Decisions)
	}
	hintData, err := json.Marshal(renameHint)
	if err != nil {
		t.Fatal(err)
	}
	plannedOutput := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-rename", "--hint", string(hintData)}, repository)
	})
	if plannedOutput.code != 0 {
		t.Fatalf("planned draft exit = %d, stdout = %s", plannedOutput.code, plannedOutput.stdout)
	}
	var planned draftflow.Report
	if err := json.Unmarshal([]byte(plannedOutput.stdout), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Outcome != string(protocol.Planned) || planned.Verification == nil || planned.Verification.Outcome != "verified" {
		t.Fatalf("planned draft = %#v", planned)
	}
	artifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "customer-rename"))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.History == nil || artifact.Manifest.History.ParentDigest != planned.HistoryHead {
		t.Fatalf("draft manifest = %#v", artifact.Manifest)
	}
	if artifact.Manifest.AnswersDigest == "" || artifact.Manifest.QuestionsDigest == "" || artifact.Manifest.SemanticDigest == "" {
		t.Fatalf("draft did not retain semantic and fingerprint-bound decision receipts: %#v", artifact.Manifest)
	}
	receiptedHints, err := bundle.SemanticHints(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(receiptedHints) != 1 || receiptedHints[0].Kind != "rename" {
		t.Fatalf("semantic decisions = %#v", receiptedHints)
	}
	verified := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if verified.code != 0 {
		t.Fatalf("verify exit = %d, stdout = %s", verified.code, verified.stdout)
	}

	// Simulate an agent rebasing files: an upstream bundle now follows the old
	// baseline while the explicitly selected feature still names that baseline
	// as its parent. No Git API is involved in detecting or repairing the fork.
	featurePath := filepath.Join(repository, "onward-bundles", "primary", "customer-rename")
	parkedPath := filepath.Join(repository, "customer-rename.parked")
	if err := os.Rename(featurePath, parkedPath); err != nil {
		t.Fatal(err)
	}
	upstreamDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint); CREATE TABLE app.audit_log (id bigint);\n"
	writeHistoryTransitionFixture(t, repository, adminURL, "upstream-audit", upstreamDDL)
	if err := os.Rename(parkedPath, featurePath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.audit_log (id bigint);\n")

	restackedOutput := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if restackedOutput.code != 0 {
		t.Fatalf("restacked draft exit = %d, stdout = %s", restackedOutput.code, restackedOutput.stdout)
	}
	var restacked draftflow.Report
	if err := json.Unmarshal([]byte(restackedOutput.stdout), &restacked); err != nil {
		t.Fatal(err)
	}
	if !restacked.ParentChanged || restacked.PreviousParent == restacked.HistoryHead || restacked.AnswerRebind == nil || len(restacked.AnswerRebind.Carried) == 0 {
		t.Fatalf("restacked draft = %#v", restacked)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 3 || chain.Entries[1].Directory != "upstream-audit" || chain.Entries[2].Directory != "customer-rename" {
		t.Fatalf("restacked chain = %#v", chain)
	}

	// A second independent base migration lands during the same feature. The
	// coding agent brings those files into the checkout; onwardpg again needs
	// only the selected bundle ID to distinguish the movable tip.
	if err := os.Rename(featurePath, parkedPath); err != nil {
		t.Fatal(err)
	}
	secondUpstreamDDL := "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n"
	writeHistoryTransitionFixture(t, repository, adminURL, "upstream-settings", secondUpstreamDDL)
	if err := os.Rename(parkedPath, featurePath); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n")
	secondRestackOutput := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if secondRestackOutput.code != 0 {
		t.Fatalf("second base restack exit = %d, stdout = %s", secondRestackOutput.code, secondRestackOutput.stdout)
	}
	var secondRestack draftflow.Report
	if err := json.Unmarshal([]byte(secondRestackOutput.stdout), &secondRestack); err != nil {
		t.Fatal(err)
	}
	if !secondRestack.ParentChanged || secondRestack.AnswerRebind == nil || len(secondRestack.AnswerRebind.Carried) == 0 {
		t.Fatalf("second base restack = %#v", secondRestack)
	}
	chain, err = history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 4 || chain.Entries[1].Directory != "upstream-audit" || chain.Entries[2].Directory != "upstream-settings" || chain.Entries[3].Directory != "customer-rename" {
		t.Fatalf("twice-restacked chain = %#v", chain)
	}

	// Ownership now moves to the coding agent. It adds product-specific SQL and
	// a boolean assertion directly to the migration folder. Read-only check mode
	// rejects the unreceipted edits; normal verification executes them only in
	// disposable PostgreSQL and receipts their exact bytes after convergence.
	migrateSQL := `-- Product-aware work owned by the feature agent.
CREATE TEMP TABLE onwardpg_agent_receipt (value text NOT NULL);
INSERT INTO onwardpg_agent_receipt (value) VALUES ('customer-rename');
`
	writeTestFile(t, featurePath, "phases/migrate.sql", migrateSQL)
	writeTestFile(t, featurePath, "verify.sql", `-- onwardpg:assert agent_sql_ran
SELECT count(*) = 1 FROM onwardpg_agent_receipt WHERE value = 'customer-rename';
`)
	unreceiptedCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if unreceiptedCheck.code != 4 {
		t.Fatalf("read-only check exit = %d, want 4: %s", unreceiptedCheck.code, unreceiptedCheck.stdout)
	}
	if !strings.Contains(unreceiptedCheck.stdout, `"code":"unreceipted_sql_edits"`) {
		t.Fatalf("read-only check did not explain the receipt step: %s", unreceiptedCheck.stdout)
	}
	receiptedOutput := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if receiptedOutput.code != 0 {
		t.Fatalf("edited verification exit = %d, stdout = %s", receiptedOutput.code, receiptedOutput.stdout)
	}
	var receiptedReport verify.Report
	if err := json.Unmarshal([]byte(receiptedOutput.stdout), &receiptedReport); err != nil {
		t.Fatal(err)
	}
	if !receiptedReport.ReceiptsUpdated || receiptedReport.Outcome != "verified" {
		t.Fatalf("edited verification = %#v", receiptedReport)
	}
	receiptedArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	if receiptedArtifact.Manifest.PhaseSource != "edited" || receiptedArtifact.Manifest.VerificationDigest == "" || string(receiptedArtifact.Files["phases/migrate.sql"]) != migrateSQL {
		t.Fatalf("receipted edited artifact = %#v", receiptedArtifact.Manifest)
	}
	checked := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if checked.code != 0 {
		t.Fatalf("receipted read-only check exit = %d, stdout = %s", checked.code, checked.stdout)
	}

	// A receipted bundle is not fresh merely because it still converges to its
	// own recorded plan. CI must compare it with the schema the working code now
	// exports and direct the agent back through the same draft loop.
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, unplanned text); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n")
	staleCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if staleCheck.code != 4 {
		t.Fatalf("stale read-only check exit = %d, stdout = %s", staleCheck.code, staleCheck.stdout)
	}
	var staleReport verify.Report
	if err := json.Unmarshal([]byte(staleCheck.stdout), &staleReport); err != nil {
		t.Fatal(err)
	}
	if staleReport.Outcome != "stale" || len(staleReport.Findings) != 1 || staleReport.Findings[0].Code != "working_schema_changed" || staleReport.WorkingFingerprint == staleReport.DesiredFingerprint {
		t.Fatalf("stale read-only report = %#v", staleReport)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.audit_log (id bigint); CREATE TABLE app.settings (id bigint);\n")

	// Applying the current migration to the developer database is not a history
	// transition. The feature may keep evolving, and the same explicit bundle
	// must remain the cumulative H → W draft.
	chain, err = history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	devConnection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	defer devConnection.Close(context.Background())
	for _, entry := range chain.Entries {
		for _, phase := range []string{"expand", "migrate", "contract"} {
			receipt, exists := entry.Artifact.Manifest.Phases[phase]
			if !exists {
				continue
			}
			if _, err := devConnection.Exec(context.Background(), string(entry.Artifact.Files[receipt.Path])); err != nil {
				t.Fatalf("apply %s/%s to disposable developer fixture: %v", entry.Directory, phase, err)
			}
		}
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint); CREATE TABLE app.audit_log (id bigint, note text); CREATE TABLE app.settings (id bigint);\n")

	localOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if localOutput.code != 0 {
		t.Fatalf("local residual plan exit = %d, stdout = %s", localOutput.code, localOutput.stdout)
	}
	var localPlan protocol.Result
	if err := json.Unmarshal([]byte(localOutput.stdout), &localPlan); err != nil {
		t.Fatal(err)
	}
	if len(localPlan.Statements) != 1 || !strings.Contains(localPlan.Statements[0].SQL, "note") {
		t.Fatalf("D → W should contain only the newly added column: %#v", localPlan.Statements)
	}

	redraftedOutput := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if redraftedOutput.code != 0 {
		t.Fatalf("same-bundle redraft exit = %d, stdout = %s", redraftedOutput.code, redraftedOutput.stdout)
	}
	var redrafted draftflow.Report
	if err := json.Unmarshal([]byte(redraftedOutput.stdout), &redrafted); err != nil {
		t.Fatal(err)
	}
	if redrafted.EditReconciliation == nil || redrafted.EditReconciliation.Outcome != "reconciled" || len(redrafted.EditReconciliation.Conflicts) != 0 {
		t.Fatalf("same-bundle reconciliation = %#v", redrafted)
	}
	refreshedArtifact, err := bundle.Read(featurePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(refreshedArtifact.Files["phases/migrate.sql"]) != migrateSQL || string(refreshedArtifact.Files["verify.sql"]) != string(receiptedArtifact.Files["verify.sql"]) {
		t.Fatalf("same-bundle redraft lost agent-owned SQL: %#v", refreshedArtifact.Manifest)
	}
	foundNote := false
	for _, receipt := range refreshedArtifact.Manifest.Phases {
		if strings.Contains(string(refreshedArtifact.Files[receipt.Path]), "note") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatal("same-bundle redraft did not include the new column")
	}
	refreshedCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if refreshedCheck.code != 0 {
		t.Fatalf("redrafted read-only check exit = %d, stdout = %s", refreshedCheck.code, refreshedCheck.stdout)
	}

	// When both the agent and generator change expand, draft moves the bundle's
	// receipts to the new plan but preserves the current SQL as an unreceipted
	// three-way handoff. This makes the conflict resolvable through ordinary SQL
	// editing followed by verify, without a merge DSL or Git knowledge.
	expandReceipt, exists := refreshedArtifact.Manifest.Phases["expand"]
	if !exists {
		t.Fatal("fixture needs an expand phase for the conflict handoff")
	}
	agentExpand := string(refreshedArtifact.Files[expandReceipt.Path]) + "\n-- Agent-owned rollout note retained across regeneration.\n"
	writeTestFile(t, featurePath, expandReceipt.Path, agentExpand)
	receiptAgentExpand := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if receiptAgentExpand.code != 0 {
		t.Fatalf("receipt agent expand edit = %d, %s", receiptAgentExpand.code, receiptAgentExpand.stdout)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint, timezone text); CREATE TABLE app.audit_log (id bigint, note text); CREATE TABLE app.settings (id bigint);\n")
	conflictedOutput := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if conflictedOutput.code != 4 {
		t.Fatalf("same-phase conflict exit = %d, stdout = %s", conflictedOutput.code, conflictedOutput.stdout)
	}
	var conflicted draftflow.Report
	if err := json.Unmarshal([]byte(conflictedOutput.stdout), &conflicted); err != nil {
		t.Fatal(err)
	}
	if conflicted.Outcome != "blocked" || conflicted.EditReconciliation == nil || len(conflicted.EditReconciliation.Conflicts) != 1 {
		t.Fatalf("same-phase conflict report = %#v", conflicted)
	}
	conflict := conflicted.EditReconciliation.Conflicts[0]
	if conflict.Path != expandReceipt.Path || conflict.OldGeneratedSQL == nil || conflict.CurrentSQL == nil || conflict.NewGeneratedSQL == nil || !strings.Contains(*conflict.CurrentSQL, "Agent-owned rollout note") || !strings.Contains(*conflict.NewGeneratedSQL, "timezone") {
		t.Fatalf("same-phase conflict evidence = %#v", conflict)
	}
	preservedExpand, err := os.ReadFile(filepath.Join(featurePath, filepath.FromSlash(conflict.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(preservedExpand), "Agent-owned rollout note") || strings.Contains(string(preservedExpand), "timezone") {
		t.Fatalf("conflict handoff overwrote current SQL: %s", preservedExpand)
	}
	mergedExpand := *conflict.NewGeneratedSQL + "\n-- Agent-owned rollout note retained across regeneration.\n"
	writeTestFile(t, featurePath, conflict.Path, mergedExpand)
	resolvedOutput := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename"}, repository)
	})
	if resolvedOutput.code != 0 {
		t.Fatalf("resolved same-phase conflict = %d, %s", resolvedOutput.code, resolvedOutput.stdout)
	}
	resolvedCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "customer-rename", "--check"}, repository)
	})
	if resolvedCheck.code != 0 {
		t.Fatalf("resolved conflict read-only check = %d, %s", resolvedCheck.code, resolvedCheck.stdout)
	}
}

func TestSemanticManualSQLHandoffRequiresEditedCloneVerificationOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_UNUSED_DEV_DATABASE_URL"
scratch_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.events (id bigint, occurred_on text);\n")
	initialized := captureStdout(t, func() int {
		return runInitAt([]string{"--target", "primary", "--bundle", "baseline"}, repository)
	})
	if initialized.code != 0 {
		t.Fatalf("init exit = %d, stdout = %s", initialized.code, initialized.stdout)
	}
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.events (id bigint, occurred_on date);\n")
	hint := `{"kind":"type_change","name":["app","events","occurred_on"],"strategy":"manual_sql"}`
	drafted := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "event-date", "--hint", hint}, repository)
	})
	if drafted.code != 2 {
		t.Fatalf("manual draft exit = %d, stdout = %s", drafted.code, drafted.stdout)
	}
	var handoff struct {
		Protocol string   `json:"protocol"`
		Status   string   `json:"status"`
		Path     string   `json:"path"`
		Edit     []string `json:"edit"`
	}
	if err := json.Unmarshal([]byte(drafted.stdout), &handoff); err != nil {
		t.Fatal(err)
	}
	if handoff.Protocol != draftflow.Version || handoff.Status != string(protocol.NeedsSQLEdits) || len(handoff.Edit) != 1 || handoff.Edit[0] != "phases/migrate.sql" {
		t.Fatalf("handoff = %#v", handoff)
	}
	bundlePath := filepath.Join(repository, filepath.FromSlash(handoff.Path))
	artifact, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.State != string(protocol.NeedsSQLEdits) || !strings.Contains(string(artifact.Files["phases/migrate.sql"]), "ONWARDPG TODO") {
		t.Fatalf("incomplete bundle = %#v", artifact.Manifest)
	}
	todoCheck := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "event-date", "--check"}, repository)
	})
	if todoCheck.code != 4 || !strings.Contains(todoCheck.stdout, `"code":"unresolved_sql_todo"`) || !strings.Contains(todoCheck.stdout, "phases/migrate.sql") {
		t.Fatalf("TODO check = %d, %s", todoCheck.code, todoCheck.stdout)
	}
	writeTestFile(t, bundlePath, "phases/migrate.sql", "ALTER TABLE app.events ALTER COLUMN occurred_on TYPE date USING occurred_on::date;\n")
	verified := captureStdout(t, func() int {
		return runVerifyAt([]string{"--target", "primary", "--bundle", "event-date"}, repository)
	})
	if verified.code != 0 {
		t.Fatalf("edited verification exit = %d, stdout = %s", verified.code, verified.stdout)
	}
	receipted, err := bundle.Read(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if receipted.Manifest.State != string(protocol.Planned) || receipted.Manifest.PhaseSource != "edited" {
		t.Fatalf("receipted bundle = %#v", receipted.Manifest)
	}
}

func TestHistoryInitCreatesVerifiedGroundFloorWithoutApplyingToAdminDatabase(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	schemaName := "history_init_" + randomToken(t)
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".users (id bigint PRIMARY KEY);`)

	connection, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var existedBefore bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regnamespace($1) IS NOT NULL`, schemaName).Scan(&existedBefore); err != nil {
		t.Fatal(err)
	}
	if existedBefore {
		t.Fatalf("test schema %s already exists in admin database", schemaName)
	}

	output := captureStdout(t, func() int {
		return runHistoryAt([]string{"init", "--target", "primary", "--bundle", "ground-floor"}, repository)
	})
	if output.code != 0 {
		t.Fatalf("history init exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report historyinit.Report
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.ProtocolVersion != historyinit.Version || report.Outcome != "initialized" || report.BundleID != "ground-floor" || report.Verification == nil || report.Verification.Outcome != "verified" {
		t.Fatalf("history init report = %#v", report)
	}
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Entries) != 1 || chain.HeadDigest != report.HistoryHead {
		t.Fatalf("history chain = %#v", chain)
	}
	manifest := chain.Entries[0].Artifact.Manifest
	if manifest.BundleID != "ground-floor" || manifest.Purpose != "baseline" || manifest.History == nil || manifest.History.ParentDigest != bundle.HistoryRootDigest() {
		t.Fatalf("baseline manifest = %#v", manifest)
	}
	if !strings.Contains(string(chain.Entries[0].Artifact.Files["phases/expand.sql"]), `CREATE TABLE "`+schemaName+`"."users"`) {
		t.Fatalf("baseline expand SQL = %s", chain.Entries[0].Artifact.Files["phases/expand.sql"])
	}
	var existsAfter bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regnamespace($1) IS NOT NULL`, schemaName).Scan(&existsAfter); err != nil {
		t.Fatal(err)
	}
	if existsAfter {
		t.Fatalf("history init applied declarative schema to caller database")
	}

	second := captureStdout(t, func() int {
		return runHistoryAt([]string{"init", "--target", "primary", "--bundle", "another-root"}, repository)
	})
	if second.code != 4 {
		t.Fatalf("second history init exit = %d, stdout = %s", second.code, second.stdout)
	}
	var blocked historyinit.Report
	if err := json.Unmarshal([]byte(second.stdout), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Outcome != "blocked" || len(blocked.Findings) != 1 || blocked.Findings[0].Code != "history_already_initialized" {
		t.Fatalf("second history init report = %#v", blocked)
	}

	writeTestFile(t, repository, "schema.sql", `CREATE SCHEMA "`+schemaName+`"; CREATE TABLE "`+schemaName+`".users (id bigint PRIMARY KEY); CREATE TABLE "`+schemaName+`".projects (id bigint PRIMARY KEY);`)
	feature := captureStdout(t, func() int {
		return runDraftAt([]string{"--target", "primary", "--bundle", "projects"}, repository)
	})
	if feature.code != 0 {
		t.Fatalf("draft after history init exit = %d, stdout = %s", feature.code, feature.stdout)
	}
	featureArtifact, err := bundle.Read(filepath.Join(repository, "onward-bundles", "primary", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	if featureArtifact.Manifest.History == nil || featureArtifact.Manifest.History.ParentDigest != report.HistoryHead {
		t.Fatalf("feature history receipt = %#v, baseline head = %s", featureArtifact.Manifest.History, report.HistoryHead)
	}
}

func TestHistoryInitWritesNothingForUnsupportedBaseline(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE DOMAIN public.email_address AS text CHECK (VALUE <> '');\n")

	output := captureStdout(t, func() int {
		return runHistoryAt([]string{"init", "--target", "primary"}, repository)
	})
	if output.code != 3 {
		t.Fatalf("history init exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report historyinit.Report
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Outcome != string(protocol.Unsupported) || report.Plan == nil || report.Plan.Status != protocol.Unsupported {
		t.Fatalf("history init report = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(repository, "onward-bundles")); !os.IsNotExist(err) {
		t.Fatalf("unsupported history init wrote bundle root: %v", err)
	}
}

func TestConfigCheckMaterializesEveryTarget(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	t.Setenv("ONWARDPG_CONFIG_CHECK_URL", url)
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_CONFIG_CHECK_URL"
scratch_database_env = "ONWARDPG_CONFIG_CHECK_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.config_check (id bigint PRIMARY KEY);\n")
	output := captureStdout(t, func() int {
		return runConfig([]string{"check", "--config", filepath.Join(repository, ".onwardpg.toml")})
	})
	if output.code != 0 {
		t.Fatalf("config check exit = %d, stdout = %s", output.code, output.stdout)
	}
	var report struct {
		ProtocolVersion string `json:"protocol_version"`
		Status          string `json:"status"`
		Targets         []struct {
			Name          string `json:"name"`
			Provenance    string `json:"provenance"`
			Fingerprint   string `json:"fingerprint"`
			PostgresMajor int    `json:"postgres_major"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(output.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.ProtocolVersion != "onwardpg.config-check/v2" || report.Status != "valid" || len(report.Targets) != 1 {
		t.Fatalf("config check report = %#v", report)
	}
	target := report.Targets[0]
	if target.Name != "primary" || target.Provenance != "schema_file:schema.sql" || !strings.HasPrefix(target.Fingerprint, "sha256:") || target.PostgresMajor < 14 || target.PostgresMajor > 18 {
		t.Fatalf("config check target = %#v", target)
	}
}

func TestDevPlanQuestionsNeverApplyAndConvergesAfterDeliberateApplication(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	devURL, cleanup := createTestDatabase(t, adminURL)
	defer cleanup()
	t.Setenv("ONWARDPG_DEV_TEST_URL", devURL)
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_DEV_TEST_URL"
`)
	writeTestFile(t, repository, "schema.sql", "CREATE SCHEMA app; CREATE TABLE app.customers (id bigint);\n")
	connection, err := pgx.Connect(context.Background(), devURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(context.Background(), "CREATE SCHEMA app; CREATE TABLE app.accounts (id bigint);"); err != nil {
		t.Fatal(err)
	}

	pendingOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if pendingOutput.code != 2 {
		t.Fatalf("pending exit = %d, stdout = %s", pendingOutput.code, pendingOutput.stdout)
	}
	var pending struct {
		Protocol  string              `json:"protocol"`
		Status    string              `json:"status"`
		Decisions []protocol.Decision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(pendingOutput.stdout), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Protocol != "onwardpg/dev-plan/2" || pending.Status != "needs_decisions" || len(pending.Decisions) != 1 {
		t.Fatalf("pending = %#v", pending)
	}
	var renameHint *protocol.Hint
	for _, choice := range pending.Decisions[0].Choices {
		if choice.Hint.Kind == "rename" {
			hint := choice.Hint
			renameHint = &hint
		}
	}
	if renameHint == nil {
		t.Fatalf("pending decision has no rename hint: %#v", pending)
	}
	hintData, err := json.Marshal(renameHint)
	if err != nil {
		t.Fatal(err)
	}
	plannedOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary", "--hint", string(hintData)}, repository)
	})
	if plannedOutput.code != 0 {
		t.Fatalf("planned exit = %d, stdout = %s", plannedOutput.code, plannedOutput.stdout)
	}
	var planned protocol.Result
	if err := json.Unmarshal([]byte(plannedOutput.stdout), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned || len(planned.Statements) == 0 || planned.Statements[0].Phase != "contract" {
		t.Fatalf("planned = %#v", planned)
	}
	var oldExists, newExists bool
	if err := connection.QueryRow(context.Background(), `SELECT to_regclass('app.accounts') IS NOT NULL, to_regclass('app.customers') IS NOT NULL`).Scan(&oldExists, &newExists); err != nil {
		t.Fatal(err)
	}
	if !oldExists || newExists {
		t.Fatalf("dev plan applied SQL: old=%t new=%t", oldExists, newExists)
	}
	for _, statement := range planned.Statements {
		if _, err := connection.Exec(context.Background(), statement.SQL); err != nil {
			t.Fatalf("deliberate local application: %v", err)
		}
	}
	residualOutput := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary"}, repository)
	})
	if residualOutput.code != 0 {
		t.Fatalf("residual exit = %d, stdout = %s", residualOutput.code, residualOutput.stdout)
	}
	var residual protocol.Result
	if err := json.Unmarshal([]byte(residualOutput.stdout), &residual); err != nil {
		t.Fatal(err)
	}
	if residual.Status != protocol.Planned || len(residual.Statements) != 0 || residual.CurrentFingerprint != residual.DesiredFingerprint {
		t.Fatalf("residual = %#v", residual)
	}
}

func TestBundleVerifyReportsPartialResidualAndFullConvergenceOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
	baseDDL := "CREATE SCHEMA app; CREATE TABLE app.obsolete (id bigint PRIMARY KEY);\n"
	writeTestFile(t, repository, "schema.sql", "")
	writeHistoryFixture(t, repository, url, "genesis", baseDDL)
	writeHistoryContractDropFixture(t, repository, url, "remove-obsolete")

	before := disposableDatabaseCount(t, url)
	partial := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "remove-obsolete", "--through", "expand"}, repository)
	})
	if partial.code != 4 {
		t.Fatalf("partial exit = %d, stdout = %s", partial.code, partial.stdout)
	}
	var partialReport verify.Report
	if err := json.Unmarshal([]byte(partial.stdout), &partialReport); err != nil {
		t.Fatal(err)
	}
	if partialReport.Outcome != "residual" || partialReport.Residual == nil || partialReport.Residual.Status != protocol.NeedsInput {
		t.Fatalf("partial report = %#v", partialReport)
	}

	full := captureStdout(t, func() int {
		return runBundleAt([]string{"verify", "--target", "primary", "--bundle", "remove-obsolete"}, repository)
	})
	if full.code != 0 {
		t.Fatalf("full exit = %d, stdout = %s", full.code, full.stdout)
	}
	var fullReport verify.Report
	if err := json.Unmarshal([]byte(full.stdout), &fullReport); err != nil {
		t.Fatal(err)
	}
	if fullReport.ProtocolVersion != verify.Version || fullReport.Outcome != "verified" || fullReport.ObservedFingerprint != fullReport.DesiredFingerprint || fullReport.Residual == nil || len(fullReport.Residual.Statements) != 0 {
		t.Fatalf("full report = %#v", fullReport)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("disposable database count = %d, want %d", after, before)
	}
}

func TestVerifyFailureModesAlwaysCleanDisposableDatabases(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	tests := []struct {
		name       string
		phaseSQL   string
		verifySQL  string
		wantCode   string
		cancelSoon bool
	}{
		{
			name: "transactional failure",
			phaseSQL: "-- onwardpg:batch transactional\n" +
				"CREATE SCHEMA should_rollback;\nSELECT definitely_not_a_function();\n",
			wantCode: "transactional_batch_failed",
		},
		{
			name: "non-transactional failure",
			phaseSQL: "-- onwardpg:batch nontransactional\n" +
				"CREATE SCHEMA partial_disposable_effect;\nSELECT definitely_not_a_function();\n",
			wantCode: "non_transactional_batch_failed",
		},
		{
			name:      "false assertion",
			verifySQL: "-- onwardpg:assert expected_product_effect\nSELECT false;\n",
			wantCode:  "assertion_false",
		},
		{
			name:       "cancellation",
			phaseSQL:   "-- onwardpg:batch transactional\nSELECT pg_sleep(30);\n",
			wantCode:   "transactional_batch_failed",
			cancelSoon: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			writeTestFile(t, repository, ".onwardpg.toml", `version = 1
bundle_root = "onward-bundles"
[targets.primary]
schema_file = "schema.sql"
dev_database_env = "ONWARDPG_TEST_DATABASE_URL"
`)
			ddl := "CREATE SCHEMA app; CREATE TABLE app.items (id bigint PRIMARY KEY);\n"
			writeTestFile(t, repository, "schema.sql", ddl)
			writeHistoryFixture(t, repository, url, "genesis", ddl)
			bundlePath := filepath.Join(repository, "onward-bundles", "primary", "genesis")
			if test.phaseSQL != "" {
				writeTestFile(t, bundlePath, "phases/expand.sql", test.phaseSQL)
			}
			if test.verifySQL != "" {
				writeTestFile(t, bundlePath, "verify.sql", test.verifySQL)
			}
			edited, err := bundle.PrepareEdited(bundlePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := bundle.InstallReceipts(bundlePath, edited); err != nil {
				t.Fatal(err)
			}
			chain, err := history.Load(repository, "onward-bundles", "primary")
			if err != nil {
				t.Fatal(err)
			}
			before := disposableDatabaseCount(t, url)
			ctx := context.Background()
			if test.cancelSoon {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			report, err := verify.Run(ctx, verify.Input{AdminURL: url, Chain: chain, BundleID: "genesis"})
			if err != nil {
				t.Fatal(err)
			}
			if report.Outcome != "failed" || report.Failure == nil || report.Failure.Code != test.wantCode || len(report.Findings) != 1 || report.Findings[0].Code != test.wantCode || report.Failure.Remediation == "" {
				t.Fatalf("failure report = %#v", report)
			}
			if after := disposableDatabaseCount(t, url); after != before {
				t.Fatalf("disposable database count after failure = %d, want %d", after, before)
			}
		})
	}
}

func TestVerifyRejectsHistoryReceiptedForAnotherPostgresMajor(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	repository := t.TempDir()
	ddl := "CREATE TABLE items (id bigint PRIMARY KEY);\n"
	writeHistoryFixture(t, repository, url, "genesis", ddl)
	chain, err := history.Load(repository, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := source.PostgresMajor(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	different := 14
	if actual == different {
		different = 15
	}
	chain.Entries[0].Artifact.Manifest.DesiredSource.PostgresMajor = different
	before := disposableDatabaseCount(t, url)
	_, err = verify.Run(context.Background(), verify.Input{AdminURL: url, Chain: chain, BundleID: "genesis"})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("targets PostgreSQL %d but the scratch server is PostgreSQL %d", different, actual)) {
		t.Fatalf("major mismatch error = %v", err)
	}
	if after := disposableDatabaseCount(t, url); after != before {
		t.Fatalf("major mismatch created a disposable database: before=%d after=%d", before, after)
	}
}

func writeHistoryFixture(t *testing.T, root, devURL, id, desiredDDL string) {
	t.Helper()
	ctx := context.Background()
	postgresMajor, err := source.PostgresMajor(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-history", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "history-fixture", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature",
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "empty history", Fingerprint: result.CurrentFingerprint, PostgresMajor: postgresMajor},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "fixture schema", Fingerprint: result.DesiredFingerprint, PostgresMajor: postgresMajor},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: bundle.HistoryRootDigest(),
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func writeHistoryTransitionFixture(t *testing.T, root, devURL, id, desiredDDL string) {
	t.Helper()
	ctx := context.Background()
	postgresMajor, err := source.PostgresMajor(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := history.Load(root, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := chain.Replay()
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "history-transition-fixture", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "feature",
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "fixture history", Fingerprint: result.CurrentFingerprint, PostgresMajor: postgresMajor},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "fixture transition", Fingerprint: result.DesiredFingerprint, PostgresMajor: postgresMajor},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: chain.HeadDigest,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func writeHistoryContractDropFixture(t *testing.T, root, devURL, id string) {
	t.Helper()
	ctx := context.Background()
	postgresMajor, err := source.PostgresMajor(ctx, devURL)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := history.Load(root, "onward-bundles", "primary")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := chain.Replay()
	if err != nil {
		t.Fatal(err)
	}
	current, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, nil, "empty-desired", devURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) == 0 {
		t.Fatalf("drop planning did not ask for destructive intent: %#v", pending)
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint,
		DesiredFingerprint: pending.DesiredFingerprint,
	}
	for _, question := range pending.Questions {
		if len(question.Choices) == 0 {
			t.Fatalf("drop question has no explicit choice: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{
			Kind: question.Kind, Key: question.Key, Value: question.Choices[0],
			QuestionFingerprint: question.ScopeFingerprint,
		})
	}
	result, err := graphplan.Build(current, desired, answers, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != protocol.Planned {
		t.Fatalf("drop result = %#v", result)
	}
	metadata := bundle.Metadata{
		BundleID: id, Generation: 1, Target: "primary", Purpose: "contract",
		BaselineSource: bundle.SourceReceipt{Kind: "onwardpg_history", Description: "fixture history", Fingerprint: result.CurrentFingerprint, PostgresMajor: postgresMajor},
		DesiredSource:  bundle.SourceReceipt{Kind: "ddl_export", Description: "empty fixture schema", Fingerprint: result.DesiredFingerprint, PostgresMajor: postgresMajor},
		Planner:        bundle.PlannerReceipt{Version: "test"}, HistoryParentDigest: chain.HeadDigest,
	}
	artifact, err := bundle.Build(bundle.Input{Metadata: metadata, Result: result, Answers: &answers})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(filepath.Join(root, "onward-bundles", "primary", id), artifact, bundle.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func disposableDatabaseCount(t *testing.T, url string) int {
	t.Helper()
	connection, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	var count int
	if err := connection.QueryRow(context.Background(), `SELECT count(*) FROM pg_database WHERE datname LIKE 'onwardpg_verify_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func randomToken(t *testing.T) string {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(random)
}

func createTestDatabase(t *testing.T, adminURL string) (string, func()) {
	t.Helper()
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	name := "onwardpg_dev_test_" + hex.EncodeToString(random)
	admin, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+`"`+name+`"`); err != nil {
		admin.Close(context.Background())
		t.Fatal(err)
	}
	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	return parsed.String(), func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+`"`+name+`" WITH (FORCE)`)
		_ = admin.Close(context.Background())
	}
}

type captured struct {
	code   int
	stdout string
}

func captureStdout(t *testing.T, call func() int) captured {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	code := call()
	_ = writer.Close()
	os.Stdout = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	return captured{code: code, stdout: string(data)}
}
