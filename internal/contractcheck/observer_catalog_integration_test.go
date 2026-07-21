package contractcheck

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/pgschema"
)

func TestInspectObserverCatalogProjectsOnlyDedicatedAccess(t *testing.T) {
	adminURL := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, adminURL, "onwardpg_observer_catalog")
	if err != nil {
		t.Fatal(err)
	}
	observerRole := database.Role + "_observer"
	applicationRole := database.Role + "_app"
	externalOwnerRole := database.Role + "_external"
	observerPassword := "observer-catalog-test-password"
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{observerRole}.Sanitize()+" LOGIN PASSWORD '"+observerPassword+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS; CREATE ROLE "+pgx.Identifier{applicationRole}.Sanitize()+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS; CREATE ROLE "+pgx.Identifier{externalOwnerRole}.Sanitize()+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"); err != nil {
		_ = admin.Close(ctx)
		_ = database.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = admin.Close(context.Background())
		_ = database.Close()
		cleanup, cleanupErr := pgx.Connect(context.Background(), adminURL)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{observerRole}.Sanitize()+"; DROP ROLE IF EXISTS "+pgx.Identifier{applicationRole}.Sanitize()+"; DROP ROLE IF EXISTS "+pgx.Identifier{externalOwnerRole}.Sanitize())
			_ = cleanup.Close(context.Background())
		}
	}()

	owner, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, "CREATE SCHEMA app; CREATE TABLE app.orders (id bigint PRIMARY KEY, status text NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	ownerURL := restrictedScratchURL(database.Config)
	baseline, ownerProjection, finding, err := InspectObserverCatalog(ctx, ownerURL, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if finding != nil {
		t.Fatalf("database owner finding = %#v", finding)
	}
	if ownerProjection.Mode != "database_owner" || ownerProjection.Role != database.Role || ownerProjection.DatabaseOwner != database.Role {
		t.Fatalf("database owner projection = %#v", ownerProjection)
	}
	ordersID := (pgschema.Table{Schema: "app", Name: "orders"}).ObjectID()
	ordersObject, exists := baseline.Object(ordersID)
	if !exists || ordersObject.(pgschema.Table).Owner != "" {
		t.Fatalf("database-owned table = %#v, exists=%t", ordersObject, exists)
	}

	if _, err := admin.Exec(ctx, "GRANT CONNECT ON DATABASE "+pgx.Identifier{database.Name}.Sanitize()+" TO "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA app TO "+pgx.Identifier{observerRole}.Sanitize()+"; GRANT SELECT ON app.orders TO "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	observerConfig := database.Config.Copy()
	observerConfig.User = observerRole
	observerConfig.Password = observerPassword
	observerURL := restrictedScratchURL(observerConfig)

	observed, projection, finding, err := InspectObserverCatalog(ctx, observerURL, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if finding != nil {
		t.Fatalf("dedicated observer finding = %#v", finding)
	}
	if projection.Mode != "dedicated_read_only" || projection.Role != observerRole || projection.DatabaseOwner != database.Role {
		t.Fatalf("dedicated observer projection = %#v", projection)
	}
	if len(projection.ProjectedAccess) == 0 {
		t.Fatal("dedicated observer access was not reported as projected")
	}
	baselineFingerprint, err := baseline.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	observedFingerprint, err := observed.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if observedFingerprint != baselineFingerprint {
		t.Fatalf("observer overlay changed catalog: owner=%s observer=%s projected=%#v", baselineFingerprint, observedFingerprint, projection.ProjectedAccess)
	}
	ordersObject, exists = observed.Object(ordersID)
	if !exists || ordersObject.(pgschema.Table).Owner != "" {
		t.Fatalf("projected database-owned table = %#v, exists=%t", ordersObject, exists)
	}

	t.Run("genuine application grant is retained", func(t *testing.T) {
		if _, err := owner.Exec(ctx, "GRANT SELECT ON app.orders TO "+pgx.Identifier{applicationRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		snapshot, _, finding, err := InspectObserverCatalog(ctx, observerURL, nil, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if finding != nil {
			t.Fatalf("application grant finding = %#v", finding)
		}
		privilegeID := (pgschema.TablePrivilege{Table: ordersID, Grantee: applicationRole, Privilege: "SELECT"}).ObjectID()
		object, exists := snapshot.Object(privilegeID)
		privilege, ok := object.(pgschema.TablePrivilege)
		if !exists || !ok || privilege.Grantor != "@owner" || privilege.Grantable {
			t.Fatalf("application privilege = %#v, exists=%t", object, exists)
		}
	})

	t.Run("observer grant option is rejected", func(t *testing.T) {
		if _, err := owner.Exec(ctx, "GRANT SELECT ON app.orders TO "+pgx.Identifier{observerRole}.Sanitize()+" WITH GRANT OPTION"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = owner.Exec(context.Background(), "REVOKE GRANT OPTION FOR SELECT ON app.orders FROM "+pgx.Identifier{observerRole}.Sanitize())
		})
		_, _, finding, err := InspectObserverCatalog(ctx, observerURL, nil, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if finding == nil || finding.Code != "observer_access_policy_unsafe" {
			t.Fatalf("grant-option finding = %#v", finding)
		}
		if _, err := owner.Exec(ctx, "REVOKE GRANT OPTION FOR SELECT ON app.orders FROM "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("observer unsafe privilege is rejected", func(t *testing.T) {
		if _, err := owner.Exec(ctx, "GRANT INSERT ON app.orders TO "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = owner.Exec(context.Background(), "REVOKE INSERT ON app.orders FROM "+pgx.Identifier{observerRole}.Sanitize())
		})
		_, _, finding, err := InspectObserverCatalog(ctx, observerURL, nil, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if finding == nil || finding.Code != "observer_access_policy_unsafe" {
			t.Fatalf("unsafe-privilege finding = %#v", finding)
		}
		if _, err := owner.Exec(ctx, "REVOKE INSERT ON app.orders FROM "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("observer revocation is rejected as incomplete", func(t *testing.T) {
		if _, err := owner.Exec(ctx, "REVOKE SELECT ON app.orders FROM "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = owner.Exec(context.Background(), "GRANT SELECT ON app.orders TO "+pgx.Identifier{observerRole}.Sanitize())
		})
		_, _, finding, err := InspectObserverCatalog(ctx, observerURL, nil, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if finding == nil || finding.Code != "observer_access_incomplete" {
			t.Fatalf("revoked-access finding = %#v", finding)
		}
		if _, err := owner.Exec(ctx, "GRANT SELECT ON app.orders TO "+pgx.Identifier{observerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("explicit external owner is retained", func(t *testing.T) {
		if _, err := owner.Exec(ctx, "CREATE TABLE app.externally_owned (id bigint); GRANT USAGE ON SCHEMA app TO "+pgx.Identifier{externalOwnerRole}.Sanitize()); err != nil {
			t.Fatal(err)
		}
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
		if _, err := adminDatabase.Exec(ctx, "ALTER TABLE app.externally_owned OWNER TO "+pgx.Identifier{externalOwnerRole}.Sanitize()+"; SET ROLE "+pgx.Identifier{externalOwnerRole}.Sanitize()+"; GRANT SELECT ON app.externally_owned TO "+pgx.Identifier{observerRole}.Sanitize()+"; RESET ROLE"); err != nil {
			t.Fatal(err)
		}
		snapshot, _, finding, err := InspectObserverCatalog(ctx, observerURL, nil, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if finding != nil {
			t.Fatalf("external owner finding = %#v", finding)
		}
		externalID := (pgschema.Table{Schema: "app", Name: "externally_owned"}).ObjectID()
		object, exists := snapshot.Object(externalID)
		table, ok := object.(pgschema.Table)
		if !exists || !ok || table.Owner != externalOwnerRole {
			t.Fatalf("externally owned table = %#v, exists=%t", object, exists)
		}
	})
}
