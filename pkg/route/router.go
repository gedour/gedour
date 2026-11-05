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
	"github.com/gedour/gedour/pkg/watch"
)

const (
	routeBatch = 100
	// routeBudget bounds the time one batch holds its event locks; what is left goes to the next batch.
	routeBudget = 2 * time.Second
	// maxPrograms bounds the compiled filter cache; past it the cache starts over.
	maxPrograms = 1000
)

// Router turns unrouted events into inbox items and pending deliveries.
type Router struct {
	pool    *pgxpool.Pool
	log     *zap.Logger
	metrics *Metrics
	poll    time.Duration
	budget  time.Duration
	progs   map[string]*filter.Program
}

// NewRouter builds a router polling every poll.
func NewRouter(pool *pgxpool.Pool, log *zap.Logger, m *Metrics, poll time.Duration) *Router {
	return &Router{pool: pool, log: log, metrics: m, poll: poll, budget: routeBudget, progs: map[string]*filter.Program{}}
}

// Run routes until ctx ends. An interrupted batch rolls back: its events stay unrouted for the next run.
func (r *Router) Run(ctx context.Context) {
	r.logRefusedRules(ctx)
	tick := time.NewTicker(r.poll)
	defer tick.Stop()
	for ctx.Err() == nil {
		for ctx.Err() == nil {
			n, err := r.RouteOnce(ctx)
			if err != nil && ctx.Err() == nil {
				r.log.Error("route_failed", zap.Error(err))
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
// route nothing, while its watches keep collecting for the users they are shared with (006-AC8).
const activeOwner = "u.disabled_at IS NULL AND u.purged_at IS NULL"

type routedEvent struct {
	ev watch.Event
	w  WatchRef
}

type matchRule struct {
	id, owner, filter, priority string
	channels                    []string
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

// routeEvent evaluates the enabled rules of every user who can read the event's watch right now: a
// revoked share stops routing at once.
func (r *Router) routeEvent(ctx context.Context, tx pgx.Tx, x routedEvent, notify map[string]bool) error {
	rows, err := tx.Query(ctx, `SELECT r.id::text, r.owner_id::text, r.filter, r.priority,
			COALESCE((SELECT array_agg(rc.channel_id::text) FROM rule_channels rc WHERE rc.rule_id = r.id), '{}')
		FROM rules r JOIN watches w ON w.id = $1 JOIN users u ON u.id = r.owner_id
		WHERE r.enabled AND `+activeOwner+` AND (r.watch_id IS NULL OR r.watch_id = $1) AND `+watch.ReadableSQL("w", "r.owner_id")+`
		ORDER BY r.created_at, r.id`, x.w.ID)
	if err != nil {
		return err
	}
	rules, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (matchRule, error) {
		var m matchRule
		err := row.Scan(&m.id, &m.owner, &m.filter, &m.priority, &m.channels)
		return m, err
	})
	if err != nil {
		return err
	}
	env := envOf(x.ev, x.w)
	// Decided over all of a user's rules before the insert, as the item is written once.
	items := map[string]string{}
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
			if _, err := tx.Exec(ctx, `INSERT INTO deliveries (event_id, rule_id, channel_id) SELECT $1, $2, unnest($3::uuid[])
				ON CONFLICT (event_id, channel_id) DO NOTHING`, x.ev.ID, rule.id, rule.channels); err != nil {
				return err
			}
		}
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

func (r *Router) match(rule matchRule, env filter.Env) (bool, error) {
	p, err := r.program(rule.filter)
	if err != nil {
		return false, err
	}
	return filter.Match(p, env)
}
