package route

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/wake"
)

// tgMessage is one sendMessage call the fake Telegram got.
type tgMessage struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
	Silent bool   `json:"disable_notification"`
}

// digestEnv is a fake Telegram counting every message, answering 500 while failing is set.
type digestEnv struct {
	*env
	tg      string
	alice   string
	failing atomic.Bool
	mu      sync.Mutex
	got     []tgMessage
}

func newDigestEnv(t *testing.T) *digestEnv {
	t.Helper()
	e := &digestEnv{env: newEnv(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m tgMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		e.mu.Lock()
		e.got = append(e.got, m)
		e.mu.Unlock()
		if e.failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	e.tg, e.alice = srv.URL, e.user(t, "alice")
	return e
}

func (e *digestEnv) messages() []tgMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tgMessage(nil), e.got...)
}

func (e *digestEnv) deliverer(t *testing.T, pool *pgxpool.Pool) *Deliverer {
	t.Helper()
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := &notify.Sender{Client: g.Client(netguard.Options{Timeout: notify.SendTimeout, HTTPHosts: []string{"127.0.0.1"}, NoRedirects: true}),
		UserAgent: "gedour/test", TelegramURL: e.tg, PublicURL: "https://gedour.test"}
	return NewDeliverer(pool, zap.NewNop(), e.box, s, nil, e.m, wake.NewLoop("deliveries", 10*time.Millisecond, nil, nil), 4, time.Second)
}

func (e *digestEnv) namedWatch(t *testing.T, name string) string {
	t.Helper()
	w := e.watch(t, e.alice)
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET name = $2 WHERE id = $1`, w, name); err != nil {
		t.Fatal(err)
	}
	return w
}

// round makes every waiting delivery due and runs one pass.
func (e *digestEnv) round(t *testing.T, d *Deliverer) {
	t.Helper()
	e.makeDue(t)
	if _, err := d.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *digestEnv) inbox(t *testing.T, event int64, set string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE inbox_items SET `+set+` WHERE event_id = $1`, event); err != nil {
		t.Fatal(err)
	}
}

func (e *digestEnv) of(t *testing.T, event int64) delivery {
	t.Helper()
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries WHERE event_id = $1`, event)
	return e.delivery(t, id)
}

// 012-AC3: one message for the channel's due deliveries, grouped by watch, oldest first, silent applied;
// every delivery sent with the same sent_at.
func TestDigestSendGrouped(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET config = config || '{"silent": true}' WHERE id = $1`, ch); err != nil {
		t.Fatal(err)
	}
	a, b := e.namedWatch(t, "org/alpha"), e.namedWatch(t, "org/beta")
	e.digestRule(t, e.alice, nil, "", ch)
	e.event(t, a, "release.published", "v1")
	e.event(t, b, "release.published", "v2")
	e.event(t, a, "release.published", "v3")
	routeAll(t, e.router())
	d := e.deliverer(t, e.pool)
	if _, err := d.DeliverOnce(context.Background()); err != nil || len(e.messages()) != 0 {
		t.Fatalf("sent before the scheduled time: %v, %d", err, len(e.messages()))
	}
	e.round(t, d)
	got := e.messages()
	if len(got) != 1 || !got[0].Silent {
		t.Fatalf("%d messages (%+v), want 1 silent", len(got), got)
	}
	want := []string{"3 unread events", "org/alpha", "v1 published", "v3 published", "org/beta", "v2 published"}
	at := 0
	for _, w := range want {
		i := strings.Index(got[0].Text[at:], w)
		if i < 0 {
			t.Fatalf("%q missing or out of order in %q", w, got[0].Text)
		}
		at += i + len(w)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'sent'`, ch); n != 3 {
		t.Errorf("%d sent, want 3", n)
	}
	if n := e.count(t, `SELECT count(DISTINCT sent_at) FROM deliveries WHERE channel_id = $1`, ch); n != 1 {
		t.Errorf("%d distinct sent_at, want 1", n)
	}
}

// 012-AC4, 012-AC5: read and archived items are left out and end skipped, without an attempt; one marked
// unread again is listed; a digest with nothing left sends nothing.
func TestDigestSkipsRead(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", ch)
	read, archived, unread, again := e.event(t, w, "release.published", "v1"), e.event(t, w, "release.published", "v2"),
		e.event(t, w, "release.published", "v3"), e.event(t, w, "release.published", "v4")
	routeAll(t, e.router())
	e.inbox(t, read, "read_at = now()")
	e.inbox(t, archived, "archived_at = now()")
	e.inbox(t, again, "read_at = now()")
	e.inbox(t, again, "read_at = NULL")
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	got := e.messages()
	if len(got) != 1 || !strings.Contains(got[0].Text, "2 unread events") || !strings.Contains(got[0].Text, "v3 published") ||
		!strings.Contains(got[0].Text, "v4 published") || strings.Contains(got[0].Text, "v1 ") || strings.Contains(got[0].Text, "v2 ") {
		t.Fatalf("messages %+v", got)
	}
	for ev, reason := range map[int64]string{read: skippedRead, archived: skippedArchived} {
		if x := e.of(t, ev); x.status != "skipped" || x.attempts != 0 || x.lastErr == nil || *x.lastErr != reason || x.sent {
			t.Errorf("event %d: %+v (%s), want skipped: %s", ev, x, deref(x.lastErr), reason)
		}
	}
	for _, ev := range []int64{unread, again} {
		if x := e.of(t, ev); x.status != "sent" {
			t.Errorf("event %d: %s, want sent", ev, x.status)
		}
	}

	// 012-AC4: all read, nothing sent.
	a, b := e.event(t, w, "release.published", "v5"), e.event(t, w, "release.published", "v6")
	routeAll(t, e.router())
	e.inbox(t, a, "read_at = now()")
	e.inbox(t, b, "archived_at = now()")
	e.round(t, d)
	if n := len(e.messages()); n != 1 {
		t.Errorf("%d messages after an all-read digest, want 1", n)
	}
	if x, y := e.of(t, a), e.of(t, b); x.status != "skipped" || y.status != "skipped" {
		t.Errorf("all read: %s, %s", x.status, y.status)
	}
}

// 012-AC7: a failed digest retries together with the backoff; an event read before the retry drops out of
// it; the rows that shared every attempt die together after MaxAttempts, one that joined late on its own count.
func TestDigestRetry(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", ch)
	evs := []int64{e.event(t, w, "release.published", "v1"), e.event(t, w, "release.published", "v2"), e.event(t, w, "release.published", "v3")}
	routeAll(t, e.router())
	e.failing.Store(true)
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	for _, ev := range evs {
		if x := e.of(t, ev); x.status != "failed" || x.attempts != 1 || x.wait < Backoff[0].Seconds()-5 || x.wait > Backoff[0].Seconds() {
			t.Fatalf("after one failure: %+v", x)
		}
	}
	e.inbox(t, evs[0], "read_at = now()")
	e.round(t, d)
	if x := e.of(t, evs[0]); x.status != "skipped" || x.attempts != 1 {
		t.Errorf("read before the retry: %+v", x)
	}
	if got := e.messages(); len(got) != 2 || !strings.Contains(got[1].Text, "2 unread events") || strings.Contains(got[1].Text, "v1 ") {
		t.Fatalf("retry messages %+v", got)
	}
	late := e.event(t, w, "release.published", "v4")
	routeAll(t, e.router())
	for range MaxAttempts - 2 {
		e.round(t, d)
	}
	for _, ev := range evs[1:] {
		if x := e.of(t, ev); x.status != "dead" || x.attempts != MaxAttempts {
			t.Errorf("event %d after %d attempts: %+v", ev, MaxAttempts, x)
		}
	}
	if x := e.of(t, late); x.status != "failed" || x.attempts != MaxAttempts-2 {
		t.Errorf("late event: %+v, want failed at %d", x, MaxAttempts-2)
	}
	for range 2 {
		e.round(t, d)
	}
	if x := e.of(t, late); x.status != "dead" || x.attempts != MaxAttempts {
		t.Errorf("late event: %+v, want dead at %d", x, MaxAttempts)
	}
	if n := len(e.messages()); n != MaxAttempts+2 {
		t.Errorf("%d messages, want %d", n, MaxAttempts+2)
	}
}

// 012-AC10: two deliverers on their own pools, claiming side by side, send each channel's digest exactly once
// and whole, wave after wave.
func TestDigestTwoDeliverers(t *testing.T) {
	e := newDigestEnv(t)
	const channels, events, waves = 8, 30, 5
	w := e.namedWatch(t, "org/alpha")
	var ids []string
	for i := range channels {
		ch := e.tgChannel(t, e.alice)
		if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET config = config || jsonb_build_object('chat_id', $2::text) WHERE id = $1`,
			ch, fmt.Sprint(-1000-i)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ch)
	}
	e.digestRule(t, e.alice, &w, "", ids...)
	ds := []*Deliverer{e.deliverer(t, connect(t, e.url)), e.deliverer(t, connect(t, e.url))}
	for wave := range waves {
		for i := range events {
			e.event(t, w, "release.published", fmt.Sprintf("w%d-v%d", wave, i))
		}
		routeAll(t, e.router())
		e.makeDue(t)
		e.mu.Lock()
		e.got = nil
		e.mu.Unlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, d := range ds {
			wg.Go(func() {
				<-start
				for range 10 {
					if _, err := d.DeliverOnce(context.Background()); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		close(start)
		wg.Wait()
		per := map[string]int{}
		for _, m := range e.messages() {
			per[m.ChatID]++
			if !strings.Contains(m.Text, fmt.Sprintf("%d unread events", events)) {
				t.Errorf("wave %d, chat %s: a partial digest: %.40q", wave, m.ChatID, m.Text)
			}
		}
		for i := range channels {
			if n := per[fmt.Sprint(-1000-i)]; n != 1 {
				t.Errorf("wave %d, chat %d: %d messages, want 1", wave, -1000-i, n)
			}
		}
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent'`); n != channels*events*waves {
		t.Errorf("%d sent, want %d", n, channels*events*waves)
	}
}

// 012-AC12: a Web Push digest is one notification per device, titled with the count and opening the inbox;
// a retry reaches only the device that has not accepted it, and the acceptance covers every delivery of it.
func TestDigestWebPush(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	ok := p.device(t, alice, "ok", false)
	p.device(t, alice, "flaky", false)
	ch := p.channel(t, alice, true)
	w := p.watch(t, alice)
	p.digestRule(t, alice, &w, "", ch)
	p.event(t, w, "release.published", "v1")
	p.event(t, w, "release.published", "v2")
	routeAll(t, p.router())
	p.failing.Store(true)
	d := p.deliverer(t)
	p.makeDue(t)
	p.once(t, d)
	if n := p.pushes(); n["push-ok"] != 1 || n["push-flaky"] != 0 {
		t.Fatalf("first attempt: %v", n)
	}
	if n := p.count(t, `SELECT count(*) FROM delivery_push_sent WHERE device_id = $1`, ok); n != 2 {
		t.Errorf("accepting device recorded on %d deliveries, want 2", n)
	}
	p.failing.Store(false)
	p.makeDue(t)
	p.once(t, d)
	if n := p.pushes(); n["push-ok"] != 1 || n["push-flaky"] != 1 {
		t.Fatalf("after the retry: %v", n)
	}
	if n := p.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'sent'`, ch); n != 2 {
		t.Errorf("%d sent, want 2", n)
	}
	var got []notify.PushMessage
	for _, r := range p.up.Received() {
		var m notify.PushMessage
		if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	for _, m := range got {
		if m.Title != "2 unread events" || m.Body != "cli/cli" || m.URL != "/inbox" || !m.Silent || m.Tag != "gedour-digest-"+ch {
			t.Errorf("push %+v", m)
		}
	}
	for _, r := range p.up.Received() {
		if u := r.Header["Urgency"]; u != notify.UrgencyLow {
			t.Errorf("urgency %q, want low on a silent channel", u)
		}
	}
}

// 012-AC7, 003-AC8: a Talk digest answered 401 dies whole, then the pause holds the next digest whole,
// without a request or an attempt.
func TestDigestTalkPause(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "bad", wrongTalkSecret)
	e.digest = true
	e.events(t, 3, ch)
	e.rounds(t, 1)
	if n, dead := e.requests("talk-bad-rejected"), e.status(t, ch, "dead"); n != 1 || dead != 3 {
		t.Fatalf("%d requests, %d dead; want 1 request, 3 dead", n, dead)
	}
	e.events(t, 3, ch)
	e.rounds(t, 3)
	if n := e.requests("talk-bad-rejected"); n != 1 {
		t.Errorf("%d requests while paused, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'failed' AND attempts = 0 AND last_error = $2 AND claim_token IS NULL`,
		ch, talkPausedError); n != 3 {
		t.Errorf("%d held deliveries, want 3", n)
	}
}

// 012-AC10: a channel held by an edit in flight is passed over at once, nothing claimed; the claim does not wait on it.
func TestDigestClaimSkipsHeldChannel(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", ch)
	e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	e.makeDue(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// As updateChannel does.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, ch); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	if n, err := e.deliverer(t, e.pool).DeliverOnce(cctx); n != 0 || err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("claimed %d, %v after %s; want 0 at once", n, err, time.Since(start))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	e.round(t, e.deliverer(t, e.pool))
	if n := len(e.messages()); n != 1 {
		t.Errorf("%d messages once the channel is free, want 1", n)
	}
}

// 012-AC3: a full backlog of single deliveries still leaves one slot of the pass to a due digest.
func TestDigestThroughBacklog(t *testing.T) {
	e := newDigestEnv(t)
	dig, now := e.tgChannel(t, e.alice), e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", dig)
	e.rule(t, e.alice, &w, "", now)
	for i := range 10 {
		e.event(t, w, "release.published", fmt.Sprint("v", i))
	}
	routeAll(t, e.router())
	e.makeDue(t)
	if n, err := e.deliverer(t, e.pool).DeliverOnce(context.Background()); n != 4 || err != nil {
		t.Fatalf("claimed %d, %v; want 4", n, err)
	}
	digests := 0
	for _, m := range e.messages() {
		if strings.Contains(m.Text, "10 unread events") {
			digests++
		}
	}
	if got := len(e.messages()); digests != 1 || got != 4 {
		t.Errorf("%d messages, %d digests; want 3 single and 1 digest", got, digests)
	}
}

// 012-AC7: a manual retry takes the delivery out of the digest: sent alone, it does not join a batch whose
// attempt count is not its own.
func TestDigestManualRetry(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", ch)
	old, fresh := e.event(t, w, "release.published", "v1"), e.event(t, w, "release.published", "v2")
	routeAll(t, e.router())
	var id int64
	e.scalar(t, &id, `UPDATE deliveries SET status = 'dead', attempts = $2 WHERE event_id = $1 RETURNING id`, old, MaxAttempts)
	s := New(e.pool, zap.NewNop(), e.box, nil, nil, nil, nil, e.m)
	s.viewer = func(context.Context) (string, bool) { return e.alice, true }
	if _, err := s.retryDelivery(context.Background(), &deliveryPath{ID: id}); err != nil {
		t.Fatal(err)
	}
	if q := e.queued(t, old, ch); q.digest {
		t.Fatal("a retried delivery stays in the digest")
	}
	e.round(t, e.deliverer(t, e.pool))
	if got := e.messages(); len(got) != 2 {
		t.Fatalf("%d messages, want the retry alone and the digest", len(got))
	}
	if x, y := e.of(t, old), e.of(t, fresh); x.status != "sent" || x.attempts != MaxAttempts+1 || y.status != "sent" || y.attempts != 1 {
		t.Errorf("retried %+v, fresh %+v", x, y)
	}
}

// 012-AC7: in a digest, a delivery whose watch left the owner or whose owner is disabled ends dead; the rest is sent.
func TestDigestDeadBranches(t *testing.T) {
	e := newDigestEnv(t)
	bob, carol := e.user(t, "bob"), e.user(t, "carol")
	ch := e.tgChannel(t, e.alice)
	w, moved := e.namedWatch(t, "org/alpha"), e.namedWatch(t, "org/moved")
	e.digestRule(t, e.alice, nil, "", ch)
	kept, gone := e.event(t, w, "release.published", "v1"), e.event(t, moved, "release.published", "v2")
	cw := e.watch(t, carol)
	cch := e.tgChannel(t, carol)
	e.digestRule(t, carol, &cw, "", cch)
	off := e.event(t, cw, "release.published", "v3")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET owner_id = $2 WHERE id = $1`, moved, bob); err != nil {
		t.Fatal(err)
	}
	e.setInactive(t, carol, "disabled_at")
	e.round(t, e.deliverer(t, e.pool))
	for ev, want := range map[int64]error{gone: errUnreadable, off: errOwnerDisabled} {
		if x := e.of(t, ev); x.status != "dead" || x.lastErr == nil || *x.lastErr != want.Error() {
			t.Errorf("event %d: %+v (%s), want dead: %v", ev, x, deref(x.lastErr), want)
		}
	}
	if got := e.messages(); len(got) != 1 || !strings.Contains(got[0].Text, "1 unread event") || e.of(t, kept).status != "sent" {
		t.Errorf("messages %+v", got)
	}
}

// 012-AC7: a digest cut by the shutdown drain is handed back without an attempt; one whose lease another
// claim took over mid-send is left to that claim.
func TestDigestDrainAndTakeover(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", ch)
	ev := e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	e.makeDue(t)

	arrived := make(chan struct{}, 1)
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices the client leave only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	e.tg = hang.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = e.deliverer(t, e.pool).DeliverOnce(ctx); close(done) }()
	<-arrived
	cancel()
	<-done
	if x := e.of(t, ev); x.status != "pending" || x.attempts != 0 || e.count(t, `SELECT count(*) FROM deliveries WHERE claim_token IS NOT NULL`) != 0 {
		t.Errorf("after the drain: %+v", x)
	}

	steal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := e.pool.Exec(r.Context(), `UPDATE deliveries SET claim_token = gen_random_uuid() WHERE claim_token IS NOT NULL`); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(steal.Close)
	e.tg = steal.URL
	if _, err := e.deliverer(t, e.pool).DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if x := e.of(t, ev); x.status != "pending" || x.sent || x.attempts != 0 {
		t.Errorf("after the takeover: %+v, want it left to the other claim", x)
	}
}

// 012-AC10: a due row held by a rule delete in flight (rule_id SET NULL) keeps the whole channel back this
// pass, without waiting; the next pass sends the digest whole.
func TestDigestClaimSkipsHeldRow(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	a, b := e.namedWatch(t, "org/alpha"), e.namedWatch(t, "org/beta")
	ra := e.digestRule(t, e.alice, &a, "", ch)
	e.digestRule(t, e.alice, &b, "", ch)
	e.event(t, a, "release.published", "v1")
	e.event(t, b, "release.published", "v2")
	routeAll(t, e.router())
	e.makeDue(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM rules WHERE id = $1`, ra); err != nil {
		t.Fatal(err)
	}
	d := e.deliverer(t, e.pool)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if n, err := d.DeliverOnce(cctx); n != 0 || err != nil {
		t.Fatalf("claimed %d, %v while a row is held; want 0", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.messages(); len(got) != 1 || !strings.Contains(got[0].Text, "2 unread events") {
		t.Errorf("messages %+v, want one digest of 2", got)
	}
}

// 012-AC7: a fresh delivery joining rows at their last attempt keeps its own count: they die, it retries.
func TestDigestRetryOwnAttempts(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.digestRule(t, e.alice, &w, "", ch)
	old, fresh := e.event(t, w, "release.published", "v1"), e.event(t, w, "release.published", "v2")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET status = 'failed', attempts = $2 WHERE event_id = $1`, old, MaxAttempts-1); err != nil {
		t.Fatal(err)
	}
	e.failing.Store(true)
	e.round(t, e.deliverer(t, e.pool))
	if got := e.messages(); len(got) != 1 || !strings.Contains(got[0].Text, "2 unread events") {
		t.Fatalf("messages %+v, want one digest of 2", got)
	}
	x, y := e.of(t, old), e.of(t, fresh)
	if x.status != "dead" || x.attempts != MaxAttempts || y.status != "failed" || y.attempts != 1 {
		t.Errorf("old %+v, fresh %+v; want dead at %d, failed at 1", x, y, MaxAttempts)
	}
	// The batch's backoff, from its highest attempt: the survivor is not retried sooner than the batch would be.
	if y.wait < retryDelay(MaxAttempts).Seconds()-5 {
		t.Errorf("fresh retry in %.0fs, want the batch's %s", y.wait, retryDelay(MaxAttempts))
	}
}

// 012-AC3: with one send slot, a digest due before a backlog of single deliveries goes out first.
func TestDigestOneSlot(t *testing.T) {
	e := newDigestEnv(t)
	dig, now := e.tgChannel(t, e.alice), e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", now)
	for i := range 5 {
		e.event(t, w, "release.published", fmt.Sprint("v", i))
	}
	routeAll(t, e.router())
	e.digestRule(t, e.alice, &w, "", dig)
	e.event(t, w, "release.published", "d1")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = now() - CASE WHEN digest THEN interval '1 minute' ELSE interval '0' END`); err != nil {
		t.Fatal(err)
	}
	d := e.deliverer(t, e.pool)
	d.concurrency = 1
	for range 2 {
		if _, err := d.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	found := false
	for _, m := range e.messages() {
		found = found || strings.Contains(m.Text, "unread event")
	}
	if !found {
		t.Errorf("no digest within 2 passes of one slot: %d messages", len(e.messages()))
	}
}

// 012-AC3: the digest channel due longest goes first.
func TestDigestOldestChannelFirst(t *testing.T) {
	e := newDigestEnv(t)
	w := e.namedWatch(t, "org/alpha")
	for i, age := range []string{"1 minute", "2 minutes", "30 seconds"} {
		ch := e.tgChannel(t, e.alice)
		if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET config = config || jsonb_build_object('chat_id', $2::text) WHERE id = $1`, ch, fmt.Sprint(-1000-i)); err != nil {
			t.Fatal(err)
		}
		e.digestRule(t, e.alice, &w, "", ch)
		e.event(t, w, "release.published", fmt.Sprint("v", i))
		routeAll(t, e.router())
		if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = now() - $2::interval WHERE channel_id = $1`, ch, age); err != nil {
			t.Fatal(err)
		}
	}
	d := e.deliverer(t, e.pool)
	d.concurrency = 1
	for range 3 {
		if _, err := d.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	for _, m := range e.messages() {
		order = append(order, m.ChatID)
	}
	if want := []string{"-1001", "-1000", "-1002"}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("order %v, want %v", order, want)
	}
}
