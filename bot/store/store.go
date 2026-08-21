// Package store persists the allowlist of group chats where the bot is
// allowed to transcribe messages.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	chat_id  BIGINT PRIMARY KEY,
	title    TEXT        NOT NULL DEFAULT '',
	added_by BIGINT      NOT NULL,
	added_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

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
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

func openWithRetry(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	const attempts = 10
	var lastErr error
	for i := 1; i <= attempts; i++ {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return nil, fmt.Errorf("parse dsn: %w", err)
		}
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
		lastErr = err
		slog.Warn("postgres not ready, retrying", "attempt", i, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, fmt.Errorf("connect postgres after %d attempts: %w", attempts, lastErr)
}

// Add inserts the chat into the allowlist.
// Returns true if the chat was not present before.
func (s *Store) Add(ctx context.Context, chatID int64, title string, addedBy int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO chats (chat_id, title, added_by) VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		chatID, title, addedBy,
	)
	if err != nil {
		return false, fmt.Errorf("add chat %d: %w", chatID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Remove deletes the chat from the allowlist.
func (s *Store) Remove(ctx context.Context, chatID int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM chats WHERE chat_id = $1`, chatID); err != nil {
		return fmt.Errorf("remove chat %d: %w", chatID, err)
	}
	return nil
}

// Exists reports whether the chat is in the allowlist.
func (s *Store) Exists(ctx context.Context, chatID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM chats WHERE chat_id = $1)`, chatID,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check chat %d: %w", chatID, err)
	}
	return ok, nil
}

func (s *Store) Close() { s.pool.Close() }
