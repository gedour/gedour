package testupstream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
