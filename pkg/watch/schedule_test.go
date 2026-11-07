package watch

import (
	"testing"
	"time"
)

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 30, 0, 0, time.FixedZone("CET", 3600))
	for _, tt := range []struct {
		schedule string
		jitter   float64
		next     time.Time
		interval time.Duration
	}{
		{"15m", 0, now.Add(15 * time.Minute), 15 * time.Minute},
		{"10m", 1, now.Add(11 * time.Minute), 10 * time.Minute},
		{"10m", -1, now.Add(9 * time.Minute), 10 * time.Minute},
		// Cron reads in UTC: 10:30 CET is 09:30 UTC, so the next 09:00 UTC is tomorrow's.
		{"0 9 * * *", 1, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC), 24 * time.Hour},
		{"*/5 * * * *", 0, time.Date(2026, 11, 4, 9, 35, 0, 0, time.UTC), 5 * time.Minute},
	} {
		next, interval, err := NextRun(tt.schedule, now, tt.jitter)
		if err != nil || !next.Equal(tt.next) || interval != tt.interval {
			t.Errorf("%s jitter %v: got %v %v %v", tt.schedule, tt.jitter, next, interval, err)
		}
	}
	for _, bad := range []string{"-1m", "0s", "61 * * * *", "nope"} {
		if _, _, err := NextRun(bad, now, 0); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDefaultScheduleFor(t *testing.T) {
	expected := map[string]string{
		KindGitHubRepo: "1h", KindGitHubPR: "1h", KindGitHubIssue: "1h", KindGitHubDiscussion: "1h",
		KindGitLabRelease: "15m", KindGitLabMR: "15m", KindOCITags: "15m",
	}
	// A new kind must pick its default here, not inherit 15m silently.
	for _, kind := range Kinds {
		want, ok := expected[kind]
		if !ok {
			t.Fatalf("%s: no expected default schedule", kind)
		}
		if got := DefaultScheduleFor(kind); got != want {
			t.Errorf("%s: got %q, want %q", kind, got, want)
		}
	}
}
