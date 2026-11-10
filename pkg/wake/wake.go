// Package wake wakes the worker-role loops on database changes, over one dedicated LISTEN connection.
package wake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Channel is the NOTIFY channel the work triggers send on, with the written table as payload.
const Channel = "gedour_work"

// Listener fans the gedour_work notifications out to the loops subscribed to each table.
type Listener struct {
	pool *pgxpool.Pool
	log  *zap.Logger
	// Reconnect backoff; tests shorten it.
	backoffBase, backoffCap time.Duration

	mu   sync.Mutex
	subs map[string][]chan struct{}
}

// NewListener returns a Listener on pool; call Subscribe for every loop, then Run.
func NewListener(pool *pgxpool.Pool, log *zap.Logger) *Listener {
	return &Listener{pool: pool, log: log, backoffBase: time.Second, backoffCap: 30 * time.Second, subs: map[string][]chan struct{}{}}
}

// Subscribe returns a channel signalled when one of tables changes, and on every (re)connect.
// Subscribe before Run: a later subscriber misses the wake-all of the first connect.
func (l *Listener) Subscribe(tables ...string) <-chan struct{} {
	// Size 1: a burst of notifications before the loop reads collapses into one wake.
	ch := make(chan struct{}, 1)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range tables {
		l.subs[t] = append(l.subs[t], ch)
	}
	return ch
}

// signal wakes the subscribers of table, or every subscriber when table is "".
func (l *Listener) signal(table string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for t, chs := range l.subs {
		if table != "" && t != table {
			continue
		}
		for _, ch := range chs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Run listens until ctx ends, reconnecting with capped exponential backoff.
func (l *Listener) Run(ctx context.Context) {
	backoff := l.backoffBase
	for {
		listened, err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if listened {
			backoff = l.backoffBase
		}
		l.log.Warn("work_listen_failed", zap.Error(err), zap.Duration("retry_in", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, l.backoffCap)
	}
}

// listen holds one connection out of the pool for LISTEN; it returns on any connection error.
func (l *Listener) listen(ctx context.Context) (listened bool, err error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire listen connection: %w", err)
	}
	// A LISTENing session must not go back to the pool: another user of it would get our notifications.
	defer func() { _ = conn.Hijack().Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return false, fmt.Errorf("listen %s: %w", Channel, err)
	}
	l.log.Info("work_listening")
	// Notifications sent while not listening are lost: every loop checks for work once.
	l.signal("")
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("wait for notification: %w", err)
		}
		l.signal(n.Payload)
	}
}
