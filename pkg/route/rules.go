package route

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/filter"
	"github.com/gedour/gedour/pkg/watch"
)

// MaxRuleChannels caps the channels of one rule.
const MaxRuleChannels = 20

// previewEvents is how many recent events a preview evaluates.
const previewEvents = 20

const (
	priorityNormal = "normal"
	priorityHigh   = "high"
)

// Rule is a routing rule: events of its scope that pass its filter go to the owner's inbox and channels.
type Rule struct {
	ID         string    `json:"id" format:"uuid"`
	Name       string    `json:"name"`
	WatchID    *string   `json:"watch_id" format:"uuid" nullable:"true" doc:"Null: every watch of the owner"`
	WatchName  *string   `json:"watch_name" nullable:"true" doc:"Null with a watch_id: the owner no longer owns that watch"`
	Filter     string    `json:"filter" doc:"expr expression over event and watch; empty matches every event"`
	Enabled    bool      `json:"enabled"`
	Priority   string    `json:"priority" enum:"normal,high" doc:"high marks the inbox items of the events this rule takes; set at routing, existing items keep theirs"`
	ChannelIDs []string  `json:"channel_ids" doc:"Channels besides the inbox, which is always on"`
	Delivery   string    `json:"delivery" enum:"immediate,digest" doc:"digest: Telegram, Nextcloud Talk, email and Web Push channels get this rule's events in one message at the at times; webhook and script channels still get each at once"`
	At         []string  `json:"at" doc:"Digest: the local times of day, HH:MM, sorted; empty when immediate"`
	TZ         string    `json:"tz" doc:"Digest: the IANA time zone of at; empty when immediate"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type ruleOutput struct {
	Body Rule
}

type listRulesOutput struct {
	Body struct {
		Items []Rule `json:"items" doc:"Sorted by name"`
	}
}

type createRuleInput struct {
	Body struct {
		Name       string   `json:"name" minLength:"1" maxLength:"200"`
		WatchID    string   `json:"watch_id,omitempty" doc:"One of the caller's watches; empty for all of them"`
		Filter     string   `json:"filter,omitempty" maxLength:"2000"`
		Enabled    *bool    `json:"enabled,omitempty" doc:"Defaults to true"`
		Priority   string   `json:"priority,omitempty" enum:"normal,high" default:"normal"`
		ChannelIDs []string `json:"channel_ids,omitempty" maxItems:"20" doc:"The caller's channels"`
		Delivery   string   `json:"delivery,omitempty" enum:"immediate,digest" default:"immediate" doc:"digest needs at, and priority normal"`
		At         []string `json:"at,omitempty" doc:"Digest only: 1 to 24 local times HH:MM"`
		TZ         string   `json:"tz,omitempty" doc:"Digest only: IANA time zone, default UTC"`
	}
}

type updateRuleInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name       *string  `json:"name,omitempty" minLength:"1" maxLength:"200"`
		WatchID    *string  `json:"watch_id,omitempty" doc:"Empty string: all the caller's watches"`
		Filter     *string  `json:"filter,omitempty" maxLength:"2000"`
		Enabled    *bool    `json:"enabled,omitempty"`
		Priority   *string  `json:"priority,omitempty" enum:"normal,high"`
		ChannelIDs []string `json:"channel_ids,omitempty" maxItems:"20" doc:"Replaces the list; [] keeps the inbox only"`
		Delivery   *string  `json:"delivery,omitempty" enum:"immediate,digest" doc:"immediate clears at and tz"`
		At         []string `json:"at,omitempty" doc:"Digest only: replaces the times"`
		TZ         *string  `json:"tz,omitempty" doc:"Digest only: empty string for UTC"`
	}
}

type previewInput struct {
	Body struct {
		WatchID string `json:"watch_id,omitempty" doc:"Scope like a rule's; empty for all the caller's watches"`
		Filter  string `json:"filter" maxLength:"2000"`
	}
}

// WatchRef names a watch inside another resource.
type WatchRef struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Kind string `json:"kind" enum:"github_repo,github_pr,github_issue,github_discussion,gitlab_release,gitlab_mr,oci_tags"`
}

// PreviewItem is the outcome of a filter on one recent event.
type PreviewItem struct {
	Event   watch.Event `json:"event"`
	Watch   WatchRef    `json:"watch"`
	Matched bool        `json:"matched"`
	Error   string      `json:"error,omitempty" doc:"Evaluation error; the rule would treat the event as not matching"`
}

type referenceOutput struct {
	Body filter.Ref
}

type previewOutput struct {
	Body struct {
		Items []PreviewItem `json:"items" doc:"The last 20 events in scope, newest first"`
	}
}

// ruleSelect names a rule's watch only while the rule's owner owns it.
var ruleSelect = `SELECT r.id::text, r.name, r.watch_id::text, w.name, r.filter, r.enabled, r.priority,
		COALESCE((SELECT array_agg(rc.channel_id::text ORDER BY rc.channel_id) FROM rule_channels rc WHERE rc.rule_id = r.id), '{}'),
		r.delivery, COALESCE(r.digest_at, '{}'), COALESCE(r.digest_tz, ''), r.created_at, r.updated_at
	FROM rules r LEFT JOIN watches w ON w.id = r.watch_id AND ` + watch.ReadableSQL("w", "@owner") + `
	WHERE r.owner_id = @owner`

func scanRules(rows pgx.Rows) ([]Rule, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Rule, error) {
		var x Rule
		err := r.Scan(&x.ID, &x.Name, &x.WatchID, &x.WatchName, &x.Filter, &x.Enabled, &x.Priority, &x.ChannelIDs, &x.Delivery, &x.At, &x.TZ, &x.CreatedAt, &x.UpdatedAt)
		return x, err
	})
}

func ownRule(ctx context.Context, q querier, owner, id string, lock bool) (Rule, error) {
	sql := ruleSelect + " AND r.id = @id"
	if lock {
		sql += " FOR UPDATE OF r"
	}
	rows, err := q.Query(ctx, sql, pgx.NamedArgs{"owner": owner, "id": id})
	if err != nil {
		return Rule{}, err
	}
	rs, err := scanRules(rows)
	if err != nil {
		return Rule{}, err
	}
	if len(rs) == 0 {
		return Rule{}, huma.Error404NotFound("rule not found")
	}
	return rs[0], nil
}

// filterError is the 400 of a filter that does not compile, its position in the detail's value.
func filterError(src string) error {
	_, err := filter.Compile(src)
	var fe *filter.Error
	if !errors.As(err, &fe) {
		return nil
	}
	return &huma.ErrorDetail{Location: "body.filter", Message: fe.Error(), Value: map[string]int{"line": fe.Line, "column": fe.Column}}
}

// ruleSchedule validates a rule's delivery against its priority: nil for immediate (012-AC13, 012-AC14).
func ruleSchedule(delivery, priority string, at []string, tz string) (*schedule, []error) {
	if delivery != deliveryDigest {
		var errs []error
		if len(at) > 0 {
			errs = append(errs, &huma.ErrorDetail{Location: "body.at", Message: "only with delivery digest", Value: at})
		}
		if tz != "" {
			errs = append(errs, &huma.ErrorDetail{Location: "body.tz", Message: "only with delivery digest", Value: tz})
		}
		return nil, errs
	}
	s, errs := checkSchedule(at, tz)
	if priority == priorityHigh {
		errs = append(errs, &huma.ErrorDetail{Location: "body.delivery", Message: "a high priority rule already sends at once: digest needs priority normal", Value: delivery})
	}
	return s, errs
}

// scheduleArgs are a schedule's digest_at and digest_tz column values, NULL for immediate.
func scheduleArgs(s *schedule) (at []string, tz *string) {
	if s == nil {
		return nil, nil
	}
	return s.At, &s.TZ
}

// checkWatch verifies owner owns the watch.
func checkWatch(ctx context.Context, tx pgx.Tx, owner, id string) error {
	detail := &huma.ErrorDetail{Location: "body.watch_id", Message: "no such watch among your watches", Value: id}
	if uuid.Validate(id) != nil {
		return huma.Error400BadRequest("invalid rule", detail)
	}
	// KEY SHARE: a concurrent delete of the watch waits for this rule to land, then cascades it.
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM watches w WHERE w.id = $1 AND `+watch.ReadableSQL("w", "$2::uuid")+` FOR KEY SHARE OF w`,
		id, owner).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return huma.Error400BadRequest("invalid rule", detail)
	}
	return err
}

// setChannels replaces the rule's channels, each one of owner's.
func setChannels(ctx context.Context, tx pgx.Tx, owner, ruleID string, ids []string) error {
	var errs []error
	seen := map[string]bool{}
	var keep []string
	for i, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		bad := &huma.ErrorDetail{Location: "body.channel_ids[" + strconv.Itoa(i) + "]", Message: "no such channel among yours", Value: id}
		if uuid.Validate(id) != nil {
			errs = append(errs, bad)
			continue
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1 AND owner_id = $2 FOR KEY SHARE)`, id, owner).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			errs = append(errs, bad)
			continue
		}
		keep = append(keep, id)
	}
	if len(errs) > 0 {
		return huma.Error400BadRequest("invalid rule", errs...)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rule_channels WHERE rule_id = $1`, ruleID); err != nil {
		return err
	}
	if len(keep) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO rule_channels (rule_id, channel_id) SELECT $1, unnest($2::uuid[])`, ruleID, keep)
	return err
}

func (s *Service) registerRules(api huma.API) {
	tags := []string{"rules"}
	huma.Register(api, huma.Operation{
		OperationID: "list-rules", Method: http.MethodGet, Path: "/api/v1/rules",
		Summary: "The caller's routing rules", Tags: tags,
	}, s.listRules)
	huma.Register(api, huma.Operation{
		OperationID: "get-rule", Method: http.MethodGet, Path: "/api/v1/rules/{id}",
		Summary: "One of the caller's rules", Tags: tags,
	}, func(ctx context.Context, in *idPath) (*ruleOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		r, err := ownRule(ctx, s.pool, me, in.ID, false)
		if err != nil {
			return nil, s.fail("get-rule", err)
		}
		return &ruleOutput{Body: r}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "create-rule", Method: http.MethodPost, Path: "/api/v1/rules",
		Summary: "Create a rule; its filter is compiled first", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.createRule)
	huma.Register(api, huma.Operation{
		OperationID: "update-rule", Method: http.MethodPatch, Path: "/api/v1/rules/{id}",
		Summary: "Change a rule", Tags: tags,
	}, s.updateRule)
	huma.Register(api, huma.Operation{
		OperationID: "delete-rule", Method: http.MethodDelete, Path: "/api/v1/rules/{id}",
		Summary: "Delete a rule", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteRule)
	huma.Register(api, huma.Operation{
		OperationID: "get-filter-reference", Method: http.MethodGet, Path: "/api/v1/filters/reference",
		Summary: "What a filter may use, and its limits", Tags: tags,
	}, func(ctx context.Context, _ *struct{}) (*referenceOutput, error) {
		if _, err := s.me(ctx); err != nil {
			return nil, err
		}
		return &referenceOutput{Body: filter.Reference()}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "preview-rule", Method: http.MethodPost, Path: "/api/v1/rules/preview",
		Summary: "Evaluate a filter against the last 20 events in scope, without saving", Tags: tags,
	}, s.previewRule)
}

func (s *Service) listRules(ctx context.Context, _ *struct{}) (*listRulesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, ruleSelect+" ORDER BY lower(r.name), r.id", pgx.NamedArgs{"owner": me})
	if err != nil {
		return nil, s.fail("list-rules", err)
	}
	out := &listRulesOutput{}
	if out.Body.Items, err = scanRules(rows); err != nil {
		return nil, s.fail("list-rules", err)
	}
	if out.Body.Items == nil {
		out.Body.Items = []Rule{}
	}
	return out, nil
}

func ruleNameError(name string) error {
	return huma.Error409Conflict("a rule with this name exists",
		&huma.ErrorDetail{Location: "body.name", Message: "you already have a rule with this name", Value: name})
}

func (s *Service) createRule(ctx context.Context, in *createRuleInput) (*ruleOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	var errs []error
	name := strings.TrimSpace(b.Name)
	if name == "" {
		errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
	}
	src := strings.TrimSpace(b.Filter)
	if fe := filterError(src); fe != nil {
		errs = append(errs, fe)
	}
	sched, serrs := ruleSchedule(b.Delivery, b.Priority, b.At, b.TZ)
	if errs = append(errs, serrs...); len(errs) > 0 {
		return nil, huma.Error400BadRequest("invalid rule", errs...)
	}
	at, tz := scheduleArgs(sched)
	enabled := b.Enabled == nil || *b.Enabled
	var r Rule
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := underCap(ctx, tx, me, "rules", "rules", MaxRules); err != nil {
			return err
		}
		var watchID *string
		if b.WatchID != "" {
			if err := checkWatch(ctx, tx, me, b.WatchID); err != nil {
				return err
			}
			watchID = &b.WatchID
		}
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO rules (owner_id, name, watch_id, filter, enabled, priority, delivery, digest_at, digest_tz)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text`,
			me, name, watchID, src, enabled, b.Priority, b.Delivery, at, tz).Scan(&id)
		if isUniqueViolation(err) {
			return ruleNameError(name)
		}
		if err != nil {
			return err
		}
		if err := setChannels(ctx, tx, me, id, b.ChannelIDs); err != nil {
			return err
		}
		if r, err = ownRule(ctx, tx, me, id, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "rule.created", TargetType: "rule", TargetID: id, After: r})
	})
	if err != nil {
		return nil, s.fail("create-rule", err)
	}
	return &ruleOutput{Body: r}, nil
}

func (s *Service) updateRule(ctx context.Context, in *updateRuleInput) (*ruleOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	var after Rule
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownRule(ctx, tx, me, in.ID, true)
		if err != nil {
			return err
		}
		next := before
		var errs []error
		if b.Name != nil {
			if next.Name = strings.TrimSpace(*b.Name); next.Name == "" {
				errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
			}
		}
		if b.Filter != nil {
			next.Filter = strings.TrimSpace(*b.Filter)
			if fe := filterError(next.Filter); fe != nil {
				errs = append(errs, fe)
			}
		}
		if b.Enabled != nil {
			next.Enabled = *b.Enabled
		}
		if b.Priority != nil {
			next.Priority = *b.Priority
		}
		next.At, next.TZ = before.At, before.TZ
		if b.Delivery != nil {
			if next.Delivery = *b.Delivery; next.Delivery != deliveryDigest {
				next.At, next.TZ = nil, ""
			}
		}
		if b.At != nil {
			next.At = b.At
		}
		if b.TZ != nil {
			next.TZ = *b.TZ
		}
		sched, serrs := ruleSchedule(next.Delivery, next.Priority, next.At, next.TZ)
		if errs = append(errs, serrs...); len(errs) > 0 {
			return huma.Error400BadRequest("invalid rule", errs...)
		}
		if b.WatchID != nil {
			next.WatchID = nil
			if *b.WatchID != "" {
				if err := checkWatch(ctx, tx, me, *b.WatchID); err != nil {
					return err
				}
				next.WatchID = b.WatchID
			}
		}
		at, tz := scheduleArgs(sched)
		_, err = tx.Exec(ctx, `UPDATE rules SET name = $2, watch_id = $3, filter = $4, enabled = $5, priority = $6, delivery = $7, digest_at = $8, digest_tz = $9,
			updated_at = now() WHERE id = $1`,
			in.ID, next.Name, next.WatchID, next.Filter, next.Enabled, next.Priority, next.Delivery, at, tz)
		if isUniqueViolation(err) {
			return ruleNameError(next.Name)
		}
		if err != nil {
			return err
		}
		if before.Delivery == deliveryDigest && before.Enabled && !next.Enabled {
			// A disabled rule's schedule no longer applies: what it holds goes out at once (012-AC18).
			sched = nil
		}
		if before.Delivery == deliveryDigest && (sched == nil || !slices.Equal(before.At, sched.At) || before.TZ != sched.TZ) {
			if err := reschedule(ctx, tx, in.ID, sched); err != nil {
				return err
			}
		}
		if b.ChannelIDs != nil {
			if err := setChannels(ctx, tx, me, in.ID, b.ChannelIDs); err != nil {
				return err
			}
		}
		if after, err = ownRule(ctx, tx, me, in.ID, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "rule.updated", TargetType: "rule", TargetID: in.ID, Before: before, After: after})
	})
	if err != nil {
		return nil, s.fail("update-rule", err)
	}
	return &ruleOutput{Body: after}, nil
}

func (s *Service) deleteRule(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownRule(ctx, tx, me, in.ID, true)
		if err != nil {
			return err
		}
		// Before the delete, which sets their rule_id NULL: nothing could move them after (012-AC18).
		if before.Delivery == deliveryDigest {
			if err := reschedule(ctx, tx, in.ID, nil); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM rules WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "rule.deleted", TargetType: "rule", TargetID: in.ID, Before: before})
	})
	if err != nil {
		return nil, s.fail("delete-rule", err)
	}
	return nil, nil
}

// previewRule evaluates the filter exactly as the router would, on events of the caller's watches.
// 001-AC13.
func (s *Service) previewRule(ctx context.Context, in *previewInput) (*previewOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	if !s.previews.Allow(me) {
		return nil, huma.ErrorWithHeaders(huma.Error429TooManyRequests("too many previews: slow down"), http.Header{"Retry-After": {"1"}})
	}
	src := strings.TrimSpace(in.Body.Filter)
	if fe := filterError(src); fe != nil {
		return nil, huma.Error400BadRequest("invalid filter", fe)
	}
	prog, _ := filter.Compile(src)
	sql := `SELECT e.id, e.type, e.occurred_at, e.observed_at, e.payload, w.id::text, w.name, w.kind
		FROM events e JOIN watches w ON w.id = e.watch_id WHERE ` + watch.ReadableSQL("w", "@viewer")
	args := pgx.NamedArgs{"viewer": me, "limit": previewEvents}
	if id := in.Body.WatchID; id != "" {
		if uuid.Validate(id) != nil {
			return nil, huma.Error400BadRequest("invalid preview", &huma.ErrorDetail{Location: "body.watch_id", Message: "not a watch ID", Value: id})
		}
		sql += " AND w.id = @watch"
		args["watch"] = id
	}
	rows, err := s.pool.Query(ctx, sql+" ORDER BY e.id DESC LIMIT @limit", args)
	if err != nil {
		return nil, s.fail("preview-rule", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PreviewItem, error) {
		var it PreviewItem
		e := &it.Event
		err := r.Scan(&e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &it.Watch.ID, &it.Watch.Name, &it.Watch.Kind)
		return it, err
	})
	if err != nil {
		return nil, s.fail("preview-rule", err)
	}
	for i := range items {
		it := &items[i]
		matched, err := filter.Match(prog, envOf(it.Event, it.Watch))
		it.Matched = matched
		if err != nil {
			it.Error = err.Error()
		}
		// Evaluated on the whole notes, returned without them: 20 events of 64 KiB for a list that never shows them.
		delete(it.Event.Payload, "body")
	}
	out := &previewOutput{}
	out.Body.Items = items
	if out.Body.Items == nil {
		out.Body.Items = []PreviewItem{}
	}
	return out, nil
}

// envOf is what a filter sees of an event: a bounded copy of its payload, and its upstream time when
// known, else when gedour saw it.
func envOf(e watch.Event, w WatchRef) filter.Env {
	at := e.ObservedAt
	if e.OccurredAt != nil {
		at = *e.OccurredAt
	}
	// Sanitize returns a copy: these defaults reach no stored or delivered payload. Only releases carry the two
	// fields, and len(nil) or lower(nil) would fail a rule on all watches at every other event.
	payload := filter.Sanitize(e.Payload)
	if payload["cves"] == nil {
		payload["cves"] = []any{}
	}
	if payload["body"] == nil {
		payload["body"] = ""
	}
	return filter.Env{
		Event: filter.Event{Type: e.Type, Payload: payload, OccurredAt: at},
		Watch: filter.Watch{ID: w.ID, Name: w.Name, Kind: w.Kind},
	}
}
