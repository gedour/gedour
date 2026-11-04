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
