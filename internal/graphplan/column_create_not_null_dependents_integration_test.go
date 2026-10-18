package graphplan

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// Real case: a new quote.company NOT NULL is added nullable in expand and set
// NOT NULL in contract. That contract work made the column a contract
// provider, so its UNIQUE constraints, and a composite foreign key from a new
// table, moved to contract without a gate and the plan was unsupported with
// contract_enforcement_missing_gate. None of them needs the column NOT NULL.
func TestNewNotNullColumnKeepsDependentsInExpandOnPostgreSQL(t *testing.T) {
	currentDDL := `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.quote (id bigint PRIMARY KEY, currency text NOT NULL);
INSERT INTO %[1]s.quote (id, currency) VALUES (1, 'ISK'), (2, 'USD');
`
	desiredDDL := `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.quote (
  id bigint PRIMARY KEY,
  currency text NOT NULL,
  company text NOT NULL,
  CONSTRAINT quote_id_company_currency_unique UNIQUE (id, company, currency),
  CONSTRAINT quote_id_company_unique UNIQUE (id, company)
);
CREATE INDEX quote_company_idx ON %[1]s.quote (company);
CREATE VIEW %[1]s.quote_company AS SELECT id, company FROM %[1]s.quote;
CREATE TABLE %[1]s.payment_due (
  id bigint PRIMARY KEY,
  quote_id bigint NOT NULL,
  company text NOT NULL,
  currency text NOT NULL,
  CONSTRAINT payment_due_quote_fkey FOREIGN KEY (quote_id, company, currency) REFERENCES %[1]s.quote (id, company, currency)
);
`
	ctx, conn, url, schema, current, desired := columnRenameCatalogs(t, "onwardpg_new_not_null", currentDDL, desiredDDL)
	plan := planColumnDefaultDependents(t, current, desired)
	setNotNull := false
	for _, statement := range plan.Statements {
		if strings.Contains(statement.SQL, `ALTER COLUMN "company" SET NOT NULL`) {
			setNotNull = true
			if statement.Phase != protocol.PhaseContract {
				t.Fatalf("SET NOT NULL phase = %s, want contract:\n%s", statement.Phase, joinSQL(plan))
			}
		}
		if containsString(statement.Hazards, "dependency_phase_promotion") {
			t.Fatalf("dependent promoted to contract: %q\n%s", statement.SQL, joinSQL(plan))
		}
	}
	if !setNotNull {
		t.Fatalf("no SET NOT NULL for the new column:\n%s", joinSQL(plan))
	}
	for _, dependent := range []string{"company", "quote_id_company_currency_unique", "quote_id_company_unique", "quote_company_idx", "quote_company", "payment_due", "payment_due_quote_fkey"} {
		if !expandStatementNames(plan, dependent) {
			t.Fatalf("no expand statement creates %s:\n%s", dependent, joinSQL(plan))
		}
	}
	// The window between expand and contract holds rows whose company is
	// NULL; the backfill runs there, before SET NOT NULL.
	applyPhasesWithBackfill(t, ctx, conn, plan, "UPDATE "+schema+".quote SET company = 'ttj'")
	assertAppliedCatalogConverges(t, ctx, url, desired)
}

// A primary key needs its columns NOT NULL. A new NOT NULL column is no longer
// a contract provider, so this pins the rule's premise for a new column: a new
// primary key over it renders in contract on its own and follows SET NOT NULL,
// while an index over the column stays in expand.
func TestNewNotNullColumnStillPrecedesNewPrimaryKeyOnPostgreSQL(t *testing.T) {
	currentDDL := `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.ticket (note text);
INSERT INTO %[1]s.ticket (note) VALUES ('one'), ('two');
`
	desiredDDL := `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.ticket (note text, code text NOT NULL CONSTRAINT ticket_pkey PRIMARY KEY);
CREATE INDEX ticket_note_code_idx ON %[1]s.ticket (note, code);
`
	ctx, conn, url, schema, current, desired := columnRenameCatalogs(t, "onwardpg_new_not_null_pk", currentDDL, desiredDDL)
	plan := planColumnDefaultDependents(t, current, desired)
	setNotNull, primaryKey := -1, -1
	for index, statement := range plan.Statements {
		if strings.Contains(statement.SQL, `ALTER COLUMN "code" SET NOT NULL`) {
			setNotNull = index
		}
		if strings.Contains(statement.SQL, `ADD CONSTRAINT "ticket_pkey" PRIMARY KEY`) {
			primaryKey = index
			if statement.Phase != protocol.PhaseContract {
				t.Fatalf("primary key phase = %s, want contract:\n%s", statement.Phase, joinSQL(plan))
			}
		}
	}
	if setNotNull < 0 || primaryKey < setNotNull {
		t.Fatalf("primary key must follow SET NOT NULL:\n%s", joinSQL(plan))
	}
	if !expandStatementNames(plan, "ticket_note_code_idx") {
		t.Fatalf("ticket_note_code_idx is not in expand:\n%s", joinSQL(plan))
	}
	applyPhasesWithBackfill(t, ctx, conn, plan, "UPDATE "+schema+".ticket SET code = note")
	assertAppliedCatalogConverges(t, ctx, url, desired)
}

// applyPhasesWithBackfill applies the expand batches, runs backfill, and then
// applies the contract batches.
func applyPhasesWithBackfill(t *testing.T, ctx context.Context, conn *pgx.Conn, plan protocol.Result, backfill string) {
	t.Helper()
	phase := func(name string) protocol.Result {
		selected := plan
		selected.Batches = nil
		for _, batch := range plan.Batches {
			if batch.Phase == name {
				selected.Batches = append(selected.Batches, batch)
			}
		}
		return selected
	}
	applyPlan(t, ctx, conn, phase(protocol.PhaseExpand))
	if _, err := conn.Exec(ctx, backfill); err != nil {
		t.Fatalf("%s: %v", backfill, err)
	}
	applyPlan(t, ctx, conn, phase(protocol.PhaseContract))
	for _, batch := range plan.Batches {
		if batch.Phase != protocol.PhaseExpand && batch.Phase != protocol.PhaseContract {
			t.Fatalf("unexpected batch phase %q", batch.Phase)
		}
	}
}

func assertAppliedCatalogConverges(t *testing.T, ctx context.Context, url string, desired *pgschema.Snapshot) {
	t.Helper()
	assertColumnDefaultPlanConverges(t, ctx, nil, url, desired, protocol.Result{})
}
