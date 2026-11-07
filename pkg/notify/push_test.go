package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify/webpush"
)

func pushSender(t *testing.T) *Sender {
	t.Helper()
	s := newSender(t, "127.0.0.0/8")
	k, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s.VAPID, s.VAPIDSubject, s.HTTPHosts, s.Now = k, "https://gedour.example", []string{"127.0.0.1"}, time.Now
	return s
}

// 010-AC6: the fake push service verifies the VAPID token and decrypts the payload with the subscription's keys.
func TestPushRoundTrip(t *testing.T) {
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	s := pushSender(t)
	payload := []byte(`{"title":"o/r v1.2.3"}`)
	if err := s.Push(context.Background(), testupstream.PushSubscription(srv.URL+"/push/dev1"), payload, UrgencyHigh); err != nil {
		t.Fatal(err)
	}
	got := up.Received()
	if len(got) != 1 || got[0].Endpoint != "push-dev1" || got[0].Body != string(payload) {
		t.Fatalf("received: %+v", got)
	}
	h := got[0].Header
	if h["Urgency"] != UrgencyHigh || h["Ttl"] != "86400" || h["Content-Encoding"] != "aes128gcm" || h["Content-Type"] != "application/octet-stream" {
		t.Errorf("headers: %v", h)
	}
	token, key, err := webpush.ParseAuthorization(h["Authorization"])
	if err != nil || key != s.VAPID.PublicKey() || webpush.VerifyJWT(token, s.VAPID.PublicKey(), srv.URL, time.Now()) != nil {
		t.Errorf("vapid header %q does not verify against the served key: %v", h["Authorization"], err)
	}

	// A key the subscription does not hold: the fake cannot decrypt.
	bad := testupstream.PushSubscription(srv.URL + "/push/dev2")
	bad.Auth = make([]byte, 16)
	if err := s.Push(context.Background(), bad, payload, UrgencyNormal); !errors.Is(err, ErrPermanent) || errors.Is(err, ErrGone) {
		t.Errorf("undecryptable: %v", err)
	}
	if err := s.Push(context.Background(), testupstream.PushSubscription(srv.URL+"/push/gone1"), payload, UrgencyLow); !errors.Is(err, ErrGone) {
		t.Errorf("gone id: %v", err)
	}
}

// 010-AC6: the push service's answers map to gone, permanent or retryable; the endpoint path never reaches an error.
func TestPushErrors(t *testing.T) {
	for _, tt := range []struct {
		code            int
		gone, permanent bool
	}{
		{http.StatusCreated, false, false},
		{http.StatusNotFound, true, true},
		{http.StatusGone, true, true},
		{http.StatusBadRequest, false, true},
		{http.StatusUnauthorized, false, true},
		{http.StatusForbidden, false, true},
		{http.StatusRequestEntityTooLarge, false, true},
		{http.StatusTooManyRequests, false, false},
		{http.StatusInternalServerError, false, false},
		{http.StatusServiceUnavailable, false, false},
	} {
		srv, _ := receiver(t, tt.code)
		err := pushSender(t).Push(context.Background(), testupstream.PushSubscription(srv.URL+"/secret-path"), []byte("x"), UrgencyNormal)
		if tt.code == http.StatusCreated {
			if err != nil {
				t.Errorf("%d: %v", tt.code, err)
			}
			continue
		}
		if err == nil || errors.Is(err, ErrGone) != tt.gone || errors.Is(err, ErrPermanent) != tt.permanent {
			t.Errorf("%d: %v (gone %v, permanent %v)", tt.code, err, errors.Is(err, ErrGone), errors.Is(err, ErrPermanent))
		} else if strings.Contains(err.Error(), "secret-path") {
			t.Errorf("%d: error carries the endpoint path: %v", tt.code, err)
		}
		if inner := errors.Unwrap(err); (tt.gone || tt.permanent) && (inner == nil || !strings.Contains(inner.Error(), fmt.Sprintf("HTTP %d", tt.code))) {
			t.Errorf("%d: unwraps to %v", tt.code, inner)
		}
	}
}

func TestPushRefusals(t *testing.T) {
	ctx := context.Background()
	sub := testupstream.PushSubscription("http://127.0.0.1:1/push/x")
	s := pushSender(t)
	for _, tt := range []struct {
		name      string
		s         *Sender
		sub       webpush.Subscription
		payload   []byte
		urgency   string
		permanent bool
		cause     error
	}{
		{"no key", &Sender{}, sub, nil, UrgencyNormal, false, nil},
		{"bad urgency", s, sub, nil, "urgent", false, nil},
		{"no host", s, testupstream.PushSubscription("/push/x"), nil, UrgencyNormal, true, nil},
		{"plain http", s, testupstream.PushSubscription("http://push.example/x"), nil, UrgencyNormal, true, netguard.ErrBlocked},
		{"too large", s, sub, make([]byte, webpush.MaxPayload+1), UrgencyNormal, true, webpush.ErrTooLarge},
		{"short auth", s, webpush.Subscription{Endpoint: sub.Endpoint, P256DH: sub.P256DH, Auth: sub.Auth[:8]}, []byte("x"), UrgencyNormal, true, nil},
		{"network", s, sub, []byte("x"), UrgencyVeryLow, false, nil},
	} {
		err := tt.s.Push(ctx, tt.sub, tt.payload, tt.urgency)
		if err == nil || errors.Is(err, ErrPermanent) != tt.permanent || (tt.cause != nil && !errors.Is(err, tt.cause)) {
			t.Errorf("%s: %v", tt.name, err)
		}
		if strings.Contains(err.Error(), "web push: web push") {
			t.Errorf("%s: stutter: %v", tt.name, err)
		}
	}
	// The guard refuses a private address that is not allowlisted, as for webhooks.
	g, _ := netguard.New(nil)
	s.Client = g.Client(netguard.Options{Timeout: SendTimeout, HTTPHosts: s.HTTPHosts, NoRedirects: true})
	if err := s.Push(ctx, sub, []byte("x"), UrgencyNormal); !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("private address: %v", err)
	}
}

// 010-AC10: silent wins over a high priority rule; no rule (a test) or a normal one is normal.
func TestPushUrgency(t *testing.T) {
	for _, tt := range []struct {
		silent bool
		rule   *Rule
		want   string
	}{
		{false, nil, UrgencyNormal},
		{false, &Rule{Priority: "normal"}, UrgencyNormal},
		{false, &Rule{Priority: "high"}, UrgencyHigh},
		{true, nil, UrgencyLow},
		{true, &Rule{Priority: "high"}, UrgencyLow},
	} {
		if got := PushUrgency(tt.silent, tt.rule); got != tt.want {
			t.Errorf("silent %v rule %+v: %s, want %s", tt.silent, tt.rule, got, tt.want)
		}
	}
}

// The rule priority stays out of the webhook and script JSON.
func TestRulePriorityNotInJSON(t *testing.T) {
	b, _ := json.Marshal(Rule{ID: "r", Name: "n", Priority: "high"})
	if string(b) != `{"id":"r","name":"n"}` {
		t.Fatal(string(b))
	}
}

func TestPushPayload(t *testing.T) {
	d := Delivery{ID: 7, Event: Event{ID: 42, Type: "release.published", Payload: map[string]any{"tag": "v1", "name": "Big <release> & co"}}, Watch: Watch{Name: "cli/cli"}}
	var m PushMessage
	if err := json.Unmarshal(PushPayload(d, true), &m); err != nil {
		t.Fatal(err)
	}
	if m != (PushMessage{Title: "cli/cli: v1 published", Body: "Big <release> & co", URL: "/inbox?event=42", Tag: "gedour-event-42", Silent: true}) {
		t.Fatalf("%+v", m)
	}
	if err := json.Unmarshal(PushPayload(TestDelivery(time.Now()), false), &m); err != nil || m.URL != "/inbox" || m.Tag != "gedour-test" || m.Silent {
		t.Fatalf("test delivery: %+v %v", m, err)
	}
	// Over the limit: the body is cut first, then the title, on rune boundaries; quotes double when escaped.
	for _, tt := range []struct {
		name, title, detail string
		keepTitle           bool
	}{
		{"long body", "w", strings.Repeat("é", 3000), true},
		{"escaped body", "w", strings.Repeat(`"`, 3000), true},
		{"long title", strings.Repeat("ü", 3000), strings.Repeat("x", 100), false},
	} {
		d := Delivery{Event: Event{Type: "release.published", Payload: map[string]any{"tag": tt.title, "name": tt.detail}}, Watch: Watch{Name: "w"}}
		b := PushPayload(d, false)
		if len(b) > webpush.MaxPayload || !utf8.Valid(b) {
			t.Fatalf("%s: %d bytes, valid %v", tt.name, len(b), utf8.Valid(b))
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if full := Format(d).Title; (m.Title == full) != tt.keepTitle || (!tt.keepTitle && m.Body != "") || m.Body == tt.detail {
			t.Errorf("%s: title kept %v, body %d bytes", tt.name, m.Title == full, len(m.Body))
		}
	}
}
