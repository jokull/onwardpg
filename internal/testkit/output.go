package testkit

type PlanEnvelope struct {
	Status      string             `json:"status"`
	Durable     DurableOutcome     `json:"durable"`
	Development DevelopmentOutcome `json:"development"`
	NextActions []NextAction       `json:"next_actions"`
}

type DurableOutcome struct {
	Status          string          `json:"status"`
	Target          string          `json:"target"`
	BundleID        string          `json:"bundle_id"`
	PlanID          string          `json:"plan_id"`
	Generation      int             `json:"generation"`
	Path            string          `json:"path"`
	Findings        []Finding       `json:"findings"`
	WrittenReceipts []string        `json:"written_receipts"`
	Edits           []EditReference `json:"edits"`
}

type DevelopmentOutcome struct {
	Status   string    `json:"status"`
	Findings []Finding `json:"findings"`
}

type Finding struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

type EditReference struct {
	Path     string `json:"path"`
	PocketID string `json:"pocket_id"`
	Phase    string `json:"phase"`
}

type NextAction struct {
	Scope           string         `json:"scope"`
	Kind            string         `json:"kind"`
	Reason          string         `json:"reason"`
	Argv            []string       `json:"argv"`
	SQL             string         `json:"sql"`
	Choices         []ActionChoice `json:"choices"`
	Path            string         `json:"path"`
	PocketID        string         `json:"pocket_id"`
	CurrentSQL      string         `json:"current_sql"`
	NewGeneratedSQL string         `json:"new_generated_sql"`
	Resolution      string         `json:"resolution"`
}

type ActionChoice struct {
	Argv []string `json:"argv"`
}
