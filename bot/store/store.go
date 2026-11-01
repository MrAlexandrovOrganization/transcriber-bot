// Package store persists group authorization and the durable intake/job/outbox lifecycle.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres (with retries, since the bot may start before the
// database is ready) and creates the schema if needed.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := openWithRetry(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

func openWithRetry(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	const attempts = 10

	for i := 1; i <= attempts; i++ {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return nil, fmt.Errorf("invalid database configuration")
		}
		cfg.MaxConns = max(cfg.MaxConns, 12)
		cfg.ConnConfig.ConnectTimeout = 3 * time.Second
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
		cfg.ConnConfig.RuntimeParams["tcp_keepalives_idle"] = "30"
		cfg.ConnConfig.RuntimeParams["tcp_keepalives_interval"] = "10"
		cfg.ConnConfig.RuntimeParams["tcp_keepalives_count"] = "3"
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		slog.Warn("postgres not ready, retrying", "attempt", i)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, fmt.Errorf("connect postgres after %d attempts failed", attempts)
}

// Add inserts the chat into the allowlist.
// Returns true if the chat was not present before.
func (s *Store) Add(ctx context.Context, chatID int64, title string, addedBy int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		query("chat_add"),
		chatID, title, addedBy,
	)
	if err != nil {
		return false, fmt.Errorf("add chat %d: %w", chatID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Remove deletes the chat from the allowlist.
func (s *Store) Remove(ctx context.Context, chatID int64) error {
	if _, err := s.pool.Exec(ctx, query("chat_remove"), chatID); err != nil {
		return fmt.Errorf("remove chat %d: %w", chatID, err)
	}
	return nil
}

// Exists reports whether the chat is in the allowlist.
func (s *Store) Exists(ctx context.Context, chatID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		query("chat_exists"), chatID,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check chat %d: %w", chatID, err)
	}
	return ok, nil
}

func (s *Store) Close() { s.pool.Close() }
