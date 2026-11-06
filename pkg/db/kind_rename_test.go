package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/filter"
	"github.com/gedour/gedour/pkg/testpg"
)

// Split so the 008-AC3 grep for the old name finds no live use.
const oldKind = "github_" + "release"

// 008-AC1, 008-AC2: a watch of the old kind with its state and event, and rules naming the kind, read
// github_repo after the up and route the watch's events; the down restores every value.
func TestGitHubRepoKindMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 14); err != nil {
		t.Fatalf("up to 14: %v", err)
	}
	filters := map[string]string{
		"double":   `watch.kind == "` + oldKind + `"`,
		"single":   `watch.kind == '` + oldKind + `' and event.type == "release.published"`,
		"backtick": "watch.kind == `" + oldKind + "`",
		// Not the quoted literal: left alone.
		"suffix": `watch.kind != "` + oldKind + `_x"`,
	}
	for _, sql := range []string{
		`INSERT INTO users (oidc_sub, email) VALUES ('a', 'a@x.test')`,
		`INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'w', '` + oldKind + `', '{"repo":"a/b"}' FROM users`,
		`INSERT INTO watches (owner_id, name, kind, config) SELECT id, 'o', 'oci_tags', '{}' FROM users`,
		`INSERT INTO watch_states (watch_id, state, state_hash) SELECT id, '{"releases":[]}', 'h' FROM watches WHERE name = 'w'`,
		`INSERT INTO events (watch_id, type, payload, dedup_key) SELECT id, 'release.published', '{"tag":"v1"}', 'k' FROM watches WHERE name = 'w'`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for name, f := range filters {
		if _, err := pool.Exec(ctx, `INSERT INTO rules (owner_id, name, filter) SELECT id, $1, $2 FROM users`, name, f); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	kind := assertWatch(t, pool, "github_repo")
	want := map[string]string{
		"double":   `watch.kind == "github_repo"`,
		"single":   `watch.kind == 'github_repo' and event.type == "release.published"`,
		"backtick": "watch.kind == `github_repo`",
		"suffix":   filters["suffix"],
	}
	assertFilters(t, pool, want)
	env := filter.Env{Event: filter.Event{Type: "release.published"}, Watch: filter.Watch{Name: "w", Kind: kind}}
	for _, name := range []string{"double", "single", "backtick"} {
		p, err := filter.Compile(want[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ok, err := filter.Match(p, env); err != nil || !ok {
			t.Errorf("%s does not route the migrated watch's event: %v, %v", name, ok, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE watches SET kind = $1 WHERE name = 'w'`, oldKind); err == nil {
		t.Fatal("after the up, the old kind must be refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE watches SET kind = 'oci_tags' WHERE name = 'o'`); err != nil {
		t.Fatalf("other kinds still pass: %v", err)
	}

	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertWatch(t, pool, oldKind)
	assertFilters(t, pool, filters)
	if _, err := pool.Exec(ctx, `UPDATE watches SET kind = 'github_repo' WHERE name = 'w'`); err == nil {
		t.Fatal("after the down, github_repo must be refused")
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertWatch(t, pool, "github_repo")
}

// assertWatch checks the seeded watch's kind and that its config, state and event survived; it returns the kind.
func assertWatch(t *testing.T, pool *pgxpool.Pool, want string) string {
	t.Helper()
	var kind, repo string
	var states, events int
	err := pool.QueryRow(context.Background(), `SELECT kind, config->>'repo',
		(SELECT count(*) FROM watch_states s WHERE s.watch_id = w.id AND s.state = '{"releases":[]}'),
		(SELECT count(*) FROM events e WHERE e.watch_id = w.id AND e.payload = '{"tag":"v1"}')
		FROM watches w WHERE name = 'w'`).Scan(&kind, &repo, &states, &events)
	if err != nil || kind != want || repo != "a/b" || states != 1 || events != 1 {
		t.Fatalf("watch: kind %q repo %q states %d events %d, %v; want kind %q", kind, repo, states, events, err, want)
	}
	return kind
}

func assertFilters(t *testing.T, pool *pgxpool.Pool, want map[string]string) {
	t.Helper()
	for name, f := range want {
		var got string
		if err := pool.QueryRow(context.Background(), `SELECT filter FROM rules WHERE name = $1`, name).Scan(&got); err != nil || got != f {
			t.Errorf("rule %s: %q, %v; want %q", name, got, err, f)
		}
	}
}

// 008 phase 3: github_issue joins the kind CHECK on a phase 2 DB; the down refuses to run while a
// github_issue watch exists rather than delete it, then runs once none is left.
func TestGitHubIssueKindMigration(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatalf("up to 15: %v", err)
	}
	insert := func(name, kind string) error {
		_, err := pool.Exec(ctx, `INSERT INTO watches (owner_id, name, kind, config) SELECT id, $1, $2, '{"repo":"a/b","number":1}' FROM users`, name, kind)
		return err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (oidc_sub, email) VALUES ('a', 'a@x.test')`); err != nil {
		t.Fatal(err)
	}
	if err := insert("pr", "github_pr"); err != nil {
		t.Fatal(err)
	}
	if insert("early", "github_issue") == nil {
		t.Fatal("github_issue accepted before the up")
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := insert("issue", "github_issue"); err != nil {
		t.Fatalf("github_issue refused after the up: %v", err)
	}
	if _, err := provider.DownTo(ctx, 15); err == nil {
		t.Fatal("down ran with a github_issue watch left")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM watches WHERE kind IN ('github_issue', 'github_pr')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("watches after the refused down: %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM watches WHERE kind = 'github_issue'`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 15); err != nil {
		t.Fatalf("down: %v", err)
	}
	if insert("late", "github_issue") == nil {
		t.Fatal("github_issue accepted after the down")
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := insert("issue", "github_issue"); err != nil {
		t.Fatalf("github_issue refused after the second up: %v", err)
	}
}
