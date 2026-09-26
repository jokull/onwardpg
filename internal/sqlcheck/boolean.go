// Package sqlcheck evaluates reviewed, user-authored database assertions.
// Read-only execution and rollback limit ordinary SQL effects; they do not
// sandbox functions with external effects or changes of execution identity.
package sqlcheck

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Beginner can be a connection or an existing transaction. pgx implements
// nested transactions using savepoints.
type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

var ErrRowSecurity = errors.New("row-level security can hide rows from the assertion role")

// RequireUnfilteredRows conservatively rejects a database with any table that
// would filter rows for the current role. A function can temporarily enable RLS
// and restore the setting before returning, so row_security=off alone is not a
// sufficient visibility check. This examines the whole database, including
// tables outside the managed catalog, because functions can reference them.
// Functions that change execution identity still require caller review.
func RequireUnfilteredRows(ctx context.Context, tx pgx.Tx) error {
	var relation string
	err := tx.QueryRow(ctx, `
SELECT pg_catalog.format('%I.%I', n.nspname, c.relname)
FROM pg_catalog.pg_class AS c
JOIN pg_catalog.pg_namespace AS n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
WHERE c.relrowsecurity AND pg_catalog.row_security_active(c.oid)
ORDER BY c.oid
LIMIT 1`, pgx.QueryExecModeExec).Scan(&relation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect assertion row visibility: %w", err)
	}
	return fmt.Errorf("%w: %s", ErrRowSecurity, relation)
}

// Boolean requires exactly one non-null PostgreSQL Boolean value. Assertions
// see the caller's uncommitted work, but run read-only and always roll back their
// own transaction or savepoint. This also restores transaction-local settings.
// Any table subject to RLS for the current role blocks assertions, even if a
// particular assertion does not use that table.
func Boolean(ctx context.Context, db Beginner, sql string) (passed bool, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(cleanup); rollbackErr != nil {
			passed = false
			err = errors.Join(err, fmt.Errorf("roll back assertion: %w", rollbackErr))
		}
	}()
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return false, err
	}
	// Fail when RLS would hide rows, including FORCE RLS for a database owner.
	// This setting does not grant the role permission to bypass RLS.
	if _, err := tx.Exec(ctx, "SET LOCAL row_security = off"); err != nil {
		return false, err
	}
	// Establish a snapshot before running user SQL so it cannot switch the
	// transaction back to read-write with set_config().
	var readOnly string
	if err := tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('transaction_read_only')", pgx.QueryExecModeExec).Scan(&readOnly); err != nil {
		return false, err
	}
	if err := RequireUnfilteredRows(ctx, tx); err != nil {
		return false, err
	}
	// Describe without executing first: even a single COMMIT is valid extended
	// protocol SQL and could commit the caller's work before rows are checked.
	// An unnamed statement avoids persistent prepared-statement cache entries.
	description, err := tx.Prepare(ctx, "", sql)
	if err != nil {
		return false, err
	}
	if len(description.Fields) != 1 || description.Fields[0].DataTypeOID != pgtype.BoolOID {
		return false, fmt.Errorf("assertion must return exactly one Boolean column")
	}
	// Force extended protocol even when the connection defaults to simple
	// protocol. PostgreSQL then rejects statement lists, including COMMIT escapes.
	rows, err := tx.Query(ctx, sql, pgx.QueryExecModeExec)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	if len(fields) != 1 || fields[0].DataTypeOID != pgtype.BoolOID {
		return false, fmt.Errorf("assertion must return exactly one Boolean column")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, err
		}
		return false, fmt.Errorf("assertion must return exactly one row")
	}
	if err := rows.Scan(&passed); err != nil {
		return false, fmt.Errorf("scan Boolean assertion: %w", err)
	}
	if rows.Next() {
		return false, fmt.Errorf("assertion returned more than one row")
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return passed, nil
}
