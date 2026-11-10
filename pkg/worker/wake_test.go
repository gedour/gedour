package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/wake"
	"github.com/gedour/gedour/pkg/watch"
)

// settle is past minPass plus the jitter: a wake after it is not held back by the previous pass.
const settle = 1300 * time.Millisecond

// replica is one worker process: a worker on the real clock and, unless listen is false, its listener.
type replica struct {
	w   *Worker
	m   *wake.Metrics
	ran chan time.Time
	f   *fake
	// stop cancels the replica and waits for it to return.
	stop func()
}

// startReplica runs a replica on pool until the test ends; its collector reports each run start on ran, then runs hold
// (nil: none). tune adjusts the options last.
func (e *env) startReplica(t *testing.T, pool *pgxpool.Pool, poll time.Duration, listen bool, hold func(ctx context.Context) error, tune ...func(*Options)) *replica {
	t.Helper()
	r := &replica{m: wake.NewMetrics(prometheus.NewRegistry()), ran: make(chan time.Time, 100)}
	r.f = newFake(func(ctx context.Context, _ string, _ *collect.Credential) (json.RawMessage, error) {
		r.ran <- time.Now()
		if hold != nil {
			if err := hold(ctx); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(`{}`), nil
	})
	var l *wake.Listener
	o := Options{Poll: poll, Concurrency: 4, DrainTimeout: time.Second, UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}, Wakeups: r.m}
	if listen {
		l = wake.NewListener(pool, zap.NewNop())
		o.Wake = l.Subscribe("watches")
	}
	for _, f := range tune {
		f(&o)
	}
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	w, err := New(pool, zap.NewNop(), e.box, g.Client(netguard.Options{Timeout: 5 * time.Second}), map[string]collect.Collector{"github_repo": r.f}, e.m, o)
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	if l != nil {
		go func() { l.Run(ctx); done <- struct{}{} }()
	}
	go func() { w.Run(ctx); done <- struct{}{} }()
	var once sync.Once
	r.stop = func() {
		once.Do(func() {
			cancel()
			<-done
			if l != nil {
				<-done
			}
		})
	}
	t.Cleanup(r.stop)
	return r
}

func (r *replica) wakeups(reason string) float64 {
	return testutil.ToFloat64(r.m.Wakeups.WithLabelValues("watches", reason))
}

func (r *replica) passes() float64 {
	n := 0.0
	for _, reason := range []string{"deadline", "notify", "poll", "retry"} {
		n += r.wakeups(reason)
	}
	return n
}

// idle waits for the wake-all of the first LISTEN, then for the pass it caused to be well over.
func (r *replica) idle(t *testing.T) {
	t.Helper()
	waitUntil(t, "the first wake-all", 5*time.Second, func() bool { return r.wakeups("notify") >= 1 })
	time.Sleep(settle)
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

// later moves every watch an hour ahead: nothing due, no deadline within a short poll.
func (e *env) later(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
}

// runNow calls the API's run-now (POST /api/v1/watches/{id}/run) as the watch's owner, and returns when it committed.
func (e *env) runNow(t *testing.T, id string) time.Time {
	t.Helper()
	_, api := humatest.New(t)
	watch.New(e.pool, zap.NewNop(), e.box, nil).Register(api, func(context.Context) (string, bool) { return e.user, true })
	if resp := api.Post("/api/v1/watches/" + id + "/run"); resp.Code != http.StatusAccepted {
		t.Fatalf("run-now: %d %s", resp.Code, resp.Body)
	}
	return time.Now()
}

// until holds a run until release closes, or until the shutdown drain cancels it: a failing test must not hang.
func until(release <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func ranWithin(t *testing.T, r *replica, from time.Time, limit time.Duration) time.Duration {
	t.Helper()
	select {
	case at := <-r.ran:
		if took := at.Sub(from); took > limit {
			t.Fatalf("claimed %s after the request, want within %s", took, limit)
		}
		return at.Sub(from)
	case <-time.After(5 * time.Second):
		t.Fatal("never claimed")
	}
	return 0
}

// 015-AC2: a run-now wakes a worker asleep on a 60 s safety poll at once.
func TestRunNowWakesWorker(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_repo", `{}`, "1h")
	e.later(t)
	r := e.startReplica(t, e.pool, time.Minute, true, nil)
	r.idle(t)
	t.Logf("claimed after %s", ranWithin(t, r, e.runNow(t, id), time.Second))
}

// 015-AC3: with no notification, a watch due in 3 s is claimed between 3 and 4 s from now, by its deadline.
func TestDeadlineWakesWorker(t *testing.T) {
	e := newEnv(t)
	due := time.Now().Add(3 * time.Second)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO watches (owner_id, name, kind, config, schedule, next_run_at)
		VALUES ($1, 'w', 'github_repo', '{}', '1h', $2)`, e.user, due); err != nil {
		t.Fatal(err)
	}
	from := due.Add(-3 * time.Second)
	r := e.startReplica(t, e.pool, time.Minute, false, nil)
	took := ranWithin(t, r, from, 4*time.Second)
	if took < 3*time.Second {
		t.Fatalf("claimed %s from now, before it was due", took)
	}
	if r.wakeups("deadline") != 1 {
		t.Fatalf("deadline wakeups %v, want 1", r.wakeups("deadline"))
	}
}

// 015-AC6: two replicas, a run-now: one run, and both replicas wake on the notification.
func TestRunNowWakesEveryReplica(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_repo", `{}`, "1h")
	e.later(t)
	a := e.startReplica(t, connect(t, e.url), time.Minute, true, nil)
	b := e.startReplica(t, connect(t, e.url), time.Minute, true, nil)
	a.idle(t)
	b.idle(t)
	na, nb := a.wakeups("notify"), b.wakeups("notify")
	e.runNow(t, id)
	waitUntil(t, "the run", 3*time.Second, func() bool {
		return e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND last_status = 'ok'`, id) == 1
	})
	waitUntil(t, "both notify wakeups", 3*time.Second, func() bool { return a.wakeups("notify") > na && b.wakeups("notify") > nb })
	// Past any second claim attempt the notifications caused.
	time.Sleep(settle)
	if runs := a.f.total()[`{}`] + b.f.total()[`{}`]; runs != 1 {
		t.Fatalf("%d runs, want 1", runs)
	}
	if n := e.count(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, id); n != 1 {
		t.Fatalf("%d states, want 1", n)
	}
}

// 015-AC8: with LISTEN failing, the loss shows in the log and a watch made due is still claimed by the safety poll.
func TestPollWithoutListener(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_repo", `{}`, "1h")
	e.later(t)
	closed := connect(t, e.url)
	closed.Close()
	core, logs := observer.New(zap.WarnLevel)
	l := wake.NewListener(closed, zap.New(core))
	r := e.startReplica(t, e.pool, time.Second, false, nil, func(o *Options) { o.Wake = l.Subscribe("watches") })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, "work_listen_failed", 5*time.Second, func() bool { return logs.FilterMessage("work_listen_failed").Len() > 0 })
	time.Sleep(settle)
	before := r.wakeups("poll")
	// A poll period, the jitter and some slack.
	ranWithin(t, r, e.runNow(t, id), 2*time.Second)
	if r.wakeups("poll") <= before {
		t.Fatal("claimed without a poll wakeup")
	}
	if r.wakeups("notify") != 0 {
		t.Fatalf("%v notify wakeups with LISTEN down", r.wakeups("notify"))
	}
}

// With every slot taken, a watch already due does not wake the loop each second: the run's end does.
func TestSaturatedLoopSleeps(t *testing.T) {
	e := newEnv(t)
	e.addWatch(t, "github_repo", `{"n": 1}`, "1h")
	e.addWatch(t, "github_repo", `{"n": 2}`, "1h")
	release := make(chan struct{})
	r := e.startReplica(t, e.pool, 10*time.Second, false, until(release),
		func(o *Options) { o.Concurrency = 1 })
	<-r.ran
	time.Sleep(3 * time.Second)
	if n := r.passes(); n != 0 {
		t.Fatalf("%v passes in 3 s with no slot free, want none", n)
	}
	close(release)
	ranWithin(t, r, time.Now(), settle)
	if r.wakeups("notify") < 1 {
		t.Fatal("the freed slot did not wake the loop")
	}
}

// gedour_watches_due is read after the pass's claims: only the due watches no free slot could take.
func TestDueGaugeAfterPass(t *testing.T) {
	for _, tt := range []struct {
		name        string
		concurrency int
		locked      bool
		want        float64
	}{{"slots free", 4, false, 0}, {"no slot free", 1, false, 1}, {"row locked", 4, true, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.addWatch(t, "github_repo", `{"n": 1}`, "1h")
			id := e.addWatch(t, "github_repo", `{"n": 2}`, "1h")
			if tt.locked {
				// SKIP LOCKED passes over a row an edit holds: still due, still counted.
				ctx := context.Background()
				tx, err := e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = tx.Rollback(ctx) })
				if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, id); err != nil {
					t.Fatal(err)
				}
			}
			// A sentinel: the gauge starts at 0, the value a pass that set nothing would show.
			e.m.due.Set(-1)
			release := make(chan struct{})
			e.startReplica(t, e.pool, 10*time.Second, false, until(release), func(o *Options) { o.Concurrency = tt.concurrency })
			t.Cleanup(func() { close(release) })
			waitUntil(t, "the first pass", 5*time.Second, func() bool { return testutil.ToFloat64(e.m.due) != -1 })
			if got := testutil.ToFloat64(e.m.due); got != tt.want {
				t.Fatalf("due gauge %v, want %v", got, tt.want)
			}
		})
	}
}

// 015-AC9: a due watch held under a row lock makes the loop pass at most once a second, not spin.
func TestLockedWatchDoesNotSpin(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_repo", `{}`, "1h")
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	r := e.startReplica(t, e.pool, 10*time.Second, false, nil)
	time.Sleep(3 * time.Second)
	if n := r.passes(); n > 4 {
		t.Fatalf("%v passes in 3 s, want at most 4", n)
	}
	if len(r.ran) != 0 {
		t.Fatal("claimed a locked watch")
	}
	_ = tx.Rollback(ctx)
	ranWithin(t, r, time.Now(), settle)
}

// 015-AC10: a burst of 1000 notifications within a second makes the watch loop pass at most twice.
func TestNotifyBurstCollapses(t *testing.T) {
	e := newEnv(t)
	r := e.startReplica(t, e.pool, time.Minute, true, nil)
	r.idle(t)
	before := r.passes()
	start := time.Now()
	for range 1000 {
		if _, err := e.pool.Exec(context.Background(), `SELECT pg_notify('gedour_work', 'watches')`); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the burst took %s, the criterion needs it within 1 s", took)
	}
	time.Sleep(settle)
	if n := r.passes() - before; n > 2 {
		t.Fatalf("%v passes for the burst, want at most 2", n)
	}
}

// 015-AC13: asleep on a far deadline, a stopped worker returns at once once its in-flight run is drained.
func TestDrainWhileAsleep(t *testing.T) {
	e := newEnv(t)
	id := e.addWatch(t, "github_repo", `{}`, "1h")
	far := e.addWatch(t, "github_repo", `{"far": 1}`, "1h")
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = now() + interval '30 minutes' WHERE id = $1`, far); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	r := e.startReplica(t, e.pool, time.Hour, false, until(release))
	<-r.ran
	// The loop is asleep: the in-flight watch is leased, the other one due in 30 min, the poll an hour.
	time.Sleep(100 * time.Millisecond)
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	start := time.Now()
	r.stop()
	if took := time.Since(start); took < 150*time.Millisecond || took > time.Second {
		t.Fatalf("stopped after %s, want just after the 200 ms run", took)
	}
	if row := e.row(t, id); row.status == nil || *row.status != "ok" {
		t.Fatalf("in-flight run lost: %+v", row)
	}
}
