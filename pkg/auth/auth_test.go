package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

type env struct {
	app    *httptest.Server
	stub   *stubProvider
	pool   *pgxpool.Pool
	client *http.Client
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// setup serves Guard + Register on a TLS test server (the cookie jar only sends Secure cookies over https),
// plus a POST /api/v1/echo mutation to probe CSRF.
func setup(t *testing.T, dev bool) *env {
	t.Helper()
	e := &env{pool: newPool(t)}
	var h http.Handler
	e.app = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.app.Close)
	cfg := config.Config{SessionTTL: 7 * 24 * time.Hour, SessionIdleTimeout: 24 * time.Hour}
	if dev {
		cfg.DevUser = "dev@local"
	} else {
		e.stub = newStub(t)
		cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.PublicURL = e.stub.URL, "gedour", "s3cret", e.app.URL
	}
	svc, err := New(cfg, e.pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	hcfg := huma.DefaultConfig("t", "v")
	hcfg.OpenAPIPath = "/api/openapi"
	api := humago.New(mux, hcfg)
	svc.Register(api, mux)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		huma.Register(api, huma.Operation{OperationID: "echo-" + m, Method: m, Path: "/api/v1/echo"},
			func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	}
	h = svc.Guard(mux)

	jar, _ := cookiejar.New(nil)
	e.client = e.app.Client()
	e.client.Jar = jar
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return e
}

func (e *env) do(t *testing.T, method, rawURL string, hdr map[string]string) *http.Response {
	t.Helper()
	if strings.HasPrefix(rawURL, "/") {
		rawURL = e.app.URL + rawURL
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startLogin hits /auth/login and returns the provider authorize URL it redirected to.
func (e *env) startLogin(t *testing.T, returnTo string) *url.URL {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/auth/login?return_to="+url.QueryEscape(returnTo), nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: want 302, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// approve follows the provider's authorize step and returns the callback URL it sends back.
func (e *env) approve(t *testing.T, authorize *url.URL) *url.URL {
	t.Helper()
	resp := e.do(t, http.MethodGet, authorize.String(), nil)
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d %v", resp.StatusCode, err)
	}
	return cb
}

func (e *env) fullLogin(t *testing.T, returnTo string) *http.Response {
	t.Helper()
	return e.do(t, http.MethodGet, e.approve(t, e.startLogin(t, returnTo)).String(), nil)
}

type me struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	CSRFToken   string `json:"csrf_token"`
	DevMode     bool   `json:"dev_mode"`
}

func (e *env) me(t *testing.T) (int, me) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/api/v1/me", nil)
	var m me
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, m
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 001-AC1: login redirects with PKCE S256, state and nonce; the callback upserts the user by sub,
// opens a session and lands on return_to.
func TestLoginFlow(t *testing.T) {
	e := setup(t, false)

	auth := e.startLogin(t, "/watches?tab=1")
	q := auth.Query()
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(k)) < 32 {
			t.Errorf("%s missing or short: %q", k, q.Get(k))
		}
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || q.Get("client_id") != "gedour" ||
		q.Get("redirect_uri") != e.app.URL+"/auth/callback" || !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("authorize query: %v", q)
	}

	resp := e.do(t, http.MethodGet, e.approve(t, auth).String(), nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/watches?tab=1" {
		t.Fatalf("callback: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// 001-AC3: session cookie flags in OIDC mode.
	c := cookieNamed(resp, SecureSessionCookie)
	// __Host- prefix rules: Secure, Path=/, no Domain.
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 7*24*3600 {
		t.Fatalf("session cookie: %+v", c)
	}
	if lc := cookieNamed(resp, "__Secure-"+loginCookie); lc == nil || lc.MaxAge >= 0 {
		t.Errorf("login state cookie not cleared: %+v", lc)
	}

	code, m := e.me(t)
	if code != http.StatusOK || m.Email != "alice@example.test" || m.DisplayName != "Alice" || m.CSRFToken == "" || m.DevMode {
		t.Fatalf("me: %d %+v", code, m)
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE oidc_sub = 'alice-sub' AND id = $1 AND last_login_at IS NOT NULL`, m.ID); n != 1 {
		t.Fatalf("user row by sub: %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE id = $1`, hashToken(c.Value)); n != 1 {
		t.Fatal("session stored under the token hash")
	}

	// Second login: same user row refreshed, no second user.created.
	e.stub.email = "alice@new.test"
	e.fullLogin(t, "/")
	if n := e.count(t, `SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("want 1 user, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE email = 'alice@new.test'`); n != 1 {
		t.Fatal("email not refreshed on login")
	}
	// 001-AC21: user.created once, user.login per login, each by the user.
	for action, want := range map[string]int{"user.created": 1, "user.login": 2} {
		if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = $1 AND actor_user_id = $2 AND target_type = 'user' AND target_id = $3`, action, m.ID, m.ID); n != want {
			t.Errorf("%s: want %d got %d", action, want, n)
		}
	}
}

// 001-AC1: the callback rejects every forged or broken round trip and opens no session.
func TestCallbackRejects(t *testing.T) {
	tests := []struct {
		name string
		code int
		// build returns the callback URL to hit after a fresh login start.
		build func(t *testing.T, e *env) string
	}{
		{"bad state", http.StatusBadRequest, func(t *testing.T, e *env) string {
			cb := e.approve(t, e.startLogin(t, "/"))
			q := cb.Query()
			q.Set("state", "forged")
			cb.RawQuery = q.Encode()
			return cb.String()
		}},
		{"missing code", http.StatusBadRequest, func(t *testing.T, e *env) string {
			cb := e.approve(t, e.startLogin(t, "/"))
			q := cb.Query()
			q.Del("code")
			cb.RawQuery = q.Encode()
			return cb.String()
		}},
		{"bad nonce", http.StatusUnauthorized, func(t *testing.T, e *env) string {
			e.stub.nonceFn = func(string) string { return "replayed" }
			return e.approve(t, e.startLogin(t, "/")).String()
		}},
		{"provider error", http.StatusUnauthorized, func(t *testing.T, e *env) string {
			st := e.startLogin(t, "/").Query().Get("state")
			return "/auth/callback?error=access_denied&state=" + st
		}},
		{"no login cookie", http.StatusBadRequest, func(t *testing.T, e *env) string {
			cb := e.approve(t, e.startLogin(t, "/"))
			e.client.Jar, _ = cookiejar.New(nil)
			return cb.String()
		}},
		{"tampered login cookie", http.StatusBadRequest, func(t *testing.T, e *env) string {
			cb := e.approve(t, e.startLogin(t, "/"))
			u, _ := url.Parse(e.app.URL + "/auth/")
			e.client.Jar.SetCookies(u, []*http.Cookie{{Name: "__Secure-" + loginCookie, Value: "AAAA" + randomToken(), Path: "/auth/"}}) //nolint:gosec // client-side fixture cookie
			return cb.String()
		}},
		// PKCE: a code minted for another login (other challenge) fails with this login's verifier.
		{"code from another login", http.StatusUnauthorized, func(t *testing.T, e *env) string {
			first := e.approve(t, e.startLogin(t, "/"))
			second := e.approve(t, e.startLogin(t, "/"))
			q := second.Query()
			q.Set("code", first.Query().Get("code"))
			second.RawQuery = q.Encode()
			return second.String()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := setup(t, false)
			resp := e.do(t, http.MethodGet, tt.build(t, e), nil)
			if resp.StatusCode != tt.code {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("want %d got %d: %s", tt.code, resp.StatusCode, b)
			}
			if cookieNamed(resp, SecureSessionCookie) != nil || e.count(t, `SELECT count(*) FROM sessions`) != 0 {
				t.Fatal("a rejected callback opened a session")
			}
		})
	}
}

// 001-AC1: without a session every guarded /api route is 401 problem+json; the OpenAPI document stays public.
func TestUnauthenticated(t *testing.T) {
	e := setup(t, false)
	for _, path := range []string{"/api/v1/me", "/api/v1/nope", "/api/"} {
		resp := e.do(t, http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	// A cookie that matches no session is the same as none.
	u, _ := url.Parse(e.app.URL)
	e.client.Jar.SetCookies(u, []*http.Cookie{{Name: SecureSessionCookie, Value: randomToken(), Path: "/"}}) //nolint:gosec // client-side fixture cookie
	if code, _ := e.me(t); code != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", code)
	}
	if resp := e.do(t, http.MethodGet, "/api/openapi.json", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("openapi: %d", resp.StatusCode)
	}
	// Logout without a session is a harmless no-op that still lands on the signed-out page.
	if resp := e.do(t, http.MethodPost, "/auth/logout", nil); resp.StatusCode != http.StatusOK || logoutTarget(t, resp) != SignedOutPath {
		t.Errorf("anonymous logout: %d", resp.StatusCode)
	}
}

// 001-AC3: every mutation needs the session's CSRF token in X-CSRF-Token; reads do not.
func TestCSRF(t *testing.T) {
	e := setup(t, false)
	e.fullLogin(t, "/")
	_, m := e.me(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for name, tc := range map[string]struct {
			hdr  map[string]string
			want int
		}{
			"none":         {nil, http.StatusForbidden},
			"empty":        {map[string]string{CSRFHeader: ""}, http.StatusForbidden},
			"wrong":        {map[string]string{CSRFHeader: randomToken()}, http.StatusForbidden},
			"prefix":       {map[string]string{CSRFHeader: m.CSRFToken[:10]}, http.StatusForbidden},
			"right":        {map[string]string{CSRFHeader: m.CSRFToken}, http.StatusNoContent},
			"other header": {map[string]string{"X-XSRF-Token": m.CSRFToken}, http.StatusForbidden},
		} {
			resp := e.do(t, method, "/api/v1/echo", tc.hdr)
			if resp.StatusCode != tc.want {
				t.Errorf("%s %s: want %d got %d", method, name, tc.want, resp.StatusCode)
			}
			if tc.want == http.StatusForbidden {
				if title := problemTitle(t, resp); resp.Header.Get("Content-Type") != "application/problem+json" || title != "CSRF token invalid" {
					t.Errorf("%s %s: 403 is not the CSRF problem (%q)", method, name, title)
				}
			}
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if resp := e.do(t, method, "/api/v1/me", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s without token: %d", method, resp.StatusCode)
		}
	}
	if resp := e.do(t, http.MethodPost, "/auth/logout", nil); resp.StatusCode != http.StatusForbidden || problemTitle(t, resp) != "CSRF token invalid" {
		t.Errorf("logout without token: %d", resp.StatusCode)
	}
	if code, _ := e.me(t); code != http.StatusOK {
		t.Fatal("a refused logout must keep the session")
	}
}

func logoutTarget(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Redirect string `json:"redirect"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("logout body: %v", err)
	}
	return body.Redirect
}

// 001-AC21: logout deletes the session and audits it in the same transaction, then sends the browser
// to the provider's end-session endpoint (RP-initiated logout) with the ID token as hint.
func TestLogout(t *testing.T) {
	e := setup(t, false)
	e.fullLogin(t, "/")
	_, m := e.me(t)
	var idToken string
	if err := e.pool.QueryRow(context.Background(), `SELECT id_token FROM sessions`).Scan(&idToken); err != nil || strings.Count(idToken, ".") != 2 {
		t.Fatalf("raw ID token not stored: %q %v", idToken, err)
	}
	// The ID token never leaves through the API.
	if body, _ := io.ReadAll(e.do(t, http.MethodGet, "/api/v1/me", nil).Body); strings.Contains(string(body), idToken) {
		t.Fatal("/api/v1/me leaks the ID token")
	}

	resp := e.do(t, http.MethodPost, "/auth/logout", map[string]string{CSRFHeader: m.CSRFToken})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("logout: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	target, err := url.Parse(logoutTarget(t, resp))
	if err != nil {
		t.Fatal(err)
	}
	q := target.Query()
	if target.Scheme+"://"+target.Host+target.Path != e.stub.URL+"/logout" || q.Get("ui_locales") != "en" ||
		q.Get("id_token_hint") != idToken || q.Get("client_id") != "gedour" ||
		q.Get("post_logout_redirect_uri") != e.app.URL+SignedOutPath {
		t.Fatalf("end-session URL: %s", target)
	}
	if c := cookieNamed(resp, SecureSessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", c)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions left", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'session.logout' AND actor_user_id = $1 AND target_type = 'session' AND length(target_id) = 16`, m.ID); n != 1 {
		t.Fatalf("session.logout rows: %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE before::text LIKE '%' || $1 || '%' OR after::text LIKE '%' || $1 || '%'`, idToken); n != 0 {
		t.Fatal("the ID token reached the audit log")
	}
	if code, _ := e.me(t); code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", code)
	}
}

// Without end_session_endpoint (or in dev mode) sign-out lands on the signed-out page.
func TestLogoutWithoutEndSession(t *testing.T) {
	e := setup(t, false)
	e.stub.noEndSession = true
	e.fullLogin(t, "/")
	_, m := e.me(t)
	resp := e.do(t, http.MethodPost, "/auth/logout", map[string]string{CSRFHeader: m.CSRFToken})
	if got := logoutTarget(t, resp); resp.StatusCode != http.StatusOK || got != SignedOutPath {
		t.Fatalf("logout: %d -> %q", resp.StatusCode, got)
	}

	dev := setup(t, true)
	_, dm := dev.me(t)
	resp = dev.do(t, http.MethodPost, "/auth/logout", map[string]string{CSRFHeader: dm.CSRFToken})
	if got := logoutTarget(t, resp); resp.StatusCode != http.StatusOK || got != SignedOutPath {
		t.Fatalf("dev logout: %d -> %q", resp.StatusCode, got)
	}
	if n := dev.count(t, `SELECT count(*) FROM sessions WHERE id_token IS NOT NULL`); n != 0 {
		t.Fatal("dev sessions carry no ID token")
	}
}

// A replica that never served a login still discovers the provider before answering a logout.
func TestLogoutOnFreshReplica(t *testing.T) {
	e := setup(t, false)
	e.fullLogin(t, "/")
	_, m := e.me(t)
	fresh, err := New(config.Config{OIDCIssuer: e.stub.URL, OIDCClientID: "gedour", OIDCClientSecret: "s3cret", PublicURL: e.app.URL,
		SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}, e.pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	u, _ := url.Parse(e.app.URL)
	for _, c := range e.client.Jar.Cookies(u) {
		req.AddCookie(c)
	}
	req.Header.Set(CSRFHeader, m.CSRFToken)
	rec := httptest.NewRecorder()
	fresh.handleLogout(rec, req)
	if !strings.HasPrefix(logoutTarget(t, rec.Result()), e.stub.URL+"/logout") {
		t.Fatalf("fresh replica skipped RP-initiated logout: %s", rec.Body)
	}
}

// 001-AC3: sessions die after the idle timeout and at the absolute TTL; the idle clock moves at most once a minute.
func TestSessionExpiry(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	lastSeenAge := func() time.Duration {
		var secs float64
		if err := e.pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - last_seen_at) FROM sessions`).Scan(&secs); err != nil {
			t.Fatal(err)
		}
		return time.Duration(secs * float64(time.Second))
	}

	e.fullLogin(t, "/")
	exec(`UPDATE sessions SET last_seen_at = now() - interval '30 seconds'`)
	e.me(t)
	if age := lastSeenAge(); age < 25*time.Second {
		t.Errorf("touched within the minute: age %v", age)
	}
	exec(`UPDATE sessions SET last_seen_at = now() - interval '2 minutes'`)
	e.me(t)
	if age := lastSeenAge(); age > 10*time.Second {
		t.Errorf("not touched after a minute: age %v", age)
	}

	exec(`UPDATE sessions SET last_seen_at = now() - interval '25 hours'`)
	if code, _ := e.me(t); code != http.StatusUnauthorized {
		t.Errorf("idle session still valid: %d", code)
	}

	e.fullLogin(t, "/")
	exec(`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE last_seen_at > now() - interval '1 hour'`)
	if code, _ := e.me(t); code != http.StatusUnauthorized {
		t.Errorf("expired session still valid: %d", code)
	}

	// The next login purges both dead sessions.
	e.fullLogin(t, "/")
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("want only the live session left, got %d", n)
	}
}

// 001-AC2: dev mode authenticates every request as the dev user, without Secure, CSRF still enforced.
func TestDevMode(t *testing.T) {
	e := setup(t, true)
	resp := e.do(t, http.MethodGet, "/api/v1/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me: %d", resp.StatusCode)
	}
	c := cookieNamed(resp, SessionCookie)
	if c == nil || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("dev cookie: %+v", c)
	}
	code, m := e.me(t)
	if code != http.StatusOK || !m.DevMode || m.Email != "dev@local" || m.DisplayName != "dev@local" {
		t.Fatalf("me: %d %+v", code, m)
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE oidc_sub = 'dev:dev@local'`); n != 1 {
		t.Fatal("dev user not upserted")
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("the cookie must be reused, got %d sessions", n)
	}
	if resp := e.do(t, http.MethodPost, "/api/v1/echo", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("dev mutation without CSRF: %d", resp.StatusCode)
	}
	if resp := e.do(t, http.MethodPost, "/api/v1/echo", map[string]string{CSRFHeader: m.CSRFToken}); resp.StatusCode != http.StatusNoContent {
		t.Errorf("dev mutation with CSRF: %d", resp.StatusCode)
	}
	resp = e.do(t, http.MethodGet, "/auth/login?return_to=/audit", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/audit" {
		t.Errorf("dev login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := e.do(t, http.MethodGet, "/auth/callback?code=x&state=y", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("dev callback: %d", resp.StatusCode)
	}
}

// Open-redirect guard: only same-origin absolute paths survive.
func TestSafeReturnTo(t *testing.T) {
	tests := map[string]string{
		"/":                     "/",
		"/watches":              "/watches",
		"/audit?from=x#top":     "/audit?from=x#top",
		"":                      "/",
		"//evil.example":        "/",
		"/\\evil.example":       "/",
		"/x\\..\\/evil.example": "/",
		"\\\\evil.example":      "/",
		"https://evil.example":  "/",
		"javascript:alert(1)":   "/",
		"evil.example/x":        "/",
		" /watches":             "/", //nolint:gocritic // leading space is the case under test
		"/\t/evil.example":      "/",
		"/\n/evil.example":      "/",
		"///evil.example":       "/",
		"/%zz":                  "/",
	}
	for in, want := range tests {
		if got := safeReturnTo(in); got != want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

// return_to goes through the same guard end to end.
func TestLoginReturnToRejected(t *testing.T) {
	e := setup(t, false)
	resp := e.do(t, http.MethodGet, e.approve(t, e.startLogin(t, "//evil.example/steal")).String(), nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLoginState(t *testing.T) {
	s := &Service{}
	var err error
	if s.stateKey, err = deriveStateKey("k"); err != nil {
		t.Fatal(err)
	}
	ok := s.seal(loginState{State: "a", Expires: time.Now().Add(time.Minute).Unix()})
	if st, err := s.open(ok); err != nil || st.State != "a" {
		t.Fatalf("round trip: %+v %v", st, err)
	}
	expired := s.seal(loginState{State: "a", Expires: time.Now().Add(-time.Second).Unix()})
	other := &Service{}
	other.stateKey, _ = deriveStateKey("other")
	for name, v := range map[string]string{"expired": expired, "short": "AA", "not base64": "!!", "other key": other.seal(loginState{Expires: time.Now().Add(time.Minute).Unix()})} {
		if _, err := s.open(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProviderUnavailable(t *testing.T) {
	cfg := config.Config{OIDCIssuer: "http://127.0.0.1:1", OIDCClientID: "c", OIDCClientSecret: "s", PublicURL: "https://app.test", SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}
	svc, err := New(cfg, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	svc.handleLogin(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

// A database outage fails closed: 500, never an anonymous pass or a half-done login.
func TestDatabaseDown(t *testing.T) {
	e := setup(t, false)
	e.fullLogin(t, "/")
	_, m := e.me(t)
	cb := e.approve(t, e.startLogin(t, "/"))
	e.pool.Close()
	for name, resp := range map[string]*http.Response{
		"guard":    e.do(t, http.MethodGet, "/api/v1/me", nil),
		"logout":   e.do(t, http.MethodPost, "/auth/logout", map[string]string{CSRFHeader: m.CSRFToken}),
		"callback": e.do(t, http.MethodGet, cb.String(), nil),
	} {
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: want 500, got %d", name, resp.StatusCode)
		}
	}

	dev := setup(t, true)
	dev.pool.Close()
	for _, path := range []string{"/auth/login", "/api/v1/me"} {
		if resp := dev.do(t, http.MethodGet, path, nil); resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("dev %s: want 500, got %d", path, resp.StatusCode)
		}
	}
}

func stubService(t *testing.T, issuer string) *Service {
	t.Helper()
	svc, err := New(config.Config{OIDCIssuer: issuer, OIDCClientID: "gedour", OIDCClientSecret: "s", PublicURL: "https://app.test",
		SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// A slow IdP costs every concurrent caller one discovery, not one each in turn.
func TestDiscoveryConcurrent(t *testing.T) {
	const callers, delay = 5, 400 * time.Millisecond
	stub := newStub(t)
	stub.discoveryDelay = delay
	svc := stubService(t, stub.URL)
	start := time.Now()
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := svc.endpoints()
			errs <- err
		}()
	}
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > delay*2 {
		t.Fatalf("%d callers took %v for a %v discovery: serialized", callers, elapsed, delay)
	}
	if n := stub.discoveries.Load(); n != 1 {
		t.Fatalf("want 1 discovery, got %d", n)
	}
	// Cached afterwards: no further hit.
	if _, err := svc.endpoints(); err != nil || stub.discoveries.Load() != 1 {
		t.Fatalf("not cached: %v, %d hits", err, stub.discoveries.Load())
	}
}

// A failed discovery is answered from cache for a few seconds instead of hammering a down IdP.
func TestDiscoveryFailureCached(t *testing.T) {
	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer down.Close()
	svc := stubService(t, down.URL)
	for range 3 {
		if _, err := svc.endpoints(); err == nil {
			t.Fatal("want discovery error")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("want 1 hit within the retry window, got %d", n)
	}
	svc.mu.Lock()
	svc.discFailedAt = time.Now().Add(-discoveryRetry)
	svc.mu.Unlock()
	_, _ = svc.endpoints()
	if n := hits.Load(); n != 2 {
		t.Fatalf("want a retry after the window, got %d hits", n)
	}
}

// 001-AC2: cookieless dev-mode reads run as the dev user without writing a session or audit row each;
// they carry no CSRF token, so mutations still fail. Only GET /api/v1/me opens a session.
func TestDevModeCookieless(t *testing.T) {
	e := setup(t, true)
	e.client.Jar = nil
	for range 40 {
		if resp := e.do(t, http.MethodGet, "/api/v1/echo", nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("cookieless GET: %d", resp.StatusCode)
		}
	}
	if resp := e.do(t, http.MethodPost, "/api/v1/echo", map[string]string{CSRFHeader: "x"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookieless POST: %d", resp.StatusCode)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions after cookieless reads, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login'`); n != 0 {
		t.Fatalf("%d user.login rows after cookieless reads, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.created'`); n != 1 {
		t.Fatalf("dev user created %d times", n)
	}
	resp := e.do(t, http.MethodGet, "/api/v1/me", nil)
	if resp.StatusCode != http.StatusOK || cookieNamed(resp, SessionCookie) == nil {
		t.Fatalf("me must open a session: %d", resp.StatusCode)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("want 1 session after /me, got %d", n)
	}
}

// Signing in over a live session replaces it in the same transaction.
func TestReloginReplacesSession(t *testing.T) {
	e := setup(t, false)
	e.fullLogin(t, "/")
	u, _ := url.Parse(e.app.URL)
	var first string
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == SecureSessionCookie {
			first = c.Value
		}
	}
	e.fullLogin(t, "/")
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("want the old session replaced, got %d sessions", n)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE id = $1`, hashToken(first)); n != 0 {
		t.Fatal("the previous session is still valid")
	}
}

// The purge is best effort: its failure never fails a login.
func TestPurgeFailureKeepsLogin(t *testing.T) {
	saved := purgeQuery
	purgeQuery = "SELECT broken("
	t.Cleanup(func() { purgeQuery = saved })
	e := setup(t, false)
	if resp := e.fullLogin(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if code, _ := e.me(t); code != http.StatusOK {
		t.Fatalf("me: %d", code)
	}
}

// DNS rebinding: in dev mode a request whose Host is not loopback is refused on /api and /auth,
// before any session is minted; loopback names (any port) still work.
func TestDevModeRejectsForeignHost(t *testing.T) {
	e := setup(t, true)
	withHost := func(method, path, host string, hdr map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, e.app.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	for _, host := range []string{"attacker.example", "attacker.example:8080", "localhost.attacker.example", "127.0.0.2", "[::2]:80"} {
		for _, tc := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/echo"},
			{http.MethodGet, "/api/v1/me"},
			{http.MethodPost, "/api/v1/echo"},
			{http.MethodGet, "/auth/login"},
			{http.MethodPost, "/auth/logout"},
		} {
			resp := withHost(tc.method, tc.path, host, map[string]string{CSRFHeader: "x"})
			// A plain Forbidden, not the CSRF problem: the client must not retry it.
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/problem+json" || problemTitle(t, resp) != "Forbidden" {
				t.Errorf("Host %q %s %s: %d", host, tc.method, tc.path, resp.StatusCode)
			}
		}
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("a foreign Host minted %d sessions", n)
	}
	for _, host := range []string{"localhost", "LOCALHOST:8080", "127.0.0.1:18080", "[::1]:8080", "::1"} {
		resp := withHost(http.MethodGet, "/api/v1/me", host, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Host %q: me %d", host, resp.StatusCode)
		}
	}
	// OIDC mode does not restrict the Host: it runs behind real domains.
	o := setup(t, false)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, o.app.URL+"/api/v1/me", nil)
	req.Host = "gedour.example"
	resp, err := o.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OIDC mode with a real Host: %d", resp.StatusCode)
	}
}

func problemTitle(t *testing.T, resp *http.Response) string {
	t.Helper()
	var p struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("problem body: %v", err)
	}
	return p.Title
}
