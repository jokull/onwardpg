package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResultJSONV1ContractIncludesExecutionMetadata(t *testing.T) {
	result := Result{
		ProtocolVersion:    Version,
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
	if got := document["protocol_version"]; got != Version {
		t.Fatalf("protocol_version = %#v, want %q", got, Version)
	}
	if _, exists := document["statements"]; !exists {
		t.Fatal("v1 result must expose statements")
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
		ProtocolVersion: Version, CurrentFingerprint: "sha256:current", DesiredFingerprint: "sha256:desired",
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
		ProtocolVersion: Version, CurrentFingerprint: "current", DesiredFingerprint: "desired",
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
	answers := Answers{ProtocolVersion: Version, CurrentFingerprint: "current", DesiredFingerprint: "desired", Answers: []Answer{{
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
	answers := Answers{ProtocolVersion: Version, CurrentFingerprint: "current", DesiredFingerprint: "desired", Answers: []Answer{{
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
			ProtocolVersion: Version, CurrentFingerprint: "current", DesiredFingerprint: "desired",
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

func TestRenderSQLIncludesPhaseAndBatchGuidance(t *testing.T) {
	result := Result{Batches: []Batch{
		{ID: "batch-001", Phase: "expand", Transactional: true, Statements: []Statement{{SQL: "CREATE TABLE x ();"}}},
		{ID: "batch-002", Phase: "migrate", Transactional: false, Statements: []Statement{{SQL: "ALTER TABLE x\nADD COLUMN id bigint;"}}},
		{ID: "batch-003", Phase: "contract", Transactional: true, Statements: []Statement{{SQL: "ALTER TABLE x DROP COLUMN legacy;"}}},
	}}
	want := `  -- onwardpg: forward-only PostgreSQL migration plan.
  -- Review every batch, safety classification, and hazard in the JSON plan before execution.
  -- ============================================================================
  -- EXPAND — run before deploying application code that relies on the new shape.
  -- Keep this compatible with the application version currently in production.
  -- ============================================================================
  -- Batch batch-001: transactional.
  CREATE TABLE x ();

  -- ============================================================================
  -- MIGRATE — run after compatible code is deployed; review data and lock hazards.
  -- Add any application-specific backfill here or run it separately and observe it.
  -- onwardpg never invents a cast or data transform that schema state cannot prove.
  -- ============================================================================
  -- Batch batch-002: non-transactional; execute outside BEGIN/COMMIT.
  ALTER TABLE x
  ADD COLUMN id bigint;

  -- ============================================================================
  -- CONTRACT — run only after old application code no longer uses the prior shape.
  -- This section can remove compatibility paths or enforce the final contract.
  -- ============================================================================
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

func TestRenderSQLIncludesManualNonTransactionalBoundary(t *testing.T) {
	result := Result{Batches: []Batch{{
		ID: "batch-004", Phase: "manual", Transactional: false,
		Statements: []Statement{{SQL: "-- MANUAL CONTRACT: build concurrently\nCREATE INDEX CONCURRENTLY idx ON items (id);", Phase: "manual", Safety: "manual"}},
	}}}
	rendered := RenderSQL(result, "")
	if !strings.Contains(rendered, "-- MANUAL — review and execute only with an explicit operator decision.") || !strings.Contains(rendered, "-- Batch batch-004: non-transactional; execute outside BEGIN/COMMIT.") || !strings.Contains(rendered, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("manual SQL rendering lost execution guidance: %q", rendered)
	}
}
