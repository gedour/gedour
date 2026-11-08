package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/notify/webpush"
)

func digestOf(n int, tag func(i int) string) Digest {
	var dg Digest
	for i := range n {
		w := Watch{ID: fmt.Sprint(i % 3), Name: fmt.Sprintf("org/repo%d", i%3)}
		dg.Deliveries = append(dg.Deliveries, Delivery{ID: int64(i + 1), Watch: w,
			Event: Event{ID: int64(i + 1), Type: "release.published", Payload: map[string]any{"tag": tag(i), "url": fmt.Sprintf("https://x.test/r/%d", i)}}})
	}
	return dg
}

// 012-AC8: past the cap the digest is cut on a whole line and ends with "+N more" and the inbox link; N
// counts exactly the events left out.
func TestDigestCap(t *testing.T) {
	moreRe := regexp.MustCompile(`\+(\d+) more`)
	for _, tt := range []struct {
		name  string
		text  func(Digest, string) string
		limit int
		size  func(string) int
		item  *regexp.Regexp
		link  string
	}{
		{"telegram", TelegramDigest, maxTelegramLen, func(s string) int { return len(s) },
			regexp.MustCompile(`^• <a href="https://x\.test/r/\d+">v\d+-é+ published</a>$`), `<a href="https://g.test/inbox">open the inbox</a>`},
		{"talk", TalkDigest, maxTalkLen, utf8.RuneCountInString,
			regexp.MustCompile(`^- \[v\d+-é+ published\]\(https://x\.test/r/\d+\)$`), `[open the inbox](https://g.test/inbox)`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const n = 400
			// Item lengths sweep the room left at the cut, so the "+N more" line must have been reserved.
			for pad := 200; pad < 260; pad++ {
				text := tt.text(digestOf(n, func(i int) string { return fmt.Sprintf("v%d-%s", i, strings.Repeat("é", pad)) }), "https://g.test/inbox")
				if size := tt.size(text); size > tt.limit {
					t.Fatalf("pad %d: %d over the cap %d", pad, size, tt.limit)
				}
				lines := strings.Split(text, "\n")
				last := lines[len(lines)-1]
				m := moreRe.FindStringSubmatch(last)
				if m == nil || !strings.HasSuffix(last, tt.link) {
					t.Fatalf("pad %d: last line %q", pad, last)
				}
				listed := 0
				for _, l := range lines[1 : len(lines)-1] {
					switch {
					case tt.item.MatchString(l):
						listed++
					case l == "" || strings.Contains(l, "org/repo"):
					default:
						t.Fatalf("pad %d: cut line %q", pad, l)
					}
				}
				if prev := lines[len(lines)-2]; !tt.item.MatchString(prev) {
					t.Fatalf("pad %d: a watch heading without an event before the end: %q", pad, prev)
				}
				if fmt.Sprint(n-listed) != m[1] || listed == 0 {
					t.Fatalf("pad %d: listed %d, %q says %s more", pad, listed, last, m[1])
				}
			}
			short := tt.text(digestOf(3, func(i int) string { return fmt.Sprint("v", i) }), "https://g.test/inbox")
			if strings.Contains(short, "more") {
				t.Errorf("a short digest is cut: %q", short)
			}
		})
	}
	if got := TelegramDigest(digestOf(400, func(i int) string { return strings.Repeat("é", 300) }), ""); !strings.HasSuffix(got, "more in the gedour inbox") {
		t.Errorf("no public URL: %q", got[len(got)-60:])
	}
}

// 012-AC3: events grouped by watch in the order of their oldest event, upstream text escaped, one line each.
func TestDigestFormat(t *testing.T) {
	dg := digestOf(4, func(i int) string { return fmt.Sprintf("v%d<b>\nx", i) })
	want := "<b>4 unread events</b>\n\n<b>org/repo0</b>\n" +
		"• <a href=\"https://x.test/r/0\">v0&lt;b&gt; x published</a>\n• <a href=\"https://x.test/r/3\">v3&lt;b&gt; x published</a>\n\n" +
		"<b>org/repo1</b>\n• <a href=\"https://x.test/r/1\">v1&lt;b&gt; x published</a>\n\n" +
		"<b>org/repo2</b>\n• <a href=\"https://x.test/r/2\">v2&lt;b&gt; x published</a>"
	if got := TelegramDigest(dg, ""); got != want {
		t.Errorf("telegram:\n%s\nwant:\n%s", got, want)
	}
	if got := TalkDigest(dg, ""); !strings.HasPrefix(got, "**4 unread events**\n\n**org/repo0**\n- [v0\\<b\\> x published](https://x.test/r/0)\n") {
		t.Errorf("talk: %q", got)
	}
	m := MailDigest(dg, "https://g.test/inbox")
	if m.Title != "gedour: 4 unread events" || !strings.HasPrefix(m.Detail, "org/repo0\r\n- v0<b> x published: https://x.test/r/0\r\n") ||
		!strings.HasSuffix(m.Detail, "\r\n\r\nInbox: https://g.test/inbox") {
		t.Errorf("mail: %+v", m)
	}
	raw, err := BuildMail(from, []string{"a@x.test"}, m, time.Now())
	if err != nil || !strings.Contains(string(raw), "<br>") {
		t.Errorf("mail html: %v", err)
	}
	if s := sampleDigest(time.Now()); len(s.Deliveries) != 2 || !strings.Contains(TelegramDigest(s, ""), "2 unread events") {
		t.Errorf("sample: %q", TelegramDigest(s, ""))
	}
}

// 012-AC12: the Web Push digest is the count, the watch names in order and the inbox, within one record.
func TestPushDigestPayload(t *testing.T) {
	var m PushMessage
	if err := json.Unmarshal(PushDigestPayload(digestOf(5, func(i int) string { return fmt.Sprint(i) }), "c1", true), &m); err != nil {
		t.Fatal(err)
	}
	if m != (PushMessage{Title: "5 unread events", Body: "org/repo0, org/repo1, org/repo2", URL: "/inbox", Tag: "gedour-digest-c1", Silent: true}) {
		t.Errorf("%+v", m)
	}
	var many Digest
	for i := range 500 {
		many.Deliveries = append(many.Deliveries, Delivery{Watch: Watch{ID: fmt.Sprint(i), Name: strings.Repeat("w", 50)}, Event: Event{Type: "tag.created"}})
	}
	if b := PushDigestPayload(many, "c1", false); len(b) > webpush.MaxPayload || !json.Valid(b) {
		t.Errorf("%d bytes", len(b))
	}
}

// 012-AC8: the mail digest is cut at maxMailEvents events, and at maxMailBytes, ending with "+N more".
func TestMailDigestCap(t *testing.T) {
	short := func(i int) string { return fmt.Sprint("v", i) }
	m := MailDigest(digestOf(maxMailEvents+5, short), "https://g.test/inbox")
	if n := strings.Count(m.Detail, "\r\n- "); n != maxMailEvents || !strings.Contains(m.Detail, "\r\n+5 more: https://g.test/inbox") {
		t.Errorf("%d event lines, tail %q", n, m.Detail[len(m.Detail)-80:])
	}
	if m.Title != fmt.Sprintf("gedour: %d unread events", maxMailEvents+5) {
		t.Errorf("subject %q", m.Title)
	}
	big := MailDigest(digestOf(600, func(i int) string { return fmt.Sprintf("v%d-%s", i, strings.Repeat("x", 1000)) }), "")
	if len(big.Detail) > maxMailBytes || !strings.Contains(big.Detail, "more in the gedour inbox") {
		t.Errorf("%d bytes, over %d or not cut", len(big.Detail), maxMailBytes)
	}
	if all := MailDigest(digestOf(maxMailEvents, short), ""); strings.Contains(all.Detail, "more") {
		t.Error("a digest at the cap is cut")
	}
}

// 012-AC3, 012-AC11: SendDigest sends one Talk message and one mail; a type without a digest form is a permanent refusal.
func TestSendDigest(t *testing.T) {
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	s := newSender(t, "127.0.0.0/8")
	ch := talkChannel(srv.URL, "abcd", true)
	ch.Secret = testupstream.TalkSecret
	dg := sampleDigest(time.Now())
	if err := s.SendDigest(context.Background(), ch, dg); err != nil {
		t.Fatal(err)
	}
	if got := up.Received(); len(got) != 1 || got[0].Endpoint != "talk-abcd" || !strings.Contains(got[0].Body, "2 unread events") || !strings.Contains(got[0].Body, `"silent":true`) {
		t.Fatalf("talk received %+v", got)
	}
	sink := newSink(t, nil, false, false)
	s.Mailer = &Mailer{Addr: sink.ln.Addr().String(), From: from, TLS: TLSNone}
	if err := s.SendDigest(context.Background(), Channel{Type: TypeEmail, Config: []byte(`{"to":["a@x.test"]}`)}, dg); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	data := sink.data
	sink.mu.Unlock()
	if !strings.Contains(data, "Subject: gedour: 2 unread events") {
		t.Errorf("mail: %s", data)
	}
	s.Mailer = nil
	if err := s.SendDigest(context.Background(), Channel{Type: TypeEmail, Config: []byte(`{"to":["a@x.test"]}`)}, dg); err == nil {
		t.Error("mail without SMTP sent")
	}
	if err := s.SendDigest(context.Background(), webhookChannel("https://x.test"), dg); !errors.Is(err, ErrPermanent) {
		t.Errorf("webhook digest: %v, want permanent", err)
	}
	for _, typ := range []string{TypeTelegram, TypeNextcloudTalk, TypeEmail} {
		if err := s.SendDigest(context.Background(), Channel{Type: typ, Config: []byte("[")}, dg); err == nil {
			t.Errorf("%s: bad config sent", typ)
		}
	}
}

// sampleDigest is two events, no real watch behind them.
func sampleDigest(now time.Time) Digest {
	w := Watch{Name: "gedour (sample)"}
	return Digest{Deliveries: []Delivery{
		{Event: Event{Type: "release.published", ObservedAt: now, Payload: map[string]any{"tag": "v1.0.0"}}, Watch: w},
		{Event: Event{Type: "pr.opened", ObservedAt: now, Payload: map[string]any{"number": 1.0, "author": "gedour"}}, Watch: w},
	}}
}
