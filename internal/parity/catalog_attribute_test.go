package parity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

type catalogAttributeLedger struct {
	SchemaVersion   int                            `json:"schema_version"`
	InventoryStatus string                         `json:"inventory_status"`
	Versions        []int                          `json:"postgres_versions"`
	Catalogs        map[string]map[string][]string `json:"catalog_columns"`
	Attributes      []catalogAttribute             `json:"attributes"`
}

type catalogAttribute struct {
	Catalog        string   `json:"catalog"`
	Column         string   `json:"column"`
	Classification string   `json:"classification"`
	Reason         string   `json:"reason"`
	Tests          []string `json:"tests"`
}

func TestPostgresCatalogAttributeLedgerCoversLiveCatalog(t *testing.T) {
	ledger := loadCatalogAttributeLedger(t)
	if ledger.SchemaVersion != 1 || ledger.InventoryStatus != "attribute_columns_classified" || !reflect.DeepEqual(ledger.Versions, []int{15, 16, 17, 18}) {
		t.Fatalf("invalid catalog attribute ledger header: %#v", ledger)
	}
	classifications := map[string]bool{
		"modeled": true, "blocked": true, "derived": true, "environment": true,
		"runtime": true, "secret": true, "not_applicable": true,
	}
	seen := make(map[string]catalogAttribute, len(ledger.Attributes))
	for _, item := range ledger.Attributes {
		key := item.Catalog + "." + item.Column
		if item.Catalog == "" || item.Column == "" || seen[key].Catalog != "" || !classifications[item.Classification] || item.Reason == "" {
			t.Fatalf("invalid catalog attribute entry: %#v", item)
		}
		if (item.Classification == "modeled" || item.Classification == "blocked") && len(item.Tests) == 0 {
			t.Fatalf("%s requires executable evidence", key)
		}
		for _, reference := range item.Tests {
			assertOnwardTestReference(t, reference)
		}
		seen[key] = item
	}
	union := make(map[string]bool)
	for _, version := range ledger.Versions {
		catalogs, exists := ledger.Catalogs[fmt.Sprint(version)]
		if !exists {
			t.Fatalf("PostgreSQL %d lacks catalog-column evidence", version)
		}
		for catalog, columns := range catalogs {
			for _, column := range columns {
				key := catalog + "." + column
				union[key] = true
				if _, exists := seen[key]; !exists {
					t.Fatalf("PostgreSQL %d attribute %s is unclassified", version, key)
				}
			}
		}
	}
	for key := range seen {
		if !union[key] {
			t.Fatalf("classified attribute %s does not exist on a supported major", key)
		}
	}

	url := os.Getenv("ONWARDPG_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ONWARDPG_TEST_DATABASE_URL to validate live PostgreSQL catalog attributes")
	}
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(ctx)
	var version int
	if err := connection.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer / 10000").Scan(&version); err != nil {
		t.Fatal(err)
	}
	actual, err := liveCatalogColumns(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	expected, exists := ledger.Catalogs[fmt.Sprint(version)]
	if !exists {
		t.Fatalf("PostgreSQL %d is outside the attribute ledger", version)
	}
	if !reflect.DeepEqual(actual, expected) {
		actualJSON, _ := json.Marshal(actual)
		expectedJSON, _ := json.Marshal(expected)
		t.Fatalf("PostgreSQL %d catalog attributes differ:\nactual=%s\nexpected=%s", version, actualJSON, expectedJSON)
	}
}

func TestPostgresCatalogAttributeLedgerKeepsKnownBoundariesExplicit(t *testing.T) {
	ledger := loadCatalogAttributeLedger(t)
	entries := make(map[string]catalogAttribute, len(ledger.Attributes))
	for _, item := range ledger.Attributes {
		entries[item.Catalog+"."+item.Column] = item
	}
	for _, key := range []string{
		"pg_class.reltoastrelid", "pg_class.reloptions", "pg_depend.deptype",
		"pg_proc.proargtypes", "pg_proc.prorettype", "pg_range.rngcanonical", "pg_range.rngsubdiff",
	} {
		item := entries[key]
		if item.Classification != "modeled" {
			t.Fatalf("%s classification = %#v, want modeled", key, item)
		}
	}
	for _, key := range []string{"pg_inherits.inhdetachpending"} {
		item := entries[key]
		if item.Classification != "blocked" {
			t.Fatalf("%s classification = %#v, want blocked", key, item)
		}
	}
	for _, key := range []string{"pg_authid.rolpassword", "pg_subscription.subconninfo", "pg_user_mapping.umoptions"} {
		item := entries[key]
		if item.Classification != "secret" {
			t.Fatalf("%s classification = %#v, want secret", key, item)
		}
	}
}

func loadCatalogAttributeLedger(t *testing.T) catalogAttributeLedger {
	t.Helper()
	data, err := os.ReadFile("../../parity/postgres-catalog-attributes.json")
	if err != nil {
		t.Fatal(err)
	}
	var ledger catalogAttributeLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	return ledger
}

func liveCatalogColumns(ctx context.Context, connection *pgx.Conn) (map[string][]string, error) {
	rows, err := connection.Query(ctx, `
SELECT c.relname, a.attname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid
WHERE n.nspname = 'pg_catalog' AND c.relkind = 'r' AND c.relname LIKE 'pg_%'
  AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY c.relname, a.attnum`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]string)
	for rows.Next() {
		var catalog, column string
		if err := rows.Scan(&catalog, &column); err != nil {
			return nil, err
		}
		result[catalog] = append(result[catalog], column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
