package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

// 007-AC12: the down turns a suspension of a live key into a revocation at the suspension date, so older code never
// sees it working, even when its owner signed in since: a later refusal may have kept the first date. A key already
// dead or never suspended keeps its row as it was.
func TestAPIKeySuspendedMigrationDown(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 24); err != nil {
		t.Fatalf("up to 24: %v", err)
	}
	users := map[string]string{}
	// a signed in before the suspensions, b signed in after its own.
	for sub, login := range map[string]string{"a": "now() - interval '3 hours'", "b": "now() - interval '1 hour'"} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, last_login_at) VALUES ($1, $1 || '@x.test', `+login+`) RETURNING id::text`, sub).Scan(&id); err != nil {
			t.Fatal(err)
		}
		users[sub] = id
	}
	ids := map[string]string{}
	for name, set := range map[string]string{
		"suspended": `suspended_at = now() - interval '2 hours'`,
		"expired":   `suspended_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`,
		"revoked":   `suspended_at = now() - interval '2 hours', revoked_at = now() - interval '3 days'`,
		"plain":     `last_used_at = NULL`,
		"signed in": `suspended_at = now() - interval '2 hours'`,
	} {
		owner := users["a"]
		if name == "signed in" {
			owner = users["b"]
		}
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO api_keys (user_id, name, hash, prefix, scope) VALUES ($1, $2, $3, 'abcdefgh', 'read') RETURNING id::text`,
			owner, name, []byte(name)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET `+set+` WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	if _, err := provider.DownTo(ctx, 23); err != nil {
		t.Fatalf("down: %v", err)
	}
	for name, where := range map[string]string{
		"suspended": `revoked_at BETWEEN now() - interval '121 minutes' AND now() - interval '119 minutes'`,
		"expired":   `revoked_at IS NULL`,
		"revoked":   `revoked_at < now() - interval '2 days'`,
		"plain":     `revoked_at IS NULL`,
		"signed in": `revoked_at BETWEEN now() - interval '121 minutes' AND now() - interval '119 minutes'`,
	} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT `+where+` FROM api_keys WHERE id = $1`, ids[name]).Scan(&ok); err != nil || !ok {
			t.Errorf("%s after the down: want %s (%v)", name, where, err)
		}
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
