package ratelimit

import (
	"strings"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(2, 5)
	l.now = func() time.Time { return now }
	for i := range 5 {
		if !l.Allow("u") {
			t.Fatalf("burst refused at %d", i)
		}
	}
	if l.Allow("u") || !l.Allow("other") {
		t.Fatal("bucket not per key or not empty after the burst")
	}
	now = now.Add(500 * time.Millisecond)
	if !l.Allow("u") || l.Allow("u") {
		t.Fatal("refill is not 2/s")
	}
	for i := range maxBuckets {
		l.Allow(strings.Repeat("k", i%50) + string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if len(l.buckets) > maxBuckets {
		t.Fatalf("%d buckets kept", len(l.buckets))
	}
}
