package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
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

func TestParseAssertionsRequiresExplicitDevelopmentPostconditionMarker(t *testing.T) {
	assertions, err := ParseAssertions([]byte("-- onwardpg:assert rows_backfilled\n-- onwardpg:dev-postcondition\nSELECT true;\n-- onwardpg:assert review_only\nSELECT true;\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(assertions) != 2 || !assertions[0].DevSafePostcondition || assertions[1].DevSafePostcondition {
		t.Fatalf("assertions = %#v", assertions)
	}
	if _, err := ParseAssertions([]byte("-- onwardpg:dev-postcondition\nSELECT true;\n")); err == nil {
		t.Fatal("accepted a development marker without an assertion")
	}
}

func TestTransplantEditedPocketsRefreshesGeneratorOwnedSurroundings(t *testing.T) {
	oldGenerated := []byte("-- generated before\n-- onwardpg:edit begin stmt-1\n-- ONWARDPG TODO: backfill\n-- onwardpg:edit end stmt-1\nSELECT 'tail';\n")
	previous := []byte("-- generated before\n-- onwardpg:edit begin stmt-1\nUPDATE app.events SET ready = true;\n-- onwardpg:edit end stmt-1\nSELECT 'tail';\n")
	newGenerated := []byte("-- generated before\nALTER TABLE app.events ADD COLUMN note text;\n-- onwardpg:edit begin stmt-1\n-- ONWARDPG TODO: backfill\n-- onwardpg:edit end stmt-1\nSELECT 'tail';\n")
	merged, transplanted, err := transplantEditedPockets(oldGenerated, previous, newGenerated)
	if err != nil {
		t.Fatal(err)
	}
	if !transplanted || !strings.Contains(string(merged), "ADD COLUMN note") || !strings.Contains(string(merged), "UPDATE app.events") || strings.Contains(string(merged), "ONWARDPG TODO") {
		t.Fatalf("pocket merge = %q, transplanted=%v", merged, transplanted)
	}

	outsideEdit := []byte("-- developer changed generated text\n-- onwardpg:edit begin stmt-1\nUPDATE app.events SET ready = true;\n-- onwardpg:edit end stmt-1\nSELECT 'tail';\n")
	if _, transplanted, err := transplantEditedPockets(oldGenerated, outsideEdit, newGenerated); err != nil || transplanted {
		t.Fatalf("outside-pocket edit must remain a phase conflict: transplanted=%v err=%v", transplanted, err)
	}
}

func TestReplaceEditPocketsPreservesGeneratorOwnedBytes(t *testing.T) {
	body := []byte("before\n-- onwardpg:edit begin pocket-a\n-- ONWARDPG TODO: a\n-- onwardpg:edit end pocket-a\nmiddle\n-- onwardpg:edit begin pocket-b\n-- ONWARDPG TODO: b\n-- onwardpg:edit end pocket-b\nafter\n")
	ids, err := EditPocketIDs(body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"pocket-a", "pocket-b"}) {
		t.Fatalf("pocket ids = %#v", ids)
	}
	edited, err := ReplaceEditPockets(body, map[string][]byte{"pocket-b": []byte("SELECT true;\n")})
	if err != nil {
		t.Fatal(err)
	}
	want := "before\n-- onwardpg:edit begin pocket-a\n-- ONWARDPG TODO: a\n-- onwardpg:edit end pocket-a\nmiddle\n-- onwardpg:edit begin pocket-b\nSELECT true;\n-- onwardpg:edit end pocket-b\nafter\n"
	if string(edited) != want {
		t.Fatalf("edited body:\n%s\nwant:\n%s", edited, want)
	}
	if _, err := ReplaceEditPockets(body, map[string][]byte{"missing": []byte("SELECT 1;\n")}); err == nil {
		t.Fatal("expected unknown pocket error")
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
	contract := "UPDATE app.users SET id = id;\n"
	if err := os.WriteFile(filepath.Join(destination, "phases", "contract.sql"), []byte(contract), 0o644); err != nil {
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
	if candidate.Manifest.PhaseSource != "edited" || candidate.Manifest.VerificationDigest == "" || candidate.Manifest.Phases[protocol.PhaseContract].Digest != Digest([]byte(contract)) {
		t.Fatalf("candidate manifest = %#v", candidate.Manifest)
	}
	candidate, err = WithExpandCheckpoint(candidate, candidate.Manifest.BaselineSource.Fingerprint)
	if err != nil {
		t.Fatal(err)
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
	if receipted.Manifest.ExpandCheckpointDigest == "" || len(receipted.Files["expand-checkpoint.json"]) == 0 {
		t.Fatal("edited SQL receipt did not atomically install its verified expand checkpoint")
	}
	if _, _, err := NextCoordinates(destination, meta, plannedResult()); err != nil {
		t.Fatalf("receipted feature draft should remain replaceable: %v", err)
	}
}

func TestPrepareEditedReceiptsResolvedManualContractGate(t *testing.T) {
	gate := protocol.ContractGate{
		ID:               "manual:users_email",
		Kind:             "manual_reconciliation",
		ScopeFingerprint: currentFingerprint,
		Reason:           "operator must define the exact desired email invariant",
		BooleanSQL:       "SELECT false /* ONWARDPG TODO: replace with an exact boolean postcondition */",
	}
	assertionSQL := "DO $onwardpg$ BEGIN IF NOT COALESCE((" + gate.BooleanSQL + "), false) THEN RAISE EXCEPTION 'onwardpg contract gate failed: " + gate.ID + "'; END IF; END $onwardpg$;"
	assertion := statement(assertionSQL, protocol.PhaseContract, true)
	assertion.TransitionID = "constraint:app:users:users_email_check"
	assertion.ID = protocol.StableStatementID(assertion)
	result := plannedResult(assertion)
	result.Status = protocol.NeedsSQLEdits
	result.ContractGates = []protocol.ContractGate{gate}
	result.Reconciliations = []protocol.Reconciliation{{
		TransitionID: assertion.TransitionID,
		Strategy:     "manual_sql",
		Work: &protocol.ManualWork{
			Statements:      []string{"-- ONWARDPG TODO: reconcile existing email values"},
			VerificationSQL: []string{gate.BooleanSQL},
		},
		GateIDs: []string{gate.ID},
	}}
	result.Batches[0].Statements[0] = assertion

	artifact, err := Build(Input{Metadata: metadata(), Result: result})
	if err != nil {
		t.Fatal(err)
	}
	resolved := "SELECT NOT EXISTS (SELECT 1 FROM app.users WHERE email IS NULL)"
	editedContract := strings.Replace(string(artifact.Files["phases/contract.sql"]), gate.BooleanSQL, resolved, 1)
	edited, err := PrepareEditedFiles(artifact, map[string][]byte{
		"phases/contract.sql": []byte(editedContract),
	})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Manifest.ContractGateOverridesDigest == "" || len(edited.Files[contractGateOverridesPath]) == 0 {
		t.Fatalf("edited gate override was not receipted: %#v", edited.Manifest)
	}
	gates, err := ContractGates(edited)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].BooleanSQL != resolved {
		t.Fatalf("effective gates = %#v", gates)
	}
	destination := filepath.Join(t.TempDir(), "resolved-gate")
	if err := Write(destination, artifact, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "phases", "contract.sql"), []byte(editedContract), 0o644); err != nil {
		t.Fatal(err)
	}
	installable, err := PrepareEdited(destination)
	if err != nil {
		t.Fatal(err)
	}
	installable, err = WithExpandCheckpoint(installable, installable.Manifest.BaselineSource.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallReceipts(destination, installable); err != nil {
		t.Fatal(err)
	}
	installed, err := Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed.Files[contractGateOverridesPath]) == 0 {
		t.Fatal("installed manifest refers to a missing contract gate override")
	}

	tampered := edited
	tampered.Files = make(map[string][]byte, len(edited.Files))
	for name, body := range edited.Files {
		tampered.Files[name] = append([]byte(nil), body...)
	}
	tampered.Files[contractGateOverridesPath] = append(tampered.Files[contractGateOverridesPath], '\n')
	if _, err := ContractGates(tampered); err == nil {
		t.Fatal("accepted a contract gate override whose bytes no longer match its receipt")
	}
}

func TestNextCoordinatesFromPreparedEditedDraftDoesNotRequireStrictReread(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(destination, "phases", "contract.sql"), []byte("UPDATE app.users SET id = id;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareEdited(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NextCoordinates(destination, meta, plannedResult()); err == nil {
		t.Fatal("strict path-based coordinate calculation accepted unreceipted files")
	}
	generation, attempt, err := NextCoordinatesFromArtifact(prepared, meta, plannedResult())
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 || attempt != 1 {
		t.Fatalf("next coordinates = (%d, %d), want (1, 1)", generation, attempt)
	}
}

func TestWriteCanReplaceExactPreparedUnreceiptedDraft(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	first, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "customer-profile")
	if err := Write(destination, first, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "phases", "contract.sql"), []byte("UPDATE app.users SET id = id;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareEdited(destination)
	if err != nil {
		t.Fatal(err)
	}
	meta.Generation = 2
	meta.HistoryParentDigest = desiredFingerprint
	next, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint, email text);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(destination, next, WriteOptions{ReplaceDraft: true, ExpectedPrevious: &prepared}); err != nil {
		t.Fatal(err)
	}
	installed, err := Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Manifest.Generation != 2 || installed.Manifest.History.ParentDigest != desiredFingerprint {
		t.Fatalf("installed manifest = %#v", installed.Manifest)
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
	contract := []byte("UPDATE app.users SET id = id;\n")
	verification := []byte("-- onwardpg:assert rows_valid\nSELECT true;\n")
	previous, err := PrepareEditedFiles(oldGenerated, map[string][]byte{
		"phases/expand.sql":   oldGenerated.Files["phases/expand.sql"],
		"phases/contract.sql": contract,
		"verify.sql":          verification,
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
	if string(reconciled.Files["phases/contract.sql"]) != string(contract) || string(reconciled.Files["verify.sql"]) != string(verification) {
		t.Fatalf("agent-owned files were not preserved: %v", SortedFiles(reconciled.Files))
	}
	if !strings.Contains(string(reconciled.Files["phases/expand.sql"]), "ADD COLUMN email") {
		t.Fatalf("generated expand phase was not refreshed: %s", reconciled.Files["phases/expand.sql"])
	}
}

func TestReconcileEditedDraftCarriesResolvedTODOAcrossNewHistoryParent(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	todo := statement("-- ONWARDPG TODO: convert app.accounts.occurred_at from timestamp to date", protocol.PhaseContract, true)
	oldResult := plannedResult(todo)
	oldResult.Status = protocol.NeedsSQLEdits
	oldGenerated, err := Build(Input{Metadata: meta, Result: oldResult})
	if err != nil {
		t.Fatal(err)
	}
	conversion := []byte("ALTER TABLE app.accounts ALTER COLUMN occurred_at TYPE date USING occurred_at::date;\n")
	previous, err := PrepareEditedFiles(oldGenerated, map[string][]byte{"phases/contract.sql": conversion})
	if err != nil {
		t.Fatal(err)
	}

	meta.Generation = 2
	meta.HistoryParentDigest = desiredFingerprint
	newResult := plannedResult(todo)
	newResult.Status = protocol.NeedsSQLEdits
	newGenerated, err := Build(Input{Metadata: meta, Result: newResult})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, report, err := ReconcileEditedDraft(previous, newGenerated)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "reconciled" || reconciled.Manifest.State != string(protocol.Planned) || string(reconciled.Files["phases/contract.sql"]) != string(conversion) {
		t.Fatalf("TODO reconciliation = %#v, manifest = %#v", report, reconciled.Manifest)
	}
}

func TestReconcileEditedDraftWritesNewTODOBesidePriorResolvedPocket(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	oldTODO := statement("-- ONWARDPG TODO: backfill app.accounts.status", protocol.PhaseContract, true)
	oldResult := plannedResult(oldTODO)
	oldResult.Status = protocol.NeedsSQLEdits
	oldGenerated, err := Build(Input{Metadata: meta, Result: oldResult})
	if err != nil {
		t.Fatal(err)
	}
	oldBody := string(oldGenerated.Files["phases/contract.sql"])
	resolvedBody := strings.Replace(oldBody, oldTODO.SQL, "UPDATE app.accounts SET status = 'ready' WHERE status IS NULL;", 1)
	previous, err := PrepareEditedFiles(oldGenerated, map[string][]byte{
		"phases/contract.sql": []byte(resolvedBody),
		"verify.sql":          []byte("-- onwardpg:assert status_ready\nSELECT true;\n"),
	})
	if err != nil {
		t.Fatal(err)
	}

	meta.Generation = 2
	newTODO := statement("-- ONWARDPG TODO: backfill app.accounts.full_name", protocol.PhaseExpand, true)
	newResult := plannedResult(newTODO, oldTODO)
	newResult.Status = protocol.NeedsSQLEdits
	newGenerated, err := Build(Input{Metadata: meta, Result: newResult})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, report, err := ReconcileEditedDraft(previous, newGenerated)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "needs_sql_edits" || reconciled.Manifest.State != string(protocol.NeedsSQLEdits) || reconciled.Manifest.PhaseSource != "edited" {
		t.Fatalf("pending reconciliation = %#v manifest=%#v", report, reconciled.Manifest)
	}
	if !strings.Contains(string(reconciled.Files["phases/expand.sql"]), "ONWARDPG TODO: backfill app.accounts.full_name") {
		t.Fatalf("new edit pocket was not written: %s", reconciled.Files["phases/expand.sql"])
	}
	contract := string(reconciled.Files["phases/contract.sql"])
	if !strings.Contains(contract, "UPDATE app.accounts SET status = 'ready'") || strings.Contains(contract, "TODO: backfill app.accounts.status") {
		t.Fatalf("prior reviewed pocket was not preserved: %s", contract)
	}
	if !strings.Contains(string(reconciled.Files["verify.sql"]), "status_ready") || reconciled.Manifest.VerificationDigest == "" {
		t.Fatalf("pending candidate lost developer verification: %#v", reconciled.Manifest)
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
	if err := os.WriteFile(filepath.Join(destination, "phases", "contract.sql"), []byte("-- ONWARDPG TODO: add the reviewed backfill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareEdited(destination); err == nil || !strings.Contains(err.Error(), "unresolved") || !strings.Contains(err.Error(), "contract.sql") || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("expected unresolved TODO rejection, got %v", err)
	}
}
