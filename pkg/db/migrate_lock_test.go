package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gedour/gedour/pkg/testpg"
)

func lockTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := Connect(context.Background(), testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// holdTable creates table guarded and keeps an ACCESS SHARE lock on it until the returned func runs.
func holdTable(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE guarded (id int)`); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT * FROM guarded`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = holder.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

func waitLockWaiters(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
	}
	t.Fatalf("fewer than %d sessions waiting on a lock", want)
}

// A migration blocked by a long transaction gives up with 55P03 and frees the reader queued behind it, while
// the transaction still runs; the next run, the pod restart in production, applies the file.
func TestMigrateLockTimeout(t *testing.T) {
	for name, header := range map[string]string{"transaction": "", "no transaction": "-- +goose NO TRANSACTION\n"} {
		t.Run(name, func(t *testing.T) {
			pool := lockTestPool(t)
			release := holdTable(t, pool)
			fsys := fstest.MapFS{"00001_alter.sql": {Data: []byte(header + "-- +goose Up\nALTER TABLE guarded ADD COLUMN x int;\n")}}
			// Without a lock timeout the migration would wait for as long as the holder runs.
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			start := time.Now()
			migrated := make(chan error, 1)
			go func() {
				_, err := migrate(ctx, pool, fsys)
				migrated <- err
			}()
			waitLockWaiters(t, pool, 1)
			read := make(chan error, 1)
			go func() {
				_, err := pool.Exec(ctx, `SELECT count(*) FROM guarded`)
				read <- err
			}()
			waitLockWaiters(t, pool, 2)

			err := <-migrated
			took := time.Since(start)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "55P03" {
				t.Fatalf("after %v: want SQLSTATE 55P03, got %v", took, err)
			}
			if took < 5*time.Second || took > 10*time.Second {
				t.Fatalf("gave up after %v, want about 5s", took)
			}
			if err := <-read; err != nil {
				t.Fatalf("reader behind the migration: %v", err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("reader freed after %v", took)
			}

			release()
			if n, err := migrate(context.Background(), pool, fsys); err != nil || n != 1 {
				t.Fatalf("retry: applied=%d err=%v", n, err)
			}
		})
	}
}

// Files of both kinds run under the timeout, RESET returns to it after a file's own SET, and neither reaches
// the application pool.
func TestMigrateLockTimeoutScope(t *testing.T) {
	ctx := context.Background()
	pool := lockTestPool(t)
	record := "INSERT INTO seen SELECT current_setting('lock_timeout');\n"
	fsys := fstest.MapFS{
		"00001_tx.sql":     {Data: []byte("-- +goose Up\nCREATE TABLE seen (v text);\n" + record)},
		"00002_no_tx.sql":  {Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\n" + record)},
		"00003_reset.sql":  {Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = 0;\nRESET lock_timeout;\n" + record)},
		"00004_set_it.sql": {Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = '7s';\n")},
	}
	if _, err := migrate(ctx, pool, fsys); err != nil {
		t.Fatal(err)
	}
	var seen []string
	rows, err := pool.Query(ctx, `SELECT v FROM seen`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, v)
	}
	if len(seen) != 3 || seen[0] != "5s" || seen[1] != "5s" || seen[2] != "5s" {
		t.Errorf("lock_timeout seen by the files: %v (%v)", seen, rows.Err())
	}

	// Held together, so each SHOW lands on a different connection, idle ones first.
	for range 4 {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		var v string
		if err := conn.QueryRow(ctx, `SHOW lock_timeout`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != "0" {
			t.Fatalf("pool connection %d carries lock_timeout %q", conn.Conn().PgConn().PID(), v)
		}
	}
}

// A file that must wait longer says so with SET LOCAL, which the session setting does not override.
func TestMigrateSetLocalWins(t *testing.T) {
	pool := lockTestPool(t)
	release := holdTable(t, pool)
	fsys := fstest.MapFS{"00001_alter.sql": {Data: []byte("-- +goose Up\nSET LOCAL lock_timeout = 0;\nALTER TABLE guarded ADD COLUMN x int;\n")}}
	start := time.Now()
	time.AfterFunc(6*time.Second, release)
	if n, err := migrate(context.Background(), pool, fsys); err != nil || n != 1 {
		t.Fatalf("applied=%d err=%v", n, err)
	}
	if took := time.Since(start); took < 6*time.Second {
		t.Fatalf("applied after %v, before the holder ended", took)
	}
}

// A replica waiting for another one's migrations holds no lock wait: the 5 s do not cut it.
func TestMigrateAdvisoryWaitOutlivesLockTimeout(t *testing.T) {
	ctx := context.Background()
	dbURL := testpg.NewDB(t)
	fsys := fstest.MapFS{"00001_slow.sql": {Data: []byte("-- +goose Up\nSELECT pg_sleep(6);\n")}}
	type outcome struct {
		applied int
		err     error
	}
	results := make([]outcome, 2)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range results {
		pool, err := Connect(ctx, dbURL)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		wg.Go(func() {
			results[i].applied, results[i].err = migrate(ctx, pool, fsys)
		})
	}
	wg.Wait()
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("migrator %d after %v: %v", i, time.Since(start), r.err)
		}
	}
	if total := results[0].applied + results[1].applied; total != 1 {
		t.Fatalf("applied %d times", total)
	}
	if took := time.Since(start); took < 6*time.Second {
		t.Fatalf("both done after %v: the second one did not wait", took)
	}
}
