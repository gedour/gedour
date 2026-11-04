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
	"github.com/gedour/gedour/pkg/watch"
)

const (
	// MaxAttempts is the attempt after which a failing delivery is dead.
	MaxAttempts = 5
	// DeliveryLease holds a claimed delivery; it outlasts the slowest send (SMTP, 30 s) by far.
	DeliveryLease = 2 * time.Minute
	maxErrorLen   = 1024
	sendTimeout   = 45 * time.Second
)

// errUnreadable ends a delivery without sending: its channel's owner lost access to the watch.
var errUnreadable = errors.New("not sent: the channel's owner can no longer read this watch")

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
	poll        time.Duration
	concurrency int
	drain       time.Duration
}

// NewDeliverer builds a delivery loop sending up to concurrency deliveries at once (runner runs the
// script channels); on stop, sends in flight get drain to finish before they are cancelled.
func NewDeliverer(pool *pgxpool.Pool, log *zap.Logger, box *secret.Box, sender *notify.Sender, runner *script.Runner, m *Metrics, poll time.Duration, concurrency int, drain time.Duration) *Deliverer {
	return &Deliverer{pool: pool, log: log, box: box, sender: sender, runner: runner, metrics: m, poll: poll, concurrency: max(concurrency, 1), drain: drain}
}

type claimed struct {
	id, eventID int64
	token       string
	channelID   string
	ruleID      *string
	attempts    int
}

// Run delivers until ctx ends, claiming as slots free up. Then sends in flight get the drain time to
// finish; past it they are cancelled and their claims released, attempts unchanged.
func (d *Deliverer) Run(ctx context.Context) {
	hard, stopHard := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHard()
	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup
	tick := time.NewTicker(d.poll)
	defer tick.Stop()
	for ctx.Err() == nil {
		for ctx.Err() == nil {
			n, err := d.dispatch(ctx, hard, sem, &wg)
			if err != nil && ctx.Err() == nil {
				d.log.Error("delivery_claim_failed", zap.Error(err))
			}
			if err != nil || n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
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
			defer func() { <-sem }()
			d.deliver(hard, j)
		})
	}
	return len(jobs), nil
}

// DeliverOnce claims up to concurrency due deliveries, sends them and waits for them: one poll, for tests.
func (d *Deliverer) DeliverOnce(ctx context.Context) (int, error) {
	var wg sync.WaitGroup
	n, err := d.dispatch(ctx, ctx, make(chan struct{}, d.concurrency), &wg)
	wg.Wait()
	return n, err
}

// claim leases due deliveries on the database clock, as the watch scheduler does (001-AC15).
func (d *Deliverer) claim(ctx context.Context, n int) ([]claimed, error) {
	rows, err := d.pool.Query(ctx, `UPDATE deliveries x SET claim_token = gen_random_uuid(), leased_until = now() + make_interval(secs => $1)
		FROM (SELECT id FROM deliveries WHERE status IN ('pending', 'failed') AND next_attempt_at <= now()
			AND (leased_until IS NULL OR leased_until <= now())
			ORDER BY next_attempt_at LIMIT $2 FOR UPDATE SKIP LOCKED) due
		WHERE x.id = due.id
		RETURNING x.id, x.event_id, x.claim_token::text, x.channel_id::text, x.rule_id::text, x.attempts`,
		DeliveryLease.Seconds(), n)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
		var c claimed
		err := r.Scan(&c.id, &c.eventID, &c.token, &c.channelID, &c.ruleID, &c.attempts)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	return jobs, nil
}

// load gathers what one send needs: the channel with its secret opened, the event, its watch and rule.
func (d *Deliverer) load(ctx context.Context, j claimed) (notify.Channel, notify.Delivery, error) {
	nd := notify.Delivery{ID: j.id}
	ch, err := loadChannel(ctx, d.pool, d.box, j.channelID)
	if err != nil {
		return ch, nd, err
	}
	e := &nd.Event
	// The channel owner must still read the watch: a share revoked after routing sends nothing more.
	err = d.pool.QueryRow(ctx, `SELECT e.id, e.type, e.occurred_at, e.observed_at, e.payload, w.id::text, w.name, w.kind
		FROM events e JOIN watches w ON w.id = e.watch_id JOIN channels c ON c.id = $2
		WHERE e.id = $1 AND `+watch.ReadableSQL("w", "c.owner_id"), j.eventID, j.channelID).
		Scan(&e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &nd.Watch.ID, &nd.Watch.Name, &nd.Watch.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ch, nd, errUnreadable
	}
	if err != nil {
		return ch, nd, fmt.Errorf("load event: %w", err)
	}
	if j.ruleID != nil {
		var name string
		err := d.pool.QueryRow(ctx, `SELECT name FROM rules WHERE id = $1`, *j.ruleID).Scan(&name)
		switch {
		case err == nil:
			nd.Rule = &notify.Rule{ID: *j.ruleID, Name: name}
		case !errors.Is(err, pgx.ErrNoRows):
			return ch, nd, fmt.Errorf("load rule: %w", err)
		}
	}
	return ch, nd, nil
}

func (d *Deliverer) deliver(stop context.Context, j claimed) {
	log := d.log.With(zap.Int64("delivery_id", j.id), zap.String("channel_id", j.channelID))
	ctx, cancel := context.WithTimeout(stop, sendTimeout)
	defer cancel()
	start := time.Now()
	ch, nd, err := d.load(ctx, j)
	var logs *string
	if err == nil {
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
		// Cut by the shutdown drain, not by the receiver: not an attempt. The next poll, on any
		// replica, sends it again.
		d.release(log, j)
		return
	}
	status, ferr := d.finish(context.WithoutCancel(ctx), j, err, logs)
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
	if err != nil {
		log.Info("delivery_failed", zap.String("status", status), zap.Int("attempt", j.attempts+1), zap.Error(err))
		return
	}
	log.Debug("delivery_sent")
}

// drained tells a send the shutdown drain cut from one that ended on its own: a send that succeeded,
// even as the drain ran out, is recorded like any other.
func drained(sendErr error, stop context.Context) bool {
	return sendErr != nil && stop.Err() != nil
}

// release hands the claim back untouched: attempts and next attempt stay as they were.
func (d *Deliverer) release(log *zap.Logger, j claimed) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := d.pool.Exec(ctx, `UPDATE deliveries SET claim_token = NULL, leased_until = NULL WHERE id = $1 AND claim_token = $2`,
		j.id, j.token); err != nil {
		log.Error("delivery_release_failed", zap.Error(err))
	}
}

// finish records the attempt, and a script's logs, if the claim is still ours; "" means another claim
// took over.
func (d *Deliverer) finish(ctx context.Context, j claimed, sendErr error, logs *string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	attempt := j.attempts + 1
	status, lastErr, delay := "sent", (*string)(nil), time.Duration(0)
	if sendErr != nil {
		msg := truncate(sendErr.Error())
		lastErr = &msg
		status = "failed"
		delay = retryDelay(attempt)
		if attempt >= MaxAttempts || errors.Is(sendErr, errUnreadable) {
			status = "dead"
		}
	}
	if logs != nil {
		clean := strings.ReplaceAll(*logs, "\x00", "")
		logs = &clean
	}
	tag, err := d.pool.Exec(ctx, `UPDATE deliveries SET status = $3, attempts = $4, last_error = COALESCE($5, last_error),
			logs = COALESCE($7, logs), sent_at = CASE WHEN $3 = 'sent' THEN now() ELSE sent_at END,
			next_attempt_at = now() + make_interval(secs => $6), claim_token = NULL, leased_until = NULL
		WHERE id = $1 AND claim_token = $2`, j.id, j.token, status, attempt, lastErr, delay.Seconds(), logs)
	if err != nil {
		return "", fmt.Errorf("finish delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", nil
	}
	return status, nil
}

// truncate fits an error into last_error: NUL dropped (text columns refuse it), 1 KiB at most.
func truncate(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= maxErrorLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxErrorLen], "")
}
