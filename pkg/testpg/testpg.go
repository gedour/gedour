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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
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

// NewDB creates an empty database on the shared container and returns its URL.
// Without Docker it fails the test, unless GEDOUR_SKIP_DB_TESTS=1 turns that into a skip.
func NewDB(t testing.TB) string {
	t.Helper()
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
