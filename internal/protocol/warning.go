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
	ExportSideEffectsCode              = "export_side_effects"
	IndexLockModeChangedCode           = "index_lock_mode_changed"
	IndexLockModeDiffersFromConfigCode = "index_lock_mode_differs_from_config"
	MaxWarningPaths                    = 50
)

// IndexLockMode reports how a bundle builds and drops standalone indexes, and
// which input decided it. Source is "flag" (--concurrent-indexes on this
// run), "bundle" (the choice stored in the manifest of the previous
// generation), "config" (the target's concurrent_indexes), or "default".
type IndexLockMode struct {
	ConcurrentIndexes bool   `json:"concurrent_indexes"`
	Source            string `json:"source"`
	// PreviousConcurrentIndexes is present only when this run wrote the bundle
	// with a mode that differs from its previous generation.
	PreviousConcurrentIndexes *bool `json:"previous_concurrent_indexes,omitempty"`
	// ConfigConcurrentIndexes is present, and true, only when the bundle keeps
	// the blocking mode of its previous generation although the target
	// configuration has concurrent_indexes = true.
	ConfigConcurrentIndexes *bool `json:"config_concurrent_indexes,omitempty"`
}

func indexLockModeName(concurrent bool) string {
	if concurrent {
		return "concurrent (CREATE INDEX CONCURRENTLY and DROP INDEX CONCURRENTLY in nontransactional batches)"
	}
	return "blocking (plain CREATE INDEX and DROP INDEX in transactional batches)"
}

// IndexLockModeChanged reports that this run wrote a bundle whose index lock
// mode differs from the mode of its previous generation.
func IndexLockModeChanged(bundleID string, previous, current bool, source string) Warning {
	remediation := "if the change is not intended, rerun the plan with --concurrent-indexes"
	if !previous {
		remediation += "=false"
	}
	return Warning{
		Code: IndexLockModeChangedCode,
		Message: fmt.Sprintf("bundle %s changed its index lock mode from %s to %s; the new mode comes from the %s. The locks that this migration takes on a live database are different now",
			bundleID, indexLockModeName(previous), indexLockModeName(current), indexLockModeSource(source)),
		Remediation: remediation,
	}
}

// IndexLockModeDiffersFromConfig reports that a bundle keeps the blocking
// mode of its previous generation although the target configuration has
// concurrent_indexes = true. A bundle that was planned before the key was set
// is the usual cause.
func IndexLockModeDiffersFromConfig(bundleID string) Warning {
	return Warning{
		Code: IndexLockModeDiffersFromConfigCode,
		Message: fmt.Sprintf("bundle %s keeps the index lock mode of its previous generation, %s; the target configuration has concurrent_indexes = true. The mode of an existing bundle has precedence over the configuration",
			bundleID, indexLockModeName(false)),
		Remediation: "to move this bundle to the configured mode, rerun the plan once with --concurrent-indexes; the bundle then stores that choice",
	}
}

func indexLockModeSource(source string) string {
	switch source {
	case "flag":
		return "--concurrent-indexes flag of this run"
	case "bundle":
		return "choice stored in the bundle"
	case "config":
		return "concurrent_indexes key of the target"
	default:
		return "default"
	}
}

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
