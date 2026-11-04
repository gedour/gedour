package route

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func (e *env) scriptChannel(t *testing.T, owner, src string) string {
	t.Helper()
	var sid, cid string
	e.scalar(t, &sid, `INSERT INTO scripts (owner_id, name, source) VALUES ($1, gen_random_uuid()::text, $2) RETURNING id::text`, owner, src)
	cfg, _ := json.Marshal(ScriptConfig{ScriptID: sid})
	e.scalar(t, &cid, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, gen_random_uuid()::text, 'script', $2) RETURNING id::text`, owner, cfg)
	return cid
}

// 001-AC17: a script channel runs its script on the routed event; its http.fetch reaches the
// (allowlisted) receiver, its logs land on the delivery, and a throw is a failed attempt that retries.
func TestScriptDelivery(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var bodies []string
	recv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Header.Get("X-Rule")+" "+string(b))
		mu.Unlock()
	}))
	t.Cleanup(recv.Close)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ok := e.scriptChannel(t, alice, `function main(event, ctx) {
		log("tag", event.payload.tag, "delivery", ctx.delivery_id > 0);
		const r = http.fetch("`+recv.URL+`/hook", {method: "POST", headers: {"X-Rule": ctx.rule.name}, body: event.summary});
		log("status", r.status);
	}`)
	bad := e.scriptChannel(t, alice, `function main() { log("before"); throw new Error("receiver said no") }`)
	e.rule(t, alice, &w, "", ok, bad)
	e.event(t, w, "release.published", "v1.0.0")
	routeAll(t, e.router())
	d := e.deliverer(t)
	if n, err := d.DeliverOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("delivered %d: %v", n, err)
	}
	var logs string
	var lastErr *string
	e.scalar(t, &logs, `SELECT logs FROM deliveries WHERE channel_id = $1 AND status = 'sent'`, ok)
	if logs != "tag v1.0.0 delivery true\nstatus 200" {
		t.Fatalf("logs %q", logs)
	}
	if len(bodies) != 1 || !strings.HasSuffix(bodies[0], " v1.0.0 published") || strings.HasPrefix(bodies[0], " ") {
		t.Fatalf("receiver got %q", bodies)
	}
	e.scalar(t, &lastErr, `SELECT last_error FROM deliveries WHERE channel_id = $1 AND status = 'failed' AND attempts = 1`, bad)
	e.scalar(t, &logs, `SELECT logs FROM deliveries WHERE channel_id = $1`, bad)
	if lastErr == nil || !strings.Contains(*lastErr, "receiver said no") || logs != "before" {
		t.Fatalf("failed run: %v, logs %q", lastErr, logs)
	}
	e.makeDue(t)
	if n, err := d.DeliverOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("retried %d: %v", n, err)
	}
	if e.count(t, `SELECT attempts FROM deliveries WHERE channel_id = $1`, bad) != 2 {
		t.Fatal("throw not retried")
	}
	if testutil.ToFloat64(e.m.deliveries.WithLabelValues("script", "sent")) != 1 || testutil.ToFloat64(e.m.deliveries.WithLabelValues("script", "failed")) != 2 {
		t.Fatal("script deliveries not counted")
	}
}
