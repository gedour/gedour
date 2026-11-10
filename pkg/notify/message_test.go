package notify

import (
	"encoding/json"
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
		// 008-AC12: the two new-item events, mirrored in web/src/lib/events.ts.
		{ev("issue.opened", map[string]any{"number": 12.0, "author": "alice", "title": "Crash on start", "labels": []any{"bug"}, "url": "https://github.com/o/r/issues/12"}),
			"Issue #12 opened by alice", "Crash on start", "https://github.com/o/r/issues/12"},
		{ev("pr.opened", map[string]any{"number": 12.0, "author": "alice", "title": "Fix the crash", "labels": []any{}, "url": "https://github.com/o/r/pull/12"}),
			"PR #12 opened by alice", "Fix the crash", "https://github.com/o/r/pull/12"},
		// 011-AC16: discussion.opened, mirrored in web/src/lib/events.ts.
		{ev("discussion.opened", map[string]any{"number": 12.0, "author": "alice", "title": "Ideas welcome", "category": "Ideas", "labels": []any{}, "url": "https://github.com/o/r/discussions/12"}),
			"Discussion #12 opened by alice", "Ideas welcome", "https://github.com/o/r/discussions/12"},
		// 011-AC16: the github_discussion events, mirrored in web/src/lib/events.ts.
		{ev("discussion.state_changed", map[string]any{"number": 12.0, "title": "T", "from": "open", "to": "closed", "state_reason": "resolved"}), "Discussion #12 closed (resolved)", "T", ""},
		{ev("discussion.state_changed", map[string]any{"number": 12.0, "from": "closed", "to": "closed", "state_reason": "outdated"}), "Discussion #12 closed (outdated)", "", ""},
		{ev("discussion.state_changed", map[string]any{"number": 12.0, "from": "closed", "to": "open", "state_reason": "reopened"}), "Discussion #12 reopened", "", ""},
		{ev("discussion.answer_changed", map[string]any{"number": 12.0, "from": "", "to": "https://github.com/o/r/discussions/12#discussioncomment-1", "chosen_by": "bob"}), "Answer chosen on discussion #12", "", ""},
		{ev("discussion.answer_changed", map[string]any{"number": 12.0, "from": "https://github.com/o/r/discussions/12#discussioncomment-1", "to": "", "chosen_by": ""}), "Answer removed on discussion #12", "", ""},
		{ev("discussion.labels_changed", map[string]any{"number": 12.0, "from": []any{}, "to": []any{"question"}}), "Discussion #12 labels: question", "", ""},
		{ev("discussion.commented", map[string]any{"number": 12.0, "author": "alice", "title": "T", "excerpt": "Try this.", "url": "https://github.com/o/r/discussions/12#discussioncomment-1"}),
			"Discussion #12 commented by alice", "Try this.", "https://github.com/o/r/discussions/12#discussioncomment-1"},
		{ev("discussion.commented", map[string]any{"number": 12.0, "author": "bob", "title": "T", "excerpt": "Thanks!", "url": "https://github.com/o/r/discussions/12#discussioncomment-2",
			"reply_to": "https://github.com/o/r/discussions/12#discussioncomment-1"}),
			"Discussion #12 reply by bob", "Thanks!", "https://github.com/o/r/discussions/12#discussioncomment-2"},
		// 008-AC12: the github_issue events, mirrored in web/src/lib/events.ts.
		{ev("issue.state_changed", map[string]any{"number": 12.0, "title": "T", "from": "open", "to": "closed", "state_reason": "not_planned"}), "Issue #12 closed (not planned)", "T", ""},
		{ev("issue.state_changed", map[string]any{"number": 12.0, "from": "open", "to": "closed", "state_reason": "completed"}), "Issue #12 closed (completed)", "", ""},
		{ev("issue.state_changed", map[string]any{"number": 12.0, "from": "open", "to": "closed", "state_reason": ""}), "Issue #12 closed", "", ""},
		{ev("issue.state_changed", map[string]any{"number": 12.0, "from": "closed", "to": "open", "state_reason": "reopened"}), "Issue #12 reopened", "", ""},
		{ev("issue.state_changed", map[string]any{"number": 12.0, "from": "closed", "to": "closed", "state_reason": "duplicate"}), "Issue #12 closed (duplicate)", "", ""},
		{ev("issue.labels_changed", map[string]any{"number": 12.0, "from": []any{"bug"}, "to": []any{"bug", "triage"}}), "Issue #12 labels: bug, triage", "", ""},
		{ev("issue.labels_changed", map[string]any{"number": 12.0, "from": []any{"bug"}, "to": []any{}}), "Issue #12 labels: none", "", ""},
		{ev("issue.commented", map[string]any{"number": 12.0, "author": "alice", "title": "T", "excerpt": "Same here.\nOn 1.2", "url": "https://github.com/o/r/issues/12#issuecomment-1"}),
			"Issue #12 commented by alice", "Same here. On 1.2", "https://github.com/o/r/issues/12#issuecomment-1"},
		// 011-AC16: the reacted events, rises in GitHub's order, mirrored in web/src/lib/events.ts.
		{ev("issue.reacted", map[string]any{"number": 12.0, "title": "T", "target": "item", "added": map[string]any{"heart": 2.0}, "url": "https://github.com/o/r/issues/12"}),
			"+2 ❤️ on issue #12", "T", "https://github.com/o/r/issues/12"},
		{ev("pr.reacted", map[string]any{"number": 7.0, "target": "comment", "added": map[string]any{"+1": 1.0}}), "+1 👍 on a comment of PR #7", "", ""},
		{ev("discussion.reacted", map[string]any{"number": 3.0, "target": "item", "added": map[string]any{"rocket": 1.0, "heart": 2.0, "eyes": 0.0}}),
			"+2 ❤️ +1 🚀 on discussion #3", "", ""},
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

// 013-AC1, 013-AC11: a live test prefixes the summary, so every title shows it; a real delivery has no prefix.
func TestFormatTestMarker(t *testing.T) {
	for _, tt := range []struct {
		test           bool
		summary, title string
	}{
		{false, "v1.2.3 published", "cli/cli: v1.2.3 published"},
		{true, "[test] v1.2.3 published", "cli/cli: [test] v1.2.3 published"},
	} {
		d := ev("release.published", map[string]any{"tag": "v1.2.3"})
		d.Test = tt.test
		m := Format(d)
		if m.Summary != tt.summary || m.Title != tt.title {
			t.Errorf("test %v: summary %q title %q", tt.test, m.Summary, m.Title)
		}
		var push PushMessage
		_ = json.Unmarshal(PushPayload(d, false), &push)
		if push.Title != tt.title {
			t.Errorf("test %v: push title %q", tt.test, push.Title)
		}
	}
}
