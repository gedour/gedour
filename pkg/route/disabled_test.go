package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// setInactive marks user disabled or purged (col is disabled_at or purged_at); "" makes it active again.
func (e *env) setInactive(t *testing.T, user, col string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET disabled_at = CASE WHEN $2 = 'disabled_at' THEN now() END,
		purged_at = CASE WHEN $2 = 'purged_at' THEN now() END WHERE id = $1`, user, col); err != nil {
		t.Fatal(err)
	}
}

// inactiveCols are the two states that stop an owner's routing and deliveries.
var inactiveCols = []string{"disabled_at", "purged_at"}

// 006-AC8: a disabled (or purged) owner's rules make no delivery and no inbox item. 006-AC9: re-enabled, its rules
// route again; skipped events stay skipped.
func TestRoutingSkipsDisabledOwner(t *testing.T) {
	for _, col := range inactiveCols {
		t.Run(col, func(t *testing.T) {
			e := newEnv(t)
			alice := e.user(t, "alice")
			w := e.watch(t, alice)
			aliceHook := e.webhook(t, alice, "https://hooks.example/a")
			e.rule(t, alice, &w, "", aliceHook)

			e.setInactive(t, alice, col)
			skipped := e.event(t, w, "release.published", "v1")
			routeAll(t, e.router())
			if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1`, alice) +
				e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1`, aliceHook); n != 0 {
				t.Fatalf("disabled owner got %d inbox items and deliveries", n)
			}

			e.setInactive(t, alice, "")
			routed := e.event(t, w, "release.published", "v2")
			routeAll(t, e.router())
			if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, alice, routed) +
				e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND event_id = $2`, aliceHook, routed); n != 2 {
				t.Fatalf("re-enabled owner: %d inbox items and deliveries, want 2", n)
			}
			if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE event_id = $1 AND user_id = $2`, skipped, alice); n != 0 {
				t.Fatal("re-enable replayed a skipped event")
			}
		})
	}
}

// 006-AC8: a delivery queued before its channel's owner was disabled (or purged) ends dead, unsent, saying why.
func TestDeliveryDisabledOwner(t *testing.T) {
	for _, col := range inactiveCols {
		t.Run(col, func(t *testing.T) {
			e := newEnv(t)
			alice := e.user(t, "alice")
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
			defer srv.Close()
			w := e.watch(t, alice)
			e.rule(t, alice, nil, "", e.webhook(t, alice, srv.URL))
			e.event(t, w, "release.published", "v1")
			routeAll(t, e.router())
			e.setInactive(t, alice, col)
			if n, err := e.deliverer(t).DeliverOnce(context.Background()); n != 1 || err != nil {
				t.Fatal(n, err)
			}
			var id int64
			e.scalar(t, &id, `SELECT id FROM deliveries`)
			got := e.delivery(t, id)
			if hits.Load() != 0 || got.status != "dead" || got.lastErr == nil || *got.lastErr != errOwnerDisabled.Error() {
				t.Fatalf("sent %d times, %+v", hits.Load(), got)
			}
		})
	}
}
