package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/protocol"
)

func TestReadHintsAcceptsPredictableInlineIntent(t *testing.T) {
	hints, err := readHints([]string{`{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"]}`}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 1 || hints[0].Kind != "rename" || hints[0].To[2] != "display_name" {
		t.Fatalf("hints = %#v", hints)
	}
}

func TestReadHintsAcceptsAheadOfTimeDecisionFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.hints.json")
	writeTestFile(t, filepath.Dir(path), filepath.Base(path), `[
		{"kind":"rollout","name":["public","events","occurred_on"],"strategy":"staged_with_backfill"},
		{"kind":"manual_sql","object":"column","name":["public","events","occurred_on"],"action":"backfill_not_null"}
	]`)
	hints, err := readHints(nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(hints) != 2 || hints[0].Kind != "rollout" || hints[1].Action != "backfill_not_null" {
		t.Fatalf("hints = %#v", hints)
	}
}

func TestWriteDraftDecisionJSONContainsOnlyIrreducibleExchange(t *testing.T) {
	report := draftflow.Report{
		ProtocolVersion:    draftflow.Version,
		Outcome:            string(protocol.NeedsInput),
		Target:             "primary",
		BundleID:           "customer-profile",
		CurrentFingerprint: "sha256:" + strings.Repeat("a", 64),
		DesiredFingerprint: "sha256:" + strings.Repeat("b", 64),
		Decisions: []protocol.Decision{{
			Choices: []protocol.DecisionChoice{{
				Hint:    protocol.Hint{Kind: "drop", Object: "column", Name: []string{"public", "users", "legacy"}},
				Hazards: []string{"data_loss"},
			}},
		}},
	}
	var output bytes.Buffer
	if err := writeDraftReport(&output, report, "json"); err != nil {
		t.Fatal(err)
	}
	want := "{\"protocol\":\"onwardpg/draft/2\",\"status\":\"needs_decisions\",\"decisions\":[{\"choices\":[{\"hint\":{\"kind\":\"drop\",\"object\":\"column\",\"name\":[\"public\",\"users\",\"legacy\"]},\"hazards\":[\"data_loss\"]}]}]}\n"
	if output.String() != want {
		t.Fatalf("decision protocol bytes changed:\n got: %s\nwant: %s", output.String(), want)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 3 || string(document["protocol"]) != `"onwardpg/draft/2"` || string(document["status"]) != `"needs_decisions"` {
		t.Fatalf("document = %s", output.String())
	}
	if _, exists := document["target"]; exists {
		t.Fatalf("decision exchange echoed inferable target: %s", output.String())
	}
	if bytes.Contains(output.Bytes(), []byte("fingerprint")) || bytes.Contains(output.Bytes(), []byte("customer-profile")) {
		t.Fatalf("decision exchange leaked receipt-only fields: %s", output.String())
	}
}

func TestWriteDecisionsTextProducesCopyableHints(t *testing.T) {
	var output bytes.Buffer
	err := writeDecisionsText(&output, "dev plan", []protocol.Decision{{Choices: []protocol.DecisionChoice{
		{Hint: protocol.Hint{Kind: "type_change", Name: []string{"public", "events", "occurred_on"}, Strategy: "manual_sql"}, Hazards: []string{"manual_sql"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "dev plan needs 1 decision(s)") ||
		!strings.Contains(output.String(), `--hint "{\"kind\":\"type_change\",\"name\":[\"public\",\"events\",\"occurred_on\"],\"strategy\":\"manual_sql\"}"`) ||
		!strings.Contains(output.String(), "hazards: manual_sql") {
		t.Fatalf("text decisions = %s", output.String())
	}
}

func TestDevPlanRejectsUnknownOutputBeforeReadingConfiguration(t *testing.T) {
	output := captureStdout(t, func() int {
		return runDevAt([]string{"plan", "--target", "primary", "--output", "yaml"}, t.TempDir())
	})
	if output.code != 1 || !strings.Contains(output.stdout, "dev plan --output must be text or json") {
		t.Fatalf("output = %#v", output)
	}
}

func TestVersionedDiagnosticContract(t *testing.T) {
	diagnostic := protocol.ErrorDiagnostic("invalid_invocation", errors.New("bad flags"))
	data, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), protocol.DiagnosticVersion) || !contains(string(data), `"code":"invalid_invocation"`) {
		t.Fatalf("diagnostic = %s", data)
	}
}

func TestVersionCommandReportsEmbeddedBuildVersion(t *testing.T) {
	previousArgs, previousVersion := os.Args, buildVersion
	defer func() { os.Args, buildVersion = previousArgs, previousVersion }()
	os.Args = []string{"onwardpg", "version"}
	buildVersion = "v0.1.0-preview.1"
	output := captureStdout(t, run)
	if output.code != 0 || strings.TrimSpace(output.stdout) != `{"version":"v0.1.0-preview.1"}` {
		t.Fatalf("version output = %#v", output)
	}
}

func writeTestFile(t *testing.T, root, name, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
