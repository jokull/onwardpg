package prflow

import (
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
)

func TestFinalizeRebindReportDefersUnreachedAnswers(t *testing.T) {
	report := protocol.RebindReport{ProtocolVersion: protocol.RebindVersion, Carried: []string{"rename_table:table:app:old"}}
	carried := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: "current", DesiredFingerprint: "desired"}
	remaining := map[string]protocol.Answer{"drop:table:app:later": {Kind: "drop", Key: "table:app:later", Value: "drop"}}
	questions := []protocol.Question{{Kind: "set_not_null", Key: "column:app:users:email"}}
	finalizeRebindReport(&report, carried, remaining, questions, false)
	if len(report.Deferred) != 1 || report.Deferred[0] != "drop:table:app:later" || len(report.Unanswered) != 1 {
		t.Fatalf("report = %#v", report)
	}
}
