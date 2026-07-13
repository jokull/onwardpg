package semantichint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// Resolution is the complete evidence produced while consuming semantic hints
// through the planner's dependency-ordered question stages.
type Resolution struct {
	Result    protocol.Result
	Answers   protocol.Answers
	Questions []protocol.Question
	Hints     []protocol.Hint
}

// Resolve plans from snapshots using only semantic hints. It is shared by
// high-level CLI surfaces which do not have a stored bundle decision history.
func Resolve(current, desired *pgschema.Snapshot, hints []protocol.Hint, options graphplan.Options) (Resolution, error) {
	if err := protocol.ValidateHints(hints); err != nil {
		return Resolution{}, err
	}
	result, err := graphplan.Build(current, desired, protocol.Answers{}, options)
	if err != nil {
		return Resolution{}, err
	}
	resolution := Resolution{Result: result, Answers: protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: result.CurrentFingerprint, DesiredFingerprint: result.DesiredFingerprint,
	}}
	used := make(map[int]bool, len(hints))
	for iteration := 0; iteration <= len(hints)*2+1; iteration++ {
		resolution.Questions = mergeQuestions(resolution.Questions, resolution.Result.Questions)
		if resolution.Result.Status != protocol.NeedsInput {
			if err := rejectUnused(hints, used); err != nil {
				return Resolution{}, err
			}
			resolution.Hints = usedHints(hints, used)
			return resolution, nil
		}
		matched, err := MatchQuestions(resolution.Result.Questions, hints, current, desired)
		if err != nil {
			return Resolution{}, err
		}
		if len(matched.Answers) == 0 {
			if err := rejectUnused(hints, used); err != nil {
				return Resolution{}, err
			}
			resolution.Hints = usedHints(hints, used)
			return resolution, nil
		}
		for index := range matched.Used {
			used[index] = true
		}
		resolution.Answers.Answers = append(resolution.Answers.Answers, matched.Answers...)
		resolution.Result, err = graphplan.Build(current, desired, resolution.Answers, options)
		if err != nil {
			return Resolution{}, err
		}
	}
	return Resolution{}, fmt.Errorf("semantic hint planning did not converge")
}

func rejectUnused(hints []protocol.Hint, used map[int]bool) error {
	var unused []string
	for index, hint := range hints {
		if used[index] {
			continue
		}
		key, _ := hint.CanonicalKey()
		unused = append(unused, key)
	}
	if len(unused) == 0 {
		return nil
	}
	sort.Strings(unused)
	return fmt.Errorf("unused semantic hints: %s", strings.Join(unused, ", "))
}

func usedHints(hints []protocol.Hint, used map[int]bool) []protocol.Hint {
	result := make([]protocol.Hint, 0, len(used))
	for index, hint := range hints {
		if used[index] {
			result = append(result, hint)
		}
	}
	result, _ = protocol.CanonicalHints(result)
	return result
}

func mergeQuestions(first, second []protocol.Question) []protocol.Question {
	result := append([]protocol.Question(nil), first...)
	seen := make(map[string]bool, len(result))
	for _, question := range result {
		seen[question.Kind+":"+question.Key] = true
	}
	for _, question := range second {
		id := question.Kind + ":" + question.Key
		if !seen[id] {
			result = append(result, question)
			seen[id] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+":"+result[i].Key < result[j].Kind+":"+result[j].Key
	})
	return result
}
