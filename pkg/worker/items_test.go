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
