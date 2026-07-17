package postgres

import (
	"context"
	"errors"

	"github.com/hm2899/grokcli-2api/internal/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Connector struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Connector, error) {
	if databaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Connector{Pool: pool}, nil
}

func (c *Connector) Close() {
	if c != nil && c.Pool != nil {
		c.Pool.Close()
	}
}

func (c *Connector) Acquire(ctx context.Context) (migrate.Session, error) {
	connection, err := c.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &session{connection: connection}, nil
}

type session struct{ connection *pgxpool.Conn }

func (s *session) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := s.connection.Exec(ctx, sql, args...)
	return err
}

func (s *session) QueryRow(ctx context.Context, sql string, args ...any) migrate.Row {
	return &row{row: s.connection.QueryRow(ctx, sql, args...)}
}

func (s *session) Close(context.Context) error {
	s.connection.Release()
	return nil
}

type row struct{ row pgx.Row }

func (r *row) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return migrate.ErrNoRows
	}
	return err
}
