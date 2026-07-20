// Command catalogattributes records every pg_catalog table column for the
// supported PostgreSQL majors and assigns its reviewed schema-diff boundary.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type ledger struct {
	SchemaVersion   int                            `json:"schema_version"`
	InventoryStatus string                         `json:"inventory_status"`
	Versions        []int                          `json:"postgres_versions"`
	Catalogs        map[string]map[string][]string `json:"catalog_columns"`
	Attributes      []attribute                    `json:"attributes"`
}

type attribute struct {
	Catalog        string   `json:"catalog"`
	Column         string   `json:"column"`
	Classification string   `json:"classification"`
	Reason         string   `json:"reason"`
	Tests          []string `json:"tests,omitempty"`
}

func main() {
	if len(os.Args) != 7 || os.Args[1] != "-output" {
		fatalf("usage: catalogattributes -output PATH 15=URL 16=URL 17=URL 18=URL")
	}
	result := ledger{
		SchemaVersion: 1, InventoryStatus: "attribute_columns_classified",
		Versions: []int{15, 16, 17, 18}, Catalogs: make(map[string]map[string][]string),
	}
	union := make(map[string]map[string]bool)
	for _, value := range os.Args[3:] {
		versionText, databaseURL, found := strings.Cut(value, "=")
		if !found {
			fatalf("invalid version URL %q", value)
		}
		version, err := strconv.Atoi(versionText)
		if err != nil {
			fatalf("invalid version %q", versionText)
		}
		catalogs, err := inspect(context.Background(), databaseURL, version)
		if err != nil {
			fatalf("inspect PostgreSQL %d: %v", version, err)
		}
		result.Catalogs[versionText] = catalogs
		for catalog, columns := range catalogs {
			if union[catalog] == nil {
				union[catalog] = make(map[string]bool)
			}
			for _, column := range columns {
				union[catalog][column] = true
			}
		}
	}
	var catalogs []string
	for catalog := range union {
		catalogs = append(catalogs, catalog)
	}
	sort.Strings(catalogs)
	for _, catalog := range catalogs {
		var columns []string
		for column := range union[catalog] {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		for _, column := range columns {
			classification := classify(catalog, column)
			item := attribute{Catalog: catalog, Column: column, Classification: classification, Reason: reason(catalog, column, classification)}
			if classification == "modeled" || classification == "blocked" {
				item.Tests = evidence(catalog, classification)
			}
			result.Attributes = append(result.Attributes, item)
		}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("encode ledger: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(os.Args[2], data, 0o644); err != nil {
		fatalf("write ledger: %v", err)
	}
}

func inspect(ctx context.Context, databaseURL string, expected int) (map[string][]string, error) {
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer connection.Close(ctx)
	var version int
	if err := connection.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer / 10000").Scan(&version); err != nil {
		return nil, err
	}
	if version != expected {
		return nil, fmt.Errorf("connected to PostgreSQL %d", version)
	}
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
	return result, rows.Err()
}

func classify(catalog, column string) string {
	if secretAttributes[catalog+"."+column] {
		return "secret"
	}
	if runtimeCatalogs[catalog] || runtimeAttributes[catalog+"."+column] {
		return "runtime"
	}
	if environmentCatalogs[catalog] {
		return "environment"
	}
	if blockedAttributes[catalog+"."+column] || blockedCatalogs[catalog] {
		return "blocked"
	}
	if modeledAttributes[catalog+"."+column] {
		return "modeled"
	}
	return "derived"
}

func reason(catalog, column, classification string) string {
	if value := reviewedReasons[catalog+"."+column]; value != "" {
		return value
	}
	switch classification {
	case "modeled":
		return "retained in a typed object or canonical dependency edge"
	case "blocked":
		return "non-default user state is surfaced by a narrow unsupported-family selector"
	case "runtime":
		return "data, statistics, transaction, or maintenance state outside declarative schema comparison"
	case "environment":
		return "cluster or installation prerequisite outside database-local migration ownership"
	case "secret":
		return "intentionally neither retained nor printed"
	default:
		if value := derivedReasons[catalog+"."+column]; value != "" {
			return value
		}
		panic("derived attribute lacks a reviewed reason: " + catalog + "." + column)
	}
}

var reviewedReasons = map[string]string{
	"pg_attrdef.adbin":           "table-column defaults are typed; ordinary-view defaults fail closed through view_column_default selectors",
	"pg_attrdef.adnum":           "table-column default identity is typed; ordinary-view defaults fail closed through view_column_default selectors",
	"pg_attrdef.adrelid":         "table-column default identity is typed; ordinary-view defaults fail closed through view_column_default selectors",
	"pg_attrdef.oid":             "table-column default identity is projected; ordinary-view defaults fail closed through view_column_default selectors",
	"pg_class.relpersistence":    "typed for admitted tables and sequences; PostgreSQL requires materialized views to be permanent",
	"pg_constraint.coninhcount":  "typed through admitted constraint parent topology; exceptional PostgreSQL 18 NOT NULL inheritance fails closed",
	"pg_constraint.conislocal":   "typed through admitted constraint parent topology; local-plus-inherited PostgreSQL 18 NOT NULL state fails closed",
	"pg_description.description": "typed for admitted comments; domain-constraint, composite-attribute, and view-column comments fail closed",
}

func evidence(catalog, classification string) []string {
	if classification == "blocked" {
		return []string{"internal/source#TestLoadGraphBlocksPreviouslyBlindCatalogFamilies"}
	}
	if reference := modeledEvidence[catalog]; reference != "" {
		return []string{reference}
	}
	panic("modeled catalog lacks executable evidence: " + catalog)
}

var modeledEvidence = map[string]string{
	"pg_namespace":         "internal/source#TestLoadGraphDDLEquivalentToLiveDatabase",
	"pg_extension":         "internal/graphplan#TestExtensionCreateConvergesOnPostgreSQL",
	"pg_enum":              "internal/graphplan#TestEnumCreateAndDropConvergeOnPostgreSQL",
	"pg_type":              "internal/graphplan#TestDomainLifecycleConvergesOnPostgreSQL",
	"pg_range":             "internal/graphplan#TestRangeTypeLifecycleConvergesOnPostgreSQL",
	"pg_class":             "internal/source#TestLoadGraphPreservesAtlasCoreCatalogSemantics",
	"pg_attribute":         "internal/source#TestLoadGraphPreservesAtlasCoreCatalogSemantics",
	"pg_attrdef":           "internal/source#TestLoadGraphProjectsCatalogDependencies",
	"pg_constraint":        "internal/source#TestLoadGraphForeignKeyIntegration",
	"pg_index":             "internal/source#TestLoadGraphCapturesMaterializedViewIndexes",
	"pg_proc":              "internal/source#TestLoadGraphCapturesRoutineAndTriggerDependencies",
	"pg_trigger":           "internal/source#TestLoadGraphCapturesRoutineAndTriggerDependencies",
	"pg_policy":            "internal/graphplan#TestRowSecurityPoliciesAndTablePrivilegesConvergeOnPostgreSQL",
	"pg_rewrite":           "internal/source#TestLoadGraphCapturesViewsAndTypedDependencies",
	"pg_partitioned_table": "internal/source#TestLoadGraphModelsPartitionedIndexPropagation",
	"pg_inherits":          "internal/source#TestLoadGraphModelsPartitionedConstraintPropagation",
	"pg_sequence":          "internal/graphplan#TestSequenceOwnedByTransitionsConvergeOnPostgreSQL",
	"pg_depend":            "internal/source#TestLoadGraphProjectsCatalogDependencies",
	"pg_description":       "internal/source#TestLoadGraphPreservesAtlasCoreCatalogSemantics",
}

var derivedReasons = map[string]string{
	"pg_class.relallfrozen":     "maintenance visibility statistic covered by relfrozenxid runtime classification",
	"pg_class.relchecks":        "count derived from admitted pg_constraint CHECK rows",
	"pg_class.relisshared":      "false for admitted database-local user relations",
	"pg_class.relnatts":         "count derived from admitted pg_attribute rows",
	"pg_class.reltype":          "row-type identity derived from the admitted relation",
	"pg_extension.extcondition": "extension configuration filter retained atomically by extension name, version, and schema",
	"pg_extension.extconfig":    "extension configuration relations retained atomically by extension name, version, and schema",
	"pg_init_privs.classoid":    "address metadata used only to recognize package-supplied initial ACL state",
	"pg_init_privs.initprivs":   "baseline used to distinguish package-supplied ACLs from user ACL drift",
	"pg_init_privs.objoid":      "address metadata used only to recognize package-supplied initial ACL state",
	"pg_init_privs.objsubid":    "address metadata used only to recognize package-supplied initial ACL state",
	"pg_init_privs.privtype":    "ACL object-kind discriminator used only with the initial-privilege baseline",
	"pg_type.typbyval":          "storage implementation fixed by the admitted PostgreSQL type definition",
	"pg_type.typsubscript":      "subscription handler fixed by admitted built-in, array, domain, composite, enum, and range type kinds",
}

var secretAttributes = set(
	"pg_authid.rolpassword", "pg_subscription.subconninfo", "pg_user_mapping.umoptions",
)

var runtimeCatalogs = set(
	"pg_largeobject", "pg_largeobject_metadata", "pg_replication_origin",
	"pg_statistic", "pg_statistic_ext_data", "pg_subscription_rel",
)

var environmentCatalogs = set(
	"pg_auth_members", "pg_authid", "pg_database", "pg_db_role_setting",
	"pg_shdepend", "pg_shdescription", "pg_shseclabel", "pg_tablespace",
)

var runtimeAttributes = set(
	"pg_class.relpages", "pg_class.reltuples", "pg_class.relallvisible",
	"pg_class.relfrozenxid", "pg_class.relminmxid", "pg_class.relfilenode",
	"pg_class.relrewrite", "pg_index.indcheckxmin",
)

var modeledAttributes = set(
	"pg_namespace.oid", "pg_namespace.nspname", "pg_namespace.nspowner",
	"pg_extension.oid", "pg_extension.extname", "pg_extension.extowner", "pg_extension.extnamespace", "pg_extension.extrelocatable", "pg_extension.extversion",
	"pg_enum.oid", "pg_enum.enumtypid", "pg_enum.enumsortorder", "pg_enum.enumlabel",
	"pg_type.oid", "pg_type.typname", "pg_type.typnamespace", "pg_type.typowner", "pg_type.typlen", "pg_type.typtype", "pg_type.typcategory", "pg_type.typispreferred", "pg_type.typisdefined", "pg_type.typdelim", "pg_type.typrelid", "pg_type.typelem", "pg_type.typarray", "pg_type.typinput", "pg_type.typoutput", "pg_type.typreceive", "pg_type.typsend", "pg_type.typmodin", "pg_type.typmodout", "pg_type.typanalyze", "pg_type.typalign", "pg_type.typstorage", "pg_type.typnotnull", "pg_type.typbasetype", "pg_type.typtypmod", "pg_type.typndims", "pg_type.typcollation", "pg_type.typdefaultbin", "pg_type.typdefault",
	"pg_range.rngtypid", "pg_range.rngsubtype", "pg_range.rngmultitypid", "pg_range.rngcollation", "pg_range.rngsubopc", "pg_range.rngcanonical", "pg_range.rngsubdiff",
	"pg_class.oid", "pg_class.relname", "pg_class.relnamespace", "pg_class.relowner", "pg_class.relam", "pg_class.reltablespace", "pg_class.reltoastrelid", "pg_class.relpersistence", "pg_class.relkind", "pg_class.relrowsecurity", "pg_class.relforcerowsecurity", "pg_class.relispopulated", "pg_class.relreplident", "pg_class.relispartition", "pg_class.relacl", "pg_class.reloptions", "pg_class.relpartbound",
	"pg_attribute.attrelid", "pg_attribute.attname", "pg_attribute.atttypid", "pg_attribute.attstattarget", "pg_attribute.attlen", "pg_attribute.attnum", "pg_attribute.attndims", "pg_attribute.attcacheoff", "pg_attribute.atttypmod", "pg_attribute.attbyval", "pg_attribute.attalign", "pg_attribute.attstorage", "pg_attribute.attcompression", "pg_attribute.attnotnull", "pg_attribute.atthasdef", "pg_attribute.atthasmissing", "pg_attribute.attidentity", "pg_attribute.attgenerated", "pg_attribute.attisdropped", "pg_attribute.attislocal", "pg_attribute.attinhcount", "pg_attribute.attcollation", "pg_attribute.attacl", "pg_attribute.attoptions", "pg_attribute.attfdwoptions", "pg_attribute.attmissingval",
	"pg_attrdef.oid", "pg_attrdef.adrelid", "pg_attrdef.adnum", "pg_attrdef.adbin",
	"pg_constraint.oid", "pg_constraint.conname", "pg_constraint.connamespace", "pg_constraint.contype", "pg_constraint.condeferrable", "pg_constraint.condeferred", "pg_constraint.convalidated", "pg_constraint.conenforced", "pg_constraint.conperiod", "pg_constraint.conrelid", "pg_constraint.contypid", "pg_constraint.conindid", "pg_constraint.conparentid", "pg_constraint.confrelid", "pg_constraint.confupdtype", "pg_constraint.confdeltype", "pg_constraint.confmatchtype", "pg_constraint.conislocal", "pg_constraint.coninhcount", "pg_constraint.connoinherit", "pg_constraint.conkey", "pg_constraint.confkey", "pg_constraint.conpfeqop", "pg_constraint.conppeqop", "pg_constraint.conffeqop", "pg_constraint.confdelsetcols", "pg_constraint.conexclop", "pg_constraint.conbin",
	"pg_index.indexrelid", "pg_index.indrelid", "pg_index.indnatts", "pg_index.indnkeyatts", "pg_index.indisunique", "pg_index.indnullsnotdistinct", "pg_index.indisprimary", "pg_index.indisexclusion", "pg_index.indimmediate", "pg_index.indisclustered", "pg_index.indisvalid", "pg_index.indisready", "pg_index.indislive", "pg_index.indisreplident", "pg_index.indkey", "pg_index.indcollation", "pg_index.indclass", "pg_index.indoption", "pg_index.indexprs", "pg_index.indpred",
	"pg_proc.oid", "pg_proc.proname", "pg_proc.pronamespace", "pg_proc.proowner", "pg_proc.prolang", "pg_proc.procost", "pg_proc.prorows", "pg_proc.provariadic", "pg_proc.prosupport", "pg_proc.prokind", "pg_proc.prosecdef", "pg_proc.proleakproof", "pg_proc.proisstrict", "pg_proc.proretset", "pg_proc.provolatile", "pg_proc.proparallel", "pg_proc.pronargs", "pg_proc.pronargdefaults", "pg_proc.prorettype", "pg_proc.proargtypes", "pg_proc.proallargtypes", "pg_proc.proargmodes", "pg_proc.proargnames", "pg_proc.proargdefaults", "pg_proc.protrftypes", "pg_proc.prosrc", "pg_proc.probin", "pg_proc.prosqlbody", "pg_proc.proconfig", "pg_proc.proacl",
	"pg_trigger.oid", "pg_trigger.tgrelid", "pg_trigger.tgparentid", "pg_trigger.tgname", "pg_trigger.tgfoid", "pg_trigger.tgtype", "pg_trigger.tgenabled", "pg_trigger.tgisinternal", "pg_trigger.tgconstrrelid", "pg_trigger.tgconstrindid", "pg_trigger.tgconstraint", "pg_trigger.tgdeferrable", "pg_trigger.tginitdeferred", "pg_trigger.tgnargs", "pg_trigger.tgattr", "pg_trigger.tgargs", "pg_trigger.tgqual", "pg_trigger.tgoldtable", "pg_trigger.tgnewtable",
	"pg_policy.oid", "pg_policy.polname", "pg_policy.polrelid", "pg_policy.polcmd", "pg_policy.polpermissive", "pg_policy.polroles", "pg_policy.polqual", "pg_policy.polwithcheck",
	"pg_rewrite.oid", "pg_rewrite.rulename", "pg_rewrite.ev_class", "pg_rewrite.ev_type", "pg_rewrite.ev_enabled", "pg_rewrite.is_instead", "pg_rewrite.ev_qual", "pg_rewrite.ev_action",
	"pg_partitioned_table.partrelid", "pg_partitioned_table.partstrat", "pg_partitioned_table.partnatts", "pg_partitioned_table.partdefid", "pg_partitioned_table.partattrs", "pg_partitioned_table.partclass", "pg_partitioned_table.partcollation", "pg_partitioned_table.partexprs",
	"pg_inherits.inhrelid", "pg_inherits.inhparent", "pg_inherits.inhseqno", "pg_inherits.inhdetachpending",
	"pg_sequence.seqrelid", "pg_sequence.seqtypid", "pg_sequence.seqstart", "pg_sequence.seqincrement", "pg_sequence.seqmax", "pg_sequence.seqmin", "pg_sequence.seqcache", "pg_sequence.seqcycle",
	"pg_depend.classid", "pg_depend.objid", "pg_depend.objsubid", "pg_depend.refclassid", "pg_depend.refobjid", "pg_depend.refobjsubid", "pg_depend.deptype",
	"pg_description.objoid", "pg_description.classoid", "pg_description.objsubid", "pg_description.description",
)

var blockedAttributes = set(
	"pg_class.reloftype", "pg_class.relhasrules", "pg_class.relhastriggers", "pg_class.relhasindex", "pg_class.relhassubclass",
	"pg_attribute.attstattarget", "pg_attribute.attstorage", "pg_attribute.attcompression", "pg_attribute.attacl", "pg_attribute.attoptions", "pg_attribute.attfdwoptions",
	"pg_namespace.nspacl", "pg_type.typacl",
	"pg_inherits.inhdetachpending",
)

var blockedCatalogs = set(
	"pg_aggregate", "pg_am", "pg_amop", "pg_amproc", "pg_cast", "pg_collation",
	"pg_conversion", "pg_default_acl", "pg_event_trigger", "pg_foreign_data_wrapper",
	"pg_foreign_server", "pg_foreign_table", "pg_language", "pg_opclass",
	"pg_operator", "pg_opfamily", "pg_parameter_acl", "pg_publication",
	"pg_publication_namespace", "pg_publication_rel", "pg_seclabel",
	"pg_statistic_ext", "pg_subscription", "pg_transform", "pg_ts_config",
	"pg_ts_config_map", "pg_ts_dict", "pg_ts_parser", "pg_ts_template",
	"pg_user_mapping",
)

func set(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func fatalf(format string, arguments ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
