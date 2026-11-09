package route

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/notify"
)

const wrongTalkSecret = "wrong-talk-bot-shared-secret-0123456789abcdef" //nolint:gosec // test fixture

type talkEnv struct {
	*env
	fake  *testupstream.Server
	url   string
	alice string
	watch string
	d     *Deliverer
	// digest makes the rule of events a 07:00 UTC digest.
	digest bool
}

func newTalkEnv(t *testing.T) *talkEnv {
	t.Helper()
	e := newEnv(t)
	fake := testupstream.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	alice := e.user(t, "alice")
	d := e.deliverer(t)
	d.concurrency = 8
	return &talkEnv{env: e, fake: fake, url: srv.URL, alice: alice, watch: e.watch(t, alice), d: d}
}

func (e *talkEnv) channel(t *testing.T, conversation, sec string) string {
	t.Helper()
	cfg, _ := json.Marshal(notify.NextcloudTalkConfig{URL: e.url, Conversation: conversation})
	var id string
	// Edited long ago: only the time window can end a pause, unless a test edits the channel.
	e.scalar(t, &id, `INSERT INTO channels (owner_id, name, type, config, updated_at) VALUES ($1, $2, 'nextcloud_talk', $3, now() - interval '1 day') RETURNING id::text`, e.alice, conversation, cfg)
	e.setSecret(t, id, sec)
	return id
}

// setSecret writes the secret behind gedour's back: updated_at stays, as when the Nextcloud side changes.
func (e *talkEnv) setSecret(t *testing.T, id, sec string) {
	t.Helper()
	ct, nonce := e.box.Seal([]byte(sec), []byte(id))
	if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET secret_ciphertext = $2, secret_nonce = $3, key_version = 1 WHERE id = $1`, id, ct, nonce); err != nil {
		t.Fatal(err)
	}
}

// events routes n new events to the channels through a rule removed afterwards.
func (e *talkEnv) events(t *testing.T, n int, channels ...string) {
	t.Helper()
	rule := e.rule(t, e.alice, nil, "", channels...)
	if e.digest {
		e.setSchedule(t, rule, "07:00", "UTC")
	}
	for i := range n {
		e.event(t, e.watch, "release.published", fmt.Sprintf("v%d", i))
	}
	routeAll(t, e.router())
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM rule_channels WHERE rule_id = $1`, rule); err != nil {
		t.Fatal(err)
	}
}

// rounds makes everything due and polls, n times.
func (e *talkEnv) rounds(t *testing.T, n int) {
	t.Helper()
	for range n {
		e.makeDue(t)
		if _, err := e.d.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *talkEnv) requests(endpoint string) int {
	n := 0
	for _, r := range e.fake.Received() {
		if r.Endpoint == endpoint {
			n++
		}
	}
	return n
}

// age moves the channel's dead deliveries back in time.
func (e *talkEnv) age(t *testing.T, channel string, by time.Duration) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = now() - make_interval(secs => $2)
		WHERE channel_id = $1 AND status = 'dead'`, channel, by.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func (e *talkEnv) service(t *testing.T) (*Service, context.Context) {
	t.Helper()
	s := New(e.pool, zap.NewNop(), e.box, e.d.sender, e.d.runner, nil, nil, e.m)
	s.viewer = func(context.Context) (string, bool) { return e.alice, true }
	return s, context.Background()
}

func (e *talkEnv) status(t *testing.T, channel, status string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = $2`, channel, status)
}

// 003-AC8: 20 deliveries with a wrong secret and 8 send slots make one request; the 19 others wait
// without using an attempt, far past the 5 attempts of the backoff; a valid channel on the server sends.
func TestTalkPauseOneRequest(t *testing.T) {
	e := newTalkEnv(t)
	bad, good := e.channel(t, "bad", wrongTalkSecret), e.channel(t, "good", testupstream.TalkSecret)
	e.events(t, 20, bad)
	e.rounds(t, 2)
	if n := e.requests("talk-bad-rejected"); n != 1 {
		t.Fatalf("%d requests with the wrong secret, want 1", n)
	}
	e.events(t, 20, good)
	for range 200 {
		e.rounds(t, 1)
		if e.status(t, good, "sent") == 20 {
			break
		}
	}
	e.rounds(t, MaxAttempts+2)
	if n := e.requests("talk-bad-rejected"); n != 1 {
		t.Errorf("%d requests with the wrong secret after the valid channel ran, want 1", n)
	}
	if n := e.requests("talk-good"); n != 20 {
		t.Errorf("valid channel got %d requests, want 20", n)
	}
	if n := e.status(t, bad, "dead"); n != 1 {
		t.Errorf("%d dead, want the 1 delivery really refused", n)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'failed' AND attempts = 0 AND last_error = $2`, bad, talkPausedError); n != 19 {
		t.Errorf("%d held without an attempt, want 19", n)
	}
}

// 003-AC8: a slow 401 leaves the others in flight without a recorded failure: one request still goes out.
func TestTalkPauseBurst(t *testing.T) {
	e := newTalkEnv(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	e.url = srv.URL
	e.events(t, 20, e.channel(t, "room", wrongTalkSecret))
	e.rounds(t, 2)
	if n := hits.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// 003-AC8: a send cut before the 401 keeps its one attempt, then waits behind the pause: one 401, no more
// requests, whichever delivery got the failure.
func TestTalkPauseAfterFailedSend(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		fail       func(t *testing.T, w http.ResponseWriter)
	}{
		{"connection reset", "connection reset by peer", func(t *testing.T, w http.ResponseWriter) {
			t.Helper()
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.(*net.TCPConn).SetLinger(0) // Close sends RST, not FIN
			_ = conn.Close()
		}},
		{"server error", "HTTP 500", func(_ *testing.T, w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTalkEnv(t)
			var hits, refused atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					tt.fail(t, w)
					return
				}
				refused.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer srv.Close()
			e.url = srv.URL
			ch := e.channel(t, "room", wrongTalkSecret)
			e.events(t, 2, ch)
			// No makeDue: the failed delivery stays in its backoff, as on the real clock.
			for range 200 {
				if _, err := e.d.DeliverOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				if e.status(t, ch, "dead") == 1 {
					break
				}
				time.Sleep(e.d.poll)
			}
			var failed, dead int64
			e.scalar(t, &failed, `SELECT id FROM deliveries WHERE channel_id = $1 AND status = 'failed'`, ch)
			e.scalar(t, &dead, `SELECT id FROM deliveries WHERE channel_id = $1 AND status = 'dead'`, ch)
			got := e.delivery(t, failed)
			if got.attempts != 1 || !strings.Contains(*got.lastErr, tt.want) {
				t.Errorf("failed send: %d attempts, %q", got.attempts, *got.lastErr)
			}
			if got := e.delivery(t, dead); got.attempts != 1 || !strings.HasSuffix(*got.lastErr, ": HTTP 401") {
				t.Errorf("refused send: %d attempts, %q", got.attempts, *got.lastErr)
			}
			e.rounds(t, MaxAttempts+2)
			if got := e.delivery(t, failed); got.status != "failed" || got.attempts != 1 || *got.lastErr != talkPausedError {
				t.Errorf("behind the pause: %s, %d attempts, %q", got.status, got.attempts, *got.lastErr)
			}
			if n := refused.Load(); n != 1 {
				t.Errorf("%d requests refused, want 1", n)
			}
			if n := hits.Load(); n != 2 {
				t.Errorf("%d requests, want the failed one and the refused one", n)
			}
		})
	}
}

// 003-AC8: an older dead delivery retried by hand during the pause, its attempts used up, is held, not dead.
func TestTalkPauseKeepsExhausted(t *testing.T) {
	e := newTalkEnv(t)
	bad := e.channel(t, "bad", wrongTalkSecret)
	e.events(t, 2, bad)
	e.rounds(t, 1)
	e.rounds(t, 1)
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET attempts = $2 WHERE channel_id = $1 AND status = 'failed'`, bad, MaxAttempts); err != nil {
		t.Fatal(err)
	}
	e.rounds(t, 3)
	if n := e.status(t, bad, "failed"); n != 1 {
		t.Fatalf("exhausted delivery not held: %d failed", n)
	}
	if n := e.requests("talk-bad-rejected"); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

// 003-AC8: any edit lifts the pause at once, even right after the failure, and a new secret delivers
// what waited. A name edit keeps the wrong secret: one more request, then a new pause.
func TestTalkPauseLiftedByEdit(t *testing.T) {
	good := testupstream.TalkSecret
	name := "renamed"
	for _, tt := range []struct {
		name         string
		sec, newName *string
		sent, bad    int
	}{
		{name: "secret rotated", sec: &good, sent: 2, bad: 1},
		{name: "renamed", newName: &name, sent: 0, bad: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTalkEnv(t)
			ch := e.channel(t, "room", wrongTalkSecret)
			e.events(t, 3, ch)
			e.rounds(t, 3)
			if n := e.requests("talk-room-rejected"); n != 1 {
				t.Fatalf("%d requests before the edit", n)
			}
			s, ctx := e.service(t)
			in := &updateChannelInput{ID: ch}
			in.Body.Secret, in.Body.Name = tt.sec, tt.newName
			if _, err := s.updateChannel(ctx, in); err != nil {
				t.Fatal(err)
			}
			e.rounds(t, 4)
			if got := e.requests("talk-room"); got != tt.sent {
				t.Errorf("%d sent, want %d", got, tt.sent)
			}
			if got := e.requests("talk-room-rejected"); got != tt.bad {
				t.Errorf("%d refused, want %d", got, tt.bad)
			}
		})
	}
}

// 003-AC8: only a 401 pauses; a 429 or 404 keeps going, as does a dead delivery whose host holds "401".
func TestTalkPauseOnly401(t *testing.T) {
	e := newTalkEnv(t)
	for _, conv := range []string{"force429x", "force404x"} {
		ch := e.channel(t, conv, testupstream.TalkSecret)
		e.events(t, 3, ch)
		e.rounds(t, 3)
		if got := e.requests("talk-" + conv + "-rejected"); got < 3 {
			t.Errorf("%s: %d requests in 3 rounds", conv, got)
		}
		if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND last_error = $2`, ch, talkPausedError); n != 0 {
			t.Errorf("%s: %d deliveries paused", conv, n)
		}
	}
	ch := e.channel(t, "room", testupstream.TalkSecret)
	e.events(t, 1, ch)
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET status = 'dead', last_error = 'POST nc:401: HTTP 500' WHERE channel_id = $1`, ch); err != nil {
		t.Fatal(err)
	}
	e.events(t, 1, ch)
	e.rounds(t, 1)
	if got := e.requests("talk-room"); got != 1 {
		t.Errorf("a host holding 401 paused the channel: %d sent", got)
	}
}

// 003-AC8: once the pause ends the deliveries go out, one request at a time.
func TestTalkPauseEnds(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", wrongTalkSecret)
	e.events(t, 4, ch)
	e.rounds(t, 2)
	e.setSecret(t, ch, testupstream.TalkSecret) // fixed on the Nextcloud side: no channel edit
	e.rounds(t, 2)
	if n := e.requests("talk-room"); n != 0 {
		t.Fatalf("%d sent during the pause", n)
	}
	e.age(t, ch, 29*time.Minute)
	e.rounds(t, 2)
	if n := e.requests("talk-room"); n != 0 {
		t.Fatalf("%d sent a minute before the pause ends", n)
	}
	e.age(t, ch, 30*time.Minute+time.Second)
	e.rounds(t, 6)
	if n := e.requests("talk-room"); n != 3 {
		t.Errorf("%d sent after the pause, want the 3 held", n)
	}
	if n := e.requests("talk-room-rejected"); n != 1 {
		t.Errorf("%d refused, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE status = 'sent' AND last_error IS NOT NULL`); n != 0 {
		t.Errorf("%d sent deliveries still show the hold", n)
	}
}

// 003-AC8: one poll clears a backlog larger than the send slots, paused or waiting on a request in flight,
// so it cannot keep other channels out.
func TestTalkHoldClearsBacklog(t *testing.T) {
	for _, tt := range []struct {
		name string
		code int
	}{{"paused", http.StatusUnauthorized}, {"busy", http.StatusCreated}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTalkEnv(t)
			e.d.poll = time.Hour // a held delivery stays not due for the assertion
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				time.Sleep(200 * time.Millisecond)
				w.WriteHeader(tt.code)
			}))
			defer srv.Close()
			e.url = srv.URL
			e.events(t, 40, e.channel(t, "room", testupstream.TalkSecret))
			if tt.code == http.StatusUnauthorized {
				e.rounds(t, 1)
				hits.Store(0)
			}
			e.makeDue(t)
			if _, err := e.d.DeliverOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := e.count(t, `SELECT count(*) FROM deliveries WHERE status IN ('pending', 'failed') AND next_attempt_at <= now()`); n != 0 {
				t.Errorf("%d deliveries left due", n)
			}
			if tt.code == http.StatusUnauthorized && hits.Load() != 0 {
				t.Errorf("%d requests during the pause", hits.Load())
			}
		})
	}
}

// 003-AC8: a held delivery wakes after 30 s when paused and half a poll when waiting on a request in
// flight; a delivery still in its own backoff stays out of the hold.
func TestTalkHoldDelays(t *testing.T) {
	for _, tt := range []struct {
		name          string
		code          int
		status        string
		minWait, wait float64
	}{
		{"paused", http.StatusUnauthorized, "failed", 25, 31},
		{"busy", http.StatusCreated, "pending", 55, 61},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTalkEnv(t)
			e.d.poll = 2 * time.Minute
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(200 * time.Millisecond)
				w.WriteHeader(tt.code)
			}))
			defer srv.Close()
			e.url = srv.URL
			e.events(t, 6, e.channel(t, "room", testupstream.TalkSecret))
			if tt.code == http.StatusUnauthorized {
				e.rounds(t, 1)
			}
			e.makeDue(t)
			var waiting int64
			e.scalar(t, &waiting, `UPDATE deliveries SET next_attempt_at = now() + interval '2 hours'
				WHERE id = (SELECT max(id) FROM deliveries) RETURNING id`)
			if _, err := e.d.DeliverOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := e.delivery(t, waiting); got.wait < 3600 {
				t.Errorf("a delivery in its backoff moved: next in %.0f s", got.wait)
			}
			if n := e.count(t, `SELECT count(*) FROM deliveries WHERE id <> $1 AND status = $2
				AND next_attempt_at NOT BETWEEN now() + make_interval(secs => $3) AND now() + make_interval(secs => $4)`,
				waiting, tt.status, tt.minWait, tt.wait); n != 0 {
				t.Errorf("%d held deliveries outside %.0f to %.0f s", n, tt.minWait, tt.wait)
			}
			if n := e.count(t, `SELECT count(*) FROM deliveries WHERE id <> $1 AND status = $2`, waiting, tt.status); n == 0 {
				t.Error("nothing held")
			}
		})
	}
}

// 003-AC8: with the secret still wrong, a pause that ends lets one request through, then pauses again.
func TestTalkPauseProbe(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", wrongTalkSecret)
	e.events(t, 10, ch)
	e.rounds(t, 2)
	e.age(t, ch, 30*time.Minute+time.Second)
	e.rounds(t, 4)
	if n := e.requests("talk-room-rejected"); n != 2 {
		t.Errorf("%d requests over two pauses, want 2", n)
	}
}

// 003-AC8: over 3 simulated hours a bad channel makes one request per 30 minutes, so it takes 11 broken
// channels on one IP to pass the 10 failures in 30 minutes of Nextcloud 32 and later.
func TestTalkPauseThirtyMinuteLimit(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", wrongTalkSecret)
	e.events(t, 30, ch)
	var at []int
	for minute := 0; minute < 3*60; minute += 10 {
		before := e.requests("talk-room-rejected")
		e.rounds(t, 1)
		for range e.requests("talk-room-rejected") - before {
			at = append(at, minute)
		}
		if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = next_attempt_at - interval '10 minutes'
			WHERE channel_id = $1 AND status = 'dead'`, ch); err != nil {
			t.Fatal(err)
		}
	}
	if want := []int{0, 30, 60, 90, 120, 150}; fmt.Sprint(at) != fmt.Sprint(want) {
		t.Errorf("requests at minutes %v, want %v", at, want)
	}
}

// 003-AC8: "Send test" reaches a paused channel; it is how a user checks a fix.
func TestTalkPauseSendTest(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", wrongTalkSecret)
	e.events(t, 2, ch)
	e.rounds(t, 2)
	s, ctx := e.service(t)
	out, err := s.testChannel(ctx, &idPath{ID: ch})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.OK || !strings.HasSuffix(out.Body.Error, "HTTP 401") {
		t.Errorf("test result %+v", out.Body)
	}
	if n := e.requests("talk-room-rejected"); n != 2 {
		t.Errorf("%d requests, want the delivery and the test", n)
	}
}

// 003-AC8: a manual retry of the only refused delivery is direct, the way to check a fix on the Nextcloud side.
func TestTalkPauseManualRetry(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", wrongTalkSecret)
	e.events(t, 1, ch)
	e.rounds(t, 1)
	var id int64
	e.scalar(t, &id, `SELECT id FROM deliveries WHERE channel_id = $1 AND status = 'dead'`, ch)
	s, ctx := e.service(t)
	if _, err := s.retryDelivery(ctx, &deliveryPath{ID: id}); err != nil {
		t.Fatal(err)
	}
	e.rounds(t, 1)
	if n := e.requests("talk-room-rejected"); n != 2 {
		t.Errorf("%d requests, want the delivery and its retry", n)
	}
}

// 003-AC8: a crashed sender's expired lease does not block the channel.
func TestTalkExpiredLeaseIgnored(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", testupstream.TalkSecret)
	e.events(t, 2, ch)
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET claim_token = gen_random_uuid(), leased_until = now() - interval '1 second',
		next_attempt_at = now() + interval '1 hour' WHERE id = (SELECT min(id) FROM deliveries WHERE channel_id = $1)`, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.requests("talk-room"); n != 1 {
		t.Errorf("%d sent, want 1", n)
	}
}

// 003-AC8: a failed pause check holds the delivery: no request, no attempt spent, even on the last one.
func TestTalkHoldCheckFails(t *testing.T) {
	e := newTalkEnv(t)
	ch := e.channel(t, "room", testupstream.TalkSecret)
	e.events(t, 1, ch)
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET attempts = $1 WHERE channel_id = $2`, MaxAttempts-1, ch); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.d.claim(context.Background(), 1)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !e.d.holdTalk(ctx, zap.NewNop(), jobs[0]) {
		t.Fatal("not held")
	}
	if got := e.delivery(t, jobs[0].id); got.status != "pending" || got.attempts != MaxAttempts-1 || got.wait <= -5 {
		t.Errorf("%+v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE claim_token IS NOT NULL`); n != 0 {
		t.Errorf("%d claims left", n)
	}
}

// 006-AC8: a disabled owner's delivery to a Talk channel paused after a 401 ends dead with the reason, not held.
func TestTalkPauseDisabledOwner(t *testing.T) {
	e := newTalkEnv(t)
	bad := e.channel(t, "bad", wrongTalkSecret)
	e.events(t, 2, bad)
	e.rounds(t, 1)
	e.rounds(t, 1)
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'failed' AND last_error = $2`, bad, talkPausedError); n != 1 {
		t.Fatalf("%d held, want 1 behind the pause", n)
	}
	e.setInactive(t, e.alice, "disabled_at")
	e.rounds(t, 1)
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE channel_id = $1 AND status = 'dead' AND last_error = $2`, bad, errOwnerDisabled.Error()); n != 1 {
		t.Fatalf("%d dead for the disabled owner, want 1", n)
	}
	if n := e.requests("talk-bad-rejected"); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}
