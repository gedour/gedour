package route

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/notify/webpush"
)

// pushEnv is a fake push service whose /push/flaky answers 500 while failing is set.
type pushEnv struct {
	*env
	up      *testupstream.Server
	url     string
	failing atomic.Bool
}

func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	p := &pushEnv{env: newEnv(t), up: testupstream.New()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/push/flaky" && p.failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		p.up.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func pushSender(t *testing.T) *notify.Sender {
	t.Helper()
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	k, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return &notify.Sender{Client: g.Client(netguard.Options{Timeout: notify.SendTimeout, HTTPHosts: []string{"127.0.0.1"}, NoRedirects: true}),
		UserAgent: "gedour/test", VAPID: k, HTTPHosts: []string{"127.0.0.1"}}
}

func (p *pushEnv) deliverer(t *testing.T) *Deliverer {
	t.Helper()
	return NewDeliverer(p.pool, zap.NewNop(), p.box, pushSender(t), nil, p.m, 10*time.Millisecond, 4, time.Second)
}

// device registers a subscription to the fake service at /push/<id>; auth zeros make it undecryptable (400).
func (p *pushEnv) device(t *testing.T, owner, id string, badKeys bool) string {
	t.Helper()
	sub := testupstream.PushSubscription(p.url + "/push/" + id)
	if badKeys {
		sub.Auth = make([]byte, 16)
	}
	plain, _ := json.Marshal(sealedSub(sub))
	hash := sha256.Sum256([]byte(sub.Endpoint))
	var dev string
	p.scalar(t, &dev, `INSERT INTO push_devices (owner_id, name, endpoint_hash, sealed, nonce, key_version) VALUES ($1, $2, $3, '', '', 1) RETURNING id::text`, owner, id, hash[:])
	ct, nonce := p.box.Seal(plain, []byte(dev))
	if _, err := p.pool.Exec(context.Background(), `UPDATE push_devices SET sealed = $2, nonce = $3 WHERE id = $1`, dev, ct, nonce); err != nil {
		t.Fatal(err)
	}
	return dev
}

// channel makes a web_push channel for all of owner's devices (none given) or the listed ones.
func (p *pushEnv) channel(t *testing.T, owner string, silent bool, devices ...string) string {
	t.Helper()
	cfg := map[string]any{"silent": silent}
	if len(devices) == 0 {
		cfg["devices"] = notify.AllDevices
	}
	var id string
	p.scalar(t, &id, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, gen_random_uuid()::text, 'web_push', $2) RETURNING id::text`, owner, cfg)
	for _, d := range devices {
		if _, err := p.pool.Exec(context.Background(), `INSERT INTO channel_push_devices (channel_id, device_id) VALUES ($1, $2)`, id, d); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// deliver routes one new event of w through rule and delivers it once; it returns the delivery.
func (p *pushEnv) deliver(t *testing.T, d *Deliverer, w, tag string) int64 {
	t.Helper()
	ev := p.event(t, w, "release.published", tag)
	routeAll(t, p.router())
	var id int64
	p.scalar(t, &id, `SELECT id FROM deliveries WHERE event_id = $1`, ev)
	p.once(t, d)
	return id
}

func (p *pushEnv) once(t *testing.T, d *Deliverer) {
	t.Helper()
	if _, err := d.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// pushes counts what the fake accepted per endpoint name ("push-<id>").
func (p *pushEnv) pushes() map[string]int {
	n := map[string]int{}
	for _, r := range p.up.Received() {
		n[r.Endpoint]++
	}
	return n
}

// 010-AC7: one device accepts, one fails: the retry sends to the failed one only, and the delivery is sent
// once both accepted; each accepted device gets its last push time.
func TestPushPartialFailure(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	w := p.watch(t, alice)
	ok, flaky := p.device(t, alice, "ok", false), p.device(t, alice, "flaky", false)
	p.device(t, alice, "bad", true)
	p.rule(t, alice, nil, "", p.channel(t, alice, false))
	d := p.deliverer(t)
	p.failing.Store(true)
	id := p.deliver(t, d, w, "v1")
	got := p.delivery(t, id)
	if got.status != "failed" || got.lastErr == nil || !strings.Contains(*got.lastErr, "2 of 3 devices failed") || !strings.Contains(*got.lastErr, "HTTP 500") {
		t.Fatalf("first attempt: %+v %v", got, deref(got.lastErr))
	}
	if n := p.count(t, `SELECT count(*) FROM delivery_push_sent WHERE delivery_id = $1 AND device_id = $2`, id, ok); n != 1 {
		t.Fatal("accepted device not in the ledger")
	}
	p.failing.Store(false)
	p.makeDue(t)
	p.once(t, d)
	if got := p.delivery(t, id); got.status != "sent" || got.attempts != 2 || !strings.HasPrefix(deref(got.lastErr), "1 of 3 devices refused") {
		t.Fatalf("retry: %+v %v", got, deref(got.lastErr))
	}
	// A refusal skips a device for one attempt only: the retry flaky caused tries it again.
	if n := p.pushes(); n["push-ok"] != 1 || n["push-flaky"] != 1 || n["push-bad-rejected"] != 2 {
		t.Fatalf("pushes %v: the accepted device must not get it twice, the refused one is tried each attempt", n)
	}
	if n := p.count(t, `SELECT count(*) FROM push_devices WHERE id IN ($1, $2) AND last_push_at IS NOT NULL`, ok, flaky); n != 2 {
		t.Fatalf("%d devices with a last push", n)
	}
}

// 010-AC8: a gone device is removed and audited by the system; the delivery is sent to the rest, not retried.
func TestPushGoneDevice(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	w := p.watch(t, alice)
	p.device(t, alice, "ok", false)
	gone := p.device(t, alice, "gone1", false)
	p.rule(t, alice, nil, "", p.channel(t, alice, false))
	id := p.deliver(t, p.deliverer(t), w, "v1")
	if got := p.delivery(t, id); got.status != "sent" || got.lastErr != nil {
		t.Fatalf("%+v %v", got, deref(got.lastErr))
	}
	if n := p.count(t, `SELECT count(*) FROM push_devices WHERE id = $1`, gone); n != 0 {
		t.Fatal("gone device kept")
	}
	if n := p.count(t, `SELECT count(*) FROM audit_log WHERE action = 'push_device.gone' AND target_id = $1 AND actor_user_id IS NULL
		AND before->>'owner_id' = $2 AND before::text NOT LIKE '%push/%'`, gone, alice); n != 1 {
		t.Fatalf("%d gone audit entries", n)
	}
	// Every device gone: nothing left to send to.
	only := p.user(t, "bob")
	bw := p.watch(t, only)
	p.device(t, only, "gone2", false)
	p.rule(t, only, nil, "", p.channel(t, only, false))
	id = p.deliver(t, p.deliverer(t), bw, "v2")
	if got := p.delivery(t, id); got.status != "dead" || deref(got.lastErr) != "no device" {
		t.Fatalf("all gone: %+v %v", got, deref(got.lastErr))
	}
}

// 010-AC9: no device registered, or every listed device removed: dead with "no device", the channel's last status.
func TestPushNoDevice(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	w := p.watch(t, alice)
	all := p.channel(t, alice, false)
	rule := p.rule(t, alice, nil, "", all)
	id := p.deliver(t, p.deliverer(t), w, "v1")
	if got := p.delivery(t, id); got.status != "dead" || got.attempts != 1 || deref(got.lastErr) != "no device" {
		t.Fatalf("none registered: %+v %v", got, deref(got.lastErr))
	}
	if _, err := p.pool.Exec(context.Background(), `DELETE FROM rule_channels WHERE rule_id = $1`, rule); err != nil {
		t.Fatal(err)
	}
	dev := p.device(t, alice, "ok", false)
	listed := p.channel(t, alice, false, dev)
	p.rule(t, alice, nil, "", listed)
	if _, err := p.pool.Exec(context.Background(), `DELETE FROM push_devices WHERE id = $1`, dev); err != nil {
		t.Fatal(err)
	}
	id = p.deliver(t, p.deliverer(t), w, "v2")
	if got := p.delivery(t, id); got.status != "dead" || deref(got.lastErr) != "no device" {
		t.Fatalf("listed device removed: %+v %v", got, deref(got.lastErr))
	}
	var last string
	p.scalar(t, &last, `SELECT status FROM deliveries WHERE channel_id = $1 ORDER BY id DESC LIMIT 1`, listed)
	if last != "dead" {
		t.Fatal(last)
	}
}

// 010-AC4, 010-AC5: "all" reaches a device registered after the channel; a list reaches only its devices,
// never another user's.
func TestPushTargets(t *testing.T) {
	p := newPushEnv(t)
	alice, bob := p.user(t, "alice"), p.user(t, "bob")
	w := p.watch(t, alice)
	p.device(t, bob, "bob", false)
	all := p.channel(t, alice, false)
	p.device(t, alice, "later", false)
	picked := p.device(t, alice, "picked", false)
	p.rule(t, alice, nil, "", all, p.channel(t, alice, false, picked))
	p.deliver(t, p.deliverer(t), w, "v1")
	if n := p.pushes(); n["push-later"] != 1 || n["push-picked"] != 2 || n["push-bob"] != 0 || len(n) != 2 {
		t.Fatalf("pushes %v", n)
	}
	if n := p.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`); n != 2 {
		t.Fatalf("%d sent", n)
	}
}

// 010-AC10: Urgency follows the rule priority, silent wins; silent is in the payload.
func TestPushUrgencyHeader(t *testing.T) {
	p := newPushEnv(t)
	for _, tt := range []struct {
		priority string
		silent   bool
		want     string
	}{
		{priorityNormal, false, notify.UrgencyNormal},
		{priorityHigh, false, notify.UrgencyHigh},
		{priorityHigh, true, notify.UrgencyLow},
		{priorityNormal, true, notify.UrgencyLow},
	} {
		u := p.user(t, tt.priority+tt.want)
		w := p.watch(t, u)
		p.device(t, u, u, false)
		p.setPriority(t, p.rule(t, u, nil, "", p.channel(t, u, tt.silent)), tt.priority)
		ev := p.event(t, w, "release.published", "v1")
		routeAll(t, p.router())
		p.once(t, p.deliverer(t))
		var got *testupstream.Received
		for _, r := range p.up.Received() {
			if r.Endpoint == "push-"+u {
				got = &r
			}
		}
		if got == nil {
			t.Fatalf("%v: no push", tt)
		}
		var m notify.PushMessage
		_ = json.Unmarshal([]byte(got.Body), &m)
		if got.Header["Urgency"] != tt.want || m.Silent != tt.silent || m.URL != "/inbox?event="+strconv.FormatInt(ev, 10) {
			t.Errorf("%v: urgency %s, payload %+v", tt, got.Header["Urgency"], m)
		}
	}
}

// A permanent refusal from one device is skipped for this delivery and named; with no device accepting, dead.
func TestPushRefused(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	w := p.watch(t, alice)
	p.device(t, alice, "ok", false)
	p.device(t, alice, "bad", true)
	p.rule(t, alice, nil, "", p.channel(t, alice, false))
	core, logs := observer.New(zap.DebugLevel)
	d := NewDeliverer(p.pool, zap.New(core), p.box, pushSender(t), nil, p.m, 10*time.Millisecond, 4, time.Second)
	id := p.deliver(t, d, w, "v1")
	if got := p.delivery(t, id); got.status != "sent" || !strings.HasPrefix(deref(got.lastErr), "1 of 2 devices refused") || !strings.Contains(deref(got.lastErr), "HTTP 400") {
		t.Fatalf("partly refused: %+v %v", got, deref(got.lastErr))
	}
	// Logged as sent, with the refusals; not as a failure.
	sent := logs.FilterMessage("delivery_sent").All()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].ContextMap()["refused"].(string), "1 of 2 devices refused") || logs.FilterMessage("delivery_failed").Len() != 0 {
		t.Fatalf("logs: %v", logs.All())
	}
	bob := p.user(t, "bob")
	bw := p.watch(t, bob)
	p.device(t, bob, "bad2", true)
	dev := p.device(t, bob, "unreadable", false)
	if _, err := p.pool.Exec(context.Background(), `UPDATE push_devices SET nonce = 'xx' WHERE id = $1`, dev); err != nil {
		t.Fatal(err)
	}
	p.rule(t, bob, nil, "", p.channel(t, bob, false))
	id = p.deliver(t, d, bw, "v2")
	if got := p.delivery(t, id); got.status != "dead" || !strings.Contains(deref(got.lastErr), "HTTP 400") || !strings.Contains(deref(got.lastErr), "device keys") {
		t.Fatalf("all refused: %+v %v", got, deref(got.lastErr))
	}
}

// A test push reaches the devices without a ledger, removes the gone ones and counts the accepted.
func TestPushTestSend(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	p.device(t, alice, "ok", false)
	p.device(t, alice, "gone1", false)
	ch, err := loadChannel(context.Background(), p.pool, p.box, p.channel(t, alice, true))
	if err != nil {
		t.Fatal(err)
	}
	n, err := pushSend(context.Background(), p.pool, p.box, pushSender(t), ch, notify.TestDelivery(time.Now()))
	if err != nil || n != 1 || p.count(t, `SELECT count(*) FROM push_devices`) != 1 || p.count(t, `SELECT count(*) FROM delivery_push_sent`) != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if _, err := pushSend(context.Background(), p.pool, p.box, pushSender(t), notify.Channel{ID: ch.ID, Config: []byte("[")}, notify.TestDelivery(time.Now())); err == nil {
		t.Fatal("bad config sent")
	}
}

// One delivery's fan-out of MaxDevices hung pushes still ends inside the send timeout and the lease.
func TestPushFanOutFitsTimeout(t *testing.T) {
	rounds := time.Duration((MaxDevices + pushParallel - 1) / pushParallel)
	if rounds*notify.SendTimeout >= sendTimeout || sendTimeout >= DeliveryLease {
		t.Fatalf("%d rounds of %s do not fit %s", rounds, notify.SendTimeout, sendTimeout)
	}
	// "Send test" answers on the request: its fan-out stops before the write timeout.
	if pushTestTimeout <= 0 || pushTestTimeout >= config.HTTPWriteTimeout {
		t.Fatalf("test fan-out bound %s, write timeout %s", pushTestTimeout, config.HTTPWriteTimeout)
	}
}

func TestStoredConfig(t *testing.T) {
	list := map[string]any{"devices": []any{"x"}, "silent": true}
	if got := storedConfig(notify.TypeWebPush, list); len(got) != 1 || got["silent"] != true || len(list) != 2 {
		t.Fatalf("%v %v", got, list)
	}
	all := map[string]any{"devices": "all"}
	if got := storedConfig(notify.TypeWebPush, all); got["devices"] != "all" {
		t.Fatal(got)
	}
	if got := storedConfig(notify.TypeWebhook, list); len(got) != 2 {
		t.Fatal(got)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A database failure is an opaque 500 on every device operation, and a recording failure is a send error.
func TestPushDatabaseDown(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	dev := p.device(t, alice, "ok", false)
	ch, err := loadChannel(context.Background(), p.pool, p.box, p.channel(t, alice, false))
	if err != nil {
		t.Fatal(err)
	}
	// A device id that is gone already is skipped, not an error.
	if err := recordPushes(context.Background(), p.pool, 0, nil, []pushTarget{{id: "6f1c1a43-6d5b-4c3a-9b8f-0e1f2a3b4c5d"}}); err != nil {
		t.Fatal(err)
	}
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := New(p.pool, zap.NewNop(), p.box, pushSender(t), nil, g, []string{"127.0.0.1"}, p.m)
	s.viewer = func(context.Context) (string, bool) { return alice, true }
	p.pool.Close()
	ctx := context.Background()
	reg := &registerPushDeviceInput{}
	reg.Body.Endpoint, reg.Body.Name = p.url+"/push/x", "x"
	reg.Body.Keys.P256DH, reg.Body.Keys.Auth = testupstream.PushP256DH, testupstream.PushAuth
	rename := &renamePushDeviceInput{ID: dev}
	rename.Body.Name = "y"
	for name, call := range map[string]func() error{
		"list":     func() error { _, err := s.listDevices(ctx, nil); return err },
		"register": func() error { _, err := s.registerDevice(ctx, reg); return err },
		"rename":   func() error { _, err := s.renameDevice(ctx, rename); return err },
		"delete":   func() error { _, err := s.deleteDevice(ctx, &idPath{ID: dev}); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "internal error") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := pushSend(ctx, p.pool, p.box, pushSender(t), ch, notify.TestDelivery(time.Now())); err == nil || !strings.Contains(err.Error(), "load devices") {
		t.Errorf("send: %v", err)
	}
	if err := recordPushes(ctx, p.pool, 1, []string{dev}, nil); err == nil || !strings.Contains(err.Error(), "record pushes") {
		t.Errorf("record: %v", err)
	}
}

// Signed out, every push operation is a 401; a malformed device id is a 404 like an unknown one.
func TestPushSignedOut(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := New(p.pool, zap.NewNop(), p.box, pushSender(t), nil, g, []string{"127.0.0.1"}, p.m)
	signedIn := true
	s.viewer = func(context.Context) (string, bool) { return alice, signedIn }
	ctx := context.Background()
	rename := &renamePushDeviceInput{ID: "not-a-uuid"}
	rename.Body.Name = "y"
	if _, err := s.renameDevice(ctx, rename); err == nil || !strings.Contains(err.Error(), "device not found") {
		t.Fatalf("malformed id: %v", err)
	}
	signedIn = false
	for name, call := range map[string]func() error{
		"key":      func() error { _, err := s.pushKey(ctx, nil); return err },
		"list":     func() error { _, err := s.listDevices(ctx, nil); return err },
		"register": func() error { _, err := s.registerDevice(ctx, &registerPushDeviceInput{}); return err },
		"rename":   func() error { _, err := s.renameDevice(ctx, rename); return err },
		"delete":   func() error { _, err := s.deleteDevice(ctx, &idPath{ID: "x"}); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not signed in") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A gone answer removes the device that was pushed to, not one registered again since with new keys.
func TestPushGoneReRegistered(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	dev := p.device(t, alice, "gone1", false)
	var nonce []byte
	p.scalar(t, &nonce, `SELECT nonce FROM push_devices WHERE id = $1`, dev)
	if err := recordPushes(context.Background(), p.pool, 0, nil, []pushTarget{{id: dev, nonce: []byte("stale-nonce")}}); err != nil {
		t.Fatal(err)
	}
	if p.count(t, `SELECT count(*) FROM push_devices WHERE id = $1`, dev) != 1 || p.count(t, `SELECT count(*) FROM audit_log WHERE action = 'push_device.gone'`) != 0 {
		t.Fatal("a device registered again was removed")
	}
	if err := recordPushes(context.Background(), p.pool, 0, nil, []pushTarget{{id: dev, nonce: nonce}}); err != nil {
		t.Fatal(err)
	}
	if p.count(t, `SELECT count(*) FROM push_devices WHERE id = $1`, dev) != 0 {
		t.Fatal("gone device kept")
	}
}

// cancelAfter cancels the send context once a push service answered, as the shutdown drain may.
type cancelAfter struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	c.cancel()
	return resp, err
}

// 010-AC7: a push accepted before the context ends is still in the ledger: a retry never repeats it.
func TestPushLedgerSurvivesCancel(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	w := p.watch(t, alice)
	dev := p.device(t, alice, "ok", false)
	chID := p.channel(t, alice, false)
	p.rule(t, alice, nil, "", chID)
	ev := p.event(t, w, "release.published", "v1")
	routeAll(t, p.router())
	var id int64
	p.scalar(t, &id, `SELECT id FROM deliveries WHERE event_id = $1`, ev)
	ch, err := loadChannel(context.Background(), p.pool, p.box, chID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := pushSender(t)
	sender.Client = &http.Client{Transport: cancelAfter{sender.Client.Transport, cancel}, CheckRedirect: sender.Client.CheckRedirect}
	if n, err := pushSend(ctx, p.pool, p.box, sender, ch, notify.Delivery{ID: id, Event: notify.Event{ID: ev}}); err != nil || n != 1 || ctx.Err() == nil {
		t.Fatalf("%d, %v, ctx %v", n, err, ctx.Err())
	}
	if p.count(t, `SELECT count(*) FROM delivery_push_sent WHERE delivery_id = $1 AND device_id = $2`, id, dev) != 1 {
		t.Fatal("accepted push not in the ledger after the context ended")
	}
}

// "Send test" answers within its bound even when a push service never does: the fast devices count, ok is false.
func TestPushTestTimeout(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	p.device(t, alice, "ok", false)
	p.device(t, alice, "hang1", false)
	ch := p.channel(t, alice, false)
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := New(p.pool, zap.NewNop(), p.box, pushSender(t), nil, g, []string{"127.0.0.1"}, p.m)
	s.viewer = func(context.Context) (string, bool) { return alice, true }
	saved := pushTestTimeout
	pushTestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { pushTestTimeout = saved })
	start := time.Now()
	out, err := s.testChannel(context.Background(), &idPath{ID: ch})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second || out.Body.OK || out.Body.Accepted == nil || *out.Body.Accepted != 1 {
		t.Fatalf("took %s: %+v accepted %v", took, out.Body, deref2(out.Body.Accepted))
	}
}

func deref2(n *int) int {
	if n == nil {
		return -1
	}
	return *n
}

// cancelAt cancels the stop context once n push services answered: the shutdown drain beginning just then.
type cancelAt struct {
	base   http.RoundTripper
	n      *atomic.Int32
	at     int32
	cancel context.CancelFunc
}

func (c cancelAt) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if c.n.Add(1) == c.at {
		c.cancel()
	}
	return resp, err
}

// A partly sent attempt that ends as the drain starts is recorded as sent, not released for a resend.
func TestPushDrainKeepsPartlySent(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	w := p.watch(t, alice)
	p.device(t, alice, "ok", false)
	p.device(t, alice, "bad", true)
	p.rule(t, alice, nil, "", p.channel(t, alice, false))
	ev := p.event(t, w, "release.published", "v1")
	routeAll(t, p.router())
	var id int64
	p.scalar(t, &id, `SELECT id FROM deliveries WHERE event_id = $1`, ev)
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := pushSender(t)
	sender.Client = &http.Client{Transport: cancelAt{sender.Client.Transport, new(atomic.Int32), 2, cancel}, CheckRedirect: sender.Client.CheckRedirect}
	d := NewDeliverer(p.pool, zap.NewNop(), p.box, sender, nil, p.m, 10*time.Millisecond, 4, time.Second)
	if _, err := d.DeliverOnce(stop); err != nil {
		t.Fatal(err)
	}
	if got := p.delivery(t, id); stop.Err() == nil || got.status != "sent" || got.attempts != 1 {
		t.Fatalf("%+v %v (stopped: %v)", got, deref(got.lastErr), stop.Err())
	}
}

// 010-AC16: the old owner's move entry is written on a fresh context: no admin acting as the new owner, no
// API key, only the request id they share with the new owner's entry.
func TestPushMoveEntryOwnContext(t *testing.T) {
	p := newPushEnv(t)
	alice, bob, admin := p.user(t, "alice"), p.user(t, "bob"), p.user(t, "admin")
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := New(p.pool, zap.NewNop(), p.box, pushSender(t), nil, g, []string{"127.0.0.1"}, p.m)
	s.viewer = func(context.Context) (string, bool) { return bob, true }
	for _, tt := range []struct {
		name string
		ctx  context.Context
	}{
		{"acting", audit.WithActing(context.Background(), admin, bob)},
		{"api key", audit.WithAPIKey(context.Background(), "6f1c1a43-6d5b-4c3a-9b8f-0e1f2a3b4c5d")},
	} {
		dev := p.device(t, alice, "m-"+strings.ReplaceAll(tt.name, " ", "-"), false)
		in := &registerPushDeviceInput{}
		in.Body.Endpoint, in.Body.Name = p.url+"/push/m-"+strings.ReplaceAll(tt.name, " ", "-"), "Bob's"
		in.Body.Keys.P256DH, in.Body.Keys.Auth = testupstream.PushP256DH, testupstream.PushAuth
		if _, err := s.registerDevice(audit.WithRequestID(tt.ctx, "req-"+tt.name), in); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if n := p.count(t, `SELECT count(*) FROM audit_log WHERE action = 'push_device.moved' AND target_id = $1 AND before->>'owner_id' = $2::text
			AND actor_user_id IS NULL AND on_behalf_of IS NULL AND api_key_id IS NULL AND request_id = $3`, dev, alice, "req-"+tt.name); n != 1 {
			t.Errorf("%s: %d clean old-owner entries", tt.name, n)
		}
		if n := p.count(t, `SELECT count(*) FROM audit_log WHERE action = 'push_device.moved' AND target_id = $1 AND after->>'owner_id' = $2::text
			AND request_id = $3 AND (on_behalf_of IS NOT NULL OR api_key_id IS NOT NULL)`, dev, bob, "req-"+tt.name); n != 1 {
			t.Errorf("%s: %d new-owner entries carrying the context", tt.name, n)
		}
	}
}
