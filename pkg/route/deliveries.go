package route

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/watch"
)

// ChannelRef names a channel inside another resource.
type ChannelRef struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Type string `json:"type" enum:"telegram,nextcloud_talk,email,webhook,script"`
}

// RuleRef names a rule inside another resource.
type RuleRef struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
}

// Delivery is one event sent (or being sent) to one channel.
type Delivery struct {
	ID            int64       `json:"id"`
	Status        string      `json:"status" enum:"pending,sent,failed,dead" doc:"failed: a retry is scheduled; dead: no retry left"`
	Attempts      int         `json:"attempts"`
	NextAttemptAt *time.Time  `json:"next_attempt_at" nullable:"true" doc:"Set while pending or failed"`
	LastError     *string     `json:"last_error" nullable:"true"`
	Logs          *string     `json:"logs" nullable:"true" doc:"Script channels: the log lines of the last run"`
	SentAt        *time.Time  `json:"sent_at" nullable:"true"`
	CreatedAt     time.Time   `json:"created_at"`
	Channel       ChannelRef  `json:"channel"`
	Rule          *RuleRef    `json:"rule,omitempty" doc:"Absent once the rule is deleted"`
	Event         watch.Event `json:"event"`
	Watch         WatchRef    `json:"watch"`
}

type deliveryOutput struct {
	Body Delivery
}

type listDeliveriesInput struct {
	ChannelID string `query:"channel_id" format:"uuid" doc:"Only deliveries to this channel"`
	Status    string `query:"status" enum:"pending,sent,failed,dead"`
	Cursor    string `query:"cursor" doc:"next_cursor of the previous page"`
	Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}

type listDeliveriesOutput struct {
	Body struct {
		Items      []Delivery `json:"items" doc:"Newest first"`
		NextCursor string     `json:"next_cursor,omitempty" doc:"Absent on the last page"`
	}
}

type deliveryPath struct {
	ID int64 `path:"id" minimum:"1"`
}

// deliverySelect lists the owner's deliveries of events on watches they can still read.
var deliverySelect = `SELECT d.id, d.status, d.attempts, CASE WHEN d.status IN ('pending', 'failed') THEN d.next_attempt_at END,
		d.last_error, d.logs, d.sent_at, d.created_at, c.id::text, c.name, c.type, r.id::text, r.name,
		e.id, e.type, e.occurred_at, e.observed_at, e.payload - 'body', w.id::text, w.name, w.kind
	FROM deliveries d JOIN channels c ON c.id = d.channel_id
	JOIN events e ON e.id = d.event_id JOIN watches w ON w.id = e.watch_id
	LEFT JOIN rules r ON r.id = d.rule_id
	WHERE c.owner_id = @owner AND ` + watch.ReadableSQL("w", "@owner")

func scanDeliveries(rows pgx.Rows) ([]Delivery, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		var ruleID, ruleName *string
		e := &d.Event
		err := r.Scan(&d.ID, &d.Status, &d.Attempts, &d.NextAttemptAt, &d.LastError, &d.Logs, &d.SentAt, &d.CreatedAt,
			&d.Channel.ID, &d.Channel.Name, &d.Channel.Type, &ruleID, &ruleName,
			&e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &d.Watch.ID, &d.Watch.Name, &d.Watch.Kind)
		if ruleID != nil {
			d.Rule = &RuleRef{ID: *ruleID, Name: *ruleName}
		}
		return d, err
	})
}

func ownDelivery(ctx context.Context, q querier, owner string, id int64) (Delivery, error) {
	rows, err := q.Query(ctx, deliverySelect+" AND d.id = @id", pgx.NamedArgs{"owner": owner, "id": id})
	if err != nil {
		return Delivery{}, err
	}
	ds, err := scanDeliveries(rows)
	if err != nil {
		return Delivery{}, err
	}
	if len(ds) == 0 {
		return Delivery{}, huma.Error404NotFound("delivery not found")
	}
	return ds[0], nil
}

func (s *Service) registerDeliveries(api huma.API) {
	tags := []string{"deliveries"}
	huma.Register(api, huma.Operation{
		OperationID: "list-deliveries", Method: http.MethodGet, Path: "/api/v1/deliveries",
		Summary: "Deliveries to the caller's channels, newest first", Tags: tags,
	}, s.listDeliveries)
	huma.Register(api, huma.Operation{
		OperationID: "retry-delivery", Method: http.MethodPost, Path: "/api/v1/deliveries/{id}/retry",
		Summary: "Send a failed or dead delivery again now", Tags: tags,
	}, s.retryDelivery)
}

func (s *Service) listDeliveries(ctx context.Context, in *listDeliveriesInput) (*listDeliveriesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	sql := deliverySelect
	args := pgx.NamedArgs{"owner": me, "limit": in.Limit + 1}
	if in.ChannelID != "" {
		sql += " AND d.channel_id = @channel"
		args["channel"] = in.ChannelID
	}
	if in.Status != "" {
		sql += " AND d.status = @status"
		args["status"] = in.Status
	}
	if in.Cursor != "" {
		before, err := strconv.ParseInt(in.Cursor, 10, 64)
		if err != nil || before <= 0 {
			return nil, huma.Error400BadRequest("invalid cursor", &huma.ErrorDetail{Location: "query.cursor", Message: "not a cursor this API returned", Value: in.Cursor})
		}
		sql += " AND d.id < @before"
		args["before"] = before
	}
	rows, err := s.pool.Query(ctx, sql+" ORDER BY d.id DESC LIMIT @limit", args)
	if err != nil {
		return nil, s.fail("list-deliveries", err)
	}
	items, err := scanDeliveries(rows)
	if err != nil {
		return nil, s.fail("list-deliveries", err)
	}
	out := &listDeliveriesOutput{}
	if len(items) > in.Limit {
		items = items[:in.Limit]
		out.Body.NextCursor = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	out.Body.Items = items
	if out.Body.Items == nil {
		out.Body.Items = []Delivery{}
	}
	return out, nil
}

// retryDelivery makes a failed or dead delivery due now; attempts are kept, so a dead one gets one more.
func (s *Service) retryDelivery(ctx context.Context, in *deliveryPath) (*deliveryOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	var after Delivery
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT d.status FROM deliveries d JOIN channels c ON c.id = d.channel_id
			WHERE d.id = $1 AND c.owner_id = $2 FOR UPDATE OF d`, in.ID, me).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("delivery not found")
		}
		if err != nil {
			return err
		}
		if status != "failed" && status != "dead" {
			return huma.Error409Conflict("only a failed or dead delivery can be retried; this one is " + status)
		}
		before, err := ownDelivery(ctx, tx, me, in.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = 'pending', next_attempt_at = now() WHERE id = $1`, in.ID); err != nil {
			return err
		}
		if after, err = ownDelivery(ctx, tx, me, in.ID); err != nil {
			return err
		}
		id := strconv.FormatInt(in.ID, 10)
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "delivery.retried", TargetType: "delivery", TargetID: id,
			Before: deliveryAudit(before), After: deliveryAudit(after)})
	})
	if err != nil {
		return nil, s.fail("retry-delivery", err)
	}
	return &deliveryOutput{Body: after}, nil
}

// deliveryAudit keeps the event payload out of the audit log: it is upstream data, not configuration.
func deliveryAudit(d Delivery) map[string]any {
	return map[string]any{"id": d.ID, "status": d.Status, "attempts": d.Attempts, "channel_id": d.Channel.ID, "event_id": d.Event.ID}
}
