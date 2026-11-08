package route

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/notify"
)

// 012-AC1, 012-AC13, 012-AC14: a digest rule needs 1 to 24 HH:MM times and a known zone, and priority normal;
// times are sorted and deduplicated, the zone defaults to UTC.
func TestRuleSchedule(t *testing.T) {
	many := make([]string, 25)
	for i := range many {
		many[i] = time.Date(0, 1, 1, i%24, i/24, 0, 0, time.UTC).Format("15:04")
	}
	for _, tt := range []struct {
		name               string
		delivery, priority string
		at                 []string
		tz                 string
		field              string // "" when valid
		want               *schedule
	}{
		{name: "digest", delivery: deliveryDigest, at: []string{"18:00", "09:00", "09:00"}, tz: "Europe/Paris",
			want: &schedule{At: []string{"09:00", "18:00"}, TZ: "Europe/Paris"}},
		{name: "default tz", delivery: deliveryDigest, at: []string{"07:30"}, want: &schedule{At: []string{"07:30"}, TZ: "UTC"}},
		{name: "immediate", delivery: deliveryImmediate},
		{name: "immediate high", delivery: deliveryImmediate, priority: priorityHigh},
		{name: "empty means immediate", delivery: ""},
		{name: "no at", delivery: deliveryDigest, field: "at"},
		{name: "25 times", delivery: deliveryDigest, at: many, field: "at"},
		{name: "not HH:MM", delivery: deliveryDigest, at: []string{"9:00"}, field: "at[0]"},
		{name: "24:00", delivery: deliveryDigest, at: []string{"09:00", "24:00"}, field: "at[1]"},
		{name: "unknown tz", delivery: deliveryDigest, at: []string{"09:00"}, tz: "Europe/Atlantis", field: "tz"},
		{name: "server tz", delivery: deliveryDigest, at: []string{"09:00"}, tz: "Local", field: "tz"},
		{name: "high priority", delivery: deliveryDigest, priority: priorityHigh, at: []string{"09:00"}, field: "delivery"},
		{name: "at without digest", delivery: deliveryImmediate, at: []string{"09:00"}, field: "at"},
		{name: "tz without digest", delivery: deliveryImmediate, tz: "UTC", field: "tz"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, errs := ruleSchedule(tt.delivery, tt.priority, tt.at, tt.tz)
			if tt.field == "" {
				if len(errs) > 0 || !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("%+v, %v; want %+v", got, errs, tt.want)
				}
				return
			}
			if !hasField(errs, "body."+tt.field) {
				t.Fatalf("errors %v, want one on body.%s", errs, tt.field)
			}
		})
	}
}

// 012-AC13: a channel config still sending delivery, at or tz is refused, on every type.
func TestChannelRefusesDelivery(t *testing.T) {
	s := &Service{}
	for _, tt := range []struct {
		typ   string
		cfg   map[string]any
		field string
	}{
		{notify.TypeTelegram, map[string]any{"chat_id": "-100123", "delivery": "digest", "at": []any{"09:00"}}, "delivery"},
		{notify.TypeEmail, map[string]any{"to": []any{"a@x.test"}, "delivery": "immediate"}, "delivery"},
		{notify.TypeWebPush, map[string]any{"devices": "all", "at": []any{"09:00"}}, "at"},
		{notify.TypeWebhook, map[string]any{"url": "https://hooks.example.com/x", "tz": "UTC"}, "tz"},
	} {
		t.Run(tt.typ, func(t *testing.T) {
			if _, errs := s.validateConfig(tt.typ, tt.cfg); !hasField(errs, "body.config."+tt.field) {
				t.Fatalf("errors %v, want one on body.config.%s", errs, tt.field)
			}
		})
	}
}

func hasField(errs []error, loc string) bool {
	for _, err := range errs {
		var d *huma.ErrorDetail
		if errors.As(err, &d) && d.Location == loc {
			return true
		}
	}
	return false
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

// tgChannel is a Telegram channel: a digest rule makes its deliveries wait (012-AC15).
func (e *env) tgChannel(t *testing.T, owner string) string {
	t.Helper()
	var id string
	e.scalar(t, &id, `INSERT INTO channels (owner_id, name, type, config) VALUES ($1, gen_random_uuid()::text, 'telegram', '{"chat_id": "-100123"}') RETURNING id::text`, owner)
	return id
}

// digestRule is e.rule with a 07:00 UTC digest.
func (e *env) digestRule(t *testing.T, owner string, watch *string, filter string, channels ...string) string {
	t.Helper()
	id := e.rule(t, owner, watch, filter, channels...)
	e.setSchedule(t, id, "07:00", "UTC")
	return id
}

// setSchedule writes a rule's digest schedule behind the API, which reschedules nothing.
func (e *env) setSchedule(t *testing.T, rule, at, tz string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE rules SET delivery = 'digest', digest_at = ARRAY[$2], digest_tz = $3 WHERE id = $1`, rule, at, tz); err != nil {
		t.Fatal(err)
	}
}

type queued struct {
	due    time.Time
	digest bool
	rule   string
}

func (e *env) queued(t *testing.T, event int64, channel string) queued {
	t.Helper()
	var q queued
	if err := e.pool.QueryRow(context.Background(), `SELECT next_attempt_at, digest, COALESCE(rule_id::text, '') FROM deliveries WHERE event_id = $1 AND channel_id = $2`,
		event, channel).Scan(&q.due, &q.digest, &q.rule); err != nil {
		t.Fatal(err)
	}
	return q
}

func (q queued) now() bool { return !q.digest && time.Until(q.due) <= 0 }

// 012-AC2, 012-AC15: a digest rule's delivery to a Telegram channel waits for the rule's next scheduled time; to a
// webhook or script channel it goes at once; an immediate rule is unchanged.
func TestRoutingDigest(t *testing.T) {
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	tg, hook, other := e.tgChannel(t, alice), e.webhook(t, alice, "https://hooks.example.com/x"), e.tgChannel(t, alice)
	script := e.scriptChannel(t, alice, "function main() {}")
	rule := e.rule(t, alice, &w, "", tg, hook, script)
	e.setSchedule(t, rule, "07:00", "Europe/Paris")
	w2 := e.watch(t, alice)
	e.rule(t, alice, &w2, "", other)
	ev, ev2 := e.event(t, w, "release.published", "v1"), e.event(t, w2, "release.published", "v1")
	routeAll(t, e.router())
	paris, _ := time.LoadLocation("Europe/Paris")
	q := e.queued(t, ev, tg)
	if local := q.due.In(paris); !q.digest || local.Hour() != 7 || local.Minute() != 0 || time.Until(q.due) <= 0 || time.Until(q.due) > 25*time.Hour {
		t.Errorf("digest delivery due %s (Paris), digest %v", q.due.In(paris), q.digest)
	}
	for name, c := range map[string]string{"webhook": hook, "script": script} {
		if q := e.queued(t, ev, c); !q.now() {
			t.Errorf("%s under a digest rule: due %s, digest %v; want at once", name, q.due, q.digest)
		}
	}
	if q := e.queued(t, ev2, other); !q.now() {
		t.Errorf("immediate rule: due %s, digest %v", q.due, q.digest)
	}
}

// 012-AC6, 012-AC16: an event matched by a rule sending at once (immediate or high priority) and a digest rule on
// one channel makes one delivery, due now, whichever rule runs first; two digest rules keep the first one's.
func TestRoutingDigestMerge(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name        string
		first, then string // "digest", "immediate", "high"
	}{
		{"immediate then digest", "immediate", "digest"},
		{"digest then immediate", "digest", "immediate"},
		{"high then digest", "high", "digest"},
		{"digest then high", "digest", "high"},
		{"digest then digest", "digest", "digest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.user(t, "alice")
			w := e.watch(t, alice)
			ch := e.tgChannel(t, alice)
			var ids []string
			// created_at orders the rules the router runs.
			for i, kind := range []string{tt.first, tt.then} {
				id := e.rule(t, alice, &w, "", ch)
				if _, err := e.pool.Exec(ctx, `UPDATE rules SET created_at = now() - make_interval(mins => $2) WHERE id = $1`, id, 10-i); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "digest":
					e.setSchedule(t, id, "07:00", "UTC")
				case "high":
					e.setPriority(t, id, priorityHigh)
				}
				ids = append(ids, id)
			}
			ev := e.event(t, w, "release.published", "v1")
			routeAll(t, e.router())
			if n := e.count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1`, ev); n != 1 {
				t.Fatalf("%d deliveries, want 1", n)
			}
			q := e.queued(t, ev, ch)
			if tt.first == "digest" && tt.then == "digest" {
				if !q.digest || q.rule != ids[0] {
					t.Errorf("due %s, digest %v, rule %s; want the first rule's digest", q.due, q.digest, q.rule)
				}
				return
			}
			want := ids[0]
			if tt.first == "digest" {
				want = ids[1]
			}
			if !q.now() || q.rule != want {
				t.Errorf("due %s, digest %v, rule %s; want at once by rule %s", q.due, q.digest, q.rule, want)
			}
		})
	}
}

// 012-AC19: two digest rules with different schedules on one channel keep the earlier due time and its rule,
// whichever rule runs first.
func TestRoutingDigestEarliest(t *testing.T) {
	ctx := context.Background()
	hm := func(h int) string { return time.Date(0, 1, 1, h%24, 0, 0, 0, time.UTC).Format("15:04") }
	now := time.Now().UTC()
	soon, later := hm(now.Hour()+1), hm(now.Hour()+2)
	for _, first := range []string{"soon", "later"} {
		t.Run(first+" first", func(t *testing.T) {
			e := newEnv(t)
			alice := e.user(t, "alice")
			w := e.watch(t, alice)
			ch := e.tgChannel(t, alice)
			ids := map[string]string{}
			for i, name := range map[string][]string{"soon": {"soon", "later"}, "later": {"later", "soon"}}[first] {
				id := e.rule(t, alice, &w, "", ch)
				if _, err := e.pool.Exec(ctx, `UPDATE rules SET created_at = now() - make_interval(mins => $2) WHERE id = $1`, id, 10-i); err != nil {
					t.Fatal(err)
				}
				e.setSchedule(t, id, map[string]string{"soon": soon, "later": later}[name], "UTC")
				ids[name] = id
			}
			ev := e.event(t, w, "release.published", "v1")
			routeAll(t, e.router())
			q := e.queued(t, ev, ch)
			if want := nextAt([]string{soon}, time.UTC, now); !q.digest || q.rule != ids["soon"] || q.due.Sub(want).Abs() > time.Minute {
				t.Errorf("due %s, digest %v, rule %s; want %s by rule %s", q.due.UTC(), q.digest, q.rule, want, ids["soon"])
			}
		})
	}
}

// 012-AC17: two digest rules on one channel make one message at the scheduled time.
func TestDigestAcrossRules(t *testing.T) {
	e := newDigestEnv(t)
	ch := e.tgChannel(t, e.alice)
	a, b := e.namedWatch(t, "org/alpha"), e.namedWatch(t, "org/beta")
	e.digestRule(t, e.alice, &a, "", ch)
	e.digestRule(t, e.alice, &b, "", ch)
	e.event(t, a, "release.published", "v1")
	e.event(t, b, "release.published", "v2")
	routeAll(t, e.router())
	e.round(t, e.deliverer(t, e.pool))
	if got := e.messages(); len(got) != 1 || !strings.Contains(got[0].Text, "2 unread events") ||
		!strings.Contains(got[0].Text, "org/alpha") || !strings.Contains(got[0].Text, "org/beta") {
		t.Fatalf("messages %+v, want one digest of both rules' events", got)
	}
}

// 012-AC2: a stored digest schedule that validation would refuse does not stall routing: the delivery goes out now.
func TestRoutingDigestBadSchedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	for name, set := range map[string]string{
		"no at":      `digest_at = '{}'`,
		"null at":    `digest_at = NULL`,
		"unknown tz": `digest_tz = 'Europe/Atlantis'`,
	} {
		t.Run(name, func(t *testing.T) {
			w := e.watch(t, alice)
			ch := e.tgChannel(t, alice)
			r := e.digestRule(t, alice, &w, "", ch)
			if _, err := e.pool.Exec(ctx, `UPDATE rules SET `+set+` WHERE id = $1`, r); err != nil {
				t.Fatal(err)
			}
			ev := e.event(t, w, "release.published", "v1")
			if _, err := e.router().RouteOnce(ctx); err != nil {
				t.Fatalf("route: %v", err)
			}
			if q := e.queued(t, ev, ch); !q.now() {
				t.Errorf("delivery due %s, digest %v; want due now, out of the digest", q.due, q.digest)
			}
		})
	}
}

// 012-AC18: routing waits out a rule edit in flight and reads the schedule it commits: switching to immediate
// queues the delivery due now, out of the digest.
func TestRoutingDigestEditInFlight(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch := e.tgChannel(t, alice)
	r := e.digestRule(t, alice, &w, "", ch)
	ev := e.event(t, w, "release.published", "v1")

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// As updateRule does: FOR UPDATE, then the write.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, r); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE rules SET delivery = 'immediate', digest_at = NULL, digest_tz = NULL WHERE id = $1`, r); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.router().RouteOnce(ctx); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the router never waited on the rule lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if q := e.queued(t, ev, ch); !q.now() {
		t.Errorf("delivery due %s, digest %v; want due now, out of the digest", q.due.UTC(), q.digest)
	}
}

// 012-AC9, 012-AC18: a rule schedule edit moves only that rule's pending, unleased digest deliveries; switching
// it to immediate takes every waiting one out of the digest, due now unless it is leased or failed (its backoff
// stays). Another rule's deliveries on the same channel stay as they are.
func TestDigestReschedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	w, w2 := e.watch(t, alice), e.watch(t, alice)
	dig := e.tgChannel(t, alice)
	rule := e.digestRule(t, alice, &w, "", dig)
	e.setSchedule(t, rule, "07:00", "Europe/Paris")
	e.digestRule(t, alice, &w2, "", dig)
	waiting, leased, failed := e.event(t, w, "release.published", "v1"), e.event(t, w, "release.published", "v2"), e.event(t, w, "release.published", "v3")
	other := e.event(t, w2, "release.published", "v4")
	routeAll(t, e.router())
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET leased_until = now() + interval '1 minute' WHERE event_id = $1`, leased); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET status = 'failed', attempts = 1, next_attempt_at = now() + interval '3 hours' WHERE event_id = $1`, failed); err != nil {
		t.Fatal(err)
	}
	beforeLeased, beforeFailed, beforeOther := e.queued(t, leased, dig), e.queued(t, failed, dig), e.queued(t, other, dig)
	s := New(e.pool, zap.NewNop(), e.box, nil, nil, nil, nil, e.m)
	s.viewer = func(context.Context) (string, bool) { return alice, true }
	update := func(delivery string, at []string, tz *string) {
		t.Helper()
		in := &updateRuleInput{ID: rule}
		in.Body.Delivery, in.Body.At, in.Body.TZ = &delivery, at, tz
		if _, err := s.updateRule(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(step string, want queued, event int64, digest bool) {
		t.Helper()
		if q := e.queued(t, event, dig); !q.due.Equal(want.due) || q.digest != digest {
			t.Errorf("%s: event %d due %s, digest %v; want due %s, digest %v", step, event, q.due, q.digest, want.due, digest)
		}
	}

	utc := "UTC"
	update(deliveryDigest, []string{"21:15"}, &utc)
	if q := e.queued(t, waiting, dig); !q.digest || q.due.UTC().Hour() != 21 || q.due.UTC().Minute() != 15 || time.Until(q.due) <= 0 {
		t.Errorf("rescheduled delivery due %s, digest %v", q.due.UTC(), q.digest)
	}
	unchanged("schedule edit", beforeLeased, leased, true)
	unchanged("schedule edit", beforeFailed, failed, true)
	unchanged("schedule edit", beforeOther, other, true)

	update(deliveryImmediate, nil, nil)
	if q := e.queued(t, waiting, dig); !q.now() {
		t.Errorf("after immediate: due %s, digest %v", q.due, q.digest)
	}
	unchanged("after immediate", beforeLeased, leased, false)
	unchanged("after immediate", beforeFailed, failed, false)
	unchanged("after immediate", beforeOther, other, true)
}

// 012-AC18: a rule read as a digest by the rule query but disabled by the time its schedule is locked queues
// nothing for the digest: the event goes at once, after the disable's flush.
func TestRoutingDigestDisabledInFlight(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch := e.tgChannel(t, alice)
	rule := e.digestRule(t, alice, &w, "", ch)
	ev := e.event(t, w, "release.published", "v1")
	if _, err := e.pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, rule); err != nil {
		t.Fatal(err)
	}
	// What routeEvent read before the disable committed.
	hits := []matchRule{{id: rule, owner: alice, priority: priorityNormal, delivery: deliveryDigest, channels: []string{ch}}}
	r := e.router()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if due, _, err := r.digestDue(ctx, tx, hits); err != nil || len(due) != 0 {
		t.Fatalf("due %v, %v; want none for a disabled rule", due, err)
	}
	if err := r.insertDeliveries(ctx, tx, ev, hits); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if q := e.queued(t, ev, ch); !q.now() {
		t.Errorf("due %s, digest %v; want at once", q.due, q.digest)
	}
}

// 012-AC18: deleting or disabling a digest rule sends what it holds at once; another digest rule's deliveries wait.
func TestDigestRuleGone(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"delete", "disable"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			alice := e.user(t, "alice")
			w, w2 := e.watch(t, alice), e.watch(t, alice)
			ch := e.tgChannel(t, alice)
			rule := e.digestRule(t, alice, &w, "", ch)
			e.digestRule(t, alice, &w2, "", ch)
			held, other := e.event(t, w, "release.published", "v1"), e.event(t, w2, "release.published", "v2")
			routeAll(t, e.router())
			s := New(e.pool, zap.NewNop(), e.box, nil, nil, nil, nil, e.m)
			s.viewer = func(context.Context) (string, bool) { return alice, true }
			var err error
			if how == "delete" {
				_, err = s.deleteRule(ctx, &idPath{ID: rule})
			} else {
				in := &updateRuleInput{ID: rule}
				off := false
				in.Body.Enabled = &off
				_, err = s.updateRule(ctx, in)
			}
			if err != nil {
				t.Fatal(err)
			}
			if q := e.queued(t, held, ch); !q.now() {
				t.Errorf("held delivery due %s, digest %v; want at once", q.due, q.digest)
			}
			if q := e.queued(t, other, ch); !q.digest || time.Until(q.due) <= 0 {
				t.Errorf("other rule's delivery due %s, digest %v; want still waiting", q.due, q.digest)
			}
		})
	}
}

// 012-AC18: a disable that also edits the times sends what the rule holds at once and stores the times; once
// enabled again, the next event waits for them.
func TestDigestRuleDisableWithEdit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	alice := e.user(t, "alice")
	w := e.watch(t, alice)
	ch := e.tgChannel(t, alice)
	rule := e.digestRule(t, alice, &w, "", ch)
	held := e.event(t, w, "release.published", "v1")
	routeAll(t, e.router())
	s := New(e.pool, zap.NewNop(), e.box, nil, nil, nil, nil, e.m)
	s.viewer = func(context.Context) (string, bool) { return alice, true }
	patch := func(enabled bool, at []string) Rule {
		t.Helper()
		in := &updateRuleInput{ID: rule}
		// The watch is sent again, as the rule form does.
		in.Body.Enabled, in.Body.At, in.Body.WatchID = &enabled, at, &w
		out, err := s.updateRule(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return out.Body
	}
	if r := patch(false, []string{"21:15"}); r.Enabled || r.Delivery != deliveryDigest || !slices.Equal(r.At, []string{"21:15"}) {
		t.Errorf("after disable with an edit: %+v", r)
	}
	if q := e.queued(t, held, ch); !q.now() {
		t.Errorf("held delivery due %s, digest %v; want at once", q.due, q.digest)
	}
	patch(true, nil)
	next := e.event(t, w, "release.published", "v2")
	routeAll(t, e.router())
	if q := e.queued(t, next, ch); !q.digest || q.due.UTC().Hour() != 21 || q.due.UTC().Minute() != 15 || time.Until(q.due) <= 0 {
		t.Errorf("after enable: due %s, digest %v; want the next 21:15 UTC", q.due.UTC(), q.digest)
	}
}
