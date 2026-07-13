package prflow

import (
	"context"
	"fmt"
	"sort"

	"github.com/jokull/onwardpg/internal/protocol"
)

// AnalyzeWithReboundAnswers repeatedly carries exact question-scope matches
// through the planner's staged question flow. Decisions not yet reachable are
// deferred, not guessed or prematurely declared invalid.
func AnalyzeWithReboundAnswers(ctx context.Context, input Input, previous protocol.Answers, previousQuestions []protocol.Question) (Analysis, error) {
	if len(previous.Answers) == 0 {
		return Analyze(ctx, input)
	}
	remaining := make(map[string]protocol.Answer, len(previous.Answers))
	for _, answer := range previous.Answers {
		id := answer.Kind + ":" + answer.Key
		if _, exists := remaining[id]; exists {
			return Analysis{}, fmt.Errorf("duplicate previous answer %s", id)
		}
		remaining[id] = answer
	}
	report := protocol.RebindReport{ProtocolVersion: protocol.RebindVersion}
	var carried protocol.Answers
	for iteration := 0; iteration <= len(previous.Answers); iteration++ {
		input.Answers = carried
		analysis, err := Analyze(ctx, input)
		if err != nil {
			return Analysis{}, err
		}
		if analysis.Plan == nil {
			finalizeRebindReport(&report, carried, remaining, nil, true)
			analysis.Rebind = &report
			return analysis, nil
		}
		carried.ProtocolVersion = protocol.Version
		carried.CurrentFingerprint = analysis.Plan.CurrentFingerprint
		carried.DesiredFingerprint = analysis.Plan.DesiredFingerprint
		if analysis.Plan.Status != protocol.NeedsInput {
			finalizeRebindReport(&report, carried, remaining, nil, true)
			analysis.Rebind = &report
			return analysis, nil
		}

		candidates := protocol.Answers{
			ProtocolVersion: previous.ProtocolVersion, CurrentFingerprint: previous.CurrentFingerprint,
			DesiredFingerprint: previous.DesiredFingerprint,
		}
		for _, id := range sortedAnswerIDs(remaining) {
			candidates.Answers = append(candidates.Answers, remaining[id])
		}
		stage, err := protocol.RebindAnswers(candidates, previousQuestions, analysis.Plan.Questions, analysis.Plan.CurrentFingerprint, analysis.Plan.DesiredFingerprint)
		if err != nil {
			return Analysis{}, err
		}
		for _, finding := range stage.Invalidated {
			if finding.Reason == "question_no_longer_present" {
				continue
			}
			report.Invalidated = append(report.Invalidated, finding)
			delete(remaining, finding.Decision)
		}
		if len(stage.Answers.Answers) == 0 {
			finalizeRebindReport(&report, carried, remaining, analysis.Plan.Questions, false)
			analysis.Rebind = &report
			return analysis, nil
		}
		for _, answer := range stage.Answers.Answers {
			id := answer.Kind + ":" + answer.Key
			carried.Answers = append(carried.Answers, answer)
			report.Carried = append(report.Carried, id)
			delete(remaining, id)
		}
	}
	return Analysis{}, fmt.Errorf("answer rebinding did not converge")
}

func finalizeRebindReport(report *protocol.RebindReport, carried protocol.Answers, remaining map[string]protocol.Answer, questions []protocol.Question, final bool) {
	report.Answers = carried
	sort.Strings(report.Carried)
	if final {
		for _, id := range sortedAnswerIDs(remaining) {
			report.Invalidated = append(report.Invalidated, protocol.RebindFinding{Decision: id, Reason: "question_no_longer_present"})
		}
		sort.Slice(report.Invalidated, func(i, j int) bool { return report.Invalidated[i].Decision < report.Invalidated[j].Decision })
		return
	}
	sort.Slice(report.Invalidated, func(i, j int) bool { return report.Invalidated[i].Decision < report.Invalidated[j].Decision })
	for _, question := range questions {
		report.Unanswered = append(report.Unanswered, question.Kind+":"+question.Key)
	}
	for _, id := range sortedAnswerIDs(remaining) {
		report.Deferred = append(report.Deferred, id)
	}
	sort.Strings(report.Unanswered)
}

func sortedAnswerIDs(answers map[string]protocol.Answer) []string {
	ids := make([]string, 0, len(answers))
	for id := range answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
