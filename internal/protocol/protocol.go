// Package protocol is the stable JSON contract between onwardpg and an agent.
package protocol

type Status string

const (
	Planned     Status = "planned"
	NeedsInput  Status = "needs_input"
	Unsupported Status = "unsupported"
)

type Result struct {
	Status      Status      `json:"status"`
	Statements  []Statement `json:"statements,omitempty"`
	Questions   []Question  `json:"questions,omitempty"`
	Ignored     []string    `json:"ignored,omitempty"`
	Unsupported []string    `json:"unsupported,omitempty"`
}

type Statement struct {
	SQL    string `json:"sql"`
	Safety string `json:"safety"`
	// Phase allows a caller to split a forward migration across compatible
	// application releases. "expand" is additive/compatible work; "contract"
	// needs an explicit review after old application code is gone.
	Phase string `json:"phase"`
}

type Question struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Key     string   `json:"key"`
	Choices []string `json:"choices"`
}

type Answers struct {
	Answers []Answer `json:"answers"`
}

type Answer struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (a Answers) Lookup(kind, key string) string {
	for _, answer := range a.Answers {
		if answer.Kind == kind && answer.Key == key {
			return answer.Value
		}
	}
	return ""
}
