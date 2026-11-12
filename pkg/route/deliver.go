package route

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/wake"
	"github.com/gedour/gedour/pkg/watch"
)

const (
	// MaxAttempts is the attempt after which a failing delivery is dead.
	MaxAttempts = 5
	// DeliveryLease holds a claimed delivery; it outlasts the slowest send (SMTP, 30 s) by far.
	DeliveryLease = 2 * time.Minute
	maxErrorLen   = 1024
	sendTimeout   = 45 * time.Second
	// talkPause holds a Talk channel after a 401. Nextcloud 32 and later answer 429 to every bot of the
	// sending IP past 10 failures in 30 min, and a success resets nothing: one failure per channel in any 30 min.
	talkPause = 30 * time.Minute
	// talkBusyDelay holds a delivery behind a Talk request in flight: one request per channel at a time.
	talkBusyDelay = time.Second
)

const talkPausedError = "held: channel paused after Nextcloud Talk answered HTTP 401; it resumes on its own or when the channel is edited"

// errUnreadable ends a delivery without sending: its channel's owner no longer owns the watch.
var errUnreadable = errors.New("not sent: the channel's owner no longer owns this watch")

// errOwnerDisabled ends a delivery queued before its channel's owner was disabled (006-AC8).
var errOwnerDisabled = errors.New("not sent: the channel's owner is disabled")

// Backoff is the wait after the n-th failed attempt (n from 1).
var Backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// retryDelay is the wait before attempt n+1 after attempt n failed.
func retryDelay(n int) time.Duration {
	return Backoff[min(max(n, 1), len(Backoff))-1]
}

// Deliverer sends due deliveries and records each outcome.
type Deliverer struct {
	pool        *pgxpool.Pool
	log         *zap.Logger
	box         *secret.Box
	sender      *notify.Sender
	runner      *script.Runner
	metrics     *Metrics
	loop        *wake.Loop
	concurrency int
	drain       time.Duration
}

// NewDeliverer builds a delivery loop paced by loop, sending up to concurrency deliveries at once (runner runs
// the script channels); on stop, sends in flight get drain to finish before they are cancelled.
func NewDeliverer(pool *pgxpool.Pool, log *zap.Logger, box *secret.Box, sender *notify.Sender, runner *script.Runner, m *Metrics, loop *wake.Loop, concurrency int, drain time.Duration) *Deliverer {
	return &Deliverer{pool: pool, log: log, box: box, sender: sender, runner: runner, metrics: m, loop: loop, concurrency: max(concurrency, 1), drain: drain}
}

type claimed struct {
	id, eventID int64
	token       string
	channelID   string
	ruleID      *string
	attempts    int
	// batch is a digest's or a poll's deliveries, all under token; nil for a delivery sent alone.
	batch []claimed
	// poll marks a batch of one poll's events sent at once (017), not a digest.
	poll bool
}

// Run delivers until ctx ends, claiming as slots free up and sleeping until the next delivery is due or a
// change wakes it. Then sends in flight get the drain time to finish; past it they are cancelled and their
// claims released, attempts unchanged.
func (d *Deliverer) Run(ctx context.Context) {
	hard, stopHard := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHard()
	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup
	for ctx.Err() == nil {
		var err error
		for ctx.Err() == nil {
			var n int
			if n, err = d.dispatch(ctx, hard, sem, &wg); err != nil || n == 0 {
				break
			}
		}
		var next time.Time
		if err == nil {
			next, err = d.deadline(ctx)
		}
		// No slot free: a due delivery cannot be claimed before a send ends, and its end kicks the loop.
		if len(sem) == cap(sem) {
			next = time.Time{}
		}
		if err != nil && ctx.Err() == nil {
			d.log.Error("delivery_claim_failed", zap.Error(err))
		}
		if !d.loop.Wait(ctx, next, err != nil) {
			break
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d.drain):
		d.log.Warn("delivery_drain_timeout", zap.Duration("timeout", d.drain))
		stopHard()
		<-done
	}
}

// dispatch claims as many due deliveries as there are free slots and starts them; it returns how many.
// A slow send holds one slot only.
func (d *Deliverer) dispatch(ctx, hard context.Context, sem chan struct{}, wg *sync.WaitGroup) (int, error) {
	free := cap(sem) - len(sem)
	if free == 0 {
		return 0, nil
	}
	jobs, err := d.claim(ctx, free)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem; d.loop.Kick() }()
			d.deliver(hard, j)
		})
	}
	return len(jobs), nil
}

// deadline returns when the next claimable delivery, single or digest, falls due (zero: none), read as an
// offset from the database clock the claims use.
func (d *Deliverer) deadline(ctx context.Context) (time.Time, error) {
	var in *float64
	if err := d.pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM min(next_attempt_at) - now())::float8 FROM deliveries
		WHERE status IN ('pending', 'failed') AND (leased_until IS NULL OR leased_until <= now())`).Scan(&in); err != nil {
		return time.Time{}, fmt.Errorf("next due delivery: %w", err)
	}
	if in == nil {
		return time.Time{}, nil
	}
	return time.Now().Add(time.Duration(*in * float64(time.Second))), nil
}

// DeliverOnce claims up to concurrency due deliveries, sends them and waits for them: one pass, for tests.
func (d *Deliverer) DeliverOnce(ctx context.Context) (int, error) {
	var wg sync.WaitGroup
	n, err := d.dispatch(ctx, ctx, make(chan struct{}, d.concurrency), &wg)
	wg.Wait()
	return n, err
}

// claim leases due deliveries on the database clock, as the watch scheduler does (001-AC15). A backlog of
// single deliveries never holds a due digest back: one slot goes to whichever kind waited longest, more keep
// one for digests.
func (d *Deliverer) claim(ctx context.Context, n int) ([]claimed, error) {
	if n == 1 {
		if jobs, err := d.claimDigests(ctx, 1, true); err != nil || len(jobs) > 0 {
			return jobs, err
		}
		if jobs, err := d.claimSingles(ctx, 1); err != nil || len(jobs) > 0 {
			return jobs, err
		}
		// The older singles may be a poll group held back (017-AC10): the slot goes to a due digest then.
		return d.claimDigests(ctx, 1, false)
	}
	jobs, err := d.claimSingles(ctx, n-1)
	if err != nil {
		return nil, err
	}
	digests, err := d.claimDigests(ctx, n-len(jobs), false)
	if err != nil && len(jobs) > 0 {
		// The deliveries already leased still go out; the digests wait for the next pass.
		d.log.Error("delivery_claim_failed", zap.Error(err))
		return jobs, nil
	}
	return append(jobs, digests...), err
}

// claimSinglesSQL is claimSingles' claim: $1 the lease in seconds, $2 the slots, $3 the groupable channel types.
const claimSinglesSQL = `WITH lead AS (SELECT x.id, x.channel_id, e.watch_id, e.observed_at, r.priority = 'high' AS high,
				x.rule_id IS NOT NULL AND c.type = ANY($3) AS poll
			FROM deliveries x JOIN channels c ON c.id = x.channel_id JOIN events e ON e.id = x.event_id LEFT JOIN rules r ON r.id = x.rule_id
			WHERE x.status IN ('pending', 'failed') AND x.next_attempt_at <= now() AND (x.leased_until IS NULL OR x.leased_until <= now()) AND NOT x.digest
				AND (x.rule_id IS NULL OR NOT c.type = ANY($3) OR (
					NOT EXISTS (SELECT 1 FROM events u WHERE u.watch_id = e.watch_id AND u.observed_at = e.observed_at AND u.routed_at IS NULL)
					AND NOT EXISTS (SELECT 1 FROM deliveries o JOIN events oe ON oe.id = o.event_id JOIN rules ro ON ro.id = o.rule_id
						WHERE o.status IN ('pending', 'failed') AND o.next_attempt_at <= now() AND NOT o.digest AND o.id <> x.id
							AND o.channel_id = x.channel_id AND oe.watch_id = e.watch_id AND oe.observed_at = e.observed_at
							AND (ro.priority = 'high') = (r.priority = 'high') AND (o.id < x.id OR o.leased_until > now()))))
			ORDER BY x.next_attempt_at LIMIT $2 FOR UPDATE OF x SKIP LOCKED),
		want AS (SELECT o.id, g.id AS lead FROM lead g JOIN deliveries o ON o.channel_id = g.channel_id
				JOIN events e ON e.id = o.event_id AND e.watch_id = g.watch_id AND e.observed_at = g.observed_at
				JOIN rules r ON r.id = o.rule_id AND (r.priority = 'high') = g.high
			WHERE g.poll AND o.status IN ('pending', 'failed') AND o.next_attempt_at <= now() AND NOT o.digest
			UNION ALL SELECT id, id FROM lead WHERE NOT poll),
		held AS (SELECT o.id, w.lead FROM deliveries o JOIN want w ON w.id = o.id
			WHERE o.status IN ('pending', 'failed') AND o.next_attempt_at <= now() AND (o.leased_until IS NULL OR o.leased_until <= now()) AND NOT o.digest
			FOR UPDATE OF o SKIP LOCKED),
		whole AS (SELECT lead, gen_random_uuid() AS token FROM want GROUP BY lead HAVING count(*) = (SELECT count(*) FROM held WHERE held.lead = want.lead))
		UPDATE deliveries x SET claim_token = whole.token, leased_until = now() + make_interval(secs => $1)
		FROM held JOIN whole ON whole.lead = held.lead WHERE x.id = held.id
		RETURNING x.id, x.event_id, x.claim_token::text, x.channel_id::text, x.rule_id::text, x.attempts`

// claimSingles leases up to n due deliveries sent alone, oldest due first. A due row on a channel that lists
// events (Telegram, Talk, email, Web Push) brings, under its token, every due row of its poll: same channel, watch,
// observed_at and priority (017). Only the poll's lowest due row leads, so claimers contend on one lock per group,
// and a poll still routing or with a member leased elsewhere is passed over, never taking a slot (017-AC1,
// 017-AC10). A row whose rule is gone has no priority and stays alone; a group of one is a plain single delivery.
// events_watch_observed_idx keeps each member check to its poll's events, not a scan of events per due row.
func (d *Deliverer) claimSingles(ctx context.Context, n int) ([]claimed, error) {
	rows, err := d.pool.Query(ctx, claimSinglesSQL, DeliveryLease.Seconds(), n, digestTypes)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	all, err := pgx.CollectRows(rows, scanClaimed)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	jobs := batches(all)
	for i, j := range jobs {
		if len(j.batch) == 1 {
			jobs[i] = j.batch[0]
		} else {
			jobs[i].poll = true
		}
	}
	return jobs, nil
}

// batches gathers claimed rows by token, one job each.
func batches(all []claimed) []claimed {
	var jobs []claimed
	at := map[string]int{}
	for _, c := range all {
		i, ok := at[c.token]
		if !ok {
			i = len(jobs)
			at[c.token] = i
			jobs = append(jobs, claimed{token: c.token, channelID: c.channelID})
		}
		jobs[i].batch = append(jobs[i].batch, c)
	}
	return jobs
}

func scanClaimed(r pgx.CollectableRow) (claimed, error) {
	var c claimed
	err := r.Scan(&c.id, &c.eventID, &c.token, &c.channelID, &c.ruleID, &c.attempts)
	return c, err
}

// claimDigests leases the due deliveries of up to n digest channels, one token per channel (012-AC3,
// 012-AC10). Both locks skip what is held, so the claim never waits under its channel lock: a channel held by
// another replica or an edit is passed over, and so is one with a due row held (a rule delete, a purge, a
// manual retry), whole, so its digest never goes out in two parts.
// Oldest digest first; with older, when set, only a digest due no later than every single delivery.
func (d *Deliverer) claimDigests(ctx context.Context, n int, older bool) ([]claimed, error) {
	rows, err := d.pool.Query(ctx, `WITH cand AS (SELECT channel_id, min(next_attempt_at) AS due_at FROM deliveries
				WHERE status IN ('pending', 'failed') AND next_attempt_at <= now() AND (leased_until IS NULL OR leased_until <= now()) AND digest
				GROUP BY channel_id),
			ch AS (SELECT c.id, gen_random_uuid() AS token FROM channels c JOIN cand ON cand.channel_id = c.id
				WHERE NOT $3 OR cand.due_at <= COALESCE((SELECT min(next_attempt_at) FROM deliveries WHERE status IN ('pending', 'failed')
					AND next_attempt_at <= now() AND (leased_until IS NULL OR leased_until <= now()) AND NOT digest), 'infinity')
				ORDER BY cand.due_at, c.id LIMIT $1 FOR NO KEY UPDATE OF c SKIP LOCKED),
			due AS (SELECT o.id, o.channel_id, ch.token FROM deliveries o JOIN ch ON ch.id = o.channel_id
				WHERE o.status IN ('pending', 'failed') AND o.next_attempt_at <= now() AND (o.leased_until IS NULL OR o.leased_until <= now()) AND o.digest
				FOR UPDATE OF o SKIP LOCKED),
			whole AS (SELECT ch.id FROM ch WHERE (SELECT count(*) FROM due WHERE due.channel_id = ch.id) =
				(SELECT count(*) FROM deliveries a WHERE a.channel_id = ch.id AND a.status IN ('pending', 'failed') AND a.next_attempt_at <= now()
					AND (a.leased_until IS NULL OR a.leased_until <= now()) AND a.digest))
		UPDATE deliveries x SET claim_token = due.token, leased_until = now() + make_interval(secs => $2)
		FROM due JOIN whole ON whole.id = due.channel_id WHERE x.id = due.id
		RETURNING x.id, x.event_id, x.claim_token::text, x.channel_id::text, x.rule_id::text, x.attempts`, n, DeliveryLease.Seconds(), older)
	if err != nil {
		return nil, fmt.Errorf("claim digests: %w", err)
	}
	all, err := pgx.CollectRows(rows, scanClaimed)
	if err != nil {
		return nil, fmt.Errorf("claim digests: %w", err)
	}
	return batches(all), nil
}

// deliveryColumns are the event (e) and watch (w) columns of a delivery, scanned into deliveryFields.
const deliveryColumns = `e.id, e.type, e.occurred_at, e.observed_at, e.payload, w.id::text, w.name, w.kind`

func deliveryFields(nd *notify.Delivery) []any {
	e := &nd.Event
	return []any{&e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &nd.Watch.ID, &nd.Watch.Name, &nd.Watch.Kind}
}

// load gathers what one send needs: the channel with its secret opened, the event, its watch and rule.
func (d *Deliverer) load(ctx context.Context, j claimed) (notify.Channel, notify.Delivery, error) {
	nd := notify.Delivery{ID: j.id}
	ch, err := loadChannel(ctx, d.pool, d.box, j.channelID)
	if err != nil {
		return ch, nd, err
	}
	var active bool
	// The channel owner must still own the watch: a transfer after routing sends nothing more.
	err = d.pool.QueryRow(ctx, `SELECT `+deliveryColumns+`, `+activeOwner+`
		FROM events e JOIN watches w ON w.id = e.watch_id JOIN channels c ON c.id = $2 JOIN users u ON u.id = c.owner_id
		WHERE e.id = $1 AND `+watch.ReadableSQL("w", "c.owner_id"), j.eventID, j.channelID).
		Scan(append(deliveryFields(&nd), &active)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ch, nd, errUnreadable
	}
	if err != nil {
		return ch, nd, fmt.Errorf("load event: %w", err)
	}
	if !active {
		return ch, nd, errOwnerDisabled
	}
	if j.ruleID != nil {
		var name, priority string
		err := d.pool.QueryRow(ctx, `SELECT name, priority FROM rules WHERE id = $1`, *j.ruleID).Scan(&name, &priority)
		switch {
		case err == nil:
			nd.Rule = &notify.Rule{ID: *j.ruleID, Name: name, Priority: priority}
		case !errors.Is(err, pgx.ErrNoRows):
			return ch, nd, fmt.Errorf("load rule: %w", err)
		}
	}
	return ch, nd, nil
}

func (d *Deliverer) deliver(stop context.Context, j claimed) {
	if j.batch != nil {
		d.deliverDigest(stop, j)
		return
	}
	log := d.log.With(zap.Int64("delivery_id", j.id), zap.String("channel_id", j.channelID))
	ctx, cancel := context.WithTimeout(stop, sendTimeout)
	defer cancel()
	start := time.Now()
	ch, nd, err := d.load(ctx, j)
	if err == nil && ch.Type == notify.TypeNextcloudTalk {
		if d.holdTalk(ctx, log, j) {
			return
		}
	}
	var logs *string
	switch {
	case err == nil && ch.Type == notify.TypeWebPush:
		_, err = pushSend(ctx, d.pool, d.box, d.sender, ch, nd)
	case err == nil:
		var lines []string
		lines, err = send(ctx, d.pool, d.sender, d.runner, ch, nd, false)
		if lines != nil {
			joined := strings.Join(lines, "\n")
			logs = &joined
		}
	}
	typ := ch.Type
	if typ == "" {
		typ = "unknown"
	}
	d.metrics.duration.WithLabelValues(typ).Observe(time.Since(start).Seconds())
	if drained(err, stop) {
		// Cut by the shutdown drain, not by the receiver: not an attempt. The next pass, on any
		// replica, sends it again.
		d.release(log, []int64{j.id}, j.token)
		return
	}
	done, ferr := d.finish(context.WithoutCancel(ctx), []int64{j.id}, j.token, j.attempts, err, logs)
	var status string
	for st := range done {
		status = st
	}
	switch {
	case ferr != nil:
		// The lease expires and another attempt runs: a send may repeat, never get lost.
		log.Error("delivery_store_failed", zap.Error(ferr))
		return
	case status == "":
		log.Warn("delivery_discarded")
		return
	}
	d.metrics.deliveries.WithLabelValues(typ, status).Inc()
	switch refused, partly := partlySent(err); {
	case partly:
		log.Debug("delivery_sent", zap.String("refused", refused))
	case status == "sent":
		log.Debug("delivery_sent")
	default:
		log.Info("delivery_failed", zap.String("status", status), zap.Int("attempt", j.attempts+1), zap.Error(err))
	}
}

// drained tells a send the shutdown drain cut from one that ended on its own: a send that succeeded,
// even as the drain ran out, is recorded like any other.
func drained(sendErr error, stop context.Context) bool {
	return sendErr != nil && !errors.Is(sendErr, errPartlySent) && stop.Err() != nil
}

// holdTalk sends the delivery (for a digest, every delivery under its token, j.id being the lowest) back
// without an attempt while its channel is paused after a 401, or while an earlier claim on the channel is
// in flight: concurrent sends would all fail before the first 401 is recorded. Attempts stay put, so a pause alone never makes a delivery dead. A failed check holds too: it
// says nothing about the channel, so it must not spend an attempt or risk a request. The channel's other
// due rows go back too: one by one, a backlog would take every send slot and delay the other channels.
func (d *Deliverer) holdTalk(ctx context.Context, log *zap.Logger, j claimed) bool {
	var paused, busy bool
	// ponytail: scans the channel's rows, 18 ms at 200k; a partial index needs a migration if a channel's history outgrows it.
	err := d.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM deliveries f JOIN channels c ON c.id = f.channel_id
			WHERE f.channel_id = $1 AND f.status = 'dead' AND f.last_error LIKE '%: HTTP 401'
				AND f.next_attempt_at > GREATEST(c.updated_at, now() - make_interval(secs => $3))),
		EXISTS (SELECT 1 FROM deliveries o JOIN deliveries me ON me.id = $2
			WHERE o.channel_id = $1 AND o.leased_until > now()
				AND (o.leased_until, o.id) < (me.leased_until, me.id))`,
		j.channelID, j.id, talkPause.Seconds()).Scan(&paused, &busy)
	if err != nil {
		log.Error("delivery_hold_check_failed", zap.Error(err))
		busy = true
	}
	if !paused && !busy {
		return false
	}
	status, lastErr, delay := (*string)(nil), (*string)(nil), talkBusyDelay
	if paused {
		failed, msg := "failed", talkPausedError
		status, lastErr, delay = &failed, &msg, Backoff[0]
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := d.pool.Exec(ctx, `UPDATE deliveries SET status = COALESCE($2, status), last_error = COALESCE($3, last_error),
			next_attempt_at = now() + make_interval(secs => $4), claim_token = NULL, leased_until = NULL
		WHERE channel_id = $5 AND status IN ('pending', 'failed') AND (claim_token = $1
			OR (next_attempt_at <= now() AND (leased_until IS NULL OR leased_until <= now())))`,
		j.token, status, lastErr, delay.Seconds(), j.channelID); err != nil {
		// The lease expires and the delivery is claimed again: nothing went out.
		log.Error("delivery_hold_failed", zap.Error(err))
	}
	log.Debug("delivery_held", zap.Bool("paused", paused))
	return true
}

// release hands the claim back untouched: attempts and next attempt stay as they were.
func (d *Deliverer) release(log *zap.Logger, ids []int64, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.pool.Exec(ctx, `UPDATE deliveries SET claim_token = NULL, leased_until = NULL WHERE id = ANY($1) AND claim_token = $2`,
		ids, token); err != nil {
		log.Error("delivery_release_failed", zap.Error(err))
	}
}

// finish records an attempt of the deliveries ids, and a script's logs, if the claim is still theirs; it
// returns how many ended in each status, none when another claim took over. A digest's rows share the outcome,
// sent_at and retry time, from the batch's highest attempt; each dies on its own count (012-AC3, 012-AC7).
func (d *Deliverer) finish(ctx context.Context, ids []int64, token string, attempts int, sendErr error, logs *string) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, lastErr, delay := "sent", (*string)(nil), time.Duration(0)
	if refusals, ok := partlySent(sendErr); ok {
		msg := truncate(refusals)
		lastErr, sendErr = &msg, nil
	}
	if sendErr != nil {
		msg := truncate(sendErr.Error())
		lastErr = &msg
		status = "failed"
		delay = retryDelay(attempts + 1)
		if errors.Is(sendErr, errUnreadable) || errors.Is(sendErr, errOwnerDisabled) || errors.Is(sendErr, notify.ErrPermanent) {
			status = "dead"
		}
	}
	if logs != nil {
		clean := strings.ReplaceAll(*logs, "\x00", "")
		logs = &clean
	}
	// A dead delivery's next_attempt_at is when it died: the Talk pause compares it to channel edits.
	rows, err := d.pool.Query(ctx, `WITH f AS (UPDATE deliveries SET
				status = CASE WHEN $3 = 'failed' AND attempts + 1 >= $7 THEN 'dead' ELSE $3 END, attempts = attempts + 1,
				last_error = CASE WHEN $3 = 'sent' AND last_error LIKE 'held:%' THEN NULL ELSE COALESCE($4, last_error) END,
				logs = COALESCE($6, logs), sent_at = CASE WHEN $3 = 'sent' THEN now() ELSE sent_at END,
				next_attempt_at = now() + CASE WHEN $3 = 'failed' AND attempts + 1 < $7 THEN make_interval(secs => $5) ELSE interval '0' END,
				claim_token = NULL, leased_until = NULL
			WHERE id = ANY($1) AND claim_token = $2 RETURNING status)
		SELECT status, count(*) FROM f GROUP BY status`, ids, token, status, lastErr, delay.Seconds(), logs, MaxAttempts)
	if err != nil {
		return nil, fmt.Errorf("finish delivery: %w", err)
	}
	done := map[string]int{}
	var st string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&st, &n}, func() error { done[st] = n; return nil }); err != nil {
		return nil, fmt.Errorf("finish delivery: %w", err)
	}
	return done, nil
}

// truncate fits an error into last_error: NUL dropped (text columns refuse it), 1 KiB at most.
func truncate(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= maxErrorLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxErrorLen], "")
}
