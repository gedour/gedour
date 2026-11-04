package watch

import (
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	minInterval = time.Minute
	maxInterval = 7 * 24 * time.Hour
	// DefaultSchedule applies when a watch is created without one.
	DefaultSchedule = "15m"
)

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
