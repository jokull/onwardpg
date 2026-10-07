package timing

import (
	"bytes"
	"encoding/json"
	"testing"
)

func reset() {
	mu.Lock()
	defer mu.Unlock()
	enabled = false
	stages = nil
}

func TestDisabledRecorderWritesNothing(t *testing.T) {
	reset()
	Start("stage")()
	var output bytes.Buffer
	Write(&output)
	if output.Len() != 0 {
		t.Fatalf("a disabled recorder wrote %q", output.String())
	}
	if report := Snapshot(); len(report.Stages) != 0 {
		t.Fatalf("a disabled recorder kept stages: %#v", report.Stages)
	}
}

func TestRecorderSumsSpansInFirstUseOrder(t *testing.T) {
	reset()
	Enable()
	t.Cleanup(reset)
	Start("second_to_end")
	first := Start("first")
	first()
	Start("first")()
	Start("last")()

	var output bytes.Buffer
	Write(&output)
	var document struct {
		Timings struct {
			Total  float64 `json:"total_ms"`
			Stages []struct {
				Name  string  `json:"name"`
				Count int     `json:"count"`
				MS    float64 `json:"ms"`
			} `json:"stages"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("report is not one JSON document: %v: %q", err, output.String())
	}
	if bytes.Count(output.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("report is not one line: %q", output.String())
	}
	stages := document.Timings.Stages
	if len(stages) != 2 || stages[0].Name != "first" || stages[0].Count != 2 || stages[1].Name != "last" || stages[1].Count != 1 {
		t.Fatalf("stages = %#v", stages)
	}
	// A span that was opened and never closed is not a stage.
	for _, stage := range stages {
		if stage.Name == "second_to_end" {
			t.Fatalf("an open span was reported: %#v", stages)
		}
		if stage.MS < 0 {
			t.Fatalf("stage %s has a negative duration", stage.Name)
		}
	}
}
