package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/wake"
	"github.com/gedour/gedour/pkg/worker"
)

// settle is past minPass plus the jitter: a wake after it is not held back by the previous pass.
const settle = 1300 * time.Millisecond

var loops = []string{"watches", "router", "deliveries"}

// process runs the worker role as app.Run wires it: one listener, the three loops paced by poll.
func (e *env) process(t *testing.T, poll time.Duration) *wake.Metrics {
	t.Helper()
	m := wake.NewMetrics(prometheus.NewRegistry())
	l := wake.NewListener(e.pool, zap.NewNop())
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	// No collector: a claimed watch fails at once, which still records when it ran.
	w, err := worker.New(e.pool, zap.NewNop(), e.box, g.Client(netguard.Options{Timeout: time.Second}), map[string]collect.Collector{},
		worker.NewMetrics(prometheus.NewRegistry()), worker.Options{Poll: poll, Concurrency: 2, DrainTimeout: time.Second, Wake: l.Subscribe("watches"), Wakeups: m})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(e.pool, zap.NewNop(), e.m, wake.NewLoop("router", poll, l.Subscribe("events"), m))
	d := e.deliverer(t)
	d.loop = wake.NewLoop("deliveries", poll, l.Subscribe("deliveries"), m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 4)
	for _, run := range []func(context.Context){l.Run, w.Run, r.Run, d.Run} {
		go func() { run(ctx); done <- struct{}{} }()
	}
	t.Cleanup(func() {
		cancel()
		for range 4 {
			<-done
		}
	})
	return m
}

func wakeups(m *wake.Metrics, loop, reason string) float64 {
	return testutil.ToFloat64(m.Wakeups.WithLabelValues(loop, reason))
}

func passes(m *wake.Metrics, loop string) float64 {
	n := 0.0
	for _, reason := range []string{"deadline", "notify", "poll", "retry"} {
		n += wakeups(m, loop, reason)
	}
	return n
}

func waitUntil(t *testing.T, what string, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// idle waits for every loop's wake after its first pass (the first LISTEN's, or a deadline that won the select),
// then for the pass it caused to be well over.
func idle(t *testing.T, m *wake.Metrics) {
	t.Helper()
	for _, loop := range loops {
		waitUntil(t, loop+" first wake", 5*time.Second, func() bool { return passes(m, loop) >= 1 })
	}
	time.Sleep(settle)
}

// hook is a webhook receiver reporting when each request arrives.
func (e *env) hook(t *testing.T, owner string) (string, chan time.Time) {
	t.Helper()
	hits := make(chan time.Time, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits <- time.Now() }))
	t.Cleanup(srv.Close)
	return e.webhook(t, owner, srv.URL), hits
}

func hitWithin(t *testing.T, hits chan time.Time, from time.Time, limit time.Duration) {
	t.Helper()
	select {
	case at := <-hits:
		if took := at.Sub(from); took > limit {
			t.Fatalf("sent %s after the commit, want within %s", took, limit)
		} else {
			t.Logf("sent %s after the commit", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never sent")
	}
}

// 015-AC1: an idle worker process passes about once per safety poll per loop, not on a fixed tick.
// Scaled: 2 s poll over 5 s, at most 3 passes per loop (the spec: 60 s over 120 s, 9 for the three).
func TestIdleProcessSleeps(t *testing.T) {
	e := newEnv(t)
	m := e.process(t, 2*time.Second)
	idle(t, m)
	before := map[string]float64{}
	for _, loop := range loops {
		before[loop] = passes(m, loop)
	}
	time.Sleep(5 * time.Second)
	for _, loop := range loops {
		if n := passes(m, loop) - before[loop]; n > 3 {
			t.Errorf("%s: %v passes in 5 s, want at most 3", loop, n)
		}
	}
}

// 015-AC4: events committed by a run are routed and their immediate deliveries sent within 2 s, with a 60 s poll.
func TestEventsWakeRouterAndDeliverer(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch, hits := e.hook(t, alice)
	e.rule(t, alice, nil, "", ch)
	m := e.process(t, time.Minute)
	idle(t, m)
	// The insert a run's finish makes, in its own committed statement.
	e.event(t, w, "release.published", "v1")
	hitWithin(t, hits, time.Now(), 2*time.Second)
}

// 015-AC5: a manual retry is sent within 1 s, with a 60 s poll.
func TestRetryWakesDeliverer(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch, hits := e.hook(t, alice)
	e.rule(t, alice, nil, "", ch)
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	var id int64
	e.scalar(t, &id, `UPDATE deliveries SET status = 'failed', attempts = 1, next_attempt_at = now() + interval '1 hour' RETURNING id`)
	m := e.process(t, time.Minute)
	idle(t, m)
	s := New(e.pool, zap.NewNop(), e.box, nil, nil, nil, nil, e.m)
	s.viewer = func(context.Context) (string, bool) { return alice, true }
	if _, err := s.retryDelivery(context.Background(), &deliveryPath{ID: id}); err != nil {
		t.Fatal(err)
	}
	hitWithin(t, hits, time.Now(), time.Second)
}

// 015-AC7: a killed LISTEN connection comes back, every loop wakes once it does, and a run-now is then claimed
// within 1 s.
func TestListenerReconnectWakesLoops(t *testing.T) {
	e := newEnv(t)
	w := e.watch(t, e.user(t, "alice"))
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	m := e.process(t, time.Minute)
	idle(t, m)
	before := map[string]float64{}
	for _, loop := range loops {
		before[loop] = wakeups(m, loop, "notify")
	}
	var killed bool
	e.scalar(t, &killed, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND query = 'LISTEN `+wake.Channel+`'`)
	if !killed {
		t.Fatal("no listen backend to terminate")
	}
	// The reconnect waits its 1 s backoff.
	for _, loop := range loops {
		waitUntil(t, loop+" wake after reconnect", 5*time.Second, func() bool { return wakeups(m, loop, "notify") > before[loop] })
	}
	time.Sleep(settle)
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = now() WHERE id = $1`, w); err != nil {
		t.Fatal(err)
	}
	from := time.Now()
	waitUntil(t, "the run-now", 5*time.Second, func() bool {
		return e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND last_run_at IS NOT NULL`, w) == 1
	})
	var took float64
	e.scalar(t, &took, `SELECT EXTRACT(EPOCH FROM last_run_at - $2::timestamptz)::float8 FROM watches WHERE id = $1`, w, from)
	if took > 1 {
		t.Fatalf("claimed %.2f s after the run-now, want within 1 s", took)
	}
}

// With every send slot taken, a delivery already due does not wake the loop each second: the send's end does.
func TestSaturatedDelivererSleeps(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	release := make(chan struct{})
	hits := make(chan time.Time, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits <- time.Now()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	e.rule(t, alice, nil, "", e.webhook(t, alice, srv.URL))
	e.event(t, w, "release.published", "v1")
	e.event(t, w, "release.published", "v2")
	routeAll(t, e.router())
	m := wake.NewMetrics(prometheus.NewRegistry())
	d := e.deliverer(t)
	d.concurrency = 1
	d.loop = wake.NewLoop("deliveries", 10*time.Second, nil, m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	released := false
	defer func() {
		if !released {
			close(release)
		}
		cancel()
		<-done
	}()
	<-hits
	time.Sleep(3 * time.Second)
	if n := passes(m, "deliveries"); n != 0 {
		t.Fatalf("%v passes in 3 s with no slot free, want none", n)
	}
	close(release)
	released = true
	hitWithin(t, hits, time.Now(), settle)
	if wakeups(m, "deliveries", "notify") < 1 {
		t.Fatal("the freed slot did not wake the loop")
	}
}
