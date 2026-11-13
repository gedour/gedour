package route

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/watch"
)

const (
	deliveryImmediate = "immediate"
	deliveryDigest    = "digest"
	maxDigestTimes    = 24
)

var digestTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// digestTypes are the channel types whose message can list several events; a digest rule's other channels get
// its events at once (012-AC15).
var digestTypes = []string{notify.TypeTelegram, notify.TypeNextcloudTalk, notify.TypeEmail, notify.TypeWebPush}

// schedule is a digest rule's daily send times.
type schedule struct {
	At []string
	TZ string
}

// checkSchedule validates a rule's digest times and zone, normalized: sorted, deduplicated, UTC by default
// (012-AC13). Error locations are the rule body's.
func checkSchedule(at []string, tz string) (*schedule, []error) {
	var errs []error
	s := &schedule{TZ: "UTC", At: []string{}}
	for i, v := range at {
		if !digestTimeRe.MatchString(v) {
			errs = append(errs, &huma.ErrorDetail{Location: "body.at[" + strconv.Itoa(i) + "]", Message: "a local time as HH:MM, e.g. 09:00", Value: v})
			continue
		}
		if !slices.Contains(s.At, v) {
			s.At = append(s.At, v)
		}
	}
	slices.Sort(s.At)
	switch {
	case len(at) == 0:
		errs = append(errs, &huma.ErrorDetail{Location: "body.at", Message: "at least one time of day, e.g. [\"09:00\"]", Value: at})
	case len(s.At) > maxDigestTimes:
		errs = append(errs, &huma.ErrorDetail{Location: "body.at", Message: fmt.Sprintf("at most %d times", maxDigestTimes), Value: len(s.At)})
	}
	if tz != "" {
		// "Local" is the server's zone, not a place the user picked.
		if _, err := time.LoadLocation(tz); err != nil || tz == "Local" {
			errs = append(errs, &huma.ErrorDetail{Location: "body.tz", Message: "an IANA time zone, e.g. Europe/Paris", Value: tz})
		} else {
			s.TZ = tz
		}
	}
	return s, errs
}

// next is the first scheduled instant strictly after t, in the zone load returns for s.TZ. A stored schedule is
// checked again: one written outside checkSchedule must not panic the router nor queue for year 1.
func (s schedule) next(t time.Time, load func(string) (*time.Location, error)) (time.Time, error) {
	if len(s.At) == 0 {
		return time.Time{}, errors.New("digest schedule: no time of day")
	}
	for _, hm := range s.At {
		if !digestTimeRe.MatchString(hm) {
			return time.Time{}, fmt.Errorf("digest schedule: time %q is not HH:MM", hm)
		}
	}
	loc, err := load(s.TZ)
	if err != nil {
		return time.Time{}, fmt.Errorf("digest time zone: %w", err)
	}
	return nextAt(s.At, loc, t), nil
}

// nextAt is the first of the daily wall times at (HH:MM) in loc strictly after t. A time skipped by a
// spring-forward falls on the instant the clock jumps; one repeated by a fall-back takes its first occurrence.
func nextAt(at []string, loc *time.Location, t time.Time) time.Time {
	var best time.Time
	local := t.In(loc)
	for day := range 2 {
		for _, hm := range at {
			h, _ := strconv.Atoi(hm[:2])
			m, _ := strconv.Atoi(hm[3:])
			c := wallTime(local.Year(), local.Month(), local.Day()+day, h, m, loc)
			if c.After(t) && (best.IsZero() || c.Before(best)) {
				best = c
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return best
}

// wallTime is time.Date with the DST edge cases fixed, as Go leaves them unspecified.
func wallTime(y int, mo time.Month, d, h, m int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, h, m, 0, 0, loc)
	if t.Hour() != h || t.Minute() != m {
		// In a gap: the zone change bound next to t is the first instant after the missing wall time.
		start, end := t.ZoneBounds()
		if t.Sub(start) < end.Sub(t) {
			return start
		}
		return end
	}
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return t
	}
	// Repeated wall time: the earlier zone gives the same clock reading at an earlier instant.
	_, off := t.Zone()
	_, prev := start.Add(-time.Second).Zone()
	if alt := t.Add(time.Duration(off-prev) * time.Second); alt.Before(t) && alt.Hour() == h && alt.Minute() == m {
		return alt
	}
	return t
}

// reschedule moves the rule's pending, unleased digest deliveries to s's next time (012-AC9, 012-AC18). When s is nil
// it takes every waiting one out of the digest, due now unless a send holds it or a failure set its backoff.
func reschedule(ctx context.Context, tx pgx.Tx, rule string, s *schedule) error {
	if s == nil {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET digest = false, next_attempt_at = CASE
				WHEN status = 'pending' AND (leased_until IS NULL OR leased_until <= now()) THEN now() ELSE next_attempt_at END
			WHERE rule_id = $1 AND status IN ('pending', 'failed') AND digest`, rule); err != nil {
			return fmt.Errorf("reschedule digest: %w", err)
		}
		return nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return fmt.Errorf("reschedule digest: %w", err)
	}
	due, err := s.next(now, time.LoadLocation)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET next_attempt_at = $2
		WHERE rule_id = $1 AND status = 'pending' AND digest AND (leased_until IS NULL OR leased_until <= now())`, rule, due); err != nil {
		return fmt.Errorf("reschedule digest: %w", err)
	}
	return nil
}

// Reasons a waiting digest delivery ends skipped (012-AC4).
const (
	skippedRead     = "not sent: read in the inbox before the digest"
	skippedArchived = "not sent: archived in the inbox before the digest"
)

type digestRow struct {
	nd                               notify.Delivery
	readable, active, read, archived bool
	ruleID, ruleName, priority       *string
}

// loadDigest reads the batch's events, oldest first, and ends the deliveries that must not go out: skipped
// when the owner read or archived the item (no attempt spent), dead when the owner may no longer receive
// it (012-AC4, 012-AC7). A poll sent at once skips nothing on the inbox state (017-AC9). It returns the digest of
// the rest and their ids.
func (d *Deliverer) loadDigest(ctx context.Context, log *zap.Logger, typ string, j claimed, attempts int) (notify.Digest, []int64, error) {
	dg := notify.Digest{Poll: j.poll}
	rows, err := d.pool.Query(ctx, `SELECT x.id, e.id, e.type, e.occurred_at, e.observed_at, e.payload, w.id::text, w.name, w.kind,
			`+watch.ReadableSQL("w", "c.owner_id")+`, `+activeOwner+`, i.read_at IS NOT NULL, i.archived_at IS NOT NULL,
			r.id::text, r.name, r.priority
		FROM deliveries x JOIN events e ON e.id = x.event_id JOIN watches w ON w.id = e.watch_id
		JOIN channels c ON c.id = x.channel_id JOIN users u ON u.id = c.owner_id
		LEFT JOIN inbox_items i ON i.user_id = c.owner_id AND i.event_id = e.id LEFT JOIN rules r ON r.id = x.rule_id
		WHERE x.claim_token = $1 ORDER BY e.observed_at, e.id`, j.token)
	if err != nil {
		return dg, nil, fmt.Errorf("load digest: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (digestRow, error) {
		var x digestRow
		e := &x.nd.Event
		err := r.Scan(&x.nd.ID, &e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &x.nd.Watch.ID, &x.nd.Watch.Name, &x.nd.Watch.Kind,
			&x.readable, &x.active, &x.read, &x.archived, &x.ruleID, &x.ruleName, &x.priority)
		if err == nil && x.ruleID != nil {
			x.nd.Rule = &notify.Rule{ID: *x.ruleID, Name: *x.ruleName, Priority: *x.priority}
		}
		return x, err
	})
	if err != nil {
		return dg, nil, fmt.Errorf("load digest: %w", err)
	}
	var ids []int64
	dead := map[error][]int64{}
	skipped := map[string][]int64{}
	for _, x := range all {
		switch {
		case !x.readable:
			dead[errUnreadable] = append(dead[errUnreadable], x.nd.ID)
		case !x.active:
			dead[errOwnerDisabled] = append(dead[errOwnerDisabled], x.nd.ID)
		case j.poll:
			dg.Deliveries = append(dg.Deliveries, x.nd)
			ids = append(ids, x.nd.ID)
		case x.archived:
			skipped[skippedArchived] = append(skipped[skippedArchived], x.nd.ID)
		case x.read:
			skipped[skippedRead] = append(skipped[skippedRead], x.nd.ID)
		default:
			dg.Deliveries = append(dg.Deliveries, x.nd)
			ids = append(ids, x.nd.ID)
		}
	}
	// A failed write leaves its rows leased: the next claim after the lease checks them again.
	for cause, end := range dead {
		if _, err := d.finish(ctx, end, j.token, attempts, cause, nil); err != nil {
			log.Error("delivery_store_failed", zap.Error(err))
			continue
		}
		d.metrics.deliveries.WithLabelValues(typ, "dead").Add(float64(len(end)))
	}
	for reason, end := range skipped {
		if _, err := d.pool.Exec(ctx, `UPDATE deliveries SET status = 'skipped', last_error = $3, claim_token = NULL, leased_until = NULL
			WHERE id = ANY($1) AND claim_token = $2`, end, j.token, reason); err != nil {
			log.Error("delivery_store_failed", zap.Error(err))
			continue
		}
		d.metrics.deliveries.WithLabelValues(typ, "skipped").Add(float64(len(end)))
	}
	if len(ids) > 0 {
		dg.ID = slices.Min(ids)
	}
	return dg, ids, nil
}

// deliverDigest sends a claimed digest, or a poll's events, as one message; its deliveries share the outcome
// (012-AC3, 012-AC7, 017-AC8).
func (d *Deliverer) deliverDigest(stop context.Context, j claimed) {
	log := d.log.With(zap.String("channel_id", j.channelID), zap.Int("deliveries", len(j.batch)), zap.Bool("poll", j.poll))
	ctx, cancel := context.WithTimeout(stop, sendTimeout)
	defer cancel()
	start := time.Now()
	ids := make([]int64, len(j.batch))
	attempts := 0
	for i, c := range j.batch {
		ids[i], attempts = c.id, max(attempts, c.attempts)
	}
	ch, err := loadChannel(ctx, d.pool, d.box, j.channelID)
	var dg notify.Digest
	if err == nil {
		var left []int64
		// On a load error every delivery of the batch takes the failed attempt.
		if dg, left, err = d.loadDigest(ctx, log, ch.Type, j, attempts); err == nil {
			if ids = left; len(ids) == 0 {
				log.Debug("digest_empty")
				return
			}
		}
	}
	if err == nil && j.poll && len(ids) == 1 {
		// A poll cut to one event (the others gone since the claim) is a single message (017-AC3).
		for _, c := range j.batch {
			if c.id == ids[0] {
				d.deliver(stop, c)
				return
			}
		}
	}
	if err == nil && ch.Type == notify.TypeNextcloudTalk {
		// The lowest id leads: the batch's other rows share its lease and never count as an earlier claim.
		j.id = dg.ID
		if d.holdTalk(ctx, log, j) {
			return
		}
	}
	switch {
	case err == nil && ch.Type == notify.TypeWebPush:
		var rule *notify.Rule
		if j.poll {
			// One priority per poll group (claimSingles): a high group keeps its high urgency (005).
			rule = dg.Deliveries[0].Rule
		}
		_, err = pushFanOut(ctx, d.pool, d.box, d.sender, ch, ids, func(silent bool) ([]byte, string) {
			return notify.PushDigestPayload(dg, ch.ID, silent), notify.PushUrgency(silent, rule)
		})
	case err == nil:
		err = d.sender.SendDigest(ctx, ch, dg)
	}
	typ := ch.Type
	if typ == "" {
		typ = "unknown"
	}
	d.metrics.duration.WithLabelValues(typ).Observe(time.Since(start).Seconds())
	if drained(err, stop) {
		d.release(log, ids, j.token)
		return
	}
	done, ferr := d.finish(context.WithoutCancel(ctx), ids, j.token, attempts, err, nil)
	switch {
	case ferr != nil:
		log.Error("delivery_store_failed", zap.Error(ferr))
		return
	case len(done) == 0:
		log.Warn("delivery_discarded")
		return
	}
	for st, n := range done {
		d.metrics.deliveries.WithLabelValues(typ, st).Add(float64(n))
	}
	if done["failed"]+done["dead"] == 0 {
		log.Debug("digest_sent", zap.Int("events", done["sent"]))
		return
	}
	log.Info("digest_failed", zap.Int("failed", done["failed"]), zap.Int("dead", done["dead"]), zap.Int("attempt", attempts+1), zap.Error(err))
}
