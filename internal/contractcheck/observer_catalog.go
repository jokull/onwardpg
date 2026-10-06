package contractcheck

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/pgschema"
)

// InspectObserverCatalog reads a live catalog through the same explicit
// least-privilege observer boundary used by contract readiness. The returned
// graph projects only the proven inspection overlay: the database owner's
// ambient identity and owner-granted, non-grantable SELECT access belonging to
// the dedicated observer roles. Application authorization remains in the
// graph and therefore remains visible to drift comparison. liveIgnore removes
// only the unsupported-state selectors a project acknowledged for its live
// clusters (see source.ProjectLiveIgnored) and reports those it removed.
func InspectObserverCatalog(ctx context.Context, databaseURL string, ignores, liveIgnore []string, statementTimeout time.Duration) (*pgschema.Snapshot, ObserverProjection, *Finding, error) {
	if databaseURL == "" {
		return nil, ObserverProjection{}, nil, fmt.Errorf("observer database URL is required")
	}
	if statementTimeout <= 0 {
		statementTimeout = 30 * time.Second
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, ObserverProjection{}, nil, fmt.Errorf("parse observer database URL: %w", err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, ObserverProjection{}, nil, fmt.Errorf("connect observer database: %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, ObserverProjection{}, nil, fmt.Errorf("begin read-only observer snapshot: %w", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", statementTimeout.String()); err != nil {
		return nil, ObserverProjection{}, nil, fmt.Errorf("set observer statement timeout: %w", err)
	}

	observer, finding, err := inspectObserver(ctx, tx)
	if err != nil {
		return nil, ObserverProjection{}, nil, fmt.Errorf("inspect observer: %w", err)
	}
	report := ObserverProjection{Role: observer.Role, DatabaseOwner: observer.DatabaseOwner, Mode: observer.Mode()}
	if finding != nil {
		return nil, report, finding, nil
	}
	snapshot, err := source.InspectGraphTransaction(ctx, tx, ignores, false)
	if err != nil {
		return nil, report, nil, fmt.Errorf("inspect catalog read-only: %w", err)
	}
	snapshot, projected, finding, err := projectObserverSnapshot(snapshot, observer)
	if err != nil {
		return nil, report, nil, err
	}
	report.ProjectedAccess = projected
	if finding != nil {
		return snapshot, report, finding, nil
	}
	observed := snapshot
	snapshot, report.LiveIgnored, report.LiveIgnoreUnmatched, err = source.ProjectLiveIgnored(observed, liveIgnore)
	if err != nil {
		return nil, report, nil, err
	}
	if len(report.LiveIgnored) > 0 {
		if report.ObservedFingerprint, err = observed.Fingerprint(); err != nil {
			return nil, report, nil, err
		}
	}
	return snapshot, report, nil, nil
}
