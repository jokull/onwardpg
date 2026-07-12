// Package plan converts two canonical schema states into a forward SQL plan or
// a set of explicit questions. It never reads terminal input.
package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/schema"
)

func Build(current, desired schema.State, answers protocol.Answers) protocol.Result {
	unsupported := symmetricDifference(current.Unsupported, desired.Unsupported)
	if len(unsupported) > 0 {
		return protocol.Result{Status: protocol.Unsupported, Unsupported: unsupported}
	}

	var statements []protocol.Statement
	var questions []protocol.Question
	currentTables, desiredTables := current.Tables(), desired.Tables()
	currentOnly, desiredOnly := difference(currentTables, desiredTables), difference(desiredTables, currentTables)
	renameQuestions := make(map[string]bool)

	// A matching table shape is evidence of a possible rename, never proof.
	for oldKey, oldTable := range copyTables(currentOnly) {
		candidates := renameCandidates(oldTable, desiredOnly)
		if len(candidates) != 1 {
			continue
		}
		newKey, newTable := candidates[0], desiredOnly[candidates[0]]
		answer := answers.Lookup("rename_table", oldKey)
		switch answer {
		case newKey:
			statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", qtable(oldTable), quote(newTable.Name)), "review"))
			delete(currentOnly, oldKey)
			delete(desiredOnly, newKey)
		case "create":
			statements = append(statements, statement(fmt.Sprintf("DROP TABLE %s;", qtable(oldTable)), "review"))
			delete(currentOnly, oldKey)
		default:
			renameQuestions[oldKey] = true
			questions = append(questions, question("rename_table", oldKey, fmt.Sprintf("Was %s renamed to %s?", oldKey, newKey), []string{newKey, "create"}))
		}
	}

	for _, key := range schema.SortedKeys(currentOnly) {
		table := currentOnly[key]
		if renameQuestions[key] {
			continue
		}
		if answers.Lookup("drop_table", key) == "drop" {
			statements = append(statements, statement(fmt.Sprintf("DROP TABLE %s;", qtable(table)), "review"))
		} else {
			questions = append(questions, question("drop_table", key, fmt.Sprintf("%s is absent from desired state. Drop it?", key), []string{"drop", "keep"}))
		}
	}
	for _, key := range schema.SortedKeys(desiredOnly) {
		table := desiredOnly[key]
		statements = append(statements, createTable(table))
		for _, constraint := range table.Constraints {
			statements = append(statements, addConstraint(table, constraint, constraint.ForeignKey()))
		}
	}

	for _, key := range sharedKeys(currentTables, desiredTables) {
		oldTable, newTable := currentTables[key], desiredTables[key]
		columnStatements, columnQuestions := diffColumns(oldTable, newTable, answers)
		statements, questions = append(statements, columnStatements...), append(questions, columnQuestions...)
		statements = append(statements, diffConstraints(oldTable, newTable)...)
	}

	if len(questions) > 0 {
		return protocol.Result{Status: protocol.NeedsInput, Statements: statements, Questions: questions}
	}
	return protocol.Result{Status: protocol.Planned, Statements: statements}
}

func diffConstraints(current, desired schema.Table) []protocol.Statement {
	old, next := constraintMap(current.Constraints), constraintMap(desired.Constraints)
	var statements []protocol.Statement
	for _, name := range sharedKeys(old, next) {
		if old[name].Definition != next[name].Definition {
			statements = append(statements, dropConstraint(current, name), addConstraint(desired, next[name], true))
		}
	}
	for _, name := range sortedOnly(old, next) {
		statements = append(statements, dropConstraint(current, name))
	}
	for _, name := range sortedOnly(next, old) {
		statements = append(statements, addConstraint(desired, next[name], next[name].ForeignKey()))
	}
	return statements
}

func addConstraint(table schema.Table, constraint schema.Constraint, review bool) protocol.Statement {
	safety := "safe"
	if review {
		safety = "review"
	}
	return statement(fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s;", qtable(table), quote(constraint.Name), constraint.Definition), safety)
}

func dropConstraint(table schema.Table, name string) protocol.Statement {
	return statement(fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;", qtable(table), quote(name)), "review")
}

func diffColumns(current, desired schema.Table, answers protocol.Answers) ([]protocol.Statement, []protocol.Question) {
	var statements []protocol.Statement
	var questions []protocol.Question
	old, next := columnMap(current.Columns), columnMap(desired.Columns)
	for _, name := range sharedKeys(old, next) {
		before, after := old[name], next[name]
		key := current.Key() + "." + name
		if before.Type != after.Type {
			answer := answers.Lookup("type_change", key)
			switch answer {
			case "direct":
				statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s;", qtable(current), quote(name), after.Type), "review"))
			case "":
				questions = append(questions, question("type_change", key, fmt.Sprintf("%s changes from %s to %s. Supply a USING expression or direct.", key, before.Type, after.Type), []string{"direct"}))
			default:
				statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s;", qtable(current), quote(name), after.Type, answer), "review"))
			}
		}
		if before.Default != after.Default {
			if after.Default == nil {
				statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;", qtable(current), quote(name)), "safe"))
			} else {
				statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;", qtable(current), quote(name), *after.Default), "safe"))
			}
		}
		if before.NotNull != after.NotNull {
			verb, safety := "DROP NOT NULL", "safe"
			if after.NotNull {
				verb, safety = "SET NOT NULL", "review"
			}
			statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s %s;", qtable(current), quote(name), verb), safety))
		}
	}
	for _, name := range sortedOnly(old, next) {
		key := current.Key() + "." + name
		if answers.Lookup("drop_column", key) == "drop" {
			statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", qtable(current), quote(name)), "review"))
		} else {
			questions = append(questions, question("drop_column", key, fmt.Sprintf("%s is absent from desired state. Drop it?", key), []string{"drop", "keep"}))
		}
	}
	for _, name := range sortedOnly(next, old) {
		statements = append(statements, statement(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", qtable(current), columnSQL(next[name])), "safe"))
	}
	return statements, questions
}

func createTable(table schema.Table) protocol.Statement {
	columns := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		columns = append(columns, columnSQL(column))
	}
	return statement(fmt.Sprintf("CREATE TABLE %s (%s);", qtable(table), strings.Join(columns, ", ")), "safe")
}

func columnSQL(column schema.Column) string {
	parts := []string{quote(column.Name), column.Type}
	if column.Identity != "" {
		if column.Identity == "a" {
			parts = append(parts, "GENERATED ALWAYS AS IDENTITY")
		} else {
			parts = append(parts, "GENERATED BY DEFAULT AS IDENTITY")
		}
	}
	if column.Default != nil {
		parts = append(parts, "DEFAULT "+*column.Default)
	}
	if column.NotNull {
		parts = append(parts, "NOT NULL")
	}
	return strings.Join(parts, " ")
}

func renameCandidates(old schema.Table, candidates map[string]schema.Table) []string {
	var keys []string
	for key, candidate := range candidates {
		if old.Schema == candidate.Schema && sameShape(old, candidate) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func sameShape(a, b schema.Table) bool {
	if len(a.Columns) == 0 || len(a.Columns) != len(b.Columns) {
		return false
	}
	aShape, bShape := make([]string, 0, len(a.Columns)), make([]string, 0, len(b.Columns))
	for _, column := range a.Columns {
		aShape = append(aShape, fmt.Sprintf("%s/%t/%v", column.Type, column.NotNull, column.Default))
	}
	for _, column := range b.Columns {
		bShape = append(bShape, fmt.Sprintf("%s/%t/%v", column.Type, column.NotNull, column.Default))
	}
	sort.Strings(aShape)
	sort.Strings(bShape)
	return strings.Join(aShape, "|") == strings.Join(bShape, "|")
}

func quote(identifier string) string   { return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"` }
func qtable(table schema.Table) string { return quote(table.Schema) + "." + quote(table.Name) }
func statement(sql, safety string) protocol.Statement {
	phase := "expand"
	if safety == "review" {
		phase = "contract"
	}
	return protocol.Statement{SQL: sql, Safety: safety, Phase: phase}
}
func question(kind, key, message string, choices []string) protocol.Question {
	return protocol.Question{ID: kind + ":" + key, Kind: kind, Key: key, Message: message, Choices: choices}
}

func difference[T any](left, right map[string]T) map[string]T {
	out := map[string]T{}
	for key, value := range left {
		if _, ok := right[key]; !ok {
			out[key] = value
		}
	}
	return out
}
func copyTables(in map[string]schema.Table) map[string]schema.Table {
	out := map[string]schema.Table{}
	for key, value := range in {
		out[key] = value
	}
	return out
}
func columnMap(columns []schema.Column) map[string]schema.Column {
	out := map[string]schema.Column{}
	for _, column := range columns {
		out[column.Name] = column
	}
	return out
}
func constraintMap(constraints []schema.Constraint) map[string]schema.Constraint {
	out := map[string]schema.Constraint{}
	for _, constraint := range constraints {
		out[constraint.Name] = constraint
	}
	return out
}
func sharedKeys[T any](left, right map[string]T) []string {
	var keys []string
	for key := range left {
		if _, ok := right[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
func sortedOnly[T any](left, right map[string]T) []string {
	return schema.SortedKeys(difference(left, right))
}
func symmetricDifference(left, right []string) []string {
	m := map[string]int{}
	for _, item := range left {
		m[item]++
	}
	for _, item := range right {
		m[item]++
	}
	var out []string
	for item, count := range m {
		if count == 1 {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
