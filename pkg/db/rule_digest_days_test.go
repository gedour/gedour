package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

// 018-AC10: on a populated DB, every digest rule gets every day on each of its times, an immediate rule none, and a
// waiting digest delivery keeps its due time. The check refuses a mask per time that does not fit; the down refuses
// a rule not on every day, and then leaves a working schema.
func TestRuleDigestDaysMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("up to 27: %v", err)
	}
	var user, two, one, plain, watch, ch, delivery string
	if err := pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, email) VALUES ('a', 'a@x.test') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		dst *string
		sql string
	}{
		{&two, `INSERT INTO rules (owner_id, name, all_watches, delivery, digest_at, digest_tz) VALUES ($1, 'two', true, 'digest', '{09:00,18:00}', 'Europe/Paris') RETURNING id::text`},
		{&one, `INSERT INTO rules (owner_id, name, all_watches, delivery, digest_at, digest_tz) VALUES ($1, 'one', true, 'digest', '{07:00}', 'UTC') RETURNING id::text`},
		{&plain, `INSERT INTO rules (owner_id, name, all_watches) VALUES ($1, 'plain', true) RETURNING id::text`},
		{&watch, `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'w', 'oci_tags', '{}') RETURNING id::text`},
		{&ch, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, 'tg', 'telegram', '{"chat_id": "-1"}') RETURNING id::text`},
	} {
		if err := pool.QueryRow(ctx, s.sql, user).Scan(s.dst); err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
	}
	due := time.Date(2030, 1, 2, 7, 0, 0, 0, time.UTC)
	if err := pool.QueryRow(ctx, `WITH e AS (INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'tag.created', '{}', 'k') RETURNING id)
		INSERT INTO deliveries (event_id, rule_id, channel_id, next_attempt_at, digest) SELECT id, $2, $3, $4, true FROM e RETURNING id::text`,
		watch, one, ch, due).Scan(&delivery); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	for id, want := range map[string]string{two: "{127,127}", one: "{127}", plain: "NULL"} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(digest_days::text, 'NULL') FROM rules WHERE id = $1`, id).Scan(&got); err != nil || got != want {
			t.Errorf("rule %s digest_days %s, %v; want %s", id, got, err, want)
		}
	}
	var got time.Time
	if err := pool.QueryRow(ctx, `SELECT next_attempt_at FROM deliveries WHERE id = $1`, delivery).Scan(&got); err != nil || !got.Equal(due) {
		t.Errorf("waiting delivery due %s, %v; want %s", got, err, due)
	}
	for name, days := range map[string]string{"short": "{127}", "long": "{127,127,127}", "no day": "{0,127}", "bit 7": "{128,127}", "null mask": "{NULL,127}"} {
		if _, err := pool.Exec(ctx, `UPDATE rules SET digest_days = $2::smallint[] WHERE id = $1`, two, days); err == nil {
			t.Errorf("digest_days %s (%s) for two times must be refused", days, name)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE rules SET digest_days = '{31,127}' WHERE id = $1`, two); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 27); err == nil {
		t.Fatal("down with a rule not on every day must be refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE rules SET digest_days = '{127,127}' WHERE id = $1`, two); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 27); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rules (owner_id, name, all_watches, delivery, digest_at, digest_tz) VALUES ($1, 'after-down', true, 'digest', '{08:00}', 'UTC')`, user); err != nil {
		t.Fatalf("insert after the down: %v", err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM rules WHERE delivery = 'digest' AND digest_days IS NULL`); n != 0 {
		t.Errorf("%d digest rules without days after up again", n)
	}
}
