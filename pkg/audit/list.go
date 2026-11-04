package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Item is one audit_log row as the API returns it.
type Item struct {
	ID          int64           `json:"id"`
	At          time.Time       `json:"at"`
	ActorUserID *string         `json:"actor_user_id" doc:"Null for system actions"`
	ActorName   string          `json:"actor_name" doc:"Actor's display name, or email when it has none"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type"`
	TargetID    string          `json:"target_id"`
	Before      json.RawMessage `json:"before" doc:"State before the change, secrets redacted; null on create"`
	After       json.RawMessage `json:"after" doc:"State after the change, secrets redacted; null on delete"`
	RequestID   string          `json:"request_id"`
}

// Query selects entries newest first; zero fields do not filter.
type Query struct {
	Viewer     string // user whose visibility applies; required
	Actor      string
	TargetType string
	From, To   time.Time
	Cursor     string
	Limit      int
}

// visible is the one visibility rule (001-AC22): entries the viewer made, or on a watch they can read
// now, or on a credential, channel, rule or delivery (via its channel) they own. Share entries (target "<watch>:<user>") go to the watch owner, and
// to a sharee only for their own current share. Losing access to a watch hides its history.
const visible = `(a.actor_user_id = @viewer
	OR (a.target_type = 'watch' AND a.target_id IN (
		SELECT id::text FROM watches WHERE owner_id = @viewer
		UNION ALL SELECT watch_id::text FROM watch_shares WHERE user_id = @viewer))
	OR (a.target_type = 'watch_share' AND (
		split_part(a.target_id, ':', 1) IN (SELECT id::text FROM watches WHERE owner_id = @viewer)
		OR EXISTS (SELECT 1 FROM watch_shares s WHERE s.user_id = @viewer
			AND a.target_id = s.watch_id::text || ':' || s.user_id::text)))
	OR (a.target_type = 'credential' AND a.target_id IN (SELECT id::text FROM credentials WHERE owner_id = @viewer))
	OR (a.target_type = 'channel' AND a.target_id IN (SELECT id::text FROM channels WHERE owner_id = @viewer))
	OR (a.target_type = 'rule' AND a.target_id IN (SELECT id::text FROM rules WHERE owner_id = @viewer))
	OR (a.target_type = 'delivery' AND a.target_id IN (SELECT d.id::text FROM deliveries d JOIN channels c ON c.id = d.channel_id
		WHERE c.owner_id = @viewer)))`

// Actor is a user who appears as actor in entries visible to the viewer.
type Actor struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name" doc:"Display name, or email when there is none"`
}

// Actors lists the distinct actors of the entries viewer can see, sorted by name.
// ponytail: scans every visible row; add an actors rollup if the log grows past a few million rows.
func Actors(ctx context.Context, pool *pgxpool.Pool, viewer string) ([]Actor, error) {
	rows, err := pool.Query(ctx, `SELECT u.id::text, COALESCE(NULLIF(u.display_name, ''), u.email, '') AS name
		FROM users u WHERE u.id IN (SELECT a.actor_user_id FROM audit_log a WHERE `+visible+`)
		ORDER BY lower(COALESCE(NULLIF(u.display_name, ''), u.email, '')), u.id`, pgx.NamedArgs{"viewer": viewer})
	if err != nil {
		return nil, fmt.Errorf("audit actors: %w", err)
	}
	actors, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Actor])
	if err != nil {
		return nil, fmt.Errorf("audit actors: %w", err)
	}
	return actors, nil
}

// ErrBadCursor is returned for a cursor List did not produce.
var ErrBadCursor = errors.New("invalid cursor")

// Cursor = base64url("<at unix µs>.<id>"): the last row of a page, for keyset pagination on (at, id).
func encodeCursor(at time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d.%d", at.UnixMicro(), id))
}

func decodeCursor(c string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, 0, ErrBadCursor
	}
	us, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return time.Time{}, 0, ErrBadCursor
	}
	micros, err1 := strconv.ParseInt(us, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil {
		return time.Time{}, 0, ErrBadCursor
	}
	return time.UnixMicro(micros), n, nil
}

// List returns one page and the cursor of the next one ("" on the last page).
func List(ctx context.Context, pool *pgxpool.Pool, q Query) ([]Item, string, error) {
	if q.Viewer == "" {
		return nil, "", errors.New("audit list: viewer required")
	}
	args := pgx.NamedArgs{"viewer": q.Viewer, "limit": q.Limit + 1}
	where := []string{visible}
	if q.Actor != "" {
		where = append(where, "a.actor_user_id = @actor")
		args["actor"] = q.Actor
	}
	if q.TargetType != "" {
		where = append(where, "a.target_type = @target_type")
		args["target_type"] = q.TargetType
	}
	if !q.From.IsZero() {
		where = append(where, "a.at >= @from")
		args["from"] = q.From
	}
	if !q.To.IsZero() {
		where = append(where, "a.at < @to")
		args["to"] = q.To
	}
	if q.Cursor != "" {
		at, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		where = append(where, "(a.at, a.id) < (@cursor_at, @cursor_id)")
		args["cursor_at"], args["cursor_id"] = at, id
	}
	rows, err := pool.Query(ctx, `SELECT a.id, a.at, a.actor_user_id::text, COALESCE(NULLIF(u.display_name, ''), u.email, ''),
			a.action, a.target_type, a.target_id, a.before, a.after, a.request_id
		FROM audit_log a LEFT JOIN users u ON u.id = a.actor_user_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY a.at DESC, a.id DESC LIMIT @limit`, args)
	if err != nil {
		return nil, "", fmt.Errorf("audit list: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		err := r.Scan(&it.ID, &it.At, &it.ActorUserID, &it.ActorName, &it.Action, &it.TargetType, &it.TargetID,
			&it.Before, &it.After, &it.RequestID)
		return it, err
	})
	if err != nil {
		return nil, "", fmt.Errorf("audit list: %w", err)
	}
	next := ""
	if len(items) > q.Limit {
		items = items[:q.Limit]
		last := items[len(items)-1]
		next = encodeCursor(last.At, last.ID)
	}
	return items, next, nil
}
