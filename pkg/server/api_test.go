package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/route"
	"github.com/gedour/gedour/pkg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Terminate()
	os.Exit(code)
}

type devApp struct {
	srv    *httptest.Server
	pool   *pgxpool.Pool
	sender *notify.Sender
	routes *route.Service
	client *http.Client
	userID string
	csrf   string
}

// newDevApp serves the full handler in dev mode against a fresh database, signed in as the dev user.
func newDevApp(t *testing.T) *devApp {
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
	authn, err := auth.New(config.Config{DevUser: "dev@local", SessionTTL: time.Hour, SessionIdleTimeout: time.Hour}, pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	a := &devApp{pool: pool, sender: testSender()}
	a.routes = testRoutes(t, pool, a.sender)
	a.srv = httptest.NewServer(New(zap.NewNop(), build, pingOK, fstest.MapFS{"index.html": {Data: []byte("SPA")}}, authn, pool, testWatches(t, pool), a.routes))
	t.Cleanup(a.srv.Close)
	jar, _ := cookiejar.New(nil)
	a.client = &http.Client{Jar: jar}
	var m struct {
		ID        string `json:"id"`
		CSRFToken string `json:"csrf_token"`
	}
	a.getJSON(t, "/api/v1/me", http.StatusOK, &m)
	a.userID, a.csrf = m.ID, m.CSRFToken
	return a
}

func (a *devApp) req(t *testing.T, method, path string, hdr http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, a.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (a *devApp) getJSON(t *testing.T, path string, code int, v any) {
	t.Helper()
	resp := a.req(t, http.MethodGet, path, nil)
	if resp.StatusCode != code {
		t.Fatalf("GET %s: want %d got %d", path, code, resp.StatusCode)
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
}

// Signed in, the /api fallback still answers 404/405 problem+json, never the SPA.
func TestAuthenticatedRouting(t *testing.T) {
	a := newDevApp(t)
	csrf := http.Header{auth.CSRFHeader: {a.csrf}}
	for _, tt := range []struct {
		method, path string
		code         int
		allow        string
	}{
		{http.MethodGet, "/api/v1/nope", http.StatusNotFound, ""},
		{http.MethodPost, "/api/v1/nope", http.StatusNotFound, ""},
		{http.MethodPost, "/api/v1/me", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodDelete, "/api/v1/audit", http.StatusMethodNotAllowed, "GET, HEAD"},
	} {
		resp := a.req(t, tt.method, tt.path, csrf)
		if resp.StatusCode != tt.code || resp.Header.Get("Allow") != tt.allow || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s %s: %d allow=%q %s", tt.method, tt.path, resp.StatusCode, resp.Header.Get("Allow"), resp.Header.Get("Content-Type"))
		}
	}
	// 001-AC3: the same mutation without the token is refused before routing.
	if resp := a.req(t, http.MethodPost, "/api/v1/me", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF: %d", resp.StatusCode)
	}
}

type auditPage struct {
	Items []struct {
		ID         int64     `json:"id"`
		At         time.Time `json:"at"`
		Action     string    `json:"action"`
		TargetType string    `json:"target_type"`
		ActorName  string    `json:"actor_name"`
		RequestID  string    `json:"request_id"`
		After      any       `json:"after"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// 001-AC22: the audit API lists the caller's entries newest first, filtered, paginated, and validates input.
func TestAuditAPI(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()

	var page auditPage
	a.getJSON(t, "/api/v1/audit", http.StatusOK, &page)
	if len(page.Items) != 2 || page.Items[0].Action != "user.login" || page.Items[1].Action != "user.created" || page.NextCursor != "" {
		t.Fatalf("first page: %+v", page)
	}
	if page.Items[0].ActorName != "dev@local" || len(page.Items[0].RequestID) != 24 {
		t.Errorf("actor or request id missing: %+v", page.Items[0])
	}

	// Another user's activity stays invisible, even when asked for by actor.
	var other string
	if err := a.pool.QueryRow(ctx, `INSERT INTO users (oidc_sub) VALUES ('other') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `INSERT INTO audit_log (actor_user_id, action, target_type) SELECT $1, 'user.login', 'user' FROM generate_series(1, 5)`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `INSERT INTO audit_log (actor_user_id, action, target_type, at) SELECT $1, 'watch.created', 'watch', now() - interval '2 days' FROM generate_series(1, 3)`, a.userID); err != nil {
		t.Fatal(err)
	}
	a.getJSON(t, "/api/v1/audit?actor="+other, http.StatusOK, &page)
	if len(page.Items) != 0 {
		t.Fatalf("leaked %d entries of another user", len(page.Items))
	}

	page = auditPage{}
	a.getJSON(t, "/api/v1/audit?target_type=watch&limit=2", http.StatusOK, &page)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page 1: %+v", page)
	}
	next := auditPage{}
	a.getJSON(t, "/api/v1/audit?target_type=watch&limit=2&cursor="+page.NextCursor, http.StatusOK, &next)
	if len(next.Items) != 1 || next.NextCursor != "" || next.Items[0].ID >= page.Items[1].ID {
		t.Fatalf("page 2: %+v", next)
	}

	from := url.QueryEscape(time.Now().Add(-24 * time.Hour).Format(time.RFC3339))
	page = auditPage{}
	a.getJSON(t, "/api/v1/audit?from="+from, http.StatusOK, &page)
	if len(page.Items) != 2 {
		t.Fatalf("from filter: %d", len(page.Items))
	}
	page = auditPage{}
	a.getJSON(t, "/api/v1/audit?to="+from, http.StatusOK, &page)
	if len(page.Items) != 3 {
		t.Fatalf("to filter: %d", len(page.Items))
	}

	for _, q := range []string{"limit=0", "limit=201", "limit=x", "from=yesterday", "to=2026-13-01T00:00:00Z", "cursor=!!", "actor=not-a-uuid"} {
		resp := a.req(t, http.MethodGet, "/api/v1/audit?"+q, nil)
		if resp.StatusCode != http.StatusBadRequest || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: want 400 problem+json, got %d %s", q, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	page = auditPage{}
	a.getJSON(t, "/api/v1/audit?limit=200", http.StatusOK, &page)
	if len(page.Items) != 5 {
		t.Fatalf("limit=200: %d", len(page.Items))
	}
}

// The OpenAPI document lists the auth and audit operations.
func TestOpenAPIListsAuthAndAudit(t *testing.T) {
	a := newDevApp(t)
	var doc struct{ Paths map[string]any }
	a.getJSON(t, "/api/openapi.json", http.StatusOK, &doc)
	for _, p := range []string{"/api/v1/me", "/api/v1/audit", "/api/v1/version"} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("openapi lacks %s", p)
		}
	}
}
