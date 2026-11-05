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
	OtherUsersRules int `json:"other_users_rules" doc:"Other users' rules targeting the account's watches"`
	Scripts         int `json:"scripts"`
	// Not "credentials": the audit redacts every key naming a credential but a few exact ones.
	Credentials    int `json:"credential_count"`
	SharesReceived int `json:"shares_received" doc:"Watches of other users shared with the account"`
	InboxItems     int `json:"inbox_items"`
	Sessions       int `json:"sessions"`
	ActingSessions int `json:"acting_sessions" doc:"Admin sessions acting as the account, sent back to their admin's own data"`
}

// SharedWatch is an owned watch other users lose with the purge.
type SharedWatch struct {
	ID         string   `json:"id" format:"uuid"`
	Name       string   `json:"name"`
	SharedWith []string `json:"shared_with" doc:"Names (or emails) of the users it is shared with"`
}

// OtherRule is another user's rule a purge deletes because it targets one of the account's watches.
type OtherRule struct {
	ID    string `json:"id" format:"uuid"`
	Name  string `json:"name"`
	Owner string `json:"owner" doc:"Its owner's name, or email"`
}

type purgePreviewOutput struct {
	Body struct {
		Counts        PurgeCounts   `json:"counts"`
		SharedWatches []SharedWatch `json:"shared_watches" doc:"Sorted by name"`
		OtherRules    []OtherRule   `json:"other_rules" doc:"Sorted by owner, then name"`
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
		Summary: "What purging an account deletes, other users' shared watches and rules included (admin only)", Tags: tags,
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
		(SELECT count(*) FROM rules r JOIN watches w ON w.id = r.watch_id WHERE w.owner_id = u.id AND r.owner_id <> u.id),
		(SELECT count(*) FROM scripts WHERE owner_id = u.id),
		(SELECT count(*) FROM credentials WHERE owner_id = u.id),
		(SELECT count(*) FROM watch_shares WHERE user_id = u.id),
		(SELECT count(*) FROM inbox_items WHERE user_id = u.id),
		(SELECT count(*) FROM sessions WHERE user_id = u.id),
		(SELECT count(*) FROM sessions WHERE acting_as = u.id),
		(SELECT coalesce(json_agg(json_build_object('id', s.id, 'name', s.name, 'shared_with', s.users) ORDER BY lower(s.name), s.id), '[]')
			FROM (SELECT w.id, w.name, array_agg(` + userLabelSQL + ` ORDER BY lower(` + userLabelSQL + `)) AS users
				FROM watches w JOIN watch_shares ws ON ws.watch_id = w.id JOIN users o ON o.id = ws.user_id
				WHERE w.owner_id = u.id GROUP BY w.id, w.name) s),
		(SELECT coalesce(json_agg(json_build_object('id', r.id, 'name', r.name, 'owner', ` + userLabelSQL + `)
				ORDER BY lower(` + userLabelSQL + `), lower(r.name), r.id), '[]')
			FROM rules r JOIN watches w ON w.id = r.watch_id JOIN users o ON o.id = r.owner_id
			WHERE w.owner_id = u.id AND r.owner_id <> u.id)
	FROM users u WHERE u.id = $1`

// userLabelSQL names users row o as the UI does: its display name, else its email.
const userLabelSQL = `COALESCE(NULLIF(o.display_name, ''), o.email)`

func (s *Service) purgePreview(ctx context.Context, in *userIDPath) (*purgePreviewOutput, error) {
	out := &purgePreviewOutput{}
	b := &out.Body
	c := &b.Counts
	err := s.pool.QueryRow(ctx, purgePreviewSQL, in.ID).Scan(&c.Watches, &c.Channels, &c.Rules, &c.OtherUsersRules, &c.Scripts,
		&c.Credentials, &c.SharesReceived, &c.InboxItems, &c.Sessions, &c.ActingSessions, &b.SharedWatches, &b.OtherRules)
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
// account's own rows (inbox, shares received, sessions) go before its watches, whose cascade would remove
// some uncounted. A nil count is a lock only.
var purgeSteps = []struct {
	sql   string
	count func(*PurgeCounts) *int
}{
	{`WITH d AS (DELETE FROM rules WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Rules }},
	// NO KEY UPDATE: the router's FK KEY SHARE on a rule it routes passes, else it would wait while holding
	// the events the cascade waits on. TestPurgeRacingRouter guards it.
	{`SELECT count(*) FROM (SELECT 1 FROM rules WHERE watch_id IN (SELECT id FROM watches WHERE owner_id = $1) ORDER BY id FOR NO KEY UPDATE) x`, nil},
	// From here a rule create or move onto these watches (KEY SHARE on the watch) waits for the purge.
	{`SELECT count(*) FROM (SELECT 1 FROM watches WHERE owner_id = $1 ORDER BY id FOR UPDATE) x`, nil},
	// A statement of its own, after the watch lock: it counts a rule committed while that lock waited.
	{`SELECT count(*) FROM (SELECT 1 FROM rules WHERE watch_id IN (SELECT id FROM watches WHERE owner_id = $1) FOR NO KEY UPDATE) x`,
		func(c *PurgeCounts) *int { return &c.OtherUsersRules }},
	{`WITH d AS (DELETE FROM channels WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Channels }},
	{`WITH d AS (DELETE FROM inbox_items WHERE user_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.InboxItems }},
	{`WITH d AS (DELETE FROM watch_shares WHERE user_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.SharesReceived }},
	{`WITH d AS (DELETE FROM sessions WHERE user_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Sessions }},
	// The row stays, so ON DELETE SET NULL never fires: other admins' sessions acting as it end here.
	{`WITH d AS (UPDATE sessions SET acting_as = NULL WHERE acting_as = $1 RETURNING 1) SELECT count(*) FROM d`,
		func(c *PurgeCounts) *int { return &c.ActingSessions }},
	// Cascades to other users' rules on them, events (and their inbox items and deliveries), shares, states, cache.
	{`WITH d AS (DELETE FROM watches WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Watches }},
	{`WITH d AS (DELETE FROM credentials WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Credentials }},
	{`WITH d AS (DELETE FROM scripts WHERE owner_id = $1 RETURNING 1) SELECT count(*) FROM d`, func(c *PurgeCounts) *int { return &c.Scripts }},
}

// purgeUser deletes a disabled account's data in one tx under its row lock and blanks its personal data;
// the row stays, with its oidc_sub, so the audit log keeps its actor and the same person stays refused
// (006-AC12, 006-AC13, 006-AC18).
func (s *Service) purgeUser(ctx context.Context, in *userIDPath) (*struct{}, error) {
	actor, err := adminActor(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		// NO KEY UPDATE: sign-in, act-as start, shares and transfers still wait, FK checks (audit rows of a
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
