package notify

import (
	"strings"
	"testing"
	"time"
)

func ev(typ string, payload map[string]any) Delivery {
	return Delivery{Event: Event{Type: typ, Payload: payload}, Watch: Watch{Name: "cli/cli"}}
}

// The server-side summaries read like the UI's (web/src/lib/events.ts).
func TestFormat(t *testing.T) {
	for _, tt := range []struct {
		d       Delivery
		summary string
		detail  string
		url     string
	}{
		{ev("release.published", map[string]any{"tag": "v2.104.0", "name": "GitHub CLI 2.104.0", "url": "https://github.com/cli/cli/releases/v2.104.0"}),
			"v2.104.0 published", "GitHub CLI 2.104.0", "https://github.com/cli/cli/releases/v2.104.0"},
		{ev("release.published", map[string]any{"tag": "v1", "name": "v1", "prerelease": true}), "v1 published (pre-release)", "", ""},
		{ev("release.promoted", map[string]any{"tag": "v1"}), "v1 promoted to release", "", ""},
		{ev("mr.state_changed", map[string]any{"iid": 12.0, "from": "opened", "to": "merged", "title": "Fix it"}), "MR !12 merged", "Fix it", ""},
		{ev("pr.state_changed", map[string]any{"number": 7.0, "from": "closed", "to": "open"}), "PR #7 reopened", "", ""},
		{ev("pr.state_changed", map[string]any{"number": 7.0, "from": "x", "to": "open"}), "PR #7 opened", "", ""},
		{ev("pr.draft_changed", map[string]any{"number": 7.0, "to": true}), "PR #7 converted to draft", "", ""},
		{ev("mr.draft_changed", map[string]any{"iid": 3.0, "to": false}), "MR !3 marked ready for review", "", ""},
		{ev("pr.review_changed", map[string]any{"number": 7.0, "to": "changes_requested"}), "PR #7 review: changes requested", "", ""},
		{ev("pr.review_changed", map[string]any{"number": 7.0, "to": "odd"}), "PR #7 review: odd", "", ""},
		{ev("pr.ci_changed", map[string]any{"number": 7.0, "to": "failure"}), "PR #7 CI failing", "", ""},
		{ev("pr.commented", map[string]any{"number": 7.0, "author": "bob", "title": "T", "excerpt": "Looks good.\nShip it", "url": "https://x.test/pr/7#issuecomment-1"}),
			"PR #7 commented by bob", "Looks good. Ship it", "https://x.test/pr/7#issuecomment-1"},
		{ev("mr.approval_changed", map[string]any{"iid": 3.0, "to": map[string]any{"approved": true, "count": 2.0}}), "MR !3 approved (2 approvals)", "", ""},
		{ev("mr.approval_changed", map[string]any{"iid": 3.0, "to": map[string]any{"approved": false, "count": 1.0}}), "MR !3 not approved (1 approval)", "", ""},
		{ev("mr.pipeline_changed", map[string]any{"iid": 3.0, "to": "success"}), "MR !3 pipeline success", "", ""},
		{ev("tag.created", map[string]any{"tag": "18.7", "highest": true}), "tag 18.7 created (new highest)", "", ""},
		{ev("tag.created", map[string]any{"tag": "17.9", "highest": false}), "tag 17.9 created", "", ""},
		{ev("tag.removed", map[string]any{"tag": "17.9"}), "tag 17.9 removed", "", ""},
		{ev("odd.type", map[string]any{"url": "javascript:alert(1)", "tag": []any{1}}), "odd.type", "", ""},
		{TestDelivery(time.Now()), "test notification", "If you read this, the channel works.", ""},
	} {
		m := Format(tt.d)
		if m.Summary != tt.summary || m.Detail != tt.detail || m.URL != tt.url || m.Title != tt.d.Watch.Name+": "+tt.summary {
			t.Errorf("%s: got %+v", tt.d.Event.Type, m)
		}
	}
	// Upstream text cannot break a header line.
	m := Format(ev("pr.state_changed", map[string]any{"number": 1.0, "to": "merged\r\nBcc: x@evil", "title": "a\nb"}))
	if strings.ContainsAny(m.Title+m.Detail, "\r\n") {
		t.Fatalf("control characters kept: %q %q", m.Title, m.Detail)
	}
}

func TestTelegramText(t *testing.T) {
	got := TelegramText(Message{Title: `a <b> & "c"`, Detail: "<script>", URL: `https://x.test/?a=1&b="2"`})
	want := `<b>a &lt;b&gt; &amp; &#34;c&#34;</b>` + "\n&lt;script&gt;\n" + `<a href="https://x.test/?a=1&amp;b=&#34;2&#34;">Open upstream</a>`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	long := TelegramText(Message{Title: strings.Repeat("<", 5000)})
	if len(long) > maxTelegramLen || !strings.HasPrefix(long, "<b>&lt;") || !strings.HasSuffix(long, "</b>") {
		t.Fatalf("long text: %d bytes", len(long))
	}
}

// 004-AC8: the notes and ids change no chat or mail message.
func TestFormatIgnoresNotes(t *testing.T) {
	base := map[string]any{"tag": "v1", "name": "One", "prerelease": false, "url": "https://x/v1"}
	with := map[string]any{"body": strings.Repeat("secret notes\n", 5000), "cves": []any{"CVE-2026-1"}}
	for k, v := range base {
		with[k] = v
	}
	for _, typ := range []string{"release.published", "release.promoted"} {
		got, want := Format(ev(typ, with)), Format(ev(typ, base))
		if got != want {
			t.Fatalf("%s: %+v, want %+v", typ, got, want)
		}
		if strings.Contains(got.Title+got.Summary+got.Detail+got.URL, "CVE") || strings.Contains(got.Title+got.Summary+got.Detail, "secret") {
			t.Fatalf("%s: message shows the notes: %+v", typ, got)
		}
	}
}
