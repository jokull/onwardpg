package graphplan

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

const columnDefaultCurrentDDL = `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.quote (
  id bigint PRIMARY KEY,
  trip_id bigint NOT NULL,
  code text,
  status text DEFAULT 'draft' CONSTRAINT quote_status_check CHECK (status IN ('draft', 'open'))
);
INSERT INTO %[1]s.quote (id, trip_id, code) VALUES (1, 10, 'a'), (2, 10, 'b');
`

// The desired schema drops status's default (contract), widens its CHECK,
// and adds an index and a view chain over the column (all expand). %[2]s is
// the NOT NULL clause for status, empty or NOT NULL.
const columnDefaultDesiredDDL = `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.quote (
  id bigint PRIMARY KEY,
  trip_id bigint NOT NULL,
  code text,
  status text %[2]s CONSTRAINT quote_status_check CHECK (status IN ('draft', 'open', 'accepted'))
);
CREATE INDEX quote_trip_status_idx ON %[1]s.quote (trip_id, status);
CREATE VIEW %[1]s.open_quote AS SELECT id, trip_id, status FROM %[1]s.quote WHERE status = 'open';
CREATE VIEW %[1]s.open_quote_count AS SELECT trip_id, count(*) AS quotes FROM %[1]s.open_quote GROUP BY trip_id;
`

// Real case: quote.status DROP DEFAULT pulled the CHECK change, the index,
// and every view over status into contract, so the window between expand and
// contract lacked them. A default change is invisible to its dependents.
func TestDroppedColumnDefaultKeepsDependentsInExpandOnPostgreSQL(t *testing.T) {
	for _, test := range []struct {
		name    string
		notNull string
	}{
		{name: "default only"},
		// SET NOT NULL is contract too, but a CHECK, an index, or a view does
		// not need the column NOT NULL.
		{name: "default and not null", notNull: "NOT NULL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			desiredDDL := strings.ReplaceAll(columnDefaultDesiredDDL, "%[2]s", test.notNull)
			ctx, conn, url, _, current, desired := columnRenameCatalogs(t, "onwardpg_default", columnDefaultCurrentDDL, desiredDDL)
			plan := planColumnDefaultDependents(t, current, desired)
			for _, statement := range plan.Statements {
				switch {
				case strings.Contains(statement.SQL, `ALTER COLUMN "status" DROP DEFAULT`), strings.Contains(statement.SQL, `ALTER COLUMN "status" SET NOT NULL`):
					if statement.Phase != protocol.PhaseContract {
						t.Fatalf("%q phase = %s, want contract", statement.SQL, statement.Phase)
					}
				case containsString(statement.Hazards, "dependency_phase_promotion"):
					t.Fatalf("dependent promoted to contract: %q\n%s", statement.SQL, joinSQL(plan))
				}
			}
			sql := joinSQL(plan)
			for _, dependent := range []string{"quote_status_check", "quote_trip_status_idx", "open_quote", "open_quote_count"} {
				if !expandStatementNames(plan, dependent) {
					t.Fatalf("no expand statement creates %s:\n%s", dependent, sql)
				}
			}
			assertColumnDefaultPlanConverges(t, ctx, conn, url, desired, plan)
		})
	}
}

// A primary key needs its columns NOT NULL. A NOT NULL change is no longer a
// contract provider, so this pins the rule's premise: a new primary key on an
// existing table renders in contract on its own and follows SET NOT NULL,
// while an unrelated index over the column stays in expand.
func TestColumnNotNullStillPromotesNewPrimaryKeyOnPostgreSQL(t *testing.T) {
	currentDDL := `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.ticket (code text DEFAULT 'x', note text);
INSERT INTO %[1]s.ticket (code, note) VALUES ('a', 'one'), ('b', 'two');
`
	desiredDDL := `
CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.ticket (code text NOT NULL CONSTRAINT ticket_pkey PRIMARY KEY, note text);
CREATE INDEX ticket_note_idx ON %[1]s.ticket (note, code);
`
	ctx, conn, url, _, current, desired := columnRenameCatalogs(t, "onwardpg_default_pk", currentDDL, desiredDDL)
	plan := planColumnDefaultDependents(t, current, desired)
	setNotNull, primaryKey := -1, -1
	for index, statement := range plan.Statements {
		if strings.Contains(statement.SQL, `ALTER COLUMN "code" SET NOT NULL`) {
			setNotNull = index
		}
		if strings.Contains(statement.SQL, "ADD CONSTRAINT \"ticket_pkey\" PRIMARY KEY") {
			primaryKey = index
			if statement.Phase != protocol.PhaseContract {
				t.Fatalf("primary key phase = %s, want contract:\n%s", statement.Phase, joinSQL(plan))
			}
		}
		if strings.Contains(statement.SQL, "ticket_note_idx") && statement.Phase != protocol.PhaseExpand {
			t.Fatalf("ticket_note_idx phase = %s, want expand:\n%s", statement.Phase, joinSQL(plan))
		}
	}
	if setNotNull < 0 || primaryKey < setNotNull {
		t.Fatalf("primary key must follow SET NOT NULL:\n%s", joinSQL(plan))
	}
	assertColumnDefaultPlanConverges(t, ctx, conn, url, desired, plan)
}

// planColumnDefaultDependents answers NOT NULL with the direct strategy and
// each contract reconciliation with assert_only.
func planColumnDefaultDependents(t *testing.T, current, desired *pgschema.Snapshot) protocol.Result {
	t.Helper()
	pending, err := buildIntegration(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		value := ""
		switch question.Kind {
		case "set_not_null":
			value = "direct"
		case "reconcile_contract":
			value = "assert_only"
		default:
			t.Fatalf("unexpected question %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
	}
	plan, err := buildIntegration(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("plan status = %s: %#v", plan.Status, plan)
	}
	return plan
}

func expandStatementNames(plan protocol.Result, name string) bool {
	for _, statement := range plan.Statements {
		if statement.Phase == protocol.PhaseExpand && strings.Contains(statement.SQL, `"`+name+`"`) && !strings.Contains(statement.SQL, "DROP CONSTRAINT") {
			return true
		}
	}
	return false
}

func assertColumnDefaultPlanConverges(t *testing.T, ctx context.Context, conn *pgx.Conn, url string, desired *pgschema.Snapshot, plan protocol.Result) {
	t.Helper()
	applyPlan(t, ctx, conn, plan)
	actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported := actual.Unsupported(); len(unsupported) != 0 {
		t.Fatalf("applied catalog has blockers %v", unsupported)
	}
	got, err := actual.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	want, err := desired.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("applied catalog fingerprint %s, want %s", got, want)
	}
	residual, err := Build(actual, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(residual.Statements) != 0 || len(residual.Questions) != 0 {
		t.Fatalf("residual plan after apply: %#v", residual)
	}
}
