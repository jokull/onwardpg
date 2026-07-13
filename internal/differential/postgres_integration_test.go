// Package differential compares applied Atlas and onwardpg plans by resulting
// PostgreSQL graph state, not by byte-for-byte SQL output.
package differential

import (
	"context"
	"fmt"
	"net/url"
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

const pinnedAtlasCommit = "a5e0aecc2bb64143bf522734f8ad88e04885fca6"
const integrationLock int64 = 731095702114

func TestPinnedAtlasAndOnwardPGConvergeForCreateCorpus(t *testing.T) {
	baseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL differential tests")
	}
	atlas := os.Getenv("ATLAS_PINNED_BIN")
	if atlas == "" {
		atlas = "/tmp/onwardpg-tools/atlas-pinned"
	}
	if _, err := os.Stat(atlas); err != nil {
		t.Skip("set ATLAS_PINNED_BIN to the Atlas binary built from " + pinnedAtlasCommit)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	requirePostgresVersion(t, ctx, admin, 150000, "NULLS NOT DISTINCT differential corpus")
	// All integration packages share this database as their administrative
	// connection. Serialize setup so another package cannot observe one of
	// this test's disposable databases or schemas while it snapshots state.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()
	stamp := time.Now().UTC().Format("20060102150405")
	currentName, atlasName, onwardName, devName := "onwardpg_diff_current_"+stamp, "onwardpg_diff_atlas_"+stamp, "onwardpg_diff_onward_"+stamp, "onwardpg_diff_dev_"+stamp
	for _, name := range []string{currentName, atlasName, onwardName, devName} {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, name := range []string{currentName, atlasName, onwardName, devName} {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
		}
	}()
	currentURL := databaseURL(t, baseURL, currentName)
	atlasURL := databaseURL(t, baseURL, atlasName)
	onwardURL := databaseURL(t, baseURL, onwardName)
	devURL := databaseURL(t, baseURL, devName)
	desiredDDL := `CREATE SCHEMA app;
CREATE TYPE app.state AS ENUM ('open', 'closed');
CREATE TABLE app.orders (
  id bigint GENERATED ALWAYS AS IDENTITY,
  legacy_id serial,
  state app.state NOT NULL DEFAULT 'open',
  note text COLLATE "C",
  note_length integer GENERATED ALWAYS AS (char_length(note)) STORED
);
CREATE UNIQUE INDEX orders_state_idx ON app.orders USING btree (state DESC NULLS LAST)
  INCLUDE (id) NULLS NOT DISTINCT WHERE state IS NOT NULL;`
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte(desiredDDL), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := exec.CommandContext(ctx, atlas, "schema", "diff", "--from", currentURL, "--to", "file://"+path, "--dev-url", devURL, "--format", "{{ sql . \"\" }}").CombinedOutput()
	if err != nil {
		t.Fatalf("pinned Atlas diff failed: %v\n%s", err, output)
	}
	atlasSQL := extractAtlasSQL(string(output))
	if err := executeSQL(ctx, atlasURL, atlasSQL); err != nil {
		t.Fatalf("apply pinned Atlas plan: %v\n%s", err, output)
	}

	current, err := source.LoadGraph(ctx, source.Parse(currentURL), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned {
		t.Fatalf("onwardpg did not plan corpus case: %#v", plan)
	}
	if err := executeOnwardPlan(ctx, onwardURL, plan); err != nil {
		t.Fatal(err)
	}

	targetFingerprint, _ := desired.Fingerprint()
	for name, url := range map[string]string{"atlas": atlasURL, "onwardpg": onwardURL} {
		actual, err := source.LoadGraph(ctx, source.Parse(url), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _ := actual.Fingerprint()
		if fingerprint != targetFingerprint {
			t.Fatalf("%s did not converge: got %s want %s", name, fingerprint, targetFingerprint)
		}
		assertNoOnwardResidual(t, actual, desired)
	}
}

func TestPinnedAtlasAndOnwardPGConvergeForMutationCorpus(t *testing.T) {
	baseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL differential tests")
	}
	atlas := os.Getenv("ATLAS_PINNED_BIN")
	if atlas == "" {
		atlas = "/tmp/onwardpg-tools/atlas-pinned"
	}
	if _, err := os.Stat(atlas); err != nil {
		t.Skip("set ATLAS_PINNED_BIN to the Atlas binary built from " + pinnedAtlasCommit)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()
	cases := []struct {
		name       string
		currentDDL string
		desiredDDL string
	}{
		{
			name:       "default-change",
			currentDDL: "CREATE SCHEMA app; CREATE TABLE app.orders (value integer DEFAULT 1);",
			desiredDDL: "CREATE SCHEMA app; CREATE TABLE app.orders (value integer DEFAULT 2);",
		},
		{
			name:       "default-semantic-equivalence",
			currentDDL: "CREATE SCHEMA app; CREATE TABLE app.orders (created_at timestamptz DEFAULT now());",
			desiredDDL: "CREATE SCHEMA app; CREATE TABLE app.orders (created_at timestamptz DEFAULT CURRENT_TIMESTAMP);",
		},
		{
			name: "table-and-column-comment-change",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
COMMENT ON TABLE app.orders IS 'orders';
COMMENT ON COLUMN app.orders.id IS 'identifier';`,
		},
		{
			name: "table-and-column-comment-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
COMMENT ON TABLE app.orders IS 'orders';
COMMENT ON COLUMN app.orders.id IS 'identifier';`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name:       "schema-comment-change",
			currentDDL: "CREATE SCHEMA app;",
			desiredDDL: "CREATE SCHEMA app; COMMENT ON SCHEMA app IS 'application schema';",
		},
		{
			name:       "schema-comment-drop",
			currentDDL: "CREATE SCHEMA app; COMMENT ON SCHEMA app IS 'application schema';",
			desiredDDL: "CREATE SCHEMA app;",
		},
		{
			name: "column-and-check-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, quantity integer);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, quantity integer, note text DEFAULT 'new',
  CONSTRAINT orders_quantity_positive CHECK (quantity > 0));`,
		},
		{
			name: "identity-option-modify",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint GENERATED BY DEFAULT AS IDENTITY (START WITH 1 INCREMENT BY 1));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint GENERATED ALWAYS AS IDENTITY (START WITH 10 INCREMENT BY 5));`,
		},
		{
			name: "enum-value-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TYPE app.state AS ENUM ('open');
CREATE TABLE app.orders (state app.state);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TYPE app.state AS ENUM ('open', 'closed');
CREATE TABLE app.orders (state app.state);`,
		},
		{
			name: "enum-value-add-before",
			currentDDL: `CREATE SCHEMA app;
CREATE TYPE app.state AS ENUM ('open', 'closed');
CREATE TABLE app.orders (state app.state);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TYPE app.state AS ENUM ('open', 'pending', 'closed');
CREATE TABLE app.orders (state app.state);`,
		},
		{
			name: "structured-index-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, state text, created_at timestamptz);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, state text, created_at timestamptz);
CREATE INDEX orders_state_idx ON app.orders USING btree (state DESC NULLS LAST) INCLUDE (created_at) WHERE state IS NOT NULL;`,
		},
		{
			name: "index-rebuild",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, state text);
CREATE INDEX orders_state_idx ON app.orders (state);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, state text);
CREATE INDEX orders_state_idx ON app.orders (state DESC NULLS LAST) WHERE state IS NOT NULL;`,
		},
		{
			name: "foreign-key-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES app.accounts(id)
    ON DELETE CASCADE);`,
		},
		{
			name: "foreign-key-modify-action",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES app.accounts(id));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES app.accounts(id) ON DELETE CASCADE);`,
		},
		{
			name: "not-valid-check-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, quantity integer);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, quantity integer,
  CONSTRAINT orders_quantity_positive CHECK (quantity > 0) NOT VALID);`,
		},
		{
			name: "no-inherit-check-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, quantity integer);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint PRIMARY KEY, quantity integer,
  CONSTRAINT orders_quantity_positive CHECK (quantity > 0) NO INHERIT);`,
		},
		{
			name: "not-valid-foreign-key-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES app.accounts(id) NOT VALID);`,
		},
		{
			name: "unique-nulls-not-distinct-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint,
  CONSTRAINT orders_id_key UNIQUE NULLS NOT DISTINCT (id));`,
		},
		{
			name: "primary-key-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL, CONSTRAINT orders_pkey PRIMARY KEY (id));`,
		},
		{
			name: "primary-key-using-existing-index",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL);
CREATE UNIQUE INDEX orders_id_key ON app.orders (id);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL);
CREATE UNIQUE INDEX orders_id_key ON app.orders (id);
ALTER TABLE app.orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_id_key;`,
		},
		{
			name: "column-nullability-set",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL);`,
		},
		{
			name: "column-type-change",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id integer);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name: "indexed-column-type-change",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id integer);
CREATE INDEX orders_id_idx ON app.orders (id);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
CREATE INDEX orders_id_idx ON app.orders (id);`,
		},
		{
			name: "unique-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint, CONSTRAINT orders_id_key UNIQUE (id));`,
		},
		{
			name: "index-comment-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
CREATE INDEX orders_id_idx ON app.orders (id);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
CREATE INDEX orders_id_idx ON app.orders (id);
COMMENT ON INDEX app.orders_id_idx IS 'lookup';`,
		},
		{
			name: "expression-index-with-opclass-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (name text);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (name text);
CREATE INDEX orders_name_pattern_idx ON app.orders ((lower(name)) text_pattern_ops);`,
		},
		{
			name: "index-opclass-parameter-add",
			currentDDL: `CREATE EXTENSION pg_trgm;
CREATE SCHEMA app;
CREATE TABLE app.documents (body text);`,
			desiredDDL: `CREATE EXTENSION pg_trgm;
CREATE SCHEMA app;
CREATE TABLE app.documents (body text);
CREATE INDEX documents_body_trgm_idx ON app.documents USING gist (body gist_trgm_ops (siglen = 32));`,
		},
		{
			name: "brin-index-storage-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.events (created_at timestamptz);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.events (created_at timestamptz);
CREATE INDEX events_created_at_brin ON app.events USING brin (created_at) WITH (pages_per_range = 64, autosummarize = true);`,
		},
		{
			name: "hash-index-add",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.sessions (token text);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.sessions (token text);
CREATE INDEX sessions_token_hash_idx ON app.sessions USING hash (token);`,
		},
		{
			name: "generated-column-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (quantity integer, doubled integer GENERATED ALWAYS AS (quantity * 2) STORED);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (quantity integer);`,
		},
		{
			name: "default-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint DEFAULT 1);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name: "column-nullability-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name: "primary-key-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL, CONSTRAINT orders_pkey PRIMARY KEY (id));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL);`,
		},
		{
			name: "primary-key-modify",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL, tenant_id bigint NOT NULL,
  CONSTRAINT orders_pkey PRIMARY KEY (id));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint NOT NULL, tenant_id bigint NOT NULL,
  CONSTRAINT orders_pkey PRIMARY KEY (id, tenant_id));`,
		},
		{
			name: "foreign-key-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint,
  CONSTRAINT orders_account_id_fkey FOREIGN KEY (account_id) REFERENCES app.accounts(id));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.accounts (id bigint PRIMARY KEY);
CREATE TABLE app.orders (id bigint PRIMARY KEY, account_id bigint);`,
		},
		{
			name: "check-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (quantity integer, CONSTRAINT orders_quantity_positive CHECK (quantity > 0));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (quantity integer);`,
		},
		{
			name: "exclusion-constraint-add",
			currentDDL: `CREATE EXTENSION btree_gist;
CREATE SCHEMA app;
CREATE TABLE app.bookings (room_id integer, during tsrange);`,
			desiredDDL: `CREATE EXTENSION btree_gist;
CREATE SCHEMA app;
CREATE TABLE app.bookings (room_id integer, during tsrange,
  CONSTRAINT bookings_no_overlap EXCLUDE USING gist (room_id WITH =, during WITH &&));`,
		},
		{
			name: "exclusion-constraint-drop",
			currentDDL: `CREATE EXTENSION btree_gist;
CREATE SCHEMA app;
CREATE TABLE app.bookings (room_id integer, during tsrange,
  CONSTRAINT bookings_no_overlap EXCLUDE USING gist (room_id WITH =, during WITH &&));`,
			desiredDDL: `CREATE EXTENSION btree_gist;
CREATE SCHEMA app;
CREATE TABLE app.bookings (room_id integer, during tsrange);`,
		},
		{
			name:       "partitioned-table-range-create",
			currentDDL: "CREATE SCHEMA app;",
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.events (occurred_on date) PARTITION BY RANGE (occurred_on);`,
		},
		{
			name:       "partitioned-table-list-create",
			currentDDL: "CREATE SCHEMA app;",
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.events (region text) PARTITION BY LIST (region);`,
		},
		{
			name:       "partitioned-table-hash-create",
			currentDDL: "CREATE SCHEMA app;",
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.events (account_id bigint) PARTITION BY HASH (account_id);`,
		},
		{
			name:       "partitioned-table-expression-create",
			currentDDL: "CREATE SCHEMA app;",
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.events (name text) PARTITION BY RANGE (lower(name));`,
		},
		{
			name:       "foreign-key-cycle-create",
			currentDDL: "CREATE SCHEMA app;",
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.left_nodes (id bigint PRIMARY KEY, right_id bigint);
CREATE TABLE app.right_nodes (id bigint PRIMARY KEY, left_id bigint,
  CONSTRAINT right_nodes_left_fkey FOREIGN KEY (left_id) REFERENCES app.left_nodes(id));
ALTER TABLE app.left_nodes ADD CONSTRAINT left_nodes_right_fkey
  FOREIGN KEY (right_id) REFERENCES app.right_nodes(id);`,
		},
		{
			name: "index-comment-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
CREATE INDEX orders_id_idx ON app.orders (id);
COMMENT ON INDEX app.orders_id_idx IS 'lookup';`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
CREATE INDEX orders_id_idx ON app.orders (id);`,
		},
		{
			name: "column-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint, obsolete text);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name: "table-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
			desiredDDL: `CREATE SCHEMA app;`,
		},
		{
			name: "index-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);
CREATE INDEX orders_id_idx ON app.orders (id);`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name: "unique-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint, CONSTRAINT orders_id_key UNIQUE (id));`,
			desiredDDL: `CREATE SCHEMA app;
CREATE TABLE app.orders (id bigint);`,
		},
		{
			name: "enum-drop",
			currentDDL: `CREATE SCHEMA app;
CREATE TYPE app.state AS ENUM ('open');`,
			desiredDDL: `CREATE SCHEMA app;`,
		},
	}
	for index, corpus := range cases {
		t.Run(corpus.name, func(t *testing.T) {
			stamp := time.Now().UTC().Format("20060102150405") + fmt.Sprintf("_%02d", index)
			currentName := "onwardpg_diff_current_" + stamp
			atlasName := "onwardpg_diff_atlas_" + stamp
			onwardName := "onwardpg_diff_onward_" + stamp
			devName := "onwardpg_diff_dev_" + stamp
			for _, name := range []string{currentName, atlasName, onwardName, devName} {
				if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				for _, name := range []string{currentName, atlasName, onwardName, devName} {
					_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
				}
			}()
			currentURL := databaseURL(t, baseURL, currentName)
			atlasURL := databaseURL(t, baseURL, atlasName)
			onwardURL := databaseURL(t, baseURL, onwardName)
			devURL := databaseURL(t, baseURL, devName)
			for _, target := range []string{currentURL, atlasURL, onwardURL} {
				if err := executeSQL(ctx, target, corpus.currentDDL); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "desired.sql")
			if err := os.WriteFile(path, []byte(corpus.desiredDDL), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.CommandContext(ctx, atlas, "schema", "diff", "--from", currentURL, "--to", "file://"+path, "--dev-url", devURL, "--format", "{{ sql . \"\" }}").CombinedOutput()
			if err != nil {
				t.Fatalf("pinned Atlas diff failed: %v\n%s", err, output)
			}
			if err := executeSQL(ctx, atlasURL, extractAtlasSQL(string(output))); err != nil {
				t.Fatalf("apply pinned Atlas plan: %v\n%s", err, output)
			}
			current, err := source.LoadGraph(ctx, source.Parse(currentURL), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := source.LoadGraph(ctx, source.Parse("file://"+path), baseURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := graphplan.Build(current, desired, protocol.Answers{}, graphplan.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status == protocol.NeedsInput {
				answers, err := directAnswers(plan)
				if err != nil {
					t.Fatal(err)
				}
				plan, err = graphplan.Build(current, desired, answers, graphplan.Options{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if plan.Status != protocol.Planned {
				t.Fatalf("onwardpg did not plan corpus case: %#v", plan)
			}
			if err := executeOnwardPlan(ctx, onwardURL, plan); err != nil {
				t.Fatal(err)
			}
			targetFingerprint, _ := desired.Fingerprint()
			for name, target := range map[string]string{"atlas": atlasURL, "onwardpg": onwardURL} {
				actual, err := source.LoadGraph(ctx, source.Parse(target), "", nil)
				if err != nil {
					t.Fatal(err)
				}
				fingerprint, _ := actual.Fingerprint()
				if fingerprint != targetFingerprint {
					t.Fatalf("%s did not converge: got %s want %s", name, fingerprint, targetFingerprint)
				}
				assertNoOnwardResidual(t, actual, desired)
			}
		})
	}
}

func TestPinnedAtlasRejectsEnumReorder(t *testing.T) {
	baseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL differential tests")
	}
	atlas := os.Getenv("ATLAS_PINNED_BIN")
	if atlas == "" {
		atlas = "/tmp/onwardpg-tools/atlas-pinned"
	}
	if _, err := os.Stat(atlas); err != nil {
		t.Skip("set ATLAS_PINNED_BIN to the Atlas binary built from " + pinnedAtlasCommit)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()
	stamp := time.Now().UTC().Format("20060102150405")
	currentName, devName := "onwardpg_reject_current_"+stamp, "onwardpg_reject_dev_"+stamp
	for _, name := range []string{currentName, devName} {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, name := range []string{currentName, devName} {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
		}
	}()
	currentURL, devURL := databaseURL(t, baseURL, currentName), databaseURL(t, baseURL, devName)
	if err := executeSQL(ctx, currentURL, "CREATE SCHEMA app; CREATE TYPE app.state AS ENUM ('open', 'closed');"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte("CREATE SCHEMA app; CREATE TYPE app.state AS ENUM ('closed', 'open');"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, atlas, "schema", "diff", "--from", currentURL, "--to", "file://"+path, "--dev-url", devURL, "--format", "{{ sql . \"\" }}").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "reordering enum") {
		t.Fatalf("pinned Atlas did not reject enum reorder: err=%v output=%s", err, output)
	}
}

func TestPinnedAtlasIgnoresEnumLabelDrop(t *testing.T) {
	baseURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to run PostgreSQL differential tests")
	}
	atlas := os.Getenv("ATLAS_PINNED_BIN")
	if atlas == "" {
		atlas = "/tmp/onwardpg-tools/atlas-pinned"
	}
	if _, err := os.Stat(atlas); err != nil {
		t.Skip("set ATLAS_PINNED_BIN to the Atlas binary built from " + pinnedAtlasCommit)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", integrationLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", integrationLock) }()
	stamp := time.Now().UTC().Format("20060102150405")
	currentName, devName := "onwardpg_ignore_current_"+stamp, "onwardpg_ignore_dev_"+stamp
	for _, name := range []string{currentName, devName} {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, name := range []string{currentName, devName} {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
		}
	}()
	currentURL, devURL := databaseURL(t, baseURL, currentName), databaseURL(t, baseURL, devName)
	if err := executeSQL(ctx, currentURL, "CREATE SCHEMA app; CREATE TYPE app.state AS ENUM ('open', 'closed');"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "desired.sql")
	if err := os.WriteFile(path, []byte("CREATE SCHEMA app; CREATE TYPE app.state AS ENUM ('open');"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, atlas, "schema", "diff", "--from", currentURL, "--to", "file://"+path, "--dev-url", devURL, "--format", "{{ sql . \"\" }}").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "" {
		t.Fatalf("pinned Atlas did not silently ignore enum label drop: err=%v output=%s", err, output)
	}
}

// directAnswers makes the explicit, reviewable choices used by the narrow
// differential cases that exercise Atlas's direct ALTER behavior. New question
// kinds must be opted in here instead of silently guessing an answer.
func directAnswers(result protocol.Result) (protocol.Answers, error) {
	answers := protocol.Answers{
		ProtocolVersion:    protocol.Version,
		CurrentFingerprint: result.CurrentFingerprint,
		DesiredFingerprint: result.DesiredFingerprint,
	}
	for _, question := range result.Questions {
		var value string
		switch question.Kind {
		case "set_not_null", "type_change":
			value = "direct"
		case "drop":
			value = "drop"
		default:
			return protocol.Answers{}, fmt.Errorf("differential case requires an explicit unsupported answer kind %q", question.Kind)
		}
		answers.Answers = append(answers.Answers, protocol.Answer{Kind: question.Kind, Key: question.Key, Value: value})
	}
	return answers, nil
}

func assertNoOnwardResidual(t *testing.T, current, target *pgschema.Snapshot) {
	t.Helper()
	plan, err := graphplan.Build(current, target, protocol.Answers{}, graphplan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != protocol.Planned || len(plan.Statements) != 0 || len(plan.Batches) != 0 {
		t.Fatalf("residual onwardpg diff: %#v", plan)
	}
}

func requirePostgresVersion(t *testing.T, ctx context.Context, conn *pgx.Conn, minimum int, feature string) {
	t.Helper()
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < minimum {
		t.Skipf("%s requires PostgreSQL %d or newer; server is %d", feature, minimum/10000, version/10000)
	}
}

func extractAtlasSQL(output string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-- ") || strings.HasPrefix(trimmed, "CREATE ") || strings.HasPrefix(trimmed, "ALTER ") || strings.HasPrefix(trimmed, "DROP ") || strings.HasPrefix(trimmed, "COMMENT ") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

func TestExtractAtlasSQLSkipsCommunityNotice(t *testing.T) {
	output := "Notice: community edition\n\n-- Add schema\nCREATE SCHEMA app;\n"
	if got, want := extractAtlasSQL(output), "-- Add schema\nCREATE SCHEMA app;\n"; got != want {
		t.Fatalf("extractAtlasSQL = %q, want %q", got, want)
	}
}

func databaseURL(t *testing.T, baseURL, database string) string {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path, parsed.RawPath = "/"+database, ""
	return parsed.String()
}

func executeSQL(ctx context.Context, url, sql string) error {
	if strings.TrimSpace(sql) == "" {
		return nil
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql)
	return err
}

func executeOnwardPlan(ctx context.Context, url string, plan protocol.Result) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, batch := range plan.Batches {
		if !batch.Transactional {
			for _, statement := range batch.Statements {
				if _, err := conn.Exec(ctx, statement.SQL); err != nil {
					return err
				}
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		for _, statement := range batch.Statements {
			if _, err := tx.Exec(ctx, statement.SQL); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func quote(identifier string) string { return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"` }
