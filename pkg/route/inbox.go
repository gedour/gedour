package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/watch"
)

// InboxChannel is the NOTIFY channel the router signals new inbox items on; the payload is the user ID.
const InboxChannel = "gedour_inbox"

// InboxItem is one event in a user's inbox.
type InboxItem struct {
	ID         int64       `json:"id"`
	CreatedAt  time.Time   `json:"created_at"`
	ReadAt     *time.Time  `json:"read_at" nullable:"true"`
	ArchivedAt *time.Time  `json:"archived_at" nullable:"true"`
	Priority   string      `json:"priority" enum:"normal,high" doc:"Set once, when the event was routed"`
	Event      watch.Event `json:"event"`
	Watch      WatchRef    `json:"watch"`
}

type listInboxInput struct {
	State    string `query:"state" enum:"unread,all,archived" default:"all" doc:"unread: not read, not archived; all: not archived"`
	Priority string `query:"priority" enum:"normal,high" doc:"Only items of this priority; absent: all"`
	Cursor   string `query:"cursor" doc:"next_cursor of the previous page"`
	Limit    int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

type listInboxOutput struct {
	Body struct {
		Items      []InboxItem `json:"items" doc:"Newest first"`
		NextCursor string      `json:"next_cursor,omitempty" doc:"Absent on the last page"`
	}
}

type countOutput struct {
	Body struct {
		Unread int `json:"unread" doc:"Unread items not archived"`
	}
}

type bulkInput struct {
	Body struct {
		IDs []int64 `json:"ids" nullable:"false" minItems:"1" maxItems:"500" doc:"Inbox item IDs; others' items are ignored"`
	}
}

type bulkOutput struct {
	Body struct {
		Updated int `json:"updated" doc:"Items whose state changed"`
		Unread  int `json:"unread"`
	}
}

// Inbox items show only while their watch is readable: a revoked share hides what it brought.
var unreadCount = `SELECT count(*) FROM inbox_items i JOIN events e ON e.id = i.event_id JOIN watches w ON w.id = e.watch_id
	WHERE i.user_id = $1 AND i.read_at IS NULL AND i.archived_at IS NULL AND ` + watch.ReadableSQL("w", "$1::uuid")

func (s *Service) registerInbox(api huma.API) {
	tags := []string{"inbox"}
	huma.Register(api, huma.Operation{
		OperationID: "list-inbox", Method: http.MethodGet, Path: "/api/v1/inbox",
		Summary: "The caller's inbox, newest first", Tags: tags,
	}, s.listInbox)
	huma.Register(api, huma.Operation{
		OperationID: "count-inbox", Method: http.MethodGet, Path: "/api/v1/inbox/count",
		Summary: "Unread items in the caller's inbox", Tags: tags,
	}, func(ctx context.Context, _ *struct{}) (*countOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		out := &countOutput{}
		if err := s.pool.QueryRow(ctx, unreadCount, me).Scan(&out.Body.Unread); err != nil {
			return nil, s.fail("count-inbox", err)
		}
		return out, nil
	})
	for _, op := range []struct{ name, set string }{
		{"read", "read_at = now()"},
		{"unread", "read_at = NULL"},
		{"archive", "archived_at = now()"},
		{"unarchive", "archived_at = NULL"},
	} {
		// Only rows whose state changes: updated counts real changes, and times of the first read stay.
		cond := map[string]string{"read": "read_at IS NULL", "unread": "read_at IS NOT NULL",
			"archive": "archived_at IS NULL", "unarchive": "archived_at IS NOT NULL"}[op.name]
		huma.Register(api, huma.Operation{
			OperationID: op.name + "-inbox", Method: http.MethodPost, Path: "/api/v1/inbox/" + op.name,
			Summary: "Mark the caller's items " + op.name, Tags: tags,
		}, func(ctx context.Context, in *bulkInput) (*bulkOutput, error) {
			return s.bulk(ctx, op.name, op.set, cond, in.Body.IDs)
		})
	}
}

func (s *Service) listInbox(ctx context.Context, in *listInboxInput) (*listInboxOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	sql := `SELECT i.id, i.created_at, i.read_at, i.archived_at, i.priority, e.id, e.type, e.occurred_at, e.observed_at, e.payload,
			w.id::text, w.name, w.kind
		FROM inbox_items i JOIN events e ON e.id = i.event_id JOIN watches w ON w.id = e.watch_id
		WHERE i.user_id = @me AND ` + watch.ReadableSQL("w", "@me")
	args := pgx.NamedArgs{"me": me, "limit": in.Limit + 1}
	switch in.State {
	case "unread":
		sql += " AND i.read_at IS NULL AND i.archived_at IS NULL"
	case "archived":
		sql += " AND i.archived_at IS NOT NULL"
	default:
		sql += " AND i.archived_at IS NULL"
	}
	// Literals: a generic plan with a parameter could not use the partial index on high items.
	switch in.Priority {
	case priorityHigh:
		sql += " AND i.priority = 'high'"
	case priorityNormal:
		sql += " AND i.priority = 'normal'"
	}
	if in.Cursor != "" {
		before, err := strconv.ParseInt(in.Cursor, 10, 64)
		if err != nil || before <= 0 {
			return nil, huma.Error400BadRequest("invalid cursor", &huma.ErrorDetail{Location: "query.cursor", Message: "not a cursor this API returned", Value: in.Cursor})
		}
		sql += " AND i.id < @before"
		args["before"] = before
	}
	rows, err := s.pool.Query(ctx, sql+" ORDER BY i.id DESC LIMIT @limit", args)
	if err != nil {
		return nil, s.fail("list-inbox", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (InboxItem, error) {
		var it InboxItem
		e := &it.Event
		err := r.Scan(&it.ID, &it.CreatedAt, &it.ReadAt, &it.ArchivedAt, &it.Priority, &e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload,
			&it.Watch.ID, &it.Watch.Name, &it.Watch.Kind)
		return it, err
	})
	if err != nil {
		return nil, s.fail("list-inbox", err)
	}
	out := &listInboxOutput{}
	if len(items) > in.Limit {
		items = items[:in.Limit]
		out.Body.NextCursor = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	out.Body.Items = items
	if out.Body.Items == nil {
		out.Body.Items = []InboxItem{}
	}
	return out, nil
}

// bulk changes the caller's items among ids and tells their other open tabs the new count. 001-AC20.
func (s *Service) bulk(ctx context.Context, op, set, cond string, ids []int64) (*bulkOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	out := &bulkOutput{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE inbox_items SET `+set+` WHERE user_id = $1 AND id = ANY($2) AND `+cond, me, ids)
		if err != nil {
			return err
		}
		out.Body.Updated = int(tag.RowsAffected())
		if out.Body.Updated > 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, InboxChannel, me); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, unreadCount, me).Scan(&out.Body.Unread)
	})
	if err != nil {
		return nil, s.fail(op+"-inbox", err)
	}
	return out, nil
}

// stream is the inbox's Server-Sent Events feed: an "inbox" event with the unread count on connect and
// whenever the count may have changed, a comment every Heartbeat. 001-AC19.
func (s *Service) stream(w http.ResponseWriter, r *http.Request) {
	me, ok := s.viewer(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	sub, err := s.hub.subscribe(me)
	if err != nil {
		w.Header().Set("Retry-After", "30")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer s.hub.unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// Proxies (nginx and alikes) must not buffer the stream.
	h.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	write := func(msg string) bool {
		// Each write moves the deadline: the server's WriteTimeout would otherwise cut the stream.
		_ = rc.SetWriteDeadline(time.Now().Add(s.Heartbeat + 10*time.Second))
		if _, err := fmt.Fprint(w, msg); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	count := func() bool {
		var n int
		if err := s.pool.QueryRow(r.Context(), unreadCount, me).Scan(&n); err != nil {
			if !errors.Is(r.Context().Err(), context.Canceled) {
				s.log.Warn("inbox_stream_count_failed", zap.Error(err))
			}
			return false
		}
		b, _ := json.Marshal(map[string]int{"unread": n})
		return write("event: inbox\ndata: " + string(b) + "\n\n")
	}
	// retry: the browser's reconnect delay after a drop.
	if !write("retry: 3000\n\n") || !count() {
		return
	}
	tick := time.NewTicker(s.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.done:
			return
		case <-sub.ch:
			if !count() {
				return
			}
		case <-tick.C:
			if !s.alive(r) {
				write("event: signed_out\ndata: {}\n\n")
				return
			}
			if !write(": ping\n\n") {
				return
			}
		}
	}
}
