package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
)

func TestAppendWarningsAddsOneLastMemberToAResultDocument(t *testing.T) {
	warnings := []protocol.Warning{protocol.ExportSideEffects([]string{"a.txt", "dir/b <1>.txt"})}
	for name, document := range map[string]string{
		"result":       "{\"status\":\"verified\",\"target\":\"primary\"}\n",
		"empty object": "{}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := appendWarnings([]byte(document), nil); string(got) != document {
				t.Fatalf("a document without warnings changed: %s", got)
			}
			got := appendWarnings([]byte(document), warnings)
			if !bytes.HasSuffix(got, []byte("}\n")) {
				t.Fatalf("document = %q, want one JSON line", got)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("document is not valid JSON: %v\n%s", err, got)
			}
			var original map[string]json.RawMessage
			if err := json.Unmarshal([]byte(document), &original); err != nil {
				t.Fatal(err)
			}
			if len(decoded) != len(original)+1 {
				t.Fatalf("members = %d, want the %d original members and warnings", len(decoded), len(original))
			}
			var read []protocol.Warning
			if err := json.Unmarshal(decoded["warnings"], &read); err != nil {
				t.Fatal(err)
			}
			if len(read) != 1 || read[0].Code != protocol.ExportSideEffectsCode || read[0].PathCount != 2 || len(read[0].Paths) != 2 || read[0].Paths[1] != "dir/b <1>.txt" {
				t.Fatalf("warnings = %#v", read)
			}
			// Every byte of the original document is kept, in order.
			if !bytes.HasPrefix(got, []byte(document[:len(document)-2])) {
				t.Fatalf("the original members changed: %s", got)
			}
		})
	}
	// Output that is not a JSON object is left as it is.
	for _, document := range []string{"SELECT 1;\n", "[1]\n", ""} {
		if got := appendWarnings([]byte(document), warnings); string(got) != document {
			t.Fatalf("appendWarnings(%q) = %q", document, got)
		}
	}
}

func TestExportSideEffectsWarningListsABoundedNumberOfPaths(t *testing.T) {
	paths := make([]string, protocol.MaxWarningPaths+7)
	for index := range paths {
		paths[index] = "file"
	}
	warning := protocol.ExportSideEffects(paths)
	if len(warning.Paths) != protocol.MaxWarningPaths || warning.PathCount != len(paths) {
		t.Fatalf("paths = %d, path_count = %d", len(warning.Paths), warning.PathCount)
	}
}

func TestQueuedWarningsAreCarriedOnceByTheNextResult(t *testing.T) {
	queueWarning(protocol.IndexLockModeChanged("search", true, false, "flag"))
	queueWarning(protocol.IndexLockModeDiffersFromConfig("search"))
	var document bytes.Buffer
	if err := writeJSON(&document, struct {
		Status string `json:"status"`
	}{"ready"}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Warnings []protocol.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(document.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Warnings) != 2 || decoded.Warnings[0].Code != protocol.IndexLockModeChangedCode || decoded.Warnings[1].Code != protocol.IndexLockModeDiffersFromConfigCode {
		t.Fatalf("warnings = %#v", decoded.Warnings)
	}
	for _, fragment := range []string{"bundle search", "from concurrent", "to blocking", "--concurrent-indexes flag"} {
		if !bytes.Contains([]byte(decoded.Warnings[0].Message), []byte(fragment)) {
			t.Fatalf("message %q lacks %q", decoded.Warnings[0].Message, fragment)
		}
	}
	if decoded.Warnings[0].Remediation != "if the change is not intended, rerun the plan with --concurrent-indexes" {
		t.Fatalf("remediation = %q", decoded.Warnings[0].Remediation)
	}
	if len(pendingWarnings()) != 0 {
		t.Fatal("a carried warning stayed pending")
	}
}

func TestHintsFileActionListsOnlySingleChoiceDecisions(t *testing.T) {
	drop := func(name string) protocol.DecisionChoice {
		return protocol.DecisionChoice{Hint: protocol.Hint{Kind: "drop", Object: "column", Name: []string{"app", "t", name}}, Hazards: []string{"data_loss"}}
	}
	rename := protocol.DecisionChoice{Hint: protocol.Hint{Kind: "rename", Object: "column", From: []string{"app", "t", "c"}, To: []string{"app", "t", "d"}}}
	carried := []protocol.Hint{{Kind: "drop", Object: "table", Name: []string{"app", "old"}}}
	argv := []string{"onwardpg", "plan"}
	if action := hintsFileAction("durable", "--hints-file", argv, carried, []protocol.Decision{{Choices: []protocol.DecisionChoice{drop("a")}}, {Choices: []protocol.DecisionChoice{rename, drop("c")}}}); action != nil {
		t.Fatalf("one single-choice decision must not make a file action: %#v", action)
	}
	action := hintsFileAction("durable", "--hints-file", argv, carried, []protocol.Decision{
		{Choices: []protocol.DecisionChoice{drop("a")}}, {Choices: []protocol.DecisionChoice{rename, drop("c")}}, {Choices: []protocol.DecisionChoice{drop("b")}},
	})
	if action == nil || action.Kind != "semantic_hints_file" || action.DecisionCount != 2 || len(action.Hints) != 3 ||
		action.Hints[0].Object != "table" || action.Hints[1].Name[2] != "a" || action.Hints[2].Name[2] != "b" ||
		len(action.Hazards) != 1 || action.Hazards[0] != "data_loss" || action.Argv[len(action.Argv)-1] != hintsFileName || len(argv) != 2 {
		t.Fatalf("action = %#v", action)
	}
}
