package bundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
)

const (
	currentFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	desiredFingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestBuildPlannedBundleWritesDeterministicPhaseReceipts(t *testing.T) {
	expand := statement("CREATE TABLE app.users (id bigint);", "expand", true)
	index := statement("CREATE INDEX CONCURRENTLY users_id_idx ON app.users (id);", "expand", false)
	contract := statement("DROP TABLE app.old_users;", "contract", true)
	result := plannedResult(expand, index, contract)
	input := Input{Metadata: metadata(), Result: result, Answers: &protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint, Answers: []protocol.Answer{},
	}}
	artifact, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.State != "planned" || artifact.Manifest.PlanDigest == "" || len(artifact.Manifest.Phases) != 2 {
		t.Fatalf("manifest = %#v", artifact.Manifest)
	}
	phase := artifact.Manifest.Phases["expand"]
	if phase.Transactional != nil {
		t.Fatalf("mixed expand phase should not claim one transaction mode: %#v", phase)
	}
	expandSQL := string(artifact.Files["phases/expand.sql"])
	if !strings.Contains(expandSQL, "CREATE TABLE") || !strings.Contains(expandSQL, "outside BEGIN/COMMIT") {
		t.Fatalf("expand phase lost statements or batch guidance: %s", expandSQL)
	}
	if got := Digest(artifact.Files[phase.Path]); got != phase.Digest {
		t.Fatalf("phase digest = %q, want %q", phase.Digest, got)
	}
	var roundTrip Manifest
	if err := json.Unmarshal(artifact.Files["manifest.json"], &roundTrip); err != nil {
		t.Fatal(err)
	}
	if err := roundTrip.Validate(); err != nil {
		t.Fatal(err)
	}

	again, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	for name, first := range artifact.Files {
		if string(first) != string(again.Files[name]) {
			t.Fatalf("artifact %s is not deterministic", name)
		}
	}
}

func TestBuildReceiptsAndValidatesContractGates(t *testing.T) {
	gate := protocol.ContractGate{
		ID: "data:orders_owner", Kind: "data_assertion", ScopeFingerprint: currentFingerprint,
		Reason: "orders must reference a retained owner", BooleanSQL: "SELECT true;",
	}
	contract := protocol.Statement{
		SQL: "ALTER TABLE app.orders VALIDATE CONSTRAINT orders_owner_fkey;", Safety: "review", Phase: protocol.PhaseContract,
		RequiresGates: []string{gate.ID}, ContractDisposition: "gated_restoration",
	}
	contract.ID = protocol.StableStatementID(contract)
	result := plannedResult(contract)
	result.ContractGates = []protocol.ContractGate{gate}
	result.Reconciliations = []protocol.Reconciliation{{TransitionID: "constraint:app:orders:orders_owner_fkey", Strategy: "assert_only", GateIDs: []string{gate.ID}}}
	artifact, err := Build(Input{Metadata: metadata(), Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.ContractGatesDigest == "" || artifact.Files["contract-gates.json"] == nil {
		t.Fatalf("contract gate receipt missing: %#v", artifact.Manifest)
	}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}

	invalid := result
	invalid.Statements[0].RequiresGates = []string{"missing"}
	invalid.Statements[0].ID = protocol.StableStatementID(invalid.Statements[0])
	invalid.Batches[0].Statements[0] = invalid.Statements[0]
	if _, err := Build(Input{Metadata: metadata(), Result: invalid}); err == nil || !strings.Contains(err.Error(), "missing or duplicate") {
		t.Fatalf("missing gate validation = %v", err)
	}
}

func TestWithExpandCheckpointReceiptsObservedGraphAndHistory(t *testing.T) {
	inputMetadata := metadata()
	inputMetadata.PlanID = "plan_0123456789abcdef0123456789abcdef"
	inputMetadata.HistoryParentDigest = HistoryRootDigest()
	artifact, err := Build(Input{Metadata: inputMetadata, Result: plannedResult(statement("SELECT 1;", protocol.PhaseExpand, true))})
	if err != nil {
		t.Fatal(err)
	}
	originalEntry := artifact.Manifest.History.EntryDigest
	receipted, err := WithExpandCheckpoint(artifact, currentFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if receipted.Manifest.ExpandCheckpointDigest == "" || receipted.Manifest.History.EntryDigest == originalEntry {
		t.Fatalf("checkpoint did not enter manifest/hash chain: %#v", receipted.Manifest)
	}
	checkpoint, err := ReadCatalogCheckpoint(receipted)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ExpandFingerprint != currentFingerprint || checkpoint.PlanID != inputMetadata.PlanID {
		t.Fatalf("checkpoint=%#v", checkpoint)
	}
}

func TestBuildReceiptsSemanticHintsWithoutMakingThemAuthoringState(t *testing.T) {
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		Answers: []protocol.Answer{{Kind: "drop", Key: "column:public:users:legacy", Value: "drop", QuestionFingerprint: "sha256:scope"}},
	}
	hint := protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "legacy"}}
	artifact, err := Build(Input{
		Metadata: metadata(), Result: plannedResult(statement("ALTER TABLE public.users DROP COLUMN legacy;", "contract", true)),
		Answers: &answers, Hints: []protocol.Hint{hint},
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.SemanticDigest == "" || artifact.Files["decisions.json"] == nil {
		t.Fatalf("semantic decision receipt missing: %#v", artifact.Manifest)
	}
	hints, err := SemanticHints(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 1 || hints[0].Kind != "drop" || hints[0].Name[2] != "legacy" {
		t.Fatalf("hints = %#v", hints)
	}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildHistoryReceiptCommitsToParentAndManifest(t *testing.T) {
	meta := metadata()
	meta.HistoryParentDigest = HistoryRootDigest()
	artifact, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.History == nil || artifact.Manifest.History.ParentDigest != HistoryRootDigest() || artifact.Manifest.History.EntryDigest == "" {
		t.Fatalf("history receipt = %#v", artifact.Manifest.History)
	}
	again, err := Build(Input{Metadata: meta, Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	if again.Manifest.History.EntryDigest != artifact.Manifest.History.EntryDigest {
		t.Fatal("identical bundle inputs produced different history entry digests")
	}
	artifact.Manifest.Purpose = "repair"
	if err := artifact.Manifest.Validate(); err == nil || !strings.Contains(err.Error(), "history entry digest") {
		t.Fatalf("expected changed manifest to invalidate history receipt, got %v", err)
	}
}

func TestBuildNeedsInputStoresDecisionWithoutExecutablePlan(t *testing.T) {
	result := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		Status: protocol.NeedsInput, Questions: []protocol.Question{{ID: "rename", Kind: "rename_table", Key: "table:app:old", Choices: []string{"table:app:new"}}},
	}
	artifact, err := Build(Input{Metadata: metadata(), Result: result, Attempt: 7})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.State != "needs_input" || len(artifact.Manifest.Decisions) != 1 {
		t.Fatalf("manifest = %#v", artifact.Manifest)
	}
	if _, ok := artifact.Files["decisions/attempt-007.json"]; !ok {
		t.Fatalf("decision receipt missing: %v", SortedFiles(artifact.Files))
	}
	if _, ok := artifact.Files["plan.json"]; ok {
		t.Fatal("needs_input bundle exposed an executable plan")
	}
}

func TestManifestRejectsLegacyThreePhaseProtocolWithRegenerationAction(t *testing.T) {
	artifact, err := Build(Input{Metadata: metadata(), Result: plannedResult(statement("CREATE TABLE app.users ();", protocol.PhaseExpand, true))})
	if err != nil {
		t.Fatal(err)
	}
	artifact.Manifest.ProtocolVersion = "onwardpg.bundle/v1"
	if err := artifact.Manifest.Validate(); err == nil || !strings.Contains(err.Error(), "regenerate") || !strings.Contains(err.Error(), Version) {
		t.Fatalf("legacy bundle error = %v", err)
	}
}

func TestNeedsSQLEditsBundleBecomesPlannedOnlyAfterTODOIsReplaced(t *testing.T) {
	result := plannedResult(statement("-- ONWARDPG TODO: provide reviewed conversion SQL", protocol.PhaseContract, true))
	result.Status = protocol.NeedsSQLEdits
	artifact, err := Build(Input{Metadata: metadata(), Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Manifest.State != string(protocol.NeedsSQLEdits) || artifact.Manifest.PhaseSource != "generated" {
		t.Fatalf("manifest = %#v", artifact.Manifest)
	}
	destination := filepath.Join(t.TempDir(), "manual-conversion")
	if err := Write(destination, artifact, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	phase := filepath.Join(destination, "phases", "contract.sql")
	if err := os.WriteFile(phase, []byte("ALTER TABLE public.events ALTER COLUMN occurred_on TYPE date USING occurred_on::date;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited, err := PrepareEdited(destination)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Manifest.State != string(protocol.Planned) || edited.Manifest.PhaseSource != "edited" {
		t.Fatalf("edited manifest = %#v", edited.Manifest)
	}
}

func TestWritePreservesDecisionHistoryAcrossDraftReplacement(t *testing.T) {
	decisionResult := protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		Status: protocol.NeedsInput, Questions: []protocol.Question{{ID: "rename", Kind: "rename_table", Key: "table:app:old", Choices: []string{"table:app:new"}}},
	}
	decision, err := Build(Input{Metadata: metadata(), Result: decisionResult, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "bundle")
	if err := Write(destination, decision, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, err := Build(Input{Metadata: metadata(), Result: plannedResult()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(destination, ready, WriteOptions{ReplaceDraft: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.State != "planned" || len(manifest.Decisions) != 1 || manifest.Decisions[0].Path != "decisions/attempt-001.json" {
		t.Fatalf("decision history was not preserved: %#v", manifest)
	}
	generation, attempt, err := NextCoordinates(destination, metadata(), plannedResult())
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 || attempt != 2 {
		t.Fatalf("next coordinates = (%d, %d), want (1, 2)", generation, attempt)
	}
	generation, attempt, err = NextCoordinates(destination, metadata(), decisionResult)
	if err != nil || generation != 1 || attempt != 1 {
		t.Fatalf("repeated decision coordinates = (%d, %d, %v), want (1, 1, nil)", generation, attempt, err)
	}
	changed := metadata()
	changed.HistoryParentDigest = desiredFingerprint
	generation, attempt, err = NextCoordinates(destination, changed, plannedResult())
	if err != nil || generation != 2 || attempt != 1 {
		t.Fatalf("changed-history coordinates = (%d, %d, %v), want (2, 1, nil)", generation, attempt, err)
	}
}

func TestBuildRejectsMismatchedReceiptAndIncompletePlan(t *testing.T) {
	meta := metadata()
	meta.DesiredSource.Fingerprint = currentFingerprint
	if _, err := Build(Input{Metadata: meta, Result: plannedResult()}); err == nil {
		t.Fatal("expected desired fingerprint mismatch")
	}
	meta = metadata()
	bad := protocol.Statement{SQL: "SELECT 1;", Phase: "expand", Safety: "safe"}
	if _, err := Build(Input{Metadata: meta, Result: protocol.Result{
		ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
		Status: protocol.Planned, Statements: []protocol.Statement{bad}, Batches: []protocol.Batch{{ID: "batch-001", Phase: "expand", Transactional: true, Statements: []protocol.Statement{bad}}},
	}}); err == nil {
		t.Fatal("expected missing stable statement id")
	}
}

func TestManifestRejectsPathTokensAndSecretDescriptions(t *testing.T) {
	for _, name := range []string{".", "..", "..."} {
		meta := metadata()
		meta.BundleID = name
		if _, err := Build(Input{Metadata: meta, Result: plannedResult()}); err == nil {
			t.Fatalf("accepted unsafe bundle id %q", name)
		}
	}
	for _, description := range []string{
		"postgres://user:secret@example.test/db",
		"host=db.example.test user=app password=secret dbname=app",
		"host=db.example.test passfile=/tmp/pgpass",
	} {
		meta := metadata()
		meta.BaselineSource.Description = description
		if _, err := Build(Input{Metadata: meta, Result: plannedResult()}); err == nil {
			t.Fatalf("accepted secret-bearing description %q", description)
		}
	}
}

func TestWriteRequiresExplicitSafeDraftReplacement(t *testing.T) {
	artifact, err := Build(Input{Metadata: metadata(), Result: plannedResult()})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "bundle")
	if err := Write(destination, artifact, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Write(destination, artifact, WriteOptions{}); err == nil {
		t.Fatal("expected implicit replacement rejection")
	}
	if err := Write(destination, artifact, WriteOptions{ReplaceDraft: true}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveDraftRequiresExactReceiptedArtifact(t *testing.T) {
	artifact, err := Build(Input{Metadata: metadata(), Result: plannedResult()})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "bundle")
	if err := Write(destination, artifact, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	staleMetadata := metadata()
	staleMetadata.Purpose = "repair"
	stale, err := Build(Input{Metadata: staleMetadata, Result: plannedResult()})
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveDraft(destination, stale); err == nil {
		t.Fatal("remove accepted a stale in-memory artifact")
	}
	if err := RemoveDraft(destination, artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("removed draft still exists: %v", err)
	}
}

func TestMoveToVerifiedBackupRestoresAChangedDraft(t *testing.T) {
	expected, err := Build(Input{Metadata: metadata(), Result: plannedResult(
		statement("CREATE TABLE app.users (id bigint);", "expand", true),
	)})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "bundle")
	if err := Write(destination, expected, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	changed, err := Build(Input{Metadata: metadata(), Result: plannedResult(
		statement("CREATE TABLE app.users (id bigint, email text);", "expand", true),
	)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(destination, changed, WriteOptions{ReplaceDraft: true}); err != nil {
		t.Fatal(err)
	}
	backup := destination + ".backup"
	if err := moveToVerifiedBackup(destination, backup, expected); err == nil || !strings.Contains(err.Error(), "changed during lifecycle operation") {
		t.Fatalf("expected exact-artifact mismatch, got %v", err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("mismatched backup was not restored: %v", err)
	}
	restored, err := Read(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !artifactsEqual(restored, changed) {
		t.Fatal("restore did not preserve the changed draft exactly")
	}
}

func TestArtifactValidationRejectsTamperedPhase(t *testing.T) {
	artifact, err := Build(Input{Metadata: metadata(), Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	artifact.Files["phases/expand.sql"] = []byte("DROP DATABASE production;\n")
	if err := artifact.Validate(); err == nil || !strings.Contains(err.Error(), "does not match manifest digest") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}
}

func TestWriteRefusesToReplaceUnownedDirectory(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "notes.txt"), []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, err := Build(Input{Metadata: metadata(), Result: plannedResult()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(destination, artifact, WriteOptions{ReplaceDraft: true}); err == nil || !strings.Contains(err.Error(), "invalid bundle") {
		t.Fatalf("expected unowned directory rejection, got %v", err)
	}
}

func TestWriteRefusesToReplaceTamperedOrAugmentedBundle(t *testing.T) {
	artifact, err := Build(Input{Metadata: metadata(), Result: plannedResult(statement("CREATE TABLE app.users (id bigint);", "expand", true))})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []struct {
		name string
		call func(string) error
	}{
		{name: "tampered", call: func(destination string) error {
			return os.WriteFile(filepath.Join(destination, "phases", "expand.sql"), []byte("SELECT 1;\n"), 0o644)
		}},
		{name: "extra-file", call: func(destination string) error {
			return os.WriteFile(filepath.Join(destination, "notes.txt"), []byte("unrecorded"), 0o644)
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "bundle")
			if err := Write(destination, artifact, WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := mutate.call(destination); err != nil {
				t.Fatal(err)
			}
			if err := Write(destination, artifact, WriteOptions{ReplaceDraft: true}); err == nil || !strings.Contains(err.Error(), "invalid bundle") {
				t.Fatalf("expected invalid existing bundle rejection, got %v", err)
			}
			if _, _, err := NextCoordinates(destination, metadata(), plannedResult()); err == nil {
				t.Fatal("coordinate calculation accepted an invalid existing bundle")
			}
		})
	}
}

func metadata() Metadata {
	return Metadata{
		BundleID: "customer-profile", Generation: 1, Target: "primary-postgres", Purpose: "feature",
		BaselineSource: SourceReceipt{Kind: "onwardpg_history", Description: "replayed onwardpg history", Fingerprint: currentFingerprint, PostgresMajor: 16},
		DesiredSource:  SourceReceipt{Kind: "ddl_export", Description: "project primary schema", Fingerprint: desiredFingerprint, PostgresMajor: 16},
		Planner:        PlannerReceipt{Version: "dev", Options: PlannerOptions{ConcurrentIndexes: true}},
	}
}

func TestSourceReceiptRejectsUnsupportedPostgresMajor(t *testing.T) {
	receipt := SourceReceipt{Kind: "ddl_export", Description: "project schema", Fingerprint: currentFingerprint, PostgresMajor: 14}
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "between 15 and 18") {
		t.Fatalf("PostgreSQL 14 receipt validation = %v", err)
	}
	receipt.PostgresMajor = 15
	if err := receipt.Validate(); err != nil {
		t.Fatalf("PostgreSQL 15 receipt validation = %v", err)
	}
}

func statement(sql, phase string, transactional bool) protocol.Statement {
	result := protocol.Statement{SQL: sql, Phase: phase, Safety: "review", NonTransactional: !transactional}
	result.ID = protocol.StableStatementID(result)
	return result
}

func plannedResult(statements ...protocol.Statement) protocol.Result {
	result := protocol.Result{ProtocolVersion: protocol.Version, CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint, Status: protocol.Planned, Statements: statements}
	for _, item := range statements {
		transactional := !item.NonTransactional
		if len(result.Batches) > 0 {
			last := &result.Batches[len(result.Batches)-1]
			if last.Phase == item.Phase && last.Transactional == transactional {
				last.Statements = append(last.Statements, item)
				continue
			}
		}
		result.Batches = append(result.Batches, protocol.Batch{ID: "batch-" + string(rune('1'+len(result.Batches))), Phase: item.Phase, Transactional: transactional, Statements: []protocol.Statement{item}})
	}
	return result
}
