package route

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/filter"
	"github.com/gedour/gedour/pkg/wake"
	"github.com/gedour/gedour/pkg/watch"
)

const (
	routeBatch = 100
	// routeBudget bounds the time one batch holds its event locks; what is left goes to the next batch.
	routeBudget = 2 * time.Second
	// maxPrograms bounds the compiled filter cache; past it the cache starts over.
	maxPrograms = 1000
	// maxLocations bounds the digest time zone cache; past it the cache starts over.
	maxLocations = 100
)

// Router turns unrouted events into inbox items and pending deliveries.
type Router struct {
	pool    *pgxpool.Pool
	log     *zap.Logger
	metrics *Metrics
	loop    *wake.Loop
	budget  time.Duration
	progs   map[string]*filter.Program
	locs    map[string]*time.Location
}

// NewRouter builds a router paced by loop, which new events wake.
func NewRouter(pool *pgxpool.Pool, log *zap.Logger, m *Metrics, loop *wake.Loop) *Router {
	return &Router{pool: pool, log: log, metrics: m, loop: loop, budget: routeBudget, progs: map[string]*filter.Program{},
		locs: map[string]*time.Location{}}
}

// Run routes until ctx ends. An interrupted batch rolls back: its events stay unrouted for the next run.
func (r *Router) Run(ctx context.Context) {
	r.logRefusedRules(ctx)
	for ctx.Err() == nil {
		var err error
		for ctx.Err() == nil {
			var n int
			if n, err = r.RouteOnce(ctx); err != nil || n == 0 {
				break
			}
		}
		if err != nil && ctx.Err() == nil {
			r.log.Error("route_failed", zap.Error(err))
		}
		// No deadline: an unrouted event always comes with a notification.
		if !r.loop.Wait(ctx, time.Time{}, err != nil) {
			break
		}
	}
}

// logRefusedRules names the enabled rules that no longer compile, say after the cost model changed: each one
// counts as no match on every event, which an owner would not notice otherwise.
func (r *Router) logRefusedRules(ctx context.Context) {
	rows, err := r.pool.Query(ctx, `SELECT r.id::text, r.owner_id::text, r.filter FROM rules r JOIN users u ON u.id = r.owner_id
		WHERE r.enabled AND `+activeOwner)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("rule_check_failed", zap.Error(err))
		}
		return
	}
	rules, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (matchRule, error) {
		var m matchRule
		err := row.Scan(&m.id, &m.owner, &m.filter)
		return m, err
	})
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("rule_check_failed", zap.Error(err))
		}
		return
	}
	for _, rule := range rules {
		if _, err := filter.Compile(rule.filter); err != nil {
			r.log.Error("rule_filter_refused", zap.String("rule_id", rule.id), zap.String("owner_id", rule.owner), zap.Error(err))
		}
	}
}

// activeOwner keeps the users aliased u that are neither disabled nor purged: a disabled account's rules
// route nothing (006-AC8).
const activeOwner = "u.disabled_at IS NULL AND u.purged_at IS NULL"

type routedEvent struct {
	ev watch.Event
	w  WatchRef
}

type matchRule struct {
	id, owner, filter, priority, delivery string
	channels                              []string
}

func (r *Router) program(src string) (*filter.Program, error) {
	if p, ok := r.progs[src]; ok {
		return p, nil
	}
	p, err := filter.Compile(src)
	if err != nil {
		return nil, err
	}
	if len(r.progs) >= maxPrograms {
		r.progs = map[string]*filter.Program{}
	}
	r.progs[src] = p
	return p, nil
}

// RouteOnce routes one batch in one transaction and returns how many events it took. SKIP LOCKED
// lets replicas route side by side; routed_at set in the same transaction makes each event routed once.
// 001-AC14.
func (r *Router) RouteOnce(ctx context.Context) (int, error) {
	var n int
	err := db.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		n = 0
		rows, err := tx.Query(ctx, `SELECT e.id, e.type, e.occurred_at, e.observed_at, e.payload, w.id::text, w.name, w.kind
			FROM events e JOIN watches w ON w.id = e.watch_id
			WHERE e.routed_at IS NULL ORDER BY e.id LIMIT $1 FOR UPDATE OF e SKIP LOCKED`, routeBatch)
		if err != nil {
			return err
		}
		events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (routedEvent, error) {
			var x routedEvent
			e := &x.ev
			err := row.Scan(&e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &x.w.ID, &x.w.Name, &x.w.Kind)
			return x, err
		})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		notify := map[string]bool{}
		var ids []int64
		start := time.Now()
		for _, x := range events {
			// At least one event per batch, so a slow one still makes progress.
			if len(ids) > 0 && time.Since(start) > r.budget {
				break
			}
			ids = append(ids, x.ev.ID)
			if err := r.routeEvent(ctx, tx, x, notify); err != nil {
				return fmt.Errorf("event %d: %w", x.ev.ID, err)
			}
		}
		n = len(ids)
		if _, err := tx.Exec(ctx, `UPDATE events SET routed_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
		for user := range notify {
			// Delivered at commit, so a stream never recounts before the item is visible.
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, InboxChannel, user); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("route: %w", err)
	}
	r.metrics.routed.Add(float64(n))
	return n, nil
}

// routeEvent evaluates the enabled rules of the event's watch owner right now: a transfer stops the old
// owner's routing at once.
func (r *Router) routeEvent(ctx context.Context, tx pgx.Tx, x routedEvent, notify map[string]bool) error {
	rows, err := tx.Query(ctx, `SELECT r.id::text, r.owner_id::text, r.filter, r.priority, r.delivery,
			COALESCE((SELECT array_agg(rc.channel_id::text) FROM rule_channels rc WHERE rc.rule_id = r.id), '{}')
		FROM rules r JOIN watches w ON w.id = $1 JOIN users u ON u.id = r.owner_id
		WHERE r.enabled AND `+activeOwner+` AND (r.all_watches OR EXISTS (SELECT 1 FROM rule_watches rw WHERE rw.rule_id = r.id AND rw.watch_id = $1)) AND `+watch.ReadableSQL("w", "r.owner_id")+`
		ORDER BY r.created_at, r.id`, x.w.ID)
	if err != nil {
		return err
	}
	rules, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (matchRule, error) {
		var m matchRule
		err := row.Scan(&m.id, &m.owner, &m.filter, &m.priority, &m.delivery, &m.channels)
		return m, err
	})
	if err != nil {
		return err
	}
	env := envOf(x.ev, x.w)
	// Decided over all of a user's rules before the insert, as the item is written once.
	items := map[string]string{}
	var hits []matchRule
	for _, rule := range rules {
		matched, err := r.match(rule, env)
		if err != nil {
			r.metrics.evalErrors.Inc()
			r.log.Warn("rule_eval_failed", zap.String("rule_id", rule.id), zap.Int64("event_id", x.ev.ID), zap.Error(err))
			continue
		}
		if !matched {
			continue
		}
		if items[rule.owner] != priorityHigh {
			items[rule.owner] = rule.priority
		}
		if len(rule.channels) > 0 {
			hits = append(hits, rule)
		}
	}
	if err := r.insertDeliveries(ctx, tx, x.ev.ID, hits); err != nil {
		return err
	}
	for owner, priority := range items {
		// One item per user per event, however many of their rules match. DO NOTHING: a replay keeps the stored level.
		tag, err := tx.Exec(ctx, `INSERT INTO inbox_items (user_id, event_id, priority) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, owner, x.ev.ID, priority)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			notify[owner] = true
		}
	}
	return nil
}

// insertDeliveries queues one delivery per channel of the matched rules. A digest rule's delivery to a channel
// that can list events waits for the rule's next scheduled time (012-AC15); any rule sending at once takes it
// over, whatever the order, so an event sends one delivery per channel (012-AC6, 012-AC16); between two digest
// rules the earlier due time wins (012-AC19).
func (r *Router) insertDeliveries(ctx context.Context, tx pgx.Tx, event int64, hits []matchRule) error {
	if len(hits) == 0 {
		return nil
	}
	due, batch, err := r.digestDue(ctx, tx, hits)
	if err != nil {
		return err
	}
	for _, rule := range hits {
		at := make([]*time.Time, len(rule.channels))
		if t, ok := due[rule.id]; ok {
			for i, c := range rule.channels {
				if batch[c] {
					at[i] = &t
				}
			}
		}
		// The update lets a delivery sent at once, or a digest due sooner, take one an earlier rule left waiting; rule_id
		// follows, so that rule's schedule edits reschedule it.
		if _, err := tx.Exec(ctx, `INSERT INTO deliveries (event_id, rule_id, channel_id, next_attempt_at, digest)
				SELECT $1, $2, u.c, COALESCE(u.t, now()), u.t IS NOT NULL FROM unnest($3::uuid[], $4::timestamptz[]) AS u (c, t)
			ON CONFLICT (event_id, channel_id) DO UPDATE SET rule_id = EXCLUDED.rule_id, next_attempt_at = EXCLUDED.next_attempt_at, digest = EXCLUDED.digest
				WHERE deliveries.digest AND (NOT EXCLUDED.digest OR EXCLUDED.next_attempt_at < deliveries.next_attempt_at)`, event, rule.id, rule.channels, at); err != nil {
			return err
		}
	}
	return nil
}

// digestDue maps the digest rules among hits to their next scheduled time on the database clock, and returns the
// set of their channels that can take a digest. KEY SHARE on the digest rules waits out an edit in flight and reads
// the schedule it commits; one turning a rule into a digest is not waited on and its deliveries go out at once.
func (r *Router) digestDue(ctx context.Context, tx pgx.Tx, hits []matchRule) (map[string]time.Time, map[string]bool, error) {
	var ids, chans []string
	for _, rule := range hits {
		// A high priority rule is never stored as a digest by the API; one written otherwise still goes at once.
		if rule.delivery == deliveryDigest && rule.priority != priorityHigh {
			ids = append(ids, rule.id)
			chans = append(chans, rule.channels...)
		}
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text, COALESCE(digest_at, '{}'), COALESCE(digest_tz, ''), now() FROM rules
		WHERE id = ANY($1::uuid[]) AND delivery = 'digest' AND enabled FOR KEY SHARE`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("digest rules: %w", err)
	}
	defer rows.Close()
	due := map[string]time.Time{}
	for rows.Next() {
		var id string
		var s schedule
		var now time.Time
		if err := rows.Scan(&id, &s.At, &s.TZ, &now); err != nil {
			return nil, nil, fmt.Errorf("digest rules: %w", err)
		}
		t, err := s.next(now, r.location)
		if err != nil {
			// A bad stored schedule must not stall routing: the delivery goes out now instead.
			r.log.Warn("digest_schedule_invalid", zap.String("rule_id", id), zap.Error(err))
			continue
		}
		due[id] = t
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("digest rules: %w", err)
	}
	if len(due) == 0 {
		return nil, nil, nil
	}
	rows, err = tx.Query(ctx, `SELECT id::text FROM channels WHERE id = ANY($1::uuid[]) AND type = ANY($2)`, chans, digestTypes)
	if err != nil {
		return nil, nil, fmt.Errorf("digest channels: %w", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, fmt.Errorf("digest channels: %w", err)
	}
	batch := make(map[string]bool, len(list))
	for _, c := range list {
		batch[c] = true
	}
	return due, batch, nil
}

// location caches time.LoadLocation, which reads zoneinfo on every call.
func (r *Router) location(name string) (*time.Location, error) {
	if l, ok := r.locs[name]; ok {
		return l, nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	if len(r.locs) >= maxLocations {
		r.locs = map[string]*time.Location{}
	}
	r.locs[name] = l
	return l, nil
}

func (r *Router) match(rule matchRule, env filter.Env) (bool, error) {
	p, err := r.program(rule.filter)
	if err != nil {
		return false, err
	}
	return filter.Match(p, env)
}
