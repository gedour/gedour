package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/netguard"
)

// 008-AC4: with both boxes off the stored state is byte for byte the one develop stored.
func TestBoxesOffStateIsDevelopShape(t *testing.T) {
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	f := &collect.Fetcher{Client: g.Client(netguard.Options{Timeout: 5 * time.Second, HTTPHosts: []string{"127.0.0.1"}}),
		UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}}
	cfg := fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"include_prereleases":true,"new_issues":false,"new_prs":false}`, srv.URL)
	state, err := collect.Collectors()["github_repo"].Collect(t.Context(), f, json.RawMessage(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(developReleaseStateA)); err != nil {
		t.Fatal(err)
	}
	if string(state) != want.String() {
		t.Fatalf("state\n got  %s\n want %s", state, want.String())
	}
}

// itemsUpstream serves releases and issues from two fixture sets switched apart; failIssues answers 500 on the issues list.
type itemsUpstream struct {
	releases, issues *testupstream.Server
	failIssues       atomic.Bool
	inm              []string
}

func (u *itemsUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/issues") {
		if u.failIssues.Load() {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		u.issues.ServeHTTP(w, r)
		return
	}
	u.inm = append(u.inm, r.Header.Get("If-None-Match"))
	u.releases.ServeHTTP(w, r)
}

func (e *env) events(t *testing.T, id string) []string {
	t.Helper()
	rows, _ := e.pool.Query(context.Background(), `SELECT type || ' ' || coalesce(payload->>'tag', payload->>'number') FROM events WHERE watch_id = $1 ORDER BY id`, id)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 008-AC5, 008-AC6: through the worker, a box on: the baseline is quiet, issues still diff while the releases are
// unchanged (the run that a 304 would have ended), then releases diff while the issues are unchanged.
func TestRepoItemsThroughWorker(t *testing.T) {
	e := newEnv(t)
	up := &itemsUpstream{releases: testupstream.New(), issues: testupstream.New()}
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_repo", fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"include_prereleases":true,"new_issues":true,"new_prs":true}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	for k, sets := range [][2]string{{"a", "a"}, {"a", "b"}, {"b", "b"}} {
		up.releases.SetFixture(sets[0])
		up.issues.SetFixture(sets[1])
		e.clock.set(t0.Add(time.Duration(k) * time.Minute))
		tick(t, w)
		if r := e.row(t, id); *r.status != "ok" {
			t.Fatalf("run %d: %s %v", k, *r.status, *r.lastErr)
		}
		if k == 0 && len(e.events(t, id)) != 0 {
			t.Fatalf("baseline: %v", e.events(t, id))
		}
	}
	if strings.Join(up.inm, "") != "" {
		t.Fatalf("If-None-Match sent: %q", up.inm)
	}
	want := "issue.opened 9513,pr.opened 9514,release.promoted v2.104.0-rc.1,release.published v2.103.3,release.published v2.105.0"
	if got := strings.Join(e.events(t, id), ","); got != want {
		t.Fatalf("events\n got  %s\n want %s", got, want)
	}
	var labels, author string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload->>'labels', payload->>'author' FROM events WHERE type = 'issue.opened' AND watch_id = $1`, id).Scan(&labels, &author); err != nil {
		t.Fatal(err)
	}
	if labels != `["bug", "enhancement"]` || author != "alice-q" {
		t.Fatalf("payload labels %s author %s", labels, author)
	}
}

// 008-AC8: a failing issues call fails the run like a failing releases call: no state, no events, on the
// baseline run and on a later one.
func TestRepoItemsCallFails(t *testing.T) {
	e := newEnv(t)
	up := &itemsUpstream{releases: testupstream.New(), issues: testupstream.New()}
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_repo", fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"new_issues":true}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	run := func(k int, fail bool, status string, states int) {
		t.Helper()
		up.failIssues.Store(fail)
		e.clock.set(t0.Add(time.Duration(k) * time.Hour))
		tick(t, w)
		r := e.row(t, id)
		if *r.status != status || (fail && (r.lastErr == nil || !strings.Contains(*r.lastErr, "/issues?") || !strings.Contains(*r.lastErr, "HTTP 500"))) {
			t.Fatalf("run %d: %s %v", k, *r.status, r.lastErr)
		}
		if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != states {
			t.Fatalf("run %d: %d states, want %d", k, n, states)
		}
		if ev := e.events(t, id); len(ev) != 0 {
			t.Fatalf("run %d: events %v", k, ev)
		}
	}
	run(0, true, "error", 0)
	run(1, false, "ok", 1)
	up.releases.SetFixture("b")
	up.issues.SetFixture("b")
	run(2, true, "error", 1)
}

// 011-AC2, 011-AC3, 011-AC6: through the worker, the discussions box alone: the baseline is quiet, fixture B's new
// discussion is one event, and a 410 on the list fails the run with no state and no event, naming the box.
func TestRepoDiscussionsThroughWorker(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	var gone atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() && strings.HasSuffix(r.URL.Path, "/discussions") {
			http.Error(w, `{"message":"Discussions are disabled for this repo"}`, http.StatusGone)
			return
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	id := e.addWatch(t, "github_repo", fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"new_discussions":true}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	run := func(k int, set string, fail bool) {
		t.Helper()
		up.SetFixture(set)
		gone.Store(fail)
		e.clock.set(t0.Add(time.Duration(k) * time.Hour))
		tick(t, w)
		r := e.row(t, id)
		if fail != (*r.status == "error") || (fail && (r.lastErr == nil || !strings.Contains(*r.lastErr, `uncheck "new discussions"`))) {
			t.Fatalf("run %d: %s %v", k, *r.status, r.lastErr)
		}
	}
	run(0, "a", false)
	if ev := e.events(t, id); len(ev) != 0 {
		t.Fatalf("baseline: %v", ev)
	}
	run(1, "b", true)
	if n, ev := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id), e.events(t, id); n != 1 || len(ev) != 0 {
		t.Fatalf("410: %d states, events %v", n, ev)
	}
	run(2, "b", false)
	want := "release.published v2.104.0-rc.1,release.published v2.103.3,release.published v2.105.0,discussion.opened 9515"
	if got := strings.Join(e.events(t, id), ","); got != want {
		t.Fatalf("events\n got  %s\n want %s", got, want)
	}
	var category string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload->>'category' FROM events WHERE type = 'discussion.opened' AND watch_id = $1`, id).Scan(&category); err != nil || category != "Ideas" {
		t.Fatalf("category %q %v", category, err)
	}
}

// pageUpstream serves releases from fixture A and the issues list from page; a nil page is fixture A's.
type pageUpstream struct {
	up   *testupstream.Server
	mu   sync.Mutex
	page []byte
}

func (u *pageUpstream) set(b []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.page = b
}

func (u *pageUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	page := u.page
	u.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/issues") && page != nil {
		_, _ = w.Write(page)
		return
	}
	u.up.ServeHTTP(w, r)
}

// withItem is fixture A's issues list with items on top.
func withItem(t *testing.T, items ...string) []byte {
	t.Helper()
	var list []json.RawMessage
	if err := json.Unmarshal(testupstream.Fixture("github_issues", "a"), &list); err != nil {
		t.Fatal(err)
	}
	var top []json.RawMessage
	for _, it := range items {
		top = append(top, json.RawMessage(it))
	}
	b, err := json.Marshal(append(top, list...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 008-AC5: a NUL in upstream text of a new item (jsonb refuses \u0000) is dropped, not a failed run.
func TestRepoItemNULDropped(t *testing.T) {
	for _, tt := range []struct{ name, item, field, want string }{
		{"title", `{"number":9600,"title":"a\u0000b","user":{"login":"x"},"labels":[],"html_url":"https://x/9600"}`, "title", "ab"},
		{"label", `{"number":9600,"title":"t","user":{"login":"x"},"labels":[{"name":"b\u0000ug"}],"html_url":"https://x/9600"}`, "labels", `["bug"]`},
		{"author", `{"number":9600,"title":"t","user":{"login":"x\u0000y"},"labels":[],"html_url":"https://x/9600"}`, "author", "xy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			up := &pageUpstream{up: testupstream.New()}
			srv := httptest.NewServer(up)
			defer srv.Close()
			id := e.addWatch(t, "github_repo", fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"new_issues":true}`, srv.URL), "1m")
			w := e.worker(t, e.pool, collect.Collectors())
			for k, page := range [][]byte{nil, withItem(t, tt.item)} {
				up.set(page)
				e.clock.set(t0.Add(time.Duration(k) * time.Minute))
				tick(t, w)
				if r := e.row(t, id); *r.status != "ok" {
					t.Fatalf("run %d: %s %v", k, *r.status, *r.lastErr)
				}
			}
			var got string
			if err := e.pool.QueryRow(context.Background(), `SELECT payload->>$2 FROM events WHERE watch_id = $1 AND type = 'issue.opened'`, id, tt.field).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tt.want && strings.ReplaceAll(got, " ", "") != tt.want {
				t.Fatalf("%s %q", tt.field, got)
			}
		})
	}
}

// 008-AC6: a box toggle keeps the states. Checked: the next run is the items' baseline; unchecked, a run stores a
// state without an issue list (the edit dropped the ETags, so no 304 keeps the old one); checked again: a new baseline.
func TestRepoBoxToggleBaselines(t *testing.T) {
	e := newEnv(t)
	up := &pageUpstream{up: testupstream.New()}
	srv := httptest.NewServer(up)
	defer srv.Close()
	off := fmt.Sprintf(`{"repo":"cli/cli","api_url":%q}`, srv.URL)
	on := fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"new_issues":true}`, srv.URL)
	id := e.addWatch(t, "github_repo", off, "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	k := 0
	run := func(page []byte) {
		t.Helper()
		up.set(page)
		k++
		e.clock.set(t0.Add(time.Duration(k) * time.Hour))
		if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = $2 WHERE id = $1`, id, e.clock.now()); err != nil {
			t.Fatal(err)
		}
		tick(t, w)
		if r := e.row(t, id); *r.status != "ok" {
			t.Fatalf("run %d: %s %v", k, *r.status, *r.lastErr)
		}
	}
	// What updateWatch does on a box toggle (pkg/server TestRetargetResetsBaseline checks it does).
	toggle := func(cfg string) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET config = $2 WHERE id = $1`, id, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(context.Background(), `DELETE FROM http_cache WHERE watch_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	run(nil)
	toggle(on)
	run(withItem(t, `{"number":9601,"title":"t","user":{"login":"x"},"labels":[]}`))
	run(withItem(t, `{"number":9602,"title":"t","user":{"login":"x"},"labels":[]}`))
	toggle(off)
	run(nil)
	toggle(on)
	run(withItem(t, `{"number":9603,"title":"t","user":{"login":"x"},"labels":[]}`))
	// new_prs added while new_issues stays on: the new issue is reported, the PR list starts its baseline.
	toggle(fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"new_issues":true,"new_prs":true}`, srv.URL))
	run(withItem(t, `{"number":9605,"title":"t","user":{"login":"x"},"labels":[]}`,
		`{"number":9604,"title":"t","user":{"login":"x"},"labels":[],"pull_request":{}}`,
		`{"number":9603,"title":"t","user":{"login":"x"},"labels":[]}`))
	if got := strings.Join(e.events(t, id), ","); got != "issue.opened 9602,issue.opened 9605" {
		t.Fatalf("events %s", got)
	}
	if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1 AND state ? 'issues'`, id); n != 4 {
		t.Fatalf("states with an issue list: %d", n)
	}
}

// failComments wraps the fake upstream: fail answers 500 on an issue's comments.
type failComments struct {
	up   *testupstream.Server
	fail atomic.Bool
}

func (u *failComments) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u.fail.Load() && strings.HasSuffix(r.URL.Path, "/comments") {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		return
	}
	u.up.ServeHTTP(w, r)
}

// 008-AC9: a failing comments call fails a github_issue run: no state, no events, on the baseline and later.
func TestIssueCommentsCallFails(t *testing.T) {
	e := newEnv(t)
	up := &failComments{up: testupstream.New()}
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_issue", fmt.Sprintf(`{"repo":"cli/cli","number":%s,"api_url":%q}`, testupstream.IssueNumber, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	run := func(k int, fail bool, status string, states int) {
		t.Helper()
		up.fail.Store(fail)
		e.clock.set(t0.Add(time.Duration(k) * time.Hour))
		tick(t, w)
		r := e.row(t, id)
		if *r.status != status || (fail && (r.lastErr == nil || !strings.Contains(*r.lastErr, "/comments") || !strings.Contains(*r.lastErr, "HTTP 500"))) {
			t.Fatalf("run %d: %s %v", k, *r.status, r.lastErr)
		}
		if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != states {
			t.Fatalf("run %d: %d states, want %d", k, n, states)
		}
		if ev := e.events(t, id); len(ev) != 0 {
			t.Fatalf("run %d: events %v", k, ev)
		}
	}
	run(0, true, "error", 0)
	run(1, false, "ok", 1)
	up.up.SetFixture("b")
	run(2, true, "error", 1)
}

// 011-AC7: a github_discussion watch through the fake upstream: a quiet baseline, then fixture b's close,
// chosen answer and reply.
func TestDiscussionWatchFixtures(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_discussion", fmt.Sprintf(`{"repo":"cli/cli","number":9507,"api_url":%q}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	tick(t, w)
	if r := e.row(t, id); *r.status != "ok" || len(e.events(t, id)) != 0 {
		t.Fatalf("baseline: %s %v, events %v", *r.status, r.lastErr, e.events(t, id))
	}
	up.SetFixture("b")
	e.clock.set(t0.Add(time.Hour))
	tick(t, w)
	got := strings.Join(e.events(t, id), ",")
	if got != "discussion.state_changed 9507,discussion.answer_changed 9507,discussion.commented 9507" {
		t.Fatalf("events %s", got)
	}
	if n := e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1 AND payload->>'reply_to' = 'https://github.com/cli/cli/discussions/9507#discussioncomment-4100001'`, id); n != 1 {
		t.Fatalf("replies: %d", n)
	}
}

// 019-AC9, 019-AC3, 019-AC4: a github_schedules watch reports on its first run, unlike 001-AC8: fixture a's disabled
// workflow, then fixture b's re-enable. The worker hands the last state over, so b fetches the one changed file only.
func TestSchedulesWatchFirstRun(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	// A fixed upstream clock: on the wall clock, a tick crossing a second boundary dates the runs anew and stores a state.
	up.SetNow(func() time.Time { return t0 })
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_schedules", fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":0,"auto_enable":false}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	events := func() string {
		rows, _ := e.pool.Query(context.Background(), `SELECT type || ' ' || (payload->>'path') || ' ' || dedup_key FROM events WHERE watch_id = $1 ORDER BY id`, id)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	tick(t, w)
	var first int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM watch_states WHERE watch_id = $1`, id).Scan(&first); err != nil {
		t.Fatal(err)
	}
	// Keyed on the negated first state id: a later first run, after a retarget, cannot collide with it.
	m := ".github/workflows/mirror.yml"
	if got, want := events(), fmt.Sprintf("schedule.disabled %s schedule.disabled:%s:%d", m, m, -first); got != want {
		t.Fatalf("first run: %s, want %s", got, want)
	}
	e.clock.set(t0.Add(time.Hour))
	tick(t, w)
	if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != 1 {
		t.Fatalf("unchanged run stored a state: %d states", n)
	}
	up.SetFixture("b")
	e.clock.set(t0.Add(2 * time.Hour))
	tick(t, w)
	if got, want := events(), fmt.Sprintf("schedule.disabled %s schedule.disabled:%s:%d,schedule.enabled %s schedule.enabled:%s:%d", m, m, -first, m, m, first); got != want {
		t.Fatalf("events %s, want %s", got, want)
	}
	file := func(name string) int { return up.Hits("/repos/acme/widgets/contents/.github/workflows/" + name) }
	if file("mirror.yml") != 1 || file("yearly.yml") != 1 || file("lint.yml") != 1 || file("ci.yml") != 2 {
		t.Fatalf("file fetches mirror %d yearly %d lint %d ci %d", file("mirror.yml"), file("yearly.yml"), file("lint.yml"), file("ci.yml"))
	}
	// 019-AC13: warn_days 0 reads neither the repository nor its commits.
	if up.Hits("/repos/acme/widgets") != 0 || up.Hits("/repos/acme/widgets/commits") != 0 {
		t.Fatalf("warn calls: repo %d commits %d", up.Hits("/repos/acme/widgets"), up.Hits("/repos/acme/widgets/commits"))
	}
}

// 019-AC13: with warn_days 7, fixture a's public repository, last commit 54 days old, gets one schedule.disable_soon
// on the first run beside schedule.disabled; an unchanged run adds nothing, and fixture b's newer commit none.
func TestSchedulesWatchDisableSoon(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	up.SetNow(e.clock.now)
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_schedules", fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":7,"auto_enable":false}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	events := func() string {
		rows, _ := e.pool.Query(context.Background(), `SELECT type || ' ' || coalesce(payload->>'path', payload->>'repo') FROM events WHERE watch_id = $1 ORDER BY id`, id)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	const want = "schedule.disabled .github/workflows/mirror.yml,schedule.disable_soon acme/widgets"
	tick(t, w)
	if got := events(); got != want {
		t.Fatalf("first run: %s", got)
	}
	var disableAt, lastActivity time.Time
	var workflows string
	if err := e.pool.QueryRow(context.Background(), `SELECT (payload->>'disable_at')::timestamptz, (payload->>'last_activity_at')::timestamptz, payload->>'workflows'
		FROM events WHERE watch_id = $1 AND type = 'schedule.disable_soon'`, id).Scan(&disableAt, &lastActivity, &workflows); err != nil {
		t.Fatal(err)
	}
	if !lastActivity.Equal(t0.Add(-54*24*time.Hour)) || !disableAt.Equal(t0.Add(6*24*time.Hour)) || workflows != `[".github/workflows/yearly.yml"]` {
		t.Fatalf("payload: last %v disable %v workflows %s", lastActivity, disableAt, workflows)
	}
	e.clock.set(t0.Add(time.Hour))
	tick(t, w)
	if got := events(); got != want {
		t.Fatalf("unchanged run: %s", got)
	}
	up.SetFixture("b")
	e.clock.set(t0.Add(2 * time.Hour))
	tick(t, w)
	if got := events(); got != want+",schedule.enabled .github/workflows/mirror.yml" {
		t.Fatalf("fixture b: %s", got)
	}
	var soon *bool
	if err := e.pool.QueryRow(context.Background(), `SELECT (state->>'disable_soon')::bool, (state->>'disable_at')::timestamptz FROM watch_states
		WHERE watch_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&soon, &disableAt); err != nil || soon != nil || !disableAt.Equal(t0.Add(2*time.Hour+59*24*time.Hour)) {
		t.Fatalf("fixture b state: soon %v disable_at %v err %v", soon, disableAt, err)
	}
	if up.Hits("/repos/acme/widgets") != 3 || up.Hits("/repos/acme/widgets/commits") != 3 {
		t.Fatalf("warn calls: repo %d commits %d", up.Hits("/repos/acme/widgets"), up.Hits("/repos/acme/widgets/commits"))
	}
}

// 001-AC8: the first-run opt-in is per kind: another kind's first state stays a quiet baseline even when the
// github_schedules collector is in the same worker.
func TestBaselineQuietBesideDeadman(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.addWatch(t, "github_repo", fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"include_prereleases":true}`, srv.URL), "1m")
	tick(t, e.worker(t, e.pool, collect.Collectors()))
	if r := e.row(t, id); *r.status != "ok" || e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id) != 1 || len(e.events(t, id)) != 0 {
		t.Fatalf("baseline: %s, events %v", *r.status, e.events(t, id))
	}
}

// schedulesAutoWatch adds a github_schedules watch with auto_enable on base, its github_token credential holding secret.
func (e *env) schedulesAutoWatch(t *testing.T, base, credID, secret string) string {
	t.Helper()
	id := e.addWatch(t, "github_schedules", fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":0,"auto_enable":true}`, base), "1m")
	ct, nonce := e.box.Seal([]byte(secret), []byte(credID))
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO credentials (id, owner_id, name, kind, secret_ciphertext, secret_nonce, key_version)
		VALUES ($1, $2, 'gh', 'github_token', $3, $4, 1)`, credID, e.user, ct, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET credential_id = $2 WHERE id = $1`, id, credID); err != nil {
		t.Fatal(err)
	}
	return id
}

// enables lists the fake's received re-enable PUTs.
func enables(up *testupstream.Server) []testupstream.Received {
	var out []testupstream.Received
	for _, r := range up.Received() {
		if strings.HasPrefix(r.Endpoint, "enable-") {
			out = append(out, r)
		}
	}
	return out
}

// 019-AC14, the phase 3 DoD guard: an auto_enable watch with a credential, on fixture a with yearly.yml disabled by hand,
// sends one PUT, for the disabled_inactivity mirror.yml, with the credential, and none for yearly.yml. mirror.yml is stored
// active with reenabled_at; it gets schedule.reenabled, yearly.yml schedule.disabled. The PUT is logged, not audited.
// The next run lists mirror.yml active: no PUT, no event.
func TestSchedulesWatchAutoEnable(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	up.SetNow(e.clock.now)
	up.SetWorkflowState("1002", "disabled_manually")
	srv := httptest.NewServer(up)
	defer srv.Close()
	id := e.schedulesAutoWatch(t, srv.URL, "6f1c1d4e-0000-4000-8000-000000000019", "ghp_auto")
	w := e.worker(t, e.pool, collect.Collectors())
	core, logs := observer.New(zap.InfoLevel)
	w.log = zap.New(core)
	events := func() string {
		rows, _ := e.pool.Query(context.Background(), `SELECT type || ' ' || (payload->>'path') || ' ' || coalesce(payload->>'from', payload->>'state')
			FROM events WHERE watch_id = $1 ORDER BY id`, id)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	const want = "schedule.reenabled .github/workflows/mirror.yml disabled_inactivity,schedule.disabled .github/workflows/yearly.yml disabled_manually"
	tick(t, w)
	if r := e.row(t, id); *r.status != "ok" {
		t.Fatalf("run: %s %v", *r.status, r.lastErr)
	}
	if put := enables(up); len(put) != 1 || put[0].Endpoint != "enable-1001" || put[0].Header["Authorization"] != "Bearer ghp_auto" {
		t.Fatalf("PUTs %+v", put)
	}
	if got := events(); got != want {
		t.Fatalf("events %s", got)
	}
	var mirror struct {
		State       string    `json:"state"`
		ReenabledAt time.Time `json:"reenabled_at"`
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT w FROM watch_states, jsonb_array_elements(state->'workflows') w
		WHERE watch_id = $1 AND w->>'path' = '.github/workflows/mirror.yml'`, id).Scan(&mirror); err != nil || mirror.State != "active" || !mirror.ReenabledAt.Equal(t0) {
		t.Fatalf("mirror %+v, err %v", mirror, err)
	}
	ok := logs.FilterMessage("workflow_reenabled").AllUntimed()
	if len(ok) != 1 || logs.FilterMessage("workflow_reenable_failed").Len() != 0 {
		t.Fatalf("logs %+v", logs.All())
	}
	if f := ok[0].ContextMap(); f["watch_id"] != id || f["repo"] != "acme/widgets" || f["workflow_path"] != ".github/workflows/mirror.yml" || f["status"] != int64(http.StatusNoContent) {
		t.Fatalf("log fields %v", f)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log`); n != 0 {
		t.Fatalf("%d audit rows", n)
	}

	e.clock.set(t0.Add(time.Hour))
	tick(t, w)
	if got := events(); got != want || len(enables(up)) != 1 || logs.FilterMessage("workflow_reenabled").Len() != 1 {
		t.Fatalf("next run: events %s, PUTs %d", got, len(enables(up)))
	}
}

// 019-AC15: a PUT answering 403 does not fail the run: mirror.yml stays disabled, its schedule.disabled carries the
// reason, the failure is logged, and the next run tries again with no second event.
func TestSchedulesWatchAutoEnableRefused(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	up.SetNow(e.clock.now)
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
			return
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	id := e.schedulesAutoWatch(t, srv.URL, "6f1c1d4e-0000-4000-8000-000000000020", "ghp_auto")
	w := e.worker(t, e.pool, collect.Collectors())
	core, logs := observer.New(zap.InfoLevel)
	w.log = zap.New(core)
	tick(t, w)
	if r := e.row(t, id); *r.status != "ok" || puts.Load() != 1 {
		t.Fatalf("run: %s %v, PUTs %d", *r.status, r.lastErr, puts.Load())
	}
	var typ, reason string
	if err := e.pool.QueryRow(context.Background(), `SELECT string_agg(type, ','), string_agg(payload->>'reenable_error', ',') FROM events WHERE watch_id = $1`, id).Scan(&typ, &reason); err != nil ||
		typ != "schedule.disabled" || reason != "HTTP 403: Resource not accessible by personal access token" {
		t.Fatalf("events %s %q, err %v", typ, reason, err)
	}
	failed := logs.FilterMessage("workflow_reenable_failed").AllUntimed()
	if len(failed) != 1 || failed[0].ContextMap()["error"] != reason || failed[0].ContextMap()["status"] != int64(http.StatusForbidden) || logs.FilterMessage("workflow_reenabled").Len() != 0 {
		t.Fatalf("logs %+v", logs.All())
	}
	e.clock.set(t0.Add(time.Hour))
	tick(t, w)
	if r := e.row(t, id); *r.status != "ok" || puts.Load() != 2 || e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1`, id) != 1 {
		t.Fatalf("next run: %s, PUTs %d", *r.status, puts.Load())
	}
}

// 019-AC14: a shutdown past the drain timeout during the second PUT, the first one done: the collector returns a whole
// state, which the worker stores instead of releasing, so GitHub's re-enable is recorded (reenabled_at, one
// schedule.reenabled). A run cut short with an error is still released (TestRunDrainsAndReleases).
func TestSchedulesWatchShutdownAfterReenable(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	up.SetNow(e.clock.now)
	up.SetWorkflowState("1002", "disabled_inactivity")
	second := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/1002/") {
			close(second)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	id := e.schedulesAutoWatch(t, srv.URL, "6f1c1d4e-0000-4000-8000-000000000021", "ghp_auto")
	w := e.worker(t, e.pool, collect.Collectors())
	w.opts.DrainTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("no second PUT")
	}
	cancel()
	<-done
	if r := e.row(t, id); r.status == nil || *r.status != "ok" {
		t.Fatalf("run not stored: %+v", r)
	}
	var reenabledAt time.Time
	var reason string
	if err := e.pool.QueryRow(context.Background(), `SELECT (m->>'reenabled_at')::timestamptz, COALESCE(y->>'reenable_error', '') FROM watch_states,
		jsonb_array_elements(state->'workflows') m, jsonb_array_elements(state->'workflows') y
		WHERE watch_id = $1 AND m->>'path' = '.github/workflows/mirror.yml' AND y->>'path' = '.github/workflows/yearly.yml'`, id).Scan(&reenabledAt, &reason); err != nil ||
		!reenabledAt.Equal(t0) || reason != "" {
		t.Fatalf("state: reenabled_at %v, yearly %q, err %v", reenabledAt, reason, err)
	}
	var got string
	if err := e.pool.QueryRow(context.Background(), `SELECT string_agg(type || ' ' || (payload->>'path'), ',' ORDER BY id) FROM events WHERE watch_id = $1`, id).Scan(&got); err != nil ||
		got != "schedule.reenabled .github/workflows/mirror.yml,schedule.disabled .github/workflows/yearly.yml" {
		t.Fatalf("events %s, err %v", got, err)
	}
}
