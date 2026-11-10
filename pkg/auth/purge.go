package auth

import (
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

// PurgeCounts is what a purge deletes, per type.
type PurgeCounts struct {
	Watches         int `json:"watches"`
	Channels        int `json:"channels"`
	Rules           int `json:"rules"`
	OtherUsersRules int `json:"other_users_rules" doc:"Other users' rules whose every watch is the account's"`
	Scripts         int `json:"scripts"`
	// Not "credentials": the audit redacts every key naming a credential but a few exact ones.
	Credentials    int `json:"credential_count"`
	InboxItems     int `json:"inbox_items"`
	Sessions       int `json:"sessions"`
	ActingSessions int `json:"acting_sessions" doc:"Admin sessions acting as the account, sent back to their admin's own data"`
	// Not "api_keys": the audit redacts every name ending in key or keys.
	APIKeys int `json:"api_key_count"`
}

// OtherRule is another user's rule a purge deletes because every watch it lists is the account's.
type OtherRule struct {
	ID    string `json:"id" format:"uuid"`
	Name  string `json:"name"`
	Owner string `json:"owner" doc:"Its owner's name, or email"`
}

type purgePreviewOutput struct {
	Body struct {
		Counts     PurgeCounts `json:"counts"`
		OtherRules []OtherRule `json:"other_rules" doc:"Sorted by owner, then name"`
	}
}

// purgeAfter is the audited state after a purge: counts only, never the erased email or name (006-AC18).
type purgeAfter struct {
	Status  string      `json:"status"`
	Deleted PurgeCounts `json:"deleted"`
}

func (s *Service) registerPurge(api huma.API, tags []string) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-purge-preview", Method: http.MethodGet, Path: AdminPrefix + "users/{id}/purge-preview",
		Summary: "What purging an account deletes, other users' rules on its watches included (admin only)", Tags: tags,
	}, s.purgePreview)
	huma.Register(api, huma.Operation{
		OperationID: "admin-purge-user", Method: http.MethodPost, Path: AdminPrefix + "users/{id}/purge",
		Summary: "Delete a disabled account's data and blank its email and name; the audit log stays (admin only)", Tags: tags,
		DefaultStatus: http.StatusNoContent,
	}, s.purgeUser)
}

// One statement, so the lists and the counts come from the same snapshot.
const purgePreviewSQL = `SELECT
		(SELECT count(*) FROM watches WHERE owner_id = u.id),
		(SELECT count(*) FROM channels WHERE owner_id = u.id),
		(SELECT count(*) FROM rules WHERE owner_id = u.id),
		(SELECT count(*) FROM rules r WHERE ` + otherRulesSQL + `),
		(SELECT count(*) FROM scripts WHERE owner_id = u.id),
		(SELECT count(*) FROM credentials WHERE owner_id = u.id),
		(SELECT count(*) FROM inbox_items WHERE user_id = u.id),
		(SELECT count(*) FROM sessions WHERE user_id = u.id),
		(SELECT count(*) FROM sessions WHERE acting_as = u.id),
		(SELECT count(*) FROM api_keys WHERE user_id = u.id),
		(SELECT coalesce(json_agg(json_build_object('id', r.id, 'name', r.name, 'owner', ` + userLabelSQL + `)
				ORDER BY lower(` + userLabelSQL + `), lower(r.name), r.id), '[]')
			FROM rules r JOIN users o ON o.id = r.owner_id
			WHERE ` + otherRulesSQL + `)
	FROM users u WHERE u.id = $1`

// otherRulesSQL picks the rules r of other users than u that a purge of u leaves with no watch (014-AC9).
const otherRulesSQL = `r.owner_id <> u.id AND NOT r.all_watches
	AND EXISTS (SELECT 1 FROM rule_watches rw JOIN watches w ON w.id = rw.watch_id WHERE rw.rule_id = r.id AND w.owner_id = u.id)
	AND NOT EXISTS (SELECT 1 FROM rule_watches rw JOIN watches w ON w.id = rw.watch_id WHERE rw.rule_id = r.id AND w.owner_id <> u.id)`

// userLabelSQL names users row o as the UI does: its display name, else its email.
const userLabelSQL = `COALESCE(NULLIF(o.display_name, ''), o.email)`

func (s *Service) purgePreview(ctx context.Context, in *userIDPath) (*purgePreviewOutput, error) {
	out := &purgePreviewOutput{}
	b := &out.Body
	c := &b.Counts
	err := s.pool.QueryRow(ctx, purgePreviewSQL, in.ID).Scan(&c.Watches, &c.Channels, &c.Rules, &c.OtherUsersRules, &c.Scripts,
		&c.Credentials, &c.InboxItems, &c.Sessions, &c.ActingSessions, &c.APIKeys, &b.OtherRules)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, huma.Error404NotFound("no such account")
	}
	if err != nil {
		return nil, s.apiFail("admin-purge-preview", fmt.Errorf("purge preview: %w", err))
	}
	return out, nil
}

// purgeSteps run in this order, which follows the lock order in AGENTS.md and keeps every ON DELETE RESTRICT
// quiet: rules before the channels they use (rule_channels), every watch before the credentials they use. The
// account's own rows (inbox, sessions) go before its watches, whose cascade would remove
// some uncounted. A nil count is not reported.
var purgeSteps = []struct {
	sql   string
	count func(*PurgeCounts) *int
}{
	// One id-ordered lock, as deleteWatch's; NO KEY UPDATE lets the router's FK KEY SHARE pass (TestPurgeRacingRouter).
	{`SELECT count(*) FROM (SELECT 1 FROM rules WHERE owner_id = $1 OR id IN (SELECT rw.rule_id FROM rule_watches rw
		JOIN watches w ON w.id = rw.watch_id WHERE w.owner_id = $1) ORDER BY id FOR NO KEY UPDATE) x`, nil},
	{`WITH d AS (DELETE FROM rules WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Rules }},
	// From here a rule create or move onto these watches (KEY SHARE on the watch) waits for the purge.
	{`SELECT count(*) FROM (SELECT 1 FROM watches WHERE owner_id = $1 ORDER BY id FOR UPDATE) x`, nil},
	// A statement of its own, after the watch lock: it locks a rule committed while that lock waited.
	{`SELECT count(*) FROM (SELECT 1 FROM rules WHERE id IN (SELECT rw.rule_id FROM rule_watches rw
		JOIN watches w ON w.id = rw.watch_id WHERE w.owner_id = $1) ORDER BY id FOR NO KEY UPDATE) x`, nil},
	{`WITH d AS (DELETE FROM channels WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Channels }},
	{`WITH d AS (DELETE FROM inbox_items WHERE user_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.InboxItems }},
	{`WITH d AS (DELETE FROM sessions WHERE user_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Sessions }},
	// The row stays, so ON DELETE SET NULL never fires: other admins' sessions acting as it end here.
	{`WITH d AS (UPDATE sessions SET acting_as = NULL WHERE acting_as = $1 RETURNING 1) SELECT count(*) FROM d`,
		func(c *PurgeCounts) *int { return &c.ActingSessions }},
	{`WITH d AS (DELETE FROM api_keys WHERE user_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.APIKeys }},
	// Events before rules: a router holding one takes its rules' KEY SHARE next, which a rule delete would block.
	{`WITH d AS (DELETE FROM events WHERE watch_id IN (SELECT id FROM watches WHERE owner_id = $1) RETURNING 1) SELECT count(*) FROM d`, nil},
	// Other users' rules left with no watch (014-AC9); the rest only lose the purged watches, through the cascade.
	{`WITH d AS (DELETE FROM rules r USING users u WHERE u.id = $1 AND ` + otherRulesSQL + ` RETURNING 1) SELECT count(*) FROM d`,
		func(c *PurgeCounts) *int { return &c.OtherUsersRules }},
	// Cascades to the rule_watches rows of other users' rules, states, cache.
	{`WITH d AS (DELETE FROM watches WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Watches }},
	{`WITH d AS (DELETE FROM credentials WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Credentials }},
	{`WITH d AS (DELETE FROM scripts WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Scripts }},
}

// purgeLockKey is the purge's advisory lock; goose's migration lock is 4097083626.
const purgeLockKey int64 = 6006

// purgeUser deletes a disabled account's data in one tx under its row lock and blanks its personal data;
// the row stays, with its oidc_sub, so the audit log keeps its actor and the same person stays refused
// (006-AC12, 006-AC13, 006-AC18).
func (s *Service) purgeUser(ctx context.Context, in *userIDPath) (*struct{}, error) {
	actor, err := adminActor(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Purges serialize: two accounts with rules on each other's watches would each hold what the other deletes.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, purgeLockKey); err != nil {
			return fmt.Errorf("purge lock: %w", err)
		}
		var status string
		// NO KEY UPDATE: sign-in, act-as start and transfers still wait, FK checks (audit rows of a
		// stop or of a change made acting as the account, holding rows this purge needs) do not.
		err := tx.QueryRow(ctx, `SELECT `+statusSQL+` FROM users u WHERE u.id = $1 FOR NO KEY UPDATE`, in.ID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return huma.Error404NotFound("no such account")
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		switch status {
		case statusPurged:
			return huma.Error409Conflict("the account is already purged")
		case statusActive:
			return huma.Error409Conflict("disable the account before purging it")
		}
		after := purgeAfter{Status: statusPurged}
		for _, step := range purgeSteps {
			dst := new(int)
			if step.count != nil {
				dst = step.count(&after.Deleted)
			}
			if err := tx.QueryRow(ctx, step.sql, in.ID).Scan(dst); err != nil {
				return fmt.Errorf("purge %q: %w", step.sql, err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET email = '', display_name = '', purged_at = now() WHERE id = $1`, in.ID); err != nil {
			return fmt.Errorf("blank user: %w", err)
		}
		// Only the status before: the email and name are the personal data this erases, and the log is append-only.
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: actor, Action: "user.purge", TargetType: "user", TargetID: in.ID,
			Before: map[string]string{"status": status}, After: after})
	})
	if err != nil {
		return nil, s.apiFail("admin-purge-user", err)
	}
	s.log.Info("user_purged", zap.String("user_id", in.ID), zap.String("by", actor))
	return nil, nil
}
