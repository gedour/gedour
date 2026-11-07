package watch

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	minInterval = time.Minute
	maxInterval = 7 * 24 * time.Hour
	// defaultSchedule applies when a watch is created without one; GitHub kinds use githubSchedule.
	defaultSchedule = "15m"
	// Unauthenticated GitHub allows 60 requests an hour per address: a few 15m watches would exhaust it.
	githubSchedule = "1h"
)

// DefaultScheduleFor returns the schedule a new watch of kind gets when none is given.
func DefaultScheduleFor(kind string) string {
	if kind == KindGitHubRepo || kind == KindGitHubPR || kind == KindGitHubIssue || kind == KindGitHubDiscussion {
		return githubSchedule
	}
	return defaultSchedule
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// parseSchedule accepts a Go duration between 1m and 7d, or a 5-field cron expression, and returns it
// trimmed; the message explains a rejection.
func parseSchedule(s string) (string, string) {
	s = strings.Join(strings.Fields(s), " ")
	if d, err := time.ParseDuration(s); err == nil {
		if d < minInterval || d > maxInterval {
			return "", "interval must be between 1m and 168h (7 days)"
		}
		return s, ""
	}
	// Exactly 5 fields: no descriptors (@daily), seconds or CRON_TZ prefix, so every schedule reads the same way.
	if len(strings.Fields(s)) != 5 {
		return "", `expected a duration like "15m" or a 5-field cron expression like "0 9 * * 1-5"`
	}
	if _, err := cronParser.Parse(s); err != nil {
		return "", "invalid cron expression: " + err.Error()
	}
	return s, ""
}

// NextRun returns when a watch on schedule is next due after now, and its nominal interval (the gap
// between two cron activations). jitter in [-1, 1] spreads interval schedules by up to ±10 %; cron
// schedules fire on their activation, in UTC.
func NextRun(schedule string, now time.Time, jitter float64) (time.Time, time.Duration, error) {
	if d, err := time.ParseDuration(schedule); err == nil {
		if d <= 0 {
			return time.Time{}, 0, errors.New("schedule: non-positive interval")
		}
		return now.Add(d + time.Duration(float64(d)*0.1*jitter)), d, nil
	}
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("schedule: %w", err)
	}
	next := sched.Next(now.UTC())
	return next, sched.Next(next).Sub(next), nil
}
