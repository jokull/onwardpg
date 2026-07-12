// Package source ingests database and DDL inputs into schema.State.
package source

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/schema"
)

type Spec struct {
	Kind  string
	Value string
}

func Parse(value string) Spec {
	if strings.HasPrefix(value, "file://") {
		return Spec{Kind: "ddl", Value: strings.TrimPrefix(value, "file://")}
	}
	return Spec{Kind: "database", Value: value}
}

// Load returns a canonical state. A DDL source requires devURL; it is run in a
// temporary database that is dropped before Load returns.
func Load(ctx context.Context, spec Spec, devURL string, ignores []string) (schema.State, error) {
	switch spec.Kind {
	case "database":
		return inspectURL(ctx, spec.Value, ignores)
	case "ddl":
		if devURL == "" {
			return schema.State{}, fmt.Errorf("a file:// source requires --dev-url")
		}
		return materializeDDL(ctx, spec.Value, devURL, ignores)
	default:
		return schema.State{}, fmt.Errorf("unknown source kind %q", spec.Kind)
	}
}

func materializeDDL(ctx context.Context, path, devURL string, ignores []string) (schema.State, error) {
	ddl, err := os.ReadFile(path)
	if err != nil {
		return schema.State{}, err
	}
	admin, err := pgx.Connect(ctx, devURL)
	if err != nil {
		return schema.State{}, fmt.Errorf("connect dev database: %w", err)
	}
	defer admin.Close(ctx)
	name, err := temporaryName()
	if err != nil {
		return schema.State{}, err
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
		return schema.State{}, fmt.Errorf("create temp database: %w", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
	}()
	config, err := pgx.ParseConfig(devURL)
	if err != nil {
		return schema.State{}, fmt.Errorf("parse dev URL: %w", err)
	}
	config.Database = name
	target, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return schema.State{}, fmt.Errorf("connect temp database: %w", err)
	}
	if _, err = target.Exec(ctx, string(ddl)); err != nil {
		target.Close(ctx)
		return schema.State{}, fmt.Errorf("execute %s: %w", path, err)
	}
	target.Close(ctx)
	return inspectConfig(ctx, config, ignores)
}

func inspectURL(ctx context.Context, url string, ignores []string) (schema.State, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return schema.State{}, fmt.Errorf("parse database URL: %w", err)
	}
	return inspectConfig(ctx, config, ignores)
}

func inspectConfig(ctx context.Context, config *pgx.ConnConfig, ignores []string) (schema.State, error) {
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return schema.State{}, err
	}
	defer conn.Close(ctx)
	state := schema.NewState()
	rows, err := conn.Query(ctx, `
SELECT n.nspname, c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return schema.State{}, err
	}
	type tableName struct{ schema, name string }
	var names []tableName
	for rows.Next() {
		var namespace, name string
		if err := rows.Scan(&namespace, &name); err != nil {
			rows.Close()
			return schema.State{}, err
		}
		names = append(names, tableName{namespace, name})
	}
	if err := rows.Err(); err != nil {
		return schema.State{}, err
	}
	rows.Close()
	for _, name := range names {
		namespace := name.schema
		tableName := name.name
		key := "table:" + namespace + "." + tableName
		if ignored(key, ignores) {
			continue
		}
		table, err := inspectTable(ctx, conn, namespace, tableName)
		if err != nil {
			return schema.State{}, err
		}
		entry := state.Schemas[namespace]
		if entry.Tables == nil {
			entry = schema.Schema{Name: namespace, Tables: map[string]schema.Table{}}
		}
		entry.Tables[tableName] = table
		state.Schemas[namespace] = entry
	}
	state.Unsupported, err = inspectUnsupported(ctx, conn, ignores)
	if err != nil {
		return schema.State{}, err
	}
	return state, nil
}

func inspectTable(ctx context.Context, conn *pgx.Conn, namespace, name string) (schema.Table, error) {
	table := schema.Table{Schema: namespace, Name: name}
	rows, err := conn.Query(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
       pg_get_expr(ad.adbin, ad.adrelid), a.attidentity::text, col_description(a.attrelid, a.attnum)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, namespace, name)
	if err != nil {
		return table, err
	}
	defer rows.Close()
	for rows.Next() {
		var column schema.Column
		var defaultValue, comment *string
		if err := rows.Scan(&column.Name, &column.Type, &column.NotNull, &defaultValue, &column.Identity, &comment); err != nil {
			return table, err
		}
		column.Default, column.Comment = defaultValue, comment
		table.Columns = append(table.Columns, column)
	}
	if err := rows.Err(); err != nil {
		return table, err
	}
	rows.Close()
	constraints, err := conn.Query(ctx, `
SELECT con.conname, pg_get_constraintdef(con.oid), con.contype::text
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND con.contype IN ('p', 'u', 'c', 'f')
ORDER BY con.conname`, namespace, name)
	if err != nil {
		return table, err
	}
	defer constraints.Close()
	for constraints.Next() {
		var constraint schema.Constraint
		if err := constraints.Scan(&constraint.Name, &constraint.Definition, &constraint.Kind); err != nil {
			return table, err
		}
		table.Constraints = append(table.Constraints, constraint)
	}
	return table, constraints.Err()
}

// inspectUnsupported makes a conservative promise: unmodelled PostgreSQL
// resources cause an explicit result instead of a misleading empty plan.
func inspectUnsupported(ctx context.Context, conn *pgx.Conn, ignores []string) ([]string, error) {
	queries := []string{`
SELECT CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' WHEN 'p' THEN 'partitioned_table' END
       || ':' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`, `
SELECT 'index:' || n.nspname || '.' || ic.relname
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT i.indisprimary AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = i.indexrelid AND d.refclassid = 'pg_constraint'::regclass AND d.deptype = 'i')
ORDER BY 1`, `
SELECT 'extension:' || extname FROM pg_extension ORDER BY 1`, `
SELECT 'routine:' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE p.prokind IN ('f', 'p') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
ORDER BY 1`, `
SELECT 'sequence:' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'
ORDER BY 1`}
	var unsupported []string
	for _, query := range queries {
		rows, err := conn.Query(ctx, query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item string
			if err := rows.Scan(&item); err != nil {
				rows.Close()
				return nil, err
			}
			if !ignored(item, ignores) {
				unsupported = append(unsupported, item)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return unsupported, nil
}

func ignored(selector string, ignores []string) bool {
	kind := strings.SplitN(selector, ":", 2)[0]
	for _, ignore := range ignores {
		if ignore == selector || ignore == kind+":*" {
			return true
		}
	}
	return false
}

func temporaryName() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return fmt.Sprintf("onwardpg_ddl_%x", bytes), nil
}

func quote(identifier string) string { return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"` }
