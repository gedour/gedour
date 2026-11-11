package watch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// Watch is a watch as the API returns it to one viewer: what the user set, and the worker's view.
type Watch struct {
	Spec
	Status
}

// Spec is the user-set part of a watch; the audit log records this, never the run status.
type Spec struct {
	ID           string         `json:"id" format:"uuid"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind" enum:"github_repo,github_pr,github_issue,github_discussion,gitlab_release,gitlab_mr,oci_tags"`
	Config       map[string]any `json:"config" doc:"Per-kind settings, normalized (defaults filled)"`
	CredentialID *string        `json:"credential_id" format:"uuid" nullable:"true"`
	Schedule     string         `json:"schedule" doc:"Go duration (1m to 168h) or 5-field cron expression"`
	Paused       bool           `json:"paused"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// Status is the worker's view of a watch.
type Status struct {
	NextRunAt           time.Time  `json:"next_run_at" doc:"When the worker runs the watch next; ignored while paused"`
	Running             bool       `json:"running" doc:"A worker holds the watch right now"`
	LastRunAt           *time.Time `json:"last_run_at" nullable:"true" doc:"Null until the first run"`
	LastStatus          *string    `json:"last_status" enum:"ok,error,rate_limited" nullable:"true" doc:"Outcome of the last run"`
	LastError           *string    `json:"last_error" nullable:"true" doc:"Error or rate-limit message of the last run, up to 1 KiB"`
	ConsecutiveFailures int        `json:"consecutive_failures" doc:"Failed runs in a row; drives the backoff"`
}

type watchOutput struct {
	Body Watch
}

type idPath struct {
	ID string `path:"id" format:"uuid"`
}

type listWatchesInput struct {
	Kind  string `query:"kind" enum:"github_repo,github_pr,github_issue,github_discussion,gitlab_release,gitlab_mr,oci_tags" doc:"Only watches of this kind"`
	Q     string `query:"q" maxLength:"200" doc:"Case-insensitive match on the name or the config"`
	Limit int    `query:"limit" minimum:"1" maximum:"500" default:"500"`
}

type listWatchesOutput struct {
	Body struct {
		Items []Watch `json:"items" doc:"Sorted by name"`
	}
}

type createWatchInput struct {
	Body struct {
		Name         string         `json:"name" minLength:"1" maxLength:"200"`
		Kind         string         `json:"kind" enum:"github_repo,github_pr,github_issue,github_discussion,gitlab_release,gitlab_mr,oci_tags"`
		Config       map[string]any `json:"config"`
		CredentialID string         `json:"credential_id,omitempty" format:"uuid" doc:"Owned credential of the kind's family"`
		Schedule     string         `json:"schedule,omitempty" doc:"Go duration (1m to 168h) or 5-field cron expression; default 1h for GitHub kinds, 15m otherwise"`
		Paused       bool           `json:"paused,omitempty"`
	}
}

type updateWatchInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name         *string        `json:"name,omitempty" minLength:"1" maxLength:"200"`
		Config       map[string]any `json:"config,omitempty" doc:"Replaces the whole config"`
		CredentialID *string        `json:"credential_id,omitempty" doc:"Empty string detaches"`
		Schedule     *string        `json:"schedule,omitempty"`
		Paused       *bool          `json:"paused,omitempty"`
	}
}

func (s *Service) registerWatches(api huma.API) {
	tags := []string{"watches"}
	huma.Register(api, huma.Operation{
		OperationID: "list-watches", Method: http.MethodGet, Path: "/api/v1/watches",
		Summary: "Watches the caller owns", Tags: tags,
	}, s.listWatches)
	huma.Register(api, huma.Operation{
		OperationID: "get-watch", Method: http.MethodGet, Path: "/api/v1/watches/{id}",
		Summary: "One watch", Tags: tags,
	}, func(ctx context.Context, in *idPath) (*watchOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		w, err := authorize(ctx, s.pool, me, in.ID, false)
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
	s.registerActivity(api)
	huma.Register(api, huma.Operation{
		OperationID: "delete-watch", Method: http.MethodDelete, Path: "/api/v1/watches/{id}",
		Summary: "Delete a watch", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteWatch)
}

func (s *Service) listWatches(ctx context.Context, in *listWatchesInput) (*listWatchesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	sql := watchSelect
	args := pgx.NamedArgs{"viewer": me, "limit": in.Limit}
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
		return fmt.Errorf("credential kind: %w", err)
	}
	if want := CredentialKindFor(kind); credKind != want {
		return detail("a " + kind + " watch needs a " + want + " credential")
	}
	return nil
}

// maxKeyWatches bounds what a key can add to the worker's fetches; REST has no cap.
const maxKeyWatches = 200

// targetSQL reads a watch's target in SQL; it must follow targetOf's field order.
const targetSQL = `COALESCE(config->>'api_url', config->>'base_url', config->>'registry', '')`

// checkKnownHost keeps a key's config from carrying data to a host its owner never chose.
func checkKnownHost(ctx context.Context, tx pgx.Tx, owner, kind string, cfg map[string]any) error {
	field, raw := targetOf(cfg)
	target := comparableTarget(raw)
	if slices.Contains(defaultHosts[kind], target) {
		return nil
	}
	// A default counts on its own kind only: every default-host watch saves it, so a saved one would undo that in two calls.
	if isDefaultHost(target) {
		return unknownTargetError(field, raw)
	}
	rows, err := tx.Query(ctx, `SELECT `+targetSQL+` FROM watches WHERE owner_id = $1`, owner)
	if err != nil {
		return fmt.Errorf("saved targets: %w", err)
	}
	saved, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("saved targets: %w", err)
	}
	if slices.ContainsFunc(saved, func(s string) bool { return comparableTarget(s) == target }) {
		return nil
	}
	return unknownTargetError(field, raw)
}

// credentialTarget is comparableTarget with the scheme: a credential reaching https://H must not go to http://H.
func credentialTarget(raw string) string {
	scheme, _, ok := strings.Cut(raw, "://")
	if !ok {
		scheme = "https"
	}
	return strings.ToLower(scheme) + "://" + comparableTarget(raw)
}

// checkCredentialTarget keeps a key from sending a credential to a target no other watch sends it to; only an unused one may go to the kind's default host.
func checkCredentialTarget(ctx context.Context, tx pgx.Tx, owner, kind, credID, exceptWatchID string, cfg map[string]any) error {
	// NO KEY UPDATE serializes two key calls on one credential, so neither reads a reached set the other is changing.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM credentials WHERE id = $1 AND owner_id = $2 FOR NO KEY UPDATE`, credID, owner); err != nil {
		return fmt.Errorf("lock credential: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT id::text, `+targetSQL+` FROM watches WHERE owner_id = $1 AND credential_id = $2`, owner, credID)
	if err != nil {
		return fmt.Errorf("credential targets: %w", err)
	}
	// The edited watch makes the credential used, but its old target does not vouch for its new one.
	var used bool
	var reached []string
	var id, t string
	if _, err := pgx.ForEachRow(rows, []any{&id, &t}, func() error {
		used = true
		if id != exceptWatchID {
			reached = append(reached, t)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("credential targets: %w", err)
	}
	_, raw := targetOf(cfg)
	target := credentialTarget(raw)
	if (!used && slices.ContainsFunc(defaultHosts[kind], func(h string) bool { return "https://"+h == target })) ||
		slices.ContainsFunc(reached, func(s string) bool { return credentialTarget(s) == target }) {
		return nil
	}
	return huma.Error403Forbidden("an API key can only send a credential to a target one of your watches already sends it to: attach it in the UI first",
		&huma.ErrorDetail{Location: "body.credential_id", Message: "not a target another watch sends this credential to, nor the kind's default host for an unused one", Value: credID})
}

func unknownTargetError(field, raw string) error {
	return huma.Error403Forbidden("an API key can only target a known host and path: create a watch on it in the UI first",
		&huma.ErrorDetail{Location: "body.config." + field, Message: "not this kind's default host without a path, nor the host and path of one of your watches", Value: raw})
}

// underKeyCap refuses a key's create past maxKeyWatches. Soft, without a lock: two concurrent creates may overshoot by one.
func underKeyCap(ctx context.Context, tx pgx.Tx, owner string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM watches WHERE owner_id = $1`, owner).Scan(&n); err != nil {
		return fmt.Errorf("count watches: %w", err)
	}
	if n >= maxKeyWatches {
		return huma.Error409Conflict(fmt.Sprintf("you own %d watches: an API key cannot create more, delete one or use the UI", n))
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
	viaKey := audit.APIKeyID(ctx) != ""
	cfg, errs := s.v.Config(b.Kind, b.Config)
	if b.Schedule == "" {
		b.Schedule = DefaultScheduleFor(b.Kind)
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
		if viaKey {
			if err := underKeyCap(ctx, tx, me); err != nil {
				return err
			}
			if err := checkKnownHost(ctx, tx, me, b.Kind, cfg); err != nil {
				return err
			}
		}
		var cred *string
		if b.CredentialID != "" {
			if err := checkCredential(ctx, tx, me, b.Kind, b.CredentialID); err != nil {
				return err
			}
			if viaKey {
				if err := checkCredentialTarget(ctx, tx, me, b.Kind, b.CredentialID, "", cfg); err != nil {
					return err
				}
			}
			cred = &b.CredentialID
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO watches (owner_id, name, kind, config, credential_id, schedule, paused)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`, me, name, b.Kind, cfg, cred, schedule, b.Paused).Scan(&id); err != nil {
			return err
		}
		var err error
		if w, err = authorize(ctx, tx, me, id, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.created", TargetType: "watch", TargetID: id, After: w.Spec})
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
	viaKey := audit.APIKeyID(ctx) != ""
	var after Watch
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := authorize(ctx, tx, me, in.ID, true)
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
		// Authz reads any config change, a box included: a box spends the owner's token on another call.
		changed := !reflect.DeepEqual(next.Config, before.Config)
		retargeted := !sameTarget(next.Config, before.Config)
		if viaKey && changed {
			if err := checkKnownHost(ctx, tx, me, before.Kind, next.Config); err != nil {
				return err
			}
		}
		if b.CredentialID != nil {
			next.CredentialID = nil
			if *b.CredentialID != "" {
				if err := checkCredential(ctx, tx, me, before.Kind, *b.CredentialID); err != nil {
					return err
				}
				next.CredentialID = b.CredentialID
			}
		}
		// The target decides where the credential is sent; a detach sends nothing, so it needs no check.
		if viaKey && next.CredentialID != nil {
			_, rawBefore := targetOf(before.Config)
			_, rawNext := targetOf(next.Config)
			if before.CredentialID == nil || *before.CredentialID != *next.CredentialID || credentialTarget(rawBefore) != credentialTarget(rawNext) {
				if err := checkCredentialTarget(ctx, tx, me, before.Kind, *next.CredentialID, in.ID, next.Config); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE watches SET name = $2, config = $3, schedule = $4, paused = $5, credential_id = $6, updated_at = now()
			WHERE id = $1`, in.ID, next.Name, next.Config, next.Schedule, next.Paused, next.CredentialID); err != nil {
			return err
		}
		if retargeted {
			// A new target starts from a fresh baseline: diffing it against the old one would flood events.
			if _, err := tx.Exec(ctx, `DELETE FROM watch_states WHERE watch_id = $1`, in.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM http_cache WHERE watch_id = $1`, in.ID); err != nil {
				return err
			}
		} else if changed {
			// A box toggled: states stay, ETags go, so a boxes-off run cannot 304 onto numbers stored while a box was on.
			if _, err := tx.Exec(ctx, `DELETE FROM http_cache WHERE watch_id = $1`, in.ID); err != nil {
				return err
			}
		}
		held := viaKey && rateLimitHeld(before)
		if !held && (retargeted || next.Schedule != before.Schedule || (before.Paused && !next.Paused) || boxChecked(before.Config, next.Config)) {
			if _, err := tx.Exec(ctx, `UPDATE watches SET next_run_at = now() WHERE id = $1`, in.ID); err != nil {
				return err
			}
		}
		if after, err = authorize(ctx, tx, me, in.ID, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.updated", TargetType: "watch", TargetID: in.ID, Before: before.Spec, After: after.Spec})
	})
	if err != nil {
		return nil, s.fail("update-watch", err)
	}
	return &watchOutput{Body: after}, nil
}

// rateLimitHeld reports the upstream's rate limit holding w: a person may override it, an unattended client may not.
func rateLimitHeld(w Watch) bool {
	return w.LastStatus != nil && *w.LastStatus == "rate_limited" && w.NextRunAt.After(time.Now())
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
		before, err := authorize(ctx, tx, me, id, true)
		if err != nil {
			return err
		}
		if w = before; before.Paused == paused {
			return nil
		}
		// Resuming runs at once rather than at a due time that passed while paused.
		if _, err := tx.Exec(ctx, `UPDATE watches SET paused = $2, updated_at = now(),
			next_run_at = CASE WHEN $2 THEN next_run_at ELSE now() END WHERE id = $1`, id, paused); err != nil {
			return err
		}
		if w, err = authorize(ctx, tx, me, id, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: action, TargetType: "watch", TargetID: id, Before: before.Spec, After: w.Spec})
	})
	if err != nil {
		return nil, s.fail(op, err)
	}
	return &watchOutput{Body: w}, nil
}

// lockListingRules locks the rules listing a watch, by id; NO KEY UPDATE lets the router's FK KEY SHARE through.
const lockListingRules = `SELECT 1 FROM rules WHERE id IN (SELECT rule_id FROM rule_watches WHERE watch_id = $1) ORDER BY id FOR NO KEY UPDATE`

func (s *Service) deleteWatch(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Unlocked first: a caller who does not own the watch gets its 404 without locking anything.
		if _, err := authorize(ctx, tx, me, in.ID, false); err != nil {
			return err
		}
		// Rules before the watch (lock order in AGENTS.md): a rule edit holds its rule, then wants the watch's KEY SHARE.
		// NO KEY UPDATE lets the router's FK KEY SHARE through; TestWatchDeleteRacingRouter guards it.
		if _, err := tx.Exec(ctx, lockListingRules, in.ID); err != nil {
			return fmt.Errorf("lock rules: %w", err)
		}
		before, err := authorize(ctx, tx, me, in.ID, true)
		if err != nil {
			return err
		}
		// A statement of its own, after the watch lock: a rule edit adding this watch committed while that lock waited.
		if _, err := tx.Exec(ctx, lockListingRules, in.ID); err != nil {
			return fmt.Errorf("lock rules: %w", err)
		}
		// Events before rules: a router holding one takes its rules' KEY SHARE next, which a rule delete would block.
		if _, err := tx.Exec(ctx, `DELETE FROM events WHERE watch_id = $1`, in.ID); err != nil {
			return fmt.Errorf("delete events: %w", err)
		}
		// No digest reschedule, as the cascade it replaces: what an emptied rule held from an earlier scope goes out when due.
		if _, err := tx.Exec(ctx, `DELETE FROM rules r USING rule_watches rw
			WHERE rw.watch_id = $1 AND rw.rule_id = r.id AND NOT r.all_watches
				AND NOT EXISTS (SELECT 1 FROM rule_watches o WHERE o.rule_id = r.id AND o.watch_id <> $1)`, in.ID); err != nil {
			return fmt.Errorf("delete emptied rules: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM watches WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch.deleted", TargetType: "watch", TargetID: in.ID, Before: before.Spec})
	})
	if err != nil {
		return nil, s.fail("delete-watch", err)
	}
	return nil, nil
}
