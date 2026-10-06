package verify

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/scratchdb"
	"github.com/jokull/onwardpg/internal/source"
)

func TestVerificationRetriesEachUnitAfterTheAdministratorInstallsAnAllowlistedExtension(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	for _, fixture := range []struct {
		name string
		exec func(connection connectionExecutor) error
	}{
		{name: "transactional planner batch", exec: func(c connectionExecutor) error {
			return executeBatchRetrying(ctx, c.conn, protocol.Batch{ID: "b", Phase: "expand", Transactional: true, Statements: []protocol.Statement{
				{SQL: `CREATE SCHEMA IF NOT EXISTS "extensions";`},
				{SQL: `CREATE EXTENSION IF NOT EXISTS "earthdistance" WITH SCHEMA "extensions" CASCADE;`},
				{SQL: `SET search_path = public, extensions;`},
				{SQL: `CREATE TABLE public.place (lat double precision, lon double precision);`},
				{SQL: `CREATE INDEX ON public.place USING gist (extensions.ll_to_earth(lat, lon));`},
			}}, c.database.Retrying)
		}},
		{name: "non-transactional planner batch", exec: func(c connectionExecutor) error {
			return executeBatchRetrying(ctx, c.conn, protocol.Batch{ID: "b", Phase: "expand", Statements: []protocol.Statement{
				{SQL: `CREATE SCHEMA IF NOT EXISTS "extensions";`},
				{SQL: `CREATE EXTENSION IF NOT EXISTS "earthdistance" WITH SCHEMA "extensions" CASCADE;`},
				{SQL: `SET search_path = public, extensions;`},
				{SQL: `CREATE TABLE public.place (lat double precision, lon double precision);`},
				{SQL: `CREATE INDEX ON public.place USING gist (extensions.ll_to_earth(lat, lon));`},
			}}, c.database.Retrying)
		}},
		{name: "edited raw batch", exec: func(c connectionExecutor) error {
			return c.database.Retrying(ctx, func() error {
				return executeRawBatch(ctx, c.conn, bundle.SQLBatch{Transactional: true, SQL: `CREATE SCHEMA IF NOT EXISTS "extensions";
CREATE EXTENSION IF NOT EXISTS "earthdistance" WITH SCHEMA "extensions" CASCADE;
SET search_path = public, extensions;
CREATE TABLE public.place (lat double precision, lon double precision);
CREATE INDEX ON public.place USING gist (extensions.ll_to_earth(lat, lon));`})
			})
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			database, err := scratchdb.Create(ctx, url, "onwardpg_verify_ext", scratchdb.WithAdminExtensions([]scratchdb.AdminExtension{{Name: "earthdistance", Schema: "extensions"}}))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			connection, err := database.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close(ctx)
			if err := fixture.exec(connectionExecutor{conn: connection, database: database}); err != nil {
				if strings.Contains(err.Error(), "permission denied to create extension") && strings.Contains(err.Error(), "is trusted") {
					t.Skip("earthdistance is trusted on this server")
				}
				t.Fatal(err)
			}
			if got := strings.Join(database.InstalledByAdministrator(), ","); got == "" {
				t.Skip("earthdistance is trusted on this server: the restricted role created it itself")
			}
			graph, err := source.LoadDatabaseGraphForComparison(ctx, database.Config, nil)
			if err != nil {
				t.Fatal(err)
			}
			if blockers := graph.Unsupported(); len(blockers) != 0 {
				t.Fatalf("blockers after administrator installation: %v", blockers)
			}
		})
	}
}

func TestVerificationWithoutAllowlistKeepsTheRefusalAndNamesTheSetting(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ONWARDPG_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := scratchdb.Create(ctx, url, "onwardpg_verify_ext")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	connection, err := database.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	err = executeBatchRetrying(ctx, connection, protocol.Batch{ID: "b", Phase: "expand", Transactional: true, Statements: []protocol.Statement{
		{SQL: `CREATE EXTENSION IF NOT EXISTS "earthdistance" CASCADE;`},
	}}, database.Retrying)
	if err == nil {
		t.Skip("earthdistance is trusted on this server")
	}
	if !strings.Contains(err.Error(), "scratch_admin_extensions") {
		t.Fatalf("refusal lacks the hint: %v", err)
	}
}

type connectionExecutor struct {
	conn     *pgx.Conn
	database *scratchdb.Database
}
