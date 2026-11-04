package route

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Stream limits of one web replica: a runaway tab cannot exhaust connections.
const (
	MaxStreamsPerUser = 10
	MaxStreams        = 2000
)

var errTooManyStreams = errors.New("too many open inbox streams")

type subscriber struct {
	user string
	// ch holds at most one pending wake-up: several notifications before a read collapse into one recount.
	ch chan struct{}
}

// Hub fans the router's NOTIFYs out to this replica's inbox streams, over one dedicated LISTEN connection.
type Hub struct {
	pool    *pgxpool.Pool
	log     *zap.Logger
	metrics *Metrics
	done    chan struct{}

	mu    sync.Mutex
	users map[string]map[*subscriber]struct{}
	total int
}

func newHub(pool *pgxpool.Pool, log *zap.Logger, m *Metrics) *Hub {
	return &Hub{pool: pool, log: log, metrics: m, done: make(chan struct{}), users: map[string]map[*subscriber]struct{}{}}
}

func (h *Hub) subscribe(user string) (*subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= MaxStreams || len(h.users[user]) >= MaxStreamsPerUser {
		return nil, errTooManyStreams
	}
	s := &subscriber{user: user, ch: make(chan struct{}, 1)}
	if h.users[user] == nil {
		h.users[user] = map[*subscriber]struct{}{}
	}
	h.users[user][s] = struct{}{}
	h.total++
	h.metrics.subscribers.Set(float64(h.total))
	return s, nil
}

func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.users[s.user][s]; !ok {
		return
	}
	delete(h.users[s.user], s)
	if len(h.users[s.user]) == 0 {
		delete(h.users, s.user)
	}
	h.total--
	h.metrics.subscribers.Set(float64(h.total))
}

// wake signals the streams of user, or of everyone when user is "" (after a reconnect, when
// notifications may have been missed).
func (h *Hub) wake(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for u, subs := range h.users {
		if user != "" && u != user {
			continue
		}
		for s := range subs {
			select {
			case s.ch <- struct{}{}:
			default:
			}
		}
	}
}

// Run listens until ctx ends, reconnecting with capped exponential backoff; then it closes every stream.
func (h *Hub) Run(ctx context.Context) {
	defer close(h.done)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		h.log.Warn("inbox_listen_lost", zap.Error(err), zap.Duration("retry_in", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// listen holds one connection out of the pool for LISTEN; it returns on any connection error.
func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// A LISTENing session must not go back to the pool: another user of it would get our notifications.
	defer func() { _ = conn.Hijack().Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN "+InboxChannel); err != nil {
		return err
	}
	h.log.Info("inbox_listening")
	// Notifications sent before LISTEN took effect are lost: every stream recounts once.
	h.wake("")
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.wake(n.Payload)
	}
}
