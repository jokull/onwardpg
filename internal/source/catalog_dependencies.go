package source

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/pgschema"
)

// catalogAddress is PostgreSQL's physical identity for one catalog object or
// subobject. It deliberately remains private to catalog inspection: durable
// plans and fingerprints use logical pgschema IDs, never database-local OIDs.
type catalogAddress struct {
	classID   uint32
	objectID  uint32
	subObject int32
}

type catalogIdentityIndex map[catalogAddress]pgschema.ID

// buildCatalogIdentityIndex projects physical PostgreSQL catalog addresses
// onto the logical objects already admitted to the typed snapshot. Array and
// multirange type OIDs alias their owning logical type so signatures such as
// app.state[] retain the same dependency as app.state.
func buildCatalogIdentityIndex(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) (catalogIdentityIndex, error) {
	rows, err := tx.Query(ctx, catalogIdentityQuery)
	if err != nil {
		return nil, fmt.Errorf("load catalog identity index: %w", err)
	}
	defer rows.Close()

	index := make(catalogIdentityIndex)
	for rows.Next() {
		var address catalogAddress
		var kind, schema, name, part, signature string
		if err := rows.Scan(&address.classID, &address.objectID, &address.subObject, &kind, &schema, &name, &part, &signature); err != nil {
			return nil, fmt.Errorf("scan catalog identity: %w", err)
		}
		id := pgschema.ID{Kind: pgschema.Kind(kind), Schema: schema, Name: name, Part: part, Signature: signature}
		if _, exists := snapshot.Object(id); !exists {
			continue
		}
		if previous, exists := index[address]; exists && previous != id {
			return nil, fmt.Errorf("catalog address (%d,%d,%d) maps to both %s and %s", address.classID, address.objectID, address.subObject, previous, id)
		}
		index[address] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog identity index: %w", err)
	}
	return index, nil
}

// addCatalogDependencies is the final catalog dependency pass. PostgreSQL
// normal dependencies have the lifecycle direction onwardpg needs: the object
// cannot be created before, or outlive, the referenced object. Automatic,
// internal, extension, and pinned dependencies are handled by typed inspectors
// and blockers instead of being flattened into ordinary graph edges.
func addCatalogDependencies(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) error {
	identities, err := buildCatalogIdentityIndex(ctx, tx, snapshot)
	if err != nil {
		return err
	}
	extensionMembers, err := buildExtensionMemberIdentityIndex(ctx, tx, snapshot)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
SELECT classid::oid, objid::oid, objsubid,
       refclassid::oid, refobjid::oid, refobjsubid
FROM pg_depend
WHERE deptype = 'n'
ORDER BY classid, objid, objsubid, refclassid, refobjid, refobjsubid`)
	if err != nil {
		return fmt.Errorf("load normal catalog dependencies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var objectAddress, dependencyAddress catalogAddress
		if err := rows.Scan(
			&objectAddress.classID, &objectAddress.objectID, &objectAddress.subObject,
			&dependencyAddress.classID, &dependencyAddress.objectID, &dependencyAddress.subObject,
		); err != nil {
			return fmt.Errorf("scan normal catalog dependency: %w", err)
		}
		object, objectExists := identities[objectAddress]
		dependency, dependencyExists := identities[dependencyAddress]
		if !dependencyExists {
			dependency, dependencyExists = extensionMembers[dependencyAddress]
		}
		if !objectExists || !dependencyExists || object == dependency || !projectCatalogDependency(object, dependency) {
			continue
		}
		if err := snapshot.AddDependency(object, dependency); err != nil {
			return err
		}
		// PostgreSQL stores defaults and generated expressions as pg_attrdef
		// children of a column. New-table rendering inlines the whole column,
		// so its table must inherit the dependency as well.
		if object.Kind == pgschema.KindColumn {
			table := pgschema.ID{Kind: pgschema.KindTable, Schema: object.Schema, Name: object.Name}
			sameTableColumn := dependency.Kind == pgschema.KindColumn &&
				dependency.Schema == table.Schema && dependency.Name == table.Name
			if _, exists := snapshot.Object(table); exists && table != dependency && !sameTableColumn {
				if err := snapshot.AddDependency(table, dependency); err != nil {
					return err
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate normal catalog dependencies: %w", err)
	}
	return nil
}

// buildExtensionMemberIdentityIndex aliases an extension-owned catalog object
// only when another modeled object depends on it. Extension members never act
// as graph-edge sources: doing so would expose package internals and import
// install-history-dependent member-to-member edges into the atomic extension
// boundary.
func buildExtensionMemberIdentityIndex(ctx context.Context, tx pgx.Tx, snapshot *pgschema.Snapshot) (catalogIdentityIndex, error) {
	rows, err := tx.Query(ctx, `
SELECT member.classid::oid, member.objid::oid, member.objsubid,
       n.nspname, e.extname
FROM pg_depend member
JOIN pg_extension e ON e.oid = member.refobjid
JOIN pg_namespace n ON n.oid = e.extnamespace
WHERE member.refclassid = 'pg_extension'::regclass
  AND member.deptype = 'e'
ORDER BY member.classid, member.objid, member.objsubid`)
	if err != nil {
		return nil, fmt.Errorf("load extension member identities: %w", err)
	}
	defer rows.Close()
	index := make(catalogIdentityIndex)
	for rows.Next() {
		var address catalogAddress
		var schema, name string
		if err := rows.Scan(&address.classID, &address.objectID, &address.subObject, &schema, &name); err != nil {
			return nil, fmt.Errorf("scan extension member identity: %w", err)
		}
		id := (pgschema.Extension{Schema: schema, Name: name}).ObjectID()
		if _, exists := snapshot.Object(id); exists {
			index[address] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate extension member identities: %w", err)
	}
	return index, nil
}

func projectCatalogDependency(object, dependency pgschema.ID) bool {
	// PostgreSQL records relation metadata that refers to a relation's own
	// column subobjects (for example a partition key). The logical table owns
	// those column nodes and is rendered before them, so projecting the
	// physical table -> column row would invent a lifecycle cycle.
	if object.Kind == pgschema.KindTable && dependency.Kind == pgschema.KindColumn &&
		object.Schema == dependency.Schema && object.Name == dependency.Name {
		return false
	}
	return true
}

const catalogIdentityQuery = `
SELECT 'pg_namespace'::regclass::oid, n.oid, 0,
       'schema', '', n.nspname, '', ''
FROM pg_namespace n
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_extension'::regclass::oid, e.oid, 0,
       'extension', n.nspname, e.extname, '', ''
FROM pg_extension e
JOIN pg_namespace n ON n.oid = e.extnamespace

UNION ALL
SELECT 'pg_type'::regclass::oid, physical.oid, 0,
       CASE t.typtype WHEN 'e' THEN 'enum' WHEN 'd' THEN 'domain'
            WHEN 'c' THEN 'composite' WHEN 'r' THEN 'range' END,
       n.nspname, t.typname, '', ''
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
CROSS JOIN LATERAL unnest(array_remove(ARRAY[t.oid, t.typarray], 0::oid)) physical(oid)
WHERE t.typtype IN ('e', 'd', 'c', 'r')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_type'::regclass::oid, physical.oid, 0,
       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table'
            WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' END,
       n.nspname, c.relname, '', ''
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = c.reltype
CROSS JOIN LATERAL unnest(array_remove(ARRAY[t.oid, t.typarray], 0::oid)) physical(oid)
WHERE c.relkind IN ('r', 'p', 'v', 'm')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_type'::regclass::oid, physical.oid, 0,
       'range', n.nspname, range_type.typname, '', ''
FROM pg_range r
JOIN pg_type range_type ON range_type.oid = r.rngtypid
JOIN pg_namespace n ON n.oid = range_type.typnamespace
JOIN pg_type multirange_type ON multirange_type.oid = r.rngmultitypid
CROSS JOIN LATERAL unnest(array_remove(ARRAY[multirange_type.oid, multirange_type.typarray], 0::oid)) physical(oid)
WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_class'::regclass::oid, c.oid, 0,
       CASE c.relkind WHEN 'S' THEN 'sequence' WHEN 'r' THEN 'table'
            WHEN 'p' THEN 'table' WHEN 'v' THEN 'view'
            WHEN 'm' THEN 'materialized_view' END,
       n.nspname, c.relname, '', ''
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('S', 'r', 'p', 'v', 'm')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_class'::regclass::oid, i.oid, 0,
       'index', n.nspname, t.relname, i.relname, ''
FROM pg_class i
JOIN pg_index x ON x.indexrelid = i.oid
JOIN pg_class t ON t.oid = x.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
WHERE i.relkind IN ('i', 'I')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_class'::regclass::oid, c.oid, a.attnum,
       'column', n.nspname, c.relname, a.attname, ''
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_class'::regclass::oid, c.oid, a.attnum,
       'composite', n.nspname, t.typname, '', ''
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_type t ON t.typrelid = c.oid AND t.typtype = 'c'
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE c.relkind = 'c' AND a.attnum > 0 AND NOT a.attisdropped
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_attrdef'::regclass::oid, ad.oid, 0,
       'column', n.nspname, c.relname, a.attname, ''
FROM pg_attrdef ad
JOIN pg_class c ON c.oid = ad.adrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = ad.adrelid AND a.attnum = ad.adnum
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_constraint'::regclass::oid, con.oid, 0,
       'constraint', n.nspname, c.relname, con.conname, ''
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE con.conrelid <> 0
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_constraint'::regclass::oid, con.oid, 0,
       'domain', n.nspname, t.typname, '', ''
FROM pg_constraint con
JOIN pg_type t ON t.oid = con.contypid
JOIN pg_namespace n ON n.oid = t.typnamespace
WHERE con.contypid <> 0
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_proc'::regclass::oid, p.oid, 0,
       'routine', n.nspname, p.proname, '', pg_get_function_identity_arguments(p.oid)
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.prokind IN ('f', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_rewrite'::regclass::oid, rw.oid, 0,
       CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' END,
       n.nspname, c.relname, '', ''
FROM pg_rewrite rw
JOIN pg_class c ON c.oid = rw.ev_class
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE rw.rulename = '_RETURN' AND c.relkind IN ('v', 'm')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_trigger'::regclass::oid, tg.oid, 0,
       'trigger', n.nspname, c.relname, tg.tgname, ''
FROM pg_trigger tg
JOIN pg_class c ON c.oid = tg.tgrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT tg.tgisinternal AND c.relkind IN ('r', 'p', 'v')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

UNION ALL
SELECT 'pg_policy'::regclass::oid, p.oid, 0,
       'policy', n.nspname, c.relname, p.polname, ''
FROM pg_policy p
JOIN pg_class c ON c.oid = p.polrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'

ORDER BY 1, 2, 3, 4, 5, 6, 7, 8`
