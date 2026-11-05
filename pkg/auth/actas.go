package auth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// actAsPath is the one admin operation a session acting as a user may call: the way out.
const actAsPath = AdminPrefix + "act-as"

// Admin is the user who signed in, as /me shows it while the session acts as someone else.
type Admin struct {
	ID          string `json:"id" format:"uuid"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// actingState is the audited before and after of an act-as start or stop (006-AC18).
type actingState struct {
	ActingAs *string `json:"acting_as"`
}

// label names a user as the UI does: its display name, else its email.
func label(u User) string { return cmp.Or(u.DisplayName, u.Email, u.ID) }

func (s *Service) registerActAs(api huma.API, tags []string) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-act-as-start", Method: http.MethodPost, Path: AdminPrefix + "users/{id}/act-as",
		Summary: "Run this session as another account until stopped; audited on both sides (admin only)", Tags: tags,
		DefaultStatus: http.StatusNoContent,
	}, s.actAsStart)
	huma.Register(api, huma.Operation{
		OperationID: "admin-act-as-stop", Method: http.MethodDelete, Path: actAsPath,
		Summary: "Return this session to the admin's own account", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.actAsStop)
}

// actAsStart points the admin's session at another account (006-AC17, 006-AC18). The target's row is
// read FOR SHARE first: a purge holding it (NO KEY UPDATE) ends before the status is read, and the session
// row comes after (users before sessions).
func (s *Service) actAsStart(ctx context.Context, in *userIDPath) (*struct{}, error) {
	sess, err := sessionOf(ctx)
	if err != nil {
		return nil, err
	}
	var target string
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		// u.id::text is the canonical form: the path accepts any case.
		err := tx.QueryRow(ctx, `SELECT u.id::text, `+statusSQL+` FROM users u WHERE u.id = $1 FOR SHARE`, in.ID).Scan(&target, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("no such account")
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		// A disabled account is allowed: an admin may fix its data before enabling it.
		switch {
		case target == sess.ID:
			return huma.Error409Conflict("you cannot act as yourself")
		case status == statusPurged:
			return huma.Error409Conflict("the account is purged")
		}
		tag, err := tx.Exec(ctx, `UPDATE sessions SET acting_as = $2 WHERE id = $1 AND acting_as IS NULL`, sess.id, target)
		if err != nil {
			return fmt.Errorf("start acting: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Another tab started first, or dev mode answered without a stored session.
			return huma.Error409Conflict("this session already acts as someone, or is not stored")
		}
		return audit.Record(audit.WithActing(ctx, sess.ID, target), tx, audit.Entry{Action: "session.act_as_start",
			TargetType: "session", TargetID: auditID(sess.id), Before: actingState{}, After: actingState{ActingAs: &target}})
	})
	if err != nil {
		return nil, s.apiFail("admin-act-as-start", err)
	}
	s.log.Info("act_as_started", zap.String("user_id", target), zap.String("by", sess.ID))
	return nil, nil
}

// actAsStop returns the session to its admin (006-AC17, 006-AC18).
func (s *Service) actAsStop(ctx context.Context, _ *struct{}) (*struct{}, error) {
	sess, err := sessionOf(ctx)
	if err != nil {
		return nil, err
	}
	admin := sess.self().ID
	var target *string
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		target = nil // No row leaves Scan's target alone: a retry must not see the failed attempt's.
		err := tx.QueryRow(ctx, `SELECT acting_as::text FROM sessions WHERE id = $1 FOR UPDATE`, sess.id).Scan(&target)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock session: %w", err)
		}
		if target == nil {
			return huma.Error409Conflict("this session is not acting as anyone")
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET acting_as = NULL WHERE id = $1`, sess.id); err != nil {
			return fmt.Errorf("stop acting: %w", err)
		}
		// From the row, not the request: another tab may have switched the target since this one looked.
		return audit.Record(audit.WithActing(ctx, admin, *target), tx, audit.Entry{Action: "session.act_as_stop",
			TargetType: "session", TargetID: auditID(sess.id), Before: actingState{ActingAs: target}, After: actingState{}})
	})
	if err != nil {
		return nil, s.apiFail("admin-act-as-stop", err)
	}
	s.log.Info("act_as_stopped", zap.String("user_id", *target), zap.String("by", admin))
	return nil, nil
}
