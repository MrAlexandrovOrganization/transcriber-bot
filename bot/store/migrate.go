package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Startup migration preserves this application's existing deployment contract.
// Ordered SQL migrations and their ledger commit together under an advisory lock.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, query("migrate_settings")); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, query("migrate_lock")); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, query("migrate_init")); err != nil {
		return err
	}
	entries, err := statements.ReadDir("sql/migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var applied bool
		if err = tx.QueryRow(ctx, query("migrate_applied"), entry.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		data, err := statements.ReadFile("sql/migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(data)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, query("migrate_record"), entry.Name()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
