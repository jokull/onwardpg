package protocol

import "strings"

// RenderSQL renders a reviewable, forward-only migration file. JSON remains
// the stable CLI protocol; this format deliberately carries the phase and
// execution-boundary context that an agent or developer needs when reviewing
// SQL outside the JSON document. Indent is applied to every non-empty line.
func RenderSQL(result Result, indent string) string {
	batches := result.Batches
	if len(batches) == 0 {
		batches = batchesForRender(result.Statements)
	}
	if len(batches) == 0 {
		return ""
	}

	lines := []string{
		"-- onwardpg: forward-only PostgreSQL migration plan.",
		"-- Review every batch, safety classification, and hazard in the JSON plan before execution.",
	}
	lastPhase := ""
	for _, batch := range batches {
		if batch.Phase != lastPhase {
			if lastPhase != "" {
				lines = append(lines, "")
			}
			lines = append(lines, phaseComment(batch.Phase)...)
			lastPhase = batch.Phase
		}
		lines = append(lines, batchComment(batch))
		for _, statement := range batch.Statements {
			for _, line := range strings.Split(statement.SQL, "\n") {
				lines = append(lines, line)
			}
		}
	}
	return indentLines(lines, indent)
}

func batchesForRender(statements []Statement) []Batch {
	if len(statements) == 0 {
		return nil
	}
	batches := make([]Batch, 0, len(statements))
	for _, statement := range statements {
		phase := statement.Phase
		if phase == "" {
			phase = "migration"
		}
		transactional := !statement.NonTransactional
		if len(batches) > 0 {
			last := &batches[len(batches)-1]
			if last.Phase == phase && last.Transactional == transactional {
				last.Statements = append(last.Statements, statement)
				continue
			}
		}
		batches = append(batches, Batch{
			ID:            "ad-hoc",
			Phase:         phase,
			Transactional: transactional,
			Statements:    []Statement{statement},
		})
	}
	return batches
}

func phaseComment(phase string) []string {
	switch phase {
	case "expand":
		return []string{
			"-- ============================================================================",
			"-- EXPAND — run before deploying application code that relies on the new shape.",
			"-- Keep this compatible with the application version currently in production.",
			"-- ============================================================================",
		}
	case "migrate":
		return []string{
			"-- ============================================================================",
			"-- MIGRATE — run after compatible code is deployed; review data and lock hazards.",
			"-- Add any application-specific backfill here or run it separately and observe it.",
			"-- onwardpg never invents a cast or data transform that schema state cannot prove.",
			"-- ============================================================================",
		}
	case "contract":
		return []string{
			"-- ============================================================================",
			"-- CONTRACT — run only after old application code no longer uses the prior shape.",
			"-- This section can remove compatibility paths or enforce the final contract.",
			"-- ============================================================================",
		}
	case "manual":
		return []string{
			"-- ============================================================================",
			"-- MANUAL — review and execute only with an explicit operator decision.",
			"-- ============================================================================",
		}
	default:
		return []string{
			"-- ============================================================================",
			"-- MIGRATION",
			"-- ============================================================================",
		}
	}
}

func batchComment(batch Batch) string {
	mode := "transactional"
	if !batch.Transactional {
		mode = "non-transactional; execute outside BEGIN/COMMIT"
	}
	if batch.ID == "" {
		return "-- Batch: " + mode + "."
	}
	return "-- Batch " + batch.ID + ": " + mode + "."
}

func indentLines(lines []string, indent string) string {
	for i, line := range lines {
		if line != "" {
			lines[i] = indent + line
		}
	}
	return strings.Join(lines, "\n")
}
