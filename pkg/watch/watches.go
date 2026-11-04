package watch

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// Watch is a watch as the API returns it to one viewer.
type Watch struct {
	ID           string         `json:"id" format:"uuid"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind" enum:"github_release,github_pr,gitlab_release,gitlab_mr,oci_tags"`
	Config       map[string]any `json:"config" doc:"Per-kind settings, normalized (defaults filled)"`
	CredentialID *string        `json:"credential_id" format:"uuid" nullable:"true"`
	Schedule     string         `json:"schedule" doc:"Go duration (1m to 168h) or 5-field cron expression"`
	Paused       bool           `json:"paused"`
	OwnerID      string         `json:"owner_id" format:"uuid"`
	OwnerName    string         `json:"owner_name"`
	Permission   string         `json:"permission" enum:"owner,rw,ro" doc:"The caller's access to this watch"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type watchOutput struct {
	Body Watch
}

type idPath struct {
	ID string `path:"id" format:"uuid"`
}

type listWatchesInput struct {
	Kind   string `query:"kind" enum:"github_release,github_pr,gitlab_release,gitlab_mr,oci_tags" doc:"Only watches of this kind"`
	Q      string `query:"q" maxLength:"200" doc:"Case-insensitive match on the name or the config"`
	Shared string `query:"shared" enum:"mine,with_me,all" default:"all" doc:"Owned by the caller, shared with them, or both"`
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"500"`
}

type listWatchesOutput struct {
	Body struct {
		Items []Watch `json:"items" doc:"Sorted by name"`
	}
}

type createWatchInput struct {
	Body struct {
		Name         string         `json:"name" minLength:"1" maxLength:"200"`
		Kind         string         `json:"kind" enum:"github_release,github_pr,gitlab_release,gitlab_mr,oci_tags"`
		Config       map[string]any `json:"config"`
		CredentialID string         `json:"credential_id,omitempty" format:"uuid" doc:"Owned credential of the kind's family"`
		Schedule     string         `json:"schedule,omitempty" default:"15m" doc:"Go duration (1m to 168h) or 5-field cron expression"`
		Paused       bool           `json:"paused,omitempty"`
	}
}

type updateWatchInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name         *string        `json:"name,omitempty" minLength:"1" maxLength:"200"`
		Config       map[string]any `json:"config,omitempty" doc:"Replaces the whole config"`
		CredentialID *string        `json:"credential_id,omitempty" doc:"Owner only; empty string detaches"`
		Schedule     *string        `json:"schedule,omitempty"`
		Paused       *bool          `json:"paused,omitempty"`
	}
}

func (s *Service) registerWatches(api huma.API) {
	tags := []string{"watches"}
	huma.Register(api, huma.Operation{
		OperationID: "list-watches", Method: http.MethodGet, Path: "/api/v1/watches",
		Summary: "Watches the caller owns or that are shared with them", Tags: tags,
	}, s.listWatches)
	huma.Register(api, huma.Operation{
		OperationID: "get-watch", Method: http.MethodGet, Path: "/api/v1/watches/{id}",
		Summary: "One watch", Tags: tags,
	}, func(ctx context.Context, in *idPath) (*watchOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		w, err := authorize(ctx, s.pool, me, in.ID, levelRead, false)
		if err != nil {
			return nil, s.fail("get-watch", err)
		}
		return &watchOutput{Body: w}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "create-watch", Method: http.MethodPost, Path: "/api/v1/watches",
		Summary: "Create a watch", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.createWatch)
	huma.Register(api, huma.Operation{
		OperationID: "update-watch", Method: http.MethodPatch, Path: "/api/v1/watches/{id}",
		Summary: "Change a watch's name, config, schedule, paused state or credential", Tags: tags,
	}, s.updateWatch)
	for _, p := range []struct {
		op, path, action string
		paused           bool
	}{
		{"pause-watch", "/api/v1/watches/{id}/pause", "watch.paused", true},
		{"resume-watch", "/api/v1/watches/{id}/resume", "watch.resumed", false},
	} {
		huma.Register(api, huma.Operation{
			OperationID: p.op, Method: http.MethodPost, Path: p.path,
			Summary: strings.ToUpper(p.op[:1]) + strings.ReplaceAll(p.op[1:], "-", " "), Tags: tags,
		}, func(ctx context.Context, in *idPath) (*watchOutput, error) {
			return s.setPaused(ctx, p.op, p.action, in.ID, p.paused)
		})
	}
	huma.Register(api, huma.Operation{
		OperationID: "delete-watch", Method: http.MethodDelete, Path: "/api/v1/watches/{id}",
		Summary: "Delete a watch (owner only)", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteWatch)
}

func (s *Service) listWatches(ctx context.Context, in *listWatchesInput) (*listWatchesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	sql := watchSelect
	args := pgx.NamedArgs{"viewer": me, "limit": in.Limit}
	switch in.Shared {
	case "mine":
		sql += " AND w.owner_id = @viewer"
	case "with_me":
		sql += " AND s.user_id IS NOT NULL"
	}
	if in.Kind != "" {
		sql += " AND w.kind = @kind"
		args["kind"] = in.Kind
	}
	if q := strings.TrimSpace(in.Q); q != "" {
		sql += " AND (w.name ILIKE @q OR w.config::text ILIKE @q)"
		args["q"] = likePattern(q)
	}
	rows, err := s.pool.Query(ctx, sql+" ORDER BY lower(w.name), w.id LIMIT @limit", args)
	if err != nil {
		return nil, s.fail("list-watches", err)
	}
	items, err := scanWatches(rows)
	if err != nil {
		return nil, s.fail("list-watches", err)
	}
	out := &listWatchesOutput{}
	out.Body.Items = items
	if out.Body.Items == nil {
		out.Body.Items = []Watch{}
	}
	return out, nil
}

// checkCredential verifies that id names a credential of owner that fits kind.
func checkCredential(ctx context.Context, tx pgx.Tx, owner, kind, id string) error {
	detail := func(msg string) error {
		return huma.Error400BadRequest("invalid credential", &huma.ErrorDetail{Location: "body.credential_id", Message: msg, Value: id})
	}
	if uuid.Validate(id) != nil {
		return detail("expected a credential ID")
	}
	var credKind string
	// KEY SHARE holds the row until commit: a concurrent delete waits, then sees this watch and answers 409.
	err := tx.QueryRow(ctx, `SELECT kind FROM credentials WHERE id = $1 AND owner_id = $2 FOR KEY SHARE`, id, owner).Scan(&credKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return detail("no such credential among the watch owner's")
	}
	if err != nil {
		return err
	}
	if want := CredentialKindFor(kind); credKind != want {
		return detail("a " + kind + " watch needs a " + want + " credential")
	}
	return nil
}

func scheduleError(msg, value string) error {
	return &huma.ErrorDetail{Location: "body.schedule", Message: msg, Value: value}
}

func (s *Service) createWatch(ctx context.Context, in *createWatchInput) (*watchOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	cfg, errs := s.v.Config(b.Kind, b.Config)
	if b.Schedule == "" {
		b.Schedule = DefaultSchedule
	}
	schedule, msg := parseSchedule(b.Schedule)
	if msg != "" {
		errs = append(errs, scheduleError(msg, b.Schedule))
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
	}
	if len(errs) > 0 {
		return nil, huma.Error400BadRequest("invalid watch", errs...)
	}
	var w Watch
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var cred *string
		if b.CredentialID != "" {
			if err := checkCredential(ctx, tx, me, b.Kind, b.CredentialID); err != nil {
				return err
			}
			cred = &b.CredentialID
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO watches (owner_id, name, kind, config, credential_id, schedule, paused)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`, me, name, b.Kind, cfg, cred, schedule, b.Paused).Scan(&id); err != nil {
			return err
		}
		var err error
		if w, err = authorize(ctx, tx, me, id, levelOwner, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.created", TargetType: "watch", TargetID: id, After: w})
	})
	if err != nil {
		return nil, s.fail("create-watch", err)
	}
	return &watchOutput{Body: w}, nil
}

func (s *Service) updateWatch(ctx context.Context, in *updateWatchInput) (*watchOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	var after Watch
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		need := levelWrite
		// Credentials are private to the owner, so only the owner may attach or detach one.
		if b.CredentialID != nil {
			need = levelOwner
		}
		before, err := authorize(ctx, tx, me, in.ID, need, true)
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
		if b.Config != nil {
			var cerrs []error
			next.Config, cerrs = s.v.Config(before.Kind, b.Config)
			errs = append(errs, cerrs...)
		}
		if b.Schedule != nil {
			var msg string
			if next.Schedule, msg = parseSchedule(*b.Schedule); msg != "" {
				errs = append(errs, scheduleError(msg, *b.Schedule))
			}
		}
		if b.Paused != nil {
			next.Paused = *b.Paused
		}
		if len(errs) > 0 {
			return huma.Error400BadRequest("invalid watch", errs...)
		}
		// The config decides where the owner's credential is sent: a sharee retargeting it could hand the
		// token to another host or read the owner's private repositories with it.
		if before.CredentialID != nil && b.Config != nil && levelOf(before.Permission) < levelOwner &&
			!reflect.DeepEqual(next.Config, before.Config) {
			return huma.Error403Forbidden("only the owner can change the target of a watch that uses their credential")
		}
		if b.CredentialID != nil {
			next.CredentialID = nil
			if *b.CredentialID != "" {
				if err := checkCredential(ctx, tx, before.OwnerID, before.Kind, *b.CredentialID); err != nil {
					return err
				}
				next.CredentialID = b.CredentialID
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE watches SET name = $2, config = $3, schedule = $4, paused = $5, credential_id = $6, updated_at = now()
			WHERE id = $1`, in.ID, next.Name, next.Config, next.Schedule, next.Paused, next.CredentialID); err != nil {
			return err
		}
		if after, err = authorize(ctx, tx, me, in.ID, levelRead, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.updated", TargetType: "watch", TargetID: in.ID, Before: before, After: after})
	})
	if err != nil {
		return nil, s.fail("update-watch", err)
	}
	return &watchOutput{Body: after}, nil
}

// setPaused is idempotent: a watch already in the asked state is returned unchanged and not audited.
// 001-AC4.
func (s *Service) setPaused(ctx context.Context, op, action, id string, paused bool) (*watchOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	var w Watch
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := authorize(ctx, tx, me, id, levelWrite, true)
		if err != nil {
			return err
		}
		if w = before; before.Paused == paused {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE watches SET paused = $2, updated_at = now() WHERE id = $1`, id, paused); err != nil {
			return err
		}
		if w, err = authorize(ctx, tx, me, id, levelRead, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: action, TargetType: "watch", TargetID: id, Before: before, After: w})
	})
	if err != nil {
		return nil, s.fail(op, err)
	}
	return &watchOutput{Body: w}, nil
}

func (s *Service) deleteWatch(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := authorize(ctx, tx, me, in.ID, levelOwner, true)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM watches WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.deleted", TargetType: "watch", TargetID: in.ID, Before: before})
	})
	if err != nil {
		return nil, s.fail("delete-watch", err)
	}
	return nil, nil
}
