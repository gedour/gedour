package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/watch"
	"github.com/gedour/gedour/pkg/worker"
)

func (e *env) noteEvent(t *testing.T, w, tag, body, cves string) int64 {
	t.Helper()
	var id int64
	e.scalar(t, &id, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published',
		jsonb_build_object('tag', $2::text, 'body', $3::text, 'cves', $4::jsonb), $2) RETURNING id`, w, tag, body, cves)
	return id
}

// 004-AC5, 004-AC9: routing reads the stored payload: list membership on cves, a mark deep in body.
func TestRoutingOnNotes(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.user(t, "alice"), e.user(t, "bob"), e.user(t, "carol")
	w := e.watch(t, alice)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO watch_shares (watch_id, user_id, permission) SELECT $1, u, 'ro' FROM unnest($2::uuid[]) u`, w, []string{bob, carol}); err != nil {
		t.Fatal(err)
	}
	e.rule(t, alice, nil, `"CVE-2026-1234" in event.payload.cves`)
	e.rule(t, bob, nil, `event.payload.body matches "60KIB"`)
	e.rule(t, carol, nil, `len(event.payload.cves) == 0 && event.payload.body == ""`)
	cve := e.noteEvent(t, w, "v1", "Fixes cve-2026-1234.", `["CVE-2026-1234"]`)
	deep := e.noteEvent(t, w, "v2", strings.Repeat("x", 60<<10)+"60KIB", `[]`)
	empty := e.noteEvent(t, w, "v3", "", `[]`)
	routeAll(t, e.router())
	for _, tt := range []struct {
		user  string
		event int64
	}{{alice, cve}, {bob, deep}, {carol, empty}} {
		if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, tt.user, tt.event); n != 1 {
			t.Errorf("user %s, event %d: %d items", tt.user, tt.event, n)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_items`); n != 3 {
		t.Fatalf("%d inbox items, want 3", n)
	}
}

// 004-AC4: a rule on all watches reads the notes fields of any event: no eval error, no match by accident, and
// nothing added to what is stored.
func TestNotesFiltersOnEventsWithoutThem(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	pr := e.event(t, w, "pr.state_changed", "pr")
	// Stored before the collectors wrote the fields.
	old := e.event(t, w, "release.published", "old")
	rel := e.noteEvent(t, w, "v1", "Notes for X.", `["x"]`)
	tests := []struct {
		filter    string
		onRelease bool
		owner     string
	}{
		{filter: `len(event.payload.cves) > 0`, onRelease: true},
		{filter: `any(event.payload.cves, {# == "x"})`, onRelease: true},
		{filter: `lower(event.payload.body) contains "x"`, onRelease: true},
		{filter: `len(event.payload.cves) == 0`},
	}
	for i := range tests {
		tt := &tests[i]
		tt.owner = e.user(t, fmt.Sprintf("u%d", i))
		if _, err := e.pool.Exec(context.Background(), `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, w, tt.owner); err != nil {
			t.Fatal(err)
		}
		e.rule(t, tt.owner, nil, tt.filter)
	}
	core, logs := observer.New(zap.WarnLevel)
	routeAll(t, NewRouter(e.pool, zap.New(core), e.m, 10*time.Millisecond))
	if got := testutil.ToFloat64(e.m.evalErrors); got != 0 {
		t.Errorf("gedour_rule_eval_errors_total = %v", got)
	}
	if n := logs.FilterMessage("rule_eval_failed").Len(); n != 0 {
		t.Errorf("%d rule_eval_failed: %v", n, logs.All())
	}
	for _, tt := range tests {
		for _, ev := range []struct {
			name string
			id   int64
			want bool
		}{{"pr", pr, !tt.onRelease}, {"old release", old, !tt.onRelease}, {"release", rel, tt.onRelease}} {
			n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, tt.owner, ev.id)
			if (n == 1) != ev.want {
				t.Errorf("%s on %s: %d items, want match %v", tt.filter, ev.name, n, ev.want)
			}
		}
	}
	if n := e.count(t, `SELECT count(*) FROM events WHERE id = ANY($1) AND (payload ? 'cves' OR payload ? 'body')`, []int64{pr, old}); n != 0 {
		t.Errorf("%d stored payloads gained a field", n)
	}
}

// 004-AC4: the defaults fill absent and null fields of the filter's copy only, and overwrite nothing.
func TestEnvOfDefaults(t *testing.T) {
	for _, tt := range []struct {
		name    string
		payload map[string]any
		cves    any
		body    any
	}{
		{"nil payload", nil, []any{}, ""},
		{"absent", map[string]any{"tag": "v1"}, []any{}, ""},
		{"null", map[string]any{"cves": nil, "body": nil}, []any{}, ""},
		{"set", map[string]any{"cves": []any{"x"}, "body": "notes"}, []any{"x"}, "notes"},
		{"another type", map[string]any{"cves": "x", "body": 3.0}, "x", 3.0},
	} {
		before := fmt.Sprint(tt.payload)
		got := envOf(watch.Event{Payload: tt.payload}, WatchRef{}).Event.Payload
		if !reflect.DeepEqual(got["cves"], tt.cves) || !reflect.DeepEqual(got["body"], tt.body) {
			t.Errorf("%s: cves %#v, body %#v", tt.name, got["cves"], got["body"])
		}
		if after := fmt.Sprint(tt.payload); after != before {
			t.Errorf("%s: the event's payload became %s", tt.name, after)
		}
	}
}

// A saved filter the model no longer accepts is named at Error when the router starts: not a disabled rule, not a
// rule that still compiles.
func TestRouterLogsRefusedRules(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	const refused = `event.payload[event.type] matches "(?:[ab]?){30}zz"`
	bad := e.rule(t, alice, nil, refused)
	broken := e.rule(t, alice, nil, `event.nope == 1`)
	e.rule(t, alice, nil, `event.type == "x"`)
	e.rule(t, alice, nil, "")
	off := e.rule(t, alice, nil, refused)
	if _, err := e.pool.Exec(context.Background(), `UPDATE rules SET enabled = false WHERE id = $1`, off); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.ErrorLevel)
	r := NewRouter(e.pool, zap.New(core), e.m, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for logs.Len() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	got := map[string]string{}
	for _, l := range logs.FilterMessage("rule_filter_refused").All() {
		m := l.ContextMap()
		if m["owner_id"] != alice {
			t.Errorf("owner: %v", m)
		}
		got[fmt.Sprint(m["rule_id"])] = fmt.Sprint(m["error"])
	}
	if len(got) != 2 || !strings.Contains(got[bad], "above the budget") || !strings.Contains(got[broken], "no field nope") {
		t.Fatalf("logged %v, want rules %s and %s", got, bad, broken)
	}

	// The check does not stand in the way of a stopped context.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	core, logs = observer.New(zap.ErrorLevel)
	NewRouter(e.pool, zap.New(core), e.m, time.Hour).logRefusedRules(stopped)
	if logs.Len() != 0 {
		t.Fatalf("logs on a stopped context: %v", logs.All())
	}
}

// worstRuleSet gives one user n rules of a costly filter on a hostile body; bodyShapes holds the slowest the budget allows.
func (e *env) worstRuleSet(t *testing.T, owner string, n int) {
	t.Helper()
	src := `event.payload.body matches "(?i)` + strings.Repeat("[ab]?", 25) + `z"`
	for range n {
		e.rule(t, owner, nil, src)
	}
}

// 004-AC11: a worst-case rule set holds the router for seconds; meanwhile other watches, the routed event's own
// watch among them, finish their runs and deliveries go out, on one CPU too.
func TestRoutingBlocksNothing(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	e := newEnv(t)
	alice := e.user(t, "alice")
	up := testupstream.New()
	upstream := httptest.NewServer(up)
	defer upstream.Close()
	var sent atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent.Add(1) }))
	defer hook.Close()

	routed, other := e.watch(t, alice), e.watch(t, alice)
	// A pending delivery of an event routed before.
	first := e.event(t, other, "tag.created", "v0")
	e.rule(t, alice, &other, "", e.webhook(t, alice, hook.URL))
	routeAll(t, e.router())
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1`, first); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
	e.worstRuleSet(t, alice, 100)
	e.noteEvent(t, routed, "hostile", strings.Repeat("ab", collect.MaxNotes/2), `[]`)
	// Both watches come due: the run of the routed event's watch inserts into the rows the router holds.
	cfg := fmt.Sprintf(`{"repo":"cli/cli","api_url":%q,"include_prereleases":true}`, upstream.URL)
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET config = $1, schedule = '1h', next_run_at = now() + interval '1 hour'`, cfg); err != nil {
		t.Fatal(err)
	}

	r := e.router()
	routing := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		n, err := r.RouteOnce(context.Background())
		if err != nil || n == 0 {
			t.Errorf("route: %d, %v", n, err)
		}
		routing <- time.Since(start)
	}()
	// Holds the router inside its transaction before the others start.
	time.Sleep(50 * time.Millisecond)

	g, _ := netguard.New([]string{"127.0.0.0/8"})
	client := g.Client(netguard.Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1"}})
	wk, err := worker.New(e.pool, zap.NewNop(), e.box, client, collect.Collectors(), worker.NewMetrics(prometheus.NewRegistry()),
		worker.Options{Poll: 10 * time.Millisecond, Concurrency: 4, DrainTimeout: time.Second, UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { wk.Run(ctx); done <- struct{}{} }()
	d := e.deliverer(t)
	go func() { d.Run(ctx); done <- struct{}{} }()
	defer func() { cancel(); <-done; <-done }()
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = now()`); err != nil {
		t.Fatal(err)
	}

	const onTime = 2 * time.Second
	began := time.Now()
	for sent.Load() == 0 || e.count(t, `SELECT count(*) FROM watches WHERE last_status = 'ok'`) != 2 {
		if time.Since(began) > onTime*raceFactor {
			t.Fatalf("after %s: %d sent, %d runs done", time.Since(began), sent.Load(), e.count(t, `SELECT count(*) FROM watches WHERE last_status = 'ok'`))
		}
		time.Sleep(5 * time.Millisecond)
	}
	others := time.Since(began)
	select {
	case took := <-routing:
		t.Fatalf("the router ended first (%s, the others %s): nothing was measured", took, others)
	default:
	}
	took := <-routing
	t.Logf("router held %s; a run of each watch and a delivery took %s meanwhile", took, others)
}
