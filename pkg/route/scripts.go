package route

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/watch"
)

// MaxScripts caps the scripts of one user.
const MaxScripts = 50

// Dry runs per user: each one starts a sandbox process.
const (
	dryRunRate  = 1.0
	dryRunBurst = 3.0
)

// ScriptConfig is the config of a script channel.
type ScriptConfig struct {
	ScriptID string `json:"script_id"`
}

// ScriptSummary is a script without its source, as lists show it.
type ScriptSummary struct {
	ID        string       `json:"id" format:"uuid"`
	Name      string       `json:"name"`
	Size      int          `json:"size" doc:"Source length in bytes"`
	Channels  []ChannelRef `json:"channels" doc:"The script channels that run it"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Script is one of the caller's scripts.
type Script struct {
	ScriptSummary
	Source string `json:"source"`
}

type scriptOutput struct {
	Body Script
}

type listScriptsOutput struct {
	Body struct {
		Items []ScriptSummary `json:"items" doc:"Sorted by name"`
	}
}

type createScriptInput struct {
	Body struct {
		Name   string `json:"name" minLength:"1" maxLength:"200"`
		Source string `json:"source" maxLength:"65536" doc:"JavaScript defining function main(event, ctx); checked for syntax on save"`
	}
}

type updateScriptInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name   *string `json:"name,omitempty" minLength:"1" maxLength:"200"`
		Source *string `json:"source,omitempty" maxLength:"65536"`
	}
}

// SampleEvent is a made-up event for a dry run.
type SampleEvent struct {
	Type       string         `json:"type" minLength:"1" maxLength:"100" doc:"e.g. release.published"`
	OccurredAt *time.Time     `json:"occurred_at,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	Watch      *SampleWatch   `json:"watch,omitempty"`
}

// SampleWatch is the watch a sample event claims to come from.
type SampleWatch struct {
	Name string `json:"name" maxLength:"200"`
	Kind string `json:"kind,omitempty" enum:"github_release,github_pr,gitlab_release,gitlab_mr,oci_tags"`
}

type dryRunInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		EventID *int64       `json:"event_id,omitempty" minimum:"1" doc:"An event the caller can read; or give sample"`
		Sample  *SampleEvent `json:"sample,omitempty"`
	}
}

type dryRunSourceInput struct {
	Body struct {
		Source  string       `json:"source" maxLength:"65536" doc:"Unsaved code to run"`
		EventID *int64       `json:"event_id,omitempty" minimum:"1" doc:"An event the caller can read; or give sample"`
		Sample  *SampleEvent `json:"sample,omitempty"`
	}
}

// DryRun is the outcome of a run that sent nothing: http.fetch only recorded its calls (001-AC18).
type DryRun struct {
	OK         bool           `json:"ok"`
	Outcome    string         `json:"outcome" enum:"ok,error,timeout,killed" doc:"error: the script threw; killed: memory limit or crash"`
	Error      string         `json:"error,omitempty"`
	Logs       []string       `json:"logs"`
	Result     string         `json:"result,omitempty" doc:"main's return value as JSON text, 16 KiB at most"`
	Fetches    []script.Fetch `json:"fetches" doc:"The http.fetch calls, none of them sent; secret-looking header values redacted"`
	DurationMS int64          `json:"duration_ms"`
}

type dryRunOutput struct {
	Body DryRun
}

// scriptSelect reads the owner's scripts with the channels running them; @full adds the source.
const scriptSelect = `SELECT s.id::text, s.name, octet_length(s.source), CASE WHEN @full THEN s.source ELSE '' END,
		COALESCE((SELECT json_agg(json_build_object('id', c.id, 'name', c.name, 'type', c.type) ORDER BY lower(c.name), c.id)
			FROM channels c WHERE c.type = 'script' AND c.owner_id = s.owner_id AND c.config ->> 'script_id' = s.id::text), '[]'),
		s.created_at, s.updated_at
	FROM scripts s WHERE s.owner_id = @owner`

func scanScripts(rows pgx.Rows) ([]Script, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Script, error) {
		var x Script
		err := r.Scan(&x.ID, &x.Name, &x.Size, &x.Source, &x.Channels, &x.CreatedAt, &x.UpdatedAt)
		return x, err
	})
}

func ownScript(ctx context.Context, q querier, owner, id string, lock bool) (Script, error) {
	if uuid.Validate(id) != nil {
		return Script{}, huma.Error404NotFound("script not found")
	}
	sql := scriptSelect + " AND s.id = @id"
	if lock {
		sql += " FOR UPDATE OF s"
	}
	rows, err := q.Query(ctx, sql, pgx.NamedArgs{"owner": owner, "id": id, "full": true})
	if err != nil {
		return Script{}, err
	}
	ss, err := scanScripts(rows)
	if err != nil {
		return Script{}, err
	}
	if len(ss) == 0 {
		return Script{}, huma.Error404NotFound("script not found")
	}
	return ss[0], nil
}

// sourceError is the 400 detail of a script that does not parse, its position in the value.
func sourceError(src string) error {
	err := script.Check(src)
	var se *script.SyntaxError
	if !errors.As(err, &se) {
		return nil
	}
	var pos any
	if se.Line > 0 {
		pos = map[string]int{"line": se.Line, "column": se.Column}
	}
	return &huma.ErrorDetail{Location: "body.source", Message: se.Error(), Value: pos}
}

// scriptAudit keeps the source out of the audit log: its hash and size say whether and how it changed.
func scriptAudit(s Script) map[string]any {
	sum := sha256.Sum256([]byte(s.Source))
	return map[string]any{"id": s.ID, "name": s.Name, "source_sha256": hex.EncodeToString(sum[:]), "source_bytes": len(s.Source)}
}

// checkScriptRef verifies a script channel's script is the owner's. SHARE: a concurrent delete of the
// script waits for this channel to land, then sees it and refuses.
func checkScriptRef(ctx context.Context, tx pgx.Tx, owner string, cfg map[string]any) error {
	id, _ := cfg["script_id"].(string)
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scripts WHERE id = $1 AND owner_id = $2 FOR SHARE)`, id, owner).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return huma.Error400BadRequest("invalid channel", configError("script_id", "no such script among yours", id))
	}
	return nil
}

// send delivers d through ch: a script channel runs its script for real and returns its log lines
// (non-nil, maybe empty). reserved (a channel test) runs on the interactive slot the caller holds; a
// delivery waits for a delivery slot.
func send(ctx context.Context, q rowQuerier, sender *notify.Sender, runner *script.Runner, ch notify.Channel, d notify.Delivery, reserved bool) ([]string, error) {
	if ch.Type != notify.TypeScript {
		return nil, sender.Send(ctx, ch, d)
	}
	var c ScriptConfig
	if err := json.Unmarshal(ch.Config, &c); err != nil {
		return nil, fmt.Errorf("script config: %w", err)
	}
	var src string
	if err := q.QueryRow(ctx, `SELECT source FROM scripts WHERE id = $1`, c.ScriptID).Scan(&src); err != nil {
		return nil, fmt.Errorf("load script: %w", err)
	}
	var res script.Result
	if reserved {
		res = runner.RunReserved(ctx, src, script.InputOf(d), false)
	} else {
		res = runner.Run(ctx, src, script.InputOf(d))
	}
	logs := res.Logs
	if logs == nil {
		logs = []string{}
	}
	return logs, res.Err()
}

// busy is the 429 of an interactive run with no free sandbox.
func busy() error {
	return huma.ErrorWithHeaders(huma.Error429TooManyRequests(script.ErrBusy.Error()), http.Header{"Retry-After": {"2"}})
}

func (s *Service) registerScripts(api huma.API) {
	tags := []string{"scripts"}
	huma.Register(api, huma.Operation{
		OperationID: "list-scripts", Method: http.MethodGet, Path: "/api/v1/scripts",
		Summary: "The caller's scripts, without their source", Tags: tags,
	}, s.listScripts)
	huma.Register(api, huma.Operation{
		OperationID: "get-script", Method: http.MethodGet, Path: "/api/v1/scripts/{id}",
		Summary: "One of the caller's scripts", Tags: tags,
	}, func(ctx context.Context, in *idPath) (*scriptOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		x, err := ownScript(ctx, s.pool, me, in.ID, false)
		if err != nil {
			return nil, s.fail("get-script", err)
		}
		return &scriptOutput{Body: x}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "create-script", Method: http.MethodPost, Path: "/api/v1/scripts",
		Summary: "Create a script; its syntax is checked first", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.createScript)
	huma.Register(api, huma.Operation{
		OperationID: "update-script", Method: http.MethodPatch, Path: "/api/v1/scripts/{id}",
		Summary: "Change a script's name or source", Tags: tags,
	}, s.updateScript)
	huma.Register(api, huma.Operation{
		OperationID: "delete-script", Method: http.MethodDelete, Path: "/api/v1/scripts/{id}",
		Summary: "Delete a script no channel runs", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteScript)
	huma.Register(api, huma.Operation{
		OperationID: "dry-run-script", Method: http.MethodPost, Path: "/api/v1/scripts/{id}/dry-run",
		Summary: "Run a saved script on an event in the sandbox; http.fetch is recorded, never sent", Tags: tags,
	}, func(ctx context.Context, in *dryRunInput) (*dryRunOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		x, err := ownScript(ctx, s.pool, me, in.ID, false)
		if err != nil {
			return nil, s.fail("dry-run-script", err)
		}
		return s.dryRun(ctx, me, x.Source, in.Body.EventID, in.Body.Sample)
	})
	huma.Register(api, huma.Operation{
		OperationID: "dry-run-source", Method: http.MethodPost, Path: "/api/v1/scripts/dry-run",
		Summary: "Run unsaved code on an event in the sandbox; http.fetch is recorded, never sent", Tags: tags,
	}, func(ctx context.Context, in *dryRunSourceInput) (*dryRunOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		if se := sourceError(in.Body.Source); se != nil {
			return nil, huma.Error400BadRequest("invalid script", se)
		}
		return s.dryRun(ctx, me, in.Body.Source, in.Body.EventID, in.Body.Sample)
	})
}

func (s *Service) listScripts(ctx context.Context, _ *struct{}) (*listScriptsOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, scriptSelect+" ORDER BY lower(s.name), s.id", pgx.NamedArgs{"owner": me, "full": false})
	if err != nil {
		return nil, s.fail("list-scripts", err)
	}
	ss, err := scanScripts(rows)
	if err != nil {
		return nil, s.fail("list-scripts", err)
	}
	out := &listScriptsOutput{}
	out.Body.Items = make([]ScriptSummary, len(ss))
	for i, x := range ss {
		out.Body.Items[i] = x.ScriptSummary
	}
	return out, nil
}

func scriptNameError(name string) error {
	return huma.Error409Conflict("a script with this name exists",
		&huma.ErrorDetail{Location: "body.name", Message: "you already have a script with this name", Value: name})
}

func (s *Service) createScript(ctx context.Context, in *createScriptInput) (*scriptOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	var errs []error
	name := strings.TrimSpace(in.Body.Name)
	if name == "" {
		errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
	}
	if se := sourceError(in.Body.Source); se != nil {
		errs = append(errs, se)
	}
	if len(errs) > 0 {
		return nil, huma.Error400BadRequest("invalid script", errs...)
	}
	var out Script
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := underCap(ctx, tx, me, "scripts", MaxScripts); err != nil {
			return err
		}
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO scripts (owner_id, name, source) VALUES ($1, $2, $3) RETURNING id::text`, me, name, in.Body.Source).Scan(&id)
		if isUniqueViolation(err) {
			return scriptNameError(name)
		}
		if err != nil {
			return err
		}
		if out, err = ownScript(ctx, tx, me, id, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "script.created", TargetType: "script", TargetID: id, After: scriptAudit(out)})
	})
	if err != nil {
		return nil, s.fail("create-script", err)
	}
	return &scriptOutput{Body: out}, nil
}

func (s *Service) updateScript(ctx context.Context, in *updateScriptInput) (*scriptOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	var after Script
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownScript(ctx, tx, me, in.ID, true)
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
		if b.Source != nil {
			next.Source = *b.Source
			if se := sourceError(next.Source); se != nil {
				errs = append(errs, se)
			}
		}
		if len(errs) > 0 {
			return huma.Error400BadRequest("invalid script", errs...)
		}
		_, err = tx.Exec(ctx, `UPDATE scripts SET name = $2, source = $3, updated_at = now() WHERE id = $1`, in.ID, next.Name, next.Source)
		if isUniqueViolation(err) {
			return scriptNameError(next.Name)
		}
		if err != nil {
			return err
		}
		if after, err = ownScript(ctx, tx, me, in.ID, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "script.updated", TargetType: "script", TargetID: in.ID,
			Before: scriptAudit(before), After: scriptAudit(after)})
	})
	if err != nil {
		return nil, s.fail("update-script", err)
	}
	return &scriptOutput{Body: after}, nil
}

func (s *Service) deleteScript(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The row lock waits for a channel being created on this script, so the list below sees it.
		before, err := ownScript(ctx, tx, me, in.ID, true)
		if err != nil {
			return err
		}
		if len(before.Channels) > 0 {
			details := make([]error, len(before.Channels))
			names := make([]string, len(before.Channels))
			for i, c := range before.Channels {
				names[i] = c.Name
				details[i] = &huma.ErrorDetail{Location: "channels", Message: "runs this script", Value: c.Name}
			}
			return huma.Error409Conflict("used by "+strings.Join(names, ", ")+": delete these channels or point them at another script first", details...)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM scripts WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "script.deleted", TargetType: "script", TargetID: in.ID, Before: scriptAudit(before)})
	})
	if err != nil {
		return nil, s.fail("delete-script", err)
	}
	return nil, nil
}

// sampleWatchID marks the made-up watch of a sample event.
const sampleWatchID = "00000000-0000-0000-0000-000000000000"

// dryInput is the delivery a dry run pretends to make: a readable event, or the caller's sample.
func (s *Service) dryInput(ctx context.Context, me string, eventID *int64, sample *SampleEvent) (notify.Delivery, error) {
	var d notify.Delivery
	switch {
	case (eventID == nil) == (sample == nil):
		return d, huma.Error400BadRequest("invalid dry run", &huma.ErrorDetail{Location: "body", Message: "give event_id or sample, not both"})
	case sample != nil:
		d.Event = notify.Event{Type: sample.Type, OccurredAt: sample.OccurredAt, ObservedAt: time.Now().UTC(), Payload: sample.Payload}
		d.Watch = notify.Watch{ID: sampleWatchID, Name: "Sample watch"}
		if sample.Watch != nil {
			d.Watch.Name, d.Watch.Kind = sample.Watch.Name, sample.Watch.Kind
		}
		return d, nil
	}
	var e watch.Event
	err := s.pool.QueryRow(ctx, `SELECT e.id, e.type, e.occurred_at, e.observed_at, e.payload, w.id::text, w.name, w.kind
		FROM events e JOIN watches w ON w.id = e.watch_id WHERE e.id = $1 AND `+watch.ReadableSQL("w", "$2::uuid"), *eventID, me).
		Scan(&e.ID, &e.Type, &e.OccurredAt, &e.ObservedAt, &e.Payload, &d.Watch.ID, &d.Watch.Name, &d.Watch.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, huma.Error400BadRequest("invalid dry run", &huma.ErrorDetail{Location: "body.event_id", Message: "no such event among the ones you can read", Value: *eventID})
	}
	if err != nil {
		return d, err
	}
	d.Event = notify.Event{ID: e.ID, Type: e.Type, OccurredAt: e.OccurredAt, ObservedAt: e.ObservedAt, Payload: e.Payload}
	return d, nil
}

// dryRun runs src in the sandbox with http.fetch stubbed: no side effect, so no audit row (001-AC18).
func (s *Service) dryRun(ctx context.Context, me, src string, eventID *int64, sample *SampleEvent) (*dryRunOutput, error) {
	if !s.dryRuns.Allow(me) {
		return nil, huma.ErrorWithHeaders(huma.Error429TooManyRequests("too many dry runs: one per second"), http.Header{"Retry-After": {"1"}})
	}
	d, err := s.dryInput(ctx, me, eventID, sample)
	if err != nil {
		return nil, s.fail("dry-run", err)
	}
	res, err := s.runner.Interactive(ctx, src, script.InputOf(d), true)
	if err != nil {
		return nil, busy()
	}
	out := &dryRunOutput{Body: DryRun{OK: res.Outcome == script.OutcomeOK, Outcome: res.Outcome, Error: res.Error,
		Logs: res.Logs, Result: res.Result, Fetches: res.Fetches, DurationMS: res.Duration.Milliseconds()}}
	if out.Body.Logs == nil {
		out.Body.Logs = []string{}
	}
	if out.Body.Fetches == nil {
		out.Body.Fetches = []script.Fetch{}
	}
	return out, nil
}
