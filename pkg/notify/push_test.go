package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
