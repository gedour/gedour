package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/netguard"
)

func newSender(t *testing.T, allow ...string) *Sender {
	t.Helper()
	g, err := netguard.New(allow)
	if err != nil {
		t.Fatal(err)
	}
	return &Sender{Client: g.Client(netguard.Options{Timeout: SendTimeout, HTTPHosts: []string{"127.0.0.1"}, NoRedirects: true}),
		UserAgent: "gedour/test", Now: func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }}
}

type received struct {
	mu     sync.Mutex
	path   string
	header http.Header
	body   []byte
}

func receiver(t *testing.T, code int) (*httptest.Server, *received) {
	t.Helper()
	got := &received{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.path, got.header, got.body = r.URL.Path, r.Header.Clone(), b
		got.mu.Unlock()
		if code == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", code)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

var sample = Delivery{ID: 42,
	Event: Event{ID: 7, Type: "release.published", ObservedAt: time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC),
		Payload: map[string]any{"tag": "v1.2.3", "url": "https://github.com/o/r/releases/v1.2.3"}},
	Watch: Watch{ID: "w1", Name: "o/r", Kind: "github_repo"},
	Rule:  &Rule{ID: "r1", Name: "releases"}}

func webhookChannel(url string) Channel {
	cfg, _ := json.Marshal(WebhookConfig{URL: url})
	return Channel{ID: "c1", Type: TypeWebhook, Config: cfg, Secret: "k3y-k3y-k3y-k3y-k3y"} //nolint:gosec // test signing key
}

// 001-AC16: the receiver can verify X-Gedour-Signature (HMAC-SHA256 of the raw body) and reads the documented body.
func TestWebhook(t *testing.T) {
	srv, got := receiver(t, http.StatusNoContent)
	s := newSender(t, "127.0.0.0/8")
	if err := s.Send(context.Background(), webhookChannel(srv.URL+"/hook"), sample); err != nil {
		t.Fatal(err)
	}
	sig := got.header.Get(SignatureHeader)
	if !hmac.Equal([]byte(sig), []byte(Sign("k3y-k3y-k3y-k3y-k3y", got.body))) || !strings.HasPrefix(sig, "sha256=") || len(sig) != 71 {
		t.Fatalf("signature %q does not verify", sig)
	}
	if got.header.Get(EventHeader) != "release.published" || got.header.Get(DeliveryHeader) != "42" ||
		got.header.Get("User-Agent") != "gedour/test" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", got.header)
	}
	var body WebhookBody
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.DeliveryID != 42 || body.Event.ID != 7 || body.Watch.Name != "o/r" || body.Rule.Name != "releases" || body.SentAt.IsZero() ||
		body.Event.Payload["tag"] != "v1.2.3" {
		t.Fatalf("body: %s", got.body)
	}
}

// 004-AC8: a webhook carries both fields, whole.
func TestWebhookCarriesNotes(t *testing.T) {
	srv, got := receiver(t, http.StatusNoContent)
	s := newSender(t, "127.0.0.0/8")
	d := sample
	notes := strings.Repeat("n", 64<<10)
	d.Event.Payload = map[string]any{"tag": "v1", "body": notes, "cves": []any{"CVE-2026-1"}}
	if err := s.Send(context.Background(), webhookChannel(srv.URL+"/hook"), d); err != nil {
		t.Fatal(err)
	}
	var body WebhookBody
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Event.Payload["body"] != notes || fmt.Sprint(body.Event.Payload["cves"]) != "[CVE-2026-1]" {
		t.Fatalf("payload %.100v", body.Event.Payload)
	}
}

// 001-AC15/001-AC16: a non-2xx answer, a redirect (never followed) and a private target all fail.
func TestWebhookFailures(t *testing.T) {
	for _, tt := range []struct {
		code int
		want string
	}{
		{http.StatusInternalServerError, "HTTP 500"},
		{http.StatusFound, "HTTP 302 (redirects are not followed)"},
	} {
		srv, got := receiver(t, tt.code)
		err := newSender(t, "127.0.0.0/8").Send(context.Background(), webhookChannel(srv.URL+"/secret-path?token=abc"), sample)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%d: %v", tt.code, err)
		}
		if strings.Contains(err.Error(), "token=abc") || got.path != "/secret-path" {
			t.Errorf("%d: the URL leaked into the error or the redirect was followed: %v (%s)", tt.code, err, got.path)
		}
	}
	srv, got := receiver(t, http.StatusNoContent)
	err := newSender(t).Send(context.Background(), webhookChannel(srv.URL), sample)
	if !errors.Is(err, netguard.ErrBlocked) || got.body != nil {
		t.Fatalf("loopback target without allowlist: %v", err)
	}
}

// Telegram gets sendMessage at <api>/bot<token>/, HTML mode, escaped text; the token never shows in errors.
func TestTelegram(t *testing.T) {
	const token = "123456:AAEsecretsecretsecretsecret"
	cfg, _ := json.Marshal(TelegramConfig{ChatID: "-1001"})
	ch := Channel{Type: TypeTelegram, Config: cfg, Secret: token}

	srv, got := receiver(t, http.StatusOK)
	s := newSender(t, "127.0.0.0/8")
	s.TelegramURL = srv.URL + "/"
	d := sample
	d.Event.Payload = map[string]any{"tag": "<v1>", "url": "https://x.test/"}
	if err := s.Send(context.Background(), ch, d); err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(got.body, &msg); err != nil {
		t.Fatal(err)
	}
	if got.path != "/bot"+token+"/sendMessage" || msg["chat_id"] != "-1001" || msg["parse_mode"] != "HTML" || msg["disable_web_page_preview"] != true || msg["disable_notification"] != false ||
		msg["text"] != "<b>o/r: &lt;v1&gt; published</b>\n<a href=\"https://x.test/\">Open upstream</a>" {
		t.Fatalf("request: %s %s", got.path, got.body)
	}

	bad, _ := receiver(t, http.StatusBadRequest)
	s.TelegramURL = bad.URL
	err := s.Send(context.Background(), ch, d)
	if err == nil || err.Error() != "telegram: HTTP 400: Bad Request: chat not found" {
		t.Fatalf("error: %v", err)
	}
	s.TelegramURL = "http://127.0.0.1:1"
	if err := s.Send(context.Background(), ch, d); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("unreachable API: the error must not carry the token: %v", err)
	}
}

func TestSendErrors(t *testing.T) {
	s := newSender(t)
	for _, ch := range []Channel{
		{Type: TypeEmail, Config: json.RawMessage(`{"to":["a@b.test"]}`)},
		{Type: TypeEmail, Config: json.RawMessage(`[]`)},
		{Type: TypeTelegram, Config: json.RawMessage(`[]`)},
		{Type: TypeNextcloudTalk, Config: json.RawMessage(`[]`)},
		{Type: TypeNextcloudTalk, Config: json.RawMessage(`{"url":"::","conversation":"abcd"}`)},
		{Type: TypeWebhook, Config: json.RawMessage(`[]`)},
		{Type: TypeWebhook, Config: json.RawMessage(`{"url":"::"}`)},
		{Type: "pager"},
	} {
		if err := s.Send(context.Background(), ch, sample); err == nil {
			t.Errorf("%s %s: want an error", ch.Type, ch.Config)
		}
	}
}

// 003-AC4: Silent maps to disable_notification; a stored config without the key stays loud.
func TestTelegramSilent(t *testing.T) {
	for _, tt := range []struct {
		config string
		want   bool
	}{{`{"chat_id":"-1","silent":true}`, true}, {`{"chat_id":"-1","silent":false}`, false}, {`{"chat_id":"-1"}`, false}} {
		srv, got := receiver(t, http.StatusOK)
		s := newSender(t, "127.0.0.0/8")
		s.TelegramURL = srv.URL
		if err := s.Send(context.Background(), Channel{Type: TypeTelegram, Config: json.RawMessage(tt.config), Secret: "1:token"}, sample); err != nil {
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(got.body, &msg); err != nil {
			t.Fatal(err)
		}
		if msg["disable_notification"] != tt.want {
			t.Errorf("%s: disable_notification = %v", tt.config, msg["disable_notification"])
		}
	}
}

const talkSecret = "0123456789012345678901234567890123456789" //nolint:gosec // test secret

func talkChannel(url, conversation string, silent bool) Channel {
	cfg, _ := json.Marshal(NextcloudTalkConfig{URL: url, Conversation: conversation, Silent: silent})
	return Channel{Type: TypeNextcloudTalk, Config: cfg, Secret: talkSecret}
}

// 003-AC2: vector computed outside Go with Python's hmac module. Talk verifies
// hash_hmac('sha256', $random . $message, $secret), hex, with $message the decoded JSON field:
// nextcloud/spreed e224e2d, lib/Controller/BotController.php getBotFromHeaders and
// lib/Service/ChecksumVerificationService.php validateRequest.
func TestTalkSignature(t *testing.T) {
	const want = "ddd07d2f0c874d5643b6929cddb9b803755209f054bd32eb8938a19f7a94dd3a"
	if got := TalkSignature(talkSecret, "abcdefabcdefabcdefabcdefabcdefab", "**o/r: é <v1>**"); got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
}

// 003-AC2: one signed request; the signature covers the decoded message, referenceId and silent follow.
func TestTalkRequest(t *testing.T) {
	srv, got := receiver(t, http.StatusCreated)
	s := newSender(t, "127.0.0.0/8")
	d := sample
	d.Event.Payload = map[string]any{"tag": "<v1>_*", "name": "see [x](https://evil.test)", "url": "https://x.test/a b(c)"}
	// A trailing slash on the base URL is tolerated.
	if err := s.Send(context.Background(), talkChannel(srv.URL+"/nc/", "abcd1234", true), d); err != nil {
		t.Fatal(err)
	}
	if got.path != "/nc/ocs/v2.php/apps/spreed/api/v1/bot/abcd1234/message" {
		t.Fatalf("path %s", got.path)
	}
	for h, want := range map[string]string{"Ocs-Apirequest": "true", "Content-Type": "application/json", "Accept": "application/json", "User-Agent": "gedour/test"} {
		if got.header.Get(h) != want {
			t.Errorf("header %s = %q", h, got.header.Get(h))
		}
	}
	var body struct {
		Message     string `json:"message"`
		ReferenceID string `json:"referenceId"`
		Silent      bool   `json:"silent"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	const wantText = "**o/r: \\<v1\\>\\_\\* published**\nsee \\[x\\](https://evil.test)\n[Open upstream](https://x.test/a%20b%28c%29)"
	if body.Message != wantText {
		t.Fatalf("message %q", body.Message)
	}
	// sha256("42"), the delivery id.
	if body.ReferenceID != "73475cb40a568e8da8a045ced110137e159f890ac4da883b6b17dc651b3a8049" || !body.Silent {
		t.Fatalf("body: %s", got.body)
	}
	random := got.header.Get(TalkRandomHeader)
	if raw, err := hex.DecodeString(random); err != nil || len(raw) != 32 {
		t.Fatalf("random %q", random)
	}
	mac := hmac.New(sha256.New, []byte(talkSecret))
	mac.Write([]byte(random + body.Message))
	if got.header.Get(TalkSignatureHeader) != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature does not cover random + decoded message")
	}
}

// 003-AC2: the fake server checks the signature the way Talk does, on messages JSON would re-encode.
func TestTalkAgainstFake(t *testing.T) {
	srv := httptest.NewServer(testupstream.New())
	defer srv.Close()
	s := newSender(t, "127.0.0.0/8")
	ch := talkChannel(srv.URL, "abcd", false)
	ch.Secret = testupstream.TalkSecret
	d := sample
	for _, tag := range []string{"plain", "<&>\u2028é🙂", "bad\xffutf8"} {
		d.Event.Payload = map[string]any{"tag": tag}
		if err := s.Send(context.Background(), ch, d); err != nil {
			t.Errorf("tag %q: %v", tag, err)
		}
	}
}

// 003-AC3: only 201 sent; 401, 404, 413 permanent; everything else retried. No secret in any error.
func TestTalkStatus(t *testing.T) {
	for _, tt := range []struct {
		code      int
		permanent bool
	}{
		{201, false},
		{200, false}, {202, false}, {204, false},
		{400, false}, {401, true}, {404, true}, {413, true},
		{302, false}, {403, false}, {429, false}, {500, false}, {502, false}, {503, false},
	} {
		srv, got := receiver(t, tt.code)
		err := newSender(t, "127.0.0.0/8").Send(context.Background(), talkChannel(srv.URL, "abcd", false), sample)
		if tt.code == http.StatusCreated {
			if err != nil {
				t.Errorf("%d: %v", tt.code, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "POST "+strings.TrimPrefix(srv.URL, "http://")+": HTTP") {
			t.Errorf("%d: error %v", tt.code, err)
			continue
		}
		if errors.Is(err, ErrPermanent) != tt.permanent {
			t.Errorf("%d: permanent = %v, want %v", tt.code, errors.Is(err, ErrPermanent), tt.permanent)
		}
		for _, leak := range []string{talkSecret, got.header.Get(TalkSignatureHeader), got.header.Get(TalkRandomHeader), "abcd"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%d: error carries %q: %v", tt.code, leak, err)
			}
		}
	}
	// A proxy or SSO login page answering 200 is not a post.
	sso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>Sign in</html>"))
	}))
	defer sso.Close()
	if err := newSender(t, "127.0.0.0/8").Send(context.Background(), talkChannel(sso.URL, "abcd", false), sample); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("200 HTML: %v", err)
	}
	// Unreachable server or timeout: retried, and no secret either.
	err := newSender(t, "127.0.0.0/8").Send(context.Background(), talkChannel("http://127.0.0.1:1", "abcd", false), sample)
	if err == nil || errors.Is(err, ErrPermanent) || strings.Contains(err.Error(), talkSecret) {
		t.Fatalf("unreachable: %v", err)
	}
}

// 003-AC6: a private target is refused at send time, with the webhook's error, unless allowlisted.
func TestTalkPrivateTarget(t *testing.T) {
	srv, got := receiver(t, http.StatusCreated)
	s := newSender(t)
	talkErr := s.Send(context.Background(), talkChannel(srv.URL, "abcd", false), sample)
	hookErr := s.Send(context.Background(), webhookChannel(srv.URL), sample)
	if !errors.Is(talkErr, netguard.ErrBlocked) || got.body != nil {
		t.Fatalf("loopback target without allowlist: %v", talkErr)
	}
	if hookErr == nil || talkErr.Error() != hookErr.Error() {
		t.Fatalf("talk error %q, webhook error %q", talkErr, hookErr)
	}
	if err := newSender(t, "127.0.0.0/8").Send(context.Background(), talkChannel(srv.URL, "abcd", false), sample); err != nil {
		t.Fatalf("allowlisted: %v", err)
	}
}

// 003-AC7: over the cap, the title and link; never over it, however long the parts.
func TestTalkTextCap(t *testing.T) {
	long := strings.Repeat("🙂", maxTalkLen)
	short := TalkText(Message{Title: "o/r: v1 published", Detail: "kept", URL: "https://x.test/"})
	if short != "**o/r: v1 published**\nkept\n[Open upstream](https://x.test/)" {
		t.Fatalf("short: %q", short)
	}
	// Multi-byte text under the cap counts in characters, as Talk does: it is not shortened.
	fits := strings.Repeat("🙂", maxTalkLen-100)
	if got := TalkText(Message{Title: "t", Detail: fits}); got != "**t**\n"+fits {
		t.Fatalf("under the cap was shortened to %d characters", utf8.RuneCountInString(got))
	}
	if got := TalkText(Message{Title: "t", Detail: long, URL: "https://x.test/"}); got != "**t**\n[Open upstream](https://x.test/)" {
		t.Fatalf("long detail: %q", got)
	}
	if maxTalkLen >= 32000 {
		t.Fatalf("maxTalkLen %d: Talk refuses over 32000 characters", maxTalkLen)
	}
	const link = "\n[Open upstream](https://x.test/)"
	for name, title := range map[string]string{
		"40000 plain":  strings.Repeat("a", 40000),
		"16000 stars":  strings.Repeat("*", 16000),
		"odd boundary": "a" + strings.Repeat("*", 16000),
		"multi-byte":   strings.Repeat("🙂", 40000),
	} {
		got := TalkText(Message{Title: title, Detail: "dropped", URL: "https://x.test/"})
		if n := utf8.RuneCountInString(got); n > maxTalkLen || n < maxTalkLen-1 {
			t.Errorf("%s: %d characters", name, n)
		}
		if !strings.HasPrefix(got, "**") || !strings.HasSuffix(got, "**"+link) || strings.Contains(got, "dropped") {
			t.Errorf("%s: the link must survive a cut title: ...%s", name, got[max(len(got)-60, 0):])
		}
		kept := strings.TrimSuffix(strings.TrimPrefix(got, "**"), "**"+link)
		if strings.Count(kept, `\`) != strings.Count(kept, `*`) {
			t.Errorf("%s: cut inside an escape sequence", name)
		}
	}
	// The URL limit holds in every branch of the cap, whatever else is over it.
	urlOf := func(n int) string { return "https://x.test/" + strings.Repeat("a", n-len("https://x.test/")) }
	for _, tt := range []struct {
		name          string
		title, detail string
		urlLen        int
		kept          bool
	}{
		{"typical, fits", "t", "", 200, true},
		{"limit, fits", "t", "", 30000, true},
		{"over, fits", "t", "", 30001, false},
		{"typical, long detail", "t", strings.Repeat("d", 40000), 200, true},
		{"limit, long detail", "t", strings.Repeat("d", 40000), 30000, true},
		{"over, long detail", "t", strings.Repeat("d", 40000), 30001, false},
		{"over, 500-char title, long detail", strings.Repeat("t", 500), strings.Repeat("d", 40000), 30001, false},
		{"typical, huge title", strings.Repeat("t", 40000), "", 200, true},
		{"limit, huge title", strings.Repeat("t", 40000), "", 30000, true},
		{"over, huge title", strings.Repeat("t", 40000), "", 30001, false},
	} {
		url := urlOf(tt.urlLen)
		got := TalkText(Message{Title: tt.title, Detail: tt.detail, URL: url})
		if strings.Contains(got, "("+url+")") != tt.kept {
			t.Errorf("%s: link kept = %v", tt.name, !tt.kept)
		}
		if n := utf8.RuneCountInString(got); n > 31000 || !strings.HasPrefix(got, "**t") {
			t.Errorf("%s: %d characters", tt.name, n)
		}
	}
	// The limit counts the encoded URL: what goes into the message.
	encoded := "https://x.test/" + strings.Repeat("(", 10000)
	if got := TalkText(Message{Title: "t", URL: encoded}); got != "**t**" {
		t.Errorf("URL over the limit once encoded: %.40q", got)
	}
	for name, m := range map[string]Message{
		"title":     {Title: strings.Repeat("*", maxTalkLen), URL: "https://x.test/"},
		"url":       {Title: "t", URL: "https://x.test/" + strings.Repeat("a", maxTalkLen)},
		"all three": {Title: long, Detail: long, URL: "https://x.test/" + long},
	} {
		if n := utf8.RuneCountInString(TalkText(m)); n > maxTalkLen || n == 0 {
			t.Errorf("%s: %d characters", name, n)
		}
	}
}

// 003-AC2: upstream text cannot mention anyone, whatever the mention form. Talk's pattern
// (nextcloud/server lib/private/Comments/Comment.php extractMentions) refuses a backslash before the @.
func TestTalkTextMentions(t *testing.T) {
	bare := regexp.MustCompile(`(^|[^\\])@`)
	for _, mention := range []string{"@all urgent", "x-@all", `@"user/alice"`, `@"group/ops"`, "@alice", "a @\"federated_user/b@c\""} {
		for name, m := range map[string]Message{
			"title":  {Title: mention},
			"detail": {Title: "t", Detail: mention},
			"url":    {Title: "t", URL: "https://x.test/" + mention},
		} {
			if got := TalkText(m); bare.MatchString(got) {
				t.Errorf("%s %q: unescaped @ in %q", name, mention, got)
			}
		}
		if got := TalkText(Message{Title: "t", URL: "https://x.test/" + mention}); strings.Contains(got, "@") {
			t.Errorf("link %q keeps an @: %q", mention, got)
		}
	}
}

// 003-AC2: each character the escapers handle, in each place: removing any entry goes red.
func TestTalkTextEscapes(t *testing.T) {
	inline := map[string]string{`\`: `\\`, "`": "\\`", "~": `\~`, "*": `\*`, "_": `\_`, "[": `\[`, "]": `\]`, "<": `\<`, ">": `\>`, "@": `\@`}
	for c, want := range inline {
		if got := TalkText(Message{Title: "a" + c}); got != "**a"+want+"**" {
			t.Errorf("title %q: %q", c, got)
		}
		if got := TalkText(Message{Title: "t", Detail: "a" + c}); got != "**t**\na"+want {
			t.Errorf("detail %q: %q", c, got)
		}
	}
	for c, want := range map[string]string{"(": "%28", ")": "%29", "<": "%3C", ">": "%3E", " ": "%20", "@": "%40", `\`: "%5C"} {
		if got := TalkText(Message{Title: "t", URL: "https://x.test/a" + c}); got != "**t**\n[Open upstream](https://x.test/a"+want+")" {
			t.Errorf("url %q: %q", c, got)
		}
	}
	// Every ASCII control character, so a URL holding one still renders as a link.
	controls := []byte{0x7f}
	for c := range byte(0x20) {
		controls = append(controls, c)
	}
	for _, c := range controls {
		want := fmt.Sprintf("**t**\n[Open upstream](https://x.test/a%%%02Xb)", c)
		if got := TalkText(Message{Title: "t", URL: "https://x.test/a" + string(rune(c)) + "b"}); got != want {
			t.Errorf("url control %#x: %q", c, got)
		}
	}
	// Other bytes pass untouched, multi-byte characters included.
	if got := TalkText(Message{Title: "t", URL: "https://x.test/é-1?a=b&c=d#e"}); got != "**t**\n[Open upstream](https://x.test/é-1?a=b&c=d#e)" {
		t.Errorf("plain url: %q", got)
	}
	// Line breaks in a title or detail never reach Talk: Format flattens them.
	for _, c := range []string{"\r", "\n"} {
		d := sample
		d.Event.Payload = map[string]any{"tag": "a" + c + "b", "title": "c" + c + "d"}
		if got := TalkText(Format(d)); strings.Contains(got, "\r") || strings.Count(got, "\n") != 1 {
			t.Errorf("line break %q: %q", c, got)
		}
	}
}
