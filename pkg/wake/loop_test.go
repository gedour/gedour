package wake

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestLoop paces a loop with a 400 ms poll (minPass 400 ms, jitter < 100 ms) whose last pass is long gone.
func newTestLoop(sub <-chan struct{}) (*Loop, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	l := NewLoop("t", 400*time.Millisecond, sub, m)
	l.last = time.Now().Add(-time.Hour)
	return l, m
}

// timed runs one Wait and returns how long it took.
func timed(t *testing.T, l *Loop, deadline time.Time, failed bool) time.Duration {
	t.Helper()
	start := time.Now()
	if !l.Wait(context.Background(), deadline, failed) {
		t.Fatal("Wait returned false on a live context")
	}
	return time.Since(start)
}

func within(t *testing.T, what string, got, lo, hi time.Duration) {
	t.Helper()
	if got < lo || got > hi {
		t.Errorf("%s: waited %s, want %s to %s", what, got, lo, hi)
	}
}

func wakeups(m *Metrics, reason string) float64 {
	return testutil.ToFloat64(m.Wakeups.WithLabelValues("t", reason))
}

const slack = 150 * time.Millisecond

func TestLoopWakes(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		l, m := newTestLoop(nil)
		within(t, "deadline", timed(t, l, time.Now().Add(100*time.Millisecond), false), 100*time.Millisecond, 200*time.Millisecond+slack)
		if wakeups(m, "deadline") != 1 {
			t.Error("deadline wakeup not counted")
		}
	})
	t.Run("past deadline fires at once", func(t *testing.T) {
		l, m := newTestLoop(nil)
		within(t, "past deadline", timed(t, l, time.Now().Add(-time.Minute), false), 0, 100*time.Millisecond+slack)
		if wakeups(m, "deadline") != 1 {
			t.Error("deadline wakeup not counted")
		}
	})
	t.Run("poll without deadline, nil subscription", func(t *testing.T) {
		l, m := newTestLoop(nil)
		within(t, "poll", timed(t, l, time.Time{}, false), 400*time.Millisecond, 500*time.Millisecond+slack)
		if wakeups(m, "poll") != 1 {
			t.Error("poll wakeup not counted")
		}
	})
	t.Run("poll before a later deadline", func(t *testing.T) {
		l, m := newTestLoop(nil)
		within(t, "poll", timed(t, l, time.Now().Add(time.Hour), false), 400*time.Millisecond, 500*time.Millisecond+slack)
		if wakeups(m, "poll") != 1 || wakeups(m, "deadline") != 0 {
			t.Error("a deadline past the poll must count as poll")
		}
	})
	t.Run("notify and kick collapse", func(t *testing.T) {
		sub := make(chan struct{}, 1)
		l, m := newTestLoop(sub)
		sub <- struct{}{}
		l.Kick()
		l.Kick()
		within(t, "notify", timed(t, l, time.Time{}, false), 0, 100*time.Millisecond+slack)
		// Both signals were covered by the pass: the next wait sleeps to its deadline.
		l.last = time.Now().Add(-time.Hour)
		within(t, "after collapse", timed(t, l, time.Now().Add(200*time.Millisecond), false), 200*time.Millisecond, 300*time.Millisecond+slack)
		if wakeups(m, "notify") != 1 || wakeups(m, "deadline") != 1 {
			t.Errorf("notify %v, deadline %v: want 1 each", wakeups(m, "notify"), wakeups(m, "deadline"))
		}
	})
	t.Run("minPass between passes", func(t *testing.T) {
		sub := make(chan struct{}, 1)
		l, _ := newTestLoop(sub)
		timed(t, l, time.Now().Add(-time.Minute), false)
		start := time.Now()
		sub <- struct{}{}
		timed(t, l, time.Now().Add(-time.Minute), false)
		within(t, "second pass", time.Since(start), 400*time.Millisecond-10*time.Millisecond, 500*time.Millisecond+slack)
	})
}

func TestLoopRetryBackoff(t *testing.T) {
	l, m := newTestLoop(nil)
	l.minPass, l.jitter, l.poll = 50*time.Millisecond, 0, 300*time.Millisecond
	for i, want := range []time.Duration{50, 100, 200, 300, 300} {
		want *= time.Millisecond
		within(t, "retry", timed(t, l, time.Time{}, true), want-5*time.Millisecond, want+slack)
		if wakeups(m, "retry") != float64(i+1) {
			t.Fatalf("retry %d not counted", i+1)
		}
	}
	// A pass that did not fail resets the backoff.
	timed(t, l, time.Now(), false)
	within(t, "retry after reset", timed(t, l, time.Time{}, true), 45*time.Millisecond, 50*time.Millisecond+slack)
}

func TestLoopStopsOnCancel(t *testing.T) {
	for _, failed := range []bool{false, true} {
		l, _ := newTestLoop(make(chan struct{}))
		l.poll = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		// start before arming the timer, or the 50 ms floor sees less than 50 ms.
		start := time.Now()
		time.AfterFunc(50*time.Millisecond, cancel)
		if l.Wait(ctx, time.Now().Add(time.Hour), failed) {
			t.Fatal("Wait returned true on a cancelled context")
		}
		within(t, "cancel", time.Since(start), 50*time.Millisecond, 50*time.Millisecond+slack)
	}
}
