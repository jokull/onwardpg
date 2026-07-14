package parity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
)

type catalogMatrix struct {
	SchemaVersion   int                  `json:"schema_version"`
	InventoryStatus string               `json:"inventory_status"`
	Versions        []int                `json:"postgres_versions"`
	Evidence        []string             `json:"evidence"`
	CatalogTables   map[string][]string  `json:"catalog_tables"`
	Entries         []catalogMatrixEntry `json:"entries"`
}

func TestPostgresCatalogFamilyMatrixCoversLiveCatalogTables(t *testing.T) {
	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to validate the live PostgreSQL catalog inventory")
	}
	data, err := os.ReadFile("../../parity/postgres-catalog-families.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix catalogMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer / 10000").Scan(&version); err != nil {
		t.Fatal(err)
	}
	expected, exists := matrix.CatalogTables[fmt.Sprint(version)]
	if !exists {
		t.Fatalf("PostgreSQL %d has no catalog-table inventory", version)
	}
	rows, err := conn.Query(ctx, `
SELECT c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'pg_catalog' AND c.relkind = 'r' AND c.relname LIKE 'pg_%'
ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("PostgreSQL %d catalog tables differ from the checked-in inventory:\nactual=%#v\nexpected=%#v", version, actual, expected)
	}
}

type catalogMatrixEntry struct {
	ID        string   `json:"id"`
	Catalogs  []string `json:"catalogs"`
	State     string   `json:"state"`
	Selectors []string `json:"selectors"`
	Tests     []string `json:"tests"`
}

func TestPostgresCatalogFamilyMatrixKeepsSafetyBlockersVisible(t *testing.T) {
	data, err := os.ReadFile("../../parity/postgres-catalog-families.json")
	if err != nil {
		t.Fatal(err)
	}
	var matrix catalogMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	if matrix.SchemaVersion != 2 || matrix.InventoryStatus != "catalog_tables_classified_attribute_audit_in_progress" || !reflect.DeepEqual(matrix.Versions, []int{15, 16, 17, 18}) {
		t.Fatalf("invalid catalog matrix header: %#v", matrix)
	}
	// The catalog file keeps historical PG14 material as reference data, but the
	// declared versions are the supported policy boundary. Every supported
	// version must therefore have evidence and an inventory; extra historical
	// entries must not silently widen support.
	if len(matrix.Evidence) < len(matrix.Versions) || len(matrix.CatalogTables) < len(matrix.Versions) {
		t.Fatalf("catalog matrix lacks per-version evidence: %#v", matrix)
	}
	required := map[string]bool{
		"ownership": false, "acls_default_privileges": false, "rules": false,
		"text_search": false, "event_triggers": false, "logical_publications": false,
		"extended_statistics": false, "foreign_data": false, "extensions": false,
	}
	seen := make(map[string]bool, len(matrix.Entries))
	classifiedCatalogs := make(map[string]bool)
	for _, entry := range matrix.Entries {
		if entry.ID == "" || seen[entry.ID] || len(entry.Catalogs) == 0 || len(entry.Selectors) == 0 || len(entry.Tests) == 0 {
			t.Fatalf("invalid catalog matrix entry: %#v", entry)
		}
		seen[entry.ID] = true
		if entry.State != "modeled" && entry.State != "blocked" && entry.State != "atomic" && entry.State != "out_of_scope" {
			t.Fatalf("catalog family %s has invalid state %q", entry.ID, entry.State)
		}
		for _, reference := range entry.Tests {
			assertOnwardTestReference(t, reference)
		}
		for _, catalog := range entry.Catalogs {
			classifiedCatalogs[catalog] = true
		}
		if _, ok := required[entry.ID]; ok {
			required[entry.ID] = true
		}
	}
	for id, present := range required {
		if !present {
			t.Fatalf("required catalog safety family %s is absent", id)
		}
	}
	for _, version := range matrix.Versions {
		catalogs := matrix.CatalogTables[fmt.Sprint(version)]
		if len(catalogs) == 0 || !sort.StringsAreSorted(catalogs) {
			t.Fatalf("PostgreSQL %d catalog list is absent or non-deterministic", version)
		}
		for _, catalog := range catalogs {
			if !classifiedCatalogs[catalog] {
				t.Fatalf("PostgreSQL %d catalog %s is unclassified", version, catalog)
			}
		}
	}
}
