package testkit

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jokull/onwardpg/internal/scratchdb"
)

type Postgres struct {
	Database *scratchdb.Database
}

func NewPostgres(ctx context.Context, adminURL, prefix string) (*Postgres, error) {
	database, err := scratchdb.Create(ctx, adminURL, prefix)
	if err != nil {
		return nil, err
	}
	return &Postgres{Database: database}, nil
}

func NativePostgresMajor(ctx context.Context, databaseURL string) (int, error) {
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return 0, fmt.Errorf("connect PostgreSQL major probe: %w", err)
	}
	defer connection.Close(context.Background())
	var versionNumber int
	if err := connection.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&versionNumber); err != nil {
		return 0, fmt.Errorf("read PostgreSQL major: %w", err)
	}
	return versionNumber / 10000, nil
}

func (p *Postgres) Config() *pgx.ConnConfig {
	if p == nil || p.Database == nil || p.Database.Config == nil {
		return nil
	}
	return p.Database.Config.Copy()
}

func (p *Postgres) URL() string {
	config := p.Config()
	if config == nil {
		return ""
	}
	return connectionURL(config)
}

func (p *Postgres) Connect(ctx context.Context) (*pgx.Conn, error) {
	if p == nil || p.Database == nil {
		return nil, fmt.Errorf("test PostgreSQL database is not initialized")
	}
	return p.Database.Connect(ctx)
}

func connectionURL(config *pgx.ConnConfig) string {
	endpoint := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(config.User, config.Password),
		Host:   net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))),
		Path:   "/" + config.Database,
	}
	query := endpoint.Query()
	if config.TLSConfig == nil {
		query.Set("sslmode", "disable")
	}
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func (p *Postgres) Apply(ctx context.Context, sql []byte) error {
	connection, err := p.Connect(ctx)
	if err != nil {
		return err
	}
	defer connection.Close(context.Background())
	reader := connection.PgConn().Exec(ctx, string(sql))
	if _, err := reader.ReadAll(); err != nil {
		return fmt.Errorf("apply test SQL: %w", err)
	}
	return nil
}

func (p *Postgres) Close() error {
	if p == nil || p.Database == nil {
		return nil
	}
	err := p.Database.Close()
	p.Database = nil
	return err
}
