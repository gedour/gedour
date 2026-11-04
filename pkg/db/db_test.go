package db

import (
	"context"
	"io/fs"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	pool, err := Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	n, err := Migrate(ctx, pool)
	if err != nil || n == 0 {
		t.Fatalf("first run: applied=%d err=%v", n, err)
	}
	n, err = Migrate(ctx, pool)
	if err != nil || n != 0 {
		t.Fatalf("second run must be a no-op: applied=%d err=%v", n, err)
	}
}

// 003-AC1: the migration lets nextcloud_talk channels in; its down removes them, even one a rule uses.
func TestTalkMigration(t *testing.T) {
	ctx := context.Background()
	pool, err := Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO channels (owner_id, name, type, config) VALUES ((SELECT id FROM users), $1, $2, '{}') RETURNING id`
	if _, err := pool.Exec(ctx, `INSERT INTO users (oidc_sub, email) VALUES ('a', 'a@x.test')`); err != nil {
		t.Fatal(err)
	}
	var channel, rule string
	if err := pool.QueryRow(ctx, insert, "talk", "nextcloud_talk").Scan(&channel); err != nil {
		t.Fatalf("nextcloud_talk refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "pager", "pager"); err == nil {
		t.Fatal("an unknown type must stay refused")
	}
	if err := pool.QueryRow(ctx, `INSERT INTO rules (owner_id, name, filter) VALUES ((SELECT id FROM users), 'r', '') RETURNING id`).Scan(&rule); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, rule, channel); err != nil {
		t.Fatal(err)
	}
	sub, _ := fs.Sub(migrations, "migrations")
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 6); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "talk", "nextcloud_talk"); err == nil {
		t.Fatal("after the down, nextcloud_talk must be refused")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM channels`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after the down: %d channels, %v", n, err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// slowFS holds a migration that is not idempotent and slow enough that unlocked migrators overlap.
var slowFS = fstest.MapFS{
	"00001_slow.sql": {Data: []byte("-- +goose Up\nCREATE TABLE lock_probe (id int);\nSELECT pg_sleep(1);\n\n-- +goose Down\nDROP TABLE lock_probe;\n")},
}

func TestMigrateConcurrent(t *testing.T) {
	const migrators = 4
	ctx := context.Background()
	dbURL := testpg.NewDB(t)

	pools := make([]*pgxpool.Pool, migrators)
	for i := range pools {
		// Separate, already-connected pools: replicas never share connections, and the
		// connect latency must not serialise the start.
		pool, err := Connect(ctx, dbURL)
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		pools[i] = pool
	}

	start := make(chan struct{})
	applied := make([]int, migrators)
	errs := make([]error, migrators)
	var wg sync.WaitGroup
	for i := range migrators {
		wg.Go(func() {
			<-start
			applied[i], errs[i] = migrate(ctx, pools[i], slowFS)
		})
	}
	close(start)
	wg.Wait()

	total := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrator %d: %v", i, err)
		}
		total += applied[i]
	}
	if total != 1 {
		t.Fatalf("the migration must apply exactly once across %d migrators, got %v", migrators, applied)
	}
}

func TestConnectBadURL(t *testing.T) {
	if _, err := Connect(context.Background(), "postgres://u@h:notaport/db"); err == nil {
		t.Fatal("want error on unparsable URL")
	}
}

func TestMigrateUnreachable(t *testing.T) {
	ctx := context.Background()
	pool, err := Connect(ctx, "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := Migrate(ctx, pool); err == nil {
		t.Fatal("want error when PostgreSQL is unreachable")
	}
}
