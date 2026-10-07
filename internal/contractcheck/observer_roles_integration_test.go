package contractcheck

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/pgschema"
)

// observerRoleFixture is a database with one object of each catalog family
// that the inspection reads through a function or a view, in schemas that
// PUBLIC cannot use, with rows behind row-level security and sequences that
// have moved. If the inspected graph depended on a privilege of the reader,
// one of these objects would show it.
const observerRoleFixture = `
CREATE SCHEMA app;
CREATE SCHEMA private;
CREATE EXTENSION citext WITH SCHEMA app;
CREATE TYPE app.status AS ENUM ('new', 'done');
CREATE DOMAIN app.positive AS integer CHECK (VALUE > 0);
CREATE TYPE app.pair AS (a integer, b text);
CREATE TYPE app.span AS RANGE (subtype = integer);
CREATE SEQUENCE app.ticket_seq START 100 INCREMENT 5;
CREATE FUNCTION app.slug(text) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT lower($1) $$;
CREATE TABLE app.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email app.citext NOT NULL UNIQUE,
  status app.status NOT NULL DEFAULT 'new',
  quota app.positive,
  ticket bigint DEFAULT nextval('app.ticket_seq'),
  nickname text,
  label text GENERATED ALWAYS AS (lower(nickname)) STORED,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT accounts_quota_check CHECK (quota IS NULL OR quota < 1000)
);
COMMENT ON TABLE app.accounts IS 'accounts';
CREATE TABLE private.orders (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL REFERENCES app.accounts (id) ON DELETE CASCADE,
  amount numeric(10, 2) NOT NULL,
  during app.span,
  note text
);
CREATE INDEX orders_account_idx ON private.orders (account_id) WHERE amount > 0;
CREATE INDEX accounts_slug_idx ON app.accounts (app.slug(nickname));
CREATE VIEW app.open_orders AS
  SELECT o.id, a.email FROM private.orders o JOIN app.accounts a ON a.id = o.account_id;
CREATE MATERIALIZED VIEW private.order_totals AS
  SELECT account_id, sum(amount) AS total FROM private.orders GROUP BY account_id;
CREATE UNIQUE INDEX order_totals_idx ON private.order_totals (account_id);
CREATE TABLE private.events (id bigint, at date NOT NULL) PARTITION BY RANGE (at);
CREATE TABLE private.events_2026 PARTITION OF private.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE FUNCTION private.touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.note := coalesce(NEW.note, ''); RETURN NEW; END $$;
CREATE TRIGGER orders_touch BEFORE INSERT ON private.orders FOR EACH ROW EXECUTE FUNCTION private.touch();
ALTER TABLE private.orders ENABLE ROW LEVEL SECURITY;
CREATE POLICY orders_visible ON private.orders USING (account_id > 0);
INSERT INTO app.accounts (email) VALUES ('a@example.com'), ('b@example.com');
INSERT INTO private.orders (account_id, amount) SELECT id, 10 FROM app.accounts;
`

// Drift check reads system catalogs only. This test proves on real
// PostgreSQL what that needs: a login role with no privilege on an
// application object, and a login role that is a member of predefined
// read-only roles, read exactly the graph that the database owner reads.
// It also proves which roles the observer guard still refuses.
func TestCatalogInspectionGivesEachValidObserverTheGraphOfTheOwner(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "onwardpg_obs")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	const password = "observer-roles-test-password"
	const plain = " NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
	name := func(suffix string) string { return database.Role + "_" + suffix }
	quoted := func(suffix string) string { return pgx.Identifier{name(suffix)}.Sanitize() }
	login := func(suffix, options string) string {
		return "CREATE ROLE " + quoted(suffix) + " LOGIN PASSWORD '" + password + "'" + plain + " " + options + ";" +
			"GRANT CONNECT ON DATABASE " + pgx.Identifier{database.Name}.Sanitize() + " TO " + quoted(suffix) + ";"
	}
	roles := []string{
		"bare", "reader", "monitor", "bypass", "grants", "dedicated", "application",
		"writer", "creator", "administrator", "person", "impersonator", "nested_grants", "nested",
	}
	defer func() {
		_ = admin.Close(context.Background())
		_ = database.Close()
		cleanup, cleanupErr := pgx.Connect(context.Background(), adminURL)
		if cleanupErr != nil {
			return
		}
		defer cleanup.Close(context.Background())
		for index := len(roles) - 1; index >= 0; index-- {
			_, _ = cleanup.Exec(context.Background(), "DROP ROLE IF EXISTS "+quoted(roles[index]))
		}
	}()
	setup := "CREATE ROLE " + quoted("application") + " NOLOGIN" + plain + ";" +
		// Valid observers.
		login("bare", "") +
		login("reader", "IN ROLE pg_read_all_data") +
		login("monitor", "IN ROLE pg_monitor, pg_read_all_data") +
		// A reader that row-level security hides no row from. It can write nothing.
		login("bypass", "IN ROLE pg_read_all_data, pg_read_all_stats") + "ALTER ROLE " + quoted("bypass") + " BYPASSRLS;" +
		"CREATE ROLE " + quoted("grants") + " NOLOGIN" + plain + ";" +
		login("dedicated", "IN ROLE "+quoted("grants")+", pg_read_all_stats") +
		// Roles that the guard must refuse.
		login("writer", "IN ROLE pg_read_all_data, pg_write_all_data") +
		login("creator", "IN ROLE pg_read_all_data") + "ALTER ROLE " + quoted("creator") + " CREATEDB;" +
		login("administrator", "") + "GRANT pg_read_all_data TO " + quoted("administrator") + " WITH ADMIN OPTION;" +
		"CREATE ROLE " + quoted("person") + " LOGIN" + plain + ";" +
		login("impersonator", "IN ROLE "+quoted("person")) +
		"CREATE ROLE " + quoted("nested_grants") + " NOLOGIN" + plain + " IN ROLE pg_read_all_data;" +
		login("nested", "IN ROLE "+quoted("nested_grants"))
	if _, err := admin.Exec(ctx, setup); err != nil {
		t.Fatal(err)
	}

	owner, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, observerRoleFixture+
		"GRANT SELECT ON app.accounts TO "+quoted("application")+";"+
		// A grant to a predefined role is application state, not observer access.
		"GRANT SELECT ON app.accounts TO pg_read_all_data;"+
		"GRANT USAGE ON SCHEMA app TO "+quoted("grants")+";"+
		"GRANT SELECT ON app.accounts TO "+quoted("grants")+";"); err != nil {
		t.Fatal(err)
	}

	urlFor := func(suffix string) string {
		config := database.Config.Copy()
		config.User, config.Password = name(suffix), password
		return restrictedScratchURL(config)
	}
	canonical := func(snapshot *pgschema.Snapshot) string {
		t.Helper()
		body, err := snapshot.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	// The owner sees the grants of the dedicated role, which belong to no
	// identity of the owner. Remove them once to get the graph that every
	// observer must read.
	asOwner, ownerProjection, finding, err := InspectObserverCatalog(ctx, restrictedScratchURL(database.Config), nil, nil, 10*time.Second)
	if err != nil || finding != nil || ownerProjection.Mode != "database_owner" {
		t.Fatalf("owner inspection: %v, %#v, %#v", err, finding, ownerProjection)
	}
	if len(asOwner.Unsupported()) != 1 || asOwner.Unsupported()[0] != "acl:schema:app" {
		t.Fatalf("owner sees unsupported state other than the schema grant of the dedicated role: %#v", asOwner.Unsupported())
	}
	grantsRole := name("grants")
	expected, err := asOwner.Project(func(object pgschema.Object) (pgschema.Object, bool) {
		if privilege, ok := object.(pgschema.TablePrivilege); ok && privilege.Grantee == grantsRole {
			return nil, false
		}
		return object, true
	}, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	predefinedGrant := (pgschema.TablePrivilege{Table: (pgschema.Table{Schema: "app", Name: "accounts"}).ObjectID(), Grantee: "pg_read_all_data", Privilege: "SELECT"}).ObjectID()
	if _, exists := expected.Object(predefinedGrant); !exists {
		t.Fatal("the owner does not see the application grant to pg_read_all_data")
	}
	want := canonical(expected)

	for _, test := range []struct {
		role string
		mode string
		// projected is the number of grants removed as observer access.
		projected int
		bypassRLS bool
	}{
		// No membership and no grant: not even USAGE on the schemas.
		{role: "bare", mode: "dedicated_read_only"},
		{role: "reader", mode: "predefined_read_role"},
		// pg_monitor holds three predefined read-only roles itself.
		{role: "monitor", mode: "predefined_read_role"},
		// Row-level security is on for private.orders. The catalog graph is
		// the same for a role that it applies to and for a role that bypasses it.
		{role: "bypass", mode: "predefined_read_role", bypassRLS: true},
		// The exact grants of the dedicated role are removed and reported.
		{role: "dedicated", mode: "predefined_read_role", projected: 2},
	} {
		t.Run(test.role+" reads the graph of the owner", func(t *testing.T) {
			snapshot, projection, finding, err := InspectObserverCatalog(ctx, urlFor(test.role), nil, nil, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if finding != nil {
				t.Fatalf("finding = %#v", finding)
			}
			if projection.Mode != test.mode || projection.Role != name(test.role) || projection.DatabaseOwner != database.Role || projection.BypassRLS != test.bypassRLS {
				t.Fatalf("projection = %#v, want mode %s and bypass_rls %t", projection, test.mode, test.bypassRLS)
			}
			observed := snapshot
			if test.projected == 0 {
				// This observer also sees the grants of the dedicated role.
				if len(snapshot.Unsupported()) != 1 || snapshot.Unsupported()[0] != "acl:schema:app" {
					t.Fatalf("unsupported = %#v", snapshot.Unsupported())
				}
				observed, err = snapshot.Project(func(object pgschema.Object) (pgschema.Object, bool) {
					if privilege, ok := object.(pgschema.TablePrivilege); ok && privilege.Grantee == grantsRole {
						return nil, false
					}
					return object, true
				}, func(string) bool { return false })
				if err != nil {
					t.Fatal(err)
				}
			}
			// Each observer also lists the ownership of the database owner,
			// which is the ambient identity of the owner's own inspection.
			grants := 0
			for _, entry := range projection.ProjectedAccess {
				if !strings.HasPrefix(entry, "ownership:") {
					grants++
				}
			}
			if grants != test.projected {
				t.Fatalf("projected access = %#v, want %d grants", projection.ProjectedAccess, test.projected)
			}
			if got := canonical(observed); got != want {
				t.Fatalf("the graph depends on the privileges of the reader\nowner:    %s\nobserver: %s", want, got)
			}
			if _, exists := observed.Object(predefinedGrant); !exists {
				t.Fatal("the grant to pg_read_all_data was removed as if it were observer access")
			}
		})
	}

	for _, test := range []struct {
		role    string
		message string
	}{
		{role: "writer", message: "inherits unsafe role pg_write_all_data"},
		{role: "creator", message: "prohibited capabilities: CREATEDB"},
		{role: "administrator", message: "can grant membership in pg_read_all_data"},
		{role: "impersonator", message: "inherits unsafe role " + name("person")},
		{role: "nested", message: "inherits unsafe role " + name("nested_grants")},
	} {
		t.Run(test.role+" is refused", func(t *testing.T) {
			snapshot, projection, finding, err := InspectObserverCatalog(ctx, urlFor(test.role), nil, nil, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != nil || finding == nil || finding.Code != "observer_role_elevated" || !strings.Contains(finding.Message, test.message) {
				t.Fatalf("finding = %#v, projection = %#v", finding, projection)
			}
			if len(finding.NextActions) != 2 ||
				!strings.Contains(finding.NextActions[0].SQL, "IN ROLE pg_read_all_data;") ||
				!strings.Contains(finding.NextActions[0].SQL, "GRANT CONNECT ON DATABASE "+pgx.Identifier{database.Name}.Sanitize()) ||
				strings.Contains(finding.NextActions[1].SQL, "GRANT SELECT") {
				t.Fatalf("next actions = %#v", finding.NextActions)
			}
		})
	}

	// A valid role with one privilege to write, by any route, is refused. The
	// role with BYPASSRLS is the important case: with a write privilege it
	// could change rows that policies protect.
	adminConfig, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.Database = database.Name
	adminDatabase, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDatabase.Close(context.Background())
	for _, test := range []struct {
		name    string
		role    string
		grant   string
		revoke  string
		message string
	}{
		{
			name: "direct INSERT with BYPASSRLS", role: "bypass",
			grant:   "GRANT INSERT ON private.orders TO " + quoted("bypass"),
			revoke:  "REVOKE INSERT ON private.orders FROM " + quoted("bypass"),
			message: name("bypass") + " can write to relation private.orders",
		},
		{
			name: "column UPDATE", role: "reader",
			grant:   "GRANT UPDATE (nickname) ON app.accounts TO " + quoted("reader"),
			revoke:  "REVOKE UPDATE (nickname) ON app.accounts FROM " + quoted("reader"),
			message: name("reader") + " can write to relation app.accounts",
		},
		{
			name: "sequence USAGE", role: "reader",
			grant:   "GRANT USAGE ON SEQUENCE app.ticket_seq TO " + quoted("reader"),
			revoke:  "REVOKE USAGE ON SEQUENCE app.ticket_seq FROM " + quoted("reader"),
			message: name("reader") + " can change sequence app.ticket_seq",
		},
		{
			name: "DELETE granted to PUBLIC", role: "bare",
			grant:   "GRANT DELETE ON app.accounts TO PUBLIC",
			revoke:  "REVOKE DELETE ON app.accounts FROM PUBLIC",
			message: name("bare") + " can write to relation app.accounts",
		},
		{
			name: "TRUNCATE granted to a predefined role", role: "monitor",
			grant:   "GRANT TRUNCATE ON app.accounts TO pg_read_all_stats",
			revoke:  "REVOKE TRUNCATE ON app.accounts FROM pg_read_all_stats",
			message: "pg_read_all_stats can write to relation app.accounts",
		},
		{
			name: "CREATE on the database", role: "reader",
			grant:   "GRANT CREATE ON DATABASE " + pgx.Identifier{database.Name}.Sanitize() + " TO " + quoted("reader"),
			revoke:  "REVOKE CREATE ON DATABASE " + pgx.Identifier{database.Name}.Sanitize() + " FROM " + quoted("reader"),
			message: name("reader") + " has CREATE on database",
		},
		{
			name: "ownership of an object", role: "bypass",
			grant:   "CREATE SCHEMA owned_by_observer AUTHORIZATION " + quoted("bypass"),
			revoke:  "DROP SCHEMA owned_by_observer",
			message: name("bypass") + " owns schema owned_by_observer",
		},
	} {
		t.Run(test.name+" is refused", func(t *testing.T) {
			if _, err := adminDatabase.Exec(ctx, test.grant); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := adminDatabase.Exec(context.Background(), test.revoke); err != nil {
					t.Fatal(err)
				}
			}()
			snapshot, _, finding, err := InspectObserverCatalog(ctx, urlFor(test.role), nil, nil, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != nil || finding == nil || finding.Code != "observer_access_policy_unsafe" ||
				!strings.HasPrefix(finding.Message, "observer is not read-only: ") || !strings.Contains(finding.Message, test.message) || len(finding.NextActions) != 2 {
				t.Fatalf("finding = %#v", finding)
			}
		})
	}
	// Each grant is gone again: the roles are valid as before.
	if _, _, finding, err := InspectObserverCatalog(ctx, urlFor("bypass"), nil, nil, 10*time.Second); err != nil || finding != nil {
		t.Fatalf("observer after the grants were revoked: %v, %#v", err, finding)
	}

	// A role that holds CREATE on an application schema through a predefined
	// role is not read-only.
	if _, err := owner.Exec(ctx, "GRANT CREATE ON SCHEMA app TO pg_read_all_data"); err != nil {
		t.Fatal(err)
	}
	_, _, finding, err = InspectObserverCatalog(ctx, urlFor("reader"), nil, nil, 10*time.Second)
	if err != nil || finding == nil || finding.Code != "observer_access_policy_unsafe" || !strings.Contains(finding.Message, "pg_read_all_data has CREATE on schema app") {
		t.Fatalf("CREATE through a predefined role: %v, %#v", err, finding)
	}
}

// Row-level security with no policy for a reader hides every row from a
// pg_read_all_data role, with no error. Contract check refuses that reader,
// because a data gate could pass on rows it cannot see. The same role with
// BYPASSRLS sees every row and is ready.
func TestContractCheckNeedsBypassRLSWhenPoliciesHideRowsFromTheReader(t *testing.T) {
	fixture := newProviderFixture(t)
	ctx := context.Background()
	config, err := pgx.ParseConfig(fixture.URL)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, "CREATE SCHEMA app; CREATE TABLE app.orders (id bigint PRIMARY KEY); ALTER TABLE app.orders ENABLE ROW LEVEL SECURITY; INSERT INTO app.orders VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	const password = "observer-rls-test-password"
	reader, bypass := config.User+"_reader", config.User+"_bypass"
	database := pgx.Identifier{config.Database}.Sanitize()
	for role, attribute := range map[string]string{reader: "NOBYPASSRLS", bypass: "BYPASSRLS"} {
		quoted := pgx.Identifier{role}.Sanitize()
		if _, err := fixture.cluster.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN PASSWORD '"+password+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION "+attribute+" IN ROLE pg_read_all_data; GRANT CONNECT ON DATABASE "+database+" TO "+quoted); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			background := context.Background()
			_, _ = fixture.cluster.Exec(background, "REVOKE CONNECT ON DATABASE "+database+" FROM "+quoted)
			_, _ = fixture.cluster.Exec(background, "DROP ROLE IF EXISTS "+quoted)
		})
	}
	input := fixture.readinessInput()
	run := func(role string) Report {
		t.Helper()
		observer := config.Copy()
		observer.User, observer.Password = role, password
		next := input
		next.DatabaseURL = restrictedScratchURL(observer)
		report, err := Run(ctx, next)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	filtered := run(reader)
	if filtered.Status != "blocked" || len(filtered.Findings) != 1 || filtered.Findings[0].Code != "observer_rls_incomplete" ||
		filtered.Observer.Mode != "predefined_read_role" || filtered.Observer.BypassRLS {
		t.Fatalf("reader that policies filter = %#v", filtered)
	}
	complete := run(bypass)
	if complete.Status != "ready" || len(complete.Findings) != 0 || complete.Observer.Mode != "predefined_read_role" || !complete.Observer.BypassRLS {
		t.Fatalf("reader with BYPASSRLS = %#v", complete)
	}
}
