package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const inbox = "https://g.test/inbox"

// sent is the text a Telegram or Talk channel receives for d through s.
func sent(t *testing.T, s *Sender, ch Channel, d Delivery) string {
	t.Helper()
	srv, got := receiver(t, http.StatusOK)
	s.TelegramURL = srv.URL
	if ch.Type == TypeNextcloudTalk {
		srv, got = receiver(t, http.StatusCreated)
		var c NextcloudTalkConfig
		_ = json.Unmarshal(ch.Config, &c)
		c.URL = srv.URL
		ch.Config, _ = json.Marshal(c)
	}
	if err := s.Send(context.Background(), ch, d); err != nil {
		t.Fatal(err)
	}
	var body struct{ Text, Message string }
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Text + body.Message
}

// 017-AC12, 017-AC14: a single message ends with "Open in gedour" to the event's deep link when gedour_link is
// absent or true and a public URL is set; with gedour_link false or no public URL it is byte-identical to before.
func TestGedourLinkSingle(t *testing.T) {
	before := map[string]string{TypeTelegram: TelegramText(Format(sample)), TypeNextcloudTalk: TalkText(Format(sample))}
	tail := map[string]string{
		TypeTelegram:      "\n<a href=\"https://g.test/inbox?event=7\">Open in gedour</a>",
		TypeNextcloudTalk: "\n[Open in gedour](https://g.test/inbox?event=7)",
	}
	for _, typ := range []string{TypeTelegram, TypeNextcloudTalk} {
		base := map[string]any{"chat_id": "-1"}
		if typ == TypeNextcloudTalk {
			base = map[string]any{"url": "x", "conversation": "abcd"}
		}
		for _, tt := range []struct {
			link   any // nil: key absent
			public string
			want   string
		}{
			{nil, "https://g.test/", before[typ] + tail[typ]},
			{true, "https://g.test", before[typ] + tail[typ]},
			{false, "https://g.test", before[typ]},
			{nil, "", before[typ]},
		} {
			cfg := map[string]any{"gedour_link": tt.link}
			for k, v := range base {
				cfg[k] = v
			}
			if tt.link == nil {
				delete(cfg, "gedour_link")
			}
			raw, _ := json.Marshal(cfg)
			s := newSender(t, "127.0.0.0/8")
			s.PublicURL = tt.public
			ch := Channel{Type: typ, Config: raw, Secret: talkSecret}
			if typ == TypeTelegram {
				ch.Secret = "1:token"
			}
			if got := sent(t, s, ch, sample); got != tt.want {
				t.Errorf("%s %s public %q: %q, want %q", typ, raw, tt.public, got, tt.want)
			}
		}
	}
}

// 017-AC12, 017-AC14: the single mail ends with the gedour link in both parts; without it, as before.
func TestGedourLinkMail(t *testing.T) {
	for _, tt := range []struct {
		config, public string
		want           bool
	}{
		{`{"to":["a@x.test"]}`, "https://g.test", true},
		{`{"to":["a@x.test"],"gedour_link":false}`, "https://g.test", false},
		{`{"to":["a@x.test"]}`, "", false},
	} {
		sink := newSink(t, nil, false, false)
		s := newSender(t)
		s.PublicURL = tt.public
		s.Mailer = &Mailer{Addr: sink.ln.Addr().String(), From: from, TLS: TLSNone}
		if err := s.Send(context.Background(), Channel{Type: TypeEmail, Config: []byte(tt.config)}, sample); err != nil {
			t.Fatal(err)
		}
		sink.mu.Lock()
		// Quoted-printable encodes "=" as "=3D" and wraps long lines with a soft "=" break.
		data := strings.NewReplacer("=\r\n", "", "=\n", "").Replace(sink.data)
		sink.mu.Unlock()
		has := strings.Contains(data, "Open in gedour: https://g.test/inbox?event=3D7") && strings.Contains(data, `<a href=3D"https://g.test/inbox?event=3D7">Open in gedour</a>`)
		if has != tt.want || strings.Contains(data, "gedour</a>") != tt.want {
			t.Errorf("%s public %q: link %v, want %v\n%s", tt.config, tt.public, has, tt.want, data)
		}
	}
	m := mailMsg
	m.Gedour = "https://g.test/inbox?event=1"
	raw, err := BuildMail(from, []string{"a@x.test"}, m, time.Now())
	if err != nil || !strings.Contains(string(raw), "https://x.test/r?a=3D1&b=3D2\r\n\r\nOpen in gedour: https://g.test/inbox?event=3D1\r\n") {
		t.Errorf("text part: link not after the upstream URL: %v\n%s", err, raw)
	}
}

// 017-AC12: a single message over the cap keeps the gedour link with its title.
func TestGedourLinkSingleCap(t *testing.T) {
	m := Message{Title: strings.Repeat("<", 2000), Detail: strings.Repeat("d", 5000), URL: "https://x.test", Gedour: "https://g.test/inbox?event=1"}
	if got := TelegramText(m); len(got) > maxTelegramLen || !strings.HasSuffix(got, "</b>\n<a href=\"https://g.test/inbox?event=1\">Open in gedour</a>") {
		t.Errorf("telegram %d bytes: %q", len(got), got[max(0, len(got)-120):])
	}
	// Fits only without the link: the link counts, the text falls back to the title and link.
	edge := Message{Title: "t", Gedour: m.Gedour}
	edge.Detail = strings.Repeat("d", maxTelegramLen-10-len(TelegramText(Message{Title: "t"})))
	if got := TelegramText(edge); got != "<b>t</b>\n<a href=\"https://g.test/inbox?event=1\">Open in gedour</a>" {
		t.Errorf("telegram at the cap: %d bytes", len(got))
	}
	m.Title, m.Detail = strings.Repeat("t", 40000), ""
	if got := TalkText(m); utf8.RuneCountInString(got) > maxTalkLen || !strings.HasSuffix(got, "\n[Open upstream](https://x.test)\n[Open in gedour](https://g.test/inbox?event=1)") {
		t.Errorf("talk %d characters: %q", utf8.RuneCountInString(got), got[max(0, len(got)-120):])
	}
}

// 017-AC13, 017-AC14: a grouped message or digest with the link on ends with one "Open the inbox" link and no
// per-event gedour link; with it off, byte-identical to before. The mail digest's Inbox line is its link, never doubled.
func TestGedourLinkDigest(t *testing.T) {
	for _, poll := range []bool{true, false} {
		dg := digestOf(3, func(i int) string { return fmt.Sprint("v", i) })
		dg.Poll = poll
		off := map[string]string{"telegram": TelegramDigest(dg, inbox), "talk": TalkDigest(dg, inbox)}
		dg.Link = true
		on := map[string]string{"telegram": TelegramDigest(dg, inbox), "talk": TalkDigest(dg, inbox)}
		tails := map[string]string{"telegram": "\n\n<a href=\"https://g.test/inbox\">Open the inbox</a>", "talk": "\n\n[Open the inbox](https://g.test/inbox)"}
		for name, text := range on {
			if text != off[name]+tails[name] || strings.Count(text, "https://g.test") != 1 || strings.Contains(text, "?event=") {
				t.Errorf("%s poll %v: %q", name, poll, text)
			}
		}
		if TelegramDigest(dg, "") != off["telegram"] || TalkDigest(dg, "") != off["talk"] {
			t.Errorf("poll %v: no public URL must leave the digest as before", poll)
		}
		if m := MailDigest(dg, inbox); strings.Count(m.Detail, inbox) != 1 || !strings.HasSuffix(m.Detail, "\r\n\r\nInbox: "+inbox) {
			t.Errorf("mail poll %v: %q", poll, m.Detail)
		}
	}
}

// 017-AC16: over the cap with the link on, the message is cut on a whole line, within the cap, and holds the
// inbox link once: in its "+N more" line, no separate "Open the inbox" line.
func TestGedourLinkDigestCap(t *testing.T) {
	big := digestOf(400, func(i int) string { return fmt.Sprintf("v%d-%s", i, strings.Repeat("x", 200)) })
	big.Poll, big.Link = true, true
	for name, tt := range map[string]struct {
		text  string
		limit int
		size  func(string) int
		item  string
	}{
		"telegram": {TelegramDigest(big, inbox), maxTelegramLen, func(s string) int { return len(s) }, "\n• "},
		"talk":     {TalkDigest(big, inbox), maxTalkLen, utf8.RuneCountInString, "\n- "},
	} {
		lines := strings.Split(tt.text, "\n")
		last := lines[len(lines)-1]
		listed := strings.Count(tt.text, tt.item)
		want := fmt.Sprintf("+%d more: ", 400-listed)
		if tt.size(tt.text) > tt.limit || !strings.HasPrefix(last, want) || strings.Count(tt.text, inbox) != 1 || strings.Contains(tt.text, "Open the inbox") {
			t.Errorf("%s: %d long, %d listed, last %q, %d links", name, tt.size(tt.text), listed, last, strings.Count(tt.text, inbox))
		}
	}
	// A digest that fits only without the link line is cut: the link counts toward the cap.
	dg := digestOf(1, func(int) string { return "v" })
	pad := maxTelegramLen - 10 - len(TelegramDigest(dg, inbox))
	dg = digestOf(1, func(int) string { return "v" + strings.Repeat("x", pad) })
	if off := TelegramDigest(dg, inbox); len(off) != maxTelegramLen-10 || strings.Contains(off, "more") {
		t.Fatalf("setup: %d bytes", len(off))
	}
	dg.Link = true
	if text := TelegramDigest(dg, inbox); len(text) > maxTelegramLen || !strings.HasSuffix(text, "+1 more: <a href=\"https://g.test/inbox\">open the inbox</a>") || strings.Count(text, inbox) != 1 {
		t.Errorf("at the cap: %d bytes %q", len(text), text[max(0, len(text)-120):])
	}
}

// 017-AC13: SendDigest adds the inbox link on Telegram unless gedour_link is false.
func TestGedourLinkSendDigest(t *testing.T) {
	dg := sampleDigest(time.Now())
	for _, tt := range []struct {
		config string
		want   bool
	}{{`{"chat_id":"-1"}`, true}, {`{"chat_id":"-1","gedour_link":true}`, true}, {`{"chat_id":"-1","gedour_link":false}`, false}} {
		srv, got := receiver(t, http.StatusOK)
		s := newSender(t, "127.0.0.0/8")
		s.TelegramURL, s.PublicURL = srv.URL, "https://g.test"
		if err := s.SendDigest(context.Background(), Channel{Type: TypeTelegram, Config: []byte(tt.config), Secret: "1:token"}, dg); err != nil {
			t.Fatal(err)
		}
		var msg struct{ Text string }
		_ = json.Unmarshal(got.body, &msg)
		if has := strings.HasSuffix(msg.Text, "\n<a href=\"https://g.test/inbox\">Open the inbox</a>"); has != tt.want {
			t.Errorf("%s: %q", tt.config, msg.Text)
		}
	}
}
