package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

// PostgresMajor discovers the actual server major used for disposable
// materialization. Callers record this evidence instead of asking developers
// to duplicate an inferable value in configuration.
func PostgresMajor(ctx context.Context, databaseURL string) (int, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return 0, fmt.Errorf("connect PostgreSQL version probe: %w", err)
	}
	defer conn.Close(context.Background())
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		return 0, fmt.Errorf("read PostgreSQL version: %w", err)
	}
	if err := validatePostgresVersion(version); err != nil {
		return 0, err
	}
	return version / 10000, nil
}

func materializeDDLGraph(ctx context.Context, path, devURL string, ignores []string, validateIgnores bool) (*pgschema.Snapshot, error) {
	ddl, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return materializeDDLBytesGraph(ctx, ddl, filepath.Base(path), devURL, ignores, validateIgnores)
}

func materializeDDLBytesGraph(ctx context.Context, ddl []byte, provenance, devURL string, ignores []string, validateIgnores bool) (*pgschema.Snapshot, error) {
	admin, err := pgx.Connect(ctx, devURL)
	if err != nil {
		return nil, fmt.Errorf("connect dev database: %w", err)
	}
	defer admin.Close(ctx)
	name, err := temporaryName()
	if err != nil {
		return nil, err
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
		return nil, fmt.Errorf("create temp database: %w", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
	}()
	config, err := pgx.ParseConfig(devURL)
	if err != nil {
		return nil, fmt.Errorf("parse dev URL: %w", err)
	}
	config.Database = name
	target, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect temp database: %w", err)
	}
	if _, err = target.Exec(ctx, string(ddl)); err != nil {
		target.Close(ctx)
		return nil, fmt.Errorf("execute declarative DDL from %s: %w", provenance, err)
	}
	if err := target.Close(ctx); err != nil {
		return nil, fmt.Errorf("close temp database connection: %w", err)
	}
	return inspectGraphConfig(ctx, config, ignores, validateIgnores)
}

func inspectGraphConfig(ctx context.Context, config *pgx.ConnConfig, ignores []string, validateIgnores bool) (*pgschema.Snapshot, error) {
	tracker, err := newIgnoreTracker(ignores)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin graph catalog snapshot: %w", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, fmt.Errorf("set graph catalog snapshot search_path: %w", err)
	}
	var version int
	if err := tx.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		return nil, fmt.Errorf("read PostgreSQL version: %w", err)
	}
	if err := validatePostgresVersion(version); err != nil {
		return nil, err
	}

	snapshot := pgschema.New()
	for _, inspect := range []func(context.Context, pgx.Tx, *pgschema.Snapshot, *ignoreTracker) error{
		inspectGraphSchemas,
		inspectGraphExtensions,
		inspectGraphEnums,
		inspectGraphDomains,
		inspectGraphComposites,
		inspectGraphRanges,
		inspectGraphTables,
		inspectGraphConstraints,
		inspectGraphSequences,
	} {
		if err := inspect(ctx, tx, snapshot, tracker); err != nil {
			return nil, err
		}
	}
	if err := inspectGraphColumns(ctx, tx, snapshot, tracker, version); err != nil {
		return nil, err
	}
	if err := addSequenceOwnershipDependencies(snapshot); err != nil {
		return nil, err
	}
	if err := addConstraintColumnDependencies(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	if err := inspectGraphViews(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := inspectGraphIndexes(ctx, tx, snapshot, tracker, version); err != nil {
		return nil, err
	}
	if err := inspectGraphReplicaIdentities(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := inspectGraphRoutines(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := addRoutineDependencies(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	if err := addViewRoutineDependencies(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	if err := inspectGraphTriggers(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := inspectGraphPolicies(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := inspectGraphTablePrivileges(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := inspectGraphBlockers(ctx, tx, snapshot, tracker, version); err != nil {
		return nil, err
	}
	if validateIgnores {
		if err := tracker.Validate(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit graph catalog snapshot: %w", err)
	}
	return snapshot, nil
}

func validatePostgresVersion(version int) error {
	if version < 150000 {
		return fmt.Errorf("PostgreSQL %d is unsupported; onwardpg supports PostgreSQL 15 through 18", version)
	}
	if version >= 190000 {
		return fmt.Errorf("PostgreSQL %d is unsupported; onwardpg supports PostgreSQL 15 through 18", version)
	}
	return nil
}

func inspectGraphSchemas(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, obj_description(n.oid, 'pg_namespace')
FROM pg_namespace n
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
ORDER BY n.nspname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var object pgschema.Schema
		if err := rows.Scan(&object.Name, &object.Comment); err != nil {
			return err
		}
		object.Comment = normalizeSchemaComment(object.Name, object.Comment)
		skip, err := tracker.Skip("schema:"+object.Name, snapshot)
		if err != nil {
			return err
		}
		if !skip {
			if err := snapshot.Add(object); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func normalizeSchemaComment(name string, comment *string) *string {
	if name == "public" && comment != nil && *comment == "standard public schema" {
		return nil
	}
	return comment
}

func inspectGraphExtensions(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT e.extname, e.extversion, n.nspname,
       CASE
         WHEN obj_description(e.oid, 'pg_extension') IS DISTINCT FROM available.comment
           THEN obj_description(e.oid, 'pg_extension')
       END
FROM pg_extension e
JOIN pg_namespace n ON n.oid = e.extnamespace
LEFT JOIN pg_available_extensions available ON available.name = e.extname
ORDER BY e.extname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var object pgschema.Extension
		if err := rows.Scan(&object.Name, &object.Version, &object.Schema, &object.Comment); err != nil {
			return err
		}
		skip, err := tracker.Skip("extension:"+object.Name, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
	}
	return rows.Err()
}

func inspectGraphEnums(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, t.typname, e.enumlabel, obj_description(t.oid, 'pg_type')
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
LEFT JOIN pg_enum e ON e.enumtypid = t.oid
WHERE t.typtype = 'e'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, t.typname, e.enumsortorder`)
	if err != nil {
		return err
	}
	defer rows.Close()
	enums := make(map[string]pgschema.Enum)
	for rows.Next() {
		var namespace, name string
		var label, comment *string
		if err := rows.Scan(&namespace, &name, &label, &comment); err != nil {
			return err
		}
		key := namespace + "." + name
		object := enums[key]
		object.Schema, object.Name, object.Comment = namespace, name, comment
		if label != nil {
			object.Labels = append(object.Labels, *label)
		}
		enums[key] = object
	}
	if err := rows.Err(); err != nil {
		return err
	}
	keys := make([]string, 0, len(enums))
	for key := range enums {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object := enums[key]
		skip, err := tracker.Skip("enum:"+key, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
	}
	return nil
}

func inspectGraphDomains(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT t.oid, n.nspname, t.typname, format_type(t.typbasetype, t.typtypmod),
       CASE WHEN t.typdefaultbin IS NOT NULL THEN pg_get_expr(t.typdefaultbin, 0, true) END,
       t.typnotnull,
       CASE WHEN t.typcollation <> 0 AND t.typcollation IS DISTINCT FROM base.typcollation
            THEN quote_ident(collation_ns.nspname) || '.' || quote_ident(coll.collname) END,
       obj_description(t.oid, 'pg_type'), dependency_ns.nspname, dependency_type.typname,
       dependency_type.typtype::text
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_type base ON base.oid = t.typbasetype
LEFT JOIN pg_collation coll ON coll.oid = t.typcollation
LEFT JOIN pg_namespace collation_ns ON collation_ns.oid = coll.collnamespace
LEFT JOIN pg_type dependency_type ON dependency_type.oid = CASE WHEN base.typelem <> 0 THEN base.typelem ELSE base.oid END
LEFT JOIN pg_namespace dependency_ns ON dependency_ns.oid = dependency_type.typnamespace
WHERE t.typtype = 'd'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, t.typname`)
	if err != nil {
		return err
	}
	type dependency struct {
		schema string
		name   string
		kind   string
	}
	domains := make(map[uint32]pgschema.Domain)
	dependencies := make(map[uint32]dependency)
	for rows.Next() {
		var oid uint32
		var object pgschema.Domain
		var collation *string
		var dependencySchema, dependencyName, dependencyKind string
		if err := rows.Scan(&oid, &object.Schema, &object.Name, &object.BaseType, &object.Default, &object.NotNull, &collation, &object.Comment, &dependencySchema, &dependencyName, &dependencyKind); err != nil {
			rows.Close()
			return err
		}
		if collation != nil {
			object.Collation = *collation
		}
		domains[oid] = object
		dependencies[oid] = dependency{schema: dependencySchema, name: dependencyName, kind: dependencyKind}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = tx.Query(ctx, `
SELECT con.contypid, con.conname, pg_get_constraintdef(con.oid, true), con.convalidated
FROM pg_constraint con
JOIN pg_type domain_type ON domain_type.oid = con.contypid
JOIN pg_namespace n ON n.oid = domain_type.typnamespace
WHERE con.contype = 'c' AND domain_type.typtype = 'd'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY con.contypid, con.conname`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var oid uint32
		var constraint pgschema.DomainConstraint
		if err := rows.Scan(&oid, &constraint.Name, &constraint.Definition, &constraint.Validated); err != nil {
			rows.Close()
			return err
		}
		object, exists := domains[oid]
		if !exists {
			continue
		}
		object.Constraints = append(object.Constraints, constraint)
		domains[oid] = object
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	oids := make([]uint32, 0, len(domains))
	for oid := range domains {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool {
		return domains[oids[i]].ObjectID().String() < domains[oids[j]].ObjectID().String()
	})
	added := make(map[uint32]bool, len(oids))
	for _, oid := range oids {
		object := domains[oid]
		skip, err := tracker.Skip("domain:"+object.Schema+"."+object.Name, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
		added[oid] = true
	}
	for _, oid := range oids {
		if !added[oid] {
			continue
		}
		dependency := dependencies[oid]
		var kind pgschema.Kind
		switch dependency.kind {
		case "d":
			kind = pgschema.KindDomain
		case "e":
			kind = pgschema.KindEnum
		case "c":
			kind = pgschema.KindComposite
		default:
			continue
		}
		dependencyID := pgschema.ID{Kind: kind, Schema: dependency.schema, Name: dependency.name}
		if _, exists := snapshot.Object(dependencyID); !exists {
			continue
		}
		if err := snapshot.AddDependency(domains[oid].ObjectID(), dependencyID); err != nil {
			return err
		}
	}
	return nil
}

func inspectGraphComposites(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT composite_type.oid, n.nspname, composite_type.typname, obj_description(composite_type.oid, 'pg_type'),
       attribute.attname, format_type(attribute.atttypid, attribute.atttypmod),
       CASE WHEN attribute.attcollation <> 0 AND attribute.attcollation IS DISTINCT FROM attribute_type.typcollation
            THEN quote_ident(collation_ns.nspname) || '.' || quote_ident(coll.collname) END,
       dependency_ns.nspname, dependency_type.typname, dependency_type.typtype::text
FROM pg_class relation
JOIN pg_namespace n ON n.oid = relation.relnamespace
JOIN pg_type composite_type ON composite_type.typrelid = relation.oid AND composite_type.typtype = 'c'
LEFT JOIN pg_attribute attribute ON attribute.attrelid = relation.oid
  AND attribute.attnum > 0 AND NOT attribute.attisdropped
LEFT JOIN pg_type attribute_type ON attribute_type.oid = attribute.atttypid
LEFT JOIN pg_collation coll ON coll.oid = attribute.attcollation
LEFT JOIN pg_namespace collation_ns ON collation_ns.oid = coll.collnamespace
LEFT JOIN pg_type dependency_type ON dependency_type.oid = CASE WHEN attribute_type.typelem <> 0 THEN attribute_type.typelem ELSE attribute_type.oid END
LEFT JOIN pg_namespace dependency_ns ON dependency_ns.oid = dependency_type.typnamespace
WHERE relation.relkind = 'c'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, composite_type.typname, attribute.attnum`)
	if err != nil {
		return err
	}
	defer rows.Close()
	composites := make(map[uint32]pgschema.Composite)
	dependencies := make(map[uint32][]pgschema.ID)
	for rows.Next() {
		var oid uint32
		var schema, name string
		var comment, attributeName, attributeType, collation, dependencySchema, dependencyName, dependencyKind *string
		if err := rows.Scan(&oid, &schema, &name, &comment, &attributeName, &attributeType, &collation, &dependencySchema, &dependencyName, &dependencyKind); err != nil {
			return err
		}
		object := composites[oid]
		object.Schema, object.Name, object.Comment = schema, name, comment
		if attributeName != nil && attributeType != nil {
			attribute := pgschema.CompositeAttribute{Name: *attributeName, Position: len(object.Attributes) + 1, Type: *attributeType}
			if collation != nil {
				attribute.Collation = *collation
			}
			object.Attributes = append(object.Attributes, attribute)
		}
		composites[oid] = object
		if dependencySchema != nil && dependencyName != nil && dependencyKind != nil {
			var kind pgschema.Kind
			switch *dependencyKind {
			case "c":
				kind = pgschema.KindComposite
			case "d":
				kind = pgschema.KindDomain
			case "e":
				kind = pgschema.KindEnum
			}
			if kind != "" {
				dependencies[oid] = append(dependencies[oid], pgschema.ID{Kind: kind, Schema: *dependencySchema, Name: *dependencyName})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	oids := make([]uint32, 0, len(composites))
	for oid := range composites {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool {
		return composites[oids[i]].ObjectID().String() < composites[oids[j]].ObjectID().String()
	})
	added := make(map[uint32]bool, len(oids))
	for _, oid := range oids {
		object := composites[oid]
		skip, err := tracker.Skip("composite:"+object.Schema+"."+object.Name, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
		added[oid] = true
	}
	for _, oid := range oids {
		if !added[oid] {
			continue
		}
		seen := make(map[pgschema.ID]bool)
		for _, dependencyID := range dependencies[oid] {
			if seen[dependencyID] {
				continue
			}
			seen[dependencyID] = true
			if _, exists := snapshot.Object(dependencyID); !exists {
				continue
			}
			if err := snapshot.AddDependency(composites[oid].ObjectID(), dependencyID); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectGraphRanges(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT range_type.oid, n.nspname, range_type.typname, format_type(range_catalog.rngsubtype, NULL),
       CASE WHEN range_catalog.rngcollation <> 0 AND range_catalog.rngcollation IS DISTINCT FROM subtype.typcollation
            THEN quote_ident(collation_ns.nspname) || '.' || quote_ident(coll.collname) END,
       CASE WHEN NOT opclass.opcdefault THEN quote_ident(opclass_ns.nspname) || '.' || quote_ident(opclass.opcname) END,
       CASE WHEN range_catalog.rngcanonical <> 0 THEN range_catalog.rngcanonical::regproc::text END,
       CASE WHEN range_catalog.rngsubdiff <> 0 THEN range_catalog.rngsubdiff::regproc::text END,
       multirange_type.typname, obj_description(range_type.oid, 'pg_type'),
       dependency_ns.nspname, dependency_type.typname, dependency_type.typtype::text
FROM pg_range range_catalog
JOIN pg_type range_type ON range_type.oid = range_catalog.rngtypid
JOIN pg_namespace n ON n.oid = range_type.typnamespace
JOIN pg_type subtype ON subtype.oid = range_catalog.rngsubtype
JOIN pg_opclass opclass ON opclass.oid = range_catalog.rngsubopc
JOIN pg_namespace opclass_ns ON opclass_ns.oid = opclass.opcnamespace
LEFT JOIN pg_collation coll ON coll.oid = range_catalog.rngcollation
LEFT JOIN pg_namespace collation_ns ON collation_ns.oid = coll.collnamespace
LEFT JOIN pg_type multirange_type ON multirange_type.oid = range_catalog.rngmultitypid
LEFT JOIN pg_type dependency_type ON dependency_type.oid = CASE WHEN subtype.typelem <> 0 THEN subtype.typelem ELSE subtype.oid END
LEFT JOIN pg_namespace dependency_ns ON dependency_ns.oid = dependency_type.typnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, range_type.typname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type entry struct {
		object     pgschema.Range
		dependency *pgschema.ID
	}
	var entries []entry
	for rows.Next() {
		var oid uint32
		var object pgschema.Range
		var collation, opclass, canonical, subtypeDiff, multirangeName, dependencySchema, dependencyName, dependencyKind *string
		if err := rows.Scan(&oid, &object.Schema, &object.Name, &object.Subtype, &collation, &opclass, &canonical, &subtypeDiff, &multirangeName, &object.Comment, &dependencySchema, &dependencyName, &dependencyKind); err != nil {
			return err
		}
		if collation != nil {
			object.Collation = *collation
		}
		if opclass != nil {
			object.SubtypeOpClass = *opclass
		}
		if canonical != nil {
			object.Canonical = *canonical
		}
		if subtypeDiff != nil {
			object.SubtypeDiff = *subtypeDiff
		}
		if multirangeName != nil {
			object.MultirangeName = *multirangeName
		}
		item := entry{object: object}
		if dependencySchema != nil && dependencyName != nil && dependencyKind != nil {
			var kind pgschema.Kind
			switch *dependencyKind {
			case "c":
				kind = pgschema.KindComposite
			case "d":
				kind = pgschema.KindDomain
			case "e":
				kind = pgschema.KindEnum
			case "r":
				kind = pgschema.KindRange
			}
			if kind != "" {
				dependencyID := pgschema.ID{Kind: kind, Schema: *dependencySchema, Name: *dependencyName}
				item.dependency = &dependencyID
			}
		}
		entries = append(entries, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range entries {
		object := entries[index].object
		skip, err := tracker.Skip("range_type:"+object.Schema+"."+object.Name, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
	}
	for _, item := range entries {
		if item.dependency == nil {
			continue
		}
		if _, exists := snapshot.Object(item.object.ObjectID()); !exists {
			continue
		}
		if _, exists := snapshot.Object(*item.dependency); !exists {
			continue
		}
		if err := snapshot.AddDependency(item.object.ObjectID(), *item.dependency); err != nil {
			return err
		}
	}
	return nil
}

func inspectGraphTables(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, CASE WHEN r.rolname <> current_user THEN r.rolname END,
       c.relpersistence = 'u', obj_description(c.oid, 'pg_class'), c.relkind::text, c.relispartition,
       CASE pt.partstrat WHEN 'r' THEN 'RANGE' WHEN 'l' THEN 'LIST' WHEN 'h' THEN 'HASH' END,
       pg_get_partkeydef(c.oid), pn.nspname, pc.relname, pg_get_expr(c.relpartbound, c.oid, true)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_roles r ON r.oid = c.relowner
LEFT JOIN pg_partitioned_table pt ON pt.partrelid = c.oid
LEFT JOIN pg_inherits i ON i.inhrelid = c.oid AND c.relispartition
LEFT JOIN pg_class pc ON pc.oid = i.inhparent
LEFT JOIN pg_namespace pn ON pn.oid = pc.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var partitionDependencies [][2]pgschema.ID
	for rows.Next() {
		var object pgschema.Table
		var relkind string
		var isPartition bool
		var owner, strategy, raw, parentSchema, parentName, bound *string
		if err := rows.Scan(&object.Schema, &object.Name, &owner, &object.Unlogged, &object.Comment, &relkind, &isPartition, &strategy, &raw, &parentSchema, &parentName, &bound); err != nil {
			return err
		}
		if owner != nil {
			object.Owner = *owner
			ignored, err := tracker.Skip("table_owner:"+object.Schema+"."+object.Name, snapshot)
			if err != nil {
				return err
			}
			if ignored {
				object.Owner = ""
			}
		}
		if isPartition {
			if parentSchema == nil || parentName == nil || bound == nil {
				return fmt.Errorf("partition child %s.%s has incomplete catalog parent/bound", object.Schema, object.Name)
			}
			parent := (pgschema.Table{Schema: *parentSchema, Name: *parentName}).ObjectID()
			object.PartitionOf = &pgschema.PartitionOf{Parent: parent, Bound: *bound}
		}
		if strategy != nil {
			object.Partition = &pgschema.Partition{Strategy: *strategy}
			if raw != nil {
				object.Partition.Raw = *raw
			}
		}
		selector := "table:" + object.Schema + "." + object.Name
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
		if object.PartitionOf != nil {
			partitionDependencies = append(partitionDependencies, [2]pgschema.ID{object.ObjectID(), object.PartitionOf.Parent})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, dependency := range partitionDependencies {
		if _, exists := snapshot.Object(dependency[1]); !exists {
			return fmt.Errorf("partition parent %s missing from snapshot", dependency[1])
		}
		if err := snapshot.AddDependency(dependency[0], dependency[1]); err != nil {
			return err
		}
	}
	return nil
}

func inspectGraphColumns(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker, version int) error {
	generated := generatedColumnSelector(version)
	notNullConstraintName := notNullConstraintNameSelector(version)
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname, a.attnum, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       pg_get_expr(ad.adbin, ad.adrelid), a.attidentity::text, %s,
       CASE WHEN a.attcollation <> 0 AND a.attcollation <> typ.typcollation
            THEN quote_ident(cn.nspname) || '.' || quote_ident(coll.collname) END,
       col_description(a.attrelid, a.attnum),
       seq.seqstart, seq.seqincrement, seq.seqmin, seq.seqmax, seq.seqcache, seq.seqcycle,
       dtn.nspname, dt.typname, extn.nspname, ext.extname,
       defaultseq.schema_name, defaultseq.sequence_name, serialseq.relname,
       %s
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type typ ON typ.oid = a.atttypid
LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
LEFT JOIN pg_collation coll ON coll.oid = a.attcollation
LEFT JOIN pg_namespace cn ON cn.oid = coll.collnamespace
LEFT JOIN pg_depend dep ON dep.classid = 'pg_class'::regclass AND dep.refclassid = 'pg_class'::regclass
  AND dep.refobjid = c.oid AND dep.refobjsubid = a.attnum AND dep.deptype = 'i'
LEFT JOIN pg_class seqc ON seqc.oid = dep.objid AND seqc.relkind = 'S'
LEFT JOIN pg_sequence seq ON seq.seqrelid = seqc.oid
LEFT JOIN pg_type dt ON dt.oid = CASE WHEN typ.typelem <> 0 THEN typ.typelem ELSE typ.oid END
LEFT JOIN pg_namespace dtn ON dtn.oid = dt.typnamespace
LEFT JOIN pg_depend typedep ON typedep.classid = 'pg_type'::regclass AND typedep.objid = dt.oid
  AND typedep.refclassid = 'pg_extension'::regclass AND typedep.deptype = 'e'
LEFT JOIN pg_extension ext ON ext.oid = typedep.refobjid
LEFT JOIN pg_namespace extn ON extn.oid = ext.extnamespace
LEFT JOIN LATERAL (
  SELECT defaultn.nspname AS schema_name, defaultclass.relname AS sequence_name
  FROM pg_depend defaultdep
  JOIN pg_class defaultclass ON defaultclass.oid = defaultdep.refobjid AND defaultclass.relkind = 'S'
  JOIN pg_namespace defaultn ON defaultn.oid = defaultclass.relnamespace
  WHERE defaultdep.classid = 'pg_attrdef'::regclass AND defaultdep.objid = ad.oid
    AND defaultdep.refclassid = 'pg_class'::regclass
  LIMIT 1
) defaultseq ON true
LEFT JOIN LATERAL (
  SELECT serialclass.relname
  FROM pg_depend serialdep
  JOIN pg_class serialclass ON serialclass.oid = serialdep.objid AND serialclass.relkind = 'S'
  WHERE serialdep.classid = 'pg_class'::regclass AND serialdep.refclassid = 'pg_class'::regclass
    AND serialdep.refobjid = c.oid AND serialdep.refobjsubid = a.attnum AND serialdep.deptype = 'a'
  LIMIT 1
) serialseq ON true
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg_%%' AND n.nspname <> 'information_schema'
  AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY n.nspname, c.relname, a.attnum`, generated, notNullConstraintName)
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	positions := make(map[pgschema.ID]int)
	for rows.Next() {
		var namespace, tableName, identity, generated string
		var defaultOrGenerated, collation, typeSchema, typeName, extensionSchema, extensionName, defaultSequenceSchema, defaultSequenceName, serialSequenceName, notNullName *string
		var seqStart, seqIncrement, seqMin, seqMax, seqCache *int64
		var seqCycle *bool
		object := pgschema.Column{}
		if err := rows.Scan(
			&namespace, &tableName, &object.Name, &object.Position, &object.Type, &object.NotNull,
			&defaultOrGenerated, &identity, &generated, &collation, &object.Comment,
			&seqStart, &seqIncrement, &seqMin, &seqMax, &seqCache, &seqCycle,
			&typeSchema, &typeName, &extensionSchema, &extensionName,
			&defaultSequenceSchema, &defaultSequenceName, &serialSequenceName, &notNullName,
		); err != nil {
			return err
		}
		object.Table = (pgschema.Table{Schema: namespace, Name: tableName}).ObjectID()
		// attnum is permanently sparse after a historical DROP COLUMN. Column
		// order is descriptive only (PostgreSQL cannot ALTER it), so retain a
		// dense ordinal for deterministic rendering and fingerprints instead of
		// treating physical catalog holes as a schema mutation.
		positions[object.Table]++
		object.Position = positions[object.Table]
		if collation != nil {
			object.Collation = *collation
		}
		if notNullName != nil {
			object.NotNullConstraintName = *notNullName
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			continue
		}
		if generated != "" {
			if defaultOrGenerated != nil {
				kind := "STORED"
				if generated == "v" {
					kind = "VIRTUAL"
				}
				object.Generated = &pgschema.Generated{Expression: *defaultOrGenerated, Kind: kind}
			}
		} else {
			object.Default = defaultOrGenerated
		}
		if serialSequenceName != nil && defaultOrGenerated != nil && strings.HasPrefix(*defaultOrGenerated, "nextval(") {
			if serialType, ok := serialTypeForCatalogType(object.Type); ok {
				object.Serial = &pgschema.Serial{Type: serialType, SequenceName: *serialSequenceName}
				object.Default = nil
			}
		}
		if identity != "" {
			object.Identity = &pgschema.Identity{Generation: "BY DEFAULT", Start: 1, Increment: 1, Cache: 1}
			if identity == "a" {
				object.Identity.Generation = "ALWAYS"
			}
			if seqStart != nil {
				object.Identity.Start = *seqStart
			}
			if seqIncrement != nil {
				object.Identity.Increment = *seqIncrement
			}
			object.Identity.Min, object.Identity.Max = seqMin, seqMax
			if seqCache != nil {
				object.Identity.Cache = *seqCache
			}
			if seqCycle != nil {
				object.Identity.Cycle = *seqCycle
			}
		}
		selector := "column:" + namespace + "." + tableName + "." + object.Name
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		if typeSchema != nil && typeName != nil {
			var dependencyID *pgschema.ID
			for _, kind := range []pgschema.Kind{pgschema.KindEnum, pgschema.KindDomain, pgschema.KindComposite, pgschema.KindRange} {
				candidate := pgschema.ID{Kind: kind, Schema: *typeSchema, Name: *typeName}
				if _, exists := snapshot.Object(candidate); exists {
					dependencyID = &candidate
					break
				}
			}
			if dependencyID == nil {
				for _, candidate := range snapshot.Objects() {
					rangeType, ok := candidate.(pgschema.Range)
					if ok && rangeType.Schema == *typeSchema && rangeType.MultirangeName == *typeName {
						id := rangeType.ObjectID()
						dependencyID = &id
						break
					}
				}
			}
			if dependencyID != nil {
				if err := snapshot.AddDependency(object.ObjectID(), *dependencyID); err != nil {
					return err
				}
				// CREATE TABLE consumes its columns into one statement, so the
				// table must carry the same free-standing type dependency for
				// dependency-safe creation and reverse drop ordering.
				if err := snapshot.AddDependency(object.Table, *dependencyID); err != nil {
					return err
				}
			}
		}
		if extensionSchema != nil && extensionName != nil {
			extensionID := (pgschema.Extension{Schema: *extensionSchema, Name: *extensionName}).ObjectID()
			if _, exists := snapshot.Object(extensionID); exists {
				if err := snapshot.AddDependency(object.ObjectID(), extensionID); err != nil {
					return err
				}
				// CREATE/DROP TABLE consumes its columns as one unit, so carry the
				// extension type dependency onto the owning table as well.
				if err := snapshot.AddDependency(object.Table, extensionID); err != nil {
					return err
				}
			}
		}
		if defaultSequenceSchema != nil && defaultSequenceName != nil {
			sequenceID := (pgschema.Sequence{Schema: *defaultSequenceSchema, Name: *defaultSequenceName}).ObjectID()
			if _, exists := snapshot.Object(sequenceID); exists {
				if err := snapshot.AddDependency(object.ObjectID(), sequenceID); err != nil {
					return err
				}
				// CREATE TABLE consumes a newly-created column into its own DDL.
				// Mirror the column edge on the table so scheduling still creates
				// the sequence before that statement (and drops it afterwards).
				if err := snapshot.AddDependency(object.Table, sequenceID); err != nil {
					return err
				}
			}
		}
	}
	return rows.Err()
}

func serialTypeForCatalogType(typ string) (string, bool) {
	switch typ {
	case "smallint":
		return "smallserial", true
	case "integer":
		return "serial", true
	case "bigint":
		return "bigserial", true
	default:
		return "", false
	}
}

func inspectGraphSequences(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, format_type(s.seqtypid, NULL), s.seqstart, s.seqincrement,
       s.seqmin, s.seqmax, s.seqcache, s.seqcycle, c.relpersistence = 'u', obj_description(c.oid, 'pg_class'),
       owned.schema_name, owned.table_name, owned.column_name
FROM pg_sequence s
JOIN pg_class c ON c.oid = s.seqrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN LATERAL (
  SELECT tn.nspname AS schema_name, tc.relname AS table_name, a.attname AS column_name
  FROM pg_depend d
  JOIN pg_class tc ON tc.oid = d.refobjid
  JOIN pg_namespace tn ON tn.oid = tc.relnamespace
  JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
  WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
    AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a'
  LIMIT 1
) owned ON true
WHERE c.relkind = 'S' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('i', 'e')
  )
  AND NOT EXISTS (
    SELECT 1
    FROM pg_depend owner_dep
    JOIN pg_attribute owner_attr
      ON owner_attr.attrelid = owner_dep.refobjid AND owner_attr.attnum = owner_dep.refobjsubid
    WHERE owner_dep.classid = 'pg_class'::regclass AND owner_dep.objid = c.oid
      AND owner_dep.refclassid = 'pg_class'::regclass AND owner_dep.deptype = 'a'
      AND owner_attr.attidentity <> ''
  )
  AND NOT EXISTS (
    SELECT 1
    FROM pg_depend owner_dep
    JOIN pg_attrdef ad ON ad.adrelid = owner_dep.refobjid AND ad.adnum = owner_dep.refobjsubid
    JOIN pg_depend default_dep ON default_dep.classid = 'pg_attrdef'::regclass AND default_dep.objid = ad.oid
      AND default_dep.refclassid = 'pg_class'::regclass AND default_dep.refobjid = c.oid
    WHERE owner_dep.classid = 'pg_class'::regclass AND owner_dep.objid = c.oid
      AND owner_dep.refclassid = 'pg_class'::regclass AND owner_dep.deptype = 'a'
  )
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		object := pgschema.Sequence{}
		var ownedSchema, ownedTable, ownedColumn *string
		if err := rows.Scan(&object.Schema, &object.Name, &object.Type, &object.Start, &object.Increment, &object.Min, &object.Max, &object.Cache, &object.Cycle, &object.Unlogged, &object.Comment, &ownedSchema, &ownedTable, &ownedColumn); err != nil {
			return err
		}
		if ownedSchema != nil && ownedTable != nil && ownedColumn != nil {
			ownedBy := (pgschema.Column{Table: (pgschema.Table{Schema: *ownedSchema, Name: *ownedTable}).ObjectID(), Name: *ownedColumn}).ObjectID()
			object.OwnedBy = &ownedBy
		}
		skip, err := tracker.Skip("sequence:"+object.Schema+"."+object.Name, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
	}
	return rows.Err()
}

func addSequenceOwnershipDependencies(snapshot *pgschema.Snapshot) error {
	for _, candidate := range snapshot.Objects() {
		sequence, ok := candidate.(pgschema.Sequence)
		if !ok || sequence.OwnedBy == nil {
			continue
		}
		if _, exists := snapshot.Object(*sequence.OwnedBy); !exists {
			return fmt.Errorf("sequence %s owned by missing column %s", sequence.ObjectID(), *sequence.OwnedBy)
		}
		if err := snapshot.AddDependency(sequence.ObjectID(), *sequence.OwnedBy); err != nil {
			return err
		}
	}
	return nil
}

func inspectGraphConstraints(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	var parentDependencies [][2]pgschema.ID
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, con.conname, pg_get_constraintdef(con.oid), con.contype::text,
       con.convalidated, con.connoinherit, con.condeferrable, con.condeferred,
       rn.nspname, referenced.relname, conidx.relname, obj_description(con.oid, 'pg_constraint'),
       parent_n.nspname, parent_rel.relname, parent.conname,
       c.relispartition, con.conislocal, con.coninhcount,
       inherited_parent.nspname, inherited_parent.relname, inherited_parent.conname,
       inherited_parent.candidate_count
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class referenced ON referenced.oid = con.confrelid
LEFT JOIN pg_namespace rn ON rn.oid = referenced.relnamespace
LEFT JOIN pg_class conidx ON conidx.oid = con.conindid
LEFT JOIN pg_constraint parent ON parent.oid = con.conparentid
LEFT JOIN pg_class parent_rel ON parent_rel.oid = parent.conrelid
LEFT JOIN pg_namespace parent_n ON parent_n.oid = parent_rel.relnamespace
LEFT JOIN LATERAL (
  SELECT inherited_n.nspname, inherited_rel.relname, inherited.conname,
         count(*) OVER ()::integer AS candidate_count
  FROM pg_inherits inheritance
  JOIN pg_class inherited_rel ON inherited_rel.oid = inheritance.inhparent
  JOIN pg_namespace inherited_n ON inherited_n.oid = inherited_rel.relnamespace
  JOIN pg_constraint inherited ON inherited.conrelid = inheritance.inhparent
  WHERE inheritance.inhrelid = con.conrelid
    AND inherited.contype = 'c'
    AND inherited.conname = con.conname
    AND pg_get_constraintdef(inherited.oid) = pg_get_constraintdef(con.oid)
  ORDER BY inherited.oid
  LIMIT 1
) inherited_parent ON c.relispartition AND con.contype = 'c'
                  AND NOT con.conislocal AND con.coninhcount > 0
WHERE c.relkind IN ('r', 'p') AND con.contype IN ('p', 'u', 'c', 'f', 'x')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, con.conname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var namespace, tableName, kind string
		var partition, local bool
		var inheritanceCount int
		var referenceSchema, referenceName, usingIndex, parentSchema, parentTable, parentName *string
		var inheritedSchema, inheritedTable, inheritedName *string
		var inheritedCandidateCount *int
		object := pgschema.Constraint{}
		if err := rows.Scan(
			&namespace, &tableName, &object.Name, &object.Definition, &kind,
			&object.Validated, &object.NoInherit, &object.Deferrable, &object.Deferred,
			&referenceSchema, &referenceName, &usingIndex, &object.Comment, &parentSchema, &parentTable, &parentName,
			&partition, &local, &inheritanceCount,
			&inheritedSchema, &inheritedTable, &inheritedName, &inheritedCandidateCount,
		); err != nil {
			return err
		}
		object.Table = (pgschema.Table{Schema: namespace, Name: tableName}).ObjectID()
		if _, exists := snapshot.Object(object.Table); !exists {
			continue
		}
		object.Type = graphConstraintType(kind)
		if usingIndex != nil {
			object.UsingIndex = *usingIndex
		}
		if referenceSchema != nil && referenceName != nil {
			reference := (pgschema.Table{Schema: *referenceSchema, Name: *referenceName}).ObjectID()
			object.Reference = &reference
		}
		if parentSchema != nil && parentTable != nil && parentName != nil {
			parent := (pgschema.Constraint{Table: (pgschema.Table{Schema: *parentSchema, Name: *parentTable}).ObjectID(), Name: *parentName}).ObjectID()
			object.Parent = &parent
		}
		selector := "constraint:" + namespace + "." + tableName + "." + object.Name
		// PostgreSQL copies an inherited CHECK constraint onto a partition but
		// does not populate conparentid or a pg_depend edge. The catalog proves
		// this one parent only when the child is a partition, the child CHECK is
		// inherited, and exactly one direct parent has the same catalog-rendered
		// definition. Anything else stays explicit unsupported state rather than
		// becoming a name-based planner guess.
		if object.Type == pgschema.ConstraintCheck && object.Parent == nil && (!local || inheritanceCount > 0) {
			if partition && inheritedCandidateCount != nil && *inheritedCandidateCount == 1 && inheritedSchema != nil && inheritedTable != nil && inheritedName != nil {
				parent := (pgschema.Constraint{Table: (pgschema.Table{Schema: *inheritedSchema, Name: *inheritedTable}).ObjectID(), Name: *inheritedName}).ObjectID()
				object.Parent = &parent
			} else if err := addBlocker("inherited_check_constraint:"+selector, snapshot, tracker); err != nil {
				return err
			}
		}
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		if object.Reference != nil {
			if _, exists := snapshot.Object(*object.Reference); exists {
				if err := snapshot.AddDependency(object.ObjectID(), *object.Reference); err != nil {
					return err
				}
			}
		}
		if object.Parent != nil {
			parentDependencies = append(parentDependencies, [2]pgschema.ID{object.ObjectID(), *object.Parent})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, edge := range parentDependencies {
		if _, exists := snapshot.Object(edge[1]); !exists {
			return fmt.Errorf("partition constraint %s refers to missing parent %s", edge[0], edge[1])
		}
		if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
			return err
		}
	}
	return addForeignKeyKeyDependencies(snapshot)
}

func addConstraintColumnDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, con.conname, a.attname
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN LATERAL unnest(con.conkey) AS key(attnum) ON true
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = key.attnum
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, con.conname, a.attnum`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, name, column string
		if err := rows.Scan(&schema, &table, &name, &column); err != nil {
			return err
		}
		constraintID := (pgschema.Constraint{Table: (pgschema.Table{Schema: schema, Name: table}).ObjectID(), Name: name}).ObjectID()
		columnID := (pgschema.Column{Table: (pgschema.Table{Schema: schema, Name: table}).ObjectID(), Name: column}).ObjectID()
		if _, exists := snapshot.Object(constraintID); !exists {
			continue
		}
		if _, exists := snapshot.Object(columnID); !exists {
			continue
		}
		if err := snapshot.AddDependency(constraintID, columnID); err != nil {
			return err
		}
	}
	return rows.Err()
}

func addForeignKeyKeyDependencies(snapshot *pgschema.Snapshot) error {
	var constraints []pgschema.Constraint
	for _, object := range snapshot.Objects() {
		if constraint, ok := object.(pgschema.Constraint); ok {
			constraints = append(constraints, constraint)
		}
	}
	for _, foreignKey := range constraints {
		if foreignKey.Type != pgschema.ConstraintForeign || foreignKey.Reference == nil {
			continue
		}
		for _, candidate := range constraints {
			if candidate.Table != *foreignKey.Reference || (candidate.Type != pgschema.ConstraintPrimary && candidate.Type != pgschema.ConstraintUnique) {
				continue
			}
			if err := snapshot.AddDependency(foreignKey.ObjectID(), candidate.ObjectID()); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectGraphIndexes(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker, version int) error {
	nullsNotDistinct := indexNullsNotDistinctSelector(version)
	query := fmt.Sprintf(`
SELECT n.nspname, t.relname, t.relkind::text, i.relname, idx.indisunique, idx.indisprimary, idx.indisexclusion,
       am.amname, pg_get_expr(idx.indpred, idx.indrelid), i.reloptions,
       obj_description(i.oid, 'pg_class'), con.conname, %s,
       parent_n.nspname, parent_t.relname, parent_i.relname
FROM pg_index idx
JOIN pg_class i ON i.oid = idx.indexrelid
JOIN pg_class t ON t.oid = idx.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_am am ON am.oid = i.relam
LEFT JOIN pg_constraint con ON con.conindid = idx.indexrelid AND con.contype IN ('p', 'u', 'x')
LEFT JOIN pg_inherits inh ON inh.inhrelid = idx.indexrelid
LEFT JOIN pg_class parent_i ON parent_i.oid = inh.inhparent
LEFT JOIN pg_index parent_idx ON parent_idx.indexrelid = parent_i.oid
LEFT JOIN pg_class parent_t ON parent_t.oid = parent_idx.indrelid
LEFT JOIN pg_namespace parent_n ON parent_n.oid = parent_t.relnamespace
WHERE t.relkind IN ('r', 'p', 'm') AND n.nspname NOT LIKE 'pg_%%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, t.relname, i.relname`, nullsNotDistinct)
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return err
	}
	indexes := make(map[pgschema.ID]pgschema.Index)
	for rows.Next() {
		var namespace, tableName, relationKind string
		var options []string
		var predicate, constraint, parentSchema, parentTable, parentName *string
		object := pgschema.Index{}
		if err := rows.Scan(
			&namespace, &tableName, &relationKind, &object.Name, &object.Unique, &object.Primary, &object.Exclusion,
			&object.Method, &predicate, &options, &object.Comment, &constraint, &object.NullsNotDistinct, &parentSchema, &parentTable, &parentName,
		); err != nil {
			rows.Close()
			return err
		}
		object.Table = relationObjectID(relationKind, namespace, tableName)
		if predicate != nil {
			object.Predicate = *predicate
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			continue
		}
		if constraint != nil {
			object.Constraint = *constraint
		}
		if parentSchema != nil && parentTable != nil && parentName != nil {
			parent := (pgschema.Index{Table: (pgschema.Table{Schema: *parentSchema, Name: *parentTable}).ObjectID(), Name: *parentName}).ObjectID()
			object.Parent = &parent
		}
		parseIndexStorage(options, &object.Storage)
		selector := "index:" + namespace + "." + tableName + "." + object.Name
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			rows.Close()
			return err
		}
		if !skip {
			indexes[object.ObjectID()] = object
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	included := indexIncludeSelector(version)
	partsQuery := fmt.Sprintf(`
SELECT n.nspname, t.relname, t.relkind::text, i.relname, idx.ord, %s,
       a.attname, CASE WHEN idx.key = 0 THEN pg_get_indexdef(idx.indexrelid, idx.ord, false) END,
       CASE WHEN idx.indcollation[idx.ord-1] <> 0 AND coll.collname <> 'default'
            THEN quote_ident(cn.nspname) || '.' || quote_ident(coll.collname) END,
       pg_index_column_has_property(idx.indexrelid, idx.ord, 'desc'),
       pg_index_column_has_property(idx.indexrelid, idx.ord, 'nulls_first'),
       pg_index_column_has_property(idx.indexrelid, idx.ord, 'nulls_last'),
       op.opcname, opn.nspname, op.opcdefault, ia.attoptions
FROM (
  SELECT p.*, generate_series(1, array_length(p.indkey, 1)) AS ord, unnest(p.indkey) AS key
  FROM pg_index p
) idx
JOIN pg_class i ON i.oid = idx.indexrelid
JOIN pg_class t ON t.oid = idx.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
LEFT JOIN pg_attribute a ON (a.attrelid, a.attnum) = (idx.indrelid, idx.key)
LEFT JOIN pg_collation coll ON coll.oid = idx.indcollation[idx.ord-1]
LEFT JOIN pg_namespace cn ON cn.oid = coll.collnamespace
LEFT JOIN pg_opclass op ON op.oid = idx.indclass[idx.ord-1]
LEFT JOIN pg_namespace opn ON opn.oid = op.opcnamespace
LEFT JOIN pg_attribute ia ON (ia.attrelid, ia.attnum) = (idx.indexrelid, idx.ord)
WHERE t.relkind IN ('r', 'p', 'm') AND n.nspname NOT LIKE 'pg_%%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, t.relname, i.relname, idx.ord`, included)
	rows, err = tx.Query(ctx, partsQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var namespace, tableName, relationKind, indexName string
		var ordinal int
		var included bool
		var column, expression, collation, opclassName, opclassSchema *string
		var descending, nullsFirst, nullsLast, opclassDefault *bool
		var parameters []string
		if err := rows.Scan(
			&namespace, &tableName, &relationKind, &indexName, &ordinal, &included,
			&column, &expression, &collation, &descending, &nullsFirst, &nullsLast,
			&opclassName, &opclassSchema, &opclassDefault, &parameters,
		); err != nil {
			return err
		}
		id := (pgschema.Index{Table: relationObjectID(relationKind, namespace, tableName), Name: indexName}).ObjectID()
		object, exists := indexes[id]
		if !exists {
			continue
		}
		if included {
			if column != nil {
				object.Include = append(object.Include, *column)
			}
			indexes[id] = object
			continue
		}
		part := pgschema.IndexPart{}
		if column != nil {
			part.Column = *column
		}
		if expression != nil {
			part.Expression = *expression
		}
		if collation != nil {
			part.Collation = *collation
		}
		if descending != nil {
			part.Descending = *descending
		}
		if nullsFirst != nil {
			part.NullsFirst = *nullsFirst
		}
		if nullsLast != nil {
			part.NullsLast = *nullsLast
		}
		if opclassName != nil && (opclassDefault == nil || !*opclassDefault || len(parameters) > 0) {
			name := *opclassName
			if opclassSchema != nil && *opclassSchema != "pg_catalog" {
				name = *opclassSchema + "." + name
			}
			part.OpClass = &pgschema.OpClass{Name: name}
			if opclassDefault != nil {
				part.OpClass.IsDefault = *opclassDefault
			}
			part.OpClass.Parameters = parseOptions(parameters)
		}
		object.Parts = append(object.Parts, part)
		indexes[id] = object
	}
	if err := rows.Err(); err != nil {
		return err
	}
	ids := make([]pgschema.ID, 0, len(indexes))
	for id := range indexes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		object := indexes[id]
		if err := snapshot.Add(object); err != nil {
			return err
		}
	}
	for _, id := range ids {
		object := indexes[id]
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		if object.Parent != nil {
			if _, exists := snapshot.Object(*object.Parent); !exists {
				return fmt.Errorf("partition index %s refers to missing parent %s", object.ObjectID(), *object.Parent)
			}
			if err := snapshot.AddDependency(object.ObjectID(), *object.Parent); err != nil {
				return err
			}
		}
		if err := addIndexColumnDependencies(snapshot, object); err != nil {
			return err
		}
	}
	return addConstraintIndexDependencies(snapshot)
}

// inspectGraphViews records view definitions from PostgreSQL's own deparser.
// DDL sources therefore take the exact same catalog path as a live database;
// onwardpg never attempts to parse a view body itself.
func inspectGraphViews(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, c.relkind = 'm', pg_get_viewdef(c.oid, true),
       c.relispopulated, c.reloptions, obj_description(c.oid, 'pg_class')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var options []string
		object := pgschema.View{}
		if err := rows.Scan(&object.Schema, &object.Name, &object.Materialized, &object.Definition, &object.Populated, &options, &object.Comment); err != nil {
			return err
		}
		object.Options = parseOptions(options)
		selector := "view:" + object.Schema + "." + object.Name
		if object.Materialized {
			selector = "materialized_view:" + object.Schema + "." + object.Name
		}
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := addViewDependencies(ctx, tx, snapshot); err != nil {
		return err
	}
	return addViewTypeDependencies(ctx, tx, snapshot)
}

// addViewDependencies uses pg_rewrite/pg_depend rather than inspecting SQL
// text. A referenced column is a stronger dependency than its table and makes
// column destructive ordering mechanical. View-on-view dependencies are kept
// as graph edges too. Dependencies on system objects are outside the managed
// catalog graph and intentionally do not become synthetic nodes.
func addViewDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT vn.nspname, vc.relname, vc.relkind::text,
       rn.nspname, rc.relname, rc.relkind::text, d.refobjsubid, ra.attname,
       extn.nspname, ext.extname
FROM pg_rewrite rw
JOIN pg_class vc ON vc.oid = rw.ev_class
JOIN pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = rw.oid
JOIN pg_class rc ON rc.oid = d.refobjid
JOIN pg_namespace rn ON rn.oid = rc.relnamespace
LEFT JOIN pg_attribute ra ON ra.attrelid = rc.oid AND ra.attnum = d.refobjsubid
LEFT JOIN pg_depend extdep ON extdep.classid = 'pg_class'::regclass AND extdep.objid = rc.oid
  AND extdep.refclassid = 'pg_extension'::regclass AND extdep.deptype = 'e'
LEFT JOIN pg_extension ext ON ext.oid = extdep.refobjid
LEFT JOIN pg_namespace extn ON extn.oid = ext.extnamespace
WHERE rw.rulename = '_RETURN'
  AND vc.relkind IN ('v', 'm')
  AND d.refclassid = 'pg_class'::regclass
  AND d.deptype = 'n'
  AND rc.relkind IN ('r', 'p', 'v', 'm')
  AND vn.nspname NOT LIKE 'pg_%' AND vn.nspname <> 'information_schema'
ORDER BY vn.nspname, vc.relname, rn.nspname, rc.relname, d.refobjsubid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var viewSchema, viewName, viewKind, referenceSchema, referenceName, referenceKind string
		var referenceColumn int
		var referenceColumnName, extensionSchema, extensionName *string
		if err := rows.Scan(&viewSchema, &viewName, &viewKind, &referenceSchema, &referenceName, &referenceKind, &referenceColumn, &referenceColumnName, &extensionSchema, &extensionName); err != nil {
			return err
		}
		view := pgschema.View{Schema: viewSchema, Name: viewName, Materialized: viewKind == "m"}.ObjectID()
		if _, exists := snapshot.Object(view); !exists {
			continue
		}
		var reference pgschema.ID
		switch referenceKind {
		case "r", "p":
			reference = (pgschema.Table{Schema: referenceSchema, Name: referenceName}).ObjectID()
			if referenceColumn > 0 {
				if referenceColumnName == nil {
					return fmt.Errorf("view dependency column %s.%s/%d is missing from pg_attribute", referenceSchema, referenceName, referenceColumn)
				}
				reference = (pgschema.Column{Table: reference, Name: *referenceColumnName}).ObjectID()
			}
		case "v", "m":
			reference = (pgschema.View{Schema: referenceSchema, Name: referenceName, Materialized: referenceKind == "m"}).ObjectID()
		}
		if reference == view {
			continue
		}
		if _, exists := snapshot.Object(reference); !exists {
			if extensionSchema != nil && extensionName != nil {
				extensionID := (pgschema.Extension{Schema: *extensionSchema, Name: *extensionName}).ObjectID()
				if _, extensionExists := snapshot.Object(extensionID); extensionExists {
					if err := snapshot.AddDependency(view, extensionID); err != nil {
						return err
					}
				}
			}
			continue
		}
		if err := snapshot.AddDependency(view, reference); err != nil {
			return err
		}
	}
	return rows.Err()
}

// addViewRoutineDependencies preserves pg_rewrite -> pg_proc edges that are
// separate from relation/column dependencies. PostgreSQL records calls from a
// view or materialized view to user routines in this catalog form, so routine
// create/drop ordering can remain entirely graph-derived without attempting to
// parse view SQL or inspect routine bodies.
func addViewRoutineDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT vn.nspname, vc.relname, vc.relkind::text,
       pn.nspname, p.proname, pg_get_function_identity_arguments(p.oid)
FROM pg_rewrite rw
JOIN pg_class vc ON vc.oid = rw.ev_class
JOIN pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = rw.oid
JOIN pg_proc p ON p.oid = d.refobjid
JOIN pg_namespace pn ON pn.oid = p.pronamespace
WHERE rw.rulename = '_RETURN'
  AND vc.relkind IN ('v', 'm')
  AND d.refclassid = 'pg_proc'::regclass
  AND d.deptype = 'n'
  AND vn.nspname NOT LIKE 'pg_%' AND vn.nspname <> 'information_schema'
ORDER BY vn.nspname, vc.relname, pn.nspname, p.proname, pg_get_function_identity_arguments(p.oid)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var viewSchema, viewName, viewKind, routineSchema, routineName, signature string
		if err := rows.Scan(&viewSchema, &viewName, &viewKind, &routineSchema, &routineName, &signature); err != nil {
			return err
		}
		view := pgschema.View{Schema: viewSchema, Name: viewName, Materialized: viewKind == "m"}.ObjectID()
		routine := pgschema.Routine{Schema: routineSchema, Name: routineName, Signature: signature}.ObjectID()
		if _, exists := snapshot.Object(view); !exists {
			continue
		}
		if _, exists := snapshot.Object(routine); !exists {
			// System and extension-owned routines are intentionally outside this
			// graph boundary. User routines omitted by an ignore selector follow
			// the same explicit user acceptance as other ignored dependencies.
			continue
		}
		if err := snapshot.AddDependency(view, routine); err != nil {
			return err
		}
	}
	return rows.Err()
}

// addViewTypeDependencies preserves pg_rewrite -> pg_type edges for modeled
// free-standing types. Never synthesize a type node from a dependency row.
func addViewTypeDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT vn.nspname, vc.relname, vc.relkind::text, tn.nspname, t.typname, t.typtype::text
FROM pg_rewrite rw
JOIN pg_class vc ON vc.oid = rw.ev_class
JOIN pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = rw.oid
JOIN pg_type t ON t.oid = d.refobjid
JOIN pg_namespace tn ON tn.oid = t.typnamespace
WHERE rw.rulename = '_RETURN'
  AND vc.relkind IN ('v', 'm')
  AND d.refclassid = 'pg_type'::regclass
  AND d.deptype = 'n'
  AND t.typtype IN ('e', 'd', 'c')
  AND vn.nspname NOT LIKE 'pg_%' AND vn.nspname <> 'information_schema'
ORDER BY vn.nspname, vc.relname, tn.nspname, t.typname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var viewSchema, viewName, viewKind, typeSchema, typeName, typeKind string
		if err := rows.Scan(&viewSchema, &viewName, &viewKind, &typeSchema, &typeName, &typeKind); err != nil {
			return err
		}
		view := pgschema.View{Schema: viewSchema, Name: viewName, Materialized: viewKind == "m"}.ObjectID()
		kind := pgschema.KindEnum
		if typeKind == "d" {
			kind = pgschema.KindDomain
		} else if typeKind == "c" {
			kind = pgschema.KindComposite
		}
		typeID := pgschema.ID{Kind: kind, Schema: typeSchema, Name: typeName}
		if _, exists := snapshot.Object(view); !exists {
			continue
		}
		if _, exists := snapshot.Object(typeID); !exists {
			continue
		}
		if err := snapshot.AddDependency(view, typeID); err != nil {
			return err
		}
	}
	return rows.Err()
}

// inspectGraphRoutines preserves PostgreSQL's canonical CREATE OR REPLACE
// definition for user functions and procedures. Aggregate semantics are
// intentionally left as blockers: they are a distinct catalog object family.
// PostgreSQL does not record arbitrary PL/pgSQL body references as dependency
// edges, so onwardpg never rewrites a routine body for another schema change.
func inspectGraphRoutines(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
       CASE p.prokind WHEN 'f' THEN 'function' WHEN 'p' THEN 'procedure' END,
       COALESCE(pg_get_function_result(p.oid), ''),
       pg_get_functiondef(p.oid), obj_description(p.oid, 'pg_proc')
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.prokind IN ('f', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e'
  )
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype IN ('e', 'i')
  )
ORDER BY n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		object := pgschema.Routine{}
		if err := rows.Scan(&object.Schema, &object.Name, &object.Signature, &object.Kind, &object.ReturnType, &object.Definition, &object.Comment); err != nil {
			return err
		}
		selector := "routine:" + object.Schema + "." + object.Name + "(" + object.Signature + ")"
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := addSchemaDependency(snapshot, object, object.Schema); err != nil {
			return err
		}
	}
	return rows.Err()
}

// addRoutineDependencies turns PostgreSQL's catalog-proven expression and
// SQL-standard body references into graph edges. String-literal SQL and
// PL/pgSQL bodies deliberately remain opaque because PostgreSQL itself does
// not record their referenced objects in pg_depend.
func addRoutineDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	type edge struct{ object, dependency pgschema.ID }
	var edges []edge
	queries := []string{
		`SELECT pn.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
                rn.nspname, r.proname, pg_get_function_identity_arguments(r.oid)
           FROM pg_depend d
           JOIN pg_proc p ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
           JOIN pg_namespace pn ON pn.oid = p.pronamespace
           JOIN pg_proc r ON d.refclassid = 'pg_proc'::regclass AND d.refobjid = r.oid
           JOIN pg_namespace rn ON rn.oid = r.pronamespace
          WHERE p.oid <> r.oid AND d.deptype = 'n'
            AND pn.nspname NOT LIKE 'pg_%' AND pn.nspname <> 'information_schema'
            AND rn.nspname NOT LIKE 'pg_%' AND rn.nspname <> 'information_schema'`,
	}
	for _, query := range queries {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return err
		}
		for rows.Next() {
			var objectSchema, objectName, objectSignature, dependencySchema, dependencyName, dependencySignature string
			if err := rows.Scan(&objectSchema, &objectName, &objectSignature, &dependencySchema, &dependencyName, &dependencySignature); err != nil {
				rows.Close()
				return err
			}
			edges = append(edges, edge{
				object:     (pgschema.Routine{Schema: objectSchema, Name: objectName, Signature: objectSignature}).ObjectID(),
				dependency: (pgschema.Routine{Schema: dependencySchema, Name: dependencyName, Signature: dependencySignature}).ObjectID(),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	relationRows, err := tx.Query(ctx, `
SELECT pn.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
       rn.nspname, c.relname, c.relkind::text
FROM pg_depend d
JOIN pg_proc p ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid
JOIN pg_namespace pn ON pn.oid = p.pronamespace
JOIN pg_class c ON d.refclassid = 'pg_class'::regclass AND d.refobjid = c.oid
JOIN pg_namespace rn ON rn.oid = c.relnamespace
WHERE d.deptype = 'n' AND c.relkind IN ('r', 'p', 'v', 'm')
  AND pn.nspname NOT LIKE 'pg_%' AND pn.nspname <> 'information_schema'
  AND rn.nspname NOT LIKE 'pg_%' AND rn.nspname <> 'information_schema'
ORDER BY 1, 2, 3, 4, 5`)
	if err != nil {
		return err
	}
	for relationRows.Next() {
		var routineSchema, routineName, routineSignature, relationSchema, relationName, relationKind string
		if err := relationRows.Scan(&routineSchema, &routineName, &routineSignature, &relationSchema, &relationName, &relationKind); err != nil {
			relationRows.Close()
			return err
		}
		edges = append(edges, edge{
			object:     (pgschema.Routine{Schema: routineSchema, Name: routineName, Signature: routineSignature}).ObjectID(),
			dependency: relationObjectID(relationKind, relationSchema, relationName),
		})
	}
	if err := relationRows.Err(); err != nil {
		relationRows.Close()
		return err
	}
	relationRows.Close()

	rows, err := tx.Query(ctx, `
SELECT 'column', n.nspname, c.relname, a.attname, '',
       rn.nspname, r.proname, pg_get_function_identity_arguments(r.oid)
FROM pg_attrdef ad
JOIN pg_class c ON c.oid = ad.adrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = ad.adrelid AND a.attnum = ad.adnum
JOIN pg_depend d ON d.classid = 'pg_attrdef'::regclass AND d.objid = ad.oid
JOIN pg_proc r ON d.refclassid = 'pg_proc'::regclass AND d.refobjid = r.oid
JOIN pg_namespace rn ON rn.oid = r.pronamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND rn.nspname NOT LIKE 'pg_%' AND rn.nspname <> 'information_schema'
UNION ALL
SELECT 'constraint', n.nspname, c.relname, con.conname, '',
       rn.nspname, r.proname, pg_get_function_identity_arguments(r.oid)
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_depend d ON d.classid = 'pg_constraint'::regclass AND d.objid = con.oid
JOIN pg_proc r ON d.refclassid = 'pg_proc'::regclass AND d.refobjid = r.oid
JOIN pg_namespace rn ON rn.oid = r.pronamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND rn.nspname NOT LIKE 'pg_%' AND rn.nspname <> 'information_schema'
UNION ALL
SELECT 'index', tn.nspname, t.relname, i.relname, '',
       rn.nspname, r.proname, pg_get_function_identity_arguments(r.oid)
FROM pg_class i
JOIN pg_index x ON x.indexrelid = i.oid
JOIN pg_class t ON t.oid = x.indrelid
JOIN pg_namespace tn ON tn.oid = t.relnamespace
JOIN pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = i.oid
JOIN pg_proc r ON d.refclassid = 'pg_proc'::regclass AND d.refobjid = r.oid
JOIN pg_namespace rn ON rn.oid = r.pronamespace
WHERE i.relkind IN ('i', 'I')
  AND tn.nspname NOT LIKE 'pg_%' AND tn.nspname <> 'information_schema'
  AND rn.nspname NOT LIKE 'pg_%' AND rn.nspname <> 'information_schema'
ORDER BY 1, 2, 3, 4, 6, 7, 8`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, schema, relation, name, unused, routineSchema, routineName, routineSignature string
		if err := rows.Scan(&kind, &schema, &relation, &name, &unused, &routineSchema, &routineName, &routineSignature); err != nil {
			return err
		}
		table := (pgschema.Table{Schema: schema, Name: relation}).ObjectID()
		var object pgschema.ID
		switch kind {
		case "column":
			object = (pgschema.Column{Table: table, Name: name}).ObjectID()
			edges = append(edges, edge{object: table, dependency: (pgschema.Routine{Schema: routineSchema, Name: routineName, Signature: routineSignature}).ObjectID()})
		case "constraint":
			object = (pgschema.Constraint{Table: table, Name: name}).ObjectID()
		case "index":
			object = (pgschema.Index{Table: table, Name: name}).ObjectID()
		}
		edges = append(edges, edge{object: object, dependency: (pgschema.Routine{Schema: routineSchema, Name: routineName, Signature: routineSignature}).ObjectID()})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range edges {
		if _, exists := snapshot.Object(item.object); !exists {
			continue
		}
		if _, exists := snapshot.Object(item.dependency); !exists {
			continue
		}
		if err := snapshot.AddDependency(item.object, item.dependency); err != nil {
			return err
		}
	}
	return nil
}

func inspectGraphTriggers(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, c.relkind::text, tg.tgname,
       pn.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
       pg_get_triggerdef(tg.oid, true), tg.tgenabled::text,
       obj_description(tg.oid, 'pg_trigger')
FROM pg_trigger tg
JOIN pg_class c ON c.oid = tg.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_proc p ON p.oid = tg.tgfoid
JOIN pg_namespace pn ON pn.oid = p.pronamespace
WHERE NOT tg.tgisinternal AND tg.tgparentid = 0 AND c.relkind IN ('r', 'p', 'v')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, c.relname, tg.tgname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var routineSchema, routineName, routineSignature string
		object := pgschema.Trigger{}
		var schema, relation, relationKind string
		if err := rows.Scan(&schema, &relation, &relationKind, &object.Name, &routineSchema, &routineName, &routineSignature, &object.Definition, &object.Enabled, &object.Comment); err != nil {
			return err
		}
		object.Table = relationObjectID(relationKind, schema, relation)
		object.Routine = (pgschema.Routine{Schema: routineSchema, Name: routineName, Signature: routineSignature}).ObjectID()
		selector := "trigger:" + schema + "." + relation + "." + object.Name
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			if err := snapshot.AddUnsupported("trigger_table:" + object.ObjectID().String()); err != nil {
				return err
			}
			continue
		}
		if _, exists := snapshot.Object(object.Routine); !exists {
			if err := snapshot.AddUnsupported("trigger_routine:" + object.ObjectID().String()); err != nil {
				return err
			}
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Routine); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return addTriggerColumnDependencies(ctx, tx, snapshot)
}

func addTriggerColumnDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, tg.tgname, a.attname
FROM pg_trigger tg
JOIN pg_class c ON c.oid = tg.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN LATERAL unnest(tg.tgattr::smallint[]) AS attrs(attnum) ON true
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = attrs.attnum
WHERE NOT tg.tgisinternal AND tg.tgparentid = 0 AND c.relkind IN ('r', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, tg.tgname, a.attnum`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, trigger, column string
		if err := rows.Scan(&schema, &table, &trigger, &column); err != nil {
			return err
		}
		tableID := (pgschema.Table{Schema: schema, Name: table}).ObjectID()
		triggerID := (pgschema.Trigger{Table: tableID, Name: trigger}).ObjectID()
		columnID := (pgschema.Column{Table: tableID, Name: column}).ObjectID()
		if _, exists := snapshot.Object(triggerID); !exists {
			continue
		}
		if _, exists := snapshot.Object(columnID); !exists {
			return fmt.Errorf("trigger %s column dependency %s missing from graph", triggerID, columnID)
		}
		if err := snapshot.AddDependency(triggerID, columnID); err != nil {
			return err
		}
	}
	return rows.Err()
}

func inspectGraphPolicies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, p.polname, p.polpermissive,
       CASE p.polcmd WHEN '*' THEN 'ALL' WHEN 'r' THEN 'SELECT' WHEN 'a' THEN 'INSERT'
                     WHEN 'w' THEN 'UPDATE' WHEN 'd' THEN 'DELETE' END,
       ARRAY(
         SELECT CASE WHEN role_oid = 0 THEN 'PUBLIC' ELSE r.rolname END
         FROM unnest(p.polroles) AS roles(role_oid)
         LEFT JOIN pg_roles r ON r.oid = roles.role_oid
         ORDER BY CASE WHEN role_oid = 0 THEN 'PUBLIC' ELSE r.rolname END
       ),
       pg_get_expr(p.polqual, p.polrelid, true),
       pg_get_expr(p.polwithcheck, p.polrelid, true)
FROM pg_policy p
JOIN pg_class c ON c.oid = p.polrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, p.polname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	policiesByTable := make(map[pgschema.ID][]pgschema.ID)
	for rows.Next() {
		var schema, table string
		object := pgschema.Policy{}
		if err := rows.Scan(&schema, &table, &object.Name, &object.Permissive, &object.Command, &object.Roles, &object.Using, &object.Check); err != nil {
			return err
		}
		object.Table = (pgschema.Table{Schema: schema, Name: table}).ObjectID()
		selector := "policy:" + schema + "." + table + "." + object.Name
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			if err := snapshot.AddUnsupported("policy_table:" + object.ObjectID().String()); err != nil {
				return err
			}
			continue
		}
		if object.Command == "" || len(object.Roles) == 0 {
			if err := snapshot.AddUnsupported("policy_catalog_state:" + object.ObjectID().String()); err != nil {
				return err
			}
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		policiesByTable[object.Table] = append(policiesByTable[object.Table], object.ObjectID())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	if err := addPolicyDependencies(ctx, tx, snapshot); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `
SELECT n.nspname, c.relname, c.relrowsecurity, c.relforcerowsecurity
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND (c.relrowsecurity OR c.relforcerowsecurity)
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		object := pgschema.RowSecurity{}
		if err := rows.Scan(&schema, &table, &object.Enabled, &object.Forced); err != nil {
			return err
		}
		object.Table = (pgschema.Table{Schema: schema, Name: table}).ObjectID()
		selector := "row_level_security:" + schema + "." + table
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			if err := snapshot.AddUnsupported("row_security_table:" + object.ObjectID().String()); err != nil {
				return err
			}
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		for _, policyID := range policiesByTable[object.Table] {
			if err := snapshot.AddDependency(object.ObjectID(), policyID); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

// addPolicyDependencies records catalog-proven references to columns and
// routines. Other dependency classes remain explicit blockers rather than
// being discarded from the typed graph.
func addPolicyDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, p.polname,
       d.refclassid = 'pg_class'::regclass,
       rn.nspname, rc.relname, a.attname,
       d.refclassid = 'pg_proc'::regclass,
       pn.nspname, pr.proname, pg_get_function_identity_arguments(pr.oid),
       d.refclassid::regclass::text
FROM pg_policy p
JOIN pg_class c ON c.oid = p.polrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_depend d ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid AND d.deptype IN ('n', 'a')
LEFT JOIN pg_class rc ON d.refclassid = 'pg_class'::regclass AND rc.oid = d.refobjid
LEFT JOIN pg_namespace rn ON rn.oid = rc.relnamespace
LEFT JOIN pg_attribute a ON d.refclassid = 'pg_class'::regclass AND a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
LEFT JOIN pg_proc pr ON d.refclassid = 'pg_proc'::regclass AND pr.oid = d.refobjid
LEFT JOIN pg_namespace pn ON pn.oid = pr.pronamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, p.polname, d.refclassid, d.refobjid, d.refobjsubid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, policy, dependencyClass string
		var isRelation, isRoutine bool
		var relationSchema, relationName, column, routineSchema, routineName, routineSignature *string
		if err := rows.Scan(&schema, &table, &policy, &isRelation, &relationSchema, &relationName, &column, &isRoutine, &routineSchema, &routineName, &routineSignature, &dependencyClass); err != nil {
			return err
		}
		tableID := (pgschema.Table{Schema: schema, Name: table}).ObjectID()
		policyID := (pgschema.Policy{Table: tableID, Name: policy}).ObjectID()
		if _, exists := snapshot.Object(policyID); !exists {
			continue
		}
		var dependencyID pgschema.ID
		switch {
		case isRelation && relationSchema != nil && relationName != nil && column != nil:
			dependencyTable := (pgschema.Table{Schema: *relationSchema, Name: *relationName}).ObjectID()
			dependencyID = (pgschema.Column{Table: dependencyTable, Name: *column}).ObjectID()
		case isRelation && relationSchema != nil && relationName != nil && *relationSchema == schema && *relationName == table:
			// The policy already has a typed edge to its owning table.
			continue
		case isRoutine && routineSchema != nil && routineName != nil && routineSignature != nil:
			dependencyID = (pgschema.Routine{Schema: *routineSchema, Name: *routineName, Signature: *routineSignature}).ObjectID()
		default:
			if err := snapshot.AddUnsupported("policy_dependency:" + policyID.String() + ":" + dependencyClass); err != nil {
				return err
			}
			continue
		}
		if _, exists := snapshot.Object(dependencyID); !exists {
			if err := snapshot.AddUnsupported("policy_dependency_missing:" + policyID.String() + ":" + dependencyID.String()); err != nil {
				return err
			}
			continue
		}
		if err := snapshot.AddDependency(policyID, dependencyID); err != nil {
			return err
		}
	}
	return rows.Err()
}

func inspectGraphReplicaIdentities(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, c.relreplident::text, identity_index.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_index identity ON identity.indrelid = c.oid AND identity.indisreplident
LEFT JOIN pg_class identity_index ON identity_index.oid = identity.indexrelid
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, mode string
		var indexName *string
		if err := rows.Scan(&schema, &table, &mode, &indexName); err != nil {
			return err
		}
		object := pgschema.ReplicaIdentity{Table: (pgschema.Table{Schema: schema, Name: table}).ObjectID()}
		switch mode {
		case "d":
			object.Mode = pgschema.ReplicaIdentityDefault
		case "f":
			object.Mode = pgschema.ReplicaIdentityFull
		case "n":
			object.Mode = pgschema.ReplicaIdentityNothing
		case "i":
			object.Mode = pgschema.ReplicaIdentityIndex
			if indexName == nil {
				return fmt.Errorf("replica identity index is missing for %s", object.Table)
			}
			index := (pgschema.Index{Table: object.Table, Name: *indexName}).ObjectID()
			object.Index = &index
		default:
			return fmt.Errorf("unknown replica identity mode %q for %s", mode, object.Table)
		}
		selector := "replica_identity:" + schema + "." + table
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			return fmt.Errorf("replica identity table %s is missing from graph", object.Table)
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
		if object.Index != nil {
			if _, exists := snapshot.Object(*object.Index); !exists {
				return fmt.Errorf("replica identity index %s is missing from graph", *object.Index)
			}
			if err := snapshot.AddDependency(object.ObjectID(), *object.Index); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

func inspectGraphTablePrivileges(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname,
       CASE WHEN x.grantee = 0 THEN 'PUBLIC' ELSE grantee.rolname END,
       CASE WHEN x.grantor = c.relowner THEN '@owner' ELSE grantor.rolname END,
       x.privilege_type, x.is_grantable
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) x
LEFT JOIN pg_roles grantee ON grantee.oid = x.grantee
LEFT JOIN pg_roles grantor ON grantor.oid = x.grantor
WHERE c.relkind IN ('r', 'p') AND x.grantee <> c.relowner
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, x.privilege_type,
         CASE WHEN x.grantee = 0 THEN 'PUBLIC' ELSE grantee.rolname END`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		object := pgschema.TablePrivilege{}
		if err := rows.Scan(&schema, &table, &object.Grantee, &object.Grantor, &object.Privilege, &object.Grantable); err != nil {
			return err
		}
		object.Table = (pgschema.Table{Schema: schema, Name: table}).ObjectID()
		selector := "table_privilege:" + schema + "." + table + "." + object.Privilege + "." + object.Grantee
		skip, err := tracker.Skip(selector, snapshot)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		if object.Grantor != "@owner" {
			if err := snapshot.AddUnsupported("table_privilege_grantor:" + object.ObjectID().String() + ":" + object.Grantor); err != nil {
				return err
			}
			continue
		}
		if _, exists := snapshot.Object(object.Table); !exists {
			if err := snapshot.AddUnsupported("table_privilege_table:" + object.ObjectID().String()); err != nil {
				return err
			}
			continue
		}
		if err := snapshot.Add(object); err != nil {
			return err
		}
		if err := snapshot.AddDependency(object.ObjectID(), object.Table); err != nil {
			return err
		}
	}
	return rows.Err()
}

func addConstraintIndexDependencies(snapshot *pgschema.Snapshot) error {
	for _, object := range snapshot.Objects() {
		constraint, ok := object.(pgschema.Constraint)
		if !ok || constraint.UsingIndex == "" {
			continue
		}
		indexID := (pgschema.Index{Table: constraint.Table, Name: constraint.UsingIndex}).ObjectID()
		if _, exists := snapshot.Object(indexID); !exists {
			continue
		}
		if err := snapshot.AddDependency(constraint.ObjectID(), indexID); err != nil {
			return err
		}
	}
	return nil
}

func addIndexColumnDependencies(snapshot *pgschema.Snapshot, index pgschema.Index) error {
	columns := make([]string, 0, len(index.Parts)+len(index.Include))
	for _, part := range index.Parts {
		if part.Column != "" {
			columns = append(columns, part.Column)
		}
	}
	columns = append(columns, index.Include...)
	for _, name := range columns {
		columnID := (pgschema.Column{Table: index.Table, Name: name}).ObjectID()
		if _, exists := snapshot.Object(columnID); !exists {
			continue
		}
		if err := snapshot.AddDependency(index.ObjectID(), columnID); err != nil {
			return err
		}
	}
	return nil
}

func relationObjectID(kind, schema, name string) pgschema.ID {
	if kind == "m" {
		return (pgschema.View{Schema: schema, Name: name, Materialized: true}).ObjectID()
	}
	if kind == "v" {
		return (pgschema.View{Schema: schema, Name: name}).ObjectID()
	}
	return (pgschema.Table{Schema: schema, Name: name}).ObjectID()
}

func inspectGraphBlockers(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker, version int) error {
	selectors, err := inspectGraphBlockerSelectors(ctx, tx)
	if err != nil {
		return err
	}
	modeled := map[string]bool{
		"schema_comment": true, "relation_comment": true, "column_comment": true,
		"identity_column": true, "column_collation": true, "partitioned_table": true,
		"extension": true, "index": true, "sequence": true, "view": true, "materialized_view": true,
	}
	for _, selector := range selectors {
		if modeled[strings.SplitN(selector, ":", 2)[0]] || strings.HasPrefix(selector, "comment:trigger:") {
			continue
		}
		if err := addBlocker(selector, snapshot, tracker); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, strings.Replace(graphOutsideCoreQuery, "$ROUTINE_KIND$", routineKindPredicate(version), 1))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var selector string
		if err := rows.Scan(&selector); err != nil {
			return err
		}
		if strings.HasPrefix(selector, "comment:trigger:") {
			continue
		}
		if err := addBlocker(selector, snapshot, tracker); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	return inspectCatalogSafetyBlockers(ctx, tx, snapshot, tracker, version)
}

// inspectCatalogSafetyBlockers covers catalog families whose state is not yet
// represented by typed production nodes. Default ownership by the inspecting
// session is normalized as contextual PostgreSQL creation ownership; every
// explicit owner deviation is preserved as a blocker. Extension-owned members
// are intentionally represented atomically by their typed Extension node and
// excluded here through pg_depend.deptype='e'.
func inspectCatalogSafetyBlockers(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker, version int) error {
	rows, err := tx.Query(ctx, graphCatalogSafetyBlockersQuery)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var selector string
		if err := rows.Scan(&selector); err != nil {
			return err
		}
		if strings.HasPrefix(selector, "comment:trigger:") {
			continue
		}
		if err := addBlocker(selector, snapshot, tracker); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	query := catalogVersionSafetyBlockersQuery(version)
	if query == "" {
		return nil
	}
	rows, err = tx.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var selector string
		if err := rows.Scan(&selector); err != nil {
			return err
		}
		if err := addBlocker(selector, snapshot, tracker); err != nil {
			return err
		}
	}
	return rows.Err()
}

func catalogVersionSafetyBlockersQuery(version int) string {
	var queries []string
	if version >= 150000 {
		queries = append(queries, `
SELECT 'parameter_acl:' || quote_ident(parname)
FROM pg_parameter_acl
WHERE paracl IS NOT NULL`)
	}
	if version >= 180000 {
		// PostgreSQL 18 represents named NOT NULL constraints independently in
		// pg_constraint. Column.NotNull retains the truth value, but not the
		// constraint identity, inheritance, validation, or comment semantics.
		queries = append(queries, `
SELECT 'not_null_constraint:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(con.conname)
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
WHERE con.contype = 'n' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND (obj_description(con.oid, 'pg_constraint') IS NOT NULL
       OR NOT con.convalidated OR NOT con.conenforced OR con.connoinherit
       OR (con.conislocal AND con.coninhcount = 0 AND
           (array_length(con.conkey, 1) <> 1 OR a.attname IS NULL
            OR con.conname <> c.relname || '_' || a.attname || '_not_null')))
`)
	}
	if len(queries) == 0 {
		return ""
	}
	return strings.Join(queries, "\nUNION ALL\n") + "\nORDER BY 1"
}

func notNullConstraintNameSelector(version int) string {
	if version < 180000 {
		return "NULL::text"
	}
	return `(SELECT con.conname
FROM pg_constraint con
WHERE con.conrelid = a.attrelid AND con.contype = 'n'
  AND array_length(con.conkey, 1) = 1 AND con.conkey[1] = a.attnum
ORDER BY con.oid
LIMIT 1)`
}

// inspectGraphBlockerSelectors is deliberately selector-only: graph blockers
// use it to identify catalog families which have not yet received typed graph
// nodes. It must not construct or compare a second schema representation.
func inspectGraphBlockerSelectors(ctx context.Context, tx pgx.Tx) ([]string, error) {
	queries := []string{`
SELECT CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' WHEN 'p' THEN 'partitioned_table' END
       || ':' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`, `
SELECT 'index:' || n.nspname || '.' || ic.relname
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'm') AND NOT i.indisprimary AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = i.indexrelid AND d.refclassid = 'pg_constraint'::regclass AND d.deptype = 'i')
ORDER BY 1`, `
SELECT 'extension:' || extname FROM pg_extension ORDER BY 1`, `
SELECT 'sequence:' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`}
	var selectors []string
	for _, query := range queries {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var selector string
			if err := rows.Scan(&selector); err != nil {
				rows.Close()
				return nil, err
			}
			selectors = append(selectors, selector)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return selectors, nil
}

func addBlocker(selector string, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	skip, err := tracker.Skip(selector, snapshot)
	if err != nil {
		return err
	}
	if !skip {
		return snapshot.AddUnsupported(selector)
	}
	return nil
}

const graphOutsideCoreQuery = `
SELECT 'aggregate:' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE $ROUTINE_KIND$ AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
UNION ALL
SELECT 'collation:' || n.nspname || '.' || c.collname
FROM pg_collation c JOIN pg_namespace n ON n.oid = c.collnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'foreign_table:' || n.nspname || '.' || c.relname
FROM pg_foreign_table ft
JOIN pg_class c ON c.oid = ft.ftrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`

const graphCatalogSafetyBlockersQuery = `
SELECT 'ownership:schema:' || quote_ident(n.nspname) || '=' || quote_ident(r.rolname)
FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema' AND r.rolname <> current_user
  AND NOT (n.nspname = 'public' AND (r.rolname = 'pg_database_owner' OR r.oid = (SELECT datdba FROM pg_database WHERE datname = current_database())))
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:relation:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '=' || quote_ident(r.rolname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_roles r ON r.oid = c.relowner
WHERE c.relkind IN ('S', 'v', 'm', 'f') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND r.rolname <> current_user
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:type:' || quote_ident(n.nspname) || '.' || quote_ident(t.typname) || '=' || quote_ident(r.rolname)
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace JOIN pg_roles r ON r.oid = t.typowner
WHERE t.typtype IN ('e', 'd', 'c', 'r', 'm') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND r.rolname <> current_user
  AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.reltype = t.oid AND c.relkind IN ('r', 'p'))
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:routine:' || quote_ident(n.nspname) || '.' || quote_ident(p.proname) || '(' || pg_get_function_identity_arguments(p.oid) || ')=' || quote_ident(r.rolname)
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_roles r ON r.oid = p.proowner
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema' AND r.rolname <> current_user
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
UNION ALL
SELECT 'ownership:extension:' || quote_ident(e.extname) || '=' || quote_ident(r.rolname)
FROM pg_extension e JOIN pg_roles r ON r.oid = e.extowner
WHERE r.rolname <> current_user
UNION ALL
SELECT 'acl:schema:' || quote_ident(n.nspname)
FROM pg_namespace n
WHERE n.nspacl IS NOT NULL AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_init_privs i
    WHERE i.classoid = 'pg_namespace'::regclass AND i.objoid = n.oid AND i.objsubid = 0
      AND i.initprivs IS NOT DISTINCT FROM n.nspacl
  )
UNION ALL
SELECT 'acl:relation:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relacl IS NOT NULL AND c.relkind IN ('S', 'v', 'm', 'f')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'acl:table_owner:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND EXISTS (
    SELECT 1 FROM (
      (SELECT privilege_type, is_grantable
       FROM aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner)))
       WHERE grantee = c.relowner
       EXCEPT
       SELECT privilege_type, is_grantable
       FROM aclexplode(acldefault('r', c.relowner))
       WHERE grantee = c.relowner)
      UNION ALL
      (SELECT privilege_type, is_grantable
       FROM aclexplode(acldefault('r', c.relowner))
       WHERE grantee = c.relowner
       EXCEPT
       SELECT privilege_type, is_grantable
       FROM aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner)))
       WHERE grantee = c.relowner)
    ) owner_acl_drift
  )
UNION ALL
SELECT 'acl:column:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(a.attname)
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE a.attacl IS NOT NULL AND a.attnum > 0 AND NOT a.attisdropped
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'acl:type:' || quote_ident(n.nspname) || '.' || quote_ident(t.typname)
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.typacl IS NOT NULL AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
UNION ALL
SELECT 'acl:routine:' || quote_ident(n.nspname) || '.' || quote_ident(p.proname) || '(' || pg_get_function_identity_arguments(p.oid) || ')'
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.proacl IS NOT NULL AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
UNION ALL
SELECT 'acl:default:' || quote_ident(r.rolname) || ':' || COALESCE(quote_ident(n.nspname), '*') || ':' || d.defaclobjtype::text
FROM pg_default_acl d JOIN pg_roles r ON r.oid = d.defaclrole LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace
UNION ALL
SELECT 'clustered_index:' || quote_ident(n.nspname) || '.' || quote_ident(i.relname)
FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class c ON c.oid = x.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE x.indisclustered AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = i.oid AND d.deptype = 'e')
UNION ALL
SELECT 'invalid_index:' || quote_ident(n.nspname) || '.' || quote_ident(i.relname)
FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class c ON c.oid = x.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE (NOT x.indisvalid OR NOT x.indisready OR NOT x.indislive) AND i.relkind <> 'I'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = i.oid AND d.deptype = 'e')
UNION ALL
SELECT 'table_options:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND c.reloptions IS NOT NULL
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'relation_tablespace:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '=' || quote_ident(t.spcname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_tablespace t ON t.oid = c.reltablespace
WHERE c.relkind IN ('r', 'p', 'S', 'm', 'i', 'I')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'rule:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(r.rulename)
FROM pg_rewrite r JOIN pg_class c ON c.oid = r.ev_class JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE r.rulename <> '_RETURN' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_rewrite'::regclass AND d.objid = r.oid AND d.deptype = 'e')
UNION ALL
SELECT 'text_search_configuration:' || quote_ident(n.nspname) || '.' || quote_ident(c.cfgname)
FROM pg_ts_config c JOIN pg_namespace n ON n.oid = c.cfgnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_ts_config'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'text_search_dictionary:' || quote_ident(n.nspname) || '.' || quote_ident(d.dictname)
FROM pg_ts_dict d JOIN pg_namespace n ON n.oid = d.dictnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend x WHERE x.classid = 'pg_ts_dict'::regclass AND x.objid = d.oid AND x.deptype = 'e')
UNION ALL
SELECT 'text_search_parser:' || quote_ident(n.nspname) || '.' || quote_ident(p.prsname)
FROM pg_ts_parser p JOIN pg_namespace n ON n.oid = p.prsnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_ts_parser'::regclass AND d.objid = p.oid AND d.deptype = 'e')
UNION ALL
SELECT 'text_search_template:' || quote_ident(n.nspname) || '.' || quote_ident(t.tmplname)
FROM pg_ts_template t JOIN pg_namespace n ON n.oid = t.tmplnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_ts_template'::regclass AND d.objid = t.oid AND d.deptype = 'e')
UNION ALL
SELECT 'event_trigger:' || quote_ident(e.evtname)
FROM pg_event_trigger e
WHERE NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_event_trigger'::regclass AND d.objid = e.oid AND d.deptype = 'e')
UNION ALL
SELECT 'publication:' || quote_ident(p.pubname) FROM pg_publication p
UNION ALL
SELECT 'extended_statistics:' || quote_ident(n.nspname) || '.' || quote_ident(s.stxname)
FROM pg_statistic_ext s JOIN pg_namespace n ON n.oid = s.stxnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_statistic_ext'::regclass AND d.objid = s.oid AND d.deptype = 'e')
UNION ALL
SELECT 'foreign_data_wrapper:' || quote_ident(f.fdwname)
FROM pg_foreign_data_wrapper f
WHERE NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_foreign_data_wrapper'::regclass AND d.objid = f.oid AND d.deptype = 'e')
UNION ALL
SELECT 'foreign_server:' || quote_ident(s.srvname) FROM pg_foreign_server s
UNION ALL
SELECT 'user_mapping:' || CASE WHEN u.umuser = 0 THEN 'PUBLIC' ELSE quote_ident(r.rolname) END || '@' || quote_ident(s.srvname)
FROM pg_user_mapping u JOIN pg_foreign_server s ON s.oid = u.umserver LEFT JOIN pg_roles r ON r.oid = u.umuser
UNION ALL
SELECT 'table_inheritance:' || quote_ident(cn.nspname) || '.' || quote_ident(child.relname) || '->' ||
       quote_ident(pn.nspname) || '.' || quote_ident(parent.relname)
FROM pg_inherits i
JOIN pg_class child ON child.oid = i.inhrelid
JOIN pg_namespace cn ON cn.oid = child.relnamespace
JOIN pg_class parent ON parent.oid = i.inhparent
JOIN pg_namespace pn ON pn.oid = parent.relnamespace
WHERE child.relkind = 'r' AND NOT child.relispartition
  AND cn.nspname NOT LIKE 'pg_%' AND cn.nspname <> 'information_schema'
UNION ALL
SELECT 'typed_table:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '=' || format_type(c.reloftype, NULL)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND c.reloftype <> 0
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'table_access_method:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '=' || quote_ident(am.amname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r', 'p', 'm') AND am.amname <> 'heap'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'sequence_persistence:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '=' || c.relpersistence::text
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND c.relpersistence = 't'
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'column_storage:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(a.attname) || '=' || a.attstorage::text
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
WHERE c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped AND a.attstorage <> t.typstorage
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'column_compression:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(a.attname) || '=' || a.attcompression::text
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped AND a.attcompression <> ''::"char"
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'column_statistics:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(a.attname) || '=' || a.attstattarget::text
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped AND a.attstattarget <> -1
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'column_options:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(a.attname)
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped AND a.attoptions IS NOT NULL
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'access_method:' || quote_ident(a.amname)
FROM pg_am a
WHERE a.oid >= 16384
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_am'::regclass AND d.objid = a.oid AND d.deptype = 'e')
UNION ALL
SELECT 'operator:' || quote_ident(n.nspname) || '.' || quote_ident(o.oprname) || '(' ||
       format_type(o.oprleft, NULL) || ',' || format_type(o.oprright, NULL) || ')'
FROM pg_operator o JOIN pg_namespace n ON n.oid = o.oprnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_operator'::regclass AND d.objid = o.oid AND d.deptype = 'e')
UNION ALL
SELECT 'operator_class:' || quote_ident(n.nspname) || '.' || quote_ident(o.opcname) || '@' || quote_ident(a.amname)
FROM pg_opclass o JOIN pg_namespace n ON n.oid = o.opcnamespace JOIN pg_am a ON a.oid = o.opcmethod
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_opclass'::regclass AND d.objid = o.oid AND d.deptype = 'e')
UNION ALL
SELECT 'operator_family:' || quote_ident(n.nspname) || '.' || quote_ident(o.opfname) || '@' || quote_ident(a.amname)
FROM pg_opfamily o JOIN pg_namespace n ON n.oid = o.opfnamespace JOIN pg_am a ON a.oid = o.opfmethod
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_opfamily'::regclass AND d.objid = o.oid AND d.deptype = 'e')
UNION ALL
SELECT 'cast:' || format_type(c.castsource, NULL) || '->' || format_type(c.casttarget, NULL)
FROM pg_cast c
WHERE c.oid >= 16384
  AND NOT EXISTS (
      SELECT 1 FROM pg_depend d
      WHERE d.classid = 'pg_cast'::regclass AND d.objid = c.oid AND d.deptype IN ('e', 'i')
  )
UNION ALL
SELECT 'conversion:' || quote_ident(n.nspname) || '.' || quote_ident(c.conname)
FROM pg_conversion c JOIN pg_namespace n ON n.oid = c.connamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_conversion'::regclass AND d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT 'procedural_language:' || quote_ident(l.lanname)
FROM pg_language l
WHERE l.oid >= 16384
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_language'::regclass AND d.objid = l.oid AND d.deptype = 'e')
UNION ALL
SELECT 'transform:' || format_type(t.trftype, NULL) || '@' || quote_ident(l.lanname)
FROM pg_transform t JOIN pg_language l ON l.oid = t.trflang
WHERE t.oid >= 16384
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_transform'::regclass AND d.objid = t.oid AND d.deptype = 'e')
UNION ALL
SELECT 'subscription:' || quote_ident(s.subname) FROM pg_subscription s
UNION ALL
SELECT 'security_label:' || quote_ident(s.provider) || ':' || identified.identity
FROM pg_seclabel s
CROSS JOIN LATERAL pg_identify_object(s.classoid, s.objoid, s.objsubid) identified
UNION ALL
SELECT 'comment:trigger:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(t.tgname)
FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT t.tgisinternal AND obj_description(t.oid, 'pg_trigger') IS NOT NULL
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'comment:policy:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(p.polname)
FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE obj_description(p.oid, 'pg_policy') IS NOT NULL
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'comment:view_column:' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(a.attname)
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm') AND a.attnum > 0 AND NOT a.attisdropped
  AND col_description(a.attrelid, a.attnum) IS NOT NULL
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`

func routineKindPredicate(version int) string {
	if version >= 110000 {
		return "p.prokind = 'a'"
	}
	return "p.proisagg"
}

// unsupportedAggregatesQuery is kept version-aware because pg_proc changed
// from proisagg to prokind in PostgreSQL 11. Aggregates remain blockers until
// they receive typed graph nodes.
func unsupportedAggregatesQuery(version int) string {
	predicate := "p.proisagg"
	if version >= 110000 {
		predicate = "p.prokind = 'a'"
	}
	return `SELECT 'aggregate:' || n.nspname || '.' || p.proname
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE ` + predicate + ` AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`
}

func generatedColumnSelector(version int) string {
	if version >= 120000 {
		return "a.attgenerated::text"
	}
	return "''::text"
}

func indexIncludeSelector(version int) string {
	if version >= 110000 {
		return "idx.ord > idx.indnkeyatts"
	}
	return "false"
}

func indexNullsNotDistinctSelector(version int) string {
	if version >= 150000 {
		return "idx.indnullsnotdistinct"
	}
	return "false"
}

func addSchemaDependency(snapshot *pgschema.Snapshot, object pgschema.Object, namespace string) error {
	schemaID := (pgschema.Schema{Name: namespace}).ObjectID()
	if _, exists := snapshot.Object(schemaID); !exists {
		return nil
	}
	return snapshot.AddDependency(object.ObjectID(), schemaID)
}

func graphConstraintType(kind string) pgschema.ConstraintType {
	switch kind {
	case "p":
		return pgschema.ConstraintPrimary
	case "u":
		return pgschema.ConstraintUnique
	case "c":
		return pgschema.ConstraintCheck
	case "f":
		return pgschema.ConstraintForeign
	case "x":
		return pgschema.ConstraintExclusion
	default:
		return pgschema.ConstraintType(kind)
	}
}

func parseIndexStorage(options []string, storage *pgschema.IndexStorage) {
	storage.Options = parseOptions(options)
	for _, option := range options {
		name, value, found := strings.Cut(option, "=")
		if !found {
			continue
		}
		switch name {
		case "autosummarize":
			if parsed, err := strconv.ParseBool(value); err == nil {
				storage.AutoSummarize = &parsed
			}
		case "pages_per_range":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				storage.PagesPerRange = &parsed
			}
		}
	}
}

func parseOptions(options []string) []pgschema.Option {
	result := make([]pgschema.Option, 0, len(options))
	for _, option := range options {
		name, value, found := strings.Cut(option, "=")
		if !found {
			result = append(result, pgschema.Option{Name: option})
		} else {
			result = append(result, pgschema.Option{Name: name, Value: value})
		}
	}
	// reloptions are a set semantically, but PostgreSQL may retain them in the
	// order supplied by separate DDL paths. Canonicalize before they enter the
	// graph so physically identical indexes have identical snapshots.
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Value < result[j].Value
		}
		return result[i].Name < result[j].Name
	})
	return result
}

type ignoreTracker struct {
	requested []string
	used      map[string]bool
}

func newIgnoreTracker(selectors []string) (*ignoreTracker, error) {
	tracker := &ignoreTracker{requested: append([]string(nil), selectors...), used: make(map[string]bool)}
	for _, selector := range selectors {
		kind, value, found := strings.Cut(selector, ":")
		if !found || kind == "" || value == "" || strings.Contains(value, "*") && value != "*" {
			return nil, fmt.Errorf("invalid ignore selector %q; expected kind:name or kind:*", selector)
		}
	}
	return tracker, nil
}

func (t *ignoreTracker) Skip(actual string, snapshot *pgschema.Snapshot) (bool, error) {
	kind := strings.SplitN(actual, ":", 2)[0]
	for _, requested := range t.requested {
		if requested == actual || requested == kind+":*" {
			t.used[requested] = true
			if err := snapshot.AddIgnored(actual); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (t *ignoreTracker) Validate() error {
	var unused []string
	for _, selector := range t.requested {
		if !t.used[selector] {
			unused = append(unused, selector)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return fmt.Errorf("unused ignore selectors: %s", strings.Join(unused, ", "))
	}
	return nil
}
