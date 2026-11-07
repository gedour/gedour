package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// signedIn stores a user and a live session for it, as a sign-in would, and returns the user id and the
// session token and CSRF token.
func (e *env) signedIn(t *testing.T, sub string, admin bool) (id, token, csrf string) {
	t.Helper()
	token, csrf = randomToken(), randomToken()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, last_login_at) VALUES ($1, $1 || '@x.test', now()) RETURNING id::text`, sub).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO sessions (id, user_id, csrf_token, expires_at, is_admin) VALUES ($1, $2, $3, now() + interval '1 hour', $4)`,
		hashToken(token), id, csrf, admin); err != nil {
		t.Fatal(err)
	}
	return id, token, csrf
}

// call sends one request with the given session, outside e.client's cookie jar.
func (e *env) call(t *testing.T, method, path, token, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.app.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(e.svc.sessionCookie(token, 60))
	req.Header.Set(CSRFHeader, csrf)
	resp, err := e.app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (e *env) adminUsers(t *testing.T, token string) []AdminUser {
	t.Helper()
	resp := e.call(t, http.MethodGet, "/api/v1/admin/users", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list users: %d", resp.StatusCode)
	}
	var out struct {
		Items []AdminUser `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Items
}

// noShareKeys fails on any JSON key of raw, at any depth, that names a share.
func noShareKeys(t *testing.T, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if strings.Contains(strings.ToLower(k), "share") {
					t.Errorf("key %q in %s", k, raw)
				}
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// 006-AC6: every account, never signed in first then oldest sign-in, with its status and counts.
// 009-AC9: no count names a share.
func TestAdminListUsers(t *testing.T) {
	e := setup(t, false)
	_, token, _ := e.signedIn(t, "admin", true)
	user := func(sub, created string, lastLogin *string, status string) string {
		var id string
		if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (oidc_sub, email, display_name, created_at, last_login_at, disabled_at, purged_at)
			VALUES ($1, $1 || '@x.test', upper($1), $2::timestamptz, now() - $3::interval,
				CASE WHEN $4 IN ('disabled', 'purged') THEN now() END, CASE WHEN $4 = 'purged' THEN now() END) RETURNING id::text`,
			sub, created, lastLogin, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ago := func(s string) *string { return &s }
	never := user("never", "2026-01-01", nil, "active")
	never2 := user("never2", "2026-01-02", nil, "active")
	disabled := user("disabled", "2026-01-01", ago("3 days"), "disabled")
	old := user("old", "2026-01-03", ago("2 days"), "active")
	recent := user("recent", "2026-01-01", ago("1 hour"), "active")
	purged := user("purged", "2026-01-01", ago("30 minutes"), "purged")

	var cred string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO credentials (id, owner_id, name, kind, secret_ciphertext, secret_nonce, key_version)
		VALUES (gen_random_uuid(), $1, 'c', 'github_token', '\x00', '\x00', 1) RETURNING id::text`, old).Scan(&cred); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `INSERT INTO watches (owner_id, name, kind, config, credential_id) VALUES ($1, 'a', 'github_repo', '{}', $2)`, old, cred)
	e.exec(t, `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'b', 'github_repo', '{}'), ($2, 'c', 'github_repo', '{}')`, old, recent)
	e.exec(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'ch', 'webhook')`, old)
	e.exec(t, `INSERT INTO rules (owner_id, name, filter) VALUES ($1, 'r1', ''), ($1, 'r2', '')`, old)
	e.exec(t, `INSERT INTO scripts (owner_id, name, source) VALUES ($1, 's', '')`, old)

	got := e.adminUsers(t, token)
	var order []string
	for _, u := range got {
		order = append(order, u.Email)
	}
	want := []string{"never@x.test", "never2@x.test", "disabled@x.test", "old@x.test", "recent@x.test", "purged@x.test", "admin@x.test"}
	if !slices.Equal(order, want) {
		t.Fatalf("order %q, want %q", order, want)
	}
	byID := map[string]AdminUser{}
	for _, u := range got {
		byID[u.ID] = u
	}
	for id, status := range map[string]string{never: "active", never2: "active", disabled: "disabled", old: "active", purged: "purged"} {
		if byID[id].Status != status {
			t.Errorf("%s: status %q, want %q", byID[id].Email, byID[id].Status, status)
		}
	}
	if byID[never].LastLoginAt != nil || byID[old].LastLoginAt == nil || byID[disabled].DisabledAt == nil || byID[old].DisabledAt != nil {
		t.Errorf("timestamps: never %v, old %v, disabled %v", byID[never].LastLoginAt, byID[old].LastLoginAt, byID[disabled].DisabledAt)
	}
	if c := byID[old].Counts; c != (AdminUserCounts{Watches: 2, Channels: 1, Rules: 2, Scripts: 1, Credentials: 1}) {
		t.Errorf("old counts %+v", c)
	}
	if c := byID[recent].Counts; c != (AdminUserCounts{Watches: 1}) {
		t.Errorf("recent counts %+v", c)
	}
	_, raw := e.send(t, http.MethodGet, "/api/v1/admin/users", token, "", nil)
	noShareKeys(t, raw)
	if byID[never2].DisplayName != "NEVER2" || byID[never2].CreatedAt.IsZero() {
		t.Errorf("never2: %+v", byID[never2])
	}
}

type auditRow struct {
	Actor, Target string
	Before, After userState
}

func (e *env) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT actor_user_id::text, target_id, before, after FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Actor, &r.Target, &r.Before, &r.After); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// 006-AC7: disabling revokes every session at once and refuses the next sign-in with forbidden.
// 006-AC9: enabling restores sign-in, and nothing the account owns was lost. 006-AC18: both audited.
func TestDisableEnable(t *testing.T) {
	e := setup(t, false, withRoles)
	admin, token, csrf := e.signedIn(t, "admin", true)
	e.stub.sub = "bob-sub"
	e.stub.extra = map[string]any{"roles": []any{"gedour-user"}}
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/" {
		t.Fatalf("bob sign-in: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, bob := e.me(t)
	// Due times left stale by the disable; a paused watch and another owner's watch.
	e.exec(t, `INSERT INTO watches (owner_id, name, kind, config, paused, next_run_at) VALUES
		($1, 'w', 'github_repo', '{}', false, '2000-01-01'), ($1, 'p', 'github_repo', '{}', true, '2000-01-01'),
		($1, 'f', 'github_repo', '{}', false, '2999-01-01'), ($1, 'l', 'github_repo', '{}', false, '2000-01-01'), ($2, 'a', 'github_repo', '{}', false, '2000-01-01'),
		($1, 'x', 'github_repo', '{}', false, '2000-01-01')`, bob.ID, admin)
	e.exec(t, `UPDATE watches SET leased_until = now() + interval '1 hour' WHERE name = 'l'`)
	// A crashed worker's lease, past its end: the worker treats the watch as free, so the enable must too.
	e.exec(t, `UPDATE watches SET leased_until = now() - interval '1 hour' WHERE name = 'x'`)
	e.exec(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'ch', 'webhook')`, bob.ID)
	e.exec(t, `INSERT INTO rules (owner_id, name, filter) VALUES ($1, 'r', '')`, bob.ID)
	// A second session of bob's, from another browser.
	other := randomToken()
	e.exec(t, `INSERT INTO sessions (id, user_id, csrf_token, expires_at) VALUES ($1, $2, 'x', now() + interval '1 hour')`, hashToken(other), bob.ID)
	countsOf := func() AdminUserCounts {
		for _, u := range e.adminUsers(t, token) {
			if u.ID == bob.ID {
				return u.Counts
			}
		}
		t.Fatal("bob not listed")
		return AdminUserCounts{}
	}
	before := countsOf()

	post := func(id, op string, want int) {
		t.Helper()
		if resp := e.call(t, http.MethodPost, "/api/v1/admin/users/"+id+"/"+op, token, csrf); resp.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d", op, id, resp.StatusCode, want)
		}
	}
	post(bob.ID, "enable", http.StatusConflict)
	post(bob.ID, "disable", http.StatusNoContent)
	if code, _ := e.me(t); code != http.StatusUnauthorized {
		t.Fatalf("bob's session after disable: %d", code)
	}
	if resp := e.call(t, http.MethodGet, "/api/v1/me", other, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bob's other session after disable: %d", resp.StatusCode)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, bob.ID); n != 0 {
		t.Fatalf("%d sessions left", n)
	}
	if resp := e.fullLogin(t, "/watches"); resp.Header.Get("Location") != "/watches?auth=forbidden" || cookieNamed(resp, SecureSessionCookie) != nil {
		t.Fatalf("disabled sign-in: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, bob.ID); n != 0 {
		t.Fatalf("refused sign-in opened %d sessions", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied' AND actor_user_id = $1 AND after = '{"reason": "disabled"}'`, bob.ID); n != 1 {
		t.Fatalf("login_denied audit rows: %d", n)
	}
	post(bob.ID, "disable", http.StatusConflict)
	post(bob.ID, "enable", http.StatusNoContent)
	// 009-AC12: enabling moves a stale due time to now, not at the head of the queue; held or leased ones stay.
	if n := e.count(t, `SELECT count(*) FROM watches WHERE owner_id = $1 AND name IN ('w', 'x') AND next_run_at BETWEEN now() - interval '1 minute' AND now()`, bob.ID); n != 2 {
		t.Fatalf("%d of bob's unpaused watches, one with an expired lease, rescheduled to now; want 2", n)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE name IN ('p', 'a') AND next_run_at = '2000-01-01'`); n != 2 {
		t.Fatalf("%d of the paused and other owner's watches kept their due time, want 2", n)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE name = 'f' AND next_run_at = '2999-01-01'`); n != 1 {
		t.Fatal("bob's watch held to a future due time was pulled forward")
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE name = 'l' AND next_run_at = '2000-01-01'`); n != 1 {
		t.Fatal("bob's leased watch got a new due time, which its worker reads as a run-now")
	}
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/" {
		t.Fatalf("re-enabled sign-in: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, m := e.me(t); code != http.StatusOK || m.ID != bob.ID {
		t.Fatalf("me after enable: %d %+v", code, m)
	}
	if after := countsOf(); after != before || after.Watches != 5 || after.Channels != 1 || after.Rules != 1 {
		t.Fatalf("counts %+v before, %+v after", before, after)
	}

	dis, en := e.auditRows(t, "user.disable"), e.auditRows(t, "user.enable")
	if len(dis) != 1 || len(en) != 1 {
		t.Fatalf("audit rows: %d disable, %d enable", len(dis), len(en))
	}
	d, n := dis[0], en[0]
	if d.Actor != admin || d.Target != bob.ID || d.Before != (userState{Status: "active"}) || d.After.Status != "disabled" || d.After.DisabledAt == nil {
		t.Fatalf("disable audit %+v", d)
	}
	if n.Actor != admin || n.Target != bob.ID || n.Before.Status != "disabled" || n.Before.DisabledAt == nil ||
		!n.Before.DisabledAt.Equal(*d.After.DisabledAt) || n.After != (userState{Status: "active"}) {
		t.Fatalf("enable audit %+v", n)
	}
}

// 006-AC7: an admin cannot disable themselves; unknown and purged accounts are refused.
func TestDisableRefusals(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	purged, _, _ := e.signedIn(t, "purged", false)
	e.exec(t, `UPDATE users SET disabled_at = now(), purged_at = now() WHERE id = $1`, purged)
	for _, tt := range []struct {
		id, op string
		want   int
	}{
		{admin, "disable", http.StatusConflict},
		{admin, "enable", http.StatusConflict},
		{"00000000-0000-0000-0000-000000000000", "disable", http.StatusNotFound},
		{"00000000-0000-0000-0000-000000000000", "enable", http.StatusNotFound},
		{purged, "disable", http.StatusConflict},
		{purged, "enable", http.StatusConflict},
	} {
		if resp := e.call(t, http.MethodPost, "/api/v1/admin/users/"+tt.id+"/"+tt.op, token, csrf); resp.StatusCode != tt.want {
			t.Errorf("%s %s: %d, want %d", tt.op, tt.id, resp.StatusCode, tt.want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action IN ('user.disable', 'user.enable')`); n != 0 {
		t.Fatalf("refused calls wrote %d audit rows", n)
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE id = $1 AND disabled_at IS NULL`, admin); n != 1 {
		t.Fatal("the admin got disabled")
	}
}

// 006-AC7: in dev mode the dev user is the admin and cannot disable itself.
func TestDevUserCannotDisableItself(t *testing.T) {
	e := setup(t, true)
	_, m := e.me(t)
	if resp := e.do(t, http.MethodPost, "/api/v1/admin/users/"+m.ID+"/disable", map[string]string{CSRFHeader: m.CSRFToken}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("dev self-disable: %d", resp.StatusCode)
	}
}

// 006-AC7: a session row left on a disabled account authenticates nothing.
func TestLookupIgnoresDisabledUser(t *testing.T) {
	e := setup(t, false)
	id, token, _ := e.signedIn(t, "bob", false)
	if resp := e.call(t, http.MethodGet, "/api/v1/me", token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("me: %d", resp.StatusCode)
	}
	for _, col := range []string{"disabled_at", "purged_at"} {
		e.exec(t, `UPDATE users SET disabled_at = NULL, purged_at = NULL WHERE id = $1`, id)
		e.exec(t, `UPDATE users SET `+col+` = now() WHERE id = $1`, id)
		if resp := e.call(t, http.MethodGet, "/api/v1/me", token, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("me with %s set: %d", col, resp.StatusCode)
		}
	}
}

// 006-AC7: a disable committed while a sign-in waits on the user row lock leaves no session: the login
// tx reads the status the disable wrote, not the one before it.
func TestLoginRechecksDisabledUnderLock(t *testing.T) {
	e := setup(t, false)
	id, _, _ := e.signedIn(t, "bob-sub", false)
	e.exec(t, `DELETE FROM sessions`)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/auth/callback", nil)
		_, err := e.svc.login(ctx, httptest.NewRecorder(), r, identity{Sub: "bob-sub"})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the login never waited on the user row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errAccountDisabled) {
		t.Fatalf("login: %v, want errAccountDisabled", err)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions on a disabled account", n)
	}
}
