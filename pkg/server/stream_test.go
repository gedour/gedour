package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/route"
	"github.com/gedour/gedour/pkg/wake"
)

type replica struct {
	*httptest.Server
	cookie string
}

// replica serves the full handler on the same database as a, like a second web pod, with its own hub.
// With oidc, sessions are real ones (no dev fallback), so a sign-out shows.
func (a *devApp) replica(t *testing.T, heartbeat time.Duration, oidc bool) replica {
	t.Helper()
	cfg := config.Config{DevUser: "dev@local", SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}
	cookie := auth.SessionCookie
	if oidc {
		cfg.DevUser, cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.PublicURL = "", "http://127.0.0.1:1", "c", "s", "https://app.test"
		cookie = auth.SecureSessionCookie
	}
	authn, err := auth.New(cfg, a.pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	routes := testRoutes(t, a.pool, testSender())
	routes.Heartbeat = heartbeat
	ctx, cancel := context.WithCancel(context.Background())
	go routes.RunHub(ctx)
	srv := httptest.NewServer(New(zap.NewNop(), build, pingOK, fstest.MapFS{"index.html": {Data: []byte("SPA")}}, authn, a.pool, testWatches(t, a.pool), routes, nil))
	t.Cleanup(func() { cancel(); srv.Close() })
	return replica{srv, cookie}
}

type sseEvent struct{ name, data string }

// openStream connects u to the inbox stream of srv and yields its events; comments come as name ":".
func openStream(t *testing.T, srv replica, u apiUser) (*http.Response, <-chan sseEvent) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/inbox/stream", nil)
	req.Header.Set("Cookie", srv.cookie+"="+u.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				out <- sseEvent{name: ":"}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				out <- ev
				ev = sseEvent{}
			}
		}
	}()
	return resp, out
}

// next returns the next event named name (others skipped) within d.
func next(t *testing.T, events <-chan sseEvent, name string, d time.Duration) sseEvent {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream closed while waiting for %q", name)
			}
			if ev.name == name {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %q event within %s", name, d)
		}
	}
}

// 001-AC19: an item routed by a worker reaches a stream open on another web replica within 2 s.
func TestInboxStreamAcrossReplicas(t *testing.T) {
	a := newDevApp(t)
	one, two := a.replica(t, time.Hour, false), a.replica(t, 50*time.Millisecond, true)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "All"}, http.StatusCreated, nil)

	resp, events := openStream(t, two, alice)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" ||
		resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	if ev := next(t, events, "inbox", 2*time.Second); ev.data != `{"unread":0}` {
		t.Fatalf("initial count: %+v", ev)
	}
	next(t, events, ":", 2*time.Second) // heartbeat comment

	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', '{"tag":"v1"}', 'k')`, w.ID)
	start := time.Now()
	if n, err := route.NewRouter(a.pool, zap.NewNop(), route.NewMetrics(prometheus.NewRegistry()), wake.NewLoop("router", time.Second, nil, nil)).RouteOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("route: %d %v", n, err)
	}
	if ev := next(t, events, "inbox", 2*time.Second); ev.data != `{"unread":1}` {
		t.Fatalf("after routing: %+v", ev)
	}
	t.Logf("item visible on the other replica after %s", time.Since(start))

	// A bulk read on replica one updates the badge of the stream on replica two.
	var ids inboxJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &ids)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, one.URL+"/api/v1/inbox/read", strings.NewReader(`{"ids":[`+strconv.FormatInt(ids.Items[0].ID, 10)+`]}`))
	req.Header.Set("Cookie", auth.SessionCookie+"="+alice.token)
	req.Header.Set(auth.CSRFHeader, alice.csrf)
	req.Header.Set("Content-Type", "application/json")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("bulk read on replica one: %v %v", r, err)
	} else {
		_ = r.Body.Close()
	}
	if ev := next(t, events, "inbox", 2*time.Second); ev.data != `{"unread":0}` {
		t.Fatalf("after read elsewhere: %+v", ev)
	}

	// Signing out ends the stream at the next heartbeat.
	a.exec(t, `DELETE FROM sessions WHERE user_id = $1`, alice.id)
	next(t, events, "signed_out", 2*time.Second)
	select {
	case _, ok := <-events:
		if ok {
			for range events {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream still open after sign-out")
	}
}

// One user cannot hold more than MaxStreamsPerUser streams on a replica.
func TestInboxStreamLimit(t *testing.T) {
	a := newDevApp(t)
	srv := a.replica(t, time.Hour, false)
	alice := a.signIn(t, "alice")
	for range route.MaxStreamsPerUser {
		_, events := openStream(t, srv, alice)
		next(t, events, "inbox", 2*time.Second)
	}
	resp, _ := openStream(t, srv, alice)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("stream %d: %d", route.MaxStreamsPerUser+1, resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/inbox/stream", nil)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusOK {
		// Dev mode without a cookie runs as the dev user, a different user with its own quota.
		t.Fatalf("cookieless dev stream: %v %v", r, err)
	} else {
		_ = r.Body.Close()
	}
}

// Heartbeats re-check the session without touching it: an open tab does not keep an idle session
// alive, and the stream ends once the idle timeout passes.
func TestInboxStreamIdleTimeout(t *testing.T) {
	a := newDevApp(t)
	srv := a.replica(t, 30*time.Millisecond, true)
	alice := a.signIn(t, "alice")
	_, events := openStream(t, srv, alice)
	next(t, events, "inbox", 2*time.Second)
	// Stale enough that any touch would move it (touches happen past one minute), still within the hour.
	a.exec(t, `UPDATE sessions SET last_seen_at = now() - interval '10 minutes' WHERE user_id = $1`, alice.id)
	var before time.Time
	if err := a.pool.QueryRow(t.Context(), `SELECT last_seen_at FROM sessions WHERE user_id = $1`, alice.id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		next(t, events, ":", 2*time.Second)
	}
	var after time.Time
	if err := a.pool.QueryRow(t.Context(), `SELECT last_seen_at FROM sessions WHERE user_id = $1`, alice.id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("heartbeats moved last_seen_at from %s to %s", before, after)
	}
	a.exec(t, `UPDATE sessions SET last_seen_at = now() - interval '2 hours' WHERE user_id = $1`, alice.id)
	next(t, events, "signed_out", 2*time.Second)
}
