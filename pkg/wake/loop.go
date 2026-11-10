package wake

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics count the passes of the worker loops, shared by every loop of a process.
type Metrics struct {
	Wakeups *prometheus.CounterVec
}

// NewMetrics registers the loop metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{Wakeups: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gedour_worker_wakeups_total",
		Help: "Passes of the worker loops (watches, router, deliveries) by cause: deadline, notify (a database notification or a freed slot), poll, retry after a failed pass.",
	}, []string{"loop", "reason"})}
	reg.MustRegister(m.Wakeups)
	return m
}

// Loop paces one worker loop: it sleeps until a deadline, a notification, a kick or the safety poll.
type Loop struct {
	name     string
	poll     time.Duration
	sub      <-chan struct{}
	kick     chan struct{}
	metrics  *Metrics
	minPass  time.Duration
	jitter   time.Duration
	failures int
	last     time.Time
}

// NewLoop paces the loop name; sub (nil: no listener) and m (nil: not counted) are optional.
func NewLoop(name string, poll time.Duration, sub <-chan struct{}, m *Metrics) *Loop {
	// Bounded by the poll, so a short test poll stays fast.
	return &Loop{name: name, poll: poll, sub: sub, kick: make(chan struct{}, 1), metrics: m,
		minPass: min(time.Second, poll), jitter: min(250*time.Millisecond, poll/4), last: time.Now()}
}

// Kick wakes the loop from inside the process, say when a slot frees up; it never blocks.
func (l *Loop) Kick() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Wait sleeps before the next pass: after a failed pass, minPass doubling up to the poll; otherwise until the
// deadline (zero: none), a notification or kick, or the poll. It returns false once ctx ends.
// Wait is not safe for concurrent use: one loop goroutine calls it.
func (l *Loop) Wait(ctx context.Context, deadline time.Time, failed bool) bool {
	reason := "poll"
	if failed {
		l.failures++
		reason = "retry"
		if !sleep(ctx, min(l.minPass<<min(l.failures-1, 30), l.poll)) {
			return false
		}
	} else {
		l.failures = 0
		wait := l.poll
		if !deadline.IsZero() {
			if d := time.Until(deadline); d < wait {
				wait, reason = max(d, 0), "deadline"
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-l.sub:
			reason = "notify"
		case <-l.kick:
			reason = "notify"
		case <-t.C:
		}
		t.Stop()
	}
	// A deadline already past but not claimable (a locked row) must not make the loop spin.
	gap := l.minPass - time.Since(l.last)
	var j time.Duration
	if l.jitter > 0 {
		j = rand.N(l.jitter) //nolint:gosec // jitter, not a secret
	}
	if !sleep(ctx, max(gap, 0)+j) {
		return false
	}
	// The pass about to start reads the database afresh: signals so far are covered by it.
	select {
	case <-l.sub:
	default:
	}
	select {
	case <-l.kick:
	default:
	}
	if l.metrics != nil {
		l.metrics.Wakeups.WithLabelValues(l.name, reason).Inc()
	}
	l.last = time.Now()
	return true
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
