// Package worker runs due watches: it claims them from PostgreSQL (FOR UPDATE SKIP LOCKED), collects
// their state, stores state and events, and schedules the next run.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/watch"
)

const (
	// Lease is how long a claim holds a watch: a crashed worker's watch runs again after it.
	Lease = 5 * time.Minute
	// RunTimeout bounds one run's collection; with finishTimeout it stays well under Lease, so a live
	// run never outlives its claim.
	RunTimeout    = 60 * time.Second
	finishTimeout = 10 * time.Second
	// maxRateLimitWait caps how far a rate limit may push the next run.
	maxRateLimitWait = time.Hour
	maxErrorLen      = 1024
	keepStates       = 50
)

// Options tune a worker.
type Options struct {
	Poll         time.Duration
	Concurrency  int
	DrainTimeout time.Duration
	UserAgent    string
	HTTPHosts    []string
	RunTimeout   time.Duration // defaults to RunTimeout
}

// Worker claims and runs due watches.
type Worker struct {
	pool       *pgxpool.Pool
	log        *zap.Logger
	box        *secret.Box
	client     *http.Client
	collectors map[string]collect.Collector
	metrics    *Metrics
	opts       Options
	// now and jitter are swapped by tests for a controllable clock and exact intervals.
	now    func() time.Time
	jitter func() float64
}

// New builds a worker; client is the guarded HTTP client every collector request goes through.
// A RunTimeout that leaves the finish no room inside the lease is refused: the claim would expire mid-run.
func New(pool *pgxpool.Pool, log *zap.Logger, box *secret.Box, client *http.Client, collectors map[string]collect.Collector, m *Metrics, o Options) (*Worker, error) {
	if o.RunTimeout <= 0 {
		o.RunTimeout = RunTimeout
	}
	if o.RunTimeout > Lease-finishTimeout {
		return nil, fmt.Errorf("worker run timeout %s exceeds the lease %s minus the finish time %s", o.RunTimeout, Lease, finishTimeout)
	}
	return &Worker{pool: pool, log: log, box: box, client: client, collectors: collectors, metrics: m, opts: o,
		now: time.Now, jitter: func() float64 { return rand.Float64()*2 - 1 }}, nil //nolint:gosec // jitter, not a secret
}

type job struct {
	id, kind, config, schedule string
	credentialID               *string
	// token proves the claim is still ours at finish; due is next_run_at when claimed.
	token string
	due   time.Time
}

// Run polls until ctx ends, then stops claiming and lets in-flight runs finish within DrainTimeout;
// runs still going after that are cancelled and their claims released.
func (w *Worker) Run(ctx context.Context) {
	hard, stopHard := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHard()
	var wg sync.WaitGroup
	sem := make(chan struct{}, w.opts.Concurrency)
	tick := time.NewTicker(w.opts.Poll)
	defer tick.Stop()
	w.log.Info("worker_started", zap.Int("concurrency", w.opts.Concurrency), zap.Duration("poll", w.opts.Poll))
	for ctx.Err() == nil {
		w.updateDue(ctx)
		for ctx.Err() == nil {
			n, err := w.dispatch(ctx, hard, sem, &wg)
			if err != nil && ctx.Err() == nil {
				w.log.Error("worker_claim_failed", zap.Error(err))
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
	case <-time.After(w.opts.DrainTimeout):
		w.log.Warn("worker_drain_timeout", zap.Duration("timeout", w.opts.DrainTimeout))
		stopHard()
		<-done
	}
	w.log.Info("worker_stopped")
}

// dispatch claims as many due watches as there are free slots and starts them; it returns how many.
func (w *Worker) dispatch(ctx, hard context.Context, sem chan struct{}, wg *sync.WaitGroup) (int, error) {
	free := cap(sem) - len(sem)
	if free == 0 {
		return 0, nil
	}
	jobs, err := w.claim(ctx, free)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			w.run(hard, j)
		})
	}
	return len(jobs), nil
}

// leaseFree selects watches no live claim holds. Leases read the database clock, the one every replica
// shares, so a worker with a skewed clock cannot steal or overstay a claim.
const leaseFree = `(leased_until IS NULL OR leased_until <= now())`

// ownerEnabled skips a disabled owner's watches (009-AC12); a sub-select, so FOR UPDATE never locks users.
const ownerEnabled = `NOT EXISTS (SELECT 1 FROM users u WHERE u.id = watches.owner_id AND u.disabled_at IS NOT NULL)`

// claim leases up to n due watches; SKIP LOCKED lets replicas claim side by side, each watch once (001-AC9).
// The lease lives in its own columns: a run-now or an edit moves next_run_at, never the claim.
func (w *Worker) claim(ctx context.Context, n int) ([]job, error) {
	now := w.now()
	rows, err := w.pool.Query(ctx, `UPDATE watches w SET claim_token = gen_random_uuid(), leased_until = now() + make_interval(secs => $2)
		FROM (SELECT id FROM watches WHERE NOT paused AND next_run_at <= $1 AND `+leaseFree+` AND `+ownerEnabled+`
			ORDER BY next_run_at LIMIT $3 FOR UPDATE SKIP LOCKED) due
		WHERE w.id = due.id
		RETURNING w.id::text, w.kind, w.config::text, w.schedule, w.credential_id::text, w.claim_token::text, w.next_run_at`,
		now, Lease.Seconds(), n)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (job, error) {
		var j job
		err := r.Scan(&j.id, &j.kind, &j.config, &j.schedule, &j.credentialID, &j.token, &j.due)
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return jobs, nil
}

func (w *Worker) updateDue(ctx context.Context) {
	var n int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM watches WHERE NOT paused AND next_run_at <= $1 AND `+leaseFree+` AND `+ownerEnabled, w.now()).Scan(&n); err == nil {
		w.metrics.due.Set(float64(n))
	}
}

// run executes one claimed watch and records its outcome; it never returns an error, it logs it.
func (w *Worker) run(hard context.Context, j job) {
	start := time.Now()
	log := w.log.With(zap.String("watch_id", j.id), zap.String("kind", j.kind))
	ctx, cancel := context.WithTimeout(hard, w.opts.RunTimeout)
	state, f, err := w.collect(ctx, j)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	if hard.Err() != nil {
		w.release(log, j)
		return
	}
	if err != nil && timedOut {
		err = fmt.Errorf("timeout after %s", w.opts.RunTimeout)
	}
	// Its own context: the run's may have expired, and that outcome must still be stored.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(hard), finishTimeout)
	defer fcancel()
	status, events, ferr := w.finish(fctx, j, state, f, err)
	if ferr != nil {
		log.Error("watch_run_store_failed", zap.Strings("hosts", f.Hosts), zap.Error(ferr))
		// A failure that repeats on every retry (data the database refuses) must still surface and back off.
		if w.recordStoreFailure(context.WithoutCancel(hard), j, ferr) {
			w.metrics.runs.WithLabelValues(j.kind, "error").Inc()
		}
		return
	}
	w.metrics.runs.WithLabelValues(j.kind, status).Inc()
	hosts := zap.Strings("hosts", f.Hosts)
	if status == statusDiscarded {
		log.Warn("watch_run_discarded", hosts)
		return
	}
	w.metrics.duration.WithLabelValues(j.kind).Observe(time.Since(start).Seconds())
	for _, e := range events {
		w.metrics.events.WithLabelValues(j.kind, e).Inc()
	}
	if err != nil && !errors.Is(err, collect.ErrNotModified) {
		log.Info("watch_run_failed", zap.String("status", status), hosts, zap.Error(err))
		return
	}
	log.Debug("watch_run_done", zap.Int("events", len(events)), hosts, zap.Duration("took", time.Since(start)))
}

// release hands an unfinished claim back; next_run_at is still due, so the next poll, on any replica, runs it.
func (w *Worker) release(log *zap.Logger, j job) {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	if _, err := w.pool.Exec(ctx, `UPDATE watches SET claim_token = NULL, leased_until = NULL WHERE id = $1 AND claim_token = $2`,
		j.id, j.token); err != nil {
		log.Error("watch_release_failed", zap.Error(err))
	}
}

// collect opens the credential, loads the conditional-request cache and calls the kind's collector.
func (w *Worker) collect(ctx context.Context, j job) (json.RawMessage, *collect.Fetcher, error) {
	f := &collect.Fetcher{Client: w.client, UserAgent: w.opts.UserAgent, HTTPHosts: w.opts.HTTPHosts, Now: w.now}
	c, ok := w.collectors[j.kind]
	if !ok {
		return nil, f, fmt.Errorf("no collector for kind %q", j.kind)
	}
	cred, err := w.credential(ctx, j)
	if err != nil {
		return nil, f, err
	}
	// Validators only make sense next to a stored state: a 304 without one would leave no baseline.
	rows, err := w.pool.Query(ctx, `SELECT url, etag, last_modified FROM http_cache
		WHERE watch_id = $1 AND EXISTS (SELECT 1 FROM watch_states WHERE watch_id = $1)`, j.id)
	if err != nil {
		return nil, f, fmt.Errorf("load http cache: %w", err)
	}
	f.Cache = map[string]collect.Validators{}
	var u string
	var v collect.Validators
	if _, err := pgx.ForEachRow(rows, []any{&u, &v.ETag, &v.LastModified}, func() error {
		f.Cache[u] = v
		return nil
	}); err != nil {
		return nil, f, fmt.Errorf("load http cache: %w", err)
	}
	state, err := c.Collect(ctx, f, json.RawMessage(j.config), cred)
	return state, f, err
}

// credential decrypts the watch's credential for this run only; it is never logged.
func (w *Worker) credential(ctx context.Context, j job) (*collect.Credential, error) {
	if j.credentialID == nil {
		return nil, nil
	}
	var c collect.Credential
	var username *string
	var ct, nonce []byte
	err := w.pool.QueryRow(ctx, `SELECT kind, username, secret_ciphertext, secret_nonce FROM credentials WHERE id = $1`,
		*j.credentialID).Scan(&c.Kind, &username, &ct, &nonce)
	if err != nil {
		return nil, fmt.Errorf("load credential: %w", err)
	}
	plain, err := w.box.Open(ct, nonce, []byte(*j.credentialID))
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}
	if username != nil {
		c.Username = *username
	}
	c.Secret = string(plain)
	return &c, nil
}

// finish stores the run's outcome in one transaction, under the watch's row lock. It returns the run
// status and the types of the events inserted.
func (w *Worker) finish(ctx context.Context, j job, state json.RawMessage, f *collect.Fetcher, runErr error) (string, []string, error) {
	var status string
	var inserted []string
	err := db.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		status, inserted = "ok", nil
		var config, schedule string
		var failures int
		err := tx.QueryRow(ctx, `SELECT config::text, schedule, consecutive_failures FROM watches WHERE id = $1 AND claim_token = $2 FOR UPDATE`,
			j.id, j.token).Scan(&config, &schedule, &failures)
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted, or the lease expired and another run owns the watch now: its result wins.
			status = statusDiscarded
			return nil
		}
		if err != nil {
			return err
		}
		// Retargeted meanwhile: this state describes the old target, and the edit already made it due.
		if config != j.config {
			status = statusDiscarded
			_, err := tx.Exec(ctx, `UPDATE watches SET claim_token = NULL, leased_until = NULL WHERE id = $1`, j.id)
			return err
		}
		now := w.now()
		next, interval, err := watch.NextRun(schedule, now, w.jitter())
		if err != nil {
			return err
		}
		var rl *collect.RateLimitError
		switch {
		case runErr == nil || errors.Is(runErr, collect.ErrNotModified):
			if runErr == nil {
				if inserted, err = w.store(ctx, tx, j, state, f, now); err != nil {
					return err
				}
			}
			for u, v := range f.Updates {
				if _, err := tx.Exec(ctx, `INSERT INTO http_cache (watch_id, url, etag, last_modified) VALUES ($1, $2, $3, $4)
					ON CONFLICT (watch_id, url) DO UPDATE SET etag = excluded.etag, last_modified = excluded.last_modified`,
					j.id, u, v.ETag, v.LastModified); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, `UPDATE watches SET last_run_at = $2, last_status = 'ok', last_error = NULL,
				consecutive_failures = 0, `+scheduleNext+` WHERE id = $1`, j.id, now, next, j.due)
		case errors.As(runErr, &rl):
			// 001-AC11: wait for the reset instead of hammering; not a failure. A fresh 429 outranks a run-now made before it, so no scheduleNext.
			status = "rate_limited"
			_, err = tx.Exec(ctx, `UPDATE watches SET last_run_at = $2, last_status = 'rate_limited', last_error = $4,
				next_run_at = $3, claim_token = NULL, leased_until = NULL WHERE id = $1`, j.id, now, rateLimitNext(rl.Until, now), truncate(runErr.Error()))
		default:
			// 001-AC10: exponential backoff from now, capped at 8 intervals.
			status = "error"
			failures++
			_, err = tx.Exec(ctx, `UPDATE watches SET last_run_at = $2, last_status = 'error', last_error = $5,
				consecutive_failures = $6, `+scheduleNext+` WHERE id = $1`,
				j.id, now, now.Add(Backoff(interval, failures)), j.due, truncate(runErr.Error()), failures)
		}
		return err
	})
	return status, inserted, err
}

const statusDiscarded = "discarded"

// recordStoreFailure stores a failed finish as a run error with backoff and frees the lease, if the
// claim is still ours. Best effort: when this fails too, the lease expiry retries the watch.
func (w *Worker) recordStoreFailure(parent context.Context, j job, cause error) bool {
	// Its own deadline: the failed finish may have used up fctx's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), finishTimeout)
	defer cancel()
	recorded := false
	err := db.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		recorded = false
		var failures int
		err := tx.QueryRow(ctx, `SELECT consecutive_failures FROM watches WHERE id = $1 AND claim_token = $2 FOR UPDATE`,
			j.id, j.token).Scan(&failures)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := w.now()
		_, interval, err := watch.NextRun(j.schedule, now, 0)
		if err != nil {
			interval = time.Hour
		}
		failures++
		_, err = tx.Exec(ctx, `UPDATE watches SET last_run_at = $2, last_status = 'error', last_error = $5,
			consecutive_failures = $6, `+scheduleNext+` WHERE id = $1`,
			j.id, now, now.Add(Backoff(interval, failures)), j.due, truncate("store: "+cause.Error()), failures)
		recorded = err == nil
		return err
	})
	if err != nil {
		w.log.Error("watch_store_failure_not_recorded", zap.String("watch_id", j.id), zap.Error(err))
	}
	return recorded
}

// scheduleNext sets the next run ($3) and frees the lease. A next_run_at moved during the run ($4 is
// its value at claim) is a run-now or an edit: the earlier of the two wins, so the request is kept.
const scheduleNext = `next_run_at = CASE WHEN next_run_at <> $4 THEN LEAST(next_run_at, $3) ELSE $3 END,
	claim_token = NULL, leased_until = NULL`

// store saves a changed state and the events of the change. The first state of a watch is its
// baseline and emits nothing (001-AC8).
func (w *Worker) store(ctx context.Context, tx pgx.Tx, j job, state json.RawMessage, f *collect.Fetcher, now time.Time) ([]string, error) {
	state, err := collect.StripNUL(state)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(state)
	hash := hex.EncodeToString(sum[:])
	var prevID int64
	var prev []byte
	var prevHash string
	err = tx.QueryRow(ctx, `SELECT id, state, state_hash FROM watch_states WHERE watch_id = $1 ORDER BY id DESC LIMIT 1`,
		j.id).Scan(&prevID, &prev, &prevHash)
	baseline := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !baseline {
		return nil, err
	}
	if prevHash == hash {
		return nil, nil
	}
	var events []collect.Event
	if !baseline {
		if events, err = w.collectors[j.kind].Diff(prev, state); err != nil {
			return nil, err
		}
		events = collect.WithItems(events, f.Items)
		collect.WithNotes(events, f.Notes)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO watch_states (watch_id, observed_at, state, state_hash) VALUES ($1, $2, $3, $4)`,
		j.id, now, state, hash); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM watch_states WHERE watch_id = $1 AND id NOT IN
		(SELECT id FROM watch_states WHERE watch_id = $1 ORDER BY id DESC LIMIT $2)`, j.id, keepStates); err != nil {
		return nil, err
	}
	return insertEvents(ctx, tx, j.id, prevID, events, now)
}

// maxDedupKey is the longest key stored as is. Longer ones are hashed: a btree row holds 2704 bytes, 28 of them
// overhead here, and a change of label names (from/to JSON in the subject) has no upper bound.
const maxDedupKey = 2048

// insertEvents keys each event on the state it diffs from: replaying the same change (a run retried
// after a crash) inserts nothing new (001-AC9). It returns the types actually inserted.
func insertEvents(ctx context.Context, tx pgx.Tx, watchID string, prevID int64, events []collect.Event, now time.Time) ([]string, error) {
	var inserted []string
	for _, e := range events {
		payload, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, fmt.Errorf("event payload: %w", err)
		}
		// Upstream text (an issue title, a label) may carry NUL, which jsonb refuses.
		if payload, err = collect.StripNUL(payload); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%s:%d", e.Type, e.Subject, prevID)
		if len(key) > maxDedupKey {
			// PostgreSQL compresses index rows, so a repetitive key far past the limit may be stored raw: a replay collapses on it.
			var stored bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE watch_id = $1 AND dedup_key = $2)`, watchID, key).Scan(&stored); err != nil {
				return nil, err
			}
			if stored {
				continue
			}
			sum := sha256.Sum256([]byte(key))
			key = "sha256:" + hex.EncodeToString(sum[:])
		}
		tag, err := tx.Exec(ctx, `INSERT INTO events (watch_id, type, occurred_at, observed_at, payload, dedup_key)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (watch_id, dedup_key) DO NOTHING`,
			watchID, e.Type, e.OccurredAt, now, payload, key)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			inserted = append(inserted, e.Type)
		}
	}
	return inserted, nil
}

// Backoff is the delay after the n-th consecutive failure: interval, 2x, 4x, then 8x at most.
func Backoff(interval time.Duration, failures int) time.Duration {
	return interval << min(max(failures-1, 0), 3)
}

// rateLimitNext keeps a rate-limit wait between a minute (a reset already past) and an hour.
func rateLimitNext(until, now time.Time) time.Time {
	if lo := now.Add(time.Minute); until.Before(lo) {
		return lo
	}
	if hi := now.Add(maxRateLimitWait); until.After(hi) {
		return hi
	}
	return until
}

// truncate fits an error into last_error: NUL dropped (text columns refuse it), 1 KiB at most.
func truncate(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) <= maxErrorLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxErrorLen], "")
}
