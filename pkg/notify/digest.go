package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"
)

// Digest is one scheduled message listing several deliveries of a channel, oldest first.
type Digest struct {
	// ID is the lead delivery, Talk's reference; 0 for a test.
	ID         int64
	Deliveries []Delivery
}

// SampleDigest is what "Send test" sends on a digest channel: two events, no real watch behind them.
func SampleDigest(now time.Time) Digest {
	w := Watch{Name: "gedour (sample)"}
	return Digest{Deliveries: []Delivery{
		{Event: Event{Type: "release.published", ObservedAt: now, Payload: map[string]any{"tag": "v1.0.0"}}, Watch: w},
		{Event: Event{Type: "pr.opened", ObservedAt: now, Payload: map[string]any{"number": 1.0, "author": "gedour"}}, Watch: w},
	}}
}

type digestGroup struct {
	watch string
	msgs  []Message
}

// groups splits ds by watch, each watch where its oldest event stands.
func groups(ds []Delivery) []digestGroup {
	var out []digestGroup
	at := map[string]int{}
	for _, d := range ds {
		key := d.Watch.ID + "\x00" + d.Watch.Name
		i, ok := at[key]
		if !ok {
			i = len(out)
			at[key] = i
			out = append(out, digestGroup{watch: oneLine(d.Watch.Name)})
		}
		m := Format(d)
		m.Summary = oneLine(m.Summary)
		out[i].msgs = append(out[i].msgs, m)
	}
	return out
}

// digestStyle renders a digest in one channel's markup.
type digestStyle struct {
	header, group func(string) string
	item          func(Message) string
	more          func(n int) string
	sep           string
	limit         int // 0: no cap
	size          func(string) int
	maxItems      int // 0: no cap
}

func countEvents(n int) string { return plural(n, "unread event") }

// render lists ds by watch. Over the cap it stops before the first line that does not fit and ends
// with "+N more", never leaving a watch heading without an event under it.
func (st digestStyle) render(ds []Delivery) string {
	gs := groups(ds)
	parts := []string{st.header(countEvents(len(ds)))}
	for _, g := range gs {
		parts = append(parts, st.group(g.watch))
		for _, m := range g.msgs {
			parts = append(parts, st.item(m))
		}
	}
	full := strings.Join(parts, st.sep)
	if (st.limit == 0 || st.size(full) <= st.limit) && (st.maxItems == 0 || len(ds) <= st.maxItems) {
		return full
	}
	parts = parts[:1]
	used := st.size(parts[0])
	reserve := st.size(st.sep + st.more(len(ds)))
	listed := 0
outer:
	for _, g := range gs {
		for i, m := range g.msgs {
			add := []string{st.item(m)}
			if i == 0 {
				add = []string{st.group(g.watch), add[0]}
			}
			n := 0
			for _, a := range add {
				n += st.size(st.sep + a)
			}
			if (st.limit > 0 && used+n+reserve > st.limit) || (st.maxItems > 0 && listed == st.maxItems) {
				break outer
			}
			parts, used, listed = append(parts, add...), used+n, listed+1
		}
	}
	return strings.Join(append(parts, st.more(len(ds)-listed)), st.sep)
}

func (s *Sender) inboxURL() string {
	if s.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + "/inbox"
}

// TelegramDigest is the HTML-mode digest; cut to Telegram's limit, counted in bytes as TelegramText does.
func TelegramDigest(dg Digest, inbox string) string {
	return digestStyle{
		header: func(s string) string { return "<b>" + html.EscapeString(s) + "</b>" },
		group:  func(w string) string { return "\n<b>" + html.EscapeString(w) + "</b>" },
		item: func(m Message) string {
			if m.URL == "" {
				return "• " + html.EscapeString(m.Summary)
			}
			return "• <a href=\"" + html.EscapeString(m.URL) + "\">" + html.EscapeString(m.Summary) + "</a>"
		},
		more: func(n int) string {
			if inbox == "" {
				return fmt.Sprintf("+%d more in the gedour inbox", n)
			}
			return fmt.Sprintf("+%d more: <a href=\"%s\">open the inbox</a>", n, html.EscapeString(inbox))
		},
		sep: "\n", limit: maxTelegramLen, size: func(s string) int { return len(s) },
	}.render(dg.Deliveries)
}

// TalkDigest is the Markdown digest, cut to maxTalkLen characters.
func TalkDigest(dg Digest, inbox string) string {
	return digestStyle{
		header: func(s string) string { return "**" + talkEscaper.Replace(s) + "**" },
		group:  func(w string) string { return "\n**" + talkEscaper.Replace(w) + "**" },
		item: func(m Message) string {
			text := talkEscaper.Replace(m.Summary)
			if u := talkLink(m.URL); u != "" && utf8.RuneCountInString(u) <= maxTalkLink {
				return "- [" + text + "](" + u + ")"
			}
			return "- " + text
		},
		more: func(n int) string {
			if inbox == "" {
				return fmt.Sprintf("+%d more in the gedour inbox", n)
			}
			return fmt.Sprintf("+%d more: [open the inbox](%s)", n, talkLink(inbox))
		},
		sep: "\n", limit: maxTalkLen, size: utf8.RuneCountInString,
	}.render(dg.Deliveries)
}

// Mail digest caps: a client renders a long mail, but not an unbounded one.
const (
	maxMailEvents = 1000
	maxMailBytes  = 256 << 10
)

// MailDigest is the digest mail: the count in the subject, one line per event in the body.
func MailDigest(dg Digest, inbox string) Message {
	body := digestStyle{
		header: func(string) string { return "" },
		group:  func(w string) string { return w },
		item: func(m Message) string {
			if m.URL == "" {
				return "- " + m.Summary
			}
			return "- " + m.Summary + ": " + m.URL
		},
		more: func(n int) string {
			if inbox == "" {
				return fmt.Sprintf("+%d more in the gedour inbox", n)
			}
			return fmt.Sprintf("+%d more: %s", n, inbox)
		},
		sep: "\r\n", limit: maxMailBytes, size: func(s string) int { return len(s) }, maxItems: maxMailEvents,
	}.render(dg.Deliveries)
	if inbox != "" {
		body += "\r\n\r\nInbox: " + inbox
	}
	return Message{Title: "gedour: " + countEvents(len(dg.Deliveries)), Detail: strings.TrimPrefix(body, "\r\n")}
}

// SendDigest sends dg through ch as one message; the channel's silent setting applies.
func (s *Sender) SendDigest(ctx context.Context, ch Channel, dg Digest) error {
	switch ch.Type {
	case TypeTelegram:
		var c TelegramConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("telegram config: %w", err)
		}
		return s.telegram(ctx, c, ch.Secret, TelegramDigest(dg, s.inboxURL()))
	case TypeNextcloudTalk:
		var c NextcloudTalkConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("nextcloud talk config: %w", err)
		}
		return s.talk(ctx, c, ch.Secret, dg.ID, TalkDigest(dg, s.inboxURL()))
	case TypeEmail:
		var c EmailConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("email config: %w", err)
		}
		if s.Mailer == nil {
			return errors.New("email: no SMTP server configured")
		}
		return s.Mailer.Send(ctx, c.To, MailDigest(dg, s.inboxURL()), s.now())
	}
	return Permanent(fmt.Errorf("digest on channel type %q", ch.Type))
}

// PushDigestPayload is the Web Push digest of a channel: the count as title, the first watch names as body,
// opening the inbox; the tag per channel lets one channel's digest replace its last without hiding another's.
func PushDigestPayload(dg Digest, channelID string, silent bool) []byte {
	var names []string
	for _, g := range groups(dg.Deliveries) {
		names = append(names, g.watch)
	}
	return fitPush(PushMessage{Title: countEvents(len(dg.Deliveries)), Body: strings.Join(names, ", "), URL: "/inbox", Tag: "gedour-digest-" + channelID, Silent: silent})
}
