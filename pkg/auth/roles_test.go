package auth

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/config"
)

// 006-AC3: flat array, dotted path, literal key with dots; anything malformed is no role, never an error.
func TestRolesFrom(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		claim  string
		want   []string
	}{
		{"flat array", map[string]any{"roles": []any{"a", "b"}}, "roles", []string{"a", "b"}},
		{"dotted path", map[string]any{"realm_access": map[string]any{"roles": []any{"a"}}}, "realm_access.roles", []string{"a"}},
		{"deep path", map[string]any{"resource_access": map[string]any{"gedour": map[string]any{"roles": []any{"a"}}}}, "resource_access.gedour.roles", []string{"a"}},
		{"url key", map[string]any{"https://x.example/roles": []any{"a"}}, "https://x.example/roles", []string{"a"}},
		{"literal key wins over path", map[string]any{"a.b": []any{"lit"}, "a": map[string]any{"b": []any{"path"}}}, "a.b", []string{"lit"}},
		{"missing", map[string]any{"other": []any{"a"}}, "roles", nil},
		{"missing leaf", map[string]any{"realm_access": map[string]any{}}, "realm_access.roles", nil},
		{"string instead of array", map[string]any{"roles": "a"}, "roles", nil},
		{"object instead of array", map[string]any{"roles": map[string]any{"a": true}}, "roles", nil},
		{"null", map[string]any{"roles": nil}, "roles", nil},
		{"mixed types", map[string]any{"roles": []any{"a", 1.0, true, nil, map[string]any{}, []any{"x"}, "b"}}, "roles", []string{"a", "b"}},
		{"nested non-object", map[string]any{"realm_access": []any{"roles"}}, "realm_access.roles", nil},
		{"nested string", map[string]any{"realm_access": "roles"}, "realm_access.roles", nil},
		{"empty name", map[string]any{"roles": []any{"a"}}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rolesFrom(tt.claims, tt.claim); !slices.Equal(got, tt.want) {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
		})
	}
}

func withRoles(c *config.Config) {
	c.OIDCRolesClaim, c.OIDCRequiredRole, c.OIDCAdminRole = "roles", "gedour-user", "gedour-admin"
}

// 006-AC1: a token without the required role lands on the gate with reason forbidden, opens no session,
// creates no user and writes user.login_denied.
func TestLoginDeniedNewUser(t *testing.T) {
	e := setup(t, false, withRoles)
	e.stub.extra = map[string]any{"roles": []any{"other"}}
	resp := e.fullLogin(t, "/watches")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/watches?auth=forbidden" {
		t.Fatalf("callback: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if c := cookieNamed(resp, SecureSessionCookie); c != nil {
		t.Fatalf("session cookie set: %+v", c)
	}
	if code, _ := e.me(t); code != http.StatusUnauthorized {
		t.Fatalf("me: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM users`) + e.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("want no user nor session, got %d rows", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied' AND actor_user_id IS NULL
		AND target_type = 'user' AND target_id = 'alice-sub' AND after = '{"reason": "missing_role"}'`); n != 1 {
		t.Fatalf("login_denied audit rows: %d", n)
	}
}

// 006-AC1: a known user losing the role is refused; its row is left as is and the audit names it.
func TestLoginDeniedKnownUser(t *testing.T) {
	e := setup(t, false, withRoles)
	e.stub.extra = map[string]any{"roles": []any{"gedour-user"}}
	e.fullLogin(t, "/")
	_, m := e.me(t)
	e.stub.extra = map[string]any{"roles": "gedour-user"} // a single string is no role (006-AC3)
	e.stub.email = "alice@new.test"
	var before string
	if err := e.pool.QueryRow(t.Context(), `SELECT last_login_at::text FROM users WHERE id = $1`, m.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	resp := e.fullLogin(t, "/")
	if resp.Header.Get("Location") != "/?auth=forbidden" {
		t.Fatalf("callback: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE id = $1 AND email = 'alice@example.test' AND last_login_at::text = $2`, m.ID, before); n != 1 {
		t.Fatal("denied login changed the user row")
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("denied login must open no session, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied' AND actor_user_id::text = $1 AND target_id = $1`, m.ID); n != 1 {
		t.Fatalf("login_denied audit rows: %d", n)
	}
}

// 006-AC4: the admin role alone signs in as admin; the user role alone is not admin.
func TestAdminRole(t *testing.T) {
	for name, tt := range map[string]struct {
		roles []any
		admin bool
	}{
		"admin only":     {[]any{"gedour-admin"}, true},
		"admin and user": {[]any{"gedour-user", "gedour-admin"}, true},
		"user only":      {[]any{"gedour-user"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, false, withRoles)
			e.stub.extra = map[string]any{"roles": tt.roles}
			if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/" {
				t.Fatalf("callback: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
			}
			if code, m := e.me(t); code != http.StatusOK || m.IsAdmin != tt.admin {
				t.Fatalf("me: %d %+v", code, m)
			}
			if n := e.count(t, `SELECT count(*) FROM sessions WHERE is_admin = $1`, tt.admin); n != 1 {
				t.Fatalf("session is_admin rows: %d", n)
			}
		})
	}
}

// 006-AC2: without a required role any authenticated user signs in, roles or not, and is not admin.
func TestNoRequiredRole(t *testing.T) {
	defaults := func(c *config.Config) { c.OIDCRolesClaim = "roles" }
	for name, tt := range map[string]struct {
		opt   func(*config.Config)
		roles any
	}{
		"no roles claim": {defaults, nil},
		"other role":     {defaults, []any{"other"}},
		// An unset admin role must not match an empty role string.
		"empty role":     {defaults, []any{""}},
		"admin role set": {func(c *config.Config) { c.OIDCRolesClaim, c.OIDCAdminRole = "roles", "gedour-admin" }, []any{"other"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, false, tt.opt)
			if tt.roles != nil {
				e.stub.extra = map[string]any{"roles": tt.roles}
			}
			if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/" {
				t.Fatalf("callback: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
			}
			if code, m := e.me(t); code != http.StatusOK || m.IsAdmin {
				t.Fatalf("me: %d %+v", code, m)
			}
		})
	}
}

// 006-AC19: the cookieless dev path, which stores no session, runs as admin too.
func TestDevSessionAdmin(t *testing.T) {
	e := setup(t, true)
	sess, err := e.svc.devSession(t.Context())
	if err != nil || !sess.isAdmin {
		t.Fatalf("dev session: %+v %v", sess, err)
	}
}

// 006-AC19: a dev session still alive when the instance switches to OIDC is not admin there.
func TestDevSessionNotAdminUnderOIDC(t *testing.T) {
	e := setup(t, true)
	c := cookieNamed(e.do(t, http.MethodGet, "/api/v1/me", nil), SessionCookie)
	if c == nil {
		t.Fatal("no dev session cookie")
	}
	oidcSvc, err := New(config.Config{OIDCIssuer: "http://127.0.0.1:1", OIDCClientID: "gedour", OIDCClientSecret: "s",
		PublicURL: "https://app.test", SessionTTL: e.svc.ttl, SessionIdleTimeout: e.svc.idle}, e.pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(oidcSvc.sessionCookie(c.Value, 60))
	sess, err := oidcSvc.lookup(r, false)
	if err != nil || sess == nil || sess.isAdmin {
		t.Fatalf("dev session under OIDC: %+v %v", sess, err)
	}
}
