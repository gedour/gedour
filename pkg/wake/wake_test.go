package wake

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

const wait = 5 * time.Second

var tables = []string{"watches", "events", "deliveries"}

// sentinel is a notification sent after the statement under test: notifications arrive in commit order, so once
// it is seen every notification of the statement has been handled.
const sentinel = "sentinel"

func newDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	url := testpg.NewDB(t)
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return url, pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// start runs a Listener with one subscriber per table and returns once its first wake-all is drained.
func start(t *testing.T, l *Listener) map[string]<-chan struct{} {
	t.Helper()
	subs := map[string]<-chan struct{}{}
	for _, table := range append([]string{sentinel}, tables...) {
		subs[table] = l.Subscribe(table)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for table, ch := range subs {
		waitWake(t, ch, table+" initial wake-all")
	}
	return subs
}

func waitWake(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(wait):
		t.Fatalf("no wake: %s", what)
	}
}

func waitLog(t *testing.T, logs *observer.ObservedLogs, msg string, n int) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for logs.FilterMessage(msg).Len() < n {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d %s logs", n, msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// count returns the gedour_work notifications conn receives, by payload, up to the sentinel.
func count(t *testing.T, conn *pgx.Conn) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	got := map[string]int{}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("wait for the sentinel: %v (got %v)", err, got)
		}
		if n.Payload == sentinel {
			return got
		}
		got[n.Payload]++
	}
}

// 015-AC11: a statement notifies its table once, none for an unrelated column or a rolled-back write; a
// subscriber is woken by its own table only.
func TestNotifyPerStatement(t *testing.T) {
	ctx := context.Background()
	url, pool := newDB(t)
	exec(t, pool, `INSERT INTO users (oidc_sub, email) VALUES ('u', 'u@x.test')`)
	exec(t, pool, `INSERT INTO channels (owner_id, name, type, config) SELECT id, 'c', 'webhook', '{}' FROM users`)
	exec(t, pool, `INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'seed', 'github_repo', '{}' FROM users`)
	exec(t, pool, `INSERT INTO events (watch_id, type, payload, dedup_key) SELECT id, 't', '{}', 'seed' FROM watches`)

	raw, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	if _, err := raw.Exec(ctx, "LISTEN "+Channel); err != nil {
		t.Fatal(err)
	}
	subs := start(t, NewListener(pool, zap.NewNop()))

	for _, tc := range []struct {
		name     string
		sql      string
		rollback bool
		want     map[string]int
	}{
		{name: "watches multi-row insert", sql: `INSERT INTO watches (owner_id, name, kind, config)
			SELECT u.id, 'w' || g, 'github_repo', '{}' FROM users u, generate_series(1, 3) g`, want: map[string]int{"watches": 1}},
		{name: "watches multi-row next_run_at", sql: `UPDATE watches SET next_run_at = now()`, want: map[string]int{"watches": 1}},
		{name: "watches paused", sql: `UPDATE watches SET paused = true`, want: map[string]int{"watches": 1}},
		{name: "watches name only", sql: `UPDATE watches SET name = name || 'x'`, want: map[string]int{}},
		{name: "watches next_run_at rolled back", sql: `UPDATE watches SET next_run_at = now() + interval '1 hour'`, rollback: true, want: map[string]int{}},
		{name: "events multi-row insert", sql: `INSERT INTO events (watch_id, type, payload, dedup_key)
			SELECT id, 't', '{}', id::text FROM watches`, want: map[string]int{"events": 1}},
		{name: "deliveries multi-row insert", sql: `INSERT INTO deliveries (event_id, channel_id)
			SELECT e.id, c.id FROM events e, channels c`, want: map[string]int{"deliveries": 1}},
		{name: "deliveries next_attempt_at", sql: `UPDATE deliveries SET next_attempt_at = now()`, want: map[string]int{"deliveries": 1}},
		{name: "deliveries status", sql: `UPDATE deliveries SET status = 'failed'`, want: map[string]int{"deliveries": 1}},
		{name: "deliveries last_error only", sql: `UPDATE deliveries SET last_error = 'boom'`, want: map[string]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rollback {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, tc.sql); err != nil {
					t.Fatal(err)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				exec(t, pool, tc.sql)
			}
			exec(t, pool, `SELECT pg_notify('`+Channel+`', '`+sentinel+`')`)

			got := count(t, raw)
			if len(got) != len(tc.want) {
				t.Errorf("notifications %v, want %v", got, tc.want)
			}
			for table, n := range tc.want {
				if got[table] != n {
					t.Errorf("notifications %v, want %v", got, tc.want)
				}
			}
			waitWake(t, subs[sentinel], sentinel)
			for _, table := range tables {
				select {
				case <-subs[table]:
					if tc.want[table] == 0 {
						t.Errorf("%s subscriber woken", table)
					}
				default:
					if tc.want[table] != 0 {
						t.Errorf("%s subscriber not woken", table)
					}
				}
			}
		})
	}
}

// A killed LISTEN backend is logged, reconnected, wakes every subscriber, and later notifications still arrive.
func TestReconnect(t *testing.T) {
	_, pool := newDB(t)
	core, logs := observer.New(zap.InfoLevel)
	l := NewListener(pool, zap.New(core))
	l.backoffBase, l.backoffCap = 10*time.Millisecond, 50*time.Millisecond
	subs := start(t, l)
	waitLog(t, logs, "work_listening", 1)

	// The listener runs on a database of its own: the only session there whose last query is the LISTEN.
	var killed bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND query = 'LISTEN `+Channel+`'`).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the listen backend: %v, %v", killed, err)
	}
	waitLog(t, logs, "work_listen_failed", 1)
	if e := logs.FilterMessage("work_listen_failed").All()[0]; e.ContextMap()["error"] == nil {
		t.Errorf("work_listen_failed without error: %v", e.ContextMap())
	}
	for _, table := range append([]string{sentinel}, tables...) {
		waitWake(t, subs[table], table+" wake-all after reconnect")
	}
	waitLog(t, logs, "work_listening", 2)

	exec(t, pool, `INSERT INTO users (oidc_sub, email) VALUES ('u', 'u@x.test')`)
	exec(t, pool, `INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'w', 'github_repo', '{}' FROM users`)
	waitWake(t, subs["watches"], "watches after reconnect")
}

// The triggers are statement level: PostgreSQL folds identical notifications of a transaction, so a row-level
// trigger would notify the same and only cost more. The down removes them and the function; the up applies again.
func TestMigration(t *testing.T) {
	ctx := context.Background()
	_, pool := newDB(t)
	var statement int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname IN
		('watches_work_notify', 'events_work_notify', 'deliveries_work_notify') AND tgtype & 1 = 0`).Scan(&statement); err != nil || statement != 3 {
		t.Fatalf("statement-level work triggers: %d, want 3 (%v)", statement, err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("../db/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 24); err != nil {
		t.Fatalf("down to 24: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_trigger WHERE tgname LIKE '%_work_notify')
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'gedour_work_notify')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("after the down: %d triggers or functions left, %v", left, err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
