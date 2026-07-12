// Package schema contains the canonical PostgreSQL state model.
//
// Every input adapter (a live instance, SQL DDL, or a future ORM loader) must
// produce this model. The planner therefore never needs to know how a desired
// state was authored.
package schema

import "sort"

type State struct {
	Schemas     map[string]Schema    `json:"schemas"`
	Extensions  map[string]Extension `json:"extensions"`
	Unsupported []string             `json:"unsupported,omitempty"`
}

type Schema struct {
	Name   string           `json:"name"`
	Tables map[string]Table `json:"tables"`
}

type Table struct {
	Schema      string       `json:"schema"`
	Name        string       `json:"name"`
	Columns     []Column     `json:"columns"`
	Constraints []Constraint `json:"constraints,omitempty"`
	Indexes     []Index      `json:"indexes,omitempty"`
	Comment     *string      `json:"comment,omitempty"`
}

func (t Table) Key() string { return t.Schema + "." + t.Name }

type Column struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	NotNull  bool    `json:"not_null"`
	Default  *string `json:"default,omitempty"`
	Identity string  `json:"identity,omitempty"`
	Comment  *string `json:"comment,omitempty"`
}

type Constraint struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Kind       string `json:"kind"`
}

func (c Constraint) ForeignKey() bool { return c.Kind == "f" }

type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Canonical  string `json:"canonical"`
}

type Extension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
}

func NewState() State {
	return State{Schemas: make(map[string]Schema), Extensions: make(map[string]Extension)}
}

func (s State) Tables() map[string]Table {
	tables := make(map[string]Table)
	for _, namespace := range s.Schemas {
		for _, table := range namespace.Tables {
			tables[table.Key()] = table
		}
	}
	return tables
}

func SortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
