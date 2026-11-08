package db_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

// 012-AC19: on a populated DB, a rule whose digest channels share one schedule takes it (a webhook beside them
// stays immediate anyway). One whose digest channels disagree, that also has an immediate channel able to take a
// digest, or that has a digest channel with no usable at, stays immediate with a warning line; high priority and
// immediate-only rules stay immediate unlogged. Channel configs lose the keys and waiting digest deliveries keep
// their due time. The down gives a channel back the schedule all its normal rules share as digests, and leaves a
// working schema.
func TestRuleDigestMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 22); err != nil {
		t.Fatalf("up to 22: %v", err)
	}
	ids := map[string]string{}
	seed := func(key, sql string, args ...any) {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		ids[key] = id
	}
	seed("user", `INSERT INTO users (oidc_sub, email) VALUES ('a', 'a@x.test') RETURNING id::text`)
	for key, cfg := range map[string]string{
		"paris":  `{"chat_id": "-1", "delivery": "digest", "at": ["09:00", "18:00"], "tz": "Europe/Paris"}`,
		"paris2": `{"to": ["a@x.test"], "delivery": "digest", "at": ["09:00", "18:00"], "tz": "Europe/Paris"}`,
		"utc":    `{"chat_id": "-2", "delivery": "digest", "at": ["07:00"], "tz": "UTC"}`,
		"plain":  `{"chat_id": "-3", "delivery": "immediate"}`,
		"utc2":   `{"chat_id": "-4", "delivery": "digest", "at": ["07:00"], "tz": "UTC"}`,
		// Written outside the API: a non-array at, and an empty one.
		"badat":   `{"chat_id": "-5", "delivery": "digest", "at": "09:00"}`,
		"emptyat": `{"chat_id": "-6", "delivery": "digest", "at": []}`,
	} {
		typ := "telegram"
		if key == "paris2" {
			typ = "email"
		}
		seed(key, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, $2, $3, $4::jsonb) RETURNING id::text`, ids["user"], key, typ, cfg)
	}
	seed("hook", `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, 'hook', 'webhook', '{"url": "https://hooks.example.com/x"}') RETURNING id::text`, ids["user"])
	rules := map[string][]string{
		"shared":   {"paris", "paris2", "hook", "plain"},
		"all":      {"paris", "paris2", "hook"},
		"solo":     {"utc2"},
		"bad":      {"badat"},
		"empty":    {"emptyat"},
		"mixed":    {"paris", "utc"},
		"one":      {"utc"},
		"none":     {"plain", "hook"},
		"high":     {"paris"},
		"no-chans": {},
	}
	for name, chans := range rules {
		priority := "normal"
		if name == "high" {
			priority = "high"
		}
		seed(name, `INSERT INTO rules (owner_id, name, priority) VALUES ($1, $2, $3) RETURNING id::text`, ids["user"], name, priority)
		for _, c := range chans {
			if _, err := pool.Exec(ctx, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, ids[name], ids[c]); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("watch", `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'w', 'oci_tags', '{}') RETURNING id::text`, ids["user"])
	due := time.Date(2030, 1, 2, 8, 0, 0, 0, time.UTC)
	seed("delivery", `WITH e AS (INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'tag.created', '{}', 'k') RETURNING id)
		INSERT INTO deliveries (event_id, rule_id, channel_id, next_attempt_at, digest) SELECT id, $2, $3, $4, true FROM e RETURNING id::text`,
		ids["watch"], ids["mixed"], ids["utc"], due)

	core, logs := observer.New(zap.WarnLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	_, err = db.Migrate(ctx, pool)
	restore()
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	want := map[string]string{
		"shared":   "immediate  ",
		"all":      "digest 09:00,18:00 Europe/Paris",
		"solo":     "digest 07:00 UTC",
		"bad":      "immediate  ",
		"empty":    "immediate  ",
		"mixed":    "immediate  ",
		"one":      "digest 07:00 UTC",
		"none":     "immediate  ",
		"high":     "immediate  ",
		"no-chans": "immediate  ",
	}
	for name, w := range want {
		if got := ruleDelivery(t, pool, ids[name]); got != w {
			t.Errorf("rule %s: %q, want %q", name, got, w)
		}
	}
	warned := map[string]int{}
	for _, l := range logs.FilterMessage("migration_warning").All() {
		for _, name := range []string{"mixed", "shared", "bad", "empty"} {
			if strings.Contains(l.ContextMap()["message"].(string), ids[name]) {
				warned[name]++
			}
		}
	}
	if n := logs.FilterMessage("migration_warning").Len(); n != 4 || len(warned) != 4 {
		t.Errorf("%d warnings %v, want one each naming rules mixed, shared, bad and empty", n, warned)
	}
	if n := count(t, pool, `SELECT count(*) FROM channels WHERE config ?| ARRAY['delivery', 'at', 'tz']`); n != 0 {
		t.Errorf("%d channels keep delivery keys", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM channels WHERE config ? 'chat_id' OR config ? 'to' OR config ? 'url'`); n != 8 {
		t.Errorf("%d channels kept their own keys, want 8", n)
	}
	var got time.Time
	var digest bool
	if err := pool.QueryRow(ctx, `SELECT next_attempt_at, digest FROM deliveries WHERE id = $1`, ids["delivery"]).Scan(&got, &digest); err != nil ||
		!got.Equal(due) || !digest {
		t.Errorf("waiting delivery due %s, digest %v, %v; want %s in the digest", got, digest, err, due)
	}
	if _, err := pool.Exec(ctx, `UPDATE rules SET delivery = 'weekly' WHERE id = $1`, ids["one"]); err == nil {
		t.Error("an unknown delivery must be refused")
	}

	if _, err := provider.DownTo(ctx, 22); err != nil {
		t.Fatalf("down: %v", err)
	}
	for key, w := range map[string]string{
		// Rule shared, immediate since the up, also targets paris and paris2; mixed, utc; none, plain.
		"paris":  `{"chat_id": "-1"}`,
		"paris2": `{"to": ["a@x.test"]}`,
		"utc":    `{"chat_id": "-2"}`,
		"utc2":   `{"at": ["07:00"], "tz": "UTC", "chat_id": "-4", "delivery": "digest"}`,
		"plain":  `{"chat_id": "-3"}`,
		"hook":   `{"url": "https://hooks.example.com/x"}`,
	} {
		var cfg string
		if err := pool.QueryRow(ctx, `SELECT config::text FROM channels WHERE id = $1`, ids[key]).Scan(&cfg); err != nil || cfg != w {
			t.Errorf("channel %s after the down: %s, %v; want %s", key, cfg, err, w)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rules (owner_id, name) VALUES ($1, 'after-down')`, ids["user"]); err != nil {
		t.Fatalf("insert after the down: %v", err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	// Only utc2 came back a digest.
	for name, w := range map[string]string{"solo": "digest 07:00 UTC", "all": "immediate  ", "one": "immediate  "} {
		if got := ruleDelivery(t, pool, ids[name]); got != w {
			t.Errorf("rule %s after up again: %q, want %q", name, got, w)
		}
	}
}

// 012-AC19: a fresh install logs no migration warning: the NOTICEs of IF EXISTS drops are not forwarded.
func TestMigrateFreshNoWarning(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	core, logs := observer.New(zap.DebugLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	_, err = db.Migrate(ctx, pool)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	if n := logs.FilterMessage("migration_warning").Len(); n != 0 {
		t.Errorf("%d migration warnings on a fresh install: %v", n, logs.All())
	}
}

func ruleDelivery(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT delivery || ' ' || COALESCE(array_to_string(digest_at, ','), '') || ' ' || COALESCE(digest_tz, '')
		FROM rules WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func count(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
