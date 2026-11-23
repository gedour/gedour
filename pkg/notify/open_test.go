package notify

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	// Vector computed outside Go with Python's hmac module (HKDF-SHA256 by hand, RFC 5869).
	vectorEncKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	vectorLink   = "42.6f1d2c3b-4a59-4e8f-9a0b-1c2d3e4f5a6b.8hMGpzkjn5WyKjTeh8UnyQ"
	owner        = "6f1d2c3b-4a59-4e8f-9a0b-1c2d3e4f5a6b"
	public       = "https://g.test/"
)

func openKey(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	k, err := DeriveOpenKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// signedURL is the /r link of event id for owner under the vector key.
func signedURL(t *testing.T, id int64) string {
	return "https://g.test/r#" + openLink(openKey(t, vectorEncKey), id, owner)
}

// 021-AC9: the fixed vector pins the link format; any other spelling, a tag differing only in its unused
// bits, another key or no key does not verify.
func TestReadOnOpenVector(t *testing.T) {
	s := &Sender{OpenKey: openKey(t, vectorEncKey)}
	if got := openLink(s.OpenKey, 42, owner); got != vectorLink {
		t.Fatalf("link %q, want %q", got, vectorLink)
	}
	if id, o, ok := s.VerifyOpen(vectorLink); !ok || id != 42 || o != owner {
		t.Fatalf("vector: %d %q %v", id, o, ok)
	}
	head, tag := vectorLink[:strings.LastIndexByte(vectorLink, '.')+1], vectorLink[strings.LastIndexByte(vectorLink, '.')+1:]
	if !strings.HasSuffix(tag, "Q") {
		t.Fatalf("setup: tag %q", tag)
	}
	// 16 bytes in 22 characters: the last one holds 2 unused bits, Q R S T decode alike.
	for _, alias := range []string{"R", "S", "T"} {
		if b, err := base64.RawURLEncoding.DecodeString(tag[:21] + alias); err != nil || base64.RawURLEncoding.EncodeToString(b) != tag {
			t.Fatalf("setup: %s does not alias the tag: %v", alias, err)
		}
	}
	other := "AQECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	for _, bad := range []string{
		head + tag[:21] + "R", head + tag[:21] + "S", head + tag[:21] + "T",
		head + "A" + tag[1:], head + tag + "A", head + tag[:21], head + tag + "=",
		"43" + vectorLink[2:], "042" + vectorLink[2:], "+42" + vectorLink[2:], "0" + vectorLink[2:], "-42" + vectorLink[2:],
		strings.Replace(vectorLink, "6f1d", "7f1d", 1), strings.Replace(vectorLink, "6f1d2c3b", "6F1D2C3B", 1),
		strings.Replace(vectorLink, owner, "urn:uuid:"+owner, 1), strings.Replace(vectorLink, owner, "{"+owner+"}", 1),
		strings.Replace(vectorLink, owner, strings.ReplaceAll(owner, "-", ""), 1),
		"42." + owner + "." + openLink(openKey(t, other), 42, owner)[len("42."+owner+"."):],
		vectorLink + ".x", "#" + vectorLink, "", "..", "42..", strings.Repeat("4", 2000) + vectorLink[2:],
	} {
		if id, o, ok := s.VerifyOpen(bad); ok || id != 0 || o != "" {
			t.Errorf("%q verifies: %d %q", bad, id, o)
		}
	}
	if _, _, ok := (&Sender{}).VerifyOpen(vectorLink); ok {
		t.Error("a sender without a key verifies")
	}
	if got := openLink(nil, 42, owner); got != "" {
		t.Errorf("no key signs %q", got)
	}
}

// readOnOpenChannel is a channel of typ owned by owner, extra spliced into its config (e.g. `,"read_on_open":true`).
func readOnOpenChannel(typ, extra string) Channel {
	cfg := map[string]string{
		TypeTelegram:      `{"chat_id":"-1"`,
		TypeNextcloudTalk: `{"url":"x","conversation":"abcd"`,
		TypeEmail:         `{"to":["a@x.test"]`,
	}[typ] + extra + "}"
	ch := Channel{Type: typ, OwnerID: owner, Config: []byte(cfg), Secret: talkSecret}
	if typ == TypeTelegram {
		ch.Secret = "1:token"
	}
	return ch
}

// mailed is the decoded subject and parts of the one mail send delivers.
func mailed(t *testing.T, s *Sender, send func() error) (string, []string) {
	t.Helper()
	sink := newSink(t, nil, false, false)
	s.Mailer = &Mailer{Addr: sink.ln.Addr().String(), From: from, TLS: TLSNone}
	if err := send(); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return mailParts(t, sink.data)
}

func mailParts(t *testing.T, raw string) (string, []string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return subject, parts
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(quotedprintable.NewReader(bufio.NewReader(p)))
		if err != nil {
			t.Fatal(err)
		}
		// The sink's DATA reader turns CRLF into LF: both sides compare in that form.
		parts = append(parts, strings.ReplaceAll(string(b), "\r\n", "\n"))
	}
}

// expectMail is the decoded subject and parts of msg as BuildMail renders it.
func expectMail(t *testing.T, msg Message) (string, []string) {
	t.Helper()
	raw, err := BuildMail(from, []string{"a@x.test"}, msg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return mailParts(t, string(raw))
}

// readOnOpenCases are the channel settings and senders of 021-AC1, AC2, AC3 and the key-less sender.
var readOnOpenCases = []struct {
	name, extra, public string
	key, signed         bool
}{
	{"absent", "", public, true, false},                                      // 021-AC1
	{"false", `,"read_on_open":false`, public, true, false},                  // 021-AC1
	{"null", `,"read_on_open":null`, public, true, false},                    // 021-AC5: null reads as absent
	{"on", `,"read_on_open":true`, public, true, true},                       // 021-AC2
	{"on, no public URL", `,"read_on_open":true`, "", true, false},           // 021-AC3
	{"on, sender without key", `,"read_on_open":true`, public, false, false}, // 021-AC9
}

func readOnOpenSender(t *testing.T, public string, key bool) *Sender {
	s := newSender(t, "127.0.0.0/8")
	s.PublicURL = public
	if key {
		s.OpenKey = openKey(t, vectorEncKey)
	}
	return s
}

// 021-AC1, 021-AC2, 021-AC3: a single message, a live test included, links upstream through the signed /r link
// only with the option on, a public URL and a key; the gedour link stays; otherwise byte-identical. A channel
// test (event id 0) never gets one.
func TestReadOnOpenSingle(t *testing.T) {
	live := sample
	live.Test = true
	test := sample
	test.Event.ID = 0
	for _, tc := range readOnOpenCases {
		for _, d := range []Delivery{sample, live, test} {
			want := Format(d)
			if tc.public != "" {
				want.Gedour = (&Sender{PublicURL: tc.public}).eventURL(nil, d.Event.ID)
			}
			if tc.signed && d.Event.ID != 0 {
				want.URL = signedURL(t, d.Event.ID)
			}
			for typ, render := range map[string]func(Message) string{TypeTelegram: TelegramText, TypeNextcloudTalk: TalkText} {
				s := readOnOpenSender(t, tc.public, tc.key)
				if got := sent(t, s, readOnOpenChannel(typ, tc.extra), d); got != render(want) {
					t.Errorf("%s %s event %d: %q, want %q", tc.name, typ, d.Event.ID, got, render(want))
				}
			}
			s := readOnOpenSender(t, tc.public, tc.key)
			subject, parts := mailed(t, s, func() error { return s.Send(context.Background(), readOnOpenChannel(TypeEmail, tc.extra), d) })
			wantSubject, wantParts := expectMail(t, want)
			if subject != wantSubject || strings.Join(parts, "\x00") != strings.Join(wantParts, "\x00") {
				t.Errorf("%s mail event %d: %q %q, want %q %q", tc.name, d.Event.ID, subject, parts, wantSubject, wantParts)
			}
		}
	}
	// The vector's link as the message renders it.
	s := readOnOpenSender(t, public, true)
	d := sample
	d.Event.ID = 42
	if got := sent(t, s, readOnOpenChannel(TypeTelegram, `,"read_on_open":true`), d); !strings.Contains(got, `<a href="https://g.test/r#`+vectorLink+`">Open upstream</a>`) {
		t.Errorf("vector link not rendered: %q", got)
	}
}

// signedDigest is dg with each event's upstream URL replaced by its signed link, as a renderer must see it.
func signedDigest(t *testing.T, dg Digest) Digest {
	out := dg
	out.Deliveries = nil
	for _, d := range dg.Deliveries {
		d.Event.Payload = maps.Clone(d.Event.Payload)
		d.Event.Payload["url"] = signedURL(t, d.Event.ID)
		out.Deliveries = append(out.Deliveries, d)
	}
	return out
}

// 021-AC1, 021-AC2, 021-AC3: grouped messages (poll) and digests link every event through its signed link only
// with the option on, a public URL and a key; the inbox link stays; otherwise byte-identical.
func TestReadOnOpenDigest(t *testing.T) {
	for _, tc := range readOnOpenCases {
		for _, poll := range []bool{true, false} {
			dg := digestOf(3, func(i int) string { return "v" + strings.Repeat("1", i+1) })
			dg.Poll = poll
			want := dg
			if tc.signed {
				want = signedDigest(t, dg)
			}
			want.Link = true
			in := (&Sender{PublicURL: tc.public}).inboxURL()
			for typ, render := range map[string]func(Digest, string) string{TypeTelegram: TelegramDigest, TypeNextcloudTalk: TalkDigest} {
				s := readOnOpenSender(t, tc.public, tc.key)
				got := capture(t, s, readOnOpenChannel(typ, tc.extra), func(ch Channel) error { return s.SendDigest(context.Background(), ch, dg) })
				if got != render(want, in) {
					t.Errorf("%s %s poll %v: %q, want %q", tc.name, typ, poll, got, render(want, in))
				}
				if n := strings.Count(got, "https://g.test/r#"); tc.signed != (n == 3) || (!tc.signed && n != 0) {
					t.Errorf("%s %s poll %v: %d signed links", tc.name, typ, poll, n)
				}
			}
			s := readOnOpenSender(t, tc.public, tc.key)
			subject, parts := mailed(t, s, func() error { return s.SendDigest(context.Background(), readOnOpenChannel(TypeEmail, tc.extra), dg) })
			wantSubject, wantParts := expectMail(t, MailDigest(want, in))
			if subject != wantSubject || strings.Join(parts, "\x00") != strings.Join(wantParts, "\x00") {
				t.Errorf("%s mail poll %v: %q %q, want %q %q", tc.name, poll, subject, parts, wantSubject, wantParts)
			}
		}
	}
}

var (
	telegramHref = regexp.MustCompile(`href="([^"]*)"`)
	talkHref     = regexp.MustCompile(`\]\(([^)]*)\)`)
)

// 021-AC2: the caps, whole-line cuts and "+N more" apply to the text as rendered with signed links: a message
// that fits with the upstream URL but not with its signed link is cut, Telegram's fallback to its title has no
// upstream link, and every link left is a signed one or the inbox.
func TestReadOnOpenCaps(t *testing.T) {
	s := readOnOpenSender(t, public, true)
	on := `,"read_on_open":true`
	big := digestOf(400, func(i int) string { return "v" + strings.Repeat("x", 200) + string(rune('a'+i%26)) })
	for typ, tt := range map[string]struct {
		render func(Digest, string) string
		limit  int
		size   func(string) int
		href   *regexp.Regexp
	}{
		TypeTelegram:      {TelegramDigest, maxTelegramLen, func(s string) int { return len(s) }, telegramHref},
		TypeNextcloudTalk: {TalkDigest, maxTalkLen, utf8.RuneCountInString, talkHref},
	} {
		got := capture(t, s, readOnOpenChannel(typ, on), func(ch Channel) error { return s.SendDigest(context.Background(), ch, big) })
		want := signedDigest(t, big)
		want.Link = true
		if got != tt.render(want, inbox) || tt.size(got) > tt.limit || !strings.Contains(got, " more: ") {
			t.Errorf("%s digest: %d long, want the signed rendering cut at %d", typ, tt.size(got), tt.limit)
		}
		unsigned := big
		unsigned.Link = true
		if got == tt.render(unsigned, inbox) {
			t.Errorf("%s digest: cut as with the upstream URLs", typ)
		}
		for _, m := range tt.href.FindAllStringSubmatch(got, -1) {
			if !strings.HasPrefix(m[1], "https://g.test/r#") && m[1] != inbox {
				t.Errorf("%s digest: link %q", typ, m[1])
			}
		}
	}

	// Fits at the cap with the upstream URL: the longer signed link tips it into the title-only fallback.
	d := sample
	base := Format(d)
	base.Gedour = "https://g.test/inbox?event=7"
	d.Event.Payload = map[string]any{"tag": "v1.2.3", "url": base.URL, "title": strings.Repeat("d", maxTelegramLen-len(TelegramText(base))-1)}
	off := sent(t, s, readOnOpenChannel(TypeTelegram, ""), d)
	if len(off) != maxTelegramLen || !strings.Contains(off, "Open upstream") {
		t.Fatalf("setup: %d bytes", len(off))
	}
	got := sent(t, s, readOnOpenChannel(TypeTelegram, on), d)
	if got != "<b>o/r: v1.2.3 published</b>\n<a href=\"https://g.test/inbox?event=7\">Open in gedour</a>" {
		t.Errorf("telegram fallback: %q", got)
	}
}

// 021-AC4: no upstream link without a URL or with a non-http(s) one, as today; with the swap on, none for a URL
// url.Parse refuses or reads without a host; a Talk URL over maxTalkLink gets its signed link.
func TestReadOnOpenURLs(t *testing.T) {
	long := "https://x.test/" + strings.Repeat("a", maxTalkLink)
	for _, tt := range []struct {
		url      string
		off, on  bool // an upstream link with the option off, on
		typ      string
		onSigned bool
	}{
		{"", false, false, TypeTelegram, false},
		{"ftp://x.test/a", false, false, TypeTelegram, false},
		{"javascript:alert(1)", false, false, TypeNextcloudTalk, false},
		{"http://[::1", true, false, TypeTelegram, false},
		{"http://[::1", true, false, TypeNextcloudTalk, false},
		{"https:///no-host", true, false, TypeTelegram, false},
		{"https://%zz/", true, false, TypeNextcloudTalk, false},
		{"https://x.test/a", true, true, TypeTelegram, true},
		{long, false, true, TypeNextcloudTalk, true},
	} {
		d := sample
		d.Event.Payload = map[string]any{"tag": "v1", "url": tt.url}
		for _, c := range []struct {
			extra string
			want  bool
		}{{"", tt.off}, {`,"read_on_open":true`, tt.on}} {
			s := readOnOpenSender(t, public, true)
			got := sent(t, s, readOnOpenChannel(tt.typ, c.extra), d)
			if strings.Contains(got, "Open upstream") != c.want || (c.extra != "" && c.want && !strings.Contains(got, signedURL(t, 7))) {
				t.Errorf("%s %q %q: %q", tt.typ, tt.url, c.extra, got)
			}
			// The digest line of the same event: a link only where the single message has one.
			got = capture(t, s, readOnOpenChannel(tt.typ, c.extra), func(ch Channel) error {
				return s.SendDigest(context.Background(), ch, Digest{Deliveries: []Delivery{d}})
			})
			if links := strings.Count(got, "://") - 1; (links == 1) != c.want || links > 1 {
				t.Errorf("digest %s %q %q: %q", tt.typ, tt.url, c.extra, got)
			}
		}
	}
	// No owner id (a channel loaded without it) or event id 0: the URL as is, never a link no one can verify.
	s := readOnOpenSender(t, public, true)
	if got := s.upstream(true, "", 7, "https://x.test"); got != "https://x.test" {
		t.Errorf("no owner: %q", got)
	}
	if got := s.upstream(true, owner, 0, "https://x.test"); got != "https://x.test" {
		t.Errorf("event 0: %q", got)
	}
}

// 021-AC16, 021-AC17, 021-AC18: a Web Push notification of one real event, a live test included, opens the
// relative signed /r link, public URL or not, only with the option on, a key, an owner and a URL the swap
// accepts; its tag, title and body stay. Otherwise, a channel test, and a group or digest of two events,
// the payload is the one without the option.
func TestReadOnOpenPush(t *testing.T) {
	yes, no := true, false
	live := sample
	live.Test = true
	// A channel test with a URL: only its event id 0 keeps the link out.
	test := TestDelivery(time.Now())
	test.Event.Payload = map[string]any{"message": "m", "url": "https://x.test/a"}
	withURL := func(u any) Delivery {
		d := sample
		d.Event.Payload = map[string]any{"tag": "v1", "url": u}
		return d
	}
	for _, pub := range []string{"", public} {
		for _, tt := range []struct {
			name   string
			on     *bool
			key    bool
			owner  string
			d      Delivery
			signed bool
		}{
			{"on", &yes, true, owner, sample, true},
			{"live test", &yes, true, owner, live, true},
			{"absent", nil, true, owner, sample, false},
			{"false", &no, true, owner, sample, false},
			{"no key", &yes, false, owner, sample, false},
			{"no owner", &yes, true, "", sample, false},
			{"channel test", &yes, true, owner, test, false},
			{"no url", &yes, true, owner, withURL(nil), false},
			{"ftp url", &yes, true, owner, withURL("ftp://x.test/a"), false},
			{"no host", &yes, true, owner, withURL("https:///a"), false},
			{"unparsable", &yes, true, owner, withURL("http://[::1"), false},
		} {
			s := readOnOpenSender(t, pub, tt.key)
			before := PushPayload(tt.d, true, "")
			got := PushPayload(tt.d, true, s.PushOpen(tt.on, tt.owner, tt.d))
			var m, b PushMessage
			_ = json.Unmarshal(got, &m)
			_ = json.Unmarshal(before, &b)
			if !tt.signed {
				if string(got) != string(before) {
					t.Errorf("%s %q: %s, want %s", tt.name, pub, got, before)
				}
				continue
			}
			if want := "/r#" + openLink(s.OpenKey, tt.d.Event.ID, owner); m.URL != want {
				t.Errorf("%s %q: url %q, want %q", tt.name, pub, m.URL, want)
			}
			if b.URL = m.URL; m != b {
				t.Errorf("%s %q: %+v, want %+v", tt.name, pub, m, b)
			}
		}
	}
	s := readOnOpenSender(t, "", true)
	for _, poll := range []bool{false, true} {
		one, two := digestOf(1, func(int) string { return "v1" }), digestOf(2, func(int) string { return "v1" })
		one.Poll, two.Poll = poll, poll
		var m PushMessage
		_ = json.Unmarshal(PushDigestPayload(one, "c1", false, s.PushOpen(&yes, owner, one.Deliveries...)), &m)
		var b PushMessage
		_ = json.Unmarshal(PushDigestPayload(one, "c1", false, ""), &b)
		if want := "/r#" + openLink(s.OpenKey, 1, owner); m.URL != want || b.URL != "/inbox" {
			t.Errorf("poll %v, one event: %q, want %q", poll, m.URL, want)
		}
		if b.URL = m.URL; m != b {
			t.Errorf("poll %v, one event: %+v, want %+v", poll, m, b)
		}
		if got, want := PushDigestPayload(two, "c1", false, s.PushOpen(&yes, owner, two.Deliveries...)), PushDigestPayload(two, "c1", false, ""); string(got) != string(want) {
			t.Errorf("poll %v, two events: %s, want %s", poll, got, want)
		}
		if got, want := PushDigestPayload(one, "c1", false, s.PushOpen(nil, owner, one.Deliveries...)), PushDigestPayload(one, "c1", false, ""); string(got) != string(want) {
			t.Errorf("poll %v, option off: %s, want %s", poll, got, want)
		}
	}
}
