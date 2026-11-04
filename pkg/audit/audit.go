// Package audit records every mutation in the append-only audit_log, inside the mutation's transaction.
//
// Contract: Before and After are stored as their encoding/json form, then every secret-looking key
// (see IsSecretKey) is redacted. That deny-list is a safety net, not the mechanism: a type passed here
// must not carry secrets in its JSON form (tag secret fields `json:"-"`, or pass the API response type).
package audit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Entry is one audited mutation. Before and After are any JSON-marshalable value (nil = absent).
type Entry struct {
	ActorUserID string // empty for system actions
	Action      string // e.g. "user.login"
	TargetType  string
	TargetID    string
	Before      any
	After       any
	RequestID   string // defaults to the request ID carried by ctx
}

type ctxKey struct{}

// WithRequestID tags ctx so entries recorded under it carry the request's ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the ID set by WithRequestID, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Record inserts e within tx; a failure must abort the caller's transaction so the mutation rolls back.
// 001-AC21: same transaction as the mutation, secrets redacted.
func Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	before, err := redactedJSON(e.Before)
	if err != nil {
		return fmt.Errorf("audit before: %w", err)
	}
	after, err := redactedJSON(e.After)
	if err != nil {
		return fmt.Errorf("audit after: %w", err)
	}
	if e.RequestID == "" {
		e.RequestID = RequestID(ctx)
	}
	var actor *string
	if e.ActorUserID != "" {
		actor = &e.ActorUserID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor_user_id, action, target_type, target_id, before, after, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, actor, e.Action, e.TargetType, e.TargetID, before, after, e.RequestID)
	if err != nil {
		return fmt.Errorf("audit insert %s: %w", e.Action, err)
	}
	return nil
}
