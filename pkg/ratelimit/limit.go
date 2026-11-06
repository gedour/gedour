// Package ratelimit is a token bucket per key, shared by the API's and /mcp's limits.
package ratelimit

import (
	"sync"
	"time"
)

const maxBuckets = 10_000

// Limiter is a token bucket per key, in this process only.
// ponytail: per replica, so N replicas allow N times the rate; a shared limit would need PG.
type Limiter struct {
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

// New returns a limiter allowing rate requests per second per key, up to burst at once.
func New(rate, burst float64) *Limiter {
	return &Limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow takes a token from key's bucket, or reports there is none left.
func (l *Limiter) Allow(key string) bool {
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
