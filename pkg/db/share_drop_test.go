package db_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

// 009-AC1, 009-AC2, 009-AC3, 009-AC4: migration 17 deletes what the shares gave bob and carol, keeps
// alice's data and the transfer leftovers, audits each share, drops the table; its down brings it back empty.
// dave's rule on the shared watch and bob's delivery from his all-watches rule tell the share-row selection
// from "any non-owner rule on a shared watch" and from "deliveries by rule".
func TestShareDropMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatalf("up to 16: %v", err)
	}
	for _, sql := range []string{
		`INSERT INTO users (oidc_sub, email, display_name) VALUES ('a', 'alice@x.test', 'Alice'), ('b', 'bob@x.test', 'Bob'), ('c', 'carol@x.test', ''), ('d', 'dave@x.test', 'Dave')`,
		`INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'w', 'oci_tags', '{}' FROM users WHERE oidc_sub = 'a'`,
		`INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'x', 'oci_tags', '{}' FROM users WHERE oidc_sub = 'a'`,
		`INSERT INTO watch_shares (watch_id, user_id, permission) SELECT w.id, u.id, CASE u.oidc_sub WHEN 'b' THEN 'rw' ELSE 'ro' END
			FROM watches w, users u WHERE w.name = 'w' AND u.oidc_sub IN ('b', 'c')`,
		`INSERT INTO events (watch_id, type, payload, dedup_key) SELECT id, 'tag.created', '{}', 'k' FROM watches WHERE name = 'w'`,
		`INSERT INTO channels (owner_id, name, type) SELECT id, oidc_sub || '-ch', 'telegram' FROM users WHERE oidc_sub IN ('a', 'b')`,
		`INSERT INTO rules (owner_id, name, watch_id) SELECT u.id, u.oidc_sub || '-pinned', w.id FROM users u, watches w
			WHERE u.oidc_sub IN ('a', 'b') AND w.name = 'w'`,
		`INSERT INTO rules (owner_id, name) SELECT id, 'b-all' FROM users WHERE oidc_sub = 'b'`,
		// Carol has no share on x: what an admin transfer leaves behind.
		`INSERT INTO rules (owner_id, name, watch_id) SELECT u.id, 'c-leftover', w.id FROM users u, watches w WHERE u.oidc_sub = 'c' AND w.name = 'x'`,
		// Dave has no share on w: a leftover on a watch shared with others.
		`INSERT INTO rules (owner_id, name, watch_id) SELECT u.id, 'd-leftover', w.id FROM users u, watches w WHERE u.oidc_sub = 'd' AND w.name = 'w'`,
		`INSERT INTO rule_channels (rule_id, channel_id) SELECT r.id, c.id FROM rules r JOIN channels c ON c.owner_id = r.owner_id
			WHERE r.name LIKE '%-pinned'`,
		`INSERT INTO deliveries (event_id, rule_id, channel_id) SELECT e.id, r.id, c.id FROM events e, rules r JOIN channels c ON c.owner_id = r.owner_id
			WHERE r.name LIKE '%-pinned'`,
		// After the pinned rows: only bob's all-watches rule, which stays, brings w's event to this channel.
		`INSERT INTO channels (owner_id, name, type) SELECT id, 'b-all-ch', 'telegram' FROM users WHERE oidc_sub = 'b'`,
		`INSERT INTO deliveries (event_id, rule_id, channel_id) SELECT e.id, r.id, c.id FROM events e, rules r, channels c
			WHERE r.name = 'b-all' AND c.name = 'b-all-ch'`,
		`INSERT INTO inbox_items (user_id, event_id) SELECT u.id, e.id FROM users u, events e WHERE u.oidc_sub IN ('a', 'b')`,
		// Last, so the cross joins above miss it: carol, a sharee of w, gets an event of her own watch.
		`INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'cw', 'oci_tags', '{}' FROM users WHERE oidc_sub = 'c'`,
		`INSERT INTO events (watch_id, type, payload, dedup_key) SELECT id, 'tag.created', '{}', 'k' FROM watches WHERE name = 'cw'`,
		`INSERT INTO channels (owner_id, name, type) SELECT id, 'c-own-ch', 'telegram' FROM users WHERE oidc_sub = 'c'`,
		`INSERT INTO rules (owner_id, name, watch_id) SELECT owner_id, 'c-own', id FROM watches WHERE name = 'cw'`,
		`INSERT INTO deliveries (event_id, rule_id, channel_id) SELECT e.id, r.id, c.id FROM rules r JOIN events e ON e.watch_id = r.watch_id, channels c
			WHERE r.name = 'c-own' AND c.name = 'c-own-ch'`,
		`INSERT INTO inbox_items (user_id, event_id) SELECT w.owner_id, e.id FROM watches w JOIN events e ON e.watch_id = w.id WHERE w.name = 'cw'`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, c := range []struct {
		name, query string
		want        int
	}{
		// 009-AC1: what the share gave bob goes.
		{"bob's pinned rule", `SELECT count(*) FROM rules WHERE name = 'b-pinned'`, 0},
		{"bob's rule channel", `SELECT count(*) FROM rule_channels rc JOIN channels c ON c.id = rc.channel_id WHERE c.name = 'b-ch'`, 0},
		{"bob's deliveries", `SELECT count(*) FROM deliveries d JOIN channels c ON c.id = d.channel_id WHERE c.name LIKE 'b-%'`, 0},
		{"bob's inbox item", `SELECT count(*) FROM inbox_items i JOIN users u ON u.id = i.user_id WHERE u.oidc_sub = 'b'`, 0},
		// 009-AC1: the rest stays.
		{"bob's all-watches rule", `SELECT count(*) FROM rules WHERE name = 'b-all' AND watch_id IS NULL`, 1},
		{"watch w", `SELECT count(*) FROM watches WHERE name = 'w'`, 1},
		{"events", `SELECT count(*) FROM events`, 2},
		{"alice's rule", `SELECT count(*) FROM rules WHERE name = 'a-pinned'`, 1},
		{"alice's rule channel", `SELECT count(*) FROM rule_channels rc JOIN channels c ON c.id = rc.channel_id WHERE c.name = 'a-ch'`, 1},
		{"alice's delivery", `SELECT count(*) FROM deliveries d JOIN channels c ON c.id = d.channel_id WHERE c.name = 'a-ch'`, 1},
		{"alice's inbox item", `SELECT count(*) FROM inbox_items i JOIN users u ON u.id = i.user_id WHERE u.oidc_sub = 'a'`, 1},
		// 009-AC1: a sharee loses only what came through the shared watch, not what her own watch brings.
		{"carol's own delivery", `SELECT count(*) FROM deliveries d JOIN channels c ON c.id = d.channel_id WHERE c.name = 'c-own-ch'`, 1},
		{"carol's own inbox item", `SELECT count(*) FROM inbox_items i JOIN users u ON u.id = i.user_id WHERE u.oidc_sub = 'c'`, 1},
		// 009-AC3: no share covers carol's or dave's rule.
		{"carol's transfer leftover", `SELECT count(*) FROM rules WHERE name = 'c-leftover'`, 1},
		{"dave's transfer leftover", `SELECT count(*) FROM rules WHERE name = 'd-leftover'`, 1},
		{"table", `SELECT count(*) FROM pg_class WHERE oid = to_regclass('watch_shares')`, 0},
		{"function", `SELECT count(*) FROM pg_proc WHERE proname = 'watch_shares_not_owner'`, 0},
	} {
		var got int
		if err := pool.QueryRow(ctx, c.query).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	assertShareAudit(t, pool)

	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatalf("down: %v", err)
	}
	// 009-AC4: empty, indexed, and the trigger still refuses a share to the owner.
	var rows, idx int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM watch_shares),
		(SELECT count(*) FROM pg_class WHERE oid = to_regclass('watch_shares_user_id_idx'))`).Scan(&rows, &idx); err != nil || rows != 0 || idx != 1 {
		t.Fatalf("after the down: %d rows, %d index, %v; want an empty indexed table", rows, idx, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO watch_shares (watch_id, user_id, permission) SELECT id, owner_id, 'ro' FROM watches WHERE name = 'w'`)
	if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("share to the owner after the down: %v; want check_violation", err)
	}
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertShareAudit(t, pool)
}

// assertShareAudit checks the two system rows of 009-AC2, which the down keeps and a second up does not repeat.
func assertShareAudit(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT a.target_type, a.target_id = w.id::text || ':' || u.id::text,
			a.actor_user_id IS NULL AND a.on_behalf_of IS NULL AND a.api_key_id IS NULL,
			a.before->>'user_id' = u.id::text, a.before->>'email', a.before->>'display_name', a.before->>'permission',
			a.before ? 'created_at'
		FROM audit_log a JOIN watches w ON w.name = 'w' JOIN users u ON u.email = a.before->>'email'
		WHERE a.action = 'watch_share.removed' ORDER BY u.email`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := [][3]string{{"bob@x.test", "Bob", "rw"}, {"carol@x.test", "", "ro"}}
	n := 0
	for ; rows.Next(); n++ {
		var targetType, email, name, perm string
		var target, system, user, created bool
		if err := rows.Scan(&targetType, &target, &system, &user, &email, &name, &perm, &created); err != nil {
			t.Fatal(err)
		}
		if n >= len(want) || targetType != "watch_share" || !target || !system || !user || !created ||
			[3]string{email, name, perm} != want[n] {
			t.Errorf("audit row %d: %s target %v system %v user %v created %v %s %q %s", n, targetType, target, system, user, created, email, name, perm)
		}
	}
	if err := rows.Err(); err != nil || n != len(want) {
		t.Fatalf("%d audit rows, %v; want %d", n, err, len(want))
	}
}
