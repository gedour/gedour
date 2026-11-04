package watch

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// Share grants one user access to a watch.
type Share struct {
	UserID      string    `json:"user_id" format:"uuid"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Permission  string    `json:"permission" enum:"ro,rw"`
	CreatedAt   time.Time `json:"created_at"`
}

type shareOutput struct {
	Body Share
}

type listSharesOutput struct {
	Body struct {
		Items []Share `json:"items" doc:"Sorted by email"`
	}
}

type sharePath struct {
	ID     string `path:"id" format:"uuid"`
	UserID string `path:"user_id" format:"uuid"`
}

type addShareInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Email      string `json:"email" minLength:"3" maxLength:"320" doc:"Exact email of a user who has signed in at least once"`
		Permission string `json:"permission" enum:"ro,rw"`
	}
}

type updateShareInput struct {
	ID     string `path:"id" format:"uuid"`
	UserID string `path:"user_id" format:"uuid"`
	Body   struct {
		Permission string `json:"permission" enum:"ro,rw"`
	}
}

const shareSelect = `SELECT s.user_id::text, u.email, u.display_name, s.permission, s.created_at
	FROM watch_shares s JOIN users u ON u.id = s.user_id WHERE s.watch_id = $1`

func scanShares(rows pgx.Rows) ([]Share, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Share, error) {
		var sh Share
		err := r.Scan(&sh.UserID, &sh.Email, &sh.DisplayName, &sh.Permission, &sh.CreatedAt)
		return sh, err
	})
}

func getShare(ctx context.Context, tx pgx.Tx, watchID, userID string) (Share, error) {
	rows, err := tx.Query(ctx, shareSelect+" AND s.user_id = $2", watchID, userID)
	if err != nil {
		return Share{}, err
	}
	shares, err := scanShares(rows)
	if err != nil {
		return Share{}, err
	}
	if len(shares) == 0 {
		return Share{}, huma.Error404NotFound("share not found")
	}
	return shares[0], nil
}

// shareTarget names a share in the audit log; the audit view reads the watch ID back from it.
func shareTarget(watchID, userID string) string { return watchID + ":" + userID }

func (s *Service) registerShares(api huma.API) {
	tags := []string{"shares"}
	huma.Register(api, huma.Operation{
		OperationID: "list-watch-shares", Method: http.MethodGet, Path: "/api/v1/watches/{id}/shares",
		Summary: "Users a watch is shared with; a sharee sees only their own share", Tags: tags,
	}, s.listShares)
	huma.Register(api, huma.Operation{
		OperationID: "add-watch-share", Method: http.MethodPost, Path: "/api/v1/watches/{id}/shares",
		Summary: "Share a watch with a user, by exact email (owner only)", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.addShare)
	huma.Register(api, huma.Operation{
		OperationID: "update-watch-share", Method: http.MethodPut, Path: "/api/v1/watches/{id}/shares/{user_id}",
		Summary: "Change a share's permission (owner only)", Tags: tags,
	}, s.updateShare)
	huma.Register(api, huma.Operation{
		OperationID: "delete-watch-share", Method: http.MethodDelete, Path: "/api/v1/watches/{id}/shares/{user_id}",
		Summary: "Stop sharing a watch with a user (owner only)", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteShare)
}

func (s *Service) listShares(ctx context.Context, in *idPath) (*listSharesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	out := &listSharesOutput{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		w, err := authorize(ctx, tx, me, in.ID, levelRead, false)
		if err != nil {
			return err
		}
		// A sharee sees only their own share: the other sharees' emails are the owner's business.
		sql, args := shareSelect, []any{in.ID}
		if w.Permission != "owner" {
			sql, args = sql+" AND s.user_id = $2", append(args, me)
		}
		rows, err := tx.Query(ctx, sql+" ORDER BY lower(u.email), s.user_id", args...)
		if err != nil {
			return err
		}
		out.Body.Items, err = scanShares(rows)
		return err
	})
	if err != nil {
		return nil, s.fail("list-watch-shares", err)
	}
	if out.Body.Items == nil {
		out.Body.Items = []Share{}
	}
	return out, nil
}

func (s *Service) addShare(ctx context.Context, in *addShareInput) (*shareOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(in.Body.Email)
	var sh Share
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		w, err := authorize(ctx, tx, me, in.ID, levelOwner, true)
		if err != nil {
			return err
		}
		// Exact (case-insensitive) match only: no search, so the endpoint cannot enumerate users.
		rows, err := tx.Query(ctx, `SELECT id::text FROM users WHERE email <> '' AND lower(email) = lower($1) LIMIT 2`, email)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		switch {
		case len(ids) == 0:
			return huma.Error404NotFound("no user with that email has signed in yet",
				&huma.ErrorDetail{Location: "body.email", Message: "no user with that email has signed in yet", Value: email})
		case len(ids) > 1:
			return huma.Error409Conflict("several accounts use that email",
				&huma.ErrorDetail{Location: "body.email", Message: "several accounts use that email", Value: email})
		case ids[0] == w.OwnerID:
			return huma.Error400BadRequest("invalid share",
				&huma.ErrorDetail{Location: "body.email", Message: "you cannot share a watch with yourself", Value: email})
		}
		tag, err := tx.Exec(ctx, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			in.ID, ids[0], in.Body.Permission)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return huma.Error409Conflict("already shared with this user; change its permission instead",
				&huma.ErrorDetail{Location: "body.email", Message: "already shared with this user", Value: email})
		}
		if sh, err = getShare(ctx, tx, in.ID, ids[0]); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch_share.added", TargetType: "watch_share",
			TargetID: shareTarget(in.ID, sh.UserID), After: sh})
	})
	if err != nil {
		return nil, s.fail("add-watch-share", err)
	}
	return &shareOutput{Body: sh}, nil
}

func (s *Service) updateShare(ctx context.Context, in *updateShareInput) (*shareOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	var after Share
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := authorize(ctx, tx, me, in.ID, levelOwner, true); err != nil {
			return err
		}
		before, err := getShare(ctx, tx, in.ID, in.UserID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE watch_shares SET permission = $3 WHERE watch_id = $1 AND user_id = $2`,
			in.ID, in.UserID, in.Body.Permission); err != nil {
			return err
		}
		after = before
		after.Permission = in.Body.Permission
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch_share.updated", TargetType: "watch_share",
			TargetID: shareTarget(in.ID, in.UserID), Before: before, After: after})
	})
	if err != nil {
		return nil, s.fail("update-watch-share", err)
	}
	return &shareOutput{Body: after}, nil
}

func (s *Service) deleteShare(ctx context.Context, in *sharePath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := authorize(ctx, tx, me, in.ID, levelOwner, true); err != nil {
			return err
		}
		before, err := getShare(ctx, tx, in.ID, in.UserID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM watch_shares WHERE watch_id = $1 AND user_id = $2`, in.ID, in.UserID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "watch_share.removed", TargetType: "watch_share",
			TargetID: shareTarget(in.ID, in.UserID), Before: before})
	})
	if err != nil {
		return nil, s.fail("delete-watch-share", err)
	}
	return nil, nil
}
