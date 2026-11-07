package route

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/notify"
)

// 012-AC1: a digest needs 1 to 24 HH:MM times and a known zone, and only on types that can list events.
func TestDigestConfig(t *testing.T) {
	s := &Service{}
	tg := func(extra map[string]any) map[string]any {
		c := map[string]any{"chat_id": "-100123"}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	many := make([]any, 25)
	for i := range many {
		many[i] = time.Date(0, 1, 1, i%24, i/24, 0, 0, time.UTC).Format("15:04")
	}
	for _, tt := range []struct {
		name  string
		typ   string
		cfg   map[string]any
		field string // "" when valid
		want  map[string]any
	}{
		{name: "digest", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{"18:00", "09:00", "09:00"}, "tz": "Europe/Paris"}),
			want: map[string]any{"delivery": "digest", "at": []any{"09:00", "18:00"}, "tz": "Europe/Paris"}},
		{name: "default tz", typ: notify.TypeEmail, cfg: map[string]any{"to": []any{"a@x.test"}, "delivery": "digest", "at": []any{"07:30"}},
			want: map[string]any{"tz": "UTC"}},
		{name: "web push", typ: notify.TypeWebPush, cfg: map[string]any{"devices": "all", "delivery": "digest", "at": []any{"00:00", "23:59"}}},
		{name: "immediate", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "immediate"})},
		{name: "no at", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest"}), field: "at"},
		{name: "empty at", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{}}), field: "at"},
		{name: "25 times", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": many}), field: "at"},
		{name: "not HH:MM", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{"9:00"}}), field: "at[0]"},
		{name: "24:00", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{"09:00", "24:00"}}), field: "at[1]"},
		{name: "not a string", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{900}}), field: "at[0]"},
		{name: "unknown tz", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{"09:00"}, "tz": "Europe/Atlantis"}), field: "tz"},
		{name: "server tz", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "digest", "at": []any{"09:00"}, "tz": "Local"}), field: "tz"},
		{name: "unknown mode", typ: notify.TypeTelegram, cfg: tg(map[string]any{"delivery": "weekly"}), field: "delivery"},
		{name: "at without digest", typ: notify.TypeTelegram, cfg: tg(map[string]any{"at": []any{"09:00"}}), field: "at"},
		{name: "webhook", typ: notify.TypeWebhook, cfg: map[string]any{"url": "https://hooks.example.com/x", "delivery": "digest", "at": []any{"09:00"}}, field: "delivery"},
		{name: "script", typ: notify.TypeScript, cfg: map[string]any{"script_id": "6f1c0f5e-7d43-4a39-9b0e-2b8f5d8e0c11", "delivery": "digest", "at": []any{"09:00"}}, field: "delivery"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, errs := s.validateConfig(tt.typ, tt.cfg)
			if tt.field == "" {
				if len(errs) > 0 {
					t.Fatalf("refused: %v", errs)
				}
				for k, v := range tt.want {
					if !jsonEqual(out[k], v) {
						t.Errorf("%s = %v, want %v", k, out[k], v)
					}
				}
				return
			}
			var hit bool
			for _, err := range errs {
				var d *huma.ErrorDetail
				if errors.As(err, &d) && d.Location == "body.config."+tt.field {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("errors %v, want one on body.config.%s", errs, tt.field)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	if l, ok := a.([]string); ok {
		r, _ := b.([]any)
		return slices.EqualFunc(l, r, func(s string, x any) bool { return s == x })
	}
	return a == b
}

// 012-AC2: the next scheduled instant keeps the local wall time across both Europe/Paris DST changes.
func TestNextAt(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	twice := []string{"09:00", "18:00"}
	for _, tt := range []struct {
		name string
		at   []string
		now  string
		want string
	}{
		{"same day", twice, "2026-06-10T08:12:00Z", "2026-06-10T16:00:00Z"},                        // 10:12 CEST -> 18:00 CEST
		{"next day", twice, "2026-06-10T16:30:00Z", "2026-06-11T07:00:00Z"},                        // 18:30 CEST -> 09:00 CEST
		{"exactly at a time", twice, "2026-06-10T07:00:00Z", "2026-06-10T16:00:00Z"},               // strictly after
		{"over spring forward", twice, "2026-03-28T17:30:00Z", "2026-03-29T07:00:00Z"},             // 18:30 CET -> 09:00 CEST
		{"over fall back", twice, "2026-10-24T16:30:00Z", "2026-10-25T08:00:00Z"},                  // 18:30 CEST -> 09:00 CET
		{"in the gap", []string{"02:30"}, "2026-03-28T23:00:00Z", "2026-03-29T01:00:00Z"},          // 02:30 missing -> 03:00 CEST
		{"after the gap", []string{"02:30"}, "2026-03-29T01:00:00Z", "2026-03-30T00:30:00Z"},       // next day 02:30 CEST
		{"repeated, first", []string{"02:30"}, "2026-10-24T22:00:00Z", "2026-10-25T00:30:00Z"},     // 02:30 CEST
		{"repeated, not twice", []string{"02:30"}, "2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z"}, // skips 02:30 CET
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextAt(tt.at, paris, utc(tt.now)); !got.Equal(utc(tt.want)) {
				t.Errorf("next %s, want %s", got.UTC().Format(time.RFC3339), tt.want)
			}
		})
	}
	if got := nextAt([]string{"07:00"}, time.UTC, utc("2026-06-10T08:00:00Z")); !got.Equal(utc("2026-06-11T07:00:00Z")) {
		t.Errorf("UTC: %s", got)
	}
}

// 012-AC2: a stored schedule validation would refuse is an error, never a panic or a zero time, so the router falls back.
func TestScheduleNext(t *testing.T) {
	now := time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		s    schedule
	}{
		{"no at", schedule{TZ: "UTC"}},
		{"one digit", schedule{At: []string{"9"}, TZ: "UTC"}},
		{"hour 25", schedule{At: []string{"09:00", "25:00"}, TZ: "UTC"}},
		{"one digit minute", schedule{At: []string{"09:7"}, TZ: "UTC"}},
		{"unknown tz", schedule{At: []string{"09:00"}, TZ: "Europe/Atlantis"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := tt.s.next(now, time.LoadLocation); err == nil {
				t.Errorf("next = %s, want an error", got)
			}
		})
	}
	if got, err := (schedule{At: []string{"09:00"}}).next(now, time.LoadLocation); err != nil || !got.Equal(now.Add(time.Hour)) {
		t.Errorf("no tz: %s, %v; want UTC 09:00", got, err)
	}
}

func (e *env) digestChannel(t *testing.T, owner, at, tz string) string {
	t.Helper()
	var id string
	e.scalar(t, &id, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, gen_random_uuid()::text, 'telegram',
		jsonb_build_object('chat_id', '-100123', 'delivery', 'digest', 'at', jsonb_build_array($2::text), 'tz', $3::text)) RETURNING id::text`, owner, at, tz)
	return id
}

type queued struct {
	due    time.Time
	digest bool
	rule   string
}

func (e *env) queued(t *testing.T, event int64, channel string) queued {
	t.Helper()
	var q queued
	if err := e.pool.QueryRow(context.Background(), `SELECT next_attempt_at, digest, rule_id::text FROM deliveries WHERE event_id = $1 AND channel_id = $2`,
		event, channel).Scan(&q.due, &q.digest, &q.rule); err != nil {
		t.Fatal(err)
	}
	return q
}

// 012-AC2, 012-AC6: a digest channel's delivery waits for the next scheduled time; one taken by a high
// priority rule, even after a normal one, is due now and outside the digest; an immediate channel is unchanged.
func TestRoutingDigest(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	dig := e.digestChannel(t, alice, "07:00", "Europe/Paris")
	hook := e.webhook(t, alice, "https://hooks.example.com/x")
	normal := e.rule(t, alice, &w, `event.type == "release.published"`, dig, hook)
	ev := e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	paris, _ := time.LoadLocation("Europe/Paris")
	q := e.queued(t, ev, dig)
	if local := q.due.In(paris); !q.digest || local.Hour() != 7 || local.Minute() != 0 || time.Until(q.due) <= 0 || time.Until(q.due) > 25*time.Hour {
		t.Errorf("digest delivery due %s (Paris), digest %v", q.due.In(paris), q.digest)
	}
	if q := e.queued(t, ev, hook); q.digest || time.Until(q.due) > 0 {
		t.Errorf("immediate delivery due %s, digest %v", q.due, q.digest)
	}

	high := e.ruleAt(t, alice, &w, `event.type == "tag.created"`, priorityHigh)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, high, dig); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE rules SET filter = '' WHERE id = $1`, normal); err != nil {
		t.Fatal(err)
	}
	ev = e.event(t, w, "tag.created", "v2")
	routeAll(t, e.router())
	if q := e.queued(t, ev, dig); q.digest || time.Until(q.due) > 0 || q.rule != high {
		t.Errorf("high priority delivery due %s, digest %v, rule %s", q.due, q.digest, q.rule)
	}

	// 012-AC6: a second normal rule leaves the first one's waiting delivery as it is.
	w2 := e.watch(t, alice)
	first := e.rule(t, alice, &w2, "", dig)
	e.rule(t, alice, &w2, "", dig)
	ev = e.event(t, w2, "release.published", "v3")
	routeAll(t, e.router())
	if q := e.queued(t, ev, dig); !q.digest || time.Until(q.due) <= 0 || q.rule != first {
		t.Errorf("two normal rules: due %s, digest %v, rule %s, want rule %s", q.due, q.digest, q.rule, first)
	}

	// 012-AC6: a normal rule after a high one neither puts the delivery back in the digest nor takes an immediate one.
	w3 := e.watch(t, alice)
	high = e.ruleAt(t, alice, &w3, "", priorityHigh)
	for _, c := range []string{dig, hook} {
		if _, err := e.pool.Exec(context.Background(), `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, high, c); err != nil {
			t.Fatal(err)
		}
	}
	e.rule(t, alice, &w3, "", dig, hook)
	ev = e.event(t, w3, "release.published", "v4")
	routeAll(t, e.router())
	for _, c := range []string{dig, hook} {
		if q := e.queued(t, ev, c); q.digest || time.Until(q.due) > 0 || q.rule != high {
			t.Errorf("high then normal on %s: due %s, digest %v, rule %s, want rule %s", c, q.due, q.digest, q.rule, high)
		}
	}
}

// 012-AC2: a stored digest schedule that validation would refuse does not stall routing: the delivery goes out now.
func TestRoutingDigestBadSchedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	for name, cfg := range map[string]string{
		"no at":      `{"chat_id": "-100123", "delivery": "digest", "at": [], "tz": "UTC"}`,
		"unknown tz": `{"chat_id": "-100123", "delivery": "digest", "at": ["07:00"], "tz": "Europe/Atlantis"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := e.watch(t, alice)
			ch := e.digestChannel(t, alice, "07:00", "UTC")
			if _, err := e.pool.Exec(ctx, `UPDATE channels SET config = $2::jsonb WHERE id = $1`, ch, cfg); err != nil {
				t.Fatal(err)
			}
			e.rule(t, alice, &w, "", ch)
			ev := e.event(t, w, "release.published", "v1")
			if _, err := e.router().RouteOnce(ctx); err != nil {
				t.Fatalf("route: %v", err)
			}
			if q := e.queued(t, ev, ch); q.digest || time.Until(q.due) > 0 {
				t.Errorf("delivery due %s, digest %v; want due now, out of the digest", q.due, q.digest)
			}
		})
	}
}

// 012-AC2: routing waits out a channel edit in flight that turns it into a digest, and queues on the committed schedule.
func TestRoutingDigestEditInFlight(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch := e.digestChannel(t, alice, "07:00", "UTC")
	if _, err := e.pool.Exec(ctx, `UPDATE channels SET config = config - 'delivery' - 'at' - 'tz' WHERE id = $1`, ch); err != nil {
		t.Fatal(err)
	}
	e.rule(t, alice, &w, "", ch)
	ev := e.event(t, w, "release.published", "v1")

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// As updateChannel does: FOR UPDATE, then the config write.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE channels SET config = config || '{"delivery": "digest", "at": ["07:00"], "tz": "UTC"}' WHERE id = $1`, ch); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.router().RouteOnce(ctx); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the router never waited on the channel lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if q := e.queued(t, ev, ch); !q.digest || q.due.UTC().Hour() != 7 || time.Until(q.due) <= 0 {
		t.Errorf("delivery due %s, digest %v; want the committed 07:00 UTC digest", q.due.UTC(), q.digest)
	}
}

// 012-AC9: a schedule edit moves only the pending, unleased digest deliveries; switching to immediate takes
// every waiting one out of the digest, due now unless it is leased or failed (its backoff stays).
func TestDigestReschedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	dig := e.digestChannel(t, alice, "07:00", "Europe/Paris")
	e.rule(t, alice, &w, "", dig)
	waiting, leased, failed := e.event(t, w, "release.published", "v1"), e.event(t, w, "release.published", "v2"), e.event(t, w, "release.published", "v3")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET leased_until = now() + interval '1 minute' WHERE event_id = $1`, leased); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET status = 'failed', attempts = 1, next_attempt_at = now() + interval '3 hours' WHERE event_id = $1`, failed); err != nil {
		t.Fatal(err)
	}
	beforeLeased, beforeFailed := e.queued(t, leased, dig), e.queued(t, failed, dig)
	s := New(e.pool, zap.NewNop(), e.box, nil, nil, nil, nil, e.m)
	s.viewer = func(context.Context) (string, bool) { return alice, true }
	update := func(cfg map[string]any) {
		t.Helper()
		in := &updateChannelInput{ID: dig}
		in.Body.Config = cfg
		if _, err := s.updateChannel(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	update(map[string]any{"chat_id": "-100123", "delivery": "digest", "at": []any{"21:15"}, "tz": "UTC"})
	if q := e.queued(t, waiting, dig); !q.digest || q.due.UTC().Hour() != 21 || q.due.UTC().Minute() != 15 || time.Until(q.due) <= 0 {
		t.Errorf("rescheduled delivery due %s, digest %v", q.due.UTC(), q.digest)
	}
	for name, x := range map[string]struct {
		event int64
		was   queued
	}{"leased": {leased, beforeLeased}, "failed": {failed, beforeFailed}} {
		if q := e.queued(t, x.event, dig); !q.due.Equal(x.was.due) || !q.digest {
			t.Errorf("schedule edit: %s delivery due %s, digest %v", name, q.due, q.digest)
		}
	}

	update(map[string]any{"chat_id": "-100123"})
	if q := e.queued(t, waiting, dig); q.digest || time.Until(q.due) > 0 {
		t.Errorf("after immediate: due %s, digest %v", q.due, q.digest)
	}
	for name, x := range map[string]struct {
		event int64
		was   queued
	}{"leased": {leased, beforeLeased}, "failed": {failed, beforeFailed}} {
		if q := e.queued(t, x.event, dig); !q.due.Equal(x.was.due) || q.digest {
			t.Errorf("after immediate: %s delivery due %s, digest %v; want due %s, out of the digest", name, q.due, q.digest, x.was.due)
		}
	}
}
