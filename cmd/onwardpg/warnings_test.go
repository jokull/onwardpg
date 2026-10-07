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
