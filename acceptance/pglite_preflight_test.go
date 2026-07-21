package acceptance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jokull/onwardpg/internal/testkit"
)

type preparedBoolean struct {
	name string
	sql  string
	args []any
}

type pglitePreflightCase struct {
	registration        testkit.PGliteScenarioRegistration
	setup               []string
	beforeExpandChecks  []string
	prepared            *preparedBoolean
	expand              []string
	afterExpand         []string
	afterExpandChecks   []string
	rejectedAfterExpand []string
	contract            []string
	afterContract       []string
	afterContractChecks []string
}

func TestPGlitePreflight(t *testing.T) {
	if os.Getenv("ONWARDPG_PGLITE") != "1" {
		t.Skip("set ONWARDPG_PGLITE=1 to run the scoped PGlite preflight")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	engine, err := testkit.StartPGlite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close PGlite: %v; logs:\n%s", err, engine.Logs())
		}
	})

	report, err := json.Marshal(engine.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PGlite capability report: %s", report)

	owners := mustRegisterNativeOwners(t)
	cases := pgliteCoreCases()
	if len(cases) < 80 {
		t.Fatalf("PGlite preflight registered %d variants, want at least 80", len(cases))
	}
	t.Logf("PGlite preflight variants: %d", len(cases))
	registrations := make([]testkit.PGliteScenarioRegistration, 0, len(cases))
	for _, test := range cases {
		registrations = append(registrations, test.registration)
	}
	if err := testkit.ValidatePGliteScenarios(engine.Capabilities, owners, registrations); err != nil {
		t.Fatalf("register PGlite preflight cases: %v", err)
	}

	for _, test := range cases {
		t.Run(test.registration.VariantID, func(t *testing.T) {
			runPGlitePreflightCase(t, ctx, engine, test)
		})
	}
}

func TestPGliteRegistrationFailsClosed(t *testing.T) {
	owners := mustRegisterNativeOwners(t)
	report := testkit.CapabilityReport{Supported: map[testkit.Capability]bool{
		testkit.CapabilityTableDDL: true,
	}}
	valid := testkit.PGliteScenarioRegistration{
		VariantID:     "valid.case",
		NativeOwnerID: "native.table-column-lifecycle",
		Invariant:     "legacy inserts survive an additive column",
		Requires:      []testkit.Capability{testkit.CapabilityTableDDL},
	}

	tests := []struct {
		name      string
		scenarios []testkit.PGliteScenarioRegistration
		want      string
	}{
		{
			name: "orphaned native owner",
			scenarios: []testkit.PGliteScenarioRegistration{{
				VariantID: "orphan.case", NativeOwnerID: "native.missing", Invariant: "claim", Requires: []testkit.Capability{testkit.CapabilityTableDDL},
			}},
			want: "unregistered native owner",
		},
		{
			name: "unsupported capability",
			scenarios: []testkit.PGliteScenarioRegistration{{
				VariantID: "unsafe.concurrent", NativeOwnerID: "native.table-column-lifecycle", Invariant: "claim", Requires: []testkit.Capability{testkit.CapabilityDistinctBackends},
			}},
			want: "unsupported capability",
		},
		{
			name:      "duplicate variant",
			scenarios: []testkit.PGliteScenarioRegistration{valid, valid},
			want:      "duplicate PGlite variant",
		},
		{
			name: "unknown capability",
			scenarios: []testkit.PGliteScenarioRegistration{{
				VariantID: "unknown.capability", NativeOwnerID: "native.table-column-lifecycle", Invariant: "claim", Requires: []testkit.Capability{"invented"},
			}},
			want: "unknown capability",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := testkit.ValidatePGliteScenarios(report, owners, test.scenarios)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidatePGliteScenarios() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestPGlitePGXWireModeIsIsolated(t *testing.T) {
	config, err := testkit.PGliteConnConfig("postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if config.StatementCacheCapacity != 0 || config.DescriptionCacheCapacity != 0 {
		t.Fatalf("PGlite caches = statement:%d description:%d, want both disabled", config.StatementCacheCapacity, config.DescriptionCacheCapacity)
	}
	if config.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatalf("PGlite query mode = %s, want %s", config.DefaultQueryExecMode, pgx.QueryExecModeExec)
	}
	if _, err := testkit.PGliteConnConfig("postgres://postgres:postgres@192.0.2.1:5432/postgres?sslmode=disable"); err == nil {
		t.Fatal("non-loopback PGlite URL was accepted")
	}
}

func mustRegisterNativeOwners(t *testing.T) testkit.NativeOwnerRegistry {
	t.Helper()
	owners, err := testkit.RegisterNativeOwners(
		testkit.NativeOwner{ID: "native.table-column-lifecycle", Package: "./internal/graphplan", Test: "TestTableColumnLifecycleMatrixConvergesOnPostgreSQL"},
		testkit.NativeOwner{ID: "native.required-column", Package: "./internal/graphplan", Test: "TestRequiredColumnStagingConvergesOnPostgreSQL"},
		testkit.NativeOwner{ID: "native.check-widening", Package: "./internal/graphplan", Test: "TestCheckWideningAcceptsLegacyAndNewWritesAfterExpand"},
		testkit.NativeOwner{ID: "native.column-rename-overlap", Package: "./internal/graphplan", Test: "TestColumnRenameOverlapBridgeConvergesOnPostgreSQL"},
		testkit.NativeOwner{ID: "native.view-replacement", Package: "./internal/graphplan", Test: "TestViewCreateAndReplaceConvergeOnPostgreSQL"},
		testkit.NativeOwner{ID: "native.materialized-view-lifecycle", Package: "./internal/graphplan", Test: "TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return owners
}

func runPGlitePreflightCase(t *testing.T, ctx context.Context, engine *testkit.PGlite, test pglitePreflightCase) {
	t.Helper()
	connection, err := engine.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())

	execStatements(t, ctx, connection, "setup", test.setup)
	assertBooleans(t, ctx, connection, "before expand", test.beforeExpandChecks)
	if test.prepared != nil {
		if _, err := connection.Prepare(ctx, test.prepared.name, test.prepared.sql); err != nil {
			t.Fatalf("prepare legacy SQL: %v", err)
		}
		defer connection.Deallocate(context.Background(), test.prepared.name)
	}

	execStatements(t, ctx, connection, "expand", test.expand)
	if test.prepared != nil {
		var valid bool
		if err := connection.QueryRow(ctx, test.prepared.name, test.prepared.args...).Scan(&valid); err != nil {
			t.Fatalf("execute prepared legacy SQL after expand: %v", err)
		}
		if !valid {
			t.Fatal("prepared legacy SQL returned false after expand")
		}
	}
	execStatements(t, ctx, connection, "after expand workload", test.afterExpand)
	assertBooleans(t, ctx, connection, "after expand", test.afterExpandChecks)
	for _, statement := range test.rejectedAfterExpand {
		_, err := connection.Exec(ctx, statement)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
			t.Fatalf("rejected overlap write error = %v, want SQLSTATE 23514", err)
		}
	}

	execStatements(t, ctx, connection, "contract", test.contract)
	execStatements(t, ctx, connection, "after contract workload", test.afterContract)
	assertBooleans(t, ctx, connection, "after contract", test.afterContractChecks)
}

func execStatements(t *testing.T, ctx context.Context, connection *pgx.Conn, stage string, statements []string) {
	t.Helper()
	for index, statement := range statements {
		if _, err := connection.Exec(ctx, statement); err != nil {
			t.Fatalf("%s statement %d failed: %v\n%s", stage, index+1, err, statement)
		}
	}
}

func assertBooleans(t *testing.T, ctx context.Context, connection *pgx.Conn, stage string, queries []string) {
	t.Helper()
	for index, query := range queries {
		var valid bool
		if err := connection.QueryRow(ctx, query).Scan(&valid); err != nil {
			t.Fatalf("%s assertion %d failed: %v\n%s", stage, index+1, err, query)
		}
		if !valid {
			t.Fatalf("%s assertion %d returned false:\n%s", stage, index+1, query)
		}
	}
}

func pgliteCoreCases() []pglitePreflightCase {
	prepared := func(name, sql string, args ...any) *preparedBoolean {
		return &preparedBoolean{name: name, sql: sql, args: args}
	}
	registration := func(variant, owner, invariant string, capabilities ...testkit.Capability) testkit.PGliteScenarioRegistration {
		return testkit.PGliteScenarioRegistration{VariantID: variant, NativeOwnerID: owner, Invariant: invariant, Requires: capabilities}
	}

	cases := []pglitePreflightCase{
		{
			registration:      registration("additive.nonempty-legacy-omit", "native.table-column-lifecycle", "an old insert may omit a newly added nullable column", testkit.CapabilityTableDDL, testkit.CapabilityExplicitPrepareSameBackend),
			setup:             []string{"CREATE SCHEMA pf_additive_nonempty", "CREATE TABLE pf_additive_nonempty.bookings (id bigint PRIMARY KEY)", "INSERT INTO pf_additive_nonempty.bookings VALUES (1)"},
			prepared:          prepared("pf_additive_nonempty_old_insert", "INSERT INTO pf_additive_nonempty.bookings (id) VALUES ($1) RETURNING id = $1", int64(2)),
			expand:            []string{"ALTER TABLE pf_additive_nonempty.bookings ADD COLUMN status text"},
			afterExpand:       []string{"INSERT INTO pf_additive_nonempty.bookings (id, status) VALUES (3, 'pending')"},
			afterExpandChecks: []string{"SELECT count(*) = 3 AND count(status) = 1 FROM pf_additive_nonempty.bookings"},
		},
		{
			registration:      registration("additive.quoted-default", "native.table-column-lifecycle", "quoted additive DDL preserves old and new insert shapes", testkit.CapabilityTableDDL, testkit.CapabilityExplicitPrepareSameBackend),
			setup:             []string{`CREATE SCHEMA pf_additive_quoted`, `CREATE TABLE pf_additive_quoted."order" (id bigint PRIMARY KEY)`},
			prepared:          prepared("pf_additive_quoted_old_insert", `INSERT INTO pf_additive_quoted."order" (id) VALUES ($1) RETURNING id = $1`, int64(1)),
			expand:            []string{`ALTER TABLE pf_additive_quoted."order" ADD COLUMN "select" text DEFAULT 'ready'`},
			afterExpand:       []string{`INSERT INTO pf_additive_quoted."order" (id, "select") VALUES (2, 'chosen')`},
			afterExpandChecks: []string{`SELECT bool_and("select" IN ('ready', 'chosen')) AND count(*) = 2 FROM pf_additive_quoted."order"`},
		},
		{
			registration:        registration("required.mixed-product-cleanup", "native.required-column", "legacy NULLs are accepted during overlap and product cleanup makes NOT NULL safe", testkit.CapabilityTableDDL, testkit.CapabilityExplicitPrepareSameBackend),
			setup:               []string{"CREATE SCHEMA pf_required_mixed", "CREATE TABLE pf_required_mixed.bookings (id bigint PRIMARY KEY)", "INSERT INTO pf_required_mixed.bookings VALUES (1)"},
			prepared:            prepared("pf_required_mixed_old_insert", "INSERT INTO pf_required_mixed.bookings (id) VALUES ($1) RETURNING id = $1", int64(2)),
			expand:              []string{"ALTER TABLE pf_required_mixed.bookings ADD COLUMN status text"},
			afterExpand:         []string{"INSERT INTO pf_required_mixed.bookings VALUES (3, 'confirmed')"},
			afterExpandChecks:   []string{"SELECT count(*) FILTER (WHERE status IS NULL) = 2 FROM pf_required_mixed.bookings"},
			contract:            []string{"UPDATE pf_required_mixed.bookings SET status = CASE WHEN id = 1 THEN 'imported' ELSE 'pending' END WHERE status IS NULL", "ALTER TABLE pf_required_mixed.bookings ALTER COLUMN status SET NOT NULL"},
			afterContract:       []string{"INSERT INTO pf_required_mixed.bookings VALUES (4, 'confirmed')"},
			afterContractChecks: []string{"SELECT count(*) = 4 AND count(status) = 4 AND max(status) IS NOT NULL FROM pf_required_mixed.bookings"},
		},
		{
			registration:        registration("required.empty-table", "native.required-column", "required-column phases converge without inventing data on an empty table", testkit.CapabilityTableDDL),
			setup:               []string{"CREATE SCHEMA pf_required_empty", "CREATE TABLE pf_required_empty.bookings (id bigint PRIMARY KEY)"},
			expand:              []string{"ALTER TABLE pf_required_empty.bookings ADD COLUMN status text"},
			afterExpandChecks:   []string{"SELECT count(*) = 0 FROM pf_required_empty.bookings"},
			contract:            []string{"ALTER TABLE pf_required_empty.bookings ALTER COLUMN status SET NOT NULL"},
			afterContract:       []string{"INSERT INTO pf_required_empty.bookings VALUES (1, 'pending')"},
			afterContractChecks: []string{"SELECT status = 'pending' FROM pf_required_empty.bookings WHERE id = 1"},
		},
		{
			registration:        registration("check.named-text-widen", "native.check-widening", "a widened named CHECK accepts both legacy and new enum-like values", testkit.CapabilityTableDDL, testkit.CapabilityCheckConstraint, testkit.CapabilityExplicitPrepareSameBackend),
			setup:               []string{"CREATE SCHEMA pf_check_text", "CREATE TABLE pf_check_text.deliveries (id bigint PRIMARY KEY, tier text CONSTRAINT delivery_tier_check CHECK (tier IN ('legacy')))"},
			prepared:            prepared("pf_check_text_old_insert", "INSERT INTO pf_check_text.deliveries VALUES ($1, 'legacy') RETURNING tier = 'legacy'", int64(1)),
			expand:              []string{"ALTER TABLE pf_check_text.deliveries DROP CONSTRAINT delivery_tier_check, ADD CONSTRAINT delivery_tier_check CHECK (tier IN ('legacy', 'slack_channel'))"},
			afterExpand:         []string{"INSERT INTO pf_check_text.deliveries VALUES (2, 'slack_channel')"},
			afterExpandChecks:   []string{"SELECT count(*) = 2 AND count(DISTINCT tier) = 2 FROM pf_check_text.deliveries"},
			rejectedAfterExpand: []string{"INSERT INTO pf_check_text.deliveries VALUES (3, 'unknown')"},
		},
		{
			registration:        registration("check.numeric-boundary-widen", "native.check-widening", "numeric CHECK widening keeps the old boundary and admits the new boundary", testkit.CapabilityTableDDL, testkit.CapabilityCheckConstraint, testkit.CapabilityExplicitPrepareSameBackend),
			setup:               []string{"CREATE SCHEMA pf_check_numeric", "CREATE TABLE pf_check_numeric.readings (id bigint PRIMARY KEY, value integer CONSTRAINT value_check CHECK (value >= 0))"},
			prepared:            prepared("pf_check_numeric_old_insert", "INSERT INTO pf_check_numeric.readings VALUES ($1, 0) RETURNING value = 0", int64(1)),
			expand:              []string{"ALTER TABLE pf_check_numeric.readings DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (value >= -10)"},
			afterExpand:         []string{"INSERT INTO pf_check_numeric.readings VALUES (2, -10), (3, 100)"},
			afterExpandChecks:   []string{"SELECT min(value) = -10 AND max(value) = 100 FROM pf_check_numeric.readings"},
			rejectedAfterExpand: []string{"INSERT INTO pf_check_numeric.readings VALUES (4, -11)"},
		},
		{
			registration:        registration("rename.bidirectional-inserts", "native.column-rename-overlap", "old-only and new-only inserts are synchronized during a column rename", testkit.CapabilityTableDDL, testkit.CapabilityTrigger, testkit.CapabilityExplicitPrepareSameBackend),
			setup:               []string{"CREATE SCHEMA pf_rename_insert", "CREATE TABLE pf_rename_insert.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO pf_rename_insert.people VALUES (1, 'before')"},
			prepared:            prepared("pf_rename_insert_old_write", "INSERT INTO pf_rename_insert.people (id, old_name) VALUES ($1, $2) RETURNING old_name = $2", int64(2), "legacy"),
			expand:              renameExpandSQL("pf_rename_insert"),
			afterExpand:         []string{"INSERT INTO pf_rename_insert.people (id, new_name) VALUES (3, 'modern')"},
			afterExpandChecks:   []string{"SELECT count(*) = 3 AND bool_and(old_name = new_name) FROM pf_rename_insert.people"},
			contract:            renameContractSQL("pf_rename_insert"),
			afterContract:       []string{"INSERT INTO pf_rename_insert.people (id, new_name) VALUES (4, 'after')"},
			afterContractChecks: []string{"SELECT count(*) = 4 AND bool_and(new_name IS NOT NULL) FROM pf_rename_insert.people"},
		},
		{
			registration:        registration("rename.bidirectional-updates", "native.column-rename-overlap", "updates through either column converge on one value during overlap", testkit.CapabilityTableDDL, testkit.CapabilityTrigger, testkit.CapabilityExplicitPrepareSameBackend),
			setup:               []string{"CREATE SCHEMA pf_rename_update", "CREATE TABLE pf_rename_update.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO pf_rename_update.people VALUES (1, 'before')"},
			prepared:            prepared("pf_rename_update_old_write", "UPDATE pf_rename_update.people SET old_name = $2 WHERE id = $1 RETURNING old_name = $2", int64(1), "legacy-update"),
			expand:              renameExpandSQL("pf_rename_update"),
			afterExpand:         []string{"UPDATE pf_rename_update.people SET new_name = 'modern-update' WHERE id = 1"},
			afterExpandChecks:   []string{"SELECT old_name = 'modern-update' AND new_name = old_name FROM pf_rename_update.people WHERE id = 1"},
			contract:            renameContractSQL("pf_rename_update"),
			afterContractChecks: []string{"SELECT new_name = 'modern-update' FROM pf_rename_update.people WHERE id = 1"},
		},
		{
			registration:      registration("view.direct-append-output", "native.view-replacement", "an appended view output preserves a prepared legacy projection", testkit.CapabilityTableDDL, testkit.CapabilityView, testkit.CapabilityExplicitPrepareSameBackend),
			setup:             []string{"CREATE SCHEMA pf_view_direct", "CREATE TABLE pf_view_direct.bookings (id bigint, status text)", "INSERT INTO pf_view_direct.bookings VALUES (1, 'pending')", "CREATE VIEW pf_view_direct.booking_view AS SELECT id FROM pf_view_direct.bookings"},
			prepared:          prepared("pf_view_direct_old_read", "SELECT id = $1 FROM pf_view_direct.booking_view WHERE id = $1", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW pf_view_direct.booking_view AS SELECT id, status FROM pf_view_direct.bookings"},
			afterExpandChecks: []string{"SELECT id = 1 AND status = 'pending' FROM pf_view_direct.booking_view"},
		},
		{
			registration:      registration("view.two-level-append-output", "native.view-replacement", "a dependent legacy view remains valid when its source appends an output", testkit.CapabilityTableDDL, testkit.CapabilityView, testkit.CapabilityExplicitPrepareSameBackend),
			setup:             []string{"CREATE SCHEMA pf_view_chain", "CREATE TABLE pf_view_chain.bookings (id bigint, status text)", "INSERT INTO pf_view_chain.bookings VALUES (1, 'pending')", "CREATE VIEW pf_view_chain.booking_view AS SELECT id FROM pf_view_chain.bookings", "CREATE VIEW pf_view_chain.legacy_booking_view AS SELECT id FROM pf_view_chain.booking_view"},
			prepared:          prepared("pf_view_chain_old_read", "SELECT id = $1 FROM pf_view_chain.legacy_booking_view WHERE id = $1", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW pf_view_chain.booking_view AS SELECT id, status FROM pf_view_chain.bookings"},
			afterExpandChecks: []string{"SELECT id = 1 FROM pf_view_chain.legacy_booking_view", "SELECT status = 'pending' FROM pf_view_chain.booking_view WHERE id = 1"},
		},
		{
			registration:       registration("matview.reviewed-append-output", "native.materialized-view-lifecycle", "a reviewed materialized-view rebuild preserves the old projection and adds the new output", testkit.CapabilityTableDDL, testkit.CapabilityMaterializedView),
			setup:              []string{"CREATE SCHEMA pf_matview_append", "CREATE TABLE pf_matview_append.people (id bigint PRIMARY KEY)", "INSERT INTO pf_matview_append.people VALUES (1)", "CREATE MATERIALIZED VIEW pf_matview_append.people_rollup AS SELECT id FROM pf_matview_append.people"},
			beforeExpandChecks: []string{"SELECT id = 1 FROM pf_matview_append.people_rollup"},
			expand:             []string{"ALTER TABLE pf_matview_append.people ADD COLUMN age integer", "UPDATE pf_matview_append.people SET age = 42", "DROP MATERIALIZED VIEW pf_matview_append.people_rollup", "CREATE MATERIALIZED VIEW pf_matview_append.people_rollup AS SELECT id, age FROM pf_matview_append.people"},
			afterExpandChecks:  []string{"SELECT id = 1 FROM pf_matview_append.people_rollup", "SELECT age = 42 FROM pf_matview_append.people_rollup WHERE id = 1"},
		},
		{
			registration:       registration("matview.reviewed-grouped-output", "native.materialized-view-lifecycle", "a reviewed grouped rebuild retains legacy aggregates while appending a new aggregate", testkit.CapabilityTableDDL, testkit.CapabilityMaterializedView),
			setup:              []string{"CREATE SCHEMA pf_matview_grouped", "CREATE TABLE pf_matview_grouped.bookings (id bigint, status text)", "INSERT INTO pf_matview_grouped.bookings VALUES (1, 'pending'), (2, 'pending')", "CREATE MATERIALIZED VIEW pf_matview_grouped.booking_rollup AS SELECT status, count(*) AS total FROM pf_matview_grouped.bookings GROUP BY status"},
			beforeExpandChecks: []string{"SELECT status = 'pending' AND total = 2 FROM pf_matview_grouped.booking_rollup"},
			expand:             []string{"ALTER TABLE pf_matview_grouped.bookings ADD COLUMN priority integer", "UPDATE pf_matview_grouped.bookings SET priority = id::integer", "DROP MATERIALIZED VIEW pf_matview_grouped.booking_rollup", "CREATE MATERIALIZED VIEW pf_matview_grouped.booking_rollup AS SELECT status, count(*) AS total, min(priority) AS min_priority FROM pf_matview_grouped.bookings GROUP BY status"},
			afterExpandChecks:  []string{"SELECT status = 'pending' AND total = 2 FROM pf_matview_grouped.booking_rollup", "SELECT min_priority = 1 FROM pf_matview_grouped.booking_rollup"},
		},
	}
	cases = append(cases, additivePGliteVariants()...)
	cases = append(cases, requiredPGliteVariants()...)
	cases = append(cases, checkPGliteVariants()...)
	cases = append(cases, renamePGliteVariants()...)
	cases = append(cases, viewPGliteVariants()...)
	cases = append(cases, materializedViewPGliteVariants()...)
	return cases
}

type pgliteCaseTemplate struct {
	variantID           string
	schema              string
	nativeOwnerID       string
	invariant           string
	requires            []testkit.Capability
	setup               []string
	beforeExpandChecks  []string
	prepared            *preparedBoolean
	expand              []string
	afterExpand         []string
	afterExpandChecks   []string
	rejectedAfterExpand []string
	contract            []string
	afterContract       []string
	afterContractChecks []string
}

func materializePGliteCases(templates []pgliteCaseTemplate) []pglitePreflightCase {
	result := make([]pglitePreflightCase, 0, len(templates))
	for _, template := range templates {
		var prepared *preparedBoolean
		if template.prepared != nil {
			prepared = &preparedBoolean{
				name: template.prepared.name,
				sql:  replacePGliteSchema(template.schema, template.prepared.sql),
				args: append([]any(nil), template.prepared.args...),
			}
		}
		result = append(result, pglitePreflightCase{
			registration: testkit.PGliteScenarioRegistration{
				VariantID: template.variantID, NativeOwnerID: template.nativeOwnerID,
				Invariant: template.invariant, Requires: append([]testkit.Capability(nil), template.requires...),
			},
			setup:               replacePGliteSchemas(template.schema, template.setup),
			beforeExpandChecks:  replacePGliteSchemas(template.schema, template.beforeExpandChecks),
			prepared:            prepared,
			expand:              replacePGliteSchemas(template.schema, template.expand),
			afterExpand:         replacePGliteSchemas(template.schema, template.afterExpand),
			afterExpandChecks:   replacePGliteSchemas(template.schema, template.afterExpandChecks),
			rejectedAfterExpand: replacePGliteSchemas(template.schema, template.rejectedAfterExpand),
			contract:            replacePGliteSchemas(template.schema, template.contract),
			afterContract:       replacePGliteSchemas(template.schema, template.afterContract),
			afterContractChecks: replacePGliteSchemas(template.schema, template.afterContractChecks),
		})
	}
	return result
}

func replacePGliteSchemas(schema string, statements []string) []string {
	result := make([]string, len(statements))
	for index, statement := range statements {
		result[index] = replacePGliteSchema(schema, statement)
	}
	return result
}

func replacePGliteSchema(schema, statement string) string {
	return strings.ReplaceAll(statement, "$schema", schema)
}

func additivePGliteVariants() []pglitePreflightCase {
	owner := "native.table-column-lifecycle"
	capabilities := []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityExplicitPrepareSameBackend}
	prepared := func(name, sql string, args ...any) *preparedBoolean {
		return &preparedBoolean{name: name, sql: sql, args: args}
	}
	return materializePGliteCases([]pgliteCaseTemplate{
		{
			variantID: "additive.empty-no-default", schema: "pf_add_empty", nativeOwnerID: owner,
			invariant: "an empty table accepts a nullable addition without fabricating rows", requires: []testkit.Capability{testkit.CapabilityTableDDL},
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN note text"},
			afterExpandChecks: []string{"SELECT count(*) = 0 AND count(note) = 0 FROM $schema.items"},
		},
		{
			variantID: "additive.null-seed-preserved", schema: "pf_add_null_seed", nativeOwnerID: owner,
			invariant: "an existing NULL payload remains distinct from the newly added column", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, payload text)", "INSERT INTO $schema.items VALUES (1, NULL)"},
			prepared:          prepared("pf_add_null_seed_old", "INSERT INTO $schema.items (id, payload) VALUES ($1, NULL) RETURNING payload IS NULL", int64(2)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN note text"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (3, 'payload', 'note')"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE payload IS NULL AND note IS NULL) = 2 AND count(note) = 1 FROM $schema.items"},
		},
		{
			variantID: "additive.constant-text-default", schema: "pf_add_text_default", nativeOwnerID: owner,
			invariant: "a legacy insert receives the declared text default while a new writer can override it", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_text_default_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN state text DEFAULT 'queued'"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (2, 'sent')"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE state = 'queued') = 1 AND count(*) FILTER (WHERE state = 'sent') = 1 FROM $schema.items"},
		},
		{
			variantID: "additive.numeric-zero-default", schema: "pf_add_zero_default", nativeOwnerID: owner,
			invariant: "numeric zero remains a real default rather than being confused with NULL", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_zero_default_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN attempts integer DEFAULT 0"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (2, 3)"},
			afterExpandChecks: []string{"SELECT min(attempts) = 0 AND max(attempts) = 3 AND count(attempts) = 2 FROM $schema.items"},
		},
		{
			variantID: "additive.boolean-false-default", schema: "pf_add_false_default", nativeOwnerID: owner,
			invariant: "a false default survives legacy omission and differs from an explicit true value", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_false_default_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN archived boolean DEFAULT false"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (2, true)"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE NOT archived) = 1 AND count(*) FILTER (WHERE archived) = 1 FROM $schema.items"},
		},
		{
			variantID: "additive.expression-date-default", schema: "pf_add_date_default", nativeOwnerID: owner,
			invariant: "a stable SQL expression supplies legacy rows while explicit dates remain accepted", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_date_default_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN created_on date DEFAULT CURRENT_DATE"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (2, DATE '2000-01-01')"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE created_on = CURRENT_DATE) = 1 AND min(created_on) = DATE '2000-01-01' FROM $schema.items"},
		},
		{
			variantID: "additive.explicit-null-new-writer", schema: "pf_add_explicit_null", nativeOwnerID: owner,
			invariant: "an explicit new-writer NULL and a legacy omission are both valid for a nullable addition", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_explicit_null_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN note text"},
			afterExpand:       []string{"INSERT INTO $schema.items (id, note) VALUES (2, NULL), (3, 'set')"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE note IS NULL) = 2 AND count(*) FILTER (WHERE note = 'set') = 1 FROM $schema.items"},
		},
		{
			variantID: "additive.multirow-legacy-batch", schema: "pf_add_legacy_batch", nativeOwnerID: owner,
			invariant: "one prepared legacy statement can insert several rows after an additive change", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_legacy_batch_old", "INSERT INTO $schema.items (id) SELECT value FROM unnest($1::bigint[]) AS value RETURNING id > 0", []int64{1, 2, 3}),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN note text"},
			afterExpandChecks: []string{"SELECT count(*) = 3 AND count(note) = 0 FROM $schema.items"},
		},
		{
			variantID: "additive.two-column-shape", schema: "pf_add_two_columns", nativeOwnerID: owner,
			invariant: "a legacy writer ignores two independent additions while a new writer fills both", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_two_columns_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN note text, ADD COLUMN priority integer"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (2, 'set', 7)"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE note IS NULL AND priority IS NULL) = 1 AND max(priority) = 7 FROM $schema.items"},
		},
		{
			variantID: "additive.quoted-column-name", schema: "pf_add_quoted_column", nativeOwnerID: owner,
			invariant: "a reserved quoted column name remains addressable by new SQL without changing legacy SQL", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_quoted_column_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{`ALTER TABLE $schema.items ADD COLUMN "group" text`},
			afterExpand:       []string{`INSERT INTO $schema.items (id, "group") VALUES (2, 'staff')`},
			afterExpandChecks: []string{`SELECT count(*) FILTER (WHERE "group" IS NULL) = 1 AND max("group") = 'staff' FROM $schema.items`},
		},
		{
			variantID: "additive.array-default", schema: "pf_add_array_default", nativeOwnerID: owner,
			invariant: "an empty-array default is distinguishable from a populated new-writer array", requires: capabilities,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY)"},
			prepared:          prepared("pf_add_array_default_old", "INSERT INTO $schema.items (id) VALUES ($1) RETURNING id = $1", int64(1)),
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN tags text[] DEFAULT ARRAY[]::text[]"},
			afterExpand:       []string{"INSERT INTO $schema.items VALUES (2, ARRAY['new'])"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE cardinality(tags) = 0) = 1 AND count(*) FILTER (WHERE tags = ARRAY['new']) = 1 FROM $schema.items"},
		},
		{
			variantID: "additive.preexisting-row-backfill-free", schema: "pf_add_preexisting", nativeOwnerID: owner,
			invariant: "a nullable addition leaves every preexisting row untouched until product code chooses otherwise", requires: []testkit.Capability{testkit.CapabilityTableDDL},
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, payload text)", "INSERT INTO $schema.items SELECT value, 'p-' || value::text FROM generate_series(1, 5) AS value"},
			expand:            []string{"ALTER TABLE $schema.items ADD COLUMN note text"},
			afterExpandChecks: []string{"SELECT count(*) = 5 AND count(note) = 0 AND min(payload) = 'p-1' FROM $schema.items"},
		},
	})
}

func requiredPGliteVariants() []pglitePreflightCase {
	owner := "native.required-column"
	capabilities := []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityExplicitPrepareSameBackend}
	prepared := func(name string, args ...any) *preparedBoolean {
		return &preparedBoolean{name: name, sql: "INSERT INTO $schema.items (id, source) VALUES ($1, $2) RETURNING id = $1", args: args}
	}
	return materializePGliteCases([]pgliteCaseTemplate{
		{
			variantID: "required.all-null-text-cleanup", schema: "pf_req_all_null", nativeOwnerID: owner,
			invariant: "an all-NULL overlap is made required only after explicit product cleanup", requires: capabilities,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, 'a'), (2, 'b')"},
			prepared: prepared("pf_req_all_null_old", int64(3), "c"), expand: []string{"ALTER TABLE $schema.items ADD COLUMN state text"},
			afterExpandChecks:   []string{"SELECT count(*) = 3 AND count(state) = 0 FROM $schema.items"},
			contract:            []string{"UPDATE $schema.items SET state = 'pending' WHERE state IS NULL", "ALTER TABLE $schema.items ALTER COLUMN state SET NOT NULL"},
			afterContractChecks: []string{"SELECT count(*) = 3 AND bool_and(state = 'pending') FROM $schema.items"},
		},
		{
			variantID: "required.mixed-source-derived", schema: "pf_req_source", nativeOwnerID: owner,
			invariant: "cleanup may derive required values from existing product data without a schema default", requires: capabilities,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, 'import'), (2, 'api')"},
			prepared: prepared("pf_req_source_old", int64(3), "legacy"), expand: []string{"ALTER TABLE $schema.items ADD COLUMN state text"},
			afterExpand:         []string{"UPDATE $schema.items SET state = 'confirmed' WHERE id = 2"},
			afterExpandChecks:   []string{"SELECT count(*) FILTER (WHERE state IS NULL) = 2 AND count(*) FILTER (WHERE state = 'confirmed') = 1 FROM $schema.items"},
			contract:            []string{"UPDATE $schema.items SET state = 'from-' || source WHERE state IS NULL", "ALTER TABLE $schema.items ALTER COLUMN state SET NOT NULL"},
			afterContractChecks: []string{"SELECT count(*) FILTER (WHERE state LIKE 'from-%') = 2 AND count(*) FILTER (WHERE state = 'confirmed') = 1 FROM $schema.items"},
		},
		{
			variantID: "required.already-clean-before-contract", schema: "pf_req_clean", nativeOwnerID: owner,
			invariant: "already-clean overlap data needs no invented backfill before NOT NULL", requires: []testkit.Capability{testkit.CapabilityTableDDL},
			setup:  []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, 'a'), (2, 'b')"},
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN state text"}, afterExpand: []string{"UPDATE $schema.items SET state = upper(source)"},
			afterExpandChecks:   []string{"SELECT count(*) = count(state) AND min(state) = 'A' FROM $schema.items"},
			contract:            []string{"ALTER TABLE $schema.items ALTER COLUMN state SET NOT NULL"},
			afterContractChecks: []string{"SELECT bool_and(state IN ('A', 'B')) FROM $schema.items"},
		},
		{
			variantID: "required.numeric-zero-cleanup", schema: "pf_req_zero", nativeOwnerID: owner,
			invariant: "zero is preserved as a deliberate numeric cleanup value before NOT NULL", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, 'a')"}, prepared: prepared("pf_req_zero_old", int64(2), "b"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN attempts integer"}, contract: []string{"UPDATE $schema.items SET attempts = 0 WHERE attempts IS NULL", "ALTER TABLE $schema.items ALTER COLUMN attempts SET NOT NULL"},
			afterContractChecks: []string{"SELECT count(*) = 2 AND sum(attempts) = 0 AND count(attempts) = 2 FROM $schema.items"},
		},
		{
			variantID: "required.boolean-false-cleanup", schema: "pf_req_false", nativeOwnerID: owner,
			invariant: "false is preserved as a deliberate boolean cleanup value before NOT NULL", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, 'a')"}, prepared: prepared("pf_req_false_old", int64(2), "b"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN archived boolean"}, afterExpand: []string{"UPDATE $schema.items SET archived = true WHERE id = 1"},
			contract:            []string{"UPDATE $schema.items SET archived = false WHERE archived IS NULL", "ALTER TABLE $schema.items ALTER COLUMN archived SET NOT NULL"},
			afterContractChecks: []string{"SELECT count(*) FILTER (WHERE archived) = 1 AND count(*) FILTER (WHERE NOT archived) = 1 FROM $schema.items"},
		},
		{
			variantID: "required.blank-string-normalization", schema: "pf_req_blank", nativeOwnerID: owner,
			invariant: "blank and NULL source strings can receive different explicit product cleanup values", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, ''), (2, NULL)"}, prepared: prepared("pf_req_blank_old", int64(3), "value"),
			expand:              []string{"ALTER TABLE $schema.items ADD COLUMN state text"},
			contract:            []string{"UPDATE $schema.items SET state = CASE WHEN source = '' THEN 'blank' WHEN source IS NULL THEN 'missing' ELSE 'present' END", "ALTER TABLE $schema.items ALTER COLUMN state SET NOT NULL"},
			afterContractChecks: []string{"SELECT count(*) FILTER (WHERE state = 'blank') = 1 AND count(*) FILTER (WHERE state = 'missing') = 1 AND count(*) FILTER (WHERE state = 'present') = 1 FROM $schema.items"},
		},
		{
			variantID: "required.quoted-column-contract", schema: "pf_req_quoted", nativeOwnerID: owner,
			invariant: "quoted required-column cleanup and contract address the exact identifier", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)"}, prepared: prepared("pf_req_quoted_old", int64(1), "legacy"),
			expand: []string{`ALTER TABLE $schema.items ADD COLUMN "order" text`}, contract: []string{`UPDATE $schema.items SET "order" = 'first' WHERE "order" IS NULL`, `ALTER TABLE $schema.items ALTER COLUMN "order" SET NOT NULL`},
			afterContractChecks: []string{`SELECT "order" = 'first' FROM $schema.items WHERE id = 1`},
		},
		{
			variantID: "required.date-derived-cleanup", schema: "pf_req_date", nativeOwnerID: owner,
			invariant: "a required date can be derived from existing source data rather than a schema default", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, '2001-02-03')"}, prepared: prepared("pf_req_date_old", int64(2), "2002-03-04"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN occurred_on date"}, contract: []string{"UPDATE $schema.items SET occurred_on = source::date", "ALTER TABLE $schema.items ALTER COLUMN occurred_on SET NOT NULL"},
			afterContractChecks: []string{"SELECT min(occurred_on) = DATE '2001-02-03' AND max(occurred_on) = DATE '2002-03-04' FROM $schema.items"},
		},
		{
			variantID: "required.json-object-cleanup", schema: "pf_req_json", nativeOwnerID: owner,
			invariant: "required JSON cleanup preserves structured values for legacy rows", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)"}, prepared: prepared("pf_req_json_old", int64(1), "legacy"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN metadata jsonb"}, contract: []string{`UPDATE $schema.items SET metadata = jsonb_build_object('source', source)`, "ALTER TABLE $schema.items ALTER COLUMN metadata SET NOT NULL"},
			afterContractChecks: []string{"SELECT metadata->>'source' = 'legacy' FROM $schema.items WHERE id = 1"},
		},
		{
			variantID: "required.array-cleanup", schema: "pf_req_array", nativeOwnerID: owner,
			invariant: "an empty required array is a deliberate product value, not an omitted cleanup", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)"}, prepared: prepared("pf_req_array_old", int64(1), "legacy"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN tags text[]"}, contract: []string{"UPDATE $schema.items SET tags = ARRAY[]::text[]", "ALTER TABLE $schema.items ALTER COLUMN tags SET NOT NULL"},
			afterContractChecks: []string{"SELECT cardinality(tags) = 0 FROM $schema.items WHERE id = 1"},
		},
		{
			variantID: "required.category-sensitive-cleanup", schema: "pf_req_category", nativeOwnerID: owner,
			invariant: "mixed legacy categories receive category-specific required values", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)", "INSERT INTO $schema.items VALUES (1, 'vip'), (2, 'standard'), (3, 'trial')"}, prepared: prepared("pf_req_category_old", int64(4), "standard"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN priority integer"}, contract: []string{"UPDATE $schema.items SET priority = CASE source WHEN 'vip' THEN 100 WHEN 'standard' THEN 50 ELSE 10 END", "ALTER TABLE $schema.items ALTER COLUMN priority SET NOT NULL"},
			afterContractChecks: []string{"SELECT min(priority) = 10 AND max(priority) = 100 AND count(*) FILTER (WHERE priority = 50) = 2 FROM $schema.items"},
		},
		{
			variantID: "required.explicit-null-legacy-write", schema: "pf_req_explicit_null", nativeOwnerID: owner,
			invariant: "an explicit legacy NULL remains allowed during expand and is visible to cleanup", requires: capabilities,
			setup: []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, source text)"}, prepared: prepared("pf_req_explicit_null_old", int64(1), "legacy"),
			expand: []string{"ALTER TABLE $schema.items ADD COLUMN state text"}, afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 'new', NULL)"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE state IS NULL) = 2 FROM $schema.items"}, contract: []string{"UPDATE $schema.items SET state = 'recovered' WHERE state IS NULL", "ALTER TABLE $schema.items ALTER COLUMN state SET NOT NULL"},
			afterContractChecks: []string{"SELECT bool_and(state = 'recovered') FROM $schema.items"},
		},
	})
}

func checkPGliteVariants() []pglitePreflightCase {
	owner := "native.check-widening"
	requires := []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityCheckConstraint, testkit.CapabilityExplicitPrepareSameBackend}
	prepared := func(name, sql string, args ...any) *preparedBoolean {
		return &preparedBoolean{name: name, sql: sql, args: args}
	}
	return materializePGliteCases([]pgliteCaseTemplate{
		{
			variantID: "check.named-reordered-values", schema: "pf_check_reordered", nativeOwnerID: owner,
			invariant: "reordering old values while appending a new value preserves the same accepted set plus the addition", requires: requires,
			setup:               []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, state text CONSTRAINT state_check CHECK (state IN ('a', 'b')))"},
			prepared:            prepared("pf_check_reordered_old", "INSERT INTO $schema.items VALUES ($1, 'a') RETURNING state = 'a'", int64(1)),
			expand:              []string{"ALTER TABLE $schema.items DROP CONSTRAINT state_check, ADD CONSTRAINT state_check CHECK (state IN ('b', 'c', 'a'))"},
			afterExpand:         []string{"INSERT INTO $schema.items VALUES (2, 'b'), (3, 'c')"},
			afterExpandChecks:   []string{"SELECT array_agg(state ORDER BY state) = ARRAY['a','b','c']::text[] FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (4, 'd')"},
		},
		{
			variantID: "check.unnamed-column-widen", schema: "pf_check_unnamed", nativeOwnerID: owner,
			invariant: "an automatically named column CHECK can be replaced with a wider acceptance envelope", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, state text CHECK (state = 'old'))"},
			prepared:    prepared("pf_check_unnamed_old", "INSERT INTO $schema.items VALUES ($1, 'old') RETURNING state = 'old'", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT items_state_check, ADD CHECK (state IN ('old', 'new'))"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 'new')"}, afterExpandChecks: []string{"SELECT count(DISTINCT state) = 2 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, 'other')"},
		},
		{
			variantID: "check.numeric-upper-boundary", schema: "pf_check_upper", nativeOwnerID: owner,
			invariant: "raising a numeric upper bound accepts the new edge while retaining the old edge", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, value integer CONSTRAINT value_check CHECK (value <= 10))"},
			prepared:    prepared("pf_check_upper_old", "INSERT INTO $schema.items VALUES ($1, 10) RETURNING value = 10", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (value <= 20)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 20)"}, afterExpandChecks: []string{"SELECT min(value) = 10 AND max(value) = 20 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, 21)"},
		},
		{
			variantID: "check.two-sided-range-widen", schema: "pf_check_range", nativeOwnerID: owner,
			invariant: "both new range boundaries are accepted without losing an interior legacy value", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, value integer CONSTRAINT value_check CHECK (value BETWEEN 0 AND 10))"},
			prepared:    prepared("pf_check_range_old", "INSERT INTO $schema.items VALUES ($1, 5) RETURNING value = 5", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (value BETWEEN -5 AND 15)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, -5), (3, 15)"}, afterExpandChecks: []string{"SELECT min(value) = -5 AND max(value) = 15 AND count(*) = 3 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (4, 16)"},
		},
		{
			variantID: "check.null-remains-unconstrained", schema: "pf_check_null", nativeOwnerID: owner,
			invariant: "CHECK widening continues to permit NULL independently of old and new concrete values", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, value integer CONSTRAINT value_check CHECK (value >= 0))"},
			prepared:    prepared("pf_check_null_old", "INSERT INTO $schema.items VALUES ($1, NULL) RETURNING value IS NULL", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (value >= -1)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, -1), (3, NULL)"}, afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE value IS NULL) = 2 AND min(value) = -1 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (4, -2)"},
		},
		{
			variantID: "check.quoted-constraint-name", schema: "pf_check_quoted", nativeOwnerID: owner,
			invariant: "a quoted CHECK identity is replaced exactly and accepts the quoted transition values", requires: requires,
			setup:       []string{`CREATE SCHEMA $schema`, `CREATE TABLE $schema.items (id bigint PRIMARY KEY, state text CONSTRAINT "State Check" CHECK (state = 'old'))`},
			prepared:    prepared("pf_check_quoted_old", "INSERT INTO $schema.items VALUES ($1, 'old') RETURNING state = 'old'", int64(1)),
			expand:      []string{`ALTER TABLE $schema.items DROP CONSTRAINT "State Check", ADD CONSTRAINT "State Check" CHECK (state IN ('old', 'new'))`},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 'new')"}, afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE state = 'new') = 1 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, 'bad')"},
		},
		{
			variantID: "check.text-length-boundary", schema: "pf_check_length", nativeOwnerID: owner,
			invariant: "raising a text-length limit accepts the exact new length and retains shorter legacy strings", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, value text CONSTRAINT value_check CHECK (length(value) <= 3))"},
			prepared:    prepared("pf_check_length_old", "INSERT INTO $schema.items VALUES ($1, 'abc') RETURNING value = 'abc'", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (length(value) <= 5)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 'abcde')"}, afterExpandChecks: []string{"SELECT min(length(value)) = 3 AND max(length(value)) = 5 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, 'abcdef')"},
		},
		{
			variantID: "check.multi-column-envelope", schema: "pf_check_multi", nativeOwnerID: owner,
			invariant: "a multi-column widening admits the new relationship without weakening its outer bound", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, low integer, high integer, CONSTRAINT bounds_check CHECK (low <= high AND high - low <= 10))"},
			prepared:    prepared("pf_check_multi_old", "INSERT INTO $schema.items VALUES ($1, 0, 10) RETURNING high - low = 10", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT bounds_check, ADD CONSTRAINT bounds_check CHECK (low <= high AND high - low <= 20)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, -5, 15)"}, afterExpandChecks: []string{"SELECT max(high - low) = 20 AND min(high - low) = 10 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, 0, 21)"},
		},
		{
			variantID: "check.boolean-implication-widen", schema: "pf_check_boolean", nativeOwnerID: owner,
			invariant: "a boolean implication can admit draft rows while retaining enforcement for published rows", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, published boolean, title text, CONSTRAINT publish_check CHECK (published AND title IS NOT NULL))"},
			prepared:    prepared("pf_check_boolean_old", "INSERT INTO $schema.items VALUES ($1, true, 'title') RETURNING published", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT publish_check, ADD CONSTRAINT publish_check CHECK (NOT published OR title IS NOT NULL)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, false, NULL)"}, afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE published) = 1 AND count(*) FILTER (WHERE NOT published AND title IS NULL) = 1 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, true, NULL)"},
		},
		{
			variantID: "check.decimal-scale-boundary", schema: "pf_check_decimal", nativeOwnerID: owner,
			invariant: "a widened decimal bound accepts the precise new fractional edge", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, value numeric(6,2) CONSTRAINT value_check CHECK (value <= 10.50))"},
			prepared:    prepared("pf_check_decimal_old", "INSERT INTO $schema.items VALUES ($1, 10.50) RETURNING value = 10.50", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (value <= 20.75)"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 20.75)"}, afterExpandChecks: []string{"SELECT max(value) = 20.75 AND min(value) = 10.50 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, 20.76)"},
		},
		{
			variantID: "check.not-valid-new-envelope", schema: "pf_check_not_valid", nativeOwnerID: owner,
			invariant: "a NOT VALID replacement still enforces the wider rule for every new write", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, value integer CONSTRAINT value_check CHECK (value > 0))"},
			prepared:    prepared("pf_check_not_valid_old", "INSERT INTO $schema.items VALUES ($1, 1) RETURNING value = 1", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT value_check, ADD CONSTRAINT value_check CHECK (value >= 0) NOT VALID"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 0)"}, afterExpandChecks: []string{"SELECT min(value) = 0 AND max(value) = 1 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (3, -1)"},
		},
		{
			variantID: "check.null-or-new-value", schema: "pf_check_null_or", nativeOwnerID: owner,
			invariant: "an explicit NULL-or-value CHECK widens only its concrete branch", requires: requires,
			setup:       []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint PRIMARY KEY, state text CONSTRAINT state_check CHECK (state IS NULL OR state = 'old'))"},
			prepared:    prepared("pf_check_null_or_old", "INSERT INTO $schema.items VALUES ($1, NULL) RETURNING state IS NULL", int64(1)),
			expand:      []string{"ALTER TABLE $schema.items DROP CONSTRAINT state_check, ADD CONSTRAINT state_check CHECK (state IS NULL OR state IN ('old', 'new'))"},
			afterExpand: []string{"INSERT INTO $schema.items VALUES (2, 'old'), (3, 'new')"}, afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE state IS NULL) = 1 AND count(DISTINCT state) = 2 FROM $schema.items"},
			rejectedAfterExpand: []string{"INSERT INTO $schema.items VALUES (4, 'bad')"},
		},
	})
}

func renamePGliteVariants() []pglitePreflightCase {
	owner := "native.column-rename-overlap"
	requires := []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityTrigger, testkit.CapabilityExplicitPrepareSameBackend}
	prepared := func(name, sql string, args ...any) *preparedBoolean {
		return &preparedBoolean{name: name, sql: sql, args: args}
	}
	contractAfter := func(prefix ...string) []string { return append(prefix, renameContractSQL("$schema")...) }
	return materializePGliteCases([]pgliteCaseTemplate{
		{
			variantID: "rename.empty-table-cutover", schema: "pf_rename_empty", nativeOwnerID: owner,
			invariant: "an empty table can install and remove the bridge without manufacturing data", requires: []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityTrigger},
			setup:  []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)"},
			expand: renameExpandSQL("$schema"), afterExpandChecks: []string{"SELECT count(*) = 0 FROM $schema.people"},
			contract: renameContractSQL("$schema"), afterContract: []string{"INSERT INTO $schema.people VALUES (1, 'after')"},
			afterContractChecks: []string{"SELECT new_name = 'after' FROM $schema.people"},
		},
		{
			variantID: "rename.matching-dual-insert", schema: "pf_rename_dual_insert", nativeOwnerID: owner,
			invariant: "a new client supplying matching old and new names is accepted during overlap", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)"},
			prepared: prepared("pf_rename_dual_insert_old", "INSERT INTO $schema.people (id, old_name) VALUES ($1, $2) RETURNING old_name = $2", int64(1), "legacy"),
			expand:   renameExpandSQL("$schema"), afterExpand: []string{"INSERT INTO $schema.people (id, old_name, new_name) VALUES (2, 'match', 'match')"},
			afterExpandChecks: []string{"SELECT count(*) = 2 AND bool_and(old_name = new_name) FROM $schema.people"}, contract: renameContractSQL("$schema"),
			afterContractChecks: []string{"SELECT count(*) FILTER (WHERE new_name = 'match') = 1 FROM $schema.people"},
		},
		{
			variantID: "rename.conflicting-dual-insert", schema: "pf_rename_conflict_insert", nativeOwnerID: owner,
			invariant: "a client supplying contradictory old and new insert values is rejected", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)"},
			prepared: prepared("pf_rename_conflict_insert_old", "INSERT INTO $schema.people (id, old_name) VALUES ($1, $2) RETURNING old_name = $2", int64(1), "legacy"),
			expand:   renameExpandSQL("$schema"), rejectedAfterExpand: []string{"INSERT INTO $schema.people (id, old_name, new_name) VALUES (2, 'left', 'right')"},
			afterExpandChecks: []string{"SELECT count(*) = 1 AND bool_and(old_name = new_name) FROM $schema.people"}, contract: renameContractSQL("$schema"),
		},
		{
			variantID: "rename.matching-dual-update", schema: "pf_rename_dual_update", nativeOwnerID: owner,
			invariant: "a client updating both names to the same value remains deterministic", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO $schema.people VALUES (1, 'before')"},
			prepared: prepared("pf_rename_dual_update_old", "UPDATE $schema.people SET old_name = $2 WHERE id = $1 RETURNING old_name = $2", int64(1), "legacy"),
			expand:   renameExpandSQL("$schema"), afterExpand: []string{"UPDATE $schema.people SET old_name = 'match', new_name = 'match' WHERE id = 1"},
			afterExpandChecks: []string{"SELECT old_name = 'match' AND new_name = 'match' FROM $schema.people"}, contract: renameContractSQL("$schema"),
		},
		{
			variantID: "rename.conflicting-dual-update", schema: "pf_rename_conflict_update", nativeOwnerID: owner,
			invariant: "a client updating the two names to contradictory values is rejected without corrupting the row", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO $schema.people VALUES (1, 'before')"},
			prepared: prepared("pf_rename_conflict_update_old", "UPDATE $schema.people SET old_name = $2 WHERE id = $1 RETURNING old_name = $2", int64(1), "legacy"),
			expand:   renameExpandSQL("$schema"), rejectedAfterExpand: []string{"UPDATE $schema.people SET old_name = 'left', new_name = 'right' WHERE id = 1"},
			afterExpandChecks: []string{"SELECT old_name = 'legacy' AND new_name = 'legacy' FROM $schema.people"}, contract: renameContractSQL("$schema"),
		},
		{
			variantID: "rename.blank-string-value", schema: "pf_rename_blank", nativeOwnerID: owner,
			invariant: "an empty string is synchronized as a value rather than mistaken for absence", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO $schema.people VALUES (1, 'before')"},
			prepared: prepared("pf_rename_blank_old", "UPDATE $schema.people SET old_name = $2 WHERE id = $1 RETURNING old_name = $2", int64(1), ""),
			expand:   renameExpandSQL("$schema"), afterExpandChecks: []string{"SELECT old_name = '' AND new_name = '' FROM $schema.people"}, contract: renameContractSQL("$schema"),
			afterContractChecks: []string{"SELECT length(new_name) = 0 FROM $schema.people"},
		},
		{
			variantID: "rename.null-existing-product-cleanup", schema: "pf_rename_null_existing", nativeOwnerID: owner,
			invariant: "an existing NULL remains visible through the bridge until product cleanup supplies the required value", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text)", "INSERT INTO $schema.people VALUES (1, NULL)"},
			prepared: prepared("pf_rename_null_existing_old", "INSERT INTO $schema.people (id, old_name) VALUES ($1, NULL) RETURNING old_name IS NULL", int64(2)),
			expand:   renameExpandSQL("$schema"), afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE old_name IS NULL AND new_name IS NULL) = 2 FROM $schema.people"},
			contract:            contractAfter("UPDATE $schema.people SET old_name = 'unknown', new_name = 'unknown' WHERE new_name IS NULL"),
			afterContractChecks: []string{"SELECT count(*) = 2 AND bool_and(new_name = 'unknown') FROM $schema.people"},
		},
		{
			variantID: "rename.multirow-legacy-update", schema: "pf_rename_legacy_batch", nativeOwnerID: owner,
			invariant: "one legacy update synchronizes every affected row, not only the first", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO $schema.people VALUES (1, 'a'), (2, 'b'), (3, 'c')"},
			prepared: prepared("pf_rename_legacy_batch_old", "WITH changed AS (UPDATE $schema.people SET old_name = $1 RETURNING old_name) SELECT count(*) = 3 FROM changed", "legacy-batch"),
			expand:   renameExpandSQL("$schema"), afterExpandChecks: []string{"SELECT count(*) = 3 AND bool_and(old_name = 'legacy-batch' AND new_name = old_name) FROM $schema.people"}, contract: renameContractSQL("$schema"),
		},
		{
			variantID: "rename.multirow-new-update", schema: "pf_rename_new_batch", nativeOwnerID: owner,
			invariant: "one new-client update synchronizes every affected legacy projection", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text NOT NULL)", "INSERT INTO $schema.people VALUES (1, 'a'), (2, 'b'), (3, 'c')"},
			prepared: prepared("pf_rename_new_batch_old", "SELECT count(*) = 3 FROM $schema.people"), expand: renameExpandSQL("$schema"),
			afterExpand: []string{"UPDATE $schema.people SET new_name = 'new-batch'"}, afterExpandChecks: []string{"SELECT count(*) = 3 AND bool_and(new_name = 'new-batch' AND old_name = new_name) FROM $schema.people"}, contract: renameContractSQL("$schema"),
		},
		{
			variantID: "rename.new-only-insert-nullable-source", schema: "pf_rename_new_nullable", nativeOwnerID: owner,
			invariant: "a new-only insert backfills a nullable legacy column through the bridge", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text)"},
			prepared: prepared("pf_rename_new_nullable_old", "SELECT count(*) = 0 FROM $schema.people"), expand: renameExpandSQL("$schema"),
			afterExpand: []string{"INSERT INTO $schema.people (id, new_name) VALUES (1, 'new-only')"}, afterExpandChecks: []string{"SELECT old_name = 'new-only' AND new_name = old_name FROM $schema.people"},
			contract: renameContractSQL("$schema"), afterContractChecks: []string{"SELECT new_name = 'new-only' FROM $schema.people"},
		},
		{
			variantID: "rename.old-only-null-preserved", schema: "pf_rename_old_null", nativeOwnerID: owner,
			invariant: "an old-only explicit NULL remains a paired NULL until the product resolves it", requires: requires,
			setup:    []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.people (id bigint PRIMARY KEY, old_name text)"},
			prepared: prepared("pf_rename_old_null_old", "INSERT INTO $schema.people (id, old_name) VALUES ($1, NULL) RETURNING old_name IS NULL", int64(1)), expand: renameExpandSQL("$schema"),
			afterExpandChecks: []string{"SELECT old_name IS NULL AND new_name IS NULL FROM $schema.people"},
			contract:          contractAfter("UPDATE $schema.people SET old_name = 'resolved', new_name = 'resolved' WHERE new_name IS NULL"), afterContractChecks: []string{"SELECT new_name = 'resolved' FROM $schema.people"},
		},
		{
			variantID: "rename.quoted-column-identifiers", schema: "pf_rename_quoted", nativeOwnerID: owner,
			invariant: "quoted legacy and new column identifiers synchronize and contract exactly", requires: requires,
			setup:    []string{`CREATE SCHEMA $schema`, `CREATE TABLE $schema.people (id bigint PRIMARY KEY, "old name" text NOT NULL)`, `INSERT INTO $schema.people VALUES (1, 'before')`},
			prepared: prepared("pf_rename_quoted_old", `UPDATE $schema.people SET "old name" = $2 WHERE id = $1 RETURNING "old name" = $2`, int64(1), "legacy"),
			expand:   quotedRenameExpandSQL("$schema"), afterExpand: []string{`INSERT INTO $schema.people (id, "new name") VALUES (2, 'modern')`},
			afterExpandChecks: []string{`SELECT count(*) = 2 AND bool_and("old name" = "new name") FROM $schema.people`}, contract: quotedRenameContractSQL("$schema"),
			afterContractChecks: []string{`SELECT array_agg("new name" ORDER BY id) = ARRAY['legacy','modern']::text[] FROM $schema.people`},
		},
	})
}

func viewPGliteVariants() []pglitePreflightCase {
	owner := "native.view-replacement"
	requires := []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityView, testkit.CapabilityExplicitPrepareSameBackend}
	prepared := func(name, sql string, args ...any) *preparedBoolean {
		return &preparedBoolean{name: name, sql: sql, args: args}
	}
	return materializePGliteCases([]pgliteCaseTemplate{
		{
			variantID: "view.empty-append-output", schema: "pf_view_empty", nativeOwnerID: owner,
			invariant: "an appended output preserves an empty legacy projection", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "CREATE VIEW $schema.item_view AS SELECT id FROM $schema.items"},
			prepared:          prepared("pf_view_empty_old", "SELECT count(*) = 0 FROM $schema.item_view"),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, state FROM $schema.items"},
			afterExpandChecks: []string{"SELECT count(*) = 0 AND count(state) = 0 FROM $schema.item_view"},
		},
		{
			variantID: "view.quoted-output-alias", schema: "pf_view_quoted_alias", nativeOwnerID: owner,
			invariant: "a quoted appended alias is available without changing the legacy output name", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "INSERT INTO $schema.items VALUES (1, 'ready')", `CREATE VIEW $schema.item_view AS SELECT id AS "order" FROM $schema.items`},
			prepared:          prepared("pf_view_quoted_alias_old", `SELECT "order" = $1 FROM $schema.item_view`, int64(1)),
			expand:            []string{`CREATE OR REPLACE VIEW $schema.item_view AS SELECT id AS "order", state AS "select" FROM $schema.items`},
			afterExpandChecks: []string{`SELECT "order" = 1 AND "select" = 'ready' FROM $schema.item_view`},
		},
		{
			variantID: "view.two-appended-outputs", schema: "pf_view_two_outputs", nativeOwnerID: owner,
			invariant: "two appended outputs leave the first legacy output in its original position", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text, priority integer)", "INSERT INTO $schema.items VALUES (1, 'ready', 7)", "CREATE VIEW $schema.item_view AS SELECT id FROM $schema.items"},
			prepared:          prepared("pf_view_two_outputs_old", "SELECT id = $1 FROM $schema.item_view", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, state, priority FROM $schema.items"},
			afterExpandChecks: []string{"SELECT id = 1 AND state = 'ready' AND priority = 7 FROM $schema.item_view"},
		},
		{
			variantID: "view.append-expression-output", schema: "pf_view_expression", nativeOwnerID: owner,
			invariant: "a computed appended output does not alter the legacy base projection", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, first_name text, last_name text)", "INSERT INTO $schema.items VALUES (1, 'Ada', 'Lovelace')", "CREATE VIEW $schema.item_view AS SELECT id FROM $schema.items"},
			prepared:          prepared("pf_view_expression_old", "SELECT id = $1 FROM $schema.item_view", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, first_name || ' ' || last_name AS full_name FROM $schema.items"},
			afterExpandChecks: []string{"SELECT id = 1 AND full_name = 'Ada Lovelace' FROM $schema.item_view"},
		},
		{
			variantID: "view.append-null-output", schema: "pf_view_null", nativeOwnerID: owner,
			invariant: "an appended nullable output preserves both legacy rows and NULL identity", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, note text)", "INSERT INTO $schema.items VALUES (1, NULL), (2, 'set')", "CREATE VIEW $schema.item_view AS SELECT id FROM $schema.items"},
			prepared:          prepared("pf_view_null_old", "SELECT count(*) = 2 FROM $schema.item_view"),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, note FROM $schema.items"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE note IS NULL) = 1 AND count(*) FILTER (WHERE note = 'set') = 1 FROM $schema.item_view"},
		},
		{
			variantID: "view.three-level-projection-chain", schema: "pf_view_depth_three", nativeOwnerID: owner,
			invariant: "two downstream legacy projections survive an append at the base view", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "INSERT INTO $schema.items VALUES (1, 'ready')", "CREATE VIEW $schema.level_one AS SELECT id FROM $schema.items", "CREATE VIEW $schema.level_two AS SELECT id FROM $schema.level_one", "CREATE VIEW $schema.level_three AS SELECT id FROM $schema.level_two"},
			prepared:          prepared("pf_view_depth_three_old", "SELECT id = $1 FROM $schema.level_three", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.level_one AS SELECT id, state FROM $schema.items"},
			afterExpandChecks: []string{"SELECT id = 1 FROM $schema.level_three", "SELECT state = 'ready' FROM $schema.level_one"},
		},
		{
			variantID: "view.join-append-output", schema: "pf_view_join", nativeOwnerID: owner,
			invariant: "an appended join output keeps the legacy primary-table identifier stable", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.accounts (id bigint, name text)", "CREATE TABLE $schema.items (id bigint, account_id bigint)", "INSERT INTO $schema.accounts VALUES (10, 'Acme')", "INSERT INTO $schema.items VALUES (1, 10)", "CREATE VIEW $schema.item_view AS SELECT i.id FROM $schema.items i JOIN $schema.accounts a ON a.id = i.account_id"},
			prepared:          prepared("pf_view_join_old", "SELECT id = $1 FROM $schema.item_view", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT i.id, a.name AS account_name FROM $schema.items i JOIN $schema.accounts a ON a.id = i.account_id"},
			afterExpandChecks: []string{"SELECT id = 1 AND account_name = 'Acme' FROM $schema.item_view"},
		},
		{
			variantID: "view.filtered-projection-append", schema: "pf_view_filtered", nativeOwnerID: owner,
			invariant: "appending an output does not broaden a legacy view predicate", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, active boolean, state text)", "INSERT INTO $schema.items VALUES (1, true, 'visible'), (2, false, 'hidden')", "CREATE VIEW $schema.item_view AS SELECT id FROM $schema.items WHERE active"},
			prepared:          prepared("pf_view_filtered_old", "SELECT count(*) = 1 AND min(id) = $1 FROM $schema.item_view", int64(1)),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, state FROM $schema.items WHERE active"},
			afterExpandChecks: []string{"SELECT count(*) = 1 AND min(state) = 'visible' FROM $schema.item_view"},
		},
		{
			variantID: "view.aggregate-append-count", schema: "pf_view_aggregate", nativeOwnerID: owner,
			invariant: "an aggregate count can be appended after the stable grouping output", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "INSERT INTO $schema.items VALUES (1, 'ready'), (2, 'ready')", "CREATE VIEW $schema.item_view AS SELECT state FROM $schema.items GROUP BY state"},
			prepared:          prepared("pf_view_aggregate_old", "SELECT state = $1 FROM $schema.item_view", "ready"),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT state, count(*) AS total FROM $schema.items GROUP BY state"},
			afterExpandChecks: []string{"SELECT state = 'ready' AND total = 2 FROM $schema.item_view"},
		},
		{
			variantID: "view.preserve-two-output-order", schema: "pf_view_order", nativeOwnerID: owner,
			invariant: "two legacy outputs retain their names and order when a third is appended", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text, priority integer)", "INSERT INTO $schema.items VALUES (1, 'ready', 7)", "CREATE VIEW $schema.item_view AS SELECT id, state FROM $schema.items"},
			prepared:          prepared("pf_view_order_old", "SELECT id = $1 AND state = $2 FROM $schema.item_view", int64(1), "ready"),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, state, priority FROM $schema.items"},
			afterExpandChecks: []string{"SELECT string_agg(column_name::text, ',' ORDER BY ordinal_position) = 'id,state,priority' FROM information_schema.columns WHERE table_schema = '$schema' AND table_name = 'item_view'"},
		},
		{
			variantID: "view.quoted-view-name", schema: "pf_view_quoted_name", nativeOwnerID: owner,
			invariant: "a quoted view identity receives an appended output without losing legacy reads", requires: requires,
			setup:             []string{`CREATE SCHEMA $schema`, `CREATE TABLE $schema.items (id bigint, state text)`, `INSERT INTO $schema.items VALUES (1, 'ready')`, `CREATE VIEW $schema."Order" AS SELECT id FROM $schema.items`},
			prepared:          prepared("pf_view_quoted_name_old", `SELECT id = $1 FROM $schema."Order"`, int64(1)),
			expand:            []string{`CREATE OR REPLACE VIEW $schema."Order" AS SELECT id, state FROM $schema.items`},
			afterExpandChecks: []string{`SELECT state = 'ready' FROM $schema."Order" WHERE id = 1`},
		},
		{
			variantID: "view.multirow-explicit-projection", schema: "pf_view_multirow", nativeOwnerID: owner,
			invariant: "an explicit legacy projection returns the same row set after a new output is appended", requires: requires,
			setup:             []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "INSERT INTO $schema.items SELECT value, CASE WHEN value % 2 = 0 THEN 'even' ELSE 'odd' END FROM generate_series(1, 5) AS value", "CREATE VIEW $schema.item_view AS SELECT id FROM $schema.items"},
			prepared:          prepared("pf_view_multirow_old", "SELECT count(*) = 5 AND sum(id) = 15 FROM $schema.item_view"),
			expand:            []string{"CREATE OR REPLACE VIEW $schema.item_view AS SELECT id, state FROM $schema.items"},
			afterExpandChecks: []string{"SELECT count(*) FILTER (WHERE state = 'even') = 2 AND count(*) FILTER (WHERE state = 'odd') = 3 FROM $schema.item_view"},
		},
	})
}

func materializedViewPGliteVariants() []pglitePreflightCase {
	owner := "native.materialized-view-lifecycle"
	requires := []testkit.Capability{testkit.CapabilityTableDDL, testkit.CapabilityMaterializedView}
	return materializePGliteCases([]pgliteCaseTemplate{
		{
			variantID: "matview.empty-append-output", schema: "pf_mat_empty", nativeOwnerID: owner,
			invariant: "an empty materialized projection retains its old output while adding a new one", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT count(*) = 0 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, state FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT count(*) = 0 AND count(state) = 0 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.null-appended-output", schema: "pf_mat_null", nativeOwnerID: owner,
			invariant: "a rebuilt materialized view preserves NULL identity in its appended output", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, note text)", "INSERT INTO $schema.items VALUES (1, NULL), (2, 'set')", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT count(*) = 2 AND sum(id) = 3 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, note FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT count(*) FILTER (WHERE note IS NULL) = 1 AND count(*) FILTER (WHERE note = 'set') = 1 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.two-group-counts", schema: "pf_mat_groups", nativeOwnerID: owner,
			invariant: "a grouped rebuild retains both legacy group keys and appends each group count", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "INSERT INTO $schema.items VALUES (1, 'a'), (2, 'a'), (3, 'b')", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT state FROM $schema.items GROUP BY state"},
			beforeExpandChecks: []string{"SELECT count(*) = 2 AND min(state) = 'a' AND max(state) = 'b' FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT state, count(*) AS total FROM $schema.items GROUP BY state"},
			afterExpandChecks:  []string{"SELECT sum(total) = 3 AND max(total) = 2 AND min(total) = 1 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.append-sum-aggregate", schema: "pf_mat_sum", nativeOwnerID: owner,
			invariant: "a grouped materialized output appends the exact sum without changing its legacy group", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (state text, amount integer)", "INSERT INTO $schema.items VALUES ('ready', 2), ('ready', 3)", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT state FROM $schema.items GROUP BY state"},
			beforeExpandChecks: []string{"SELECT state = 'ready' FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT state, sum(amount) AS total_amount FROM $schema.items GROUP BY state"},
			afterExpandChecks:  []string{"SELECT state = 'ready' AND total_amount = 5 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.append-filtered-count", schema: "pf_mat_filter", nativeOwnerID: owner,
			invariant: "a filtered aggregate can be appended while the legacy total remains unchanged", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, active boolean)", "INSERT INTO $schema.items VALUES (1, true), (2, false), (3, true)", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT count(*) AS total FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT total = 3 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT count(*) AS total, count(*) FILTER (WHERE active) AS active_total FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT total = 3 AND active_total = 2 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.quoted-name-and-output", schema: "pf_mat_quoted", nativeOwnerID: owner,
			invariant: "quoted materialized-view and output identities survive a reviewed rebuild", requires: requires,
			setup:              []string{`CREATE SCHEMA $schema`, `CREATE TABLE $schema.items (id bigint, state text)`, `INSERT INTO $schema.items VALUES (1, 'ready')`, `CREATE MATERIALIZED VIEW $schema."Order" AS SELECT id AS "select" FROM $schema.items`},
			beforeExpandChecks: []string{`SELECT "select" = 1 FROM $schema."Order"`},
			expand:             []string{`DROP MATERIALIZED VIEW $schema."Order"`, `CREATE MATERIALIZED VIEW $schema."Order" AS SELECT id AS "select", state AS "group" FROM $schema.items`},
			afterExpandChecks:  []string{`SELECT "select" = 1 AND "group" = 'ready' FROM $schema."Order"`},
		},
		{
			variantID: "matview.append-expression-output", schema: "pf_mat_expression", nativeOwnerID: owner,
			invariant: "a computed materialized output is appended without changing the legacy identifier", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, first_name text, last_name text)", "INSERT INTO $schema.items VALUES (1, 'Ada', 'Lovelace')", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT id = 1 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, first_name || ' ' || last_name AS full_name FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT id = 1 AND full_name = 'Ada Lovelace' FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.join-appended-output", schema: "pf_mat_join", nativeOwnerID: owner,
			invariant: "a reviewed join rebuild appends related data while preserving the legacy row identifier", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.accounts (id bigint, name text)", "CREATE TABLE $schema.items (id bigint, account_id bigint)", "INSERT INTO $schema.accounts VALUES (10, 'Acme')", "INSERT INTO $schema.items VALUES (1, 10)", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT i.id FROM $schema.items i JOIN $schema.accounts a ON a.id = i.account_id"},
			beforeExpandChecks: []string{"SELECT id = 1 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT i.id, a.name AS account_name FROM $schema.items i JOIN $schema.accounts a ON a.id = i.account_id"},
			afterExpandChecks:  []string{"SELECT id = 1 AND account_name = 'Acme' FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.two-appended-outputs", schema: "pf_mat_two_outputs", nativeOwnerID: owner,
			invariant: "two materialized outputs can be appended while retaining the legacy first output", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text, priority integer)", "INSERT INTO $schema.items VALUES (1, 'ready', 7)", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT id = 1 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, state, priority FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT id = 1 AND state = 'ready' AND priority = 7 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.refresh-after-new-row", schema: "pf_mat_refresh", nativeOwnerID: owner,
			invariant: "the rebuilt materialized shape exposes new base rows only after its explicit refresh boundary", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text)", "INSERT INTO $schema.items VALUES (1, 'old')", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT count(*) = 1 AND min(id) = 1 FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, state FROM $schema.items", "INSERT INTO $schema.items VALUES (2, 'new')", "REFRESH MATERIALIZED VIEW $schema.item_rollup"},
			afterExpandChecks:  []string{"SELECT count(*) = 2 AND max(id) = 2 AND max(state) = 'old' FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.distinct-value-output", schema: "pf_mat_distinct", nativeOwnerID: owner,
			invariant: "a distinct materialized legacy value gains a deterministic derived output per value", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (state text)", "INSERT INTO $schema.items VALUES ('a'), ('a'), ('bb')", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT DISTINCT state FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT count(*) = 2 AND min(state) = 'a' AND max(state) = 'bb' FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT DISTINCT state, length(state) AS state_length FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT min(state_length) = 1 AND max(state_length) = 2 AND count(*) = 2 FROM $schema.item_rollup"},
		},
		{
			variantID: "matview.output-order-preserved", schema: "pf_mat_order", nativeOwnerID: owner,
			invariant: "two legacy materialized outputs retain ordinal order when a third is appended", requires: requires,
			setup:              []string{"CREATE SCHEMA $schema", "CREATE TABLE $schema.items (id bigint, state text, priority integer)", "INSERT INTO $schema.items VALUES (1, 'ready', 7)", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, state FROM $schema.items"},
			beforeExpandChecks: []string{"SELECT id = 1 AND state = 'ready' FROM $schema.item_rollup"},
			expand:             []string{"DROP MATERIALIZED VIEW $schema.item_rollup", "CREATE MATERIALIZED VIEW $schema.item_rollup AS SELECT id, state, priority FROM $schema.items"},
			afterExpandChecks:  []string{"SELECT string_agg(attname, ',' ORDER BY attnum) = 'id,state,priority' FROM pg_attribute WHERE attrelid = '$schema.item_rollup'::regclass AND attnum > 0 AND NOT attisdropped"},
		},
	})
}

func quotedRenameExpandSQL(schema string) []string {
	return []string{
		fmt.Sprintf(`ALTER TABLE %s.people ADD COLUMN "new name" text`, schema),
		fmt.Sprintf(`UPDATE %s.people SET "new name" = "old name"`, schema),
		fmt.Sprintf(`CREATE FUNCTION %[1]s.sync_quoted_names() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW."old name" IS NULL THEN NEW."old name" := NEW."new name"; END IF;
    IF NEW."new name" IS NULL THEN NEW."new name" := NEW."old name"; END IF;
  ELSIF NEW."old name" IS DISTINCT FROM OLD."old name" AND NEW."new name" IS NOT DISTINCT FROM OLD."new name" THEN
    NEW."new name" := NEW."old name";
  ELSIF NEW."new name" IS DISTINCT FROM OLD."new name" AND NEW."old name" IS NOT DISTINCT FROM OLD."old name" THEN
    NEW."old name" := NEW."new name";
  END IF;
  IF NEW."old name" IS DISTINCT FROM NEW."new name" THEN
    RAISE EXCEPTION 'quoted names disagree' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$`, schema),
		fmt.Sprintf("CREATE TRIGGER sync_quoted_names BEFORE INSERT OR UPDATE ON %s.people FOR EACH ROW EXECUTE FUNCTION %s.sync_quoted_names()", schema, schema),
	}
}

func quotedRenameContractSQL(schema string) []string {
	return []string{
		fmt.Sprintf("DROP TRIGGER sync_quoted_names ON %s.people", schema),
		fmt.Sprintf("DROP FUNCTION %s.sync_quoted_names()", schema),
		fmt.Sprintf(`ALTER TABLE %s.people ALTER COLUMN "new name" SET NOT NULL`, schema),
		fmt.Sprintf(`ALTER TABLE %s.people DROP COLUMN "old name"`, schema),
	}
}

func renameExpandSQL(schema string) []string {
	return []string{
		fmt.Sprintf("ALTER TABLE %s.people ADD COLUMN new_name text", schema),
		fmt.Sprintf("UPDATE %s.people SET new_name = old_name", schema),
		fmt.Sprintf(`CREATE FUNCTION %[1]s.sync_names() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.old_name IS NULL THEN NEW.old_name := NEW.new_name; END IF;
    IF NEW.new_name IS NULL THEN NEW.new_name := NEW.old_name; END IF;
  ELSIF NEW.old_name IS DISTINCT FROM OLD.old_name AND NEW.new_name IS NOT DISTINCT FROM OLD.new_name THEN
    NEW.new_name := NEW.old_name;
  ELSIF NEW.new_name IS DISTINCT FROM OLD.new_name AND NEW.old_name IS NOT DISTINCT FROM OLD.old_name THEN
    NEW.old_name := NEW.new_name;
  END IF;
  IF NEW.old_name IS DISTINCT FROM NEW.new_name THEN
    RAISE EXCEPTION 'old_name and new_name disagree' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$`, schema),
		fmt.Sprintf("CREATE TRIGGER sync_names BEFORE INSERT OR UPDATE ON %s.people FOR EACH ROW EXECUTE FUNCTION %s.sync_names()", schema, schema),
	}
}

func renameContractSQL(schema string) []string {
	return []string{
		fmt.Sprintf("DROP TRIGGER sync_names ON %s.people", schema),
		fmt.Sprintf("DROP FUNCTION %s.sync_names()", schema),
		fmt.Sprintf("ALTER TABLE %s.people ALTER COLUMN new_name SET NOT NULL", schema),
		fmt.Sprintf("ALTER TABLE %s.people DROP COLUMN old_name", schema),
	}
}
