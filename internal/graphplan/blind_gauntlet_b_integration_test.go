package graphplan

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// TestBlindGauntletCrossNameTypeTransitionKeepsLegacySQLAliveOnPostgreSQL is
// the durable receipt for blind field run B. The reviewed SQL is deliberately
// specific to this product transition: onwardpg owns the dependency boundary,
// while the developer owns the meaning of invalid legacy text.
func TestBlindGauntletCrossNameTypeTransitionKeepsLegacySQLAliveOnPostgreSQL(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "onwardpg_blind_b")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	const baselineDDL = `CREATE SCHEMA app;
CREATE TABLE app.people (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  label text NOT NULL,
  age_text text
);
CREATE VIEW app.people_age AS
SELECT p.id, p.age_text FROM app.people AS p;
CREATE MATERIALIZED VIEW app.people_age_summary AS
SELECT p.age_text, count(*) AS people_count
FROM app.people_age AS p
GROUP BY p.age_text;
CREATE UNIQUE INDEX people_age_summary_age_text_idx
ON app.people_age_summary (age_text);`
	const desiredDDL = `CREATE SCHEMA app;
CREATE TABLE app.people (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  label text NOT NULL,
  age integer
);
CREATE VIEW app.people_age AS
SELECT p.id, p.age FROM app.people AS p;
CREATE MATERIALIZED VIEW app.people_age_summary AS
SELECT p.age, count(*) AS people_count
FROM app.people_age AS p
GROUP BY p.age;
CREATE UNIQUE INDEX people_age_summary_age_idx
ON app.people_age_summary (age);`

	setup, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, baselineDDL+`
INSERT INTO app.people (label, age_text) VALUES
  ('null', NULL),
  ('blank', ''),
  ('unknown', 'unknown'),
  ('malformed', 'twelve'),
  ('valid', '42'),
  ('positive-sign-and-space', ' +7 '),
  ('overflow-high', '2147483648'),
  ('overflow-low', '-2147483649');
REFRESH MATERIALIZED VIEW app.people_age_summary;`); err != nil {
		setup.Close(ctx)
		t.Fatal(err)
	}
	if err := setup.Close(ctx); err != nil {
		t.Fatal(err)
	}

	current, err := source.LoadDatabaseGraphForComparison(ctx, database.Config, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, []byte(desiredDDL), "blind gauntlet B desired schema", adminURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := buildBlindGauntletBPlan(t, current, desired)
	transitionID := (pgschema.Column{Table: (pgschema.Table{Schema: "app", Name: "people"}).ObjectID(), Name: "age_text"}).ObjectID().String() +
		"->" + (pgschema.Column{Table: (pgschema.Table{Schema: "app", Name: "people"}).ObjectID(), Name: "age"}).ObjectID().String()
	plan = editBlindGauntletBPockets(t, plan, transitionID, blindGauntletBExpandSQL, blindGauntletBContractSQL)

	legacy, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacyClosed := false
	defer func() {
		if !legacyClosed {
			_ = legacy.Close(context.Background())
		}
	}()
	for name, sql := range map[string]string{
		"legacy_insert":  `INSERT INTO app.people (label, age_text) VALUES ($1, $2) RETURNING id, label, age_text`,
		"legacy_select":  `SELECT id, label, age_text FROM app.people WHERE id = $1`,
		"legacy_update":  `UPDATE app.people SET age_text = $2 WHERE id = $1 RETURNING id, age_text`,
		"legacy_view":    `SELECT id, age_text FROM app.people_age WHERE id = $1`,
		"legacy_summary": `SELECT age_text, people_count FROM app.people_age_summary WHERE age_text = $1`,
	} {
		if _, err := legacy.Prepare(ctx, name, sql); err != nil {
			t.Fatalf("prepare %s before expand: %v", name, err)
		}
	}
	var legacyPID int32
	if err := legacy.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&legacyPID); err != nil {
		t.Fatal(err)
	}

	deploy, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deploy.Close(context.Background())
	var deployPID int32
	if err := deploy.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&deployPID); err != nil {
		t.Fatal(err)
	}
	if deployPID == legacyPID {
		t.Fatal("expand must run on a connection distinct from the prepared legacy session")
	}
	applyBlindGauntletBPhase(t, ctx, deploy, plan, protocol.PhaseExpand)

	assertBlindGauntletBInitialConversion(t, ctx, deploy)
	var legacyID int64
	var label string
	var legacyAge *string
	if err := legacy.QueryRow(ctx, "legacy_insert", "legacy-after-expand", " 36 ").Scan(&legacyID, &label, &legacyAge); err != nil {
		t.Fatalf("prepared legacy INSERT failed after expand: %v", err)
	}
	if label != "legacy-after-expand" || legacyAge == nil || *legacyAge != " 36 " {
		t.Fatalf("prepared legacy INSERT returned label=%q age_text=%v", label, legacyAge)
	}
	var selectedID int64
	if err := legacy.QueryRow(ctx, "legacy_select", legacyID).Scan(&selectedID, &label, &legacyAge); err != nil {
		t.Fatalf("prepared legacy SELECT failed after expand: %v", err)
	}
	if selectedID != legacyID {
		t.Fatalf("prepared legacy SELECT returned id=%d, want %d", selectedID, legacyID)
	}
	if err := legacy.QueryRow(ctx, "legacy_view", legacyID).Scan(&selectedID, &legacyAge); err != nil {
		t.Fatalf("prepared legacy view query failed after overlap view replacement: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_update", legacyID, "37").Scan(&selectedID, &legacyAge); err != nil {
		t.Fatalf("prepared legacy UPDATE failed after expand: %v", err)
	}
	if legacyAge == nil || *legacyAge != "37" {
		t.Fatalf("prepared legacy UPDATE returned age_text=%v", legacyAge)
	}
	var synchronizedAge *int32
	if err := deploy.QueryRow(ctx, `SELECT age FROM app.people WHERE id = $1`, legacyID).Scan(&synchronizedAge); err != nil {
		t.Fatal(err)
	}
	if synchronizedAge == nil || *synchronizedAge != 37 {
		t.Fatalf("legacy-to-new synchronization produced age=%v", synchronizedAge)
	}

	var newID int64
	if err := deploy.QueryRow(ctx, `INSERT INTO app.people (label, age) VALUES ('new-after-expand', 52) RETURNING id`).Scan(&newID); err != nil {
		t.Fatalf("new INSERT failed against expanded schema: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_select", newID).Scan(&selectedID, &label, &legacyAge); err != nil {
		t.Fatalf("prepared legacy SELECT could not read a new writer's row: %v", err)
	}
	if legacyAge == nil || *legacyAge != "52" {
		t.Fatalf("new-to-legacy synchronization produced age_text=%v", legacyAge)
	}
	if _, err := deploy.Exec(ctx, `UPDATE app.people SET age = 53 WHERE id = $1`, newID); err != nil {
		t.Fatalf("new UPDATE failed against expanded schema: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_view", newID).Scan(&selectedID, &legacyAge); err != nil {
		t.Fatalf("prepared legacy view query could not see new UPDATE: %v", err)
	}
	if legacyAge == nil || *legacyAge != "53" {
		t.Fatalf("new UPDATE did not reverse-convert for the legacy view: %v", legacyAge)
	}
	var summaryAgeText string
	var summaryCount int64
	if err := legacy.QueryRow(ctx, "legacy_summary", "42").Scan(&summaryAgeText, &summaryCount); err != nil {
		t.Fatalf("prepared legacy materialized-view query failed after expand: %v", err)
	}
	if summaryAgeText != "42" || summaryCount != 1 {
		t.Fatalf("legacy materialized-view row = (%q, %d), want (42, 1)", summaryAgeText, summaryCount)
	}
	var summaryAge int32
	if err := deploy.QueryRow(ctx, `SELECT age, people_count FROM app.people_age_summary WHERE age = 42`).Scan(&summaryAge, &summaryCount); err != nil {
		t.Fatalf("new materialized-view query failed after expand: %v", err)
	}
	if summaryAge != 42 || summaryCount != 1 {
		t.Fatalf("new materialized-view row = (%d, %d), want (42, 1)", summaryAge, summaryCount)
	}

	// Legacy text had no validation before the transition, so the overlap keeps
	// accepting malformed and out-of-range old writes and maps them to NULL.
	var malformedLegacyID, overflowLegacyID int64
	if err := legacy.QueryRow(ctx, "legacy_insert", "legacy-malformed", "still-not-an-age").Scan(&malformedLegacyID, &label, &legacyAge); err != nil {
		t.Fatalf("expanded schema rejected a value accepted by the legacy contract: %v", err)
	}
	if err := legacy.QueryRow(ctx, "legacy_insert", "legacy-overflow", "2147483648").Scan(&overflowLegacyID, &label, &legacyAge); err != nil {
		t.Fatalf("expanded schema rejected legacy overflow text: %v", err)
	}
	for _, id := range []int64{malformedLegacyID, overflowLegacyID} {
		if err := deploy.QueryRow(ctx, `SELECT age FROM app.people WHERE id = $1`, id).Scan(&synchronizedAge); err != nil {
			t.Fatal(err)
		}
		if synchronizedAge != nil {
			t.Fatalf("non-convertible legacy text for id=%d produced age=%v, want NULL", id, synchronizedAge)
		}
	}
	assertBlindGauntletBRejectedInteger(t, ctx, deploy, "not-an-integer", "22P02")
	assertBlindGauntletBRejectedInteger(t, ctx, deploy, "2147483648", "22003")
	_, err = deploy.Exec(ctx, `INSERT INTO app.people (label, age_text, age) VALUES ('conflict', '41', 42)`)
	assertBlindGauntletBSQLState(t, err, "23514", "contradictory dual write")

	var materializedRows, baseRows int64
	if err := deploy.QueryRow(ctx, `SELECT COALESCE(sum(people_count), 0) FROM app.people_age_summary`).Scan(&materializedRows); err != nil {
		t.Fatal(err)
	}
	if err := deploy.QueryRow(ctx, `SELECT count(*) FROM app.people`).Scan(&baseRows); err != nil {
		t.Fatal(err)
	}
	if materializedRows != 8 || baseRows <= materializedRows {
		t.Fatalf("materialized overlap freshness boundary rows=%d base=%d, want refreshed baseline 8 and later writes absent", materializedRows, baseRows)
	}
	var legacyBackendAlive bool
	if err := deploy.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, legacyPID).Scan(&legacyBackendAlive); err != nil {
		t.Fatal(err)
	}
	if !legacyBackendAlive {
		t.Fatal("legacy session disappeared before the simulated drain")
	}

	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	legacyClosed = true
	applyBlindGauntletBPhase(t, ctx, deploy, plan, protocol.PhaseContract)

	actual, err := source.LoadDatabaseGraphForComparison(ctx, database.Config, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantFingerprint, err := desired.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	gotFingerprint, err := actual.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if gotFingerprint != wantFingerprint {
		wantJSON, _ := desired.CanonicalJSON()
		gotJSON, _ := actual.CanonicalJSON()
		t.Fatalf("contract did not reach the desired catalog\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
	assertBlindGauntletBFinalData(t, ctx, deploy, baseRows)
}

func buildBlindGauntletBPlan(t *testing.T, current, desired *pgschema.Snapshot) protocol.Result {
	t.Helper()
	oldID := (pgschema.Column{Table: (pgschema.Table{Schema: "app", Name: "people"}).ObjectID(), Name: "age_text"}).ObjectID().String()
	newID := (pgschema.Column{Table: (pgschema.Table{Schema: "app", Name: "people"}).ObjectID(), Name: "age"}).ObjectID().String()
	options := Options{IdentityHints: []protocol.Hint{{
		Kind: "rename", Object: "column",
		From: []string{"app", "people", "age_text"}, To: []string{"app", "people", "age"},
	}}}
	pending, err := Build(current, desired, protocol.Answers{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "rename_column" {
		t.Fatalf("explicit identity did not produce one rename decision: %#v", pending)
	}
	rename := pending.Questions[0]
	if rename.Key != oldID || !blindGauntletBContains(rename.Choices, newID) {
		t.Fatalf("rename decision does not bind %s to %s: %#v", oldID, newID, rename)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{
			Kind: rename.Kind, Key: rename.Key, Value: newID, QuestionFingerprint: rename.ScopeFingerprint,
		}},
	}
	pending, err = Build(current, desired, answers, options)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("cross-type identity did not ask for conversion ownership: %#v", pending)
	}
	conversion := pending.Questions[0]
	if conversion.Key != oldID || !blindGauntletBContains(conversion.ScopeObjects, newID) ||
		!blindGauntletBContains(conversion.Choices, "manual_sql") {
		t.Fatalf("conversion decision is not bound to the compound transition: %#v", conversion)
	}
	answers.Answers = append(answers.Answers, protocol.Answer{
		Kind: conversion.Kind, Key: conversion.Key, Value: "manual_sql", QuestionFingerprint: conversion.ScopeFingerprint,
	})
	planned, err := Build(current, desired, answers, options)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.NeedsSQLEdits || len(planned.Statements) != 2 {
		t.Fatalf("compound transition must produce exactly two editable SQL pockets: %#v", planned)
	}
	joined := joinSQL(planned)
	for _, dependency := range []string{
		"view:app:people_age",
		"materialized_view:app:people_age_summary",
		"index:app:people_age_summary:people_age_summary_age_text_idx",
		"index:app:people_age_summary:people_age_summary_age_idx",
	} {
		if !strings.Contains(joined, dependency) {
			t.Fatalf("owned transition does not name dependency %q:\n%s", dependency, joined)
		}
	}
	return planned
}

func editBlindGauntletBPockets(t *testing.T, plan protocol.Result, transitionID, expandSQL, contractSQL string) protocol.Result {
	t.Helper()
	rewrite := func(statement protocol.Statement) (protocol.Statement, bool) {
		if statement.TransitionID != transitionID || !strings.Contains(statement.SQL, "ONWARDPG TODO") {
			return statement, false
		}
		switch statement.Phase {
		case protocol.PhaseExpand:
			statement.SQL = expandSQL
		case protocol.PhaseContract:
			statement.SQL = contractSQL
		default:
			t.Fatalf("editable transition pocket has phase %q", statement.Phase)
		}
		statement.Manual = nil
		return statement, true
	}
	replacedStatements := 0
	for index := range plan.Statements {
		var replaced bool
		plan.Statements[index], replaced = rewrite(plan.Statements[index])
		if replaced {
			replacedStatements++
		}
	}
	replacedBatches := 0
	for batchIndex := range plan.Batches {
		for statementIndex := range plan.Batches[batchIndex].Statements {
			var replaced bool
			plan.Batches[batchIndex].Statements[statementIndex], replaced = rewrite(plan.Batches[batchIndex].Statements[statementIndex])
			if replaced {
				replacedBatches++
			}
		}
	}
	if replacedStatements != 2 || replacedBatches != 2 {
		t.Fatalf("replaced transition pockets in statements=%d batches=%d, want 2 each", replacedStatements, replacedBatches)
	}
	if strings.Contains(joinSQL(plan), "ONWARDPG TODO") {
		t.Fatalf("reviewed transition left an unresolved SQL pocket:\n%s", joinSQL(plan))
	}
	plan.Status = protocol.Planned
	return plan
}

func applyBlindGauntletBPhase(t *testing.T, ctx context.Context, conn *pgx.Conn, plan protocol.Result, phase string) {
	t.Helper()
	matched := 0
	for _, batch := range plan.Batches {
		if batch.Phase != phase {
			continue
		}
		matched++
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range batch.Statements {
			if _, err := tx.Exec(ctx, statement.SQL); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("apply %s SQL: %v\n%s", phase, err, statement.SQL)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if matched != 1 {
		t.Fatalf("%s batches=%d, want exactly 1", phase, matched)
	}
}

func assertBlindGauntletBInitialConversion(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT label, age FROM app.people ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]*int32{
		"null": nil, "blank": nil, "unknown": nil, "malformed": nil,
		"valid": blindGauntletBInt32(42), "positive-sign-and-space": blindGauntletBInt32(7),
		"overflow-high": nil, "overflow-low": nil,
	}
	seen := 0
	for rows.Next() {
		var label string
		var age *int32
		if err := rows.Scan(&label, &age); err != nil {
			t.Fatal(err)
		}
		expected, exists := want[label]
		if !exists || !blindGauntletBEqualInt32(age, expected) {
			t.Fatalf("initial conversion label=%q age=%v want=%v", label, age, expected)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("initial conversion rows=%d want=%d", seen, len(want))
	}
}

func assertBlindGauntletBRejectedInteger(t *testing.T, ctx context.Context, conn *pgx.Conn, value, code string) {
	t.Helper()
	_, err := conn.Exec(ctx, `INSERT INTO app.people (label, age) VALUES ('new-invalid', $1)`, value)
	assertBlindGauntletBSQLState(t, err, code, "new integer input "+value)
}

func assertBlindGauntletBSQLState(t *testing.T, err error, code, operation string) {
	t.Helper()
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != code {
		t.Fatalf("%s error=%v, want SQLSTATE %s", operation, err, code)
	}
}

func assertBlindGauntletBFinalData(t *testing.T, ctx context.Context, conn *pgx.Conn, expectedRows int64) {
	t.Helper()
	var legacyColumnExists, helperFunctionExists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = 'app' AND table_name = 'people' AND column_name = 'age_text'
)`).Scan(&legacyColumnExists); err != nil {
		t.Fatal(err)
	}
	if legacyColumnExists {
		t.Fatal("legacy age_text column survived contract")
	}
	if err := conn.QueryRow(ctx, `SELECT to_regprocedure('app.onwardpg_age_text_to_integer(text)') IS NOT NULL`).Scan(&helperFunctionExists); err != nil {
		t.Fatal(err)
	}
	if helperFunctionExists {
		t.Fatal("compatibility conversion function survived contract")
	}
	var materializedRows int64
	if err := conn.QueryRow(ctx, `SELECT COALESCE(sum(people_count), 0) FROM app.people_age_summary`).Scan(&materializedRows); err != nil {
		t.Fatal(err)
	}
	if materializedRows != expectedRows {
		t.Fatalf("rebuilt materialized view rows=%d want=%d", materializedRows, expectedRows)
	}
	var indexValid bool
	if err := conn.QueryRow(ctx, `SELECT i.indisvalid AND i.indisready
FROM pg_index AS i
JOIN pg_class AS idx ON idx.oid = i.indexrelid
JOIN pg_class AS rel ON rel.oid = i.indrelid
JOIN pg_namespace AS n ON n.oid = rel.relnamespace
WHERE n.nspname = 'app' AND rel.relname = 'people_age_summary' AND idx.relname = 'people_age_summary_age_idx'`).Scan(&indexValid); err != nil {
		t.Fatal(err)
	}
	if !indexValid {
		t.Fatal("final materialized-view index is not ready and valid")
	}
	var finalID int64
	var age int32
	if err := conn.QueryRow(ctx, `INSERT INTO app.people (label, age) VALUES ('final-writer', 61) RETURNING id, age`).Scan(&finalID, &age); err != nil {
		t.Fatalf("final new INSERT failed: %v", err)
	}
	if age != 61 || finalID == 0 {
		t.Fatalf("final new INSERT returned id=%d age=%d", finalID, age)
	}
	_, err := conn.Exec(ctx, `SELECT age_text FROM app.people LIMIT 1`)
	assertBlindGauntletBSQLState(t, err, "42703", "legacy query after drain and contract")
}

func blindGauntletBContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func blindGauntletBInt32(value int32) *int32 { return &value }

func blindGauntletBEqualInt32(left, right *int32) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

const blindGauntletBExpandSQL = `CREATE FUNCTION app.onwardpg_age_text_to_integer(input text)
RETURNS integer
LANGUAGE plpgsql
IMMUTABLE
AS $function$
DECLARE
  normalized text;
  numeric_value numeric;
BEGIN
  IF input IS NULL THEN
    RETURN NULL;
  END IF;
  normalized := btrim(input);
  IF normalized = '' OR lower(normalized) = 'unknown' OR normalized !~ '^[+-]?[0-9]+$' THEN
    RETURN NULL;
  END IF;
  numeric_value := normalized::numeric;
  IF numeric_value < -2147483648 OR numeric_value > 2147483647 THEN
    RETURN NULL;
  END IF;
  RETURN numeric_value::integer;
END;
$function$;

CREATE FUNCTION app.onwardpg_age_integer_to_text(input integer)
RETURNS text
LANGUAGE sql
IMMUTABLE
STRICT
AS $function$
  SELECT input::text
$function$;

ALTER TABLE app.people ADD COLUMN age integer;

UPDATE app.people
SET age = app.onwardpg_age_text_to_integer(age_text)
WHERE age IS DISTINCT FROM app.onwardpg_age_text_to_integer(age_text);

DO $assert$
BEGIN
  IF EXISTS (
    SELECT 1 FROM app.people
    WHERE age IS DISTINCT FROM app.onwardpg_age_text_to_integer(age_text)
  ) THEN
    RAISE EXCEPTION 'onwardpg initial age conversion failed' USING ERRCODE = '23514';
  END IF;
  IF app.onwardpg_age_text_to_integer('-2147483648') <> -2147483648
     OR app.onwardpg_age_text_to_integer('2147483647') <> 2147483647
     OR app.onwardpg_age_text_to_integer('-2147483649') IS NOT NULL
     OR app.onwardpg_age_text_to_integer('2147483648') IS NOT NULL
     OR app.onwardpg_age_text_to_integer('unknown') IS NOT NULL
     OR app.onwardpg_age_text_to_integer('malformed') IS NOT NULL THEN
    RAISE EXCEPTION 'onwardpg age conversion boundary assertion failed' USING ERRCODE = '23514';
  END IF;
END
$assert$;

CREATE FUNCTION app.onwardpg_sync_age_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
  old_changed boolean;
  new_changed boolean;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.age_text IS NOT NULL AND NEW.age IS NOT NULL THEN
      IF app.onwardpg_age_text_to_integer(NEW.age_text) IS DISTINCT FROM NEW.age THEN
        RAISE EXCEPTION 'conflicting age_text and age values' USING ERRCODE = '23514';
      END IF;
    ELSIF NEW.age_text IS NOT NULL THEN
      NEW.age := app.onwardpg_age_text_to_integer(NEW.age_text);
    ELSIF NEW.age IS NOT NULL THEN
      NEW.age_text := app.onwardpg_age_integer_to_text(NEW.age);
    END IF;
    RETURN NEW;
  END IF;

  old_changed := NEW.age_text IS DISTINCT FROM OLD.age_text;
  new_changed := NEW.age IS DISTINCT FROM OLD.age;
  IF old_changed AND new_changed THEN
    IF app.onwardpg_age_text_to_integer(NEW.age_text) IS DISTINCT FROM NEW.age THEN
      RAISE EXCEPTION 'conflicting age_text and age updates' USING ERRCODE = '23514';
    END IF;
  ELSIF old_changed THEN
    NEW.age := app.onwardpg_age_text_to_integer(NEW.age_text);
  ELSIF new_changed THEN
    NEW.age_text := app.onwardpg_age_integer_to_text(NEW.age);
  END IF;
  RETURN NEW;
END;
$function$;

CREATE TRIGGER onwardpg_sync_age_transition
BEFORE INSERT OR UPDATE OF age_text, age ON app.people
FOR EACH ROW EXECUTE FUNCTION app.onwardpg_sync_age_transition();

CREATE OR REPLACE VIEW app.people_age AS
SELECT p.id, p.age_text, p.age FROM app.people AS p;

DROP MATERIALIZED VIEW app.people_age_summary;

CREATE MATERIALIZED VIEW app.people_age_summary AS
SELECT p.age_text, p.age, count(*) AS people_count
FROM app.people_age AS p
GROUP BY p.age_text, p.age;

CREATE UNIQUE INDEX people_age_summary_age_text_idx
ON app.people_age_summary (age_text);`

const blindGauntletBContractSQL = `UPDATE app.people
SET age = app.onwardpg_age_text_to_integer(age_text)
WHERE age IS DISTINCT FROM app.onwardpg_age_text_to_integer(age_text);

DO $assert$
BEGIN
  IF EXISTS (
    SELECT 1 FROM app.people
    WHERE age IS DISTINCT FROM app.onwardpg_age_text_to_integer(age_text)
  ) THEN
    RAISE EXCEPTION 'onwardpg final age conversion failed' USING ERRCODE = '23514';
  END IF;
END
$assert$;

DROP TRIGGER onwardpg_sync_age_transition ON app.people;
DROP FUNCTION app.onwardpg_sync_age_transition();

DROP MATERIALIZED VIEW app.people_age_summary;
DROP VIEW app.people_age;
ALTER TABLE app.people DROP COLUMN age_text;

DROP FUNCTION app.onwardpg_age_integer_to_text(integer);
DROP FUNCTION app.onwardpg_age_text_to_integer(text);

CREATE VIEW app.people_age AS
SELECT p.id, p.age FROM app.people AS p;

CREATE MATERIALIZED VIEW app.people_age_summary AS
SELECT p.age, count(*) AS people_count
FROM app.people_age AS p
GROUP BY p.age;

CREATE UNIQUE INDEX people_age_summary_age_idx
ON app.people_age_summary (age);`
