package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// Event is one change the worker found on a watch.
type Event struct {
	ID         int64          `json:"id"`
	Type       string         `json:"type" doc:"e.g. release.published, pr.state_changed, tag.created"`
	OccurredAt *time.Time     `json:"occurred_at" nullable:"true" doc:"Upstream time of the change, when the upstream gives one"`
	ObservedAt time.Time      `json:"observed_at" doc:"When gedour saw the change"`
	Payload    map[string]any `json:"payload" doc:"Per-type details: tag, from/to, url, cves... The release notes (body) only come with the inbox list and to channels"`
}

// StateSnapshot is one stored state of a watch.
type StateSnapshot struct {
	ID         int64           `json:"id"`
	ObservedAt time.Time       `json:"observed_at"`
	State      json.RawMessage `json:"state" doc:"Normalized upstream state; its shape depends on the watch kind"`
}

type listEventsInput struct {
	ID     string `path:"id" format:"uuid"`
	Cursor string `query:"cursor" doc:"next_cursor of the previous page"`
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}

type listEventsOutput struct {
	Body struct {
		Items      []Event `json:"items" doc:"Newest first"`
		NextCursor string  `json:"next_cursor,omitempty" doc:"Absent on the last page"`
	}
}

type listStatesInput struct {
	ID    string `path:"id" format:"uuid"`
	Limit int    `query:"limit" minimum:"1" maximum:"50" default:"20"`
}

type listStatesOutput struct {
	Body struct {
		Items []StateSnapshot `json:"items" doc:"Latest first; the first item is the current state"`
	}
}

func (s *Service) registerActivity(api huma.API) {
	tags := []string{"watches"}
	huma.Register(api, huma.Operation{
		OperationID: "list-watch-events", Method: http.MethodGet, Path: "/api/v1/watches/{id}/events",
		Summary: "Changes found on a watch, newest first", Tags: tags,
	}, s.listEvents)
	huma.Register(api, huma.Operation{
		OperationID: "list-watch-states", Method: http.MethodGet, Path: "/api/v1/watches/{id}/states",
		Summary: "State history of a watch, latest first (the last 50 are kept)", Tags: tags,
	}, s.listStates)
	huma.Register(api, huma.Operation{
		OperationID: "run-watch", Method: http.MethodPost, Path: "/api/v1/watches/{id}/run",
		Summary: "Schedule a run now", Tags: tags, DefaultStatus: http.StatusAccepted,
	}, s.runWatch)
	huma.Register(api, huma.Operation{
		OperationID: "run-all-watches", Method: http.MethodPost, Path: "/api/v1/watches/run",
		Summary: "Schedule a run now of every watch the caller can run", Tags: tags, DefaultStatus: http.StatusAccepted,
	}, s.runAll)
}

type runAllOutput struct {
	Body struct {
		Scheduled int `json:"scheduled" doc:"Watches made due now: owned or shared read-write, paused ones left out"`
	}
}

// runAll is run-now on every watch the caller may run, in one statement and one audit row per watch.
func (s *Service) runAll(ctx context.Context, _ *struct{}) (*runAllOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	out := &runAllOutput{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Locked in id order (AGENTS.md), as a purge locks the owner's watches.
		rows, err := tx.Query(ctx, `UPDATE watches w SET next_run_at = now()
			WHERE w.id IN (SELECT x.id FROM watches x WHERE NOT x.paused AND (x.owner_id = $1 OR EXISTS (SELECT 1 FROM watch_shares s
				WHERE s.watch_id = x.id AND s.user_id = $1 AND s.permission = 'rw')) ORDER BY x.id FOR NO KEY UPDATE)
			RETURNING w.id::text`, me)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		// The same audit row as a single run-now: the watch's spec.
		rows, err = tx.Query(ctx, watchSelect+" AND w.id::text = ANY(@ids)", pgx.NamedArgs{"viewer": me, "ids": ids})
		if err != nil {
			return err
		}
		ws, err := scanWatches(rows)
		if err != nil {
			return err
		}
		for _, w := range ws {
			if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.run_requested", TargetType: "watch", TargetID: w.ID, After: w.Spec}); err != nil {
				return err
			}
		}
		out.Body.Scheduled = len(ids)
		return nil
	})
	if err != nil {
		return nil, s.fail("run-all-watches", err)
	}
	return out, nil
}

func (s *Service) listEvents(ctx context.Context, in *listEventsInput) (*listEventsOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := authorize(ctx, s.pool, me, in.ID, levelRead, false); err != nil {
		return nil, s.fail("list-watch-events", err)
	}
	before := int64(1<<63 - 1)
	if in.Cursor != "" {
		if before, err = strconv.ParseInt(in.Cursor, 10, 64); err != nil || before <= 0 {
			return nil, huma.Error400BadRequest("invalid cursor", &huma.ErrorDetail{Location: "query.cursor", Message: "not a cursor this API returned", Value: in.Cursor})
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id, type, occurred_at, observed_at, payload - 'body' FROM events
		WHERE watch_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`, in.ID, before, in.Limit+1)
	if err != nil {
		return nil, s.fail("list-watch-events", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Event])
	if err != nil {
		return nil, s.fail("list-watch-events", err)
	}
	out := &listEventsOutput{}
	if len(items) > in.Limit {
		items = items[:in.Limit]
		out.Body.NextCursor = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	out.Body.Items = items
	if out.Body.Items == nil {
		out.Body.Items = []Event{}
	}
	return out, nil
}

func (s *Service) listStates(ctx context.Context, in *listStatesInput) (*listStatesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := authorize(ctx, s.pool, me, in.ID, levelRead, false); err != nil {
		return nil, s.fail("list-watch-states", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, observed_at, state FROM watch_states WHERE watch_id = $1 ORDER BY id DESC LIMIT $2`,
		in.ID, in.Limit)
	if err != nil {
		return nil, s.fail("list-watch-states", err)
	}
	out := &listStatesOutput{}
	if out.Body.Items, err = pgx.CollectRows(rows, pgx.RowToStructByPos[StateSnapshot]); err != nil {
		return nil, s.fail("list-watch-states", err)
	}
	if out.Body.Items == nil {
		out.Body.Items = []StateSnapshot{}
	}
	return out, nil
}

// runWatch makes the watch due now; any worker replica picks it up at its next poll.
func (s *Service) runWatch(ctx context.Context, in *idPath) (*watchOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	var w Watch
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := authorize(ctx, tx, me, in.ID, levelWrite, true)
		if err != nil {
			return err
		}
		if before.Paused {
			return huma.Error409Conflict("the watch is paused: resume it to run it")
		}
		if _, err := tx.Exec(ctx, `UPDATE watches SET next_run_at = now() WHERE id = $1`, in.ID); err != nil {
			return err
		}
		if w, err = authorize(ctx, tx, me, in.ID, levelRead, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.run_requested", TargetType: "watch", TargetID: in.ID, After: w.Spec})
	})
	if err != nil {
		return nil, s.fail("run-watch", err)
	}
	return &watchOutput{Body: w}, nil
}
