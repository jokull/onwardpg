package testkit

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/verify"
)

func ApplyPhaseFile(ctx context.Context, connection *pgx.Conn, path string) (int, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read phase file %s: %w", path, err)
	}
	count, err := verify.ExecutePhaseSQL(ctx, connection, body, nil)
	if err != nil {
		return count, fmt.Errorf("apply phase file %s: %w", path, err)
	}
	return count, nil
}

type PhaseBatch struct {
	SQL           string
	Transactional bool
}

func ReadPhaseBatches(path string) ([]PhaseBatch, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read phase file %s: %w", path, err)
	}
	batches, err := bundle.ParsePhaseSQL(body, nil)
	if err != nil {
		return nil, fmt.Errorf("parse phase file %s: %w", path, err)
	}
	result := make([]PhaseBatch, 0, len(batches))
	for _, batch := range batches {
		result = append(result, PhaseBatch{SQL: batch.SQL, Transactional: batch.Transactional})
	}
	return result, nil
}

func ApplyPhaseBatch(ctx context.Context, connection *pgx.Conn, batch PhaseBatch) error {
	if err := verify.ExecutePhaseBatch(ctx, connection, bundle.SQLBatch{SQL: batch.SQL, Transactional: batch.Transactional}); err != nil {
		return fmt.Errorf("apply phase batch: %w", err)
	}
	return nil
}

func EditPhasePocket(path, pocketID string, replacement []byte) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read edit pocket file %s: %w", path, err)
	}
	updated, err := bundle.ReplaceEditPockets(body, map[string][]byte{pocketID: replacement})
	if err != nil {
		return fmt.Errorf("replace edit pocket %s in %s: %w", pocketID, path, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("write edit pocket file %s: %w", path, err)
	}
	return nil
}

func PhasePocketIDs(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read edit pocket file %s: %w", path, err)
	}
	ids, err := bundle.EditPocketIDs(body)
	if err != nil {
		return nil, fmt.Errorf("parse edit pockets in %s: %w", path, err)
	}
	return ids, nil
}
