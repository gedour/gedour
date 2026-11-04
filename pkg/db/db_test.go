package db

import (
	"context"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

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
