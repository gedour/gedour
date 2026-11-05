package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type listNames struct {
	Items []struct {
		ID   any    `json:"id"`
		Name string `json:"name"`
	} `json:"items"`
}

type actingAuditPage struct {
	Items []struct {
		Action         string  `json:"action"`
		ActorUserID    *string `json:"actor_user_id"`
		ActorName      string  `json:"actor_name"`
		OnBehalfOf     *string `json:"on_behalf_of"`
		OnBehalfOfName string  `json:"on_behalf_of_name"`
		TargetID       string  `json:"target_id"`
	} `json:"items"`
}

// 006-AC15: acting as bob, alice's owner-scoped lists (watches, rules, channels, inbox) and the inbox
// stream are bob's. 006-AC16: a rule she creates meanwhile is audited with her as actor and bob as
// on_behalf_of, and shows in bob's audit view and, once she stops (006-AC17), in hers.
func TestActAsRunsAsTarget(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/rules", map[string]any{"name": "bob's rule"}, http.StatusCreated, nil)
	a.exec(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'bob hook', 'webhook')`, bob.id)
	a.exec(t, `WITH e AS (INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id)
		INSERT INTO inbox_items (user_id, event_id) SELECT $2, id FROM e`, w.ID, bob.id)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/admin/users/"+bob.id+"/act-as", nil, http.StatusNoContent, nil)

	for path, want := range map[string]string{"/api/v1/watches": "Go releases", "/api/v1/rules": "bob's rule", "/api/v1/channels": "bob hook"} {
		var got listNames
		a.mustCall(t, alice, http.MethodGet, path, nil, http.StatusOK, &got)
		if len(got.Items) != 1 || got.Items[0].Name != want {
			t.Errorf("%s while acting: %+v, want bob's %q", path, got.Items, want)
		}
	}
	if ids := a.inboxIDs(t, alice, "all"); len(ids) != 1 {
		t.Errorf("inbox while acting: %v, want bob's one item", ids)
	}
	_, events := openStream(t, a.replica(t, time.Hour, false), alice)
	if ev := next(t, events, "inbox", 2*time.Second); ev.data != `{"unread":1}` {
		t.Errorf("stream while acting: %+v, want bob's count", ev)
	}

	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "made by alice"}, http.StatusCreated, &rule)
	if n := a.countRows(t, `SELECT count(*) FROM rules WHERE id = $1 AND owner_id = $2`, rule.ID, bob.id); n != 1 {
		t.Fatal("the rule created while acting is not bob's")
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'rule.created' AND target_id = $1 AND actor_user_id = $2 AND on_behalf_of = $3`,
		rule.ID, alice.id, bob.id); n != 1 {
		t.Fatal("rule.created not audited as alice on behalf of bob")
	}
	seen := func(u apiUser) bool {
		t.Helper()
		var page actingAuditPage
		a.mustCall(t, u, http.MethodGet, "/api/v1/audit?target_type=rule", nil, http.StatusOK, &page)
		for _, it := range page.Items {
			if it.TargetID == rule.ID && it.ActorUserID != nil && *it.ActorUserID == alice.id && it.ActorName == "alice" &&
				it.OnBehalfOf != nil && *it.OnBehalfOf == bob.id && it.OnBehalfOfName == "bob" {
				return true
			}
		}
		return false
	}
	if !seen(bob) {
		t.Error("bob's audit view misses the entry made on his behalf")
	}
	// Only on_behalf_of shows bob the act-as start: its target is alice's session.
	var sessions actingAuditPage
	a.mustCall(t, bob, http.MethodGet, "/api/v1/audit?target_type=session", nil, http.StatusOK, &sessions)
	if len(sessions.Items) != 1 || sessions.Items[0].Action != "session.act_as_start" {
		t.Errorf("bob's session entries: %+v, want alice's act-as start", sessions.Items)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/admin/act-as", nil, http.StatusNoContent, nil)
	var own listNames
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules", nil, http.StatusOK, &own)
	if len(own.Items) != 0 {
		t.Errorf("alice's rules after stop: %+v, want her own (none)", own.Items)
	}
	if !seen(alice) {
		t.Error("alice's audit view misses what she did as bob")
	}
}

// A change made acting as a disabled account holds its row, then its audit insert checks that account
// (on_behalf_of, KEY SHARE); a purge of it holds the account, then deletes its rules. The purge's NO KEY
// UPDATE lets the change through: no deadlock. A share lock on audit_log parks the change before its insert.
func TestActingEditRacingPurge(t *testing.T) {
	a := newDevApp(t)
	alice, bob, dave := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "dave")
	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, dave, http.MethodPost, "/api/v1/rules", map[string]any{"name": "dave's"}, http.StatusCreated, &rule)
	a.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave.id)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/act-as", nil, http.StatusNoContent, nil)
	ctx := context.Background()
	hold, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `LOCK TABLE audit_log IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	edit := a.callAsync(alice, http.MethodPatch, "/api/v1/rules/"+rule.ID, map[string]any{"name": "renamed"})
	a.waitLockWaiters(t, 1)
	purge := a.callAsync(bob, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/purge", nil)
	a.waitLockWaiters(t, 2)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if e, p := <-edit, <-purge; e != http.StatusOK || p != http.StatusNoContent {
		t.Fatalf("edit %d, purge %d: want 200 and 204", e, p)
	}
}

// A share holds its target user (FOR SHARE, so it waits on a purge) before the watch, as the lock order
// puts users first. The target creating a rule on that watch holds itself (the cap lock), then the watch
// (KEY SHARE): with the watch locked first the two would deadlock.
func TestAddShareRacingRuleCreate(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "ro"}, http.StatusCreated, nil)
	ctx := context.Background()
	create, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = create.Rollback(ctx) }()
	if _, err := create.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, bob.id); err != nil {
		t.Fatal(err)
	}
	share := a.callAsync(alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "rw"})
	a.waitLockWaiters(t, 1)
	if _, err := create.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w.ID); err != nil {
		t.Fatalf("rule create's watch check: %v", err)
	}
	if err := create.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-share; code != http.StatusConflict {
		t.Fatalf("share: %d, want 409 (already shared)", code)
	}
}

// The channel test cooldown is the effective account's: two tests acting as bob, the second within the
// interval, are throttled; once alice stops, her own first test is not throttled by the one made as bob.
func TestChannelTestCooldownWhileActing(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	hook := map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": srv.URL}}
	var bobs, alices channelJSON
	a.mustCall(t, bob, http.MethodPost, "/api/v1/channels", hook, http.StatusCreated, &bobs)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", hook, http.StatusCreated, &alices)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/admin/users/"+bob.id+"/act-as", nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+bobs.ID+"/test", nil, http.StatusOK, nil)
	if code, raw := a.call(t, alice, http.MethodPost, "/api/v1/channels/"+bobs.ID+"/test", nil); code != http.StatusTooManyRequests {
		t.Errorf("second test as bob: %d %s, want 429", code, raw)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/admin/act-as", nil, http.StatusNoContent, nil)
	if code, raw := a.call(t, alice, http.MethodPost, "/api/v1/channels/"+alices.ID+"/test", nil); code != http.StatusOK {
		t.Errorf("alice's own first test: %d %s, want 200", code, raw)
	}
}
