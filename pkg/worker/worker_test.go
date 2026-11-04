package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

const testKey = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type env struct {
	url   string
	pool  *pgxpool.Pool
	user  string
	box   *secret.Box
	clock *clock
	reg   *prometheus.Registry
	m     *Metrics
}

func connect(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{url: testpg.NewDB(t), clock: &clock{t: t0}, reg: prometheus.NewRegistry()}
	e.pool = connect(t, e.url)
	if _, err := db.Migrate(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (oidc_sub, email) VALUES ('u', 'u@x') RETURNING id::text`).Scan(&e.user); err != nil {
		t.Fatal(err)
	}
	e.box, _ = secret.New(testKey)
	e.m = NewMetrics(e.reg)
	return e
}

func (e *env) addWatch(t *testing.T, kind, config, schedule string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO watches (owner_id, name, kind, config, schedule, next_run_at)
		VALUES ($1, 'w', $2, $3, $4, $5) RETURNING id::text`, e.user, kind, config, schedule, t0).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) worker(t *testing.T, pool *pgxpool.Pool, collectors map[string]collect.Collector) *Worker {
	t.Helper()
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	client := g.Client(netguard.Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1"}})
	w, err := New(pool, zap.NewNop(), e.box, client, collectors, e.m,
		Options{Poll: 10 * time.Millisecond, Concurrency: 4, DrainTimeout: time.Second, UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	w.now = e.clock.now
	w.jitter = func() float64 { return 0 }
	return w
}

// tick runs everything due at the current clock, like one poll that keeps claiming until nothing is left.
func tick(t *testing.T, w *Worker) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	sem := make(chan struct{}, w.opts.Concurrency)
	for {
		n, err := w.dispatch(ctx, ctx, sem, &wg)
		if err != nil {
			t.Error(err)
			break
		}
		if n > 0 {
			continue
		}
		if len(sem) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
}

type row struct {
	status   *string
	lastErr  *string
	failures int
	next     time.Time
}

func (e *env) row(t *testing.T, id string) row {
	t.Helper()
	var r row
	if err := e.pool.QueryRow(context.Background(), `SELECT last_status, last_error, consecutive_failures, next_run_at FROM watches WHERE id = $1`,
		id).Scan(&r.status, &r.lastErr, &r.failures, &r.next); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// fake counts runs per config and returns what collect says; its diff is one event per changed state.
type fake struct {
	mu      sync.Mutex
	runs    map[string]int
	collect func(ctx context.Context, cfg string, cred *collect.Credential) (json.RawMessage, error)
}

func newFake(fn func(ctx context.Context, cfg string, cred *collect.Credential) (json.RawMessage, error)) *fake {
	return &fake{runs: map[string]int{}, collect: fn}
}

func (f *fake) Collect(ctx context.Context, _ *collect.Fetcher, cfg json.RawMessage, cred *collect.Credential) (json.RawMessage, error) {
	f.mu.Lock()
	f.runs[string(cfg)]++
	f.mu.Unlock()
	return f.collect(ctx, string(cfg), cred)
}

func (f *fake) Diff(old, cur json.RawMessage) ([]collect.Event, error) {
	return []collect.Event{{Type: "fake.changed", Subject: string(cur), Payload: map[string]any{"from": string(old), "to": string(cur)}}}, nil
}

func (f *fake) total() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.runs {
		out[k] = v
	}
	return out
}

// 001-AC9: 2 workers (own pools, as 2 replicas) against one database, 50 watches, 10 ticks: every
// due run executes exactly once per due time.
func TestExactlyOnceAcrossReplicas(t *testing.T) {
	e := newEnv(t)
	for i := range 50 {
		e.addWatch(t, "github_release", fmt.Sprintf(`{"n": %d}`, i), "1m")
	}
	slow := func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
		time.Sleep(2 * time.Millisecond) // keeps both replicas claiming at once
		return json.RawMessage(`{"v":1}`), nil
	}
	f1, f2 := newFake(slow), newFake(slow)
	w1 := e.worker(t, connect(t, e.url), map[string]collect.Collector{"github_release": f1})
	w2 := e.worker(t, connect(t, e.url), map[string]collect.Collector{"github_release": f2})
	for k := range 10 {
		e.clock.set(t0.Add(time.Duration(k) * time.Minute))
		var wg sync.WaitGroup
		wg.Go(func() { tick(t, w1) })
		wg.Go(func() { tick(t, w2) })
		wg.Wait()
		r1, r2 := f1.total(), f2.total()
		for i := range 50 {
			cfg := fmt.Sprintf(`{"n": %d}`, i)
			if got := r1[cfg] + r2[cfg]; got != k+1 {
				t.Fatalf("tick %d: watch %d ran %d times, want %d", k, i, got, k+1)
			}
		}
	}
	sum := func(m map[string]int) (n int) {
		for _, v := range m {
			n += v
		}
		return n
	}
	if a, b := sum(f1.total()), sum(f2.total()); a+b != 500 || a == 0 || b == 0 {
		t.Fatalf("runs split %d + %d, want 500 shared by both", a, b)
	}
	if got := testutil.ToFloat64(e.m.runs.WithLabelValues("github_release", "ok")); got != 500 {
		t.Fatalf("runs metric %v", got)
	}
}

// 001-AC8, 001-AC9: the first run stores a baseline without events; a crashed claim is re-run after
// the lease; replaying a change inserts no duplicate event.
func TestBaselineLeaseAndDedup(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "1m")
	state := `{"v":1}`
	var mu sync.Mutex
	f := newFake(func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		return json.RawMessage(state), nil
	})
	w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": f})

	tick(t, w)
	if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != 1 {
		t.Fatalf("baseline states %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("baseline emitted %d events", n)
	}

	// A replica claims the watch and dies: nobody runs it until the lease ends.
	e.clock.set(t0.Add(time.Minute))
	jobs, err := w.claim(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v %v", jobs, err)
	}
	mu.Lock()
	state = `{"v":2}`
	mu.Unlock()
	e.clock.set(t0.Add(3 * time.Minute))
	tick(t, w)
	if f.total()[`{}`] != 1 {
		t.Fatal("a leased watch ran before its lease ended")
	}
	e.expireLease(t, id)
	tick(t, w)
	if f.total()[`{}`] != 2 || e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1 AND type = 'fake.changed'`, id) != 1 {
		t.Fatal("the crashed claim was not re-run after the lease, or its change was not recorded")
	}
	// The same change replayed (same previous state) dedups on its key.
	var prevID int64
	if err := e.pool.QueryRow(context.Background(), `SELECT min(id) FROM watch_states WHERE watch_id = $1`, id).Scan(&prevID); err != nil {
		t.Fatal(err)
	}
	ev, _ := f.Diff([]byte(`{"v":1}`), []byte(`{"v":2}`))
	for range 2 {
		if err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
			got, err := insertEvents(context.Background(), tx, id, prevID, ev, t0)
			if err == nil && len(got) != 0 {
				return fmt.Errorf("replay inserted %v", got)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1`, id); n != 1 {
		t.Fatalf("events after replay %d", n)
	}
	// An unchanged state stores nothing new.
	e.clock.set(t0.Add(time.Hour))
	tick(t, w)
	if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != 2 {
		t.Fatalf("unchanged run stored a state: %d", n)
	}
	if got := testutil.ToFloat64(e.m.events.WithLabelValues("github_release", "fake.changed")); got != 1 {
		t.Fatalf("events metric %v", got)
	}
}

func TestStateHistoryPruned(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "1m")
	n := 0
	w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": newFake(func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
		n++
		return json.RawMessage(fmt.Sprintf(`{"v":%d}`, n)), nil
	})})
	for k := range keepStates + 3 {
		e.clock.set(t0.Add(time.Duration(k) * time.Minute))
		tick(t, w)
	}
	if got := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); got != keepStates {
		t.Fatalf("kept %d states", got)
	}
	if got := e.count(t, `SELECT count(*) FROM events WHERE watch_id = $1`, id); got != keepStates+2 {
		t.Fatalf("events %d (history pruning must not drop events)", got)
	}
}

// 001-AC10: failures back off exponentially from the interval up to 8x, with the error kept, and
// one success recovers.
func TestBackoffAndRecovery(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "10m")
	fail := errors.New(strings.Repeat("é", 600) + "upstream exploded")
	f := newFake(func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
		if fail != nil {
			return nil, fail
		}
		return json.RawMessage(`{}`), nil
	})
	w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": f})
	now := t0
	for i, wantDelay := range []time.Duration{10, 20, 40, 80, 80} {
		e.clock.set(now)
		tick(t, w)
		r := e.row(t, id)
		if *r.status != "error" || r.failures != i+1 || !r.next.Equal(now.Add(wantDelay*time.Minute)) {
			t.Fatalf("failure %d: %s failures=%d next=+%v", i+1, *r.status, r.failures, r.next.Sub(now))
		}
		if len(*r.lastErr) > maxErrorLen || !strings.HasPrefix(*r.lastErr, "éé") || strings.ContainsRune(*r.lastErr, '�') {
			t.Fatalf("last error not truncated on a rune boundary: %d bytes", len(*r.lastErr))
		}
		now = r.next
	}
	fail = nil
	e.clock.set(now)
	tick(t, w)
	if r := e.row(t, id); *r.status != "ok" || r.failures != 0 || r.lastErr != nil || !r.next.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("recovery: %+v", r)
	}
	if got := testutil.ToFloat64(e.m.runs.WithLabelValues("github_release", "error")); got != 5 {
		t.Fatalf("error runs metric %v", got)
	}
	if Backoff(time.Minute, 0) != time.Minute || Backoff(time.Minute, 1000) != 8*time.Minute {
		t.Fatal("backoff bounds")
	}
}

// 001-AC11: a rate limit sets the next run to the reset time (capped at 1 h), keeps the failure count.
func TestRateLimitDelaysNextRun(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "1m")
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET consecutive_failures = 2 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var until time.Time
	w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": newFake(func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
		return nil, &collect.RateLimitError{Until: until, URL: "https://api.github.com/x"}
	})})
	for _, tt := range []struct {
		until, want time.Duration
	}{
		{17 * time.Minute, 17 * time.Minute},
		{3 * time.Hour, time.Hour},
		{-time.Hour, time.Minute},
	} {
		now := e.row(t, id).next
		e.clock.set(now)
		until = now.Add(tt.until)
		tick(t, w)
		r := e.row(t, id)
		if *r.status != "rate_limited" || r.failures != 2 || !r.next.Equal(now.Add(tt.want)) || !strings.Contains(*r.lastErr, "rate limited") {
			t.Fatalf("until +%v: %s failures=%d next=+%v", tt.until, *r.status, r.failures, r.next.Sub(now))
		}
	}
	if got := testutil.ToFloat64(e.m.runs.WithLabelValues("github_release", "rate_limited")); got != 3 {
		t.Fatalf("rate limited metric %v", got)
	}
}

// 001-AC11: a real GitLab 429 with RateLimit-Reset goes through collector and worker.
func TestGitLabRateLimitEndToEnd(t *testing.T) {
	e := newEnv(t)
	reset := t0.Add(25 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("RateLimit-Reset", fmt.Sprint(reset.Unix()))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	id := e.addWatch(t, "gitlab_release", fmt.Sprintf(`{"base_url": %q, "project": "g/p"}`, srv.URL), "5m")
	tick(t, e.worker(t, e.pool, collect.Collectors()))
	if r := e.row(t, id); *r.status != "rate_limited" || !r.next.Equal(reset) {
		t.Fatalf("%+v", r)
	}
}

// 001-AC7, 001-AC8, 001-AC11: the real github_release collector through the worker: baseline, a 304
// on the stored ETag (state untouched), then fixture B's events.
func TestRealCollectorETagAndEvents(t *testing.T) {
	e := newEnv(t)
	up := testupstream.New()
	var inm []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm = append(inm, r.Header.Get("If-None-Match"))
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	id := e.addWatch(t, "github_release", fmt.Sprintf(`{"repo": "cli/cli", "api_url": %q, "include_prereleases": true}`, srv.URL), "1m")
	w := e.worker(t, e.pool, collect.Collectors())
	for k, set := range []string{"a", "a", "b"} {
		up.SetFixture(set)
		e.clock.set(t0.Add(time.Duration(k) * time.Minute))
		tick(t, w)
		if r := e.row(t, id); *r.status != "ok" {
			t.Fatalf("run %d: %s %v", k, *r.status, *r.lastErr)
		}
	}
	if inm[0] != "" || inm[1] == "" || inm[2] != inm[1] {
		t.Fatalf("If-None-Match sent: %q", inm)
	}
	if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != 2 {
		t.Fatalf("states %d", n)
	}
	rows, _ := e.pool.Query(context.Background(), `SELECT type || ' ' || (payload->>'tag') FROM events WHERE watch_id = $1 ORDER BY id`, id)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "release.promoted v2.104.0-rc.1,release.published v2.103.3,release.published v2.105.0" {
		t.Fatalf("events %v", got)
	}
	var occurred *time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT occurred_at FROM events WHERE payload->>'tag' = 'v2.103.3'`).Scan(&occurred)
	if occurred == nil || !occurred.Equal(time.Date(2026, 10, 26, 19, 20, 8, 0, time.UTC)) {
		t.Fatalf("occurred_at %v", occurred)
	}
}

// Credentials are opened for the run; a ciphertext that does not open is a run error, not a crash.
func TestCredential(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	credID := "6f1c1d4e-0000-4000-8000-000000000001" //nolint:gosec // G101 false positive: a row ID
	ct, nonce := e.box.Seal([]byte("robot-password-123"), []byte(credID))
	if _, err := e.pool.Exec(ctx, `INSERT INTO credentials (id, owner_id, name, kind, username, secret_ciphertext, secret_nonce, key_version)
		VALUES ($1, $2, 'c', 'registry_basic', 'robot', $3, $4, 1)`, credID, e.user, ct, nonce); err != nil {
		t.Fatal(err)
	}
	id := e.addWatch(t, "oci_tags", `{}`, "1m")
	if _, err := e.pool.Exec(ctx, `UPDATE watches SET credential_id = $2 WHERE id = $1`, id, credID); err != nil {
		t.Fatal(err)
	}
	var got *collect.Credential
	w := e.worker(t, e.pool, map[string]collect.Collector{"oci_tags": newFake(func(_ context.Context, _ string, c *collect.Credential) (json.RawMessage, error) {
		got = c
		return json.RawMessage(`{}`), nil
	})})
	tick(t, w)
	if got == nil || got.Username != "robot" || got.Secret != "robot-password-123" || got.Kind != "registry_basic" {
		t.Fatalf("credential %v", got)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE credentials SET secret_nonce = $2 WHERE id = $1`, credID, make([]byte, 12)); err != nil {
		t.Fatal(err)
	}
	e.clock.set(t0.Add(time.Minute))
	tick(t, w)
	if r := e.row(t, id); *r.status != "error" || !strings.Contains(*r.lastErr, "credential") {
		t.Fatalf("%+v", r)
	}
}

// A run whose watch was retargeted or deleted meanwhile stores nothing; an unknown kind is an error.
func TestStaleRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	retarget := e.addWatch(t, "github_release", `{"a": 1}`, "1m")
	gone := e.addWatch(t, "github_pr", `{"b": 1}`, "1m")
	release := newFake(func(_ context.Context, cfg string, _ *collect.Credential) (json.RawMessage, error) {
		if cfg == `{"a": 1}` {
			// What the watch API does on a config change: new config, state cleared, due now.
			if _, err := e.pool.Exec(ctx, `UPDATE watches SET config = '{"a": 2}', next_run_at = $2 WHERE id = $1`, retarget, t0); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(cfg), nil
	})
	w := e.worker(t, e.pool, map[string]collect.Collector{
		"github_release": release,
		"github_pr": newFake(func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
			_, err := e.pool.Exec(ctx, `DELETE FROM watches WHERE id = $1`, gone)
			return json.RawMessage(`{}`), err
		}),
	})
	unknown := e.addWatch(t, "gitlab_mr", `{}`, "1m")
	tick(t, w)
	var states []string
	rows, _ := e.pool.Query(ctx, `SELECT state::text FROM watch_states WHERE watch_id = $1`, retarget)
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	// The run on the old target is dropped; the re-run on the new one is the baseline.
	if runs := release.total(); runs[`{"a": 1}`] != 1 || runs[`{"a": 2}`] != 1 || len(states) != 1 || states[0] != `{"a": 2}` {
		t.Fatalf("runs %v, states %v", runs, states)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1`, gone); n != 0 {
		t.Fatal("deleted watch came back")
	}
	if r := e.row(t, unknown); *r.status != "error" || !strings.Contains(*r.lastErr, "no collector") {
		t.Fatalf("unknown kind: %+v", r)
	}
}

// Shutdown: in-flight runs finish within the drain timeout; past it they are cancelled and their
// claims released for another replica.
func TestRunDrainsAndReleases(t *testing.T) {
	t.Run("finishes in-flight", func(t *testing.T) {
		e := newEnv(t)
		id := e.addWatch(t, "github_release", `{}`, "1m")
		started, release := make(chan struct{}), make(chan struct{})
		w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": newFake(func(context.Context, string, *collect.Credential) (json.RawMessage, error) {
			close(started)
			<-release
			return json.RawMessage(`{}`), nil
		})})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.Run(ctx); close(done) }()
		<-started
		cancel()
		time.Sleep(20 * time.Millisecond)
		close(release)
		<-done
		if r := e.row(t, id); r.status == nil || *r.status != "ok" {
			t.Fatalf("in-flight run lost: %+v", r)
		}
		// 001-AC31: the gauge counts due watches; this one is scheduled a minute ahead now.
		w.updateDue(context.Background())
		e.clock.set(t0.Add(time.Minute))
		e.addWatch(t, "github_release", `{"other": 1}`, "1m")
		w.updateDue(context.Background())
		if got := testutil.ToFloat64(e.m.due); got != 2 {
			t.Fatalf("due gauge %v", got)
		}
	})
	t.Run("releases past the drain timeout", func(t *testing.T) {
		e := newEnv(t)
		id := e.addWatch(t, "github_release", `{}`, "1m")
		started := make(chan struct{})
		w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": newFake(func(ctx context.Context, _ string, _ *collect.Credential) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})})
		w.opts.DrainTimeout = 50 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.Run(ctx); close(done) }()
		<-started
		cancel()
		<-done
		if r := e.row(t, id); r.status != nil || !r.next.Equal(t0) {
			t.Fatalf("claim not released: %+v", r)
		}
	})
}

func TestMetricsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.runs.WithLabelValues("oci_tags", "ok").Inc()
	m.duration.WithLabelValues("oci_tags").Observe(0.2)
	m.events.WithLabelValues("oci_tags", "tag.created").Inc()
	// 001-AC31: run counts, durations and event counts per kind are exposed.
	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gedour_collector_runs_total Watch runs by kind and outcome (ok, error, rate_limited; discarded when the claim was lost or the target changed).
# TYPE gedour_collector_runs_total counter
gedour_collector_runs_total{kind="oci_tags",status="ok"} 1
# HELP gedour_events_total Events recorded, by watch kind and event type.
# TYPE gedour_events_total counter
gedour_events_total{kind="oci_tags",type="tag.created"} 1
`), "gedour_collector_runs_total", "gedour_events_total"); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg, "gedour_collector_run_duration_seconds", "gedour_watches_due"); err != nil || n != 2 {
		t.Fatalf("histogram and gauge: %d %v", n, err)
	}
}

// expireLease ends a claim's lease now; leases run on the database clock, not the test's.
func (e *env) expireLease(t *testing.T, id string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET leased_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// sequenced returns states v0, v1, ... in call order; the call numbered block waits for release first.
type sequenced struct {
	mu                 sync.Mutex
	calls, inflight    int
	maxInflight, block int
	started, release   chan struct{}
}

func newSequenced(block int) *sequenced {
	return &sequenced{block: block, started: make(chan struct{}), release: make(chan struct{})}
}

func (s *sequenced) collect(context.Context, string, *collect.Credential) (json.RawMessage, error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.inflight++
	s.maxInflight = max(s.maxInflight, s.inflight)
	s.mu.Unlock()
	if n == s.block {
		close(s.started)
		<-s.release
	}
	s.mu.Lock()
	s.inflight--
	s.mu.Unlock()
	return json.RawMessage(fmt.Sprintf(`{"v":%d}`, n)), nil
}

func (s *sequenced) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (e *env) history(t *testing.T, id string) (latest string, events []string) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `SELECT state::text FROM watch_states WHERE watch_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.pool.Query(ctx, `SELECT (payload->>'from') || '>' || (payload->>'to') FROM events WHERE watch_id = $1 ORDER BY id`, id)
	events, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return latest, events
}

// 001-AC9: a run-now (or an edit, a resume) while a run is in flight makes the watch due again but
// cannot start a second run: the lease is apart from next_run_at. The request is kept for after.
func TestRunNowDuringRunWaitsForTheLease(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "1m")
	seq := newSequenced(1)
	cs := map[string]collect.Collector{"github_release": newFake(seq.collect)}
	w1, w2 := e.worker(t, e.pool, cs), e.worker(t, connect(t, e.url), cs)
	tick(t, w1) // baseline v0

	e.clock.set(t0.Add(time.Minute))
	var a sync.WaitGroup
	a.Go(func() { tick(t, w1) }) // run A fetches v1, blocked
	<-seq.started
	runNow := t0.Add(time.Minute + time.Second)
	e.clock.set(runNow)
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = $2 WHERE id = $1`, id, runNow); err != nil {
		t.Fatal(err)
	}
	tick(t, w2)
	if seq.count() != 2 {
		t.Fatalf("a second replica ran the watch during run A (%d calls)", seq.count())
	}
	close(seq.release)
	a.Wait() // A stores v1; the kept request makes the watch due, so a tick runs it (v2)
	tick(t, w2)
	if seq.count() != 3 {
		t.Fatalf("the run-now request was lost or ran twice: %d calls", seq.count())
	}
	latest, events := e.history(t, id)
	if seq.maxInflight != 1 || latest != `{"v": 2}` || strings.Join(events, ",") != `{"v": 0}>{"v":1},{"v": 1}>{"v":2}` {
		t.Fatalf("max concurrent %d, latest %s, events %v", seq.maxInflight, latest, events)
	}
	if r := e.row(t, id); !r.next.Equal(runNow.Add(time.Minute)) {
		t.Fatalf("next run %v", r.next)
	}
}

// 001-AC9: a run that outlives its lease loses the watch to the replica that re-claimed it; its late
// finish is discarded, so an older fetch never overwrites a newer state.
func TestLateFinishAfterLostLeaseIsDiscarded(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "1m")
	seq := newSequenced(1)
	cs := map[string]collect.Collector{"github_release": newFake(seq.collect)}
	w1, w2 := e.worker(t, e.pool, cs), e.worker(t, connect(t, e.url), cs)
	tick(t, w1)
	e.clock.set(t0.Add(time.Minute))
	var a sync.WaitGroup
	a.Go(func() { tick(t, w1) })
	<-seq.started
	e.expireLease(t, id)
	tick(t, w2) // lease over: v2 stored
	close(seq.release)
	a.Wait() // A's v1 arrives last
	latest, events := e.history(t, id)
	if latest != `{"v": 2}` || strings.Join(events, ",") != `{"v": 0}>{"v":2}` {
		t.Fatalf("stale finish landed: latest %s, events %v", latest, events)
	}
	if got := testutil.ToFloat64(e.m.runs.WithLabelValues("github_release", statusDiscarded)); got != 1 {
		t.Fatalf("discarded metric %v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND claim_token IS NULL AND leased_until IS NULL`, id); n != 1 {
		t.Fatal("lease not freed by the finishing run")
	}
}

// 001-AC10: a collection that hits the run timeout is an error with backoff, stored despite the
// expired run context.
func TestRunTimeoutIsAnError(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "10m")
	w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": newFake(func(ctx context.Context, _ string, _ *collect.Credential) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("GET x: %w", ctx.Err())
	})})
	w.opts.RunTimeout = 50 * time.Millisecond
	tick(t, w)
	r := e.row(t, id)
	if r.status == nil || *r.status != "error" || *r.lastErr != "timeout after 50ms" || r.failures != 1 || !r.next.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("timeout not recorded: %+v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND claim_token IS NULL`, id); n != 1 {
		t.Fatal("lease kept after a timed-out run")
	}
}

// 001-AC10: data the database refuses never loops silently: NUL is stripped from states and errors,
// and a finish that still fails is stored as an error with backoff, lease freed.
func TestStoreFailures(t *testing.T) {
	e := newEnv(t)
	nulState := e.addWatch(t, "github_release", `{"a": 1}`, "10m")
	nulErr := e.addWatch(t, "github_release", `{"b": 1}`, "10m")
	broken := e.addWatch(t, "github_release", `{"c": 1}`, "10m")
	w := e.worker(t, e.pool, map[string]collect.Collector{"github_release": newFake(func(_ context.Context, cfg string, _ *collect.Credential) (json.RawMessage, error) {
		switch cfg {
		case `{"a": 1}`:
			return json.RawMessage(`{"v":"a\u0000b","k\u0000":[" \u0000"]}`), nil
		case `{"b": 1}`:
			return nil, errors.New("bad\x00byte")
		}
		return json.RawMessage(`{"v":`), nil // not JSON: the state insert fails every time
	})})
	tick(t, w)
	var state string
	if err := e.pool.QueryRow(context.Background(), `SELECT state::text FROM watch_states WHERE watch_id = $1`, nulState).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, nulState); *r.status != "ok" || state != `{"k": [" "], "v": "ab"}` {
		t.Fatalf("NUL state: %s %+v", state, r)
	}
	if r := e.row(t, nulErr); *r.status != "error" || *r.lastErr != "badbyte" {
		t.Fatalf("NUL error: %+v", r)
	}
	r := e.row(t, broken)
	if r.status == nil || *r.status != "error" || !strings.HasPrefix(*r.lastErr, "store: ") || r.failures != 1 || !r.next.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("finish failure not recorded: %+v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE claim_token IS NULL AND leased_until IS NULL`); n != 3 {
		t.Fatalf("leases freed on %d of 3 watches", n)
	}
	if got := testutil.ToFloat64(e.m.runs.WithLabelValues("github_release", "error")); got != 2 {
		t.Fatalf("error runs metric %v", got)
	}
	if _, err := collect.StripNUL(json.RawMessage(`{"\u0000`)); err == nil {
		t.Fatal("bad JSON passed StripNUL")
	}
}

func TestNewRejectsRunTimeoutPastLease(t *testing.T) {
	for _, d := range []time.Duration{Lease, Lease - finishTimeout + time.Second} {
		if _, err := New(nil, zap.NewNop(), nil, nil, nil, nil, Options{RunTimeout: d}); err == nil || !strings.Contains(err.Error(), "exceeds the lease") {
			t.Errorf("%v: %v", d, err)
		}
	}
	w, err := New(nil, zap.NewNop(), nil, nil, nil, nil, Options{RunTimeout: Lease - finishTimeout})
	if err != nil || w.opts.RunTimeout != Lease-finishTimeout {
		t.Fatal(err)
	}
	if w, err := New(nil, zap.NewNop(), nil, nil, nil, nil, Options{}); err != nil || w.opts.RunTimeout != RunTimeout {
		t.Fatal("default run timeout")
	}
}

// Leases run on the database clock: a worker clock far off does not shorten or stretch them.
func TestLeaseUsesDatabaseClock(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_release", `{}`, "1m")
	e.clock.set(t0.Add(-365 * 24 * time.Hour * 10)) // a decade behind
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = $2 WHERE id = $1`, id, e.clock.now()); err != nil {
		t.Fatal(err)
	}
	w := e.worker(t, e.pool, map[string]collect.Collector{})
	if jobs, err := w.claim(context.Background(), 1); err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v %v", jobs, err)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND leased_until BETWEEN now() + interval '4 minutes' AND now() + interval '6 minutes'`, id); n != 1 {
		t.Fatal("lease not on the database clock")
	}
}
