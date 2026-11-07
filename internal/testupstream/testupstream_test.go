package testupstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/notify/webpush"
)

func post(t *testing.T, url string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(`{"x":1}`))
	req.Header.Set("X-Gedour-Signature", "sha256=00")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The fake channel endpoints record what they get, in order, bounded; "fail*" hooks answer 500.
func TestReceivers(t *testing.T) {
	up := New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	for _, tt := range []struct {
		path string
		code int
	}{{"/tg/bot1:abc/sendMessage", 200}, {"/_hooks/ok", 200}, {"/_hooks/fail-1", 500}} {
		if got := post(t, srv.URL+tt.path); got != tt.code {
			t.Errorf("%s: %d", tt.path, got)
		}
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/_received", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got []Received
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Endpoint != "telegram" || got[1].Endpoint != "ok" || got[2].Header["X-Gedour-Signature"] != "sha256=00" || got[2].Body != `{"x":1}` {
		t.Fatalf("received: %+v", got)
	}
	for range maxReceived {
		post(t, srv.URL+"/_hooks/more")
	}
	if all := up.Received(); len(all) != maxReceived || all[0].Endpoint != "more" {
		t.Fatalf("kept %d, oldest %q", len(all), all[0].Endpoint)
	}
}

// 003-AC2: the fake Talk server accepts only a valid signature, and forces 404 and 429 by token.
func TestTalk(t *testing.T) {
	up := New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	const random = "0123456789abcdef0123456789abcdef"
	sign := func(secret, msg string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(random + msg))
		return hex.EncodeToString(mac.Sum(nil))
	}
	good := sign(TalkSecret, "hi")
	for _, tt := range []struct {
		name, token, ocs, random, signature, message string
		code                                         int
	}{
		{"valid", "room", "true", random, good, "hi", 201},
		{"wrong secret", "room", "true", random, sign("other", "hi"), "hi", 401},
		{"signature of another message", "room", "true", random, good, "other", 401},
		{"no OCS header", "room", "", random, good, "hi", 412},
		{"short random", "room", "true", "short", good, "hi", 400},
		{"no signature", "room", "true", random, "", "hi", 400},
		{"empty message", "room", "true", random, sign(TalkSecret, " "), " ", 400},
		{"too long", "room", "true", random, sign(TalkSecret, strings.Repeat("a", 32001)), strings.Repeat("a", 32001), 413},
		{"forced 404", "force404x", "true", random, good, "hi", 404},
		{"forced 429", "force429x", "true", random, good, "hi", 429},
	} {
		body, _ := json.Marshal(map[string]string{"message": tt.message})
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/ocs/v2.php/apps/spreed/api/v1/bot/"+tt.token+"/message", strings.NewReader(string(body)))
		req.Header.Set("OCS-APIRequest", tt.ocs)
		req.Header.Set("X-Nextcloud-Talk-Bot-Random", tt.random)
		req.Header.Set("X-Nextcloud-Talk-Bot-Signature", tt.signature)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // test server
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.code {
			t.Errorf("%s: %d, want %d", tt.name, resp.StatusCode, tt.code)
		}
	}
	got := up.Received()
	if got[0].Endpoint != "talk-room" || got[1].Endpoint != "talk-room-rejected" || got[len(got)-1].Endpoint != "talk-force429x-rejected" {
		t.Fatalf("received: %+v", got)
	}
}

// 011-AC4: the fake pages the discussions list like GitHub, so the e2e run takes the probe-then-page path.
func TestDiscussionsPaging(t *testing.T) {
	srv := httptest.NewServer(New())
	defer srv.Close()
	get := func(q string) (string, []map[string]any) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/repos/cli/cli/discussions?"+q, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var items []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			t.Fatal(err)
		}
		return resp.Header.Get("Link"), items
	}
	if link, items := get("per_page=1"); !strings.Contains(link, `per_page=1&page=2>; rel="last"`) || len(items) != 1 || items[0]["number"] != 9507.0 {
		t.Fatalf("probe: %q %v", link, items)
	}
	if link, items := get("per_page=100&page=1"); link != "" || len(items) != 2 {
		t.Fatalf("page: %q %d", link, len(items))
	}
}

// 010-AC6: the fake push service records a payload it decrypts under a VAPID token that verifies, and
// refuses the rest like a push service.
func TestPush(t *testing.T) {
	up := New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	k, _ := webpush.GenerateKey()
	other, _ := webpush.GenerateKey()
	auth := func(key *webpush.Key, endpoint string) string {
		h, err := key.Authorization(endpoint, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	body, err := webpush.Encrypt([]byte("hello"), PushSubscription(""))
	if err != nil {
		t.Fatal(err)
	}
	_, servedKey, _ := webpush.ParseAuthorization(auth(k, srv.URL))
	otherToken, _, _ := webpush.ParseAuthorization(auth(other, srv.URL))
	for _, tt := range []struct {
		name, id, authz, encoding string
		body                      []byte
		code                      int
	}{
		{"valid", "dev", auth(k, srv.URL), "aes128gcm", body, 201},
		{"gone", "gone-1", auth(k, srv.URL), "aes128gcm", body, 410},
		{"fail", "fail-1", auth(k, srv.URL), "aes128gcm", body, 500},
		{"other origin", "dev", auth(k, "https://elsewhere.example"), "aes128gcm", body, 403},
		{"token of another key", "dev", "vapid t=" + otherToken + ", k=" + servedKey, "aes128gcm", body, 403},
		{"no vapid", "dev", "Bearer x", "aes128gcm", body, 403},
		{"other encoding", "dev", auth(k, srv.URL), "aesgcm", body, 400},
		{"not decryptable", "dev", auth(k, srv.URL), "aes128gcm", []byte("garbage"), 400},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/push/"+tt.id, bytes.NewReader(tt.body))
		req.Header.Set("Authorization", tt.authz)
		req.Header.Set("Content-Encoding", tt.encoding)
		req.Header.Set("TTL", "60")
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // test server
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.code {
			t.Errorf("%s: %d, want %d", tt.name, resp.StatusCode, tt.code)
		}
	}
	got := up.Received()
	if got[0].Endpoint != "push-dev" || got[0].Body != "hello" || got[1].Endpoint != "push-gone-1-rejected" || got[len(got)-1].Body != "" {
		t.Fatalf("received: %+v", got)
	}
	// hang* never answers: the sender's deadline ends the request, and nothing is recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/push/hang-1", bytes.NewReader(body))
	if resp, err := http.DefaultClient.Do(req); !errors.Is(err, context.DeadlineExceeded) { //nolint:gosec // test server
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatalf("hang: %v", err)
	}
	if n := len(up.Received()); n != len(got) {
		t.Fatalf("hang recorded: %d entries, want %d", n, len(got))
	}
}
