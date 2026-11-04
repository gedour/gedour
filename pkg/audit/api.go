package audit

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

type listInput struct {
	Actor      string    `query:"actor" format:"uuid" doc:"Only entries by this user"`
	TargetType string    `query:"target_type" maxLength:"64" doc:"Only entries on this target type, e.g. user"`
	From       time.Time `query:"from" doc:"Inclusive lower bound, RFC 3339"`
	To         time.Time `query:"to" doc:"Exclusive upper bound, RFC 3339"`
	Cursor     string    `query:"cursor" maxLength:"128" doc:"next_cursor of the previous page"`
	Limit      int       `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

type listOutput struct {
	Body struct {
		Items      []Item `json:"items"`
		NextCursor string `json:"next_cursor,omitempty" doc:"Absent on the last page"`
	}
}

// Register mounts GET /api/v1/audit; viewer returns the authenticated user's ID.
func Register(api huma.API, pool *pgxpool.Pool, log *zap.Logger, viewer func(context.Context) (string, bool)) {
	huma.Register(api, huma.Operation{
		OperationID: "list-audit",
		Method:      http.MethodGet,
		Path:        "/api/v1/audit",
		Summary:     "Audit entries visible to the caller, newest first",
		Tags:        []string{"audit"},
	}, func(ctx context.Context, in *listInput) (*listOutput, error) {
		id, ok := viewer(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("not signed in")
		}
		items, next, err := List(ctx, pool, Query{
			Viewer: id, Actor: in.Actor, TargetType: in.TargetType,
			From: in.From, To: in.To, Cursor: in.Cursor, Limit: in.Limit,
		})
		if errors.Is(err, ErrBadCursor) {
			return nil, huma.Error400BadRequest("invalid cursor")
		}
		if err != nil {
			log.Error("audit_list_failed", zap.Error(err))
			return nil, huma.Error500InternalServerError("could not list audit entries")
		}
		out := &listOutput{}
		out.Body.Items = items
		if out.Body.Items == nil {
			out.Body.Items = []Item{}
		}
		out.Body.NextCursor = next
		return out, nil
	})
}
