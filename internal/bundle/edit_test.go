package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePhaseSQLPreservesBatchesAndDefaultsToTransactional(t *testing.T) {
	body := []byte("-- heading\n-- onwardpg:batch transactional\nSELECT 1;\n-- onwardpg:batch nontransactional\nCREATE INDEX CONCURRENTLY x ON t (id);\n")
	batches, err := ParsePhaseSQL(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || !batches[0].Transactional || batches[1].Transactional {
		t.Fatalf("batches = %#v", batches)
	}
	if !strings.Contains(batches[0].SQL, "-- heading") || !strings.Contains(batches[1].SQL, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("batch SQL was not preserved: %#v", batches)
	}
	plain, err := ParsePhaseSQL([]byte("UPDATE app.users SET active = true;\n"), nil)
	if err != nil || len(plain) != 1 || !plain[0].Transactional {
		t.Fatalf("plain = %#v, err = %v", plain, err)
	}
}

func TestParseAssertionsSupportsNamedBooleanQueries(t *testing.T) {
	assertions, err := ParseAssertions([]byte("-- onwardpg:assert rows_backfilled\nSELECT true;\n-- onwardpg:assert no_nulls\nSELECT count(*) = 0 FROM t WHERE value IS NULL;\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 2 || assertions[0].ID != "rows_backfilled" || assertions[1].ID != "no_nulls" {
		t.Fatalf("assertions = %#v", assertions)
	}
}

func TestPrepareEditedAndInstallReceiptsExactSQL(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	artifact, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "customer-profile")
	if err := Write(destination, artifact, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	migrate := "UPDATE app.users SET id = id;\n"
	if err := os.WriteFile(filepath.Join(destination, "phases", "migrate.sql"), []byte(migrate), 0o644); err != nil {
		t.Fatal(err)
	}
	verification := "-- onwardpg:assert rows_valid\nSELECT true;\n"
	if err := os.WriteFile(filepath.Join(destination, "verify.sql"), []byte(verification), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(destination); err == nil {
		t.Fatal("strict read accepted unreceipted SQL edits")
	}
	candidate, err := PrepareEdited(destination)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Manifest.PhaseSource != "edited" || candidate.Manifest.VerificationDigest == "" || candidate.Manifest.Phases["migrate"].Digest != Digest([]byte(migrate)) {
		t.Fatalf("candidate manifest = %#v", candidate.Manifest)
	}
	if err := InstallReceipts(destination, candidate); err != nil {
		t.Fatal(err)
	}
	receipted, err := Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	if receipted.Manifest.History.EntryDigest == artifact.Manifest.History.EntryDigest {
		t.Fatal("edited SQL did not change the history entry digest")
	}
	if _, _, err := NextCoordinates(destination, meta, plannedResult()); err != nil {
		t.Fatalf("receipted feature draft should remain replaceable: %v", err)
	}
}

func TestReconcileEditedDraftPreservesUntouchedAgentPhase(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	oldGenerated, err := Build(Input{Metadata: meta, Result: plannedResult(
		statement("CREATE TABLE app.users (id bigint);", "expand", true),
	)})
	if err != nil {
		t.Fatal(err)
	}
	migrate := []byte("UPDATE app.users SET id = id;\n")
	verification := []byte("-- onwardpg:assert rows_valid\nSELECT true;\n")
	previous, err := PrepareEditedFiles(oldGenerated, map[string][]byte{
		"phases/expand.sql":  oldGenerated.Files["phases/expand.sql"],
		"phases/migrate.sql": migrate,
		"verify.sql":         verification,
	})
	if err != nil {
		t.Fatal(err)
	}
	meta.Generation = 2
	newGenerated, err := Build(Input{Metadata: meta, Result: plannedResult(
		statement("CREATE TABLE app.users (id bigint);", "expand", true),
		statement("ALTER TABLE app.users ADD COLUMN email text;", "expand", true),
	)})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, report, err := ReconcileEditedDraft(previous, newGenerated)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "reconciled" || len(report.Conflicts) != 0 {
		t.Fatalf("reconciliation = %#v", report)
	}
	if string(reconciled.Files["phases/migrate.sql"]) != string(migrate) || string(reconciled.Files["verify.sql"]) != string(verification) {
		t.Fatalf("agent-owned files were not preserved: %v", SortedFiles(reconciled.Files))
	}
	if !strings.Contains(string(reconciled.Files["phases/expand.sql"]), "ADD COLUMN email") {
		t.Fatalf("generated expand phase was not refreshed: %s", reconciled.Files["phases/expand.sql"])
	}
}

func TestReconcileEditedDraftReportsSamePhaseConflict(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	oldGenerated, err := Build(Input{Metadata: meta, Result: plannedResult(
		statement("CREATE TABLE app.users (id bigint);", "expand", true),
	)})
	if err != nil {
		t.Fatal(err)
	}
	previous, err := PrepareEditedFiles(oldGenerated, map[string][]byte{
		"phases/expand.sql": []byte("CREATE TABLE app.users (id bigint, agent_note text);\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	meta.Generation = 2
	newGenerated, err := Build(Input{Metadata: meta, Result: plannedResult(
		statement("CREATE TABLE app.users (id bigint, email text);", "expand", true),
	)})
	if err != nil {
		t.Fatal(err)
	}
	_, report, err := ReconcileEditedDraft(previous, newGenerated)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "conflict" || len(report.Conflicts) != 1 || report.Conflicts[0].Path != "phases/expand.sql" {
		t.Fatalf("reconciliation = %#v", report)
	}
	conflict := report.Conflicts[0]
	if conflict.OldGeneratedSQL == nil || !strings.Contains(*conflict.OldGeneratedSQL, "id bigint") || conflict.CurrentSQL == nil || !strings.Contains(*conflict.CurrentSQL, "agent_note") || conflict.NewGeneratedSQL == nil || !strings.Contains(*conflict.NewGeneratedSQL, "email") || conflict.Resolution == "" {
		t.Fatalf("conflict omitted three-way SQL evidence: %#v", conflict)
	}

	destination := filepath.Join(t.TempDir(), "customer-profile")
	if err := Write(destination, previous, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Write(destination, newGenerated, WriteOptions{ReplaceDraft: true, PreserveEdited: []string{conflict.Path}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(destination); err == nil {
		t.Fatal("conflict handoff incorrectly appeared receipted")
	}
	preserved, err := os.ReadFile(filepath.Join(destination, "phases", "expand.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(preserved), "agent_note") || strings.Contains(string(preserved), "email") {
		t.Fatalf("conflict handoff overwrote current SQL: %s", preserved)
	}
	prepared, err := PrepareEdited(destination)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Manifest.PhaseSource != "edited" || !strings.Contains(string(prepared.Files["plan.json"]), "email") {
		t.Fatalf("conflict handoff did not anchor edits to the new plan: %#v", prepared.Manifest)
	}
}

func TestPrepareEditedRejectsUnresolvedTODO(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	artifact, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "customer-profile")
	if err := Write(destination, artifact, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "phases", "migrate.sql"), []byte("-- ONWARDPG TODO: add the reviewed backfill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareEdited(destination); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("expected unresolved TODO rejection, got %v", err)
	}
}
