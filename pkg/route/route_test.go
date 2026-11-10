package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	script.MaybeSandbox()
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

const testKey = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="

type env struct {
	url  string
	pool *pgxpool.Pool
	box  *secret.Box
	m    *Metrics
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{url: testpg.NewDB(t)}
	e.pool = connect(t, e.url)
	if _, err := db.Migrate(context.Background(), e.pool); err != nil {
		t.Fatal(err)
	}
	e.box, _ = secret.New(testKey)
	e.m = NewMetrics(prometheus.NewRegistry())
	return e
}

func connect(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (e *env) scalar(t *testing.T, dst any, sql string, args ...any) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	e.scalar(t, &n, sql, args...)
	return n
}

func (e *env) user(t *testing.T, name string) string {
	t.Helper()
	var id string
	e.scalar(t, &id, `INSERT INTO users (oidc_sub, email) VALUES ($1, $1 || '@x.test') RETURNING id::text`, name)
	return id
}

func (e *env) watch(t *testing.T, owner string) string {
	t.Helper()
	var id string
	e.scalar(t, &id, `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'cli/cli', 'github_repo', '{}') RETURNING id::text`, owner)
	return id
}

func (e *env) event(t *testing.T, watch, typ, tag string) int64 {
	t.Helper()
	var id int64
	e.scalar(t, &id, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, $2, jsonb_build_object('tag', $3::text), $3 || random()::text) RETURNING id`,
		watch, typ, tag)
	return id
}

func (e *env) rule(t *testing.T, owner string, watch *string, filter string, channels ...string) string {
	t.Helper()
	r := testpg.Rule{Owner: owner, Filter: filter}
	if watch != nil {
		r.Watches = []string{*watch}
	}
	id := testpg.InsertRule(t, e.pool, r)
	for _, c := range channels {
		if _, err := e.pool.Exec(context.Background(), `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, id, c); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (e *env) webhook(t *testing.T, owner, url string) string {
	t.Helper()
	var id string
	cfg, _ := json.Marshal(notify.WebhookConfig{URL: url})
	e.scalar(t, &id, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, gen_random_uuid()::text, 'webhook', $2) RETURNING id::text`, owner, cfg)
	ct, nonce := e.box.Seal([]byte("signing-key-0123456789"), []byte(id))
	if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET secret_ciphertext = $2, secret_nonce = $3, key_version = 1 WHERE id = $1`, id, ct, nonce); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) router() *Router {
	return NewRouter(e.pool, zap.NewNop(), e.m, 10*time.Millisecond)
}

func routeAll(t *testing.T, r *Router) {
	t.Helper()
	for {
		n, err := r.RouteOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// 001-AC14: a matching event makes one inbox item per user, even with two matching rules, and one
// delivery per channel; a non-matching event makes neither.
func TestRouting(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	w := e.watch(t, alice)
	hook := e.webhook(t, alice, "https://hooks.example/a")
	hook2 := e.webhook(t, alice, "https://hooks.example/b")
	e.rule(t, alice, &w, `event.type == "release.published"`, hook)
	e.rule(t, alice, nil, `event.payload.tag startsWith "v2"`, hook, hook2)
	e.rule(t, bob, nil, "") // bob cannot read the watch: never routed to him
	var disabled string
	e.scalar(t, &disabled, `INSERT INTO rules (owner_id, name, filter, enabled, all_watches) VALUES ($1, 'off', '', false, true) RETURNING id::text`, alice)

	match := e.event(t, w, "release.published", "v2.1.0")
	miss := e.event(t, w, "tag.removed", "v1.0.0")
	routeAll(t, e.router())

	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE event_id = $1`, match); n != 1 {
		t.Fatalf("matching event: %d inbox items, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1`, match); n != 2 {
		t.Fatalf("matching event: %d deliveries, want one per channel (2)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE event_id = $1`, miss) + e.count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1`, miss); n != 0 {
		t.Fatalf("non-matching event created %d rows", n)
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1`, bob); n != 0 {
		t.Fatal("a user without access got an item")
	}
	if n := e.count(t, `SELECT count(*) FROM events WHERE routed_at IS NULL`); n != 0 {
		t.Fatalf("%d events left unrouted", n)
	}
	if got := testutil.ToFloat64(e.m.routed); got != 2 {
		t.Fatalf("gedour_events_routed_total = %v", got)
	}
	// Routed again (a crash replay): nothing doubles.
	if _, err := e.pool.Exec(context.Background(), `UPDATE events SET routed_at = NULL`); err != nil {
		t.Fatal(err)
	}
	routeAll(t, e.router())
	if n := e.count(t, `SELECT count(*) FROM inbox_items`) + e.count(t, `SELECT count(*) FROM deliveries`); n != 3 {
		t.Fatalf("replay doubled rows: %d", n)
	}
}

// 009-AC8: the router reaches the watch owner's rules only; another user's rules, pinned to the watch or on all
// watches, route nothing until a transfer makes the watch theirs, and the old owner's then stop at once.
func TestRoutingOwnedWatchesOnly(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	w := e.watch(t, alice)
	e.rule(t, alice, &w, "")
	e.rule(t, bob, nil, "")
	e.rule(t, bob, &w, "")
	owned := e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1`, bob); n != 0 {
		t.Fatal("another user's rules routed the watch's event")
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, alice, owned); n != 1 {
		t.Fatal("the owner's rule did not route")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET owner_id = $2 WHERE id = $1`, w, bob); err != nil {
		t.Fatal(err)
	}
	moved := e.event(t, w, "release.published", "v2")
	routeAll(t, e.router())
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE event_id = $1 AND user_id = $2`, moved, bob); n != 1 {
		t.Fatal("the new owner's rules did not route")
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE event_id = $1 AND user_id = $2`, moved, alice); n != 0 {
		t.Fatal("the old owner's rule still routes")
	}
}

// A filter that fails on an event is counted and treated as not matching; the other rules still run.
func TestRoutingEvalError(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	e.rule(t, alice, nil, `event.payload.tag > 3`)
	e.rule(t, alice, nil, `this is not expr`) // stored before a stricter release, say
	ev := e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	if got := testutil.ToFloat64(e.m.evalErrors); got != 2 {
		t.Fatalf("gedour_rule_eval_errors_total = %v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE event_id = $1`, ev); n != 0 {
		t.Fatal("an erroring rule matched")
	}
}

// 001-AC14: two routers on the same database route each of 300 events exactly once.
func TestRoutingExactlyOnce(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	hook := e.webhook(t, alice, "https://hooks.example/a")
	e.rule(t, alice, nil, "", hook)
	e.rule(t, alice, &w, "", hook)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO events (watch_id, type, payload, dedup_key)
		SELECT $1, 'tag.created', '{}', g::text FROM generate_series(1, 300) g`, w); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var routed atomic.Int64
	for range 2 {
		r := NewRouter(connect(t, e.url), zap.NewNop(), e.m, time.Millisecond)
		wg.Go(func() {
			for {
				n, err := r.RouteOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
				routed.Add(int64(n))
			}
		})
	}
	wg.Wait()
	if routed.Load() != 300 || e.count(t, `SELECT count(*) FROM inbox_items`) != 300 || e.count(t, `SELECT count(*) FROM deliveries`) != 300 {
		t.Fatalf("routed %d, items %d, deliveries %d", routed.Load(), e.count(t, `SELECT count(*) FROM inbox_items`), e.count(t, `SELECT count(*) FROM deliveries`))
	}
}

func (e *env) deliverer(t *testing.T) *Deliverer {
	t.Helper()
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := &notify.Sender{Client: g.Client(netguard.Options{Timeout: notify.SendTimeout, HTTPHosts: []string{"127.0.0.1"}, NoRedirects: true}), UserAgent: "gedour/test"}
	return NewDeliverer(e.pool, zap.NewNop(), e.box, s, testRunner(t, s.Client, g), e.m, 10*time.Millisecond, 4, time.Second)
}

// testRunner starts this test binary as the sandbox (TestMain hands it to script.MaybeSandbox).
func testRunner(t *testing.T, client *http.Client, g *netguard.Guard) *script.Runner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return script.NewRunner(script.Options{Exe: exe, Args: []string{script.Subcommand}, Client: client, Guard: g,
		HTTPHosts: []string{"127.0.0.1"}, UserAgent: "gedour/test"})
}

type delivery struct {
	status   string
	attempts int
	lastErr  *string
	wait     float64
	sent     bool
}

func (e *env) delivery(t *testing.T, id int64) delivery {
	t.Helper()
	var d delivery
	if err := e.pool.QueryRow(context.Background(), `SELECT status, attempts, last_error, EXTRACT(EPOCH FROM next_attempt_at - now()), sent_at IS NOT NULL
		FROM deliveries WHERE id = $1`, id).Scan(&d.status, &d.attempts, &d.lastErr, &d.wait, &d.sent); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) makeDue(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = now() WHERE status IN ('pending', 'failed')`); err != nil {
		t.Fatal(err)
	}
}

// 001-AC15, 001-AC31: a failing webhook is retried with growing backoff and dead after 5 attempts; a
// manual retry gives it one more; a recovered receiver marks it sent; the delivery metrics count each.
func TestDeliveryRetries(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	var hits atomic.Int64
	var ok atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if ok.Load() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	w := e.watch(t, alice)
	hook := e.webhook(t, alice, srv.URL)
	e.rule(t, alice, nil, "", hook)
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries`)
	d := e.deliverer(t)

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if n, err := d.DeliverOnce(context.Background()); err != nil || n != 1 {
			t.Fatalf("attempt %d: claimed %d, %v", attempt, n, err)
		}
		got := e.delivery(t, id)
		if got.attempts != attempt || got.lastErr == nil || *got.lastErr != "POST "+srv.Listener.Addr().String()+": HTTP 502" {
			t.Fatalf("attempt %d: %+v", attempt, got)
		}
		if attempt < MaxAttempts {
			want := retryDelay(attempt).Seconds()
			if got.status != "failed" || got.wait < want-5 || got.wait > want+1 {
				t.Fatalf("attempt %d: status %s, next in %.0f s, want ~%.0f", attempt, got.status, got.wait, want)
			}
			// Not due yet: nothing to claim until the backoff passes.
			if n, _ := d.DeliverOnce(context.Background()); n != 0 {
				t.Fatalf("attempt %d: claimed before its backoff", attempt)
			}
			e.makeDue(t)
		} else if got.status != "dead" {
			t.Fatalf("after %d attempts: %s, want dead", attempt, got.status)
		}
	}
	e.makeDue(t)
	if n, _ := d.DeliverOnce(context.Background()); n != 0 || hits.Load() != MaxAttempts {
		t.Fatalf("a dead delivery was sent again (%d hits)", hits.Load())
	}
	if got := testutil.ToFloat64(e.m.deliveries.WithLabelValues("webhook", "failed")); got != 4 {
		t.Fatalf("failed counter %v", got)
	}
	if got := testutil.ToFloat64(e.m.deliveries.WithLabelValues("webhook", "dead")); got != 1 {
		t.Fatalf("dead counter %v", got)
	}

	ok.Store(true)
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET status = 'pending', next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.DeliverOnce(context.Background()); n != 1 {
		t.Fatal("retried delivery not claimed")
	}
	if got := e.delivery(t, id); got.status != "sent" || got.attempts != MaxAttempts+1 || !got.sent {
		t.Fatalf("after recovery: %+v", got)
	}
	if testutil.ToFloat64(e.m.deliveries.WithLabelValues("webhook", "sent")) != 1 || testutil.CollectAndCount(e.m.duration) != 1 {
		t.Fatal("sent counter or duration histogram missing")
	}
}

// 001-AC15: an attempt whose claim was taken over (lease expired) is discarded, not recorded twice.
func TestDeliveryTokenMismatch(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	w := e.watch(t, alice)
	e.rule(t, alice, nil, "", e.webhook(t, alice, srv.URL))
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	d := e.deliverer(t)
	done := make(chan int)
	go func() {
		n, _ := d.DeliverOnce(context.Background())
		done <- n
	}()
	// While the send hangs, another worker re-claims after a (simulated) lease expiry.
	deadline := time.Now().Add(5 * time.Second)
	for e.count(t, `SELECT count(*) FROM deliveries WHERE claim_token IS NOT NULL`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never claimed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET claim_token = gen_random_uuid(), attempts = 3`); err != nil {
		t.Fatal(err)
	}
	close(release)
	if n := <-done; n != 1 {
		t.Fatal("not claimed")
	}
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries`)
	if got := e.delivery(t, id); got.status != "pending" || got.attempts != 3 {
		t.Fatalf("a stale claim's outcome was stored: %+v", got)
	}
}

// A delivery whose channel secret cannot be opened fails like a send, with a clear error.
func TestDeliveryBadSecret(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	hook := e.webhook(t, alice, "https://hooks.example/a")
	if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET secret_nonce = 'xx'`); err != nil {
		t.Fatal(err)
	}
	e.rule(t, alice, nil, "", hook)
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	if n, err := e.deliverer(t).DeliverOnce(context.Background()); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries`)
	if got := e.delivery(t, id); got.status != "failed" || got.lastErr == nil || *got.lastErr != "channel secret: open secret: bad nonce size" {
		t.Fatalf("%+v", got)
	}
}

// 003-AC3: Talk's permanent answers end the delivery with the reason; any other answer but 201 follows
// the retry policy.
func TestTalkDeliveryOutcome(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	for _, tt := range []struct {
		code   int
		status string
	}{{201, "sent"}, {200, "failed"}, {204, "failed"}, {400, "failed"}, {401, "dead"}, {404, "dead"}, {413, "dead"}, {429, "failed"}, {500, "failed"}, {503, "failed"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.code) }))
		defer srv.Close()
		cfg, _ := json.Marshal(notify.NextcloudTalkConfig{URL: srv.URL, Conversation: "abcd"})
		var ch string
		e.scalar(t, &ch, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, gen_random_uuid()::text, 'nextcloud_talk', $2) RETURNING id::text`, alice, cfg)
		rule := e.rule(t, alice, nil, "", ch)
		event := e.event(t, w, "release.published", fmt.Sprintf("v%d", tt.code))
		routeAll(t, e.router())
		if _, err := e.pool.Exec(context.Background(), `DELETE FROM rule_channels WHERE rule_id = $1`, rule); err != nil {
			t.Fatal(err)
		}
		if _, err := e.deliverer(t).DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		var id int64
		e.scalar(t, &id, `SELECT id FROM deliveries WHERE channel_id = $1 AND event_id = $2`, ch, event)
		got := e.delivery(t, id)
		if got.status != tt.status || got.attempts != 1 {
			t.Errorf("%d: %+v", tt.code, got)
		}
		if tt.status == "sent" {
			continue
		}
		if want := fmt.Sprintf("POST %s: HTTP %d", srv.Listener.Addr(), tt.code); got.lastErr == nil || *got.lastErr != want {
			t.Errorf("%d: reason %v", tt.code, got.lastErr)
		}
		if tt.status == "failed" && got.wait < retryDelay(1).Seconds()-5 {
			t.Errorf("%d: retried in %.0f s", tt.code, got.wait)
		}
	}
	if n, err := e.deliverer(t).DeliverOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("a dead or backing-off delivery was claimed: %d, %v", n, err)
	}
}

func TestRetryDelay(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 30 * time.Second, 1: 30 * time.Second, 2: 2 * time.Minute, 4: 30 * time.Minute, 9: 30 * time.Minute} {
		if got := retryDelay(n); got != want {
			t.Errorf("retryDelay(%d) = %s", n, got)
		}
	}
	if got := truncate(string(make([]byte, 3000)) + "x"); got != "x" {
		t.Fatalf("NUL not stripped: %q", got)
	}
	if got := truncate(strings.Repeat("é", 2000)); len(got) > maxErrorLen || !utf8.ValidString(got) {
		t.Fatal("not truncated")
	}
}

// A delivery claim that fails, before the query runs or on a row it updates, is an error, not an empty poll,
// and leaves the delivery unclaimed.
func TestDeliveryClaimFails(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	e.rule(t, alice, nil, "", e.webhook(t, alice, "http://127.0.0.1:1/hook"))
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := e.deliverer(t).DeliverOnce(cancelled); n != 0 || err == nil {
		t.Fatalf("cancelled claim: %d %v", n, err)
	}
	if _, err := e.pool.Exec(context.Background(), `CREATE FUNCTION refuse_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$;
		CREATE TRIGGER refuse_claim BEFORE UPDATE ON deliveries FOR EACH ROW EXECUTE FUNCTION refuse_claim()`); err != nil {
		t.Fatal(err)
	}
	if n, err := e.deliverer(t).DeliverOnce(context.Background()); n != 0 || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refused claim: %d %v", n, err)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE claim_token IS NULL AND attempts = 0`); n != 1 {
		t.Fatalf("unclaimed deliveries %d, want 1", n)
	}
}

// The loops run until cancelled.
func TestLoopsStop(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { e.router().Run(ctx) })
	wg.Go(func() { e.deliverer(t).Run(ctx) })
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()
}

// 001-AC19/001-AC31: the hub bounds streams, tracks them in gedour_sse_subscribers and wakes a user's
// streams on NOTIFY, after a lost connection too.
func TestHub(t *testing.T) {
	e := newEnv(t)
	h := newHub(e.pool, zap.NewNop(), e.m)
	var subs []*subscriber
	for range MaxStreamsPerUser {
		s, err := h.subscribe("u1")
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	if _, err := h.subscribe("u1"); err == nil {
		t.Fatal("per-user limit not enforced")
	}
	other, err := h.subscribe("u2")
	if err != nil || testutil.ToFloat64(e.m.subscribers) != MaxStreamsPerUser+1 {
		t.Fatalf("gauge %v, %v", testutil.ToFloat64(e.m.subscribers), err)
	}
	h.unsubscribe(subs[0])
	h.unsubscribe(subs[0])
	if testutil.ToFloat64(e.m.subscribers) != MaxStreamsPerUser {
		t.Fatalf("gauge after unsubscribe: %v", testutil.ToFloat64(e.m.subscribers))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	// The first LISTEN wakes everyone, then only the notified user.
	<-other.ch
	<-subs[1].ch
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := e.pool.Exec(context.Background(), `SELECT pg_notify($1, 'u2')`, InboxChannel); err != nil {
			t.Fatal(err)
		}
		select {
		case <-other.ch:
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no wake-up on NOTIFY")
			}
			continue
		}
		break
	}
	select {
	case <-subs[1].ch:
		t.Fatal("another user's stream woke up")
	default:
	}
	// Kill the LISTEN backend: the hub reconnects and wakes every stream.
	if _, err := e.pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE query = 'LISTEN gedour_inbox' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-subs[1].ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no wake-up after reconnect")
	}
	cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not stop")
	}
}

// A batch stops taking events once its time budget is spent; the rest go to the next batch.
func TestRoutingBudget(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	e.rule(t, alice, nil, "")
	for _, tag := range []string{"v1", "v2", "v3"} {
		e.event(t, w, "release.published", tag)
	}
	r := e.router()
	r.budget = 0
	if n, err := r.RouteOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("first batch routed %d, %v; want 1 within a spent budget", n, err)
	}
	if left := e.count(t, `SELECT count(*) FROM events WHERE routed_at IS NULL`); left != 2 {
		t.Fatalf("%d events left, want 2", left)
	}
	routeAll(t, r)
	if n := e.count(t, `SELECT count(*) FROM inbox_items`); n != 3 {
		t.Fatalf("%d items", n)
	}
}

// A delivery whose channel owner no longer owns the watch (transferred after routing) is not sent, and ends dead with why.
func TestDeliveryUnreadableWatch(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	w := e.watch(t, alice)
	e.rule(t, alice, nil, "", e.webhook(t, alice, srv.URL))
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET owner_id = $2 WHERE id = $1`, w, bob); err != nil {
		t.Fatal(err)
	}
	if n, err := e.deliverer(t).DeliverOnce(context.Background()); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries`)
	got := e.delivery(t, id)
	if hits.Load() != 0 || got.status != "dead" || got.lastErr == nil || *got.lastErr != errUnreadable.Error() {
		t.Fatalf("sent %d times, %+v", hits.Load(), got)
	}
}

// A slow receiver holds one slot: the other deliveries go out meanwhile.
func TestDeliverySlowDoesNotBlock(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fast.Close()
	w := e.watch(t, alice)
	slowHook := e.webhook(t, alice, slow.URL)
	e.rule(t, alice, nil, "", slowHook)
	e.event(t, w, "release.published", "v0")
	routeAll(t, e.router())
	e.rule(t, alice, nil, "", e.webhook(t, alice, fast.URL))
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM rule_channels WHERE channel_id = $1`, slowHook); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v1", "v2", "v3", "v4", "v5"} {
		e.event(t, w, "release.published", tag)
	}
	routeAll(t, e.router())
	d := e.deliverer(t)
	d.concurrency = 2
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`) < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d fast deliveries sent while the slow one hangs", e.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// On stop, a hanging send gets the drain time, then is cancelled; the claim goes back, not an attempt.
func TestDeliveryDrain(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer hang.Close()
	defer close(stop)
	w := e.watch(t, alice)
	e.rule(t, alice, nil, "", e.webhook(t, alice, hang.URL))
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	d := e.deliverer(t)
	d.drain = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	for e.count(t, `SELECT count(*) FROM deliveries WHERE claim_token IS NOT NULL`) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run outlived its drain time")
	}
	if took := time.Since(start); took < 150*time.Millisecond || took > 2*time.Second {
		t.Fatalf("stopped after %s, want about the 200 ms drain", took)
	}
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries`)
	if got := e.delivery(t, id); got.status != "pending" || got.attempts != 0 || got.lastErr != nil {
		t.Fatalf("a drain-cancelled send counted as an attempt: %+v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE claim_token IS NULL AND leased_until IS NULL`); n != 1 {
		t.Fatal("the claim was not released")
	}
}

// Only a failed send cut by the drain is released; a send that succeeded is always recorded.
func TestDrained(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	boom := errors.New("context canceled")
	for _, tt := range []struct {
		err  error
		stop context.Context
		want bool
	}{{nil, live, false}, {nil, stopped, false}, {boom, live, false}, {boom, stopped, true}} {
		if got := drained(tt.err, tt.stop); got != tt.want {
			t.Errorf("drained(%v, stopped=%v) = %v", tt.err, tt.stop.Err() != nil, got)
		}
	}
}

// A retry that finds the batch gone reports nothing routed, not the failed attempt's count.
func TestRouteOnceRetryDropsStaleCount(t *testing.T) {
	e := newEnv(t)
	u := e.user(t, "u")
	w := e.watch(t, u)
	var ev int64
	e.scalar(t, &ev, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id`, w)
	retries := testpg.AllowDeadlockRetries(t)
	release := testpg.ForceDeadlockOnce(t, e.pool, "events")
	r := NewRouter(e.pool, zap.NewNop(), e.m, time.Hour)
	type result struct {
		n   int
		err error
	}
	routed := make(chan result, 1)
	go func() {
		n, err := r.RouteOnce(context.Background())
		routed <- result{n, err}
	}()
	testpg.WaitLockWaiters(t, e.pool, 1)
	gone := make(chan error, 1)
	go func() {
		_, err := e.pool.Exec(context.Background(), `DELETE FROM events WHERE id = $1`, ev)
		gone <- err
	}()
	testpg.WaitLockWaiters(t, e.pool, 2)
	release()
	if err := <-gone; err != nil {
		t.Fatal(err)
	}
	if got := <-routed; got.err != nil || got.n != 0 || retries() != 1 {
		t.Fatalf("routed %d, err %v, retries %d: want 0, nil, 1", got.n, got.err, retries())
	}
}
