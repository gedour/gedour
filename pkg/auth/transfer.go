package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
)

// AdminWatch is one watch an account owns, as the admin transfer view lists it.
type AdminWatch struct {
	ID             string  `json:"id" format:"uuid"`
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	CredentialName *string `json:"credential_name" nullable:"true" doc:"Null when the watch uses no credential"`
}

type listUserWatchesOutput struct {
	Body struct {
		Items []AdminWatch `json:"items" doc:"Sorted by name"`
	}
}

type transferInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		ToUserID string `json:"to_user_id" format:"uuid" doc:"The new owner, an active account"`
	}
}

// transferBefore is the audited state before a transfer (006-AC18).
type transferBefore struct {
	OwnerID      string  `json:"owner_id"`
	CredentialID *string `json:"credential_id"`
}

// TransferResult is the state after a transfer, answered and audited; it never holds a secret (006-AC18).
type TransferResult struct {
	OwnerID         string  `json:"owner_id" format:"uuid"`
	CredentialID    *string `json:"credential_id" nullable:"true"`
	CredentialMoved bool    `json:"credential_moved"`
	CredentialName  *string `json:"credential_name" nullable:"true" doc:"Its name at the new owner, suffixed on a clash"`
}

type transferOutput struct {
	Body TransferResult
}

func (s *Service) registerTransfer(api huma.API, tags []string) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-list-user-watches", Method: http.MethodGet, Path: AdminPrefix + "users/{id}/watches",
		Summary: "The watches an account owns, for a transfer (admin only)", Tags: tags,
	}, s.listUserWatches)
	huma.Register(api, huma.Operation{
		OperationID: "admin-transfer-watch", Method: http.MethodPost, Path: AdminPrefix + "watches/{id}/transfer",
		Summary: "Give a watch, and its credential, to another active account (admin only)", Tags: tags,
	}, s.transferWatch)
}

// sessionOf is the session Guard put in ctx; handlers outside Guard (tests) get a 401.
func sessionOf(ctx context.Context) (*session, error) {
	sess, ok := ctx.Value(ctxKey{}).(*session)
	if !ok {
		return nil, huma.Error401Unauthorized("not signed in")
	}
	return sess, nil
}

// adminActor is the signed-in admin's user id.
func adminActor(ctx context.Context) (string, error) {
	sess, err := sessionOf(ctx)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

func (s *Service) listUserWatches(ctx context.Context, in *userIDPath) (*listUserWatchesOutput, error) {
	out := &listUserWatchesOutput{}
	err := s.pool.QueryRow(ctx, `SELECT coalesce((SELECT json_agg(json_build_object('id', w.id, 'name', w.name, 'kind', w.kind, 'credential_name', c.name) ORDER BY lower(w.name), w.id)
		FROM watches w LEFT JOIN credentials c ON c.id = w.credential_id WHERE w.owner_id = u.id), '[]')
		FROM users u WHERE u.id = $1`, in.ID).Scan(&out.Body.Items)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, huma.Error404NotFound("no such account")
	}
	if err != nil {
		return nil, s.apiFail("admin-list-user-watches", fmt.Errorf("list watches: %w", err))
	}
	return out, nil
}

// transferWatch moves a watch to another account in one tx (006-AC10, 006-AC11, 006-AC18); locks follow the
// order in AGENTS.md. The old owner's rules on it stay and route nothing while it does not own the watch
// (a transfer back restores routing).
func (s *Service) transferWatch(ctx context.Context, in *transferInput) (*transferOutput, error) {
	// Canonical whatever spelling format:"uuid" let through: lockTransferUsers matches ids read back as text.
	id, err := uuid.Parse(in.Body.ToUserID)
	if err != nil {
		return nil, huma.Error400BadRequest("validation failed", &huma.ErrorDetail{Location: "body.to_user_id", Message: "expected a UUID", Value: in.Body.ToUserID})
	}
	to := id.String()
	actor, err := adminActor(ctx)
	if err != nil {
		return nil, err
	}
	var res TransferResult
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		res = TransferResult{OwnerID: to}
		var owner string
		err := tx.QueryRow(ctx, `SELECT owner_id::text FROM watches WHERE id = $1`, in.ID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("no such watch")
		}
		if err != nil {
			return fmt.Errorf("read watch: %w", err)
		}
		if err := lockTransferUsers(ctx, tx, owner, to); err != nil {
			return err
		}
		var before transferBefore
		err = tx.QueryRow(ctx, `SELECT owner_id::text, credential_id::text FROM watches WHERE id = $1 FOR UPDATE`, in.ID).
			Scan(&before.OwnerID, &before.CredentialID)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("no such watch")
		}
		if err != nil {
			return fmt.Errorf("lock watch: %w", err)
		}
		if before.OwnerID != owner {
			return huma.Error409Conflict("the watch changed owner meanwhile: reload and retry")
		}
		res.CredentialID = before.CredentialID
		if before.CredentialID != nil {
			if res.CredentialName, err = moveCredential(ctx, tx, in.ID, *before.CredentialID, before.OwnerID, to); err != nil {
				return err
			}
			res.CredentialMoved = true
		}
		if _, err := tx.Exec(ctx, `UPDATE watches SET owner_id = $2, updated_at = now() WHERE id = $1`, in.ID, to); err != nil {
			return fmt.Errorf("update watch: %w", err)
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: actor, Action: "watch.transfer", TargetType: "watch", TargetID: in.ID,
			Before: before, After: res})
	})
	if err != nil {
		return nil, s.apiFail("admin-transfer-watch", err)
	}
	s.log.Info("watch_transferred", zap.String("watch_id", in.ID), zap.String("to", to), zap.String("by", actor))
	return &transferOutput{Body: res}, nil
}

// lockTransferUsers share-locks both accounts in id order and checks them: a purge, disable or sign-in waits
// for the transfer, while the FK checks of the users' own writes (KEY SHARE) go through.
func lockTransferUsers(ctx context.Context, tx pgx.Tx, from, to string) error {
	if from == to {
		return huma.Error409Conflict("the account already owns this watch")
	}
	rows, err := tx.Query(ctx, `SELECT u.id::text, `+statusSQL+` FROM users u WHERE u.id IN ($1, $2) ORDER BY u.id FOR SHARE`, from, to)
	if err != nil {
		return fmt.Errorf("lock users: %w", err)
	}
	status := map[string]string{}
	var id, st string
	if _, err := pgx.ForEachRow(rows, []any{&id, &st}, func() error { status[id] = st; return nil }); err != nil {
		return fmt.Errorf("lock users: %w", err)
	}
	switch {
	case status[to] == "":
		return huma.Error404NotFound("no such account")
	case status[from] == statusPurged:
		return huma.Error409Conflict("the current owner is purged")
	case status[to] != statusActive:
		return huma.Error409Conflict("the new owner is " + status[to])
	}
	return nil
}

// moveCredential hands the watch's credential to the new owner, renamed on a clash, unless another watch of
// the old owner still uses it. The secret is not touched: its AAD is the credential id, not the owner.
func moveCredential(ctx context.Context, tx pgx.Tx, watch, cred, from, to string) (*string, error) {
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM credentials WHERE id = $1 FOR UPDATE`, cred).Scan(&name); err != nil {
		return nil, fmt.Errorf("lock credential: %w", err)
	}
	// A statement of its own, after the lock: its snapshot sees an attach committed while the lock waited.
	var others, taken []string
	if err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT name FROM watches WHERE credential_id = $1 AND owner_id = $2 AND id <> $3 ORDER BY lower(name), id),
			ARRAY(SELECT name FROM credentials WHERE owner_id = $4)`, cred, from, watch, to).Scan(&others, &taken); err != nil {
		return nil, fmt.Errorf("credential users: %w", err)
	}
	if len(others) > 0 {
		details := make([]error, len(others))
		for i, n := range others {
			details[i] = &huma.ErrorDetail{Location: "watches", Message: "uses the same credential", Value: n}
		}
		return nil, huma.Error409Conflict("the credential is also used by "+strings.Join(others, ", "), details...)
	}
	name = freeName(name, taken)
	_, err := tx.Exec(ctx, `UPDATE credentials SET owner_id = $2, name = $3, updated_at = now() WHERE id = $1`, cred, to, name)
	// A same-name credential the new owner creates or renames meanwhile surfaces through the unique index.
	if isNameClash(err) {
		return nil, huma.Error409Conflict("the new owner changed a credential name meanwhile: retry the transfer")
	}
	if err != nil {
		return nil, fmt.Errorf("move credential: %w", err)
	}
	return &name, nil
}

// freeName is name, or the first of "name (transferred)", "name (transferred 2)"... not in taken.
func freeName(name string, taken []string) string {
	used := make(map[string]bool, len(taken))
	for _, t := range taken {
		used[t] = true
	}
	for i, n := 1, name; ; i++ {
		if !used[n] {
			return n
		}
		n = name + " (transferred)"
		if i > 1 {
			n = name + " (transferred " + strconv.Itoa(i) + ")"
		}
	}
}

// isNameClash reports a credential name taken at its owner (UNIQUE (owner_id, name)).
func isNameClash(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "credentials_owner_id_name_key"
}
