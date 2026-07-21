package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStableStatementIDUsesCompleteNormalizedContract(t *testing.T) {
	left := Statement{SQL: "CREATE INDEX idx ON items (id);", Safety: "review", Phase: "expand", Hazards: []string{"lock", "rewrite"}}
	right := left
	right.Hazards = []string{"rewrite", "lock"}
	if StableStatementID(left) != StableStatementID(right) {
		t.Fatal("hazard insertion order changed stable statement identity")
	}
	right.SQL = "CREATE INDEX idx ON items (other_id);"
	if StableStatementID(left) == StableStatementID(right) {
		t.Fatal("different SQL received the same stable statement identity")
	}
	right = left
	right.NonTransactional = true
	if StableStatementID(left) == StableStatementID(right) {
		t.Fatal("transaction boundary did not affect stable statement identity")
	}
	right = left
	right.BatchBoundaryBefore = true
	if StableStatementID(left) == StableStatementID(right) {
		t.Fatal("explicit batch boundary did not affect stable statement identity")
	}
	right = left
	right.StatementTimeoutMS, right.LockTimeoutMS = 1200000, 3000
	if StableStatementID(left) == StableStatementID(right) {
		t.Fatal("timeout guidance did not affect stable statement identity")
	}
	right = left
	right.RequiresGates = []string{"data:b", "data:a"}
	ordered := left
	ordered.RequiresGates = []string{"data:a", "data:b"}
	if StableStatementID(right) != StableStatementID(ordered) {
		t.Fatal("contract gate insertion order changed stable statement identity")
	}
	if StableStatementID(left) == StableStatementID(right) {
		t.Fatal("contract gate requirements did not affect stable statement identity")
	}
}

func TestErrorDiagnosticHasVersionedStableShape(t *testing.T) {
	diagnostic := ErrorDiagnostic("invalid_config", errors.New("bad config"))
	if diagnostic.Status != "error" || diagnostic.Code != "invalid_config" || diagnostic.Message == "" {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestResultJSONV1ContractIncludesExecutionMetadata(t *testing.T) {
	result := Result{
		CurrentFingerprint: "sha256:current",
		DesiredFingerprint: "sha256:desired",
		Status:             Planned,
		Statements: []Statement{{
			SQL: "CREATE INDEX CONCURRENTLY idx ON items (id);", Safety: "review", Phase: "expand", Hazards: []string{"non_transactional"}, NonTransactional: true,
		}},
		Batches: []Batch{{ID: "expand-1", Phase: "expand", Transactional: false, Statements: []Statement{{SQL: "CREATE INDEX CONCURRENTLY idx ON items (id);", Safety: "review", Phase: "expand"}}}},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document["protocol_version"]; exists {
		t.Fatalf("result must not expose a protocol version: %#v", document)
	}
	if _, exists := document["statements"]; !exists {
		t.Fatal("result must expose statements")
	}
	batches, ok := document["batches"].([]any)
	if !ok || len(batches) != 1 {
		t.Fatalf("batches = %#v", document["batches"])
	}
	statement := document["statements"].([]any)[0].(map[string]any)
	if _, leaked := statement["NonTransactional"]; leaked {
		t.Fatal("internal NonTransactional metadata leaked into statement JSON")
	}
	if _, leaked := statement["non_transactional"]; leaked {
		t.Fatal("batch execution boundary, not statements, is the v1 public contract")
	}
}

func TestResolverBindsAnswersToFingerprintsAndConsumesThem(t *testing.T) {
	answers := Answers{
		CurrentFingerprint: "sha256:current", DesiredFingerprint: "sha256:desired",
		Answers: []Answer{{Kind: "drop_table", Key: "public.orders", Value: "drop"}},
	}
	resolver, err := answers.Resolver("sha256:current", "sha256:desired")
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := resolver.Resolve(Question{Kind: "drop_table", Key: "public.orders", Choices: []string{"drop", "keep"}})
	if err != nil || !found || value != "drop" {
		t.Fatalf("resolve = %q, %v, %v", value, found, err)
	}
	if err := resolver.ValidateAllUsed(); err != nil {
		t.Fatal(err)
	}
}

func TestResolverRejectsStaleContradictoryInvalidAndUnusedAnswers(t *testing.T) {
	base := Answers{
		CurrentFingerprint: "current", DesiredFingerprint: "desired",
		Answers: []Answer{{Kind: "drop_table", Key: "public.orders", Value: "drop"}},
	}
	if _, err := base.Resolver("other", "desired"); err == nil {
		t.Fatal("expected stale fingerprints to fail")
	}
	contradictory := base
	contradictory.Answers = append(contradictory.Answers, Answer{Kind: "drop_table", Key: "public.orders", Value: "keep"})
	if _, err := contradictory.Resolver("current", "desired"); err == nil {
		t.Fatal("expected contradictory answers to fail")
	}
	resolver, err := base.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.Resolve(Question{Kind: "drop_table", Key: "public.orders", Choices: []string{"keep"}}); err == nil {
		t.Fatal("expected invalid choice to fail")
	}
	if err := resolver.ValidateAllUsed(); err == nil {
		t.Fatal("expected unused answer to fail")
	}
}

func TestResolverRequiresCompleteManualWorkContract(t *testing.T) {
	answers := Answers{CurrentFingerprint: "current", DesiredFingerprint: "desired", Answers: []Answer{{
		Kind: "partition_reconfiguration", Key: "table:app:events_2026", Value: "provided",
		Manual: &ManualWork{Summary: "move rows", ExecutionMode: "transactional", Statements: []string{"ALTER TABLE app.events DETACH PARTITION app.events_2026;"}, VerificationSQL: []string{"SELECT 1;"}},
	}}}
	resolver, err := answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	work, found, err := resolver.ResolveManual(Question{Kind: "partition_reconfiguration", Key: "table:app:events_2026", Choices: []string{"provided"}})
	if err != nil || !found || work.Summary != "move rows" {
		t.Fatalf("ResolveManual = %#v, %v, %v", work, found, err)
	}
	if err := resolver.ValidateAllUsed(); err != nil {
		t.Fatal(err)
	}
	answers.Answers[0].Manual = nil
	resolver, err = answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.ResolveManual(Question{Kind: "partition_reconfiguration", Key: "table:app:events_2026", Choices: []string{"provided"}}); err == nil {
		t.Fatal("expected missing manual contract to fail")
	}
	answers.Answers[0].Manual = &ManualWork{Summary: "move rows", Statements: []string{"SELECT 1;"}}
	resolver, err = answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.ResolveManual(Question{Kind: "partition_reconfiguration", Key: "table:app:events_2026", Choices: []string{"provided"}}); err == nil {
		t.Fatal("expected missing execution mode to fail")
	}
	answers.Answers[0].Manual = &ManualWork{Summary: "move rows\nSELECT unsafe", ExecutionMode: "transactional", Statements: []string{"SELECT 1;"}}
	resolver, err = answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.ResolveManual(Question{Kind: "partition_reconfiguration", Key: "table:app:events_2026", Choices: []string{"provided"}}); err == nil {
		t.Fatal("expected multi-line summary to fail")
	}
	answers.Answers[0].Manual = &ManualWork{Summary: "move rows", ExecutionMode: "transactional", Statements: []string{"SELECT 1;"}, VerificationSQL: []string{"SELECT 1;\nDROP TABLE unsafe;"}}
	resolver, err = answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.ResolveManual(Question{Kind: "partition_reconfiguration", Key: "table:app:events_2026", Choices: []string{"provided"}}); err == nil {
		t.Fatal("expected multi-line verification SQL to fail")
	}
}

func TestResolverRejectsManualPayloadForOrdinaryQuestion(t *testing.T) {
	answers := Answers{CurrentFingerprint: "current", DesiredFingerprint: "desired", Answers: []Answer{{
		Kind: "drop_table", Key: "table:app:orders", Value: "drop",
		Manual: &ManualWork{Summary: "not applicable", ExecutionMode: "transactional", Statements: []string{"SELECT 1;"}},
	}}}
	resolver, err := answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.Resolve(Question{Kind: "drop_table", Key: "table:app:orders", Choices: []string{"drop"}}); err == nil {
		t.Fatal("expected manual payload on ordinary answer to fail")
	}
}

func FuzzResolverAnswerValidation(f *testing.F) {
	f.Add("drop_table", "table:app:orders", "drop")
	f.Add("rename_table", "table:app:orders", "table:app:accounts")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, kind, key, value string) {
		if len(kind) > 256 || len(key) > 256 || len(value) > 256 {
			return
		}
		answers := Answers{
			CurrentFingerprint: "current", DesiredFingerprint: "desired",
			Answers: []Answer{{Kind: kind, Key: key, Value: value}},
		}
		resolver, err := answers.Resolver("current", "desired")
		if err != nil {
			return
		}
		_, _, _ = resolver.Resolve(Question{Kind: kind, Key: key, Choices: []string{"drop", "table:app:accounts"}})
		_ = resolver.ValidateAllUsed()
	})
}

func TestResolverRejectsStaleScopedAnswerAtConsumption(t *testing.T) {
	answers := Answers{
		CurrentFingerprint: "current", DesiredFingerprint: "desired",
		Answers: []Answer{{Kind: "reconcile_contract", Key: "constraint:app:orders:check", Value: "assert_only", QuestionFingerprint: "sha256:old"}},
	}
	resolver, err := answers.Resolver("current", "desired")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolver.Resolve(Question{Kind: "reconcile_contract", Key: "constraint:app:orders:check", Choices: []string{"assert_only"}, ScopeFingerprint: "sha256:new"})
	if err == nil || !strings.Contains(err.Error(), "question fingerprint is stale") {
		t.Fatalf("stale scope error=%v", err)
	}
}

func TestRenderSQLIncludesPhaseAndBatchGuidance(t *testing.T) {
	result := Result{Batches: []Batch{
		{ID: "batch-001", Phase: "expand", Transactional: true, Statements: []Statement{{SQL: "CREATE TABLE x ();"}}},
		{ID: "batch-002", Phase: "contract", Transactional: false, Statements: []Statement{{SQL: "ALTER TABLE x\nADD COLUMN id bigint;"}}},
		{ID: "batch-003", Phase: "contract", Transactional: true, Statements: []Statement{{SQL: "ALTER TABLE x DROP COLUMN legacy;"}}},
	}}
	want := `  -- onwardpg: forward-only PostgreSQL migration plan.
  -- Review every batch, safety classification, and hazard in the JSON plan before execution.
  -- ============================================================================
  -- EXPAND — run before the one application deployment anchored to this plan.
  -- Old code must remain usable while new code begins using the expanded shape.
  -- Transactional and non-transactional batches are marked below; this phase is not split by transaction.
  -- ============================================================================
  -- onwardpg:batch transactional
  -- Batch batch-001: transactional.
  CREATE TABLE x ();

  -- ============================================================================
  -- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.
  -- The one newly deployed application version must work before and after every batch below.
  -- Catch-up, validation, enforcement, and compatibility cleanup belong here.
  -- ============================================================================
  -- onwardpg:batch nontransactional
  -- Batch batch-002: non-transactional; execute outside BEGIN/COMMIT.
  ALTER TABLE x
  ADD COLUMN id bigint;

  -- onwardpg:batch transactional
  -- Batch batch-003: transactional.
  ALTER TABLE x DROP COLUMN legacy;`
	if got := RenderSQL(result, "  "); got != want {
		t.Fatalf("RenderSQL = %q, want %q", got, want)
	}
}

func TestRenderSQLDerivesAnAdHocBatchForLibraryCallers(t *testing.T) {
	result := Result{Statements: []Statement{{SQL: "CREATE TABLE x ();", Phase: "expand"}}}
	if got := RenderSQL(result, ""); !strings.Contains(got, "-- Batch ad-hoc: transactional.") || !strings.Contains(got, "CREATE TABLE x ();") {
		t.Fatalf("RenderSQL = %q", got)
	}
}

func TestRenderSQLIncludesTimeoutGuidanceWithoutApplyingIt(t *testing.T) {
	result := Result{Statements: []Statement{{
		SQL: "CREATE INDEX CONCURRENTLY idx ON items (id);", Phase: "expand", NonTransactional: true,
		Safety: "review", Hazards: []string{"index_build", "availability_risk"},
		StatementTimeoutMS: 1200000, LockTimeoutMS: 3000,
	}}}
	rendered := RenderSQL(result, "")
	if !strings.Contains(rendered, "-- Review: safety=review; hazards=index_build,availability_risk.") {
		t.Fatalf("rendered plan lost review metadata: %q", rendered)
	}
	if !strings.Contains(rendered, "-- Suggested session timeouts: statement_timeout=20m, lock_timeout=3s.") {
		t.Fatalf("rendered plan lost timeout guidance: %q", rendered)
	}
	if strings.Contains(rendered, "SET statement_timeout") || strings.Contains(rendered, "SET lock_timeout") {
		t.Fatalf("review guidance must not become an apply command: %q", rendered)
	}
}

func TestRenderSQLIncludesProductSpecificNonTransactionalBoundary(t *testing.T) {
	result := Result{Batches: []Batch{{
		ID: "batch-004", Phase: "contract", Transactional: false,
		Statements: []Statement{{SQL: "-- PRODUCT-SPECIFIC SQL: build concurrently\nCREATE INDEX CONCURRENTLY idx ON items (id);", Phase: "contract", Safety: "manual"}},
	}}}
	rendered := RenderSQL(result, "")
	if !strings.Contains(rendered, "-- CONTRACT — run after pre-deployment instances") || !strings.Contains(rendered, "-- Batch batch-004: non-transactional; execute outside BEGIN/COMMIT.") || !strings.Contains(rendered, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("manual SQL rendering lost execution guidance: %q", rendered)
	}
}

func TestRenderSQLBoundsEditableTODOWithStableMarkers(t *testing.T) {
	statement := Statement{ID: "stmt-sha256-editable", SQL: "-- ONWARDPG TODO: supply overlap SQL", Phase: PhaseExpand, Safety: "manual"}
	rendered := RenderSQL(Result{Batches: []Batch{{ID: "batch-expand-001", Phase: PhaseExpand, Transactional: true, Statements: []Statement{statement}}}}, "")
	if !strings.Contains(rendered, "-- onwardpg:edit begin "+statement.ID+"\n-- ONWARDPG TODO") || !strings.Contains(rendered, "-- onwardpg:edit end "+statement.ID) {
		t.Fatalf("editable SQL pocket is not stable and bounded: %q", rendered)
	}
}

func TestRenderSQLExplainsSameTypeRenameShadowCleanup(t *testing.T) {
	drop := Statement{
		SQL: "ALTER TABLE \"app\".\"accounts\" DROP COLUMN \"full_name\";", Phase: PhaseContract,
		Safety: "dangerous", Hazards: []string{"compatibility_removal", "data_loss"},
	}
	rename := Statement{
		SQL: "ALTER TABLE \"app\".\"accounts\" RENAME COLUMN \"display_name\" TO \"full_name\";", Phase: PhaseContract,
		Safety: "review", Hazards: []string{"compatibility_removal"},
	}
	rendered := RenderSQL(Result{Batches: []Batch{{
		ID: "batch-contract-001", Phase: PhaseContract, Transactional: true, Statements: []Statement{drop, rename},
	}}}, "")
	for _, explanation := range []string{
		"equality was asserted; remove the synchronized shadow",
		"original column keeps its storage and dependencies and now takes the final name",
	} {
		if !strings.Contains(rendered, explanation) {
			t.Fatalf("rename choreography omitted %q:\n%s", explanation, rendered)
		}
	}
	unrelated := rename
	unrelated.SQL = "ALTER TABLE \"app\".\"other_accounts\" RENAME COLUMN \"display_name\" TO \"full_name\";"
	if rendered := RenderSQL(Result{Statements: []Statement{drop, unrelated}}, ""); strings.Contains(rendered, "onwardpg rename transition") {
		t.Fatalf("unrelated DROP/RENAME pair received rename choreography guidance:\n%s", rendered)
	}
}
