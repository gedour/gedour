package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// Account statuses as the admin API reports them.
const (
	statusActive   = "active"
	statusDisabled = "disabled"
	statusPurged   = "purged"
)

// AdminUserCounts is what an account owns.
type AdminUserCounts struct {
	Watches     int `json:"watches"`
	Channels    int `json:"channels"`
	Rules       int `json:"rules"`
	Scripts     int `json:"scripts"`
	Credentials int `json:"credentials"`
}

// AdminUser is one account in the admin list.
type AdminUser struct {
	ID          string          `json:"id" format:"uuid"`
	Email       string          `json:"email"`
	DisplayName string          `json:"display_name"`
	CreatedAt   time.Time       `json:"created_at"`
	LastLoginAt *time.Time      `json:"last_login_at" nullable:"true" doc:"Null when the account never signed in"`
	Status      string          `json:"status" enum:"active,disabled,purged"`
	DisabledAt  *time.Time      `json:"disabled_at" nullable:"true"`
	Counts      AdminUserCounts `json:"counts"`
}

type listUsersOutput struct {
	Body struct {
		Items []AdminUser `json:"items"`
	}
}

type userIDPath struct {
	ID string `path:"id" format:"uuid"`
}

// userState is the audited before and after of a disable or an enable (006-AC18).
type userState struct {
	Status     string     `json:"status"`
	DisabledAt *time.Time `json:"disabled_at"`
	// Not "revoked_api_keys": the audit redacts every name ending in key or keys.
	RevokedKeys int64 `json:"revoked_api_key_count,omitempty"`
}

// staleDueToNow moves a watch's past due time to now so it does not jump other owners' work; a future one (a
// rate-limit hold) and a leased one stay, as the worker reads a changed due time as a run-now (009-AC12).
const staleDueToNow = `CASE WHEN leased_until > now() THEN next_run_at ELSE GREATEST(next_run_at, now()) END`

const statusSQL = `CASE WHEN u.purged_at IS NOT NULL THEN 'purged' WHEN u.disabled_at IS NOT NULL THEN 'disabled' ELSE 'active' END`

// 006-AC6: never signed in first, then oldest sign-in; counts in the same query.
const adminUsersSQL = `SELECT u.id::text, u.email, u.display_name, u.created_at, u.last_login_at, ` + statusSQL + `, u.disabled_at,
		(SELECT count(*) FROM watches w WHERE w.owner_id = u.id),
		(SELECT count(*) FROM channels c WHERE c.owner_id = u.id),
		(SELECT count(*) FROM rules r WHERE r.owner_id = u.id),
		(SELECT count(*) FROM scripts sc WHERE sc.owner_id = u.id),
		(SELECT count(*) FROM credentials cr WHERE cr.owner_id = u.id)
	FROM users u
	ORDER BY u.last_login_at ASC NULLS FIRST, u.created_at, u.id`

func (s *Service) registerAdmin(api huma.API) {
	tags := []string{"admin"}
	huma.Register(api, huma.Operation{
		OperationID: "admin-list-users", Method: http.MethodGet, Path: AdminPrefix + "users",
		Summary: "Every account with its last sign-in, status and what it owns (admin only)", Tags: tags,
	}, s.listUsers)
	huma.Register(api, huma.Operation{
		OperationID: "admin-disable-user", Method: http.MethodPost, Path: AdminPrefix + "users/{id}/disable",
		Summary: "Disable an account: sessions revoked, sign-in refused, rules route nothing, watches stop collecting (admin only)", Tags: tags,
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *userIDPath) (*struct{}, error) {
		return nil, s.setDisabled(ctx, in.ID, true)
	})
	huma.Register(api, huma.Operation{
		OperationID: "admin-enable-user", Method: http.MethodPost, Path: AdminPrefix + "users/{id}/enable",
		Summary: "Enable a disabled account again (admin only)", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *userIDPath) (*struct{}, error) {
		return nil, s.setDisabled(ctx, in.ID, false)
	})
	s.registerTransfer(api, tags)
	s.registerPurge(api, tags)
	s.registerActAs(api, tags)
}

// apiFail passes API errors through and turns anything else into a logged, opaque 500.
func (s *Service) apiFail(op string, err error) error {
	var se huma.StatusError
	if errors.As(err, &se) {
		return se
	}
	s.log.Error("auth_api_failed", zap.String("operation", op), zap.Error(err))
	return huma.Error500InternalServerError("internal error")
}

func (s *Service) listUsers(ctx context.Context, _ *struct{}) (*listUsersOutput, error) {
	rows, err := s.pool.Query(ctx, adminUsersSQL)
	if err != nil {
		return nil, s.apiFail("admin-list-users", fmt.Errorf("list users: %w", err))
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminUser, error) {
		var u AdminUser
		c := &u.Counts
		err := r.Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.LastLoginAt, &u.Status, &u.DisabledAt,
			&c.Watches, &c.Channels, &c.Rules, &c.Scripts, &c.Credentials)
		return u, err
	})
	if err != nil {
		return nil, s.apiFail("admin-list-users", fmt.Errorf("list users: %w", err))
	}
	out := &listUsersOutput{}
	out.Body.Items = items
	if out.Body.Items == nil {
		out.Body.Items = []AdminUser{}
	}
	return out, nil
}

// setDisabled disables or enables an account under its row lock (NO KEY UPDATE: FK checks on the user, such as
// the audit insert of a logout holding its session, never wait on it); disabling deletes its sessions in the
// same tx (006-AC7, 006-AC9, 006-AC18).
func (s *Service) setDisabled(ctx context.Context, id string, disable bool) error {
	sess, err := sessionOf(ctx)
	if err != nil {
		return err
	}
	action, op := "user.enable", "admin-enable-user"
	if disable {
		action, op = "user.disable", "admin-disable-user"
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid string
		var before userState
		err := tx.QueryRow(ctx, `SELECT u.id::text, `+statusSQL+`, u.disabled_at FROM users u WHERE u.id = $1 FOR NO KEY UPDATE`, id).
			Scan(&uid, &before.Status, &before.DisabledAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("no such account")
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		switch {
		case disable && uid == sess.ID:
			return huma.Error409Conflict("you cannot disable your own account")
		case before.Status == statusPurged:
			return huma.Error409Conflict("the account is purged")
		case disable && before.Status == statusDisabled:
			return huma.Error409Conflict("the account is already disabled")
		case !disable && before.Status == statusActive:
			return huma.Error409Conflict("the account is not disabled")
		}
		after := userState{Status: statusActive}
		if err := tx.QueryRow(ctx, `UPDATE users SET disabled_at = CASE WHEN $2 THEN now() END WHERE id = $1 RETURNING disabled_at`,
			uid, disable).Scan(&after.DisabledAt); err != nil {
			return fmt.Errorf("update user: %w", err)
		}
		if disable {
			after.Status = statusDisabled
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, uid); err != nil {
				return fmt.Errorf("revoke sessions: %w", err)
			}
			if after.RevokedKeys, err = revokeActiveKeys(ctx, tx, uid); err != nil {
				return err
			}
		} else {
			// Watches in id order after the user (AGENTS.md).
			if _, err := tx.Exec(ctx, `UPDATE watches w SET next_run_at = `+staleDueToNow+`
				WHERE w.id IN (SELECT x.id FROM watches x WHERE NOT x.paused AND x.owner_id = $1 ORDER BY x.id FOR NO KEY UPDATE)`, uid); err != nil {
				return fmt.Errorf("reschedule watches: %w", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: sess.ID, Action: action, TargetType: "user", TargetID: uid,
			Before: before, After: after})
	})
	if err != nil {
		return s.apiFail(op, err)
	}
	s.log.Info("user_status_changed", zap.String("user_id", id), zap.String("action", action), zap.String("by", sess.ID))
	return nil
}
