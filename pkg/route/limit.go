package route

import (
	"sync"
	"time"
)

// Previews per user: the live editor sends one per pause in typing; a script cannot loop on them.
const (
	previewRate  = 2.0
	previewBurst = 5.0
	maxBuckets   = 10_000
)

// limiter is a token bucket per key, in this process only.
// ponytail: per replica, so N replicas allow N times the rate; a shared limit would need PG.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		// Bounded memory: past the cap, every bucket restarts full.
		if len(l.buckets) >= maxBuckets {
			l.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
