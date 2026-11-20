// Package notify formats events into messages and sends them to Telegram, Nextcloud Talk, SMTP and webhooks.
package notify

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// Event is the routed event a delivery carries.
type Event struct {
	ID         int64          `json:"id"`
	Type       string         `json:"type"`
	OccurredAt *time.Time     `json:"occurred_at"`
	ObservedAt time.Time      `json:"observed_at"`
	Payload    map[string]any `json:"payload"`
}

// Watch is the event's watch.
type Watch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Rule is the rule that routed the delivery; nil for a channel test.
type Rule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Priority sets the Web Push urgency; out of JSON so webhook and script payloads keep their shape.
	Priority string `json:"-"`
}

// Delivery is one message to send.
type Delivery struct {
	ID    int64
	Event Event
	Watch Watch
	Rule  *Rule
	// Test marks a rule's live test: its summary reads "[test] ", webhook and script inputs carry test: true.
	Test bool
}

// testPrefix starts the summary of a live test: on a shared channel an unmarked message reads as a real event.
const testPrefix = "[test] "

// TestEventType marks the synthetic event of a channel test.
const TestEventType = "gedour.test"

// TestDelivery is what "Send test" sends: no real event, watch or rule behind it.
func TestDelivery(now time.Time) Delivery {
	return Delivery{
		Event: Event{Type: TestEventType, ObservedAt: now, Payload: map[string]any{"message": "If you read this, the channel works."}},
		Watch: Watch{Name: "gedour"},
	}
}

// Message is the human form of an event, shared by every channel type.
type Message struct {
	// Title is "<watch>: <summary>", the email subject and the Telegram headline.
	Title string
	// Summary is the sentence alone, e.g. "v2.104.0 published", "MR !12 merged".
	Summary string
	// Detail is an optional second line (PR title, release name).
	Detail string
	// URL is the upstream page, http(s) only.
	URL string
	// Gedour is the event's inbox deep link, set by the sender when the channel's gedour_link is on.
	Gedour string
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return fmt.Sprint(t)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

var review = map[string]string{"approved": "approved", "changes_requested": "changes requested", "review_required": "review required", "none": "no review"}
var ci = map[string]string{"success": "passing", "failure": "failing", "pending": "running", "none": "none"}

// transition reads opened→closed as "closed" and closed→open(ed) as "reopened".
func transition(from, to string) string {
	if (to == "open" || to == "opened") && (from == "closed" || from == "locked") {
		return "reopened"
	}
	if to == "open" {
		return "opened"
	}
	return to
}

// reactions maps GitHub's reaction kinds to their emoji, in GitHub's order.
var reactions = [...][2]string{{"+1", "👍"}, {"-1", "👎"}, {"laugh", "😄"}, {"hooray", "🎉"}, {"confused", "😕"}, {"heart", "❤️"}, {"rocket", "🚀"}, {"eyes", "👀"}}

// reacted reads a <kind>.reacted as "+2 ❤️ +1 🚀 on issue #12" or "+1 👍 on a comment of PR #7".
func reacted(e Event) string {
	added, _ := e.Payload["added"].(map[string]any)
	var parts []string
	for _, r := range reactions {
		if n, _ := added[r[0]].(float64); n > 0 {
			parts = append(parts, fmt.Sprintf("+%d %s", int(n), r[1]))
		}
	}
	on := map[string]string{"issue.reacted": "issue #", "pr.reacted": "PR #", "discussion.reacted": "discussion #"}[e.Type] + str(e.Payload["number"])
	if str(e.Payload["target"]) == "comment" {
		on = "a comment of " + on
	}
	return strings.Join(parts, " ") + " on " + on
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// summarize mirrors web/src/lib/events.ts summarizeEvent, so a message reads like the UI.
func summarize(e Event) string {
	p := e.Payload
	from, to := str(p["from"]), str(p["to"])
	pr, mr, issue := "PR #"+str(p["number"]), "MR !"+str(p["iid"]), "Issue #"+str(p["number"])
	subject := pr
	if strings.HasPrefix(e.Type, "mr.") {
		subject = mr
	}
	discussion := "Discussion #" + str(p["number"])
	workflow := path.Base(str(p["path"]))
	// Issue and discussion events share the state and labels wording.
	item := issue
	if strings.HasPrefix(e.Type, "discussion.") {
		item = discussion
	}
	switch e.Type {
	case "release.published":
		if b, _ := p["prerelease"].(bool); b {
			return str(p["tag"]) + " published (pre-release)"
		}
		return str(p["tag"]) + " published"
	case "release.promoted":
		return str(p["tag"]) + " promoted to release"
	case "pr.state_changed", "mr.state_changed":
		return subject + " " + transition(from, to)
	case "pr.draft_changed", "mr.draft_changed":
		if b, _ := p["to"].(bool); b {
			return subject + " converted to draft"
		}
		return subject + " marked ready for review"
	case "pr.review_changed":
		if r, ok := review[to]; ok {
			to = r
		}
		return pr + " review: " + to
	case "pr.ci_changed":
		if c, ok := ci[to]; ok {
			to = c
		}
		return pr + " CI " + to
	case "pr.commented":
		return pr + " commented by " + str(p["author"])
	case "issue.opened":
		return issue + " opened by " + str(p["author"])
	case "issue.state_changed", "discussion.state_changed":
		t := transition(from, to)
		if r := str(p["state_reason"]); t == "closed" && r != "" {
			t += " (" + strings.ReplaceAll(r, "_", " ") + ")"
		}
		return item + " " + t
	case "issue.labels_changed", "discussion.labels_changed":
		l, _ := p["to"].([]any)
		names := make([]string, 0, len(l))
		for _, n := range l {
			names = append(names, str(n))
		}
		if len(names) == 0 {
			return item + " labels: none"
		}
		return item + " labels: " + strings.Join(names, ", ")
	case "issue.commented":
		return issue + " commented by " + str(p["author"])
	case "discussion.commented":
		if str(p["reply_to"]) != "" {
			return discussion + " reply by " + str(p["author"])
		}
		return discussion + " commented by " + str(p["author"])
	case "discussion.answer_changed":
		if to == "" {
			return "Answer removed on discussion #" + str(p["number"])
		}
		return "Answer chosen on discussion #" + str(p["number"])
	case "pr.opened":
		return pr + " opened by " + str(p["author"])
	case "discussion.opened":
		return discussion + " opened by " + str(p["author"])
	case "issue.reacted", "pr.reacted", "discussion.reacted":
		return reacted(e)
	case "mr.approval_changed":
		a, _ := p["to"].(map[string]any)
		n, _ := a["count"].(float64)
		if ok, _ := a["approved"].(bool); ok {
			return fmt.Sprintf("%s approved (%s)", mr, plural(int(n), "approval"))
		}
		return fmt.Sprintf("%s not approved (%s)", mr, plural(int(n), "approval"))
	case "mr.pipeline_changed":
		return mr + " pipeline " + to
	case "tag.created":
		if b, _ := p["highest"].(bool); b {
			return "tag " + str(p["tag"]) + " created (new highest)"
		}
		return "tag " + str(p["tag"]) + " created"
	case "tag.removed":
		return "tag " + str(p["tag"]) + " removed"
	case "schedule.disabled":
		return "Scheduled workflow " + workflow + " disabled (" + disabledReason(str(p["state"])) + ")"
	case "schedule.enabled":
		return "Scheduled workflow " + workflow + " enabled"
	case "schedule.stale":
		n, _ := p["missed"].(float64)
		runs := plural(int(n), "run")
		// The collector stops counting at 1000.
		if n >= 1000 {
			runs = "1000+ runs"
		}
		// The oldest missed time: the count may start after last_alive_at, at the workflow's last edit.
		if since := when(p["expected_at"]); since != "" {
			return fmt.Sprintf("%s missed %s since %s", workflow, runs, since)
		}
		return fmt.Sprintf("%s missed %s", workflow, runs)
	case "schedule.recovered":
		return "Scheduled workflow " + workflow + " runs again"
	case "schedule.reenabled":
		return "Scheduled workflow " + workflow + " re-enabled after " + disabledReason(from)
	case "schedule.disable_soon":
		// A warning turned on, or seen, after the cutoff: "turn off on" a past date would read wrong.
		if t, err := time.Parse(time.RFC3339, str(p["disable_at"])); err == nil && !e.ObservedAt.IsZero() && !t.After(e.ObservedAt) {
			return "Scheduled workflows of " + str(p["repo"]) + " are past the 60-day cutoff since " + when(p["disable_at"])
		}
		return "Scheduled workflows of " + str(p["repo"]) + " turn off on " + when(p["disable_at"]) + " without a commit"
	case TestEventType:
		return "test notification"
	}
	return e.Type
}

// disabledReason reads GitHub's disabled_inactivity as "inactivity".
func disabledReason(state string) string {
	return strings.ReplaceAll(strings.TrimPrefix(state, "disabled_"), "_", " ")
}

// when shows an RFC 3339 payload time in UTC to the minute; anything else as is.
func when(v any) string {
	t, err := time.Parse(time.RFC3339, str(v))
	if err != nil {
		return str(v)
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// Format builds the message of d; payload values are untrusted text, escaped by each channel.
func Format(d Delivery) Message {
	m := Message{Summary: summarize(d.Event)}
	if d.Test {
		m.Summary = testPrefix + m.Summary
	}
	m.Title = d.Watch.Name + ": " + m.Summary
	switch {
	case d.Event.Type == TestEventType:
		m.Detail = str(d.Event.Payload["message"])
	case d.Event.Type == "pr.commented" || d.Event.Type == "issue.commented" || d.Event.Type == "discussion.commented":
		m.Detail = str(d.Event.Payload["excerpt"])
	case str(d.Event.Payload["reenable_error"]) != "":
		m.Detail = "Re-enable failed: " + str(d.Event.Payload["reenable_error"])
	case str(d.Event.Payload["title"]) != "":
		m.Detail = str(d.Event.Payload["title"])
	case str(d.Event.Payload["name"]) != "" && str(d.Event.Payload["name"]) != str(d.Event.Payload["tag"]):
		m.Detail = str(d.Event.Payload["name"])
	}
	if u := str(d.Event.Payload["url"]); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		m.URL = u
	}
	// Headers and one-line channels: no control characters from the payload.
	m.Title = oneLine(m.Title)
	m.Detail = oneLine(m.Detail)
	return m
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
