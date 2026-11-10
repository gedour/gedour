// Package script runs user JavaScript (goja) in a sandbox subprocess: the same binary re-executed as
// `gedour sandbox`, with an empty environment, its own memory and file limits, and nothing but `log`
// and `http.fetch` to reach out. The parent talks to it over line-delimited JSON on stdin/stdout and
// performs every fetch itself, through the SSRF guard (001-AC17).
package script

import (
	"time"

	"github.com/gedour/gedour/pkg/notify"
)

// Subcommand is the argument that turns the binary into a sandbox child.
const Subcommand = "sandbox"

// Limits of one run. The wall clock is the parent's; the others are checked on both sides.
const (
	// Timeout is the wall clock of a run, fetches included (001-AC17).
	Timeout = 5 * time.Second
	// MaxSource is the largest script, in bytes.
	MaxSource = 64 << 10
	// MaxFetches is the number of http.fetch calls one run may make.
	MaxFetches   = 10
	maxLogLines  = 200
	maxLogBytes  = 16 << 10
	maxResult    = 16 << 10
	maxFetchBody = 256 << 10
	maxResponse  = 1 << 20
	fetchTimeout = 10 * time.Second
	// A fetch message holds a 256 KiB body JSON-escaped (up to 6 bytes per byte for control characters).
	maxMessage = 2 << 20
	maxOutput  = 8 << 20
	killGrace  = 500 * time.Millisecond
	// The child stops itself this long after its own timeout, should the parent be gone.
	selfGrace = 2 * time.Second
)

// Outcomes of a run, the label of gedour_script_runs_total.
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeTimeout = "timeout"
	OutcomeKilled  = "killed"
)

// Event is what a script sees of an event.
type Event struct {
	Type       string         `json:"type"`
	OccurredAt *time.Time     `json:"occurred_at"`
	Payload    map[string]any `json:"payload"`
	Summary    string         `json:"summary"`
	URL        string         `json:"url"`
}

// Input is the data a run gets: `event`, `watch`, `rule`, `delivery_id` and `test`.
type Input struct {
	Event      Event        `json:"event"`
	Watch      notify.Watch `json:"watch"`
	Rule       *notify.Rule `json:"rule"`
	DeliveryID int64        `json:"delivery_id"`
	// Test is true on a rule's live test, absent otherwise.
	Test bool `json:"test,omitempty"`
}

// InputOf is the input of a run for delivery d; summary and URL read as in every other channel.
func InputOf(d notify.Delivery) Input {
	m := notify.Format(d)
	at := d.Event.OccurredAt
	if at == nil {
		at = &d.Event.ObservedAt
	}
	payload := d.Event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return Input{
		Event: Event{Type: d.Event.Type, OccurredAt: at, Payload: payload, Summary: m.Summary, URL: m.URL},
		Watch: d.Watch, Rule: d.Rule, DeliveryID: d.ID, Test: d.Test,
	}
}

// FetchRequest is one http.fetch call.
type FetchRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// FetchResponse is what http.fetch returns to the script.
type FetchResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Text    string            `json:"text"`
}

// message is one line of the parent/child channel; t tells which fields are set.
//
//	parent → child: run (source, input, timeout_ms), fetch (res or error), interrupt
//	child → parent: log (line), fetch (req), done (outcome, error, result)
type message struct {
	T         string         `json:"t"`
	Source    string         `json:"source,omitempty"`
	Input     *Input         `json:"input,omitempty"`
	TimeoutMS int64          `json:"timeout_ms,omitempty"`
	Line      string         `json:"line,omitempty"`
	Req       *FetchRequest  `json:"req,omitempty"`
	Res       *FetchResponse `json:"res,omitempty"`
	Error     string         `json:"error,omitempty"`
	Outcome   string         `json:"outcome,omitempty"`
	Result    string         `json:"result,omitempty"`
}
