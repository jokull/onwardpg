package protocol

import (
	"fmt"
	"sort"
)

type RebindFinding struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type RebindReport struct {
	Answers     Answers         `json:"answers"`
	Questions   []Question      `json:"questions,omitempty"`
	Carried     []string        `json:"carried,omitempty"`
	Invalidated []RebindFinding `json:"invalidated,omitempty"`
	Unanswered  []string        `json:"unanswered,omitempty"`
	Deferred    []string        `json:"deferred,omitempty"`
}

// RebindAnswers carries only decisions whose exact question scope survives.
// Whole-schema fingerprints are deliberately replaced only after the old
// answer has been matched to its old question and the old/new scoped object
// receipts are identical.
func RebindAnswers(previous Answers, previousQuestions, currentQuestions []Question, currentFingerprint, desiredFingerprint string) (RebindReport, error) {
	if currentFingerprint == "" || desiredFingerprint == "" {
		return RebindReport{}, fmt.Errorf("current answer fingerprints are required")
	}
	oldQuestions, err := questionMap(previousQuestions)
	if err != nil {
		return RebindReport{}, fmt.Errorf("previous questions: %w", err)
	}
	newQuestions, err := questionMap(currentQuestions)
	if err != nil {
		return RebindReport{}, fmt.Errorf("current questions: %w", err)
	}
	report := RebindReport{Answers: Answers{
		CurrentFingerprint: currentFingerprint, DesiredFingerprint: desiredFingerprint,
	}}
	seenAnswers := make(map[string]bool, len(previous.Answers))
	carried := make(map[string]bool)
	for _, answer := range previous.Answers {
		id := answer.Kind + ":" + answer.Key
		if answer.Kind == "" || answer.Key == "" || answer.Value == "" {
			return RebindReport{}, fmt.Errorf("answer kind, key and value are required")
		}
		if seenAnswers[id] {
			return RebindReport{}, fmt.Errorf("duplicate or contradictory previous answer %s", id)
		}
		seenAnswers[id] = true
		oldQuestion, exists := oldQuestions[id]
		if !exists {
			report.Invalidated = append(report.Invalidated, RebindFinding{Decision: id, Reason: "previous_question_missing"})
			continue
		}
		if oldQuestion.CurrentFingerprint != previous.CurrentFingerprint || oldQuestion.DesiredFingerprint != previous.DesiredFingerprint {
			return RebindReport{}, fmt.Errorf("previous question %s does not match answer fingerprints", id)
		}
		if oldQuestion.ScopeFingerprint == "" || answer.QuestionFingerprint != "" && answer.QuestionFingerprint != oldQuestion.ScopeFingerprint {
			report.Invalidated = append(report.Invalidated, RebindFinding{Decision: id, Reason: "previous_scope_invalid"})
			continue
		}
		newQuestion, exists := newQuestions[id]
		if !exists {
			report.Invalidated = append(report.Invalidated, RebindFinding{Decision: id, Reason: "question_no_longer_present"})
			continue
		}
		if newQuestion.ScopeFingerprint == "" || newQuestion.ScopeFingerprint != oldQuestion.ScopeFingerprint {
			report.Invalidated = append(report.Invalidated, RebindFinding{Decision: id, Reason: "question_scope_changed"})
			continue
		}
		if !answerValidForQuestion(answer, newQuestion) {
			report.Invalidated = append(report.Invalidated, RebindFinding{Decision: id, Reason: "answer_no_longer_valid"})
			continue
		}
		answer.QuestionFingerprint = newQuestion.ScopeFingerprint
		report.Answers.Answers = append(report.Answers.Answers, answer)
		report.Carried = append(report.Carried, id)
		carried[id] = true
	}
	for id := range newQuestions {
		if !carried[id] {
			report.Unanswered = append(report.Unanswered, id)
		}
	}
	sort.Strings(report.Carried)
	sort.Slice(report.Answers.Answers, func(i, j int) bool {
		left := report.Answers.Answers[i].Kind + ":" + report.Answers.Answers[i].Key
		right := report.Answers.Answers[j].Kind + ":" + report.Answers.Answers[j].Key
		return left < right
	})
	sort.Slice(report.Invalidated, func(i, j int) bool { return report.Invalidated[i].Decision < report.Invalidated[j].Decision })
	sort.Strings(report.Unanswered)
	return report, nil
}

func questionMap(questions []Question) (map[string]Question, error) {
	result := make(map[string]Question, len(questions))
	for _, question := range questions {
		id := question.Kind + ":" + question.Key
		if question.Kind == "" || question.Key == "" {
			return nil, fmt.Errorf("question kind and key are required")
		}
		if _, exists := result[id]; exists {
			return nil, fmt.Errorf("duplicate question %s", id)
		}
		result[id] = question
	}
	return result, nil
}

func answerValidForQuestion(answer Answer, question Question) bool {
	if answer.Manual != nil {
		return answer.Value == "provided" && len(answer.Manual.Statements) > 0
	}
	if question.AllowsFreeform || len(question.Choices) == 0 {
		return true
	}
	for _, choice := range question.Choices {
		if answer.Value == choice {
			return true
		}
	}
	return false
}
