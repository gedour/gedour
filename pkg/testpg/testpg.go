// Package testpg gives integration tests a throwaway PostgreSQL database.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Image is the PostgreSQL image used by integration tests; keep it aligned with compose.yaml.
const Image = "postgres:18-alpine"

var (
	once     sync.Once
	ctr      *postgres.PostgresContainer
	adminURL string
	startErr error
)

func start() {
	ctx := context.Background()
	ctr, startErr = postgres.Run(ctx, Image,
		postgres.WithDatabase("gedour"),
		postgres.WithUsername("gedour"),
		postgres.WithPassword("gedour"),
		postgres.BasicWaitStrategies(),
	)
	if startErr != nil {
		return
	}
	adminURL, startErr = ctr.ConnectionString(ctx, "sslmode=disable")
}

// globalLog receives what pkg/db logs: a tx_deadlock_retry the retry would otherwise hide from a test.
var globalLog = func() *observer.ObservedLogs {
	core, logs := observer.New(zap.WarnLevel)
	zap.ReplaceGlobals(zap.New(core, zap.AddCaller()))
	return logs
}()

var retriesAllowed = map[testing.TB]bool{}

func deadlockRetries() int { return globalLog.FilterMessage("tx_deadlock_retry").Len() }

// AllowDeadlockRetries lets t see db.InTx retry a deadlock; the func counts the retries since this call.
func AllowDeadlockRetries(t testing.TB) func() int {
	t.Helper()
	retriesAllowed[t] = true
	start := deadlockRetries()
	return func() int { return deadlockRetries() - start }
}

// failOnDeadlockRetry keeps lock-order tests able to fail: the retry answers 2xx where a lost lock used to
// answer 500, so the retry itself is the failure unless the test allowed it.
func failOnDeadlockRetry(t testing.TB) {
	start := deadlockRetries()
	t.Cleanup(func() {
		if retriesAllowed[t] {
			return
		}
		for _, e := range globalLog.FilterMessage("tx_deadlock_retry").All()[start:] {
			t.Errorf("db.InTx retried a deadlock at %s, a lock order broke (testpg.AllowDeadlockRetries if this test forces one): %v", e.Caller.TrimmedPath(), e.ContextMap()["error"])
		}
	})
}

// ForceDeadlockOnce fails the next commit that wrote to table with SQLSTATE 40P01, the transaction's locks
// held until the returned func runs, so a test can queue a concurrent change for the retry to see.
func ForceDeadlockOnce(t testing.TB, pool *pgxpool.Pool, table string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7007)`); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
	release = sync.OnceFunc(func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(7007)`)
		conn.Release()
	})
	t.Cleanup(release)
	for _, sql := range []string{
		`CREATE SEQUENCE IF NOT EXISTS force_deadlock`,
		`CREATE OR REPLACE FUNCTION force_deadlock() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF nextval('force_deadlock') = 1 THEN
				PERFORM pg_advisory_xact_lock(7007);
				RAISE EXCEPTION 'forced deadlock' USING ERRCODE = '40P01';
			END IF;
			RETURN NULL;
		END $$`,
		`CREATE CONSTRAINT TRIGGER force_deadlock AFTER INSERT OR UPDATE ON ` + pgx.Identifier{table}.Sanitize() + `
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION force_deadlock()`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return release
}

// WaitLockWaiters blocks until n backends of this database wait on a lock.
func WaitLockWaiters(t testing.TB, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d lock waiters", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// NewDB creates an empty database on the shared container and returns its URL.
// Without Docker it fails the test, unless GEDOUR_SKIP_DB_TESTS=1 turns that into a skip.
func NewDB(t testing.TB) string {
	t.Helper()
	failOnDeadlockRetry(t)
	once.Do(start)
	if startErr != nil {
		if os.Getenv("GEDOUR_SKIP_DB_TESTS") == "1" {
			t.Skipf("PostgreSQL container unavailable, skipped (GEDOUR_SKIP_DB_TESTS=1): %v", startErr)
		}
		t.Fatalf("PostgreSQL container unavailable (is Docker running? set GEDOUR_SKIP_DB_TESTS=1 to skip): %v", startErr)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// Terminate stops the shared container; call it from TestMain after m.Run.
func Terminate() {
	if ctr != nil {
		_ = ctr.Terminate(context.Background())
	}
}

// FlipEventsCascade recreates events_watch_id_fkey, as a pg_restore --jobs or a later migration may: its
// trigger then sorts after the rules one, so a watch delete cascades its rules before its events.
func FlipEventsCascade(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `ALTER TABLE events DROP CONSTRAINT events_watch_id_fkey,
		ADD CONSTRAINT events_watch_id_fkey FOREIGN KEY (watch_id) REFERENCES watches (id) ON DELETE CASCADE`)
	// PostgreSQL fires a table's triggers in name order: RI_ConstraintTrigger_a_<oid>, the oid unpadded.
	var order string
	if err == nil {
		err = pool.QueryRow(ctx, `SELECT string_agg(c.conrelid::regclass::text, ',' ORDER BY t.tgname)
			FROM pg_trigger t JOIN pg_constraint c ON c.oid = t.tgconstraint
			WHERE t.tgrelid = 'watches'::regclass AND c.conrelid IN ('rules'::regclass, 'events'::regclass) AND t.tgtype & 8 <> 0`).Scan(&order)
	}
	if err != nil || order != "rules,events" {
		t.Fatalf("watches delete cascade order %q, want rules,events: %v", order, err)
	}
	t.Logf("watches delete cascade order: %s", order)
}
