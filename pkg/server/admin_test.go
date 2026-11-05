package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/testpg"
)

// 006-AC5: every operation under /api/v1/admin, read from the served OpenAPI document so a new one is
// covered on its own, answers a non-admin session 403 problem+json, and an admin session something else.
func TestAdminOperationsNeedAdmin(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	authn, err := auth.New(config.Config{OIDCIssuer: "http://127.0.0.1:1", OIDCClientID: "c", OIDCClientSecret: "s",
		PublicURL: "https://app.test", SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}, pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(zap.NewNop(), build, pingOK, fstest.MapFS{"index.html": {Data: []byte("SPA")}}, authn, pool,
		testWatches(t, pool), testRoutes(t, pool, testSender())))
	t.Cleanup(srv.Close)
	session := func(sub string, admin bool) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users (oidc_sub) VALUES ($1) RETURNING id::text`, sub).Scan(&id); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(sub))
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, user_id, csrf_token, expires_at, is_admin) VALUES ($1, $2, 'csrf', now() + interval '1 hour', $3)`,
			sum[:], id, admin); err != nil {
			t.Fatal(err)
		}
		return sub
	}
	call := func(method, path, token string) *http.Response {
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", auth.SecureSessionCookie+"="+token)
		req.Header.Set(auth.CSRFHeader, "csrf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	user, admin := session("user-token", false), session("admin-token", true)

	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	resp := call(http.MethodGet, "/api/openapi.json", "")
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for path, ops := range doc.Paths {
		if !strings.HasPrefix(path, "/api/v1/admin") {
			continue
		}
		concrete := strings.NewReplacer("{id}", "00000000-0000-0000-0000-000000000000").Replace(path)
		if strings.Contains(concrete, "{") {
			t.Fatalf("%s: unknown path parameter, extend the replacer", path)
		}
		for method := range ops {
			m := strings.ToUpper(method)
			n++
			if resp := call(m, concrete, user); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/problem+json" {
				t.Errorf("%s %s as a user: %d %s", m, path, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			if resp := call(m, concrete, admin); resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
				t.Errorf("%s %s as an admin: %d", m, path, resp.StatusCode)
			}
		}
	}
	if n < 3 {
		t.Fatalf("found %d admin operations, want at least 3", n)
	}
}
