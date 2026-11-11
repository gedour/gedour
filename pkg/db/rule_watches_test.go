package db_test

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

// ruleScopes maps each rule's name to its scope: "all", or its watch names sorted.
func ruleScopes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	var out map[string]string
	if err := pool.QueryRow(context.Background(), `SELECT jsonb_object_agg(r.name, CASE WHEN r.all_watches THEN 'all'
			ELSE COALESCE((SELECT string_agg(w.name, ',' ORDER BY w.name) FROM rule_watches rw JOIN watches w ON w.id = rw.watch_id WHERE rw.rule_id = r.id), '') END)
		FROM rules r`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 014-AC11: the up keeps every rule's scope; the down restores watch_id and refuses, naming them, rules on several
// watches or on none; a second up gives the same scopes back.
func TestRuleWatchesMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 25); err != nil {
		t.Fatalf("up to 25: %v", err)
	}
	for _, sql := range []string{
		`INSERT INTO users (oidc_sub, email) VALUES ('a', 'a@x.test')`,
		`INSERT INTO watches (owner_id, name, kind, config) SELECT id, w, 'oci_tags', '{}' FROM users, unnest(ARRAY['w1', 'w2']) w`,
		`INSERT INTO rules (owner_id, name, watch_id) SELECT u.id, 'one', w.id FROM users u, watches w WHERE w.name = 'w1'`,
		`INSERT INTO rules (owner_id, name, watch_id) SELECT u.id, 'two', w.id FROM users u, watches w WHERE w.name = 'w2'`,
		`INSERT INTO rules (owner_id, name) SELECT id, 'all' FROM users`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	want := map[string]string{"one": "w1", "two": "w2", "all": "all"}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := ruleScopes(t, pool); !maps.Equal(got, want) {
		t.Fatalf("after the up: %v, want %v", got, want)
	}
	var leftovers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'rules' AND column_name = 'watch_id'`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("rules.watch_id after the up: %d %v", leftovers, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rules (owner_id, name) SELECT id, 'no default' FROM users`); err == nil {
		t.Fatal("a rule without all_watches was accepted")
	}

	var several, none string
	if err := pool.QueryRow(ctx, `WITH r AS (INSERT INTO rules (owner_id, name, all_watches) SELECT id, 'several', false FROM users RETURNING id),
		rw AS (INSERT INTO rule_watches (rule_id, watch_id) SELECT r.id, w.id FROM r, watches w) SELECT id::text FROM r`).Scan(&several); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO rules (owner_id, name, all_watches) SELECT id, 'none', false FROM users RETURNING id::text`).Scan(&none); err != nil {
		t.Fatal(err)
	}
	_, err = provider.DownTo(ctx, 25)
	if err == nil {
		t.Fatal("down ran with a rule on several watches")
	}
	for _, s := range []string{several, "several", none, "none"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("down refusal %q does not name %s", err, s)
		}
	}
	for _, s := range []string{"one", "two", "all"} {
		if strings.Contains(err.Error(), "("+s+")") {
			t.Errorf("down refusal %q names rule %s, which it could restore", err, s)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM rules WHERE id IN ($1, $2)`, several, none); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 25); err != nil {
		t.Fatalf("down: %v", err)
	}
	var got []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(r.name || ':' || COALESCE(w.name, 'all') ORDER BY r.name) FROM rules r LEFT JOIN watches w ON w.id = r.watch_id`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"all:all", "one:w1", "two:w2"}) {
		t.Fatalf("after the down: %v", got)
	}
	var idx int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE oid = to_regclass('rules_watch_id_idx')`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("rules_watch_id_idx after the down: %d %v", idx, err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := ruleScopes(t, pool); !maps.Equal(got, want) {
		t.Fatalf("after the second up: %v, want %v", got, want)
	}
}
