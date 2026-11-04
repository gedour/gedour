package testupstream

import (
	"context"
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
