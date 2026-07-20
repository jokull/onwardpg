package protocol

import (
	"fmt"
	"sort"
	"strings"
)

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
		} else {
			lines = append(lines, "")
		}
		lines = append(lines, batchDirective(batch), batchComment(batch))
		for _, statement := range batch.Statements {
			if review := reviewComment(statement); review != "" {
				lines = append(lines, review)
			}
			if statement.StatementTimeoutMS > 0 || statement.LockTimeoutMS > 0 {
				lines = append(lines, timeoutComment(statement))
			}
			if strings.Contains(statement.SQL, "ONWARDPG TODO") {
				id := statement.ID
				if id == "" {
					id = StableStatementID(statement)
				}
				lines = append(lines, "-- onwardpg:edit begin "+id)
				lines = append(lines, strings.Split(statement.SQL, "\n")...)
				lines = append(lines, "-- onwardpg:edit end "+id)
				continue
			}
			lines = append(lines, strings.Split(statement.SQL, "\n")...)
		}
	}
	return indentLines(lines, indent)
}

func reviewComment(statement Statement) string {
	var parts []string
	if statement.Safety != "" {
		parts = append(parts, "safety="+statement.Safety)
	}
	if len(statement.Hazards) > 0 {
		parts = append(parts, "hazards="+strings.Join(statement.Hazards, ","))
	}
	if len(statement.RequiresGates) > 0 {
		gates := append([]string(nil), statement.RequiresGates...)
		sort.Strings(gates)
		parts = append(parts, "requires_gates="+strings.Join(gates, ","))
	}
	if len(parts) == 0 {
		return ""
	}
	return "-- Review: " + strings.Join(parts, "; ") + "."
}

func batchDirective(batch Batch) string {
	mode := "transactional"
	if !batch.Transactional {
		mode = "nontransactional"
	}
	return "-- onwardpg:batch " + mode
}

func timeoutComment(statement Statement) string {
	parts := make([]string, 0, 2)
	if statement.StatementTimeoutMS > 0 {
		parts = append(parts, "statement_timeout="+formatMilliseconds(statement.StatementTimeoutMS))
	}
	if statement.LockTimeoutMS > 0 {
		parts = append(parts, "lock_timeout="+formatMilliseconds(statement.LockTimeoutMS))
	}
	return "-- Suggested session timeouts: " + strings.Join(parts, ", ") + "."
}

func formatMilliseconds(milliseconds int64) string {
	if milliseconds%60000 == 0 {
		return fmt.Sprintf("%dm", milliseconds/60000)
	}
	if milliseconds%1000 == 0 {
		return fmt.Sprintf("%ds", milliseconds/1000)
	}
	return fmt.Sprintf("%dms", milliseconds)
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
			if last.Phase == phase && last.Transactional == transactional && !statement.BatchBoundaryBefore {
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
			"-- EXPAND — run before the one application deployment anchored to this plan.",
			"-- Old code must remain usable while new code begins using the expanded shape.",
			"-- Transactional and non-transactional batches are marked below; this phase is not split by transaction.",
			"-- ============================================================================",
		}
	case "contract":
		return []string{
			"-- ============================================================================",
			"-- CONTRACT — run after pre-deployment instances, workers, pools, and queues have drained.",
			"-- The one newly deployed application version must work before and after every batch below.",
			"-- Catch-up, validation, enforcement, and compatibility cleanup belong here.",
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
