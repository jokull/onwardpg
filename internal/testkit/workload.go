package testkit

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type PreparedAction struct {
	Name string
	SQL  string
}

type ClientContract struct {
	Name     string
	Prepared []PreparedAction
	Actions  []ClientAction
}

type ClientAction struct {
	Name             string
	SQL              string
	Arguments        []any
	ExpectedRow      []any
	ExpectedSQLState string
}

type ReleaseClientContracts struct {
	Legacy ClientContract
	New    ClientContract
	Final  ClientContract
}

func (c ReleaseClientContracts) Validate() error {
	for stage, contract := range map[string]ClientContract{"legacy": c.Legacy, "new": c.New, "final": c.Final} {
		if contract.Name == "" {
			return fmt.Errorf("%s client contract requires a name", stage)
		}
		if len(contract.Prepared) == 0 && len(contract.Actions) == 0 {
			return fmt.Errorf("%s client contract requires at least one prepared statement or action", stage)
		}
		for _, action := range contract.Actions {
			if action.Name == "" || action.SQL == "" {
				return fmt.Errorf("%s client action requires a name and SQL", stage)
			}
			if action.ExpectedSQLState != "" && action.ExpectedRow != nil {
				return fmt.Errorf("%s client action %s cannot expect both a row and SQLSTATE", stage, action.Name)
			}
		}
	}
	return nil
}

func PrepareAll(ctx context.Context, connection *pgx.Conn, actions []PreparedAction) error {
	for _, action := range actions {
		if action.Name == "" || action.SQL == "" {
			return fmt.Errorf("prepared action requires a name and SQL")
		}
		if _, err := connection.Prepare(ctx, action.Name, action.SQL); err != nil {
			return fmt.Errorf("prepare %s: %w", action.Name, err)
		}
	}
	return nil
}

func RunClientActions(ctx context.Context, connection *pgx.Conn, actions []ClientAction) error {
	for _, action := range actions {
		if action.Name == "" || action.SQL == "" {
			return fmt.Errorf("client action requires a name and SQL")
		}
		if action.ExpectedRow != nil {
			if err := ExpectRow(ctx, connection, action.SQL, action.ExpectedRow, action.Arguments...); err != nil {
				return fmt.Errorf("client action %s: %w", action.Name, err)
			}
			continue
		}
		_, err := connection.Exec(ctx, action.SQL, action.Arguments...)
		if action.ExpectedSQLState != "" {
			if err == nil {
				return fmt.Errorf("client action %s succeeded, want SQLSTATE %q", action.Name, action.ExpectedSQLState)
			}
			if state := SQLState(err); state != action.ExpectedSQLState {
				return fmt.Errorf("client action %s SQLSTATE = %q, want %q: %w", action.Name, state, action.ExpectedSQLState, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("client action %s: %w", action.Name, err)
		}
	}
	return nil
}

func QueryRowValues(ctx context.Context, connection *pgx.Conn, sql string, arguments ...any) ([]any, error) {
	rows, err := connection.Query(ctx, sql, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		return nil, fmt.Errorf("query returned no rows")
	}
	values, err := rows.Values()
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		return nil, fmt.Errorf("query returned more than one row")
	}
	return values, rows.Err()
}

func ExpectRow(ctx context.Context, connection *pgx.Conn, sql string, expected []any, arguments ...any) error {
	actual, err := QueryRowValues(ctx, connection, sql, arguments...)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("query %q values = %#v, want %#v", sql, actual, expected)
	}
	return nil
}

func SQLState(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code
	}
	return ""
}
