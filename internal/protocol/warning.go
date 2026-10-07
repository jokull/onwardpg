package protocol

import "fmt"

// Warning is a non-blocking observation. A command appends its warnings to
// the result document that it prints, as the top-level `warnings` member. A
// warning never changes the status or the exit code, and it is never written
// to a bundle or included in a digest.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Paths holds at most MaxWarningPaths paths; PathCount is the number that
	// was observed.
	Paths       []string `json:"paths,omitempty"`
	PathCount   int      `json:"path_count,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
}

const (
	ExportSideEffectsCode = "export_side_effects"
	MaxWarningPaths       = 50
)

// ExportSideEffects reports the paths of the git work tree whose status
// changed between the first and the last run of schema_command. paths must
// be sorted.
func ExportSideEffects(paths []string) Warning {
	listed := paths
	if len(listed) > MaxWarningPaths {
		listed = listed[:MaxWarningPaths]
	}
	return Warning{
		Code:        ExportSideEffectsCode,
		Message:     fmt.Sprintf("the git status of %d path(s) changed while schema_command ran; the command or another process wrote to the work tree. The result rests on the export output and does not depend on these files", len(paths)),
		Paths:       append([]string(nil), listed...),
		PathCount:   len(paths),
		Remediation: "make schema_command write only to standard output; if another process changed the paths, no action is needed",
	}
}
