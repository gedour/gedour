package notify

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	Watch: Watch{ID: "w1", Name: "o/r", Kind: "github_release"},
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
	if got.path != "/bot"+token+"/sendMessage" || msg["chat_id"] != "-1001" || msg["parse_mode"] != "HTML" || msg["disable_web_page_preview"] != true ||
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
		{Type: TypeWebhook, Config: json.RawMessage(`[]`)},
		{Type: TypeWebhook, Config: json.RawMessage(`{"url":"::"}`)},
		{Type: "pager"},
	} {
		if err := s.Send(context.Background(), ch, sample); err == nil {
			t.Errorf("%s %s: want an error", ch.Type, ch.Config)
		}
	}
}
