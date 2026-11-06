package server

import (
	"context"
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

// 007-AC2: through the real server, whose huma answers 400, a bad duration or scope is refused and stores nothing.
func TestAPIKeyInvalidInputs(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Connect(ctx, testpg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	authn, err := auth.New(config.Config{OIDCIssuer: "http://127.0.0.1:1", OIDCClientID: "c", OIDCClientSecret: "s", PublicURL: "https://app.test",
		SessionTTL: time.Hour, SessionIdleTimeout: time.Hour, APIKeySigninMaxAge: time.Hour}, pool, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(zap.NewNop(), build, pingOK, fstest.MapFS{"index.html": {Data: []byte("SPA")}}, authn, pool,
		testWatches(t, pool), testRoutes(t, pool, testSender())))
	t.Cleanup(srv.Close)
	const token = "session-token"
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, last_login_at) VALUES ('alice', now()) RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, user_id, csrf_token, expires_at) VALUES (sha256($1), $2, 'csrf', now() + interval '1 hour')`, []byte(token), userID); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, body string
		want       int
	}{
		{"duration 8", `{"name":"k","duration":"8"}`, 400},
		{"duration 0", `{"name":"k","duration":"0"}`, 400},
		{"duration blank", `{"name":"k","duration":""}`, 400},
		{"duration 366", `{"name":"k","duration":"366"}`, 400},
		{"duration forever", `{"name":"k","duration":"forever"}`, 400},
		{"duration as a number", `{"name":"k","duration":30}`, 400},
		{"scope admin", `{"name":"k","scope":"admin"}`, 400},
		{"scope blank", `{"name":"k","scope":""}`, 400},
		{"scope upper case", `{"name":"k","scope":"READ"}`, 400},
		{"name missing", `{}`, 400},
		{"name blank", `{"name":"  "}`, 400},
		{"name too long", `{"name":"` + strings.Repeat("x", 65) + `"}`, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/api-keys", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", auth.SecureSessionCookie+"="+token)
			req.Header.Set(auth.CSRFHeader, "csrf")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var problem map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&problem)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d: %v", resp.StatusCode, problem)
			}
		})
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d keys stored (%v)", n, err)
	}
}
