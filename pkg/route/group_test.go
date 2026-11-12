package route

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/notify"
)

// poll inserts the events of one poll of w in one transaction: one observed_at, as the worker writes them.
func (e *env) poll(t *testing.T, w string, tags ...string) []int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids := make([]int64, len(tags))
	for i, tag := range tags {
		if err := tx.QueryRow(ctx, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', jsonb_build_object('tag', $2::text), $2 || random()::text) RETURNING id`,
			w, tag).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}

// inOrder fails unless every want appears in text, in that order.
func inOrder(t *testing.T, text string, want ...string) {
	t.Helper()
	at := 0
	for _, w := range want {
		i := strings.Index(text[at:], w)
		if i < 0 {
			t.Fatalf("%q missing or out of order in %q", w, text)
		}
		at += i + len(w)
	}
}

// 017-AC1, 017-AC9, 017-AC3: two events of one poll go out as one Telegram message, oldest first, even read
// in the inbox before the send; both sent with one sent_at. A poll of one event sends the single message as before.
func TestGroupTelegram(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch)
	evs := e.poll(t, w, "v1", "v2")
	routeAll(t, e.router())
	e.inbox(t, evs[0], "read_at = now()")
	e.inbox(t, evs[1], "archived_at = now()")
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	got := e.messages()
	if len(got) != 1 {
		t.Fatalf("%d messages (%+v), want 1", len(got), got)
	}
	inOrder(t, got[0].Text, "<b>2 new events</b>", "org/alpha", "v1 published", "v2 published")
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'sent' AND attempts = 1`, ch); n != 2 {
		t.Errorf("%d sent, want 2", n)
	}
	if n := e.count(t, `SELECT count(DISTINCT sent_at) FROM deliveries WHERE channel_id = $1`, ch); n != 1 {
		t.Errorf("%d distinct sent_at, want 1", n)
	}

	// 017-AC3: a single event, byte-identical to the message before this spec.
	e.poll(t, w, "v9")
	routeAll(t, e.router())
	e.round(t, d)
	want := notify.TelegramText(notify.Format(notify.Delivery{Watch: notify.Watch{Name: "org/alpha"},
		Event: notify.Event{Type: "release.published", Payload: map[string]any{"tag": "v9"}}}))
	if got := e.messages(); len(got) != 2 || got[1].Text != want {
		t.Fatalf("single: %+v, want %q", got, want)
	}
}

// 017-AC4, 017-AC5: two polls of one watch, or one poll time on two watches, routed in one pass, are separate messages.
func TestGroupPollAndWatchApart(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	a, b := e.namedWatch(t, "org/alpha"), e.namedWatch(t, "org/beta")
	e.rule(t, e.alice, nil, "", ch)
	e.poll(t, a, "v1", "v2")
	e.poll(t, a, "v3", "v4")
	bs := e.poll(t, b, "v5", "v6")
	// Same observed_at as the first poll of a: only the watch tells them apart.
	if _, err := e.pool.Exec(context.Background(), `UPDATE events SET observed_at = (SELECT min(observed_at) FROM events WHERE watch_id = $1) WHERE id = ANY($2)`, a, bs); err != nil {
		t.Fatal(err)
	}
	routeAll(t, e.router())
	d := e.deliverer(t, e.pool)
	for range 3 {
		e.round(t, d)
	}
	got := e.messages()
	if len(got) != 3 {
		t.Fatalf("%d messages (%+v), want 3", len(got), got)
	}
	texts := map[string]bool{}
	for _, m := range got {
		if !strings.HasPrefix(m.Text, "<b>2 new events</b>") {
			t.Errorf("message %q, want a group of 2", m.Text)
		}
		for _, pair := range [][2]string{{"v1 ", "v2 "}, {"v3 ", "v4 "}, {"v5 ", "v6 "}} {
			if strings.Contains(m.Text, pair[0]) && strings.Contains(m.Text, pair[1]) {
				texts[pair[0]] = true
			}
		}
	}
	if len(texts) != 3 {
		t.Errorf("groups %v, want v1+v2, v3+v4, v5+v6", texts)
	}
}

// 017-AC7: a high and a normal event of one poll are two messages; two high events of one poll are one.
func TestGroupPriority(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.setPriority(t, e.rule(t, e.alice, &w, `event.payload.tag startsWith "h"`, ch), "high")
	e.rule(t, e.alice, &w, `event.payload.tag startsWith "n"`, ch)
	e.poll(t, w, "h1", "n1")
	routeAll(t, e.router())
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	if got := e.messages(); len(got) != 2 || strings.Contains(got[0].Text, "new events") || strings.Contains(got[1].Text, "new events") {
		t.Fatalf("high + normal: %+v, want 2 single messages", got)
	}
	e.poll(t, w, "h2", "h3")
	routeAll(t, e.router())
	e.round(t, d)
	got := e.messages()
	if len(got) != 3 {
		t.Fatalf("%d messages, want 3", len(got))
	}
	inOrder(t, got[2].Text, "2 new events", "h2 published", "h3 published")
}

// 017-AC8: a failed group retries whole, with the backoff, as one message each time; a row dies on its own
// count while the rest keep retrying together.
func TestGroupRetry(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch)
	evs := e.poll(t, w, "v1", "v2", "v3")
	routeAll(t, e.router())
	e.failing.Store(true)
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	if n := len(e.messages()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	for _, ev := range evs {
		if x := e.of(t, ev); x.status != "failed" || x.attempts != 1 || x.wait < 20 || x.wait > 31 {
			t.Errorf("event %d after the first failure: %+v", ev, x)
		}
	}
	if n := e.count(t, `SELECT count(DISTINCT next_attempt_at) FROM deliveries WHERE channel_id = $1`, ch); n != 1 {
		t.Errorf("%d distinct retry times, want 1", n)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET attempts = $2 - 1 WHERE event_id = $1`, evs[0], MaxAttempts); err != nil {
		t.Fatal(err)
	}
	e.round(t, d)
	if n := len(e.messages()); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
	if x := e.of(t, evs[0]); x.status != "dead" || x.attempts != MaxAttempts {
		t.Errorf("exhausted row: %+v, want dead", x)
	}
	for _, ev := range evs[1:] {
		if x := e.of(t, ev); x.status != "failed" || x.attempts != 2 {
			t.Errorf("event %d after the second failure: %+v", ev, x)
		}
	}
	e.failing.Store(false)
	e.round(t, d)
	got := e.messages()
	if len(got) != 3 || !strings.Contains(got[2].Text, "2 new events") || strings.Contains(got[2].Text, "v1 ") {
		t.Fatalf("after recovery: %+v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'sent'`, ch); n != 2 {
		t.Errorf("%d sent, want 2", n)
	}
}

// 017-AC10: a member leased or locked by another claim keeps its group back whole; once free, the group goes out as one.
func TestGroupHeldMember(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch)
	evs := e.poll(t, w, "v1", "v2")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET claim_token = gen_random_uuid(), leased_until = now() + interval '1 minute' WHERE event_id = $1`, evs[1]); err != nil {
		t.Fatal(err)
	}
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	if n := len(e.messages()); n != 0 {
		t.Fatalf("%d messages with a member held, want 0", n)
	}
	if x := e.of(t, evs[0]); x.status != "pending" || x.attempts != 0 {
		t.Fatalf("free member: %+v, want pending untouched", x)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND claim_token IS NULL`, evs[0]); n != 1 {
		t.Fatal("free member left leased")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET claim_token = NULL, leased_until = NULL`); err != nil {
		t.Fatal(err)
	}
	// A row lock (a purge, a manual retry in flight) holds the group back the same way.
	ctx := context.Background()
	e.makeDue(t)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM deliveries WHERE event_id = $1 FOR UPDATE`, evs[1]); err != nil {
		t.Fatal(err)
	}
	_, err = d.DeliverOnce(ctx)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.messages()); n != 0 {
		t.Fatalf("%d messages with a member locked, want 0", n)
	}
	e.round(t, d)
	if got := e.messages(); len(got) != 1 || !strings.Contains(got[0].Text, "2 new events") {
		t.Fatalf("after release: %+v", got)
	}
}

// 017-AC11: a group over Telegram's cap is cut on a whole line and ends with "+N more" and the inbox link.
func TestGroupCap(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch)
	tags := make([]string, 200)
	for i := range tags {
		tags[i] = fmt.Sprintf("v%d-%s", i, strings.Repeat("x", 40))
	}
	e.poll(t, w, tags...)
	routeAll(t, e.router())
	e.round(t, e.deliverer(t, e.pool))
	got := e.messages()
	if len(got) != 1 {
		t.Fatalf("%d messages, want 1", len(got))
	}
	text := got[0].Text
	lines := strings.Split(text, "\n")
	last := lines[len(lines)-1]
	listed := strings.Count(text, "\n• ")
	if len(text) > 4096 || listed == 0 || last != fmt.Sprintf(`+%d more: <a href="https://gedour.test/inbox">open the inbox</a>`, len(tags)-listed) {
		t.Fatalf("%d bytes, %d listed, last line %q", len(text), listed, last)
	}
	if !strings.HasPrefix(text, "<b>200 new events</b>") {
		t.Errorf("header %q", lines[0])
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'sent'`, ch); n != len(tags) {
		t.Errorf("%d sent, want %d", n, len(tags))
	}
}

// 017-AC2: one poll on Nextcloud Talk is one message.
func TestGroupTalk(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "abcd", testupstream.TalkSecret)
	e.rule(t, e.alice, &e.watch, "", ch)
	e.poll(t, e.watch, "v1", "v2")
	routeAll(t, e.router())
	e.rounds(t, 1)
	got := e.fake.Received()
	if len(got) != 1 || !strings.Contains(got[0].Body, "**2 new events**") || !strings.Contains(got[0].Body, "v2 published") {
		t.Fatalf("talk received %+v", got)
	}
	if n := e.status(t, ch, "sent"); n != 2 {
		t.Errorf("%d sent, want 2", n)
	}
}

// 017-AC2: one poll on Web Push is one notification per device, "N new events" with the watch name, opening
// the inbox, high urgency kept for a high group.
func TestGroupWebPush(t *testing.T) {
	p := newPushEnv(t)
	alice := p.user(t, "alice")
	p.device(t, alice, "a", false)
	p.device(t, alice, "b", false)
	ch := p.channel(t, alice, false)
	w := p.watch(t, alice)
	p.setPriority(t, p.rule(t, alice, &w, "", ch), "high")
	p.poll(t, w, "v1", "v2")
	routeAll(t, p.router())
	var lead int64
	p.scalar(t, &lead, `SELECT min(id) FROM deliveries WHERE channel_id = $1`, ch)
	p.once(t, p.deliverer(t))
	if n := p.pushes(); len(n) != 2 || n["push-a"] != 1 || n["push-b"] != 1 {
		t.Fatalf("pushes %v, want one per device", n)
	}
	for _, r := range p.up.Received() {
		var m notify.PushMessage
		if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
			t.Fatal(err)
		}
		if m.Title != "2 new events" || m.Body != "cli/cli" || m.URL != "/inbox" || m.Silent || m.Tag != "gedour-poll-"+strconv.FormatInt(lead, 10) {
			t.Errorf("push %+v", m)
		}
		if u := r.Header["Urgency"]; u != notify.UrgencyHigh {
			t.Errorf("urgency %q, want high", u)
		}
	}
	if n := p.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'sent'`, ch); n != 2 {
		t.Errorf("%d sent, want 2", n)
	}
}

// 017-AC2, 017-AC6: an email channel claims one poll as one batch; webhook and script channels and a rule
// test send (no rule) claim each event alone.
func TestGroupClaimByType(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	var mail string
	e.scalar(t, &mail, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, 'mail', 'email', '{"to": ["a@x.test"]}') RETURNING id::text`, alice)
	hook := e.webhook(t, alice, "https://hooks.example/a")
	script := e.scriptChannel(t, alice, "")
	e.rule(t, alice, &w, "", mail, hook, script)
	e.poll(t, w, "v1", "v2")
	routeAll(t, e.router())
	d := e.deliverer(t)
	claim := func() map[string][]int {
		t.Helper()
		jobs, err := d.claimSingles(context.Background(), 10)
		if err != nil {
			t.Fatal(err)
		}
		per := map[string][]int{}
		for _, j := range jobs {
			per[j.channelID] = append(per[j.channelID], len(j.batch))
			if (len(j.batch) > 0) != j.poll {
				t.Errorf("job %+v: poll %v with a batch of %d", j, j.poll, len(j.batch))
			}
		}
		return per
	}
	per := claim()
	if b := per[mail]; len(b) != 1 || b[0] != 2 {
		t.Errorf("email jobs %v, want one batch of 2", b)
	}
	for _, c := range []string{hook, script} {
		if b := per[c]; len(b) != 2 || b[0] != 0 || b[1] != 0 {
			t.Errorf("channel %s jobs %v, want 2 alone", c, b)
		}
	}

	// Deliveries whose rule is gone carry no priority: each goes alone.
	evs := e.poll(t, w, "v3", "v4")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET rule_id = NULL WHERE event_id = ANY($1)`, evs); err != nil {
		t.Fatal(err)
	}
	if b := claim()[mail]; len(b) != 2 || b[0] != 0 || b[1] != 0 {
		t.Errorf("email jobs without a rule %v, want 2 alone", b)
	}
}

// 017-AC1, 017-AC10: two claimers racing on one poll contend on its lead row: the group is claimed exactly once,
// whole, never lost to both.
func TestGroupConcurrentClaim(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch)
	ds := []*Deliverer{e.deliverer(t, e.pool), e.deliverer(t, connect(t, e.url))}
	for round := range 8 {
		e.poll(t, w, fmt.Sprint("a", round), fmt.Sprint("b", round))
		routeAll(t, e.router())
		var wg sync.WaitGroup
		start := make(chan struct{})
		jobs := make([][]claimed, len(ds))
		for i, d := range ds {
			wg.Go(func() {
				<-start
				var err error
				if jobs[i], err = d.claimSingles(context.Background(), 1); err != nil {
					t.Error(err)
				}
			})
		}
		close(start)
		wg.Wait()
		var got []claimed
		for _, j := range jobs {
			got = append(got, j...)
		}
		if len(got) != 1 || len(got[0].batch) != 2 || !got[0].poll {
			t.Fatalf("round %d: claimed %+v, want one group of 2", round, got)
		}
		if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET status = 'sent', claim_token = NULL, leased_until = NULL`); err != nil {
			t.Fatal(err)
		}
	}
}

// 017-AC1: a poll the router splits over two passes waits for its last event, then goes out as one message; a
// webhook delivery of the routed event is not held by it.
func TestGroupSplitRouting(t *testing.T) {
	e := newDigestEnv(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	ch, hook := e.tgChannel(t, e.alice), e.webhook(t, e.alice, srv.URL)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch, hook)
	evs := e.poll(t, w, "v1", "v2")
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, evs[1]); err != nil {
		t.Fatal(err)
	}
	if n, err := e.router().RouteOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first pass routed %d, %v; want 1", n, err)
	}
	d := e.deliverer(t, e.pool)
	e.round(t, d)
	if n := len(e.messages()); n != 0 || hits.Load() != 1 {
		t.Fatalf("poll half routed: %d messages, %d webhook hits; want 0 and 1", n, hits.Load())
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	routeAll(t, e.router())
	e.round(t, d)
	if got := e.messages(); len(got) != 1 || !strings.Contains(got[0].Text, "2 new events") || hits.Load() != 2 {
		t.Fatalf("after routing: %+v, %d webhook hits", got, hits.Load())
	}
}

// 017-AC10: a group held back by a leased member takes no claim slot: the next due delivery goes out.
func TestGroupHeldTakesNoSlot(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	a, b := e.namedWatch(t, "org/alpha"), e.namedWatch(t, "org/beta")
	e.rule(t, e.alice, nil, "", ch)
	evs := e.poll(t, a, "v1", "v2")
	other := e.event(t, b, "release.published", "v3")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = now() - interval '1 minute' WHERE event_id = ANY($1)`, evs); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET claim_token = gen_random_uuid(), leased_until = now() + interval '1 minute' WHERE event_id = $1`, evs[1]); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.deliverer(t, e.pool).claimSingles(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].batch != nil || jobs[0].eventID != other {
		t.Fatalf("claimed %+v, want the other watch's delivery", jobs)
	}
}

// 017-AC10: with one free slot, a poll group held back by a leased member does not starve a due digest.
func TestGroupHeldNoDigestStarve(t *testing.T) {
	e := newDigestEnv(t)
	ch, dch := e.tgChannel(t, e.alice), e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, `event.payload.tag startsWith "v"`, ch)
	e.digestRule(t, e.alice, &w, `event.payload.tag startsWith "d"`, dch)
	evs := e.poll(t, w, "v1", "v2")
	e.event(t, w, "release.published", "d1")
	routeAll(t, e.router())
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET next_attempt_at = now() - CASE WHEN digest THEN interval '10 seconds' ELSE interval '1 minute' END`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET claim_token = gen_random_uuid(), leased_until = now() + interval '1 minute' WHERE event_id = $1`, evs[1]); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.deliverer(t, e.pool).claim(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].channelID != dch || jobs[0].poll || len(jobs[0].batch) != 1 {
		t.Fatalf("claimed %+v, want the digest", jobs)
	}
}

// explainClaim is the plan of the single-delivery claim, as claimSingles runs it.
func (e *env) explainClaim(t *testing.T) string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `EXPLAIN `+claimSinglesSQL, DeliveryLease.Seconds(), 1, digestTypes)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

// 017-AC1: with a due backlog the claim finds each poll's events through events_watch_observed_idx, never a scan of
// events per due row; dropping the index brings the scan back, so the check can fail.
func TestGroupClaimUsesIndex(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch := e.tgChannel(t, alice)
	rule := e.rule(t, alice, &w, "", ch)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `WITH ev AS (INSERT INTO events (watch_id, type, payload, dedup_key, observed_at, routed_at)
			SELECT $1, 'release.published', jsonb_build_object('tag', 'v' || i), 'k' || i, now() - make_interval(secs => i), now()
			FROM generate_series(1, 2000) i RETURNING id)
		INSERT INTO deliveries (event_id, rule_id, channel_id, status, attempts, next_attempt_at)
		SELECT id, $2, $3, 'failed', 1, now() - interval '1 minute' FROM ev`, w, rule, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `ANALYZE events, deliveries`); err != nil {
		t.Fatal(err)
	}
	if plan := e.explainClaim(t); strings.Contains(plan, "Seq Scan on events") {
		t.Fatalf("claim scans events:\n%s", plan)
	}
	if _, err := e.pool.Exec(ctx, `DROP INDEX events_watch_observed_idx`); err != nil {
		t.Fatal(err)
	}
	if plan := e.explainClaim(t); !strings.Contains(plan, "Seq Scan on events") {
		t.Fatalf("no events scan without the index, the check proves nothing:\n%s", plan)
	}
}

// 017-AC3: a claimed poll cut to one event before the send (the other's event deleted) sends the single message.
func TestGroupCutToOne(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", ch)
	evs := e.poll(t, w, "v1", "v2")
	routeAll(t, e.router())
	ctx := context.Background()
	d := e.deliverer(t, e.pool)
	jobs, err := d.claimSingles(ctx, 1)
	if err != nil || len(jobs) != 1 || !jobs[0].poll {
		t.Fatalf("claimed %+v, %v; want one poll group", jobs, err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, evs[1]); err != nil {
		t.Fatal(err)
	}
	d.deliver(ctx, jobs[0])
	want := notify.TelegramText(notify.Format(notify.Delivery{Watch: notify.Watch{Name: "org/alpha"},
		Event: notify.Event{Type: "release.published", Payload: map[string]any{"tag": "v1"}}}))
	if got := e.messages(); len(got) != 1 || got[0].Text != want {
		t.Fatalf("got %+v, want the single message %q", got, want)
	}
	if x := e.of(t, evs[0]); x.status != "sent" || x.attempts != 1 {
		t.Errorf("survivor %+v, want sent", x)
	}
}
