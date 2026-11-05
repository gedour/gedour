package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

type meActing struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"is_admin"`
	ActingAs *Admin `json:"acting_as"`
}

func (e *env) meOf(t *testing.T, token string) meActing {
	t.Helper()
	code, raw := e.send(t, http.MethodGet, "/api/v1/me", token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("me: %d %s", code, raw)
	}
	var m meActing
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) actAs(t *testing.T, token, csrf, id string) (int, []byte) {
	t.Helper()
	return e.send(t, http.MethodPost, "/api/v1/admin/users/"+id+"/act-as", token, csrf, nil)
}

func (e *env) stopActing(t *testing.T, token, csrf string) (int, []byte) {
	t.Helper()
	return e.send(t, http.MethodDelete, "/api/v1/admin/act-as", token, csrf, nil)
}

type actingRow struct {
	Actor, OnBehalfOf, Target string
	Before, After             actingState
}

func (e *env) actingRows(t *testing.T, action string) []actingRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT actor_user_id::text, COALESCE(on_behalf_of::text, ''), target_id, before, after
		FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []actingRow
	for rows.Next() {
		var r actingRow
		if err := rows.Scan(&r.Actor, &r.OnBehalfOf, &r.Target, &r.Before, &r.After); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// 006-AC15: while acting, /me is the target plus the admin in acting_as, is_admin stays the session's.
// 006-AC17: stopping returns /me to the admin. 006-AC18: start and stop audited with the admin as actor.
func TestActAsStartStop(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	bob, _, _ := e.signedIn(t, "bob", false)
	if code, raw := e.actAs(t, token, csrf, bob); code != http.StatusNoContent {
		t.Fatalf("start: %d %s", code, raw)
	}
	m := e.meOf(t, token)
	if m.ID != bob || m.Email != "bob@x.test" || !m.IsAdmin || m.ActingAs == nil || m.ActingAs.ID != admin || m.ActingAs.Email != "admin@x.test" {
		t.Fatalf("me while acting: %+v", m)
	}
	if code, raw := e.stopActing(t, token, csrf); code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", code, raw)
	}
	if m := e.meOf(t, token); m.ID != admin || m.ActingAs != nil || !m.IsAdmin {
		t.Fatalf("me after stop: %+v", m)
	}
	for action, want := range map[string]actingRow{
		"session.act_as_start": {Before: actingState{}, After: actingState{ActingAs: &bob}},
		"session.act_as_stop":  {Before: actingState{ActingAs: &bob}, After: actingState{}},
	} {
		rows := e.actingRows(t, action)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", action, len(rows))
		}
		r := rows[0]
		if r.Actor != admin || r.OnBehalfOf != bob || r.Target != auditID(hashToken(token)) ||
			deref(r.Before.ActingAs) != deref(want.Before.ActingAs) || deref(r.After.ActingAs) != deref(want.After.ActingAs) {
			t.Errorf("%s: %+v", action, r)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// 006-AC17: refused for oneself, a purged account and a session already acting; 404 for an unknown one;
// a disabled account is allowed. Stop without acting is 409; a non-admin is 403 on both.
func TestActAsRefusals(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	bob, _, _ := e.signedIn(t, "bob", false)
	carol, _, _ := e.signedIn(t, "carol", false)
	gone, _, _ := e.signedIn(t, "gone", false)
	_, userToken, userCSRF := e.signedIn(t, "user", false)
	e.exec(t, `UPDATE users SET disabled_at = now(), purged_at = now() WHERE id = $1`, gone)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, bob)
	for _, tt := range []struct {
		name string
		code int
		call func() (int, []byte)
	}{
		{"stop while not acting", http.StatusConflict, func() (int, []byte) { return e.stopActing(t, token, csrf) }},
		{"self", http.StatusConflict, func() (int, []byte) { return e.actAs(t, token, csrf, admin) }},
		{"self, upper case", http.StatusConflict, func() (int, []byte) { return e.actAs(t, token, csrf, strings.ToUpper(admin)) }},
		{"purged", http.StatusConflict, func() (int, []byte) { return e.actAs(t, token, csrf, gone) }},
		{"unknown", http.StatusNotFound, func() (int, []byte) { return e.actAs(t, token, csrf, "00000000-0000-0000-0000-000000000000") }},
		{"non-admin start", http.StatusForbidden, func() (int, []byte) { return e.actAs(t, userToken, userCSRF, bob) }},
		{"non-admin stop", http.StatusForbidden, func() (int, []byte) { return e.stopActing(t, userToken, userCSRF) }},
		{"disabled", http.StatusNoContent, func() (int, []byte) { return e.actAs(t, token, csrf, bob) }},
		{"already acting", http.StatusConflict, func() (int, []byte) { return e.actAs(t, token, csrf, carol) }},
	} {
		if code, raw := tt.call(); code != tt.code {
			t.Errorf("%s: %d %s, want %d", tt.name, code, raw, tt.code)
		}
	}
	if m := e.meOf(t, token); m.ID != bob {
		t.Fatalf("a refused start moved the session: %+v", m)
	}
	if n := len(e.actingRows(t, "session.act_as_start")); n != 1 {
		t.Fatalf("%d act_as_start rows, want 1", n)
	}
}

// A second start racing on the same session row (another tab) finds it acting once the first commits.
func TestActAsSecondStartOnSameSession(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	bob, _, _ := e.signedIn(t, "bob", false)
	carol, _, _ := e.signedIn(t, "carol", false)
	first := e.begin(t)
	if _, err := first.Exec(context.Background(), `UPDATE sessions SET acting_as = $2 WHERE id = $1`, hashToken(token), bob); err != nil {
		t.Fatal(err)
	}
	second := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+carol+"/act-as", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	if err := first.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := <-second; code != http.StatusConflict {
		t.Fatalf("second start: %d, want 409", code)
	}
}

// Act-as start reads its target FOR SHARE before touching the session: one queued behind a purge holding
// the account sees it purged (409). Reading it unlocked would pass and leave the session on a purged account.
func TestActAsWaitsForPurge(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	ctx := context.Background()
	purge := e.begin(t)
	if _, err := purge.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, dave); err != nil {
		t.Fatal(err)
	}
	if _, err := purge.Exec(ctx, `UPDATE users SET email = '', display_name = '', purged_at = now() WHERE id = $1`, dave); err != nil {
		t.Fatal(err)
	}
	start := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/act-as", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	if err := purge.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-start; code != http.StatusConflict {
		t.Fatalf("start behind a purge: %d, want 409", code)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE acting_as IS NOT NULL`); n != 0 {
		t.Fatal("a session acts as the purged account")
	}
}

// 006-AC13: a purge sends every admin session acting as the account back to its admin, in its tx, and
// counts them in the preview and the audit; another admin's session stays as it was.
func TestPurgeClearsActingSessions(t *testing.T) {
	e := setup(t, false)
	one, tokenOne, csrfOne := e.signedIn(t, "one", true)
	_, tokenTwo, csrfTwo := e.signedIn(t, "two", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	if code, raw := e.actAs(t, tokenOne, csrfOne, dave); code != http.StatusNoContent {
		t.Fatalf("start: %d %s", code, raw)
	}
	_, tokenThree, csrfThree := e.signedIn(t, "three", true)
	if code, raw := e.actAs(t, tokenThree, csrfThree, erin); code != http.StatusNoContent {
		t.Fatalf("start: %d %s", code, raw)
	}
	code, raw := e.send(t, http.MethodGet, "/api/v1/admin/users/"+dave+"/purge-preview", tokenTwo, "", nil)
	var preview struct {
		Counts PurgeCounts `json:"counts"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &preview) != nil || preview.Counts.ActingSessions != 1 {
		t.Fatalf("preview: %d %s", code, raw)
	}
	if code, raw := e.purge(t, tokenTwo, csrfTwo, dave); code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", code, raw)
	}
	if n := e.count(t, `SELECT (after -> 'deleted' ->> 'acting_sessions')::int FROM audit_log WHERE action = 'user.purge'`); n != preview.Counts.ActingSessions {
		t.Fatalf("audit acting_sessions %d, preview %d", n, preview.Counts.ActingSessions)
	}
	if m := e.meOf(t, tokenOne); m.ID != one || m.ActingAs != nil {
		t.Fatalf("acting admin after the purge: %+v", m)
	}
	if m := e.meOf(t, tokenThree); m.ID != erin {
		t.Fatalf("the purge touched another acting session: %+v", m)
	}
}

// Signing out while acting ends the admin's session, audited with the admin as actor for the target.
func TestLogoutWhileActing(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	bob, _, _ := e.signedIn(t, "bob", false)
	if code, raw := e.actAs(t, token, csrf, bob); code != http.StatusNoContent {
		t.Fatalf("start: %d %s", code, raw)
	}
	if code, raw := e.send(t, http.MethodPost, "/auth/logout", token, csrf, nil); code != http.StatusOK {
		t.Fatalf("logout: %d %s", code, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE id = $1`, hashToken(token)); n != 0 {
		t.Fatal("the session survived the logout")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'session.logout' AND actor_user_id = $1 AND on_behalf_of = $2`, admin, bob); n != 1 {
		t.Fatal("logout not audited as the admin on behalf of bob")
	}
}

// A failed session write is an opaque 500 that leaves the session as it was, for start and stop.
func TestActAsDatabaseFailure(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	bob, _, _ := e.signedIn(t, "bob", false)
	e.exec(t, `CREATE FUNCTION boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END; $$`)
	const arm = `CREATE TRIGGER boom BEFORE UPDATE OF acting_as ON sessions FOR EACH ROW EXECUTE FUNCTION boom()`
	e.exec(t, arm)
	if code, raw := e.actAs(t, token, csrf, bob); code != http.StatusInternalServerError || strings.Contains(string(raw), "boom") {
		t.Fatalf("start: %d %s", code, raw)
	}
	e.exec(t, `DROP TRIGGER boom ON sessions`)
	e.exec(t, `UPDATE sessions SET acting_as = $2 WHERE id = $1`, hashToken(token), bob)
	e.exec(t, arm)
	if code, raw := e.stopActing(t, token, csrf); code != http.StatusInternalServerError || strings.Contains(string(raw), "boom") {
		t.Fatalf("stop: %d %s", code, raw)
	}
	if m := e.meOf(t, token); m.ID != bob {
		t.Fatalf("a failed stop moved the session: %+v", m)
	}
}

// A handler mounted outside Guard finds no session: 401, never a nil dereference.
func TestSessionOfOutsideGuard(t *testing.T) {
	var se huma.StatusError
	if _, err := sessionOf(context.Background()); !errors.As(err, &se) || se.GetStatus() != http.StatusUnauthorized {
		t.Fatalf("sessionOf without Guard: %v", err)
	}
	if u, ok := UserFrom(context.Background()); ok || u != (User{}) {
		t.Fatalf("UserFrom without Guard: %+v %v", u, ok)
	}
}

// Dev mode has no session a stream could outlive: SessionAlive holds even without a cookie.
func TestSessionAliveDevMode(t *testing.T) {
	e := setup(t, true)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/inbox/stream", nil)
	if !e.svc.SessionAlive(req) {
		t.Fatal("dev mode stream cut")
	}
}

// Stop takes its target from the session row it locks, not from the request's lookup: a switch committed
// between the two is what the stop row records.
func TestActAsStopReadsLockedRow(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	bob, _, _ := e.signedIn(t, "bob", false)
	carol, _, _ := e.signedIn(t, "carol", false)
	if code, raw := e.actAs(t, token, csrf, bob); code != http.StatusNoContent {
		t.Fatalf("start: %d %s", code, raw)
	}
	ctx := context.Background()
	hold := e.begin(t)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, hashToken(token)); err != nil {
		t.Fatal(err)
	}
	stop := e.sendAsync(http.MethodDelete, "/api/v1/admin/act-as", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	if _, err := hold.Exec(ctx, `UPDATE sessions SET acting_as = $2 WHERE id = $1`, hashToken(token), carol); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-stop; code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
	if rows := e.actingRows(t, "session.act_as_stop"); len(rows) != 1 || rows[0].OnBehalfOf != carol || deref(rows[0].Before.ActingAs) != carol {
		t.Fatalf("stop row: %+v, want carol from the locked row", rows)
	}
}

// Stop holds its session row, then its audit insert checks the target (KEY SHARE); a purge of that target
// holds it, then clears the sessions acting as it. The purge's NO KEY UPDATE lets the stop through: no
// deadlock, both succeed. The purge is held past its user lock by one of the account's rules.
func TestActAsStopRacingPurge(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "one", true)
	_, tokenTwo, csrfTwo := e.signedIn(t, "two", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	r := e.ruleOf(t, dave, "r", nil)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	if code, raw := e.actAs(t, token, csrf, dave); code != http.StatusNoContent {
		t.Fatalf("start: %d %s", code, raw)
	}
	ctx := context.Background()
	hold := e.begin(t)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, r); err != nil {
		t.Fatal(err)
	}
	purge := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", tokenTwo, csrfTwo, nil)
	e.waitLockWaiters(t, 1)
	stop := e.sendAsync(http.MethodDelete, "/api/v1/admin/act-as", token, csrf, nil)
	stopCode := 0
	select {
	case stopCode = <-stop:
	case <-e.lockWaiters(2):
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if stopCode == 0 {
		stopCode = <-stop
	}
	if purgeCode := <-purge; stopCode != http.StatusNoContent || purgeCode != http.StatusNoContent {
		t.Fatalf("stop %d, purge %d: want 204 both", stopCode, purgeCode)
	}
}

// Logout holds its session row, then its audit insert checks the admin (KEY SHARE); a disable of that admin
// holds them, then deletes their sessions. The disable's NO KEY UPDATE lets the logout through. A share lock
// on audit_log parks the logout right before its insert.
func TestLogoutRacingDisable(t *testing.T) {
	e := setup(t, false)
	one, token, csrf := e.signedIn(t, "one", true)
	_, tokenTwo, csrfTwo := e.signedIn(t, "two", true)
	ctx := context.Background()
	hold := e.begin(t)
	if _, err := hold.Exec(ctx, `LOCK TABLE audit_log IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	logout := e.sendAsync(http.MethodPost, "/auth/logout", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	disable := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+one+"/disable", tokenTwo, csrfTwo, nil)
	e.waitLockWaiters(t, 2)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if l, d := <-logout, <-disable; l != http.StatusOK || d != http.StatusNoContent {
		t.Fatalf("logout %d, disable %d: want 200 and 204", l, d)
	}
}
