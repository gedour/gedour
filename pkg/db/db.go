// Package db owns the PostgreSQL pool and the embedded schema migrations.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLockTimeout bounds each lock wait of a migration: a waiting ALTER TABLE queues every later query on
// its table behind it, so it fails and the pod restart retries. goose polls with pg_try_advisory_lock, which never waits.
const migrationLockTimeout = "5s"

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
	// Own connections, closed below: a setting on a pooled connection would outlive the migrations. A startup
	// parameter is also what RESET returns to, should a file SET its own wait.
	cfg := pool.Config().ConnConfig
	cfg.RuntimeParams["lock_timeout"] = migrationLockTimeout
	// A data migration reports what it could not move with RAISE WARNING; NOTICEs (IF EXISTS skips) are noise.
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if n.SeverityUnlocalized == "WARNING" {
			zap.L().Warn("migration_warning", zap.String("message", n.Message))
		}
	}
	sqlDB := stdlib.OpenDB(*cfg)
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

// InTx runs fn in a transaction: committed when fn returns nil, rolled back otherwise. A deadlock victim
// (40P01) gets one more attempt in a new transaction, so fn may run twice: it must reset what it captures
// and leave for after InTx returns the side effects that must happen only once.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	err := pgx.BeginFunc(ctx, pool, fn)
	if isDeadlock(err) && waitJitter(ctx) {
		// The caller's line names the transaction: closures do not all wrap their errors.
		zap.L().WithOptions(zap.AddCallerSkip(1)).Warn("tx_deadlock_retry", zap.Error(err))
		err = pgx.BeginFunc(ctx, pool, fn)
	}
	if err != nil {
		return fmt.Errorf("transaction: %w", err)
	}
	return nil
}

func isDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// waitJitter waits 10 to 50 ms, so the other side of the deadlock can finish; false if ctx ended first.
func waitJitter(ctx context.Context) bool {
	t := time.NewTimer(10*time.Millisecond + rand.N(40*time.Millisecond)) //nolint:gosec // jitter, not a secret
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
