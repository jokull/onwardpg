package differential

// These tests use Stripe pg-schema-diff v1.0.7 (MIT; Copyright 2023-
// Stripe, Inc.) as a test-only executable reference. See THIRD_PARTY_NOTICES.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

const pinnedStripeReferenceCommit = "6208f8f3ceccae8ca634055dc47907a6a864cb76"

type stripePlan struct {
	Statements        []stripeStatement `json:"statements"`
	CurrentSchemaHash string            `json:"current_schema_hash"`
}

type stripeStatement struct {
	DDL           string         `json:"ddl"`
	TimeoutMS     int64          `json:"timeout_ms"`
	LockTimeoutMS int64          `json:"lock_timeout_ms"`
	Hazards       []stripeHazard `json:"hazards"`
}

type stripeHazard struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Stripe fixture receipt:
// migration_acceptance_tests/column_cases_test.go#TestColumnTestCases/
// Change data type, nullability (NOT NULL), and default.
func TestPinnedStripeColumnMutationRequiresOnwardBridgeWithCompleteChoreography(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE TABLE public.foobar (
    id integer PRIMARY KEY,
    foobar text DEFAULT 'some default'
);`
	desiredDDL := `CREATE TABLE public.foobar (
    id integer PRIMARY KEY,
    foobar char NOT NULL DEFAULT 'A'
);`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	for _, fragment := range []string{"SET DATA TYPE", "ANALYZE", "SET NOT NULL", "SET DEFAULT"} {
		if !strings.Contains(stripeSQL, fragment) {
			t.Fatalf("pinned Stripe column fixture omitted %q: %#v", fragment, stripe)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 2 {
		t.Fatalf("onwardpg column decisions=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{
		ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
	}
	for _, question := range pending.Questions {
		value := ""
		switch question.Kind {
		case "type_change":
			value = "manual_sql"
		case "set_not_null":
			value = "staged"
		default:
			t.Fatalf("unexpected onwardpg column question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
	}
	onward, err := graphplan.Build(current, desired, answers, graphplan.Options{})
	if err != nil || onward.Status != protocol.NeedsSQLEdits {
		t.Fatalf("onwardpg column handoff=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	contractTODO := strings.LastIndex(onwardSQL, "ONWARDPG TODO")
	dropDefault := strings.Index(onwardSQL, `ALTER COLUMN "foobar" DROP DEFAULT`)
	analyze := strings.Index(onwardSQL, `ANALYZE "public"."foobar" ("foobar")`)
	setDefault := strings.Index(onwardSQL, `ALTER COLUMN "foobar" SET DEFAULT 'A'`)
	setNotNull := strings.Index(onwardSQL, `ALTER COLUMN "foobar" SET NOT NULL`)
	if dropDefault < 0 || contractTODO < dropDefault || analyze < contractTODO || setDefault < analyze || setNotNull < setDefault {
		t.Fatalf("onwardpg column choreography is incomplete or unordered:\n%s", onwardSQL)
	}
}

// Combined Stripe fixture receipt:
// migration_acceptance_tests/view_cases_test.go#TestViewTestCases/
// Recreate view due to dependent column changing, and
// migration_acceptance_tests/materialized_view_cases_test.go#TestMaterializedViewTestCases/
// Recreate materialized view due to dependent column changing.
func TestPinnedStripeRejectsDerivedViewTypeClosureOnwardGeneratesIt(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE SCHEMA app;
CREATE TABLE app.facts (val integer NOT NULL);
INSERT INTO app.facts VALUES (7);
CREATE VIEW app.fact_view AS SELECT val FROM app.facts;
CREATE MATERIALIZED VIEW app.fact_cache AS SELECT val FROM app.fact_view;
CREATE UNIQUE INDEX fact_cache_val_idx ON app.fact_cache (val);`
	desiredDDL := strings.Replace(currentDDL, "val integer", "val bigint", 1)
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripeError := runStripePlanExpectFailure(t, ctx, stripeBinary, urls["source"], desiredDir)
	if !strings.Contains(stripeError, "validating migration plan") || !strings.Contains(stripeError, `relation "app.fact_view" does not exist`) {
		t.Fatalf("pinned Stripe derived type-closure failure changed:\n%s", stripeError)
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("onwardpg derived closure decision=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{
		ProtocolVersion: pending.ProtocolVersion, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{{Kind: "type_change", Key: pending.Questions[0].Key, Value: "manual_sql", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}},
	}
	onward, err := graphplan.Build(current, desired, answers, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.NeedsSQLEdits {
		t.Fatalf("onwardpg derived closure handoff=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	onwardDropMatView := strings.Index(onwardSQL, `DROP MATERIALIZED VIEW "app"."fact_cache"`)
	onwardDropView := strings.Index(onwardSQL, `DROP VIEW "app"."fact_view"`)
	onwardType := strings.LastIndex(onwardSQL, "ONWARDPG TODO")
	onwardCreateView := strings.Index(onwardSQL, `CREATE VIEW "app"."fact_view"`)
	onwardCreateMatView := strings.Index(onwardSQL, `CREATE MATERIALIZED VIEW "app"."fact_cache"`)
	onwardCreateIndex := strings.Index(onwardSQL, `CREATE UNIQUE INDEX CONCURRENTLY "fact_cache_val_idx"`)
	if onwardDropMatView < 0 || onwardDropView < 0 || onwardType < 0 || onwardCreateView < 0 || onwardCreateMatView < 0 || onwardCreateIndex < 0 ||
		onwardDropMatView > onwardDropView || onwardDropView > onwardType || onwardType > onwardCreateView || onwardCreateView > onwardCreateMatView || onwardCreateMatView > onwardCreateIndex || !strings.Contains(onwardSQL, "WITH DATA") {
		t.Fatalf("onwardpg derived closure is incomplete or unordered:\n%s", onwardSQL)
	}

	onward = editOnwardTypeHandoff(onward, `ALTER TABLE "app"."facts" ALTER COLUMN "val" TYPE bigint USING "val"::bigint;`)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s derived closure did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
}

// Stripe pg-schema-diff v1.0.7 receipts:
// migration_acceptance_tests/partitioned_table_cases_test.go#TestPartitionedTableTestCases/Unpartitioned to partitioned,
// /Partitioned to unpartitioned, and /Changing partition key def errors.
func TestPinnedStripePartitionTopologyReplacementOnwardScaffoldsRetainedData(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	t.Run("ordinary_to_partitioned", func(t *testing.T) {
		currentDDL := `CREATE TABLE public.events (id bigint NOT NULL, occurred_at date NOT NULL, payload text, CONSTRAINT events_pkey PRIMARY KEY (id));
INSERT INTO public.events VALUES (1, '2026-04-05', 'retained');`
		desiredDDL := `CREATE TABLE public.events (id bigint NOT NULL, occurred_at date NOT NULL, payload text, CONSTRAINT events_pkey PRIMARY KEY (id, occurred_at)) PARTITION BY RANGE (occurred_at);
CREATE TABLE public.events_2026 PARTITION OF public.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE INDEX events_payload_idx ON public.events_2026 (payload);`
		urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source")
		desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
		stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
		stripeSQL := stripeDDL(stripe)
		if !strings.Contains(stripeSQL, `DROP TABLE "public"."events"`) || !strings.Contains(stripeSQL, `CREATE TABLE "public"."events"`) || !stripeHasHazard(stripe, "DELETES_DATA") {
			t.Fatalf("pinned Stripe ordinary-to-partitioned receipt changed: %#v", stripe)
		}
		current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
		if err != nil || onward.Status != protocol.NeedsInput || len(onward.Guidance) != 1 {
			t.Fatalf("onwardpg partition scaffold=%#v err=%v", onward, err)
		}
		runbook := joinGuidanceSQL(onward.Guidance[0])
		for _, fragment := range []string{"CREATE TABLE \"public\".\"onwardpg_shadow_", "OVERRIDING SYSTEM VALUE", "onwardpg_row_count_matches", "onwardpg_rows_match", "CREATE INDEX \"onwardpg_idx_", "separate, current-catalog fingerprint authorization"} {
			if !strings.Contains(runbook, fragment) {
				t.Fatalf("onwardpg retained-data runbook omitted %q:\n%s", fragment, runbook)
			}
		}
	})

	t.Run("partition_key_replacement", func(t *testing.T) {
		currentDDL := `CREATE TABLE public.events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY RANGE (occurred_at);
CREATE TABLE public.events_2026 PARTITION OF public.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');`
		desiredDDL := `CREATE TABLE public.events (id bigint NOT NULL, occurred_at date NOT NULL) PARTITION BY HASH (id);
CREATE TABLE public.events_0 PARTITION OF public.events FOR VALUES WITH (MODULUS 2, REMAINDER 0);
CREATE TABLE public.events_1 PARTITION OF public.events FOR VALUES WITH (MODULUS 2, REMAINDER 1);`
		urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source")
		desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
		stripeFailure := runStripePlanExpectFailure(t, ctx, stripeBinary, urls["source"], desiredDir)
		if !strings.Contains(strings.ToLower(stripeFailure), "partition") {
			t.Fatalf("pinned Stripe partition-key rejection changed: %s", stripeFailure)
		}
		current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
		if err != nil || onward.Status != protocol.NeedsInput || len(onward.Guidance) != 1 {
			t.Fatalf("onwardpg key-replacement scaffold=%#v err=%v", onward, err)
		}
		runbook := joinGuidanceSQL(onward.Guidance[0])
		for _, fragment := range []string{"PARTITION BY HASH (id)", "FOR VALUES WITH (modulus 2, remainder 0)", "contract_cutover_assertions", "brief_rename_cutover_and_typed_closure"} {
			if !strings.Contains(strings.ToLower(runbook), strings.ToLower(fragment)) {
				t.Fatalf("onwardpg partition-key runbook omitted %q:\n%s", fragment, runbook)
			}
		}
	})

	t.Run("partitioned_to_ordinary", func(t *testing.T) {
		currentDDL := `CREATE TABLE public.events (id bigint NOT NULL, occurred_at date NOT NULL, payload text, CONSTRAINT events_pkey PRIMARY KEY (id, occurred_at)) PARTITION BY RANGE (occurred_at);
CREATE TABLE public.events_2026 PARTITION OF public.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
INSERT INTO public.events VALUES (1, '2026-04-05', 'retained');`
		desiredDDL := `CREATE TABLE public.events (id bigint NOT NULL, occurred_at date NOT NULL, payload text, CONSTRAINT events_pkey PRIMARY KEY (id));`
		urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source")
		desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
		stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
		stripeSQL := stripeDDL(stripe)
		if !strings.Contains(stripeSQL, `DROP TABLE "public"."events"`) || !strings.Contains(stripeSQL, `CREATE TABLE "public"."events"`) || !stripeHasHazard(stripe, "DELETES_DATA") {
			t.Fatalf("pinned Stripe partitioned-to-ordinary receipt changed: %#v", stripe)
		}
		current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
		if err != nil || onward.Status != protocol.NeedsInput || len(onward.Guidance) != 1 {
			t.Fatalf("onwardpg unpartition scaffold=%#v err=%v", onward, err)
		}
		runbook := joinGuidanceSQL(onward.Guidance[0])
		for _, fragment := range []string{"table:public:events_2026", "OVERRIDING SYSTEM VALUE", "RENAME TO \"onwardpg_old_", "onwardpg_rows_match", "Never infer production copy duration"} {
			if !strings.Contains(runbook, fragment) {
				t.Fatalf("onwardpg unpartition runbook omitted %q:\n%s", fragment, runbook)
			}
		}
	})
}

func joinGuidanceSQL(guidance protocol.Guidance) string {
	parts := make([]string, 0, len(guidance.Steps))
	for _, step := range guidance.Steps {
		parts = append(parts, step.Stage+"\n"+step.SQL)
	}
	return strings.Join(parts, "\n")
}

func TestPinnedStripeAndOnwardPGContinuousIndexReplacement(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE SCHEMA app;
CREATE TABLE app.orders (account_id bigint, created_at timestamptz);
CREATE INDEX orders_lookup_idx ON app.orders (account_id);`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.orders (account_id bigint, created_at timestamptz);
CREATE INDEX orders_lookup_idx ON app.orders (account_id, created_at DESC);`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	if len(stripe.Statements) != 3 {
		t.Fatalf("Stripe reference emitted %d statements, want 3: %#v", len(stripe.Statements), stripe)
	}
	if !strings.HasPrefix(stripe.Statements[0].DDL, `ALTER INDEX "app"."orders_lookup_idx" RENAME TO `) ||
		!strings.Contains(stripe.Statements[1].DDL, `CREATE INDEX CONCURRENTLY orders_lookup_idx`) ||
		!strings.HasPrefix(stripe.Statements[2].DDL, `DROP INDEX CONCURRENTLY "app"."pgschemadiff_tmpidx_`) {
		t.Fatalf("Stripe reference lost continuous replacement ordering: %#v", stripe.Statements)
	}
	assertStripeTimeoutsAndHazards(t, stripe)

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := buildOnwardWithAssertions(current, desired, graphplan.Options{ConcurrentIndexes: true})
	if err != nil {
		t.Fatal(err)
	}
	if onward.Status != protocol.Planned || len(onward.Statements) != 3 {
		t.Fatalf("onwardpg did not produce a continuous replacement: %#v", onward)
	}
	if !strings.HasPrefix(onward.Statements[0].SQL, `ALTER INDEX "app"."orders_lookup_idx" RENAME TO "onwardpg_tmpidx_`) ||
		!strings.Contains(onward.Statements[1].SQL, `CREATE INDEX CONCURRENTLY "orders_lookup_idx"`) ||
		!strings.HasPrefix(onward.Statements[2].SQL, `DROP INDEX CONCURRENTLY "app"."onwardpg_tmpidx_`) {
		t.Fatalf("onwardpg lost continuous replacement ordering: %#v", onward.Statements)
	}
	for i, statement := range onward.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("onwardpg statement %d lacks timeout guidance: %#v", i, statement)
		}
	}

	applyStripePlanWithInvariant(t, ctx, urls["stripe"], stripe)
	applyOnwardPlanWithInvariant(t, ctx, urls["onward"], onward)
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		actualFingerprint, _ := actual.Fingerprint()
		if actualFingerprint != targetFingerprint {
			t.Fatalf("%s result did not converge: got %s want %s", name, actualFingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe reference is not idempotent after application: %#v", residual)
	}
	actual, err := source.LoadGraph(ctx, source.Parse(urls["onward"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	onwardResidual, err := graphplan.Build(actual, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onwardResidual.Status != protocol.Planned || len(onwardResidual.Statements) != 0 {
		t.Fatalf("onwardpg residual = %#v, err=%v", onwardResidual, err)
	}
}

func TestPinnedStripeAndOnwardPGContinuousMaterializedViewAndLocalIndexReplacement(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint, tenant_id bigint);
CREATE MATERIALIZED VIEW app.account_cache AS SELECT id, tenant_id FROM app.accounts;
CREATE INDEX account_cache_lookup_idx ON app.account_cache (id);
CREATE SCHEMA schema_1;
CREATE TABLE schema_1.foobar (id integer, foo varchar(255)) PARTITION BY LIST (foo);
CREATE SCHEMA schema_2;
CREATE TABLE schema_2.foobar_1 PARTITION OF schema_1.foobar FOR VALUES IN ('foo_1');
CREATE UNIQUE INDEX some_unique_idx ON schema_2.foobar_1 (foo, id);
CREATE SCHEMA schema_3;
CREATE TABLE schema_3.foobar (id text, foo integer) PARTITION BY LIST (foo);
CREATE SCHEMA schema_4;
CREATE TABLE schema_4.foobar_1 PARTITION OF schema_3.foobar FOR VALUES IN (1);
CREATE UNIQUE INDEX some_unique_idx ON schema_4.foobar_1 (foo, id);`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint, tenant_id bigint);
CREATE MATERIALIZED VIEW app.account_cache AS SELECT id, tenant_id FROM app.accounts;
CREATE INDEX account_cache_lookup_idx ON app.account_cache (id, tenant_id);
CREATE SCHEMA schema_1;
CREATE TABLE schema_1.foobar (id integer, foo varchar(255)) PARTITION BY LIST (foo);
CREATE SCHEMA schema_2;
CREATE TABLE schema_2.foobar_1 PARTITION OF schema_1.foobar FOR VALUES IN ('foo_1');
CREATE UNIQUE INDEX some_unique_idx ON schema_2.foobar_1 (id, foo);
CREATE SCHEMA schema_3;
CREATE TABLE schema_3.foobar (id text, foo integer) PARTITION BY LIST (foo);
CREATE SCHEMA schema_4;
CREATE TABLE schema_4.foobar_1 PARTITION OF schema_3.foobar FOR VALUES IN (1);
CREATE UNIQUE INDEX some_unique_idx ON schema_4.foobar_1 (id, foo);`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	if len(stripe.Statements) != 9 || !stripeHasHazard(stripe, "INDEX_BUILD") || !stripeHasHazard(stripe, "INDEX_DROPPED") {
		t.Fatalf("Stripe special-index replacement changed: %#v", stripe)
	}
	for i, statement := range stripe.Statements {
		if statement.TimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("Stripe special-index statement %d lacks timeout metadata: %#v", i, statement)
		}
	}
	stripeSQL := stripeDDL(stripe)
	if !strings.Contains(stripeSQL, `ALTER INDEX "app"."account_cache_lookup_idx" RENAME TO`) || !strings.Contains(stripeSQL, `CREATE INDEX CONCURRENTLY account_cache_lookup_idx`) {
		t.Fatalf("Stripe did not continuously replace the materialized-view index: %#v", stripe)
	}
	for _, schema := range []string{"schema_2", "schema_4"} {
		if !strings.Contains(stripeSQL, `ALTER INDEX "`+schema+`"."some_unique_idx" RENAME TO`) {
			t.Fatalf("Stripe did not continuously replace the conflicting local index in %s: %#v", schema, stripe)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := buildOnwardWithAssertions(current, desired, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.Planned || len(onward.Statements) != 9 {
		t.Fatalf("onwardpg special-index replacement=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	if !strings.Contains(onwardSQL, `ALTER INDEX "app"."account_cache_lookup_idx" RENAME TO "onwardpg_tmpidx_`) || !strings.Contains(onwardSQL, `CREATE INDEX CONCURRENTLY "account_cache_lookup_idx"`) {
		t.Fatalf("onwardpg did not continuously replace the materialized-view index: %#v", onward)
	}
	for _, schema := range []string{"schema_2", "schema_4"} {
		if !strings.Contains(onwardSQL, `DROP INDEX CONCURRENTLY "`+schema+`"."some_unique_idx"`) ||
			!strings.Contains(onwardSQL, `CREATE UNIQUE INDEX CONCURRENTLY "some_unique_idx" ON "`+schema+`"."foobar_1"`) {
			t.Fatalf("onwardpg did not loosen the local unique index in expand and restore it behind a contract gate in %s: %#v", schema, onward)
		}
	}
	for i, statement := range onward.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("onwardpg special-index statement %d lacks timeout metadata: %#v", i, statement)
		}
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s special-index result did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe special-index residual: %#v", residual)
	}
}

func TestPinnedStripeAndOnwardPGContinuousPartitionedParentIndexReplacement(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE TABLE public.foobar (id integer, foo varchar(255), bar integer) PARTITION BY LIST (foo);
CREATE TABLE public.foobar_1 PARTITION OF public.foobar FOR VALUES IN ('foo_1');
CREATE TABLE public.foobar_2 PARTITION OF public.foobar FOR VALUES IN ('foo_2');
CREATE TABLE public.foobar_3 PARTITION OF public.foobar FOR VALUES IN ('foo_3');
CREATE INDEX some_idx ON public.foobar (foo, bar);`
	desiredDDL := `CREATE TABLE public.foobar (id integer, foo varchar(255), bar integer) PARTITION BY LIST (foo);
CREATE TABLE public.foobar_1 PARTITION OF public.foobar FOR VALUES IN ('foo_1');
CREATE TABLE public.foobar_2 PARTITION OF public.foobar FOR VALUES IN ('foo_2');
CREATE TABLE public.foobar_3 PARTITION OF public.foobar FOR VALUES IN ('foo_3');
CREATE INDEX some_idx ON public.foobar (bar, foo);`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	if !strings.Contains(stripeSQL, `ALTER INDEX "public"."some_idx" RENAME TO`) || !strings.Contains(stripeSQL, `CREATE INDEX some_idx ON ONLY public.foobar`) || !strings.Contains(stripeSQL, `ATTACH PARTITION`) || !stripeHasHazard(stripe, "INDEX_BUILD") || !stripeHasHazard(stripe, "INDEX_DROPPED") {
		t.Fatalf("Stripe partition-parent replacement changed: %#v", stripe)
	}
	for i, statement := range stripe.Statements {
		if statement.TimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("Stripe partition-parent statement %d lacks timeout metadata: %#v", i, statement)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.Planned {
		t.Fatalf("onwardpg partition-parent replacement=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	if !strings.Contains(onwardSQL, `ALTER INDEX "public"."some_idx" RENAME TO "onwardpg_tmpidx_`) || !strings.Contains(onwardSQL, `CREATE INDEX "some_idx" ON ONLY "public"."foobar"`) || !strings.Contains(onwardSQL, `CREATE INDEX CONCURRENTLY`) || !strings.Contains(onwardSQL, `ATTACH PARTITION`) {
		t.Fatalf("onwardpg partition-parent replacement lost its hierarchy strategy: %#v", onward)
	}
	for i, statement := range onward.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("onwardpg partition-parent statement %d lacks timeout metadata: %#v", i, statement)
		}
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s partition-parent result did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe partition-parent residual: %#v", residual)
	}
}

func TestPinnedStripeAndOnwardPGContinuousPrimaryConstraintReplacement(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE TABLE public.foobar (id integer NOT NULL, foo text NOT NULL);
CREATE UNIQUE INDEX unique_idx ON public.foobar (id);
ALTER TABLE public.foobar ADD CONSTRAINT non_default_primary_key PRIMARY KEY USING INDEX unique_idx;`
	desiredDDL := `CREATE TABLE public.foobar (id integer NOT NULL, foo text NOT NULL);
CREATE UNIQUE INDEX unique_idx ON public.foobar (id, foo);
ALTER TABLE public.foobar ADD CONSTRAINT non_default_primary_key PRIMARY KEY USING INDEX unique_idx;`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	if !strings.Contains(stripeSQL, `CREATE UNIQUE INDEX CONCURRENTLY`) || !strings.Contains(stripeSQL, `DROP CONSTRAINT "non_default_primary_key"`) || !strings.Contains(stripeSQL, `PRIMARY KEY USING INDEX`) || !stripeHasHazard(stripe, "INDEX_BUILD") || !stripeHasHazard(stripe, "INDEX_DROPPED") {
		t.Fatalf("Stripe primary-constraint replacement changed: %#v", stripe)
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.Planned {
		t.Fatalf("onwardpg primary-constraint replacement=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	if !strings.Contains(onwardSQL, `CREATE UNIQUE INDEX CONCURRENTLY "onwardpg_tmpidx_`) || !strings.Contains(onwardSQL, `DROP CONSTRAINT "non_default_primary_key"`) || !strings.Contains(onwardSQL, `PRIMARY KEY USING INDEX "onwardpg_tmpidx_`) {
		t.Fatalf("onwardpg primary-constraint replacement lost its swap: %#v", onward)
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s primary-constraint result did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe primary-constraint residual: %#v", residual)
	}
}

// Combined Stripe fixture receipt:
// migration_acceptance_tests/index_cases_test.go#TestIndexTestCases/
// Alter primary key columns (name stays same), and
// migration_acceptance_tests/foreign_key_constraint_cases_test.go#TestForeignKeyConstraintTestCases/
// Alter FK (columns).
func TestPinnedStripeAndOnwardPGCoordinateReferencedKeyAndForeignKey(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  CONSTRAINT accounts_pkey PRIMARY KEY (id)
);
CREATE TABLE app.orders (
  account_id bigint,
  tenant_id bigint,
  CONSTRAINT orders_account_fkey FOREIGN KEY (account_id)
    REFERENCES app.accounts(id) DEFERRABLE INITIALLY DEFERRED
);`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.accounts (
  id bigint NOT NULL,
  tenant_id bigint NOT NULL,
  CONSTRAINT accounts_pkey PRIMARY KEY (id, tenant_id)
);
CREATE TABLE app.orders (
  account_id bigint,
  tenant_id bigint,
  CONSTRAINT orders_account_fkey FOREIGN KEY (account_id, tenant_id)
    REFERENCES app.accounts(id, tenant_id) DEFERRABLE INITIALLY DEFERRED
);`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	for _, fragment := range []string{"CREATE UNIQUE INDEX CONCURRENTLY", `DROP CONSTRAINT "orders_account_fkey"`, `DROP CONSTRAINT "accounts_pkey"`, `ADD CONSTRAINT "accounts_pkey"`, `ADD CONSTRAINT "orders_account_fkey"`, "VALIDATE CONSTRAINT"} {
		if !strings.Contains(stripeSQL, fragment) {
			t.Fatalf("pinned Stripe key/FK receipt omitted %q: %#v", fragment, stripe)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := buildOnwardWithAssertions(current, desired, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.Planned {
		t.Fatalf("onwardpg key/FK replacement=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	foreignKeyDrop := strings.Index(onwardSQL, `DROP CONSTRAINT "orders_account_fkey"`)
	keyDrop := strings.Index(onwardSQL, `DROP CONSTRAINT "accounts_pkey"`)
	keyAdd := strings.Index(onwardSQL, `ADD CONSTRAINT "accounts_pkey"`)
	foreignKeyAdd := strings.Index(onwardSQL, `ADD CONSTRAINT "orders_account_fkey"`)
	foreignKeyValidate := strings.Index(onwardSQL, `VALIDATE CONSTRAINT "orders_account_fkey"`)
	if !strings.Contains(onwardSQL, "CREATE UNIQUE INDEX CONCURRENTLY") || foreignKeyDrop < 0 || keyDrop < 0 || keyAdd < 0 || foreignKeyAdd < 0 || foreignKeyValidate < 0 ||
		foreignKeyDrop > keyDrop || keyDrop > keyAdd || keyAdd > foreignKeyAdd || foreignKeyAdd > foreignKeyValidate {
		t.Fatalf("onwardpg key/FK closure is incomplete or unordered:\n%s", onwardSQL)
	}
	for _, fragment := range []string{"DEFERRABLE INITIALLY DEFERRED", "NOT VALID"} {
		if !strings.Contains(onwardSQL, fragment) {
			t.Fatalf("onwardpg key/FK replacement lost %q:\n%s", fragment, onwardSQL)
		}
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			actualJSON, _ := actual.CanonicalJSON()
			desiredJSON, _ := desired.CanonicalJSON()
			t.Fatalf("%s key/FK result did not converge: got %s want %s\nactual: %s\ndesired: %s", name, fingerprint, targetFingerprint, actualJSON, desiredJSON)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe key/FK residual: %#v", residual)
	}
	actual, err := source.LoadGraph(ctx, source.Parse(urls["onward"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	onwardResidual, err := graphplan.Build(actual, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onwardResidual.Status != protocol.Planned || len(onwardResidual.Statements) != 0 {
		t.Fatalf("onwardpg key/FK residual=%#v err=%v", onwardResidual, err)
	}
}

func TestPinnedStripePrimaryConstraintCaseRequiresOnwardTypeBridgeIntent(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	// This is Stripe v1.0.7's exact "Alter primary key columns (name stays
	// same)" acceptance DDL. Stripe emits its own direct cast; onwardpg refuses
	// to treat a catalog-convergent cast as proof that old and new application
	// versions can overlap during one deployment.
	currentDDL := `CREATE TABLE public.foobar (id integer NOT NULL, foo text NOT NULL);
CREATE UNIQUE INDEX unique_idx ON public.foobar (id);
ALTER TABLE public.foobar ADD CONSTRAINT non_default_primary_key PRIMARY KEY USING INDEX unique_idx;`
	desiredDDL := `CREATE TABLE public.foobar (id integer, foo integer NOT NULL);
CREATE UNIQUE INDEX unique_idx ON public.foobar (id, foo);
ALTER TABLE public.foobar ADD CONSTRAINT non_default_primary_key PRIMARY KEY USING INDEX unique_idx;`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	if err := executeStripePlan(ctx, urls["stripe"], stripe); err != nil {
		t.Fatalf("pinned Stripe acceptance plan should be executable: %v\n%#v", err, stripe)
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || pending.Status != protocol.NeedsInput || len(pending.Questions) != 1 || pending.Questions[0].Kind != "type_change" {
		t.Fatalf("onwardpg must ask for the absent cast: plan=%#v err=%v", pending, err)
	}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint, Answers: []protocol.Answer{{Kind: "type_change", Key: pending.Questions[0].Key, Value: "manual_sql", QuestionFingerprint: pending.Questions[0].ScopeFingerprint}}}
	onward, err := graphplan.Build(current, desired, answers, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.NeedsSQLEdits ||
		!hasPhaseTODO(onward, protocol.PhaseExpand) || !hasPhaseTODO(onward, protocol.PhaseContract) {
		t.Fatalf("onwardpg overlap-aware type handoff=%#v err=%v", onward, err)
	}
	stripeActual, err := source.LoadGraph(ctx, source.Parse(urls["stripe"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoOnwardResidual(t, stripeActual, desired)
}

// Adapted from Stripe pg-schema-diff v1.0.7's MIT-licensed partitioned-index
// acceptance case "Attach an unnattached, invalid index".
func TestPinnedStripeAndOnwardPGAttachExistingPartitionIndex(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE TABLE "Foobar" (id integer, foo varchar(255), PRIMARY KEY (foo)) PARTITION BY LIST (foo);
CREATE TABLE foobar_1 PARTITION OF "Foobar" FOR VALUES IN ('foo_1');
CREATE TABLE foobar_2 PARTITION OF "Foobar" FOR VALUES IN ('foo_2');
CREATE TABLE "Foobar_3" PARTITION OF "Foobar" FOR VALUES IN ('foo_3');
CREATE INDEX "Partitioned_Idx" ON ONLY "Foobar" (foo);
CREATE INDEX foobar_1_part ON foobar_1 (foo);
ALTER INDEX "Partitioned_Idx" ATTACH PARTITION foobar_1_part;
CREATE INDEX foobar_2_part ON foobar_2 (foo);
ALTER INDEX "Partitioned_Idx" ATTACH PARTITION foobar_2_part;
CREATE INDEX "Foobar_3_Part" ON "Foobar_3" (foo);`
	desiredDDL := currentDDL + `ALTER INDEX "Partitioned_Idx" ATTACH PARTITION "Foobar_3_Part";`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	if len(stripe.Statements) != 1 || stripe.Statements[0].DDL != `ALTER INDEX "public"."Partitioned_Idx" ATTACH PARTITION "public"."Foobar_3_Part"` {
		t.Fatalf("Stripe partition attachment changed: %#v", stripe)
	}
	if stripe.Statements[0].TimeoutMS <= 0 || stripe.Statements[0].LockTimeoutMS <= 0 {
		t.Fatalf("Stripe attachment lacks timeout metadata: %#v", stripe.Statements[0])
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if err != nil || onward.Status != protocol.Planned || len(onward.Statements) != 1 {
		t.Fatalf("onwardpg partition attachment=%#v err=%v", onward, err)
	}
	statement := onward.Statements[0]
	if statement.SQL != `ALTER INDEX "public"."Partitioned_Idx" ATTACH PARTITION "public"."Foobar_3_Part";` || statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 || !containsString(statement.Hazards, "partition_index_attach") {
		t.Fatalf("onwardpg attachment lost SQL/hazards/timeouts: %#v", statement)
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		assertNoOnwardResidual(t, actual, desired)
		if name == "stripe" {
			if residual := runStripePlan(t, ctx, stripeBinary, databaseURL, desiredDir); len(residual.Statements) != 0 {
				t.Fatalf("Stripe attachment residual: %#v", residual)
			}
		}
	}
}

// Adapted from Stripe pg-schema-diff v1.0.7's MIT-licensed primary-key and
// unique-constraint "to partition using existing index" acceptance cases.
func TestPinnedStripeAndOnwardPGAttachExistingPartitionConstraintIndexes(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()
	var serverVersion int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}

	currentDDL := `CREATE TABLE pk_events (id integer NOT NULL, bucket text NOT NULL) PARTITION BY LIST (bucket);
CREATE TABLE pk_events_1 PARTITION OF pk_events FOR VALUES IN ('one');
ALTER TABLE ONLY pk_events ADD CONSTRAINT pk_events_pkey PRIMARY KEY (bucket, id);
CREATE UNIQUE INDEX pk_events_1_pkey ON pk_events_1 (bucket, id);
CREATE TABLE uq_events (id integer NOT NULL, bucket text NOT NULL) PARTITION BY LIST (bucket);
CREATE TABLE uq_events_1 PARTITION OF uq_events FOR VALUES IN ('one');
ALTER TABLE ONLY uq_events ADD CONSTRAINT uq_events_key UNIQUE (bucket, id);
CREATE UNIQUE INDEX uq_events_1_key ON uq_events_1 (bucket, id);`
	desiredDDL := currentDDL + `
ALTER TABLE pk_events_1 ADD CONSTRAINT pk_events_1_pkey PRIMARY KEY USING INDEX pk_events_1_pkey;
ALTER INDEX pk_events_pkey ATTACH PARTITION pk_events_1_pkey;
ALTER TABLE uq_events_1 ADD CONSTRAINT uq_events_1_key UNIQUE USING INDEX uq_events_1_key;
ALTER INDEX uq_events_key ATTACH PARTITION uq_events_1_key;`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	if len(stripe.Statements) != 4 || strings.Count(stripeSQL, "ADD CONSTRAINT") != 2 || strings.Count(stripeSQL, "ATTACH PARTITION") != 2 {
		t.Fatalf("Stripe constraint attachment changed: %#v", stripe)
	}
	for i, statement := range stripe.Statements {
		if statement.TimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("Stripe constraint attachment %d lacks timeout metadata: %#v", i, statement)
		}
	}
	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{ConcurrentIndexes: true})
	if serverVersion >= 180000 {
		if err != nil {
			t.Fatal(err)
		}
		wantPrefix := "not_null_constraint:public.pk_events_1."
		for _, selector := range onward.Unsupported {
			if strings.HasPrefix(selector, wantPrefix) {
				return
			}
		}
		t.Fatalf("PostgreSQL 18 local-plus-inherited NOT NULL state did not block: %#v", onward)
	}
	if err != nil || onward.Status != protocol.Planned || len(onward.Statements) != 4 {
		t.Fatalf("onwardpg constraint attachment=%#v err=%v", onward, err)
	}
	for i, statement := range onward.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 || !containsString(statement.Hazards, "partition_constraint_attach") {
			t.Fatalf("onwardpg constraint attachment %d lacks hazards/timeouts: %#v", i, statement)
		}
	}
	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		assertNoOnwardResidual(t, actual, desired)
		if name == "stripe" {
			if residual := runStripePlan(t, ctx, stripeBinary, databaseURL, desiredDir); len(residual.Statements) != 0 {
				t.Fatalf("Stripe constraint attachment residual: %#v", residual)
			}
		}
	}
}

func TestPinnedStripeNameIdentityDiffersFromOnwardRenameIntent(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE SCHEMA app; CREATE TABLE app.orders (id bigint);`
	desiredDDL := `CREATE SCHEMA app; CREATE TABLE app.purchases (id bigint);`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)
	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	joined := stripeDDL(stripe)
	if !strings.Contains(joined, `CREATE TABLE "app"."purchases"`) || !strings.Contains(joined, `DROP TABLE "app"."orders"`) || !stripeHasHazard(stripe, "DELETES_DATA") {
		t.Fatalf("Stripe reference no longer treats names as identity: %#v", stripe)
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if onward.Status != protocol.NeedsInput || len(onward.Questions) == 0 || onward.Questions[0].Kind != "rename_table" || onward.Questions[0].CurrentFingerprint == "" || onward.Questions[0].DesiredFingerprint == "" || onward.Questions[0].ScopeFingerprint == "" {
		t.Fatalf("onwardpg must preserve fingerprint-bound rename ambiguity: %#v", onward)
	}
}

func TestPinnedStripeAndOnwardPGSequenceOwnershipAndIdentityOptions(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	currentDDL := `CREATE SCHEMA app;
CREATE TABLE app.old_owner (id bigint);
CREATE TABLE app.new_owner (id bigint);
CREATE TABLE app.items (id bigint NOT NULL DEFAULT 5);
CREATE SEQUENCE app.ticket_seq AS bigint START WITH 10 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 3 NO CYCLE;
ALTER SEQUENCE app.ticket_seq OWNED BY app.old_owner.id;`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.old_owner (id bigint);
CREATE TABLE app.new_owner (id bigint);
CREATE TABLE app.items (id bigint GENERATED ALWAYS AS IDENTITY (MINVALUE 2 MAXVALUE 90 START WITH 3 INCREMENT BY 4 CACHE 5 CYCLE));
CREATE SEQUENCE app.ticket_seq AS bigint START WITH 10 INCREMENT BY 2 MINVALUE 1 MAXVALUE 999 CACHE 3 NO CYCLE;
ALTER SEQUENCE app.ticket_seq OWNED BY app.new_owner.id;`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	if strings.Index(stripeSQL, "DROP DEFAULT") > strings.Index(stripeSQL, "ADD GENERATED ALWAYS AS IDENTITY") || !strings.Contains(stripeSQL, `OWNED BY "app"."new_owner"."id"`) {
		t.Fatalf("Stripe reference lost identity/ownership ordering: %#v", stripe)
	}
	for i, statement := range stripe.Statements {
		if statement.TimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("Stripe statement %d lacks timeout metadata: %#v", i, statement)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	onward, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil || onward.Status != protocol.Planned {
		t.Fatalf("onwardpg identity/ownership plan=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	if strings.Index(onwardSQL, "DROP DEFAULT") > strings.Index(onwardSQL, "ADD GENERATED ALWAYS AS IDENTITY") || !strings.Contains(onwardSQL, `OWNED BY "app"."new_owner"."id"`) {
		t.Fatalf("onwardpg lost identity/ownership ordering: %#v", onward)
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe identity/ownership residual: %#v", residual)
	}
}

func TestPinnedStripeAndOnwardPGRowSecurityPoliciesAndPrivileges(t *testing.T) {
	baseURL, stripeBinary := requireStripeReference(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()

	roleName := fmt.Sprintf("onwardpg_stripe_reader_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE ROLE "+quote(roleName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+quote(roleName)) })
	currentDDL := `CREATE SCHEMA app;
CREATE TABLE app.orders (tenant_id bigint NOT NULL, amount bigint NOT NULL);
CREATE POLICY tenant_access ON app.orders AS PERMISSIVE FOR ALL TO PUBLIC USING (tenant_id > 0);
ALTER TABLE app.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.orders FORCE ROW LEVEL SECURITY;
GRANT SELECT ON TABLE app.orders TO "` + roleName + `" WITH GRANT OPTION;
GRANT INSERT ON TABLE app.orders TO PUBLIC;`
	desiredDDL := `CREATE SCHEMA app;
CREATE TABLE app.orders (tenant_id bigint NOT NULL, amount bigint NOT NULL);
CREATE POLICY tenant_access ON app.orders AS PERMISSIVE FOR ALL TO PUBLIC USING (tenant_id > 10) WITH CHECK (amount >= 0);
ALTER TABLE app.orders ENABLE ROW LEVEL SECURITY;
GRANT SELECT ON TABLE app.orders TO "` + roleName + `";`
	urls := createStripeDatabases(t, ctx, admin, baseURL, currentDDL, "source", "stripe", "onward")
	desiredDir, desiredPath := writeStripeDesiredDDL(t, desiredDDL)

	stripe := runStripePlan(t, ctx, stripeBinary, urls["source"], desiredDir)
	stripeSQL := stripeDDL(stripe)
	for _, fragment := range []string{"ALTER POLICY", "NO FORCE ROW LEVEL SECURITY", "REVOKE SELECT", "REVOKE INSERT", "GRANT SELECT"} {
		if !strings.Contains(stripeSQL, fragment) {
			t.Fatalf("Stripe authorization plan missing %q: %#v", fragment, stripe)
		}
	}
	if !stripeHasHazard(stripe, "AUTHZ_UPDATE") {
		t.Fatalf("Stripe authorization plan lacks AUTHZ_UPDATE hazard: %#v", stripe)
	}
	for i, statement := range stripe.Statements {
		if statement.TimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("Stripe authorization statement %d lacks timeout metadata: %#v", i, statement)
		}
	}

	current, err := source.LoadGraph(ctx, source.Parse(urls["source"]), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+desiredPath), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil || pending.Status != protocol.NeedsInput {
		t.Fatalf("onwardpg authorization plan must require intent: plan=%#v err=%v", pending, err)
	}
	values := map[string]string{"alter_policy": "alter", "relax_row_security": "relax", "revoke_grant_option": "revoke", "drop": "drop"}
	answers := protocol.Answers{ProtocolVersion: protocol.Version, CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}
	for _, question := range pending.Questions {
		value, exists := values[question.Kind]
		if !exists {
			t.Fatalf("unexpected onwardpg authorization question: %#v", question)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value, QuestionFingerprint: question.ScopeFingerprint})
	}
	onward, err := graphplan.Build(current, desired, answers, graphplan.Options{})
	if err != nil || onward.Status != protocol.Planned {
		t.Fatalf("onwardpg authorization plan=%#v err=%v", onward, err)
	}
	onwardSQL := joinOnwardSQL(onward)
	for _, fragment := range []string{"ALTER POLICY", "NO FORCE ROW LEVEL SECURITY", "REVOKE GRANT OPTION FOR SELECT", "REVOKE INSERT"} {
		if !strings.Contains(onwardSQL, fragment) {
			t.Fatalf("onwardpg authorization plan missing %q: %#v", fragment, onward)
		}
	}
	for i, statement := range onward.Statements {
		if statement.StatementTimeoutMS <= 0 || statement.LockTimeoutMS <= 0 || !containsString(statement.Hazards, "authorization_change") && !containsString(statement.Hazards, "authorization_relaxation") {
			t.Fatalf("onwardpg authorization statement %d lacks hazards/timeouts: %#v", i, statement)
		}
	}

	applyStripePlan(t, ctx, urls["stripe"], stripe)
	if err := executeOnwardPlan(ctx, urls["onward"], onward); err != nil {
		t.Fatal(err)
	}
	targetFingerprint, _ := desired.Fingerprint()
	for name, databaseURL := range map[string]string{"stripe": urls["stripe"], "onwardpg": urls["onward"]} {
		actual, err := source.LoadGraph(ctx, source.Parse(databaseURL), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s authorization result did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
	if residual := runStripePlan(t, ctx, stripeBinary, urls["stripe"], desiredDir); len(residual.Statements) != 0 {
		t.Fatalf("Stripe authorization residual: %#v", residual)
	}
}

func requireStripeReference(t *testing.T) (string, string) {
	t.Helper()
	baseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL differential tests")
	}
	binary := os.Getenv("STRIPE_PG_SCHEMA_DIFF_BIN")
	if binary == "" {
		binary = filepath.Join("..", "..", ".tools", "pg-schema-diff-v1.0.7")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skip("set STRIPE_PG_SCHEMA_DIFF_BIN to the binary built from " + pinnedStripeReferenceCommit)
	}
	return baseURL, binary
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func createStripeDatabases(t *testing.T, ctx context.Context, admin *pgx.Conn, baseURL, ddl string, labels ...string) map[string]string {
	t.Helper()
	stamp := time.Now().UnixNano()
	result := make(map[string]string, len(labels))
	var names []string
	for _, label := range labels {
		name := fmt.Sprintf("onwardpg_stripe_%d_%s", stamp, label)
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		result[label] = databaseURL(t, baseURL, name)
		if err := executeSQL(ctx, result[label], ddl); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
		}
	})
	return result
}

func writeStripeDesiredDDL(t *testing.T, ddl string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "schema.sql")
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory, path
}

func runStripePlan(t *testing.T, ctx context.Context, binary, fromURL, desiredDir string) stripePlan {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "plan", "--from-dsn", fromURL, "--to-dir", desiredDir, "--output-format", "json", "--data-pack-new-tables=false")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("pinned Stripe plan failed: %v\n%s", err, stderr.String())
	}
	var plan stripePlan
	if err := json.Unmarshal(output, &plan); err != nil {
		t.Fatalf("decode pinned Stripe plan: %v\nstdout=%s\nstderr=%s", err, output, stderr.String())
	}
	return plan
}

func runStripePlanExpectFailure(t *testing.T, ctx context.Context, binary, fromURL, desiredDir string) string {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "plan", "--from-dsn", fromURL, "--to-dir", desiredDir, "--output-format", "json", "--data-pack-new-tables=false")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if output, err := command.Output(); err == nil {
		t.Fatalf("pinned Stripe plan unexpectedly succeeded: %s", output)
	}
	return stderr.String()
}

func assertStripeTimeoutsAndHazards(t *testing.T, plan stripePlan) {
	t.Helper()
	for i, statement := range plan.Statements {
		if statement.TimeoutMS <= 0 || statement.LockTimeoutMS <= 0 {
			t.Fatalf("Stripe statement %d lacks timeout metadata: %#v", i, statement)
		}
	}
	if !stripeHasHazard(plan, "INDEX_BUILD") || !stripeHasHazard(plan, "INDEX_DROPPED") {
		t.Fatalf("Stripe plan lost expected index hazards: %#v", plan)
	}
}

func stripeHasHazard(plan stripePlan, hazard string) bool {
	for _, statement := range plan.Statements {
		for _, candidate := range statement.Hazards {
			if candidate.Type == hazard {
				return true
			}
		}
	}
	return false
}

func stripeDDL(plan stripePlan) string {
	parts := make([]string, len(plan.Statements))
	for i, statement := range plan.Statements {
		parts[i] = statement.DDL
	}
	return strings.Join(parts, ";\n")
}

func applyStripePlanWithInvariant(t *testing.T, ctx context.Context, databaseURL string, plan stripePlan) {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for i, statement := range plan.Statements {
		if _, err := conn.Exec(ctx, "SELECT set_config('statement_timeout', $1, false), set_config('lock_timeout', $2, false)", fmt.Sprint(statement.TimeoutMS), fmt.Sprint(statement.LockTimeoutMS)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, statement.DDL); err != nil {
			t.Fatalf("apply Stripe statement %d: %v\n%s", i, err, statement.DDL)
		}
		assertUsableIndex(t, ctx, conn)
	}
}

func applyStripePlan(t *testing.T, ctx context.Context, databaseURL string, plan stripePlan) {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for i, statement := range plan.Statements {
		if _, err := conn.Exec(ctx, "SELECT set_config('statement_timeout', $1, false), set_config('lock_timeout', $2, false)", fmt.Sprint(statement.TimeoutMS), fmt.Sprint(statement.LockTimeoutMS)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, statement.DDL); err != nil {
			t.Fatalf("apply Stripe statement %d: %v\n%s", i, err, statement.DDL)
		}
	}
}

func executeStripePlan(ctx context.Context, databaseURL string, plan stripePlan) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, statement := range plan.Statements {
		if _, err := conn.Exec(ctx, statement.DDL); err != nil {
			return err
		}
	}
	return nil
}

func joinOnwardSQL(plan protocol.Result) string {
	parts := make([]string, len(plan.Statements))
	for i, statement := range plan.Statements {
		parts[i] = statement.SQL
	}
	return strings.Join(parts, "\n")
}

func buildOnwardWithAssertions(current, desired *pgschema.Snapshot, options graphplan.Options) (protocol.Result, error) {
	answers := protocol.Answers{}
	for attempt := 0; attempt < 5; attempt++ {
		result, err := graphplan.Build(current, desired, answers, options)
		if err != nil || result.Status != protocol.NeedsInput {
			return result, err
		}
		if answers.ProtocolVersion == "" {
			answers.ProtocolVersion = protocol.Version
			answers.CurrentFingerprint = result.CurrentFingerprint
			answers.DesiredFingerprint = result.DesiredFingerprint
		}
		for _, question := range result.Questions {
			if question.Kind != "reconcile_contract" || !containsChoice(question.Choices, "assert_only") {
				return result, nil
			}
			answers.Answers = append(answers.Answers, protocol.Answer{
				Kind: question.Kind, Key: question.Key, Value: "assert_only", QuestionFingerprint: question.ScopeFingerprint,
			})
		}
	}
	return protocol.Result{}, fmt.Errorf("onwardpg reconciliation questions did not converge")
}

func editOnwardTypeHandoff(plan protocol.Result, contractSQL string) protocol.Result {
	rewrite := func(statement protocol.Statement) protocol.Statement {
		switch {
		case strings.Contains(statement.SQL, "reviewed EXPAND SQL"):
			statement.SQL = "SELECT true;"
			statement.Manual = nil
		case strings.Contains(statement.SQL, "reviewed CONTRACT SQL"):
			statement.SQL = contractSQL
			statement.Manual = nil
		}
		return statement
	}
	for index := range plan.Statements {
		plan.Statements[index] = rewrite(plan.Statements[index])
	}
	for batchIndex := range plan.Batches {
		for statementIndex := range plan.Batches[batchIndex].Statements {
			plan.Batches[batchIndex].Statements[statementIndex] = rewrite(plan.Batches[batchIndex].Statements[statementIndex])
		}
	}
	return plan
}

func applyOnwardPlanWithInvariant(t *testing.T, ctx context.Context, databaseURL string, plan protocol.Result) {
	t.Helper()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for i, statement := range plan.Statements {
		if statement.StatementTimeoutMS > 0 || statement.LockTimeoutMS > 0 {
			if _, err := conn.Exec(ctx, "SELECT set_config('statement_timeout', $1, false), set_config('lock_timeout', $2, false)", fmt.Sprint(statement.StatementTimeoutMS), fmt.Sprint(statement.LockTimeoutMS)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := conn.Exec(ctx, statement.SQL); err != nil {
			t.Fatalf("apply onwardpg statement %d: %v\n%s", i, err, statement.SQL)
		}
		assertUsableIndex(t, ctx, conn)
	}
}

func assertUsableIndex(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	var count int
	err := conn.QueryRow(ctx, `
SELECT count(*)
FROM pg_index i
JOIN pg_class idx ON idx.oid = i.indexrelid
JOIN pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_namespace n ON n.oid = tbl.relnamespace
WHERE n.nspname = 'app' AND tbl.relname = 'orders' AND i.indisvalid AND i.indisready`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatal("index replacement created an availability window with no usable index")
	}
}
