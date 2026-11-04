// Package db owns the PostgreSQL pool and the embedded schema migrations.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Connect opens a pool; it does not wait for the server to be reachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return pool, nil
}

// Migrate applies the embedded migrations; a session advisory lock serialises concurrent replicas.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (applied int, err error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return 0, fmt.Errorf("migrations fs: %w", err)
	}
	return migrate(ctx, pool, sub)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) (int, error) {
	// Wait up to 5 min (1 s x 300) for another replica holding the lock.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return 0, fmt.Errorf("migration locker: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return 0, fmt.Errorf("migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}
	return len(results), nil
}

// InTx runs fn in a transaction: committed when fn returns nil, rolled back otherwise.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	if err := pgx.BeginFunc(ctx, pool, fn); err != nil {
		return fmt.Errorf("transaction: %w", err)
	}
	return nil
}
