package source

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

func materializeDDLGraph(ctx context.Context, path, devURL string, ignores []string, validateIgnores bool) (*pgschema.Snapshot, error) {
	ddl, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("execute %s: %w", path, err)
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
	if err := addConstraintColumnDependencies(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	if err := inspectGraphViews(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := inspectGraphIndexes(ctx, tx, snapshot, tracker, version); err != nil {
		return nil, err
	}
	if err := inspectGraphRoutines(ctx, tx, snapshot, tracker); err != nil {
		return nil, err
	}
	if err := addViewRoutineDependencies(ctx, tx, snapshot); err != nil {
		return nil, err
	}
	if err := inspectGraphTriggers(ctx, tx, snapshot, tracker); err != nil {
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
	if version < 140000 {
		return fmt.Errorf("PostgreSQL %d is unsupported; onwardpg supports PostgreSQL 14 through 18", version)
	}
	if version >= 190000 {
		return fmt.Errorf("PostgreSQL %d is unsupported; onwardpg supports PostgreSQL 14 through 18", version)
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
	rows, err := tx.Query(ctx, `SELECT e.extname, e.extversion, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var object pgschema.Extension
		if err := rows.Scan(&object.Name, &object.Version, &object.Schema); err != nil {
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
SELECT n.nspname, t.typname, e.enumlabel
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace JOIN pg_enum e ON e.enumtypid = t.oid
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, t.typname, e.enumsortorder`)
	if err != nil {
		return err
	}
	defer rows.Close()
	enums := make(map[string]pgschema.Enum)
	for rows.Next() {
		var namespace, name, label string
		if err := rows.Scan(&namespace, &name, &label); err != nil {
			return err
		}
		key := namespace + "." + name
		object := enums[key]
		object.Schema, object.Name = namespace, name
		object.Labels = append(object.Labels, label)
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

func inspectGraphTables(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, c.relpersistence = 'u', obj_description(c.oid, 'pg_class'), c.relkind::text, c.relispartition,
       CASE pt.partstrat WHEN 'r' THEN 'RANGE' WHEN 'l' THEN 'LIST' WHEN 'h' THEN 'HASH' END,
       pg_get_partkeydef(c.oid), pn.nspname, pc.relname, pg_get_expr(c.relpartbound, c.oid, true)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_partitioned_table pt ON pt.partrelid = c.oid
LEFT JOIN pg_inherits i ON i.inhrelid = c.oid
LEFT JOIN pg_class pc ON pc.oid = i.inhparent
LEFT JOIN pg_namespace pn ON pn.oid = pc.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
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
		var strategy, raw, parentSchema, parentName, bound *string
		if err := rows.Scan(&object.Schema, &object.Name, &object.Unlogged, &object.Comment, &relkind, &isPartition, &strategy, &raw, &parentSchema, &parentName, &bound); err != nil {
			return err
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
	query := fmt.Sprintf(`
SELECT n.nspname, c.relname, a.attname, a.attnum, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       pg_get_expr(ad.adbin, ad.adrelid), a.attidentity::text, %s,
       CASE WHEN a.attcollation <> 0 AND a.attcollation <> typ.typcollation
            THEN quote_ident(cn.nspname) || '.' || quote_ident(coll.collname) END,
       col_description(a.attrelid, a.attnum),
       seq.seqstart, seq.seqincrement, seq.seqmin, seq.seqmax, seq.seqcache, seq.seqcycle,
       dtn.nspname, dt.typname, defaultseq.schema_name, defaultseq.sequence_name, serialseq.relname
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
ORDER BY n.nspname, c.relname, a.attnum`, generated)
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	positions := make(map[pgschema.ID]int)
	for rows.Next() {
		var namespace, tableName, identity, generated string
		var defaultOrGenerated, collation, typeSchema, typeName, defaultSequenceSchema, defaultSequenceName, serialSequenceName *string
		var seqStart, seqIncrement, seqMin, seqMax, seqCache *int64
		var seqCycle *bool
		object := pgschema.Column{}
		if err := rows.Scan(
			&namespace, &tableName, &object.Name, &object.Position, &object.Type, &object.NotNull,
			&defaultOrGenerated, &identity, &generated, &collation, &object.Comment,
			&seqStart, &seqIncrement, &seqMin, &seqMax, &seqCache, &seqCycle,
			&typeSchema, &typeName, &defaultSequenceSchema, &defaultSequenceName, &serialSequenceName,
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
		if _, exists := snapshot.Object(object.Table); !exists {
			continue
		}
		if generated != "" {
			if defaultOrGenerated != nil {
				object.Generated = &pgschema.Generated{Expression: *defaultOrGenerated, Kind: "STORED"}
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
			for _, kind := range []pgschema.Kind{pgschema.KindEnum, pgschema.KindDomain, pgschema.KindComposite} {
				typeID := pgschema.ID{Kind: kind, Schema: *typeSchema, Name: *typeName}
				if _, exists := snapshot.Object(typeID); exists {
					if err := snapshot.AddDependency(object.ObjectID(), typeID); err != nil {
						return err
					}
					break
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
       s.seqmin, s.seqmax, s.seqcache, s.seqcycle, obj_description(c.oid, 'pg_class')
FROM pg_sequence s
JOIN pg_class c ON c.oid = s.seqrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('a', 'i')
  )
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		object := pgschema.Sequence{}
		if err := rows.Scan(&object.Schema, &object.Name, &object.Type, &object.Start, &object.Increment, &object.Min, &object.Max, &object.Cache, &object.Cycle, &object.Comment); err != nil {
			return err
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
		var column, expression, opclassName, opclassSchema *string
		var descending, nullsFirst, nullsLast, opclassDefault *bool
		var parameters []string
		if err := rows.Scan(
			&namespace, &tableName, &relationKind, &indexName, &ordinal, &included,
			&column, &expression, &descending, &nullsFirst, &nullsLast,
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
		if descending != nil {
			part.Descending = *descending
		}
		if nullsFirst != nil {
			part.NullsFirst = *nullsFirst
		}
		if nullsLast != nil {
			part.NullsLast = *nullsLast
		}
		if opclassName != nil {
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
	return addViewEnumDependencies(ctx, tx, snapshot)
}

// addViewDependencies uses pg_rewrite/pg_depend rather than inspecting SQL
// text. A referenced column is a stronger dependency than its table and makes
// column destructive ordering mechanical. View-on-view dependencies are kept
// as graph edges too. Dependencies on system objects are outside the managed
// catalog graph and intentionally do not become synthetic nodes.
func addViewDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT vn.nspname, vc.relname, vc.relkind::text,
       rn.nspname, rc.relname, rc.relkind::text, d.refobjsubid, ra.attname
FROM pg_rewrite rw
JOIN pg_class vc ON vc.oid = rw.ev_class
JOIN pg_namespace vn ON vn.oid = vc.relnamespace
JOIN pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = rw.oid
JOIN pg_class rc ON rc.oid = d.refobjid
JOIN pg_namespace rn ON rn.oid = rc.relnamespace
LEFT JOIN pg_attribute ra ON ra.attrelid = rc.oid AND ra.attnum = d.refobjsubid
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
		var referenceColumnName *string
		if err := rows.Scan(&viewSchema, &viewName, &viewKind, &referenceSchema, &referenceName, &referenceKind, &referenceColumn, &referenceColumnName); err != nil {
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

// addViewEnumDependencies preserves pg_rewrite -> pg_type edges for modeled
// enum types. Domain, range, and composite type families remain explicit
// blockers elsewhere; never synthesize a type node from a dependency row.
func addViewEnumDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	rows, err := tx.Query(ctx, `
SELECT vn.nspname, vc.relname, vc.relkind::text, tn.nspname, t.typname
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
  AND t.typtype = 'e'
  AND vn.nspname NOT LIKE 'pg_%' AND vn.nspname <> 'information_schema'
ORDER BY vn.nspname, vc.relname, tn.nspname, t.typname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var viewSchema, viewName, viewKind, enumSchema, enumName string
		if err := rows.Scan(&viewSchema, &viewName, &viewKind, &enumSchema, &enumName); err != nil {
			return err
		}
		view := pgschema.View{Schema: viewSchema, Name: viewName, Materialized: viewKind == "m"}.ObjectID()
		enum := pgschema.Enum{Schema: enumSchema, Name: enumName}.ObjectID()
		if _, exists := snapshot.Object(view); !exists {
			continue
		}
		if _, exists := snapshot.Object(enum); !exists {
			continue
		}
		if err := snapshot.AddDependency(view, enum); err != nil {
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
       pg_get_functiondef(p.oid), obj_description(p.oid, 'pg_proc')
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.prokind IN ('f', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		object := pgschema.Routine{}
		if err := rows.Scan(&object.Schema, &object.Name, &object.Signature, &object.Kind, &object.Definition, &object.Comment); err != nil {
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

func inspectGraphTriggers(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot, tracker *ignoreTracker) error {
	rows, err := tx.Query(ctx, `
SELECT n.nspname, c.relname, tg.tgname,
       pn.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
       pg_get_triggerdef(tg.oid, true), tg.tgenabled::text
FROM pg_trigger tg
JOIN pg_class c ON c.oid = tg.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_proc p ON p.oid = tg.tgfoid
JOIN pg_namespace pn ON pn.oid = p.pronamespace
WHERE NOT tg.tgisinternal AND c.relkind IN ('r', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname, tg.tgname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var routineSchema, routineName, routineSignature string
		object := pgschema.Trigger{}
		var schema, table string
		if err := rows.Scan(&schema, &table, &object.Name, &routineSchema, &routineName, &routineSignature, &object.Definition, &object.Enabled); err != nil {
			return err
		}
		object.Table = (pgschema.Table{Schema: schema, Name: table}).ObjectID()
		object.Routine = (pgschema.Routine{Schema: routineSchema, Name: routineName, Signature: routineSignature}).ObjectID()
		selector := "trigger:" + schema + "." + table + "." + object.Name
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
WHERE NOT tg.tgisinternal AND c.relkind IN ('r', 'p')
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
		if modeled[strings.SplitN(selector, ":", 2)[0]] {
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
		if err := addBlocker(selector, snapshot, tracker); err != nil {
			return err
		}
	}
	return rows.Err()
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
SELECT 'domain:' || n.nspname || '.' || t.typname
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.typtype = 'd' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'composite:' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'c' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'aggregate:' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE $ROUTINE_KIND$ AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
UNION ALL
SELECT 'collation:' || n.nspname || '.' || c.collname
FROM pg_collation c JOIN pg_namespace n ON n.oid = c.collnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'range_type:' || n.nspname || '.' || t.typname
FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE t.typtype IN ('r', 'm') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'foreign_table:' || n.nspname || '.' || c.relname
FROM pg_foreign_table ft
JOIN pg_class c ON c.oid = ft.ftrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'row_level_security:' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE (c.relrowsecurity OR c.relforcerowsecurity) AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
UNION ALL
SELECT 'policy:' || n.nspname || '.' || c.relname || '.' || p.polname
FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
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
