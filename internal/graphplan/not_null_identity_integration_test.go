package graphplan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// PostgreSQL 18 shortens a generated NOT NULL name past NAMEDATALEN and then
// appends a collision counter, so tables that share a long prefix get
// <prefix>_not_null, <prefix>_not_null1, ... in creation order. A production
// database and onwardpg's replay of the same schema can therefore name the
// same (table, column) constraint differently. A plan built from the replay
// must still apply to both creation orders, and each result must verify.
func TestNotNullConstraintRenamesResolveRealNamesOnPostgreSQL(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	lockGraphPlanIntegration(t, ctx, conn)
	postgres18 := strings.HasPrefix(conn.PgConn().ParameterStatus("server_version"), "18.")
	schemaName := "onwardpg_nn_" + time.Now().UTC().Format("20060102150405")
	defer func() { _, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`) }()

	// Both tables truncate to trip_line_item_destination__trip_line_item_destination_not_null,
	// so the second one created takes the counter.
	day := `CREATE TABLE "` + schemaName + `".trip_line_item_destination_day (trip_line_item_destination_id bigint NOT NULL);`
	room := `CREATE TABLE "` + schemaName + `".trip_line_item_destination_room (trip_line_item_destination_id bigint NOT NULL);`
	schema := `CREATE SCHEMA "` + schemaName + `";`
	productionDDL := schema + "\n" + day + "\n" + room
	replayDDL := schema + "\n" + room + "\n" + day
	desiredDDL := schema + `
CREATE TABLE "` + schemaName + `".trip_line_item_destination_day (destination_id bigint NOT NULL);
CREATE TABLE "` + schemaName + `".trip_line_item_dest_room (trip_line_item_destination_id bigint NOT NULL);`
	directory := t.TempDir()
	load := func(name, ddl string) *pgschema.Snapshot {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.LoadGraph(ctx, source.Parse("file://"+path), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	current := load("replay.sql", replayDDL)
	desired := load("desired.sql", desiredDDL)

	options := Options{DirectColumnRenames: true}
	plan, err := buildIntegration(current, desired, protocol.Answers{}, options)
	if err != nil {
		t.Fatal(err)
	}
	dayColumn := pgschema.ID{Kind: pgschema.KindColumn, Schema: schemaName, Name: "trip_line_item_destination_day", Part: "trip_line_item_destination_id"}
	renamedColumn := dayColumn
	renamedColumn.Part = "destination_id"
	answers := protocol.Answers{CurrentFingerprint: plan.CurrentFingerprint, DesiredFingerprint: plan.DesiredFingerprint, Answers: []protocol.Answer{
		{Kind: "rename_column", Key: dayColumn.String(), Value: renamedColumn.String()},
		{Kind: "rename_table", Key: (pgschema.Table{Schema: schemaName, Name: "trip_line_item_destination_room"}).ObjectID().String(), Value: (pgschema.Table{Schema: schemaName, Name: "trip_line_item_dest_room"}).ObjectID().String()},
	}}
	plan, err = buildIntegration(current, desired, answers, options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("plan status = %s: %#v", plan.Status, plan)
	}
	sql := joinSQL(plan)
	if postgres18 {
		// The replay holds trip_line_item_destination__..._not_null on room
		// and ..._not_null1 on day; production holds the reverse. No
		// statement may name either replay constraint.
		if strings.Count(sql, "con.contype = 'n'") != 2 {
			t.Fatalf("expected two apply-time NOT NULL renames:\n%s", sql)
		}
		if strings.Contains(sql, "_not_null1") || strings.Contains(sql, "destination__trip_line_item_destination_not_null") {
			t.Fatalf("plan names a replay NOT NULL constraint:\n%s", sql)
		}
	} else if strings.Contains(sql, "con.contype = 'n'") {
		t.Fatalf("PostgreSQL before 18 has no NOT NULL constraint to rename:\n%s", sql)
	}

	want, err := desired.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ name, ddl string }{{"production order", productionDDL}, {"replay order", replayDDL}} {
		if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, target.ddl); err != nil {
			t.Fatal(err)
		}
		if postgres18 {
			assertNotNullNames(t, ctx, conn, schemaName, target.name == "production order")
		}
		applyPlan(t, ctx, conn, plan)
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		projected := actual
		if unsupported := projected.Unsupported(); len(unsupported) != 0 {
			t.Fatalf("%s: applied catalog has blockers %v", target.name, unsupported)
		}
		got, err := projected.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: applied catalog fingerprint %s, want %s", target.name, got, want)
		}
		residual, err := Build(projected, desired, protocol.Answers{}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(residual.Statements) != 0 || len(residual.Questions) != 0 {
			t.Fatalf("%s: residual plan after apply: %#v", target.name, residual)
		}
	}
}

// assertNotNullNames proves the fixture premise: which table holds the
// collision counter depends on creation order.
func assertNotNullNames(t *testing.T, ctx context.Context, conn *pgx.Conn, schemaName string, dayFirst bool) {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT c.relname::text, con.conname::text FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND con.contype = 'n'`, schemaName)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var pair [2]string
		return pair, row.Scan(&pair[0], &pair[1])
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "trip_line_item_destination__trip_line_item_destination_not_null"
	first, second := "trip_line_item_destination_room", "trip_line_item_destination_day"
	if dayFirst {
		first, second = second, first
	}
	byTable := map[string]string{}
	for _, pair := range names {
		byTable[pair[0]] = pair[1]
	}
	if byTable[first] != base || !strings.HasPrefix(byTable[second], "trip_line_item_destination") || !strings.HasSuffix(byTable[second], "_not_null1") {
		t.Fatalf("fixture premise: NOT NULL names %v", byTable)
	}
}
