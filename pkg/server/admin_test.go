package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/testpg"
	"github.com/gedour/gedour/pkg/worker"
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
	if n < 7 {
		t.Fatalf("found %d admin operations, want at least 7", n)
	}
}

// secretSeen is a collector recording the secret each run was handed, as a forge would receive it.
type secretSeen struct {
	mu   sync.Mutex
	seen []string
}

func (c *secretSeen) Collect(_ context.Context, _ *collect.Fetcher, _ json.RawMessage, cred *collect.Credential) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cred == nil {
		c.seen = append(c.seen, "")
	} else {
		c.seen = append(c.seen, cred.Secret)
	}
	return json.RawMessage(`{}`), nil
}

func (c *secretSeen) Diff(json.RawMessage, json.RawMessage) ([]collect.Event, error) { return nil, nil }

func (c *secretSeen) last() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		return "", 0
	}
	return c.seen[len(c.seen)-1], len(c.seen)
}

// 006-AC10: after a transfer through the API, the new owner owns the watch and its credential, the
// worker still unseals the secret and collects, and the old owner gets 404 on the watch and its credential.
func TestTransferKeepsCollecting(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	admin := a.signIn(t, "admin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	const plain = "ghp_TRANSFERRED0123456789"
	var c credJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": plain}, http.StatusCreated, &c)
	var w watchJSON
	body := newWatch("Private", "github_release", map[string]any{"repo": "alice/private"})
	body["credential_id"] = c.ID
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", body, http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "ro"}, http.StatusCreated, nil)

	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w.ID+"/transfer", map[string]any{"to_user_id": bob.id}, http.StatusOK, nil)
	var got watchJSON
	a.mustCall(t, bob, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusOK, &got)
	if got.Permission != "owner" || got.CredentialID == nil || *got.CredentialID != c.ID {
		t.Fatalf("bob's view: %+v", got)
	}
	a.mustCall(t, bob, http.MethodGet, "/api/v1/credentials/"+c.ID, nil, http.StatusOK, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusNotFound, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/credentials/"+c.ID, nil, http.StatusNotFound, nil)

	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	col := &secretSeen{}
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	wk, err := worker.New(a.pool, zap.NewNop(), box, g.Client(netguard.Options{Timeout: 5 * time.Second, HTTPHosts: []string{"127.0.0.1"}}),
		map[string]collect.Collector{"github_release": col}, worker.NewMetrics(prometheus.NewRegistry()),
		worker.Options{Poll: 10 * time.Millisecond, Concurrency: 1, DrainTimeout: time.Second, UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	a.exec(t, `UPDATE watches SET next_run_at = now() - interval '1 second' WHERE id = $1`, w.ID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { wk.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for a.countRows(t, `SELECT count(*) FROM watches WHERE id = $1 AND last_status = 'ok'`, w.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the transferred watch never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s, n := col.last(); n == 0 || s != plain {
		t.Fatalf("collector got %d runs, last secret %q", n, s)
	}
}

// A share added while a purge of the target holds its row waits for the purge, then finds no account:
// 404 and no share row on the purged account.
func TestShareWaitsForPurge(t *testing.T) {
	a := newDevApp(t)
	alice, dave := a.signIn(t, "alice"), a.signIn(t, "dave")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	ctx := context.Background()
	purge, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = purge.Rollback(ctx) }()
	if _, err := purge.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, dave.id); err != nil {
		t.Fatal(err)
	}
	if _, err := purge.Exec(ctx, `UPDATE users SET email = '', display_name = '', disabled_at = now(), purged_at = now() WHERE id = $1`, dave.id); err != nil {
		t.Fatal(err)
	}
	shared := a.callAsync(alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": dave.email, "permission": "ro"})
	a.waitLockWaiters(t, 1)
	if err := purge.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-shared; code != http.StatusNotFound {
		t.Fatalf("share: %d, want 404", code)
	}
	if n := a.countRows(t, `SELECT count(*) FROM watch_shares WHERE user_id = $1`, dave.id); n != 0 {
		t.Fatal("a share landed on the purged account")
	}
}

// callAsync is call in a goroutine, without t; a transport error answers 0.
func (a *devApp) callAsync(u apiUser, method, path string, body any) <-chan int {
	out := make(chan int, 1)
	go func() {
		var rd bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&rd).Encode(body)
		}
		req, err := http.NewRequestWithContext(context.Background(), method, a.srv.URL+path, &rd)
		if err != nil {
			out <- 0
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.CSRFHeader, u.csrf)
		req.Header.Set("Cookie", auth.SessionCookie+"="+u.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- 0
			return
		}
		_ = resp.Body.Close()
		out <- resp.StatusCode
	}()
	return out
}

// waitLockWaiters blocks until n backends of this database wait on a lock.
func (a *devApp) waitLockWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for a.countRows(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) < n {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d lock waiters", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A transfer and the owner's delete of the same watch both lock its rules before the watch, so whichever
// queues first on the rule wins and the other answers 404 (or 409); never a deadlock 500. A held rule
// lock queues them in each order.
func TestTransferRacingWatchDelete(t *testing.T) {
	for _, transferFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "transfer first", false: "delete first"}[transferFirst], func(t *testing.T) {
			a := newDevApp(t)
			alice, bob, admin := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "admin")
			a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
			var w watchJSON
			a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
			var rule struct {
				ID string `json:"id"`
			}
			a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "on w", "watch_id": w.ID}, http.StatusCreated, &rule)
			ctx := context.Background()
			hold, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(ctx) }()
			if _, err := hold.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, rule.ID); err != nil {
				t.Fatal(err)
			}
			start := []func() <-chan int{
				func() <-chan int {
					return a.callAsync(admin, http.MethodPost, "/api/v1/admin/watches/"+w.ID+"/transfer", map[string]any{"to_user_id": bob.id})
				},
				func() <-chan int { return a.callAsync(alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil) },
			}
			if !transferFirst {
				start[0], start[1] = start[1], start[0]
			}
			first := start[0]()
			a.waitLockWaiters(t, 1)
			second := start[1]()
			a.waitLockWaiters(t, 2)
			if err := hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := []int{<-first, <-second}
			transfer, del := got[0], got[1]
			if !transferFirst {
				transfer, del = got[1], got[0]
			}
			if transferFirst && (transfer != http.StatusOK || del != http.StatusNotFound) {
				t.Fatalf("transfer %d, delete %d: want 200 then 404", transfer, del)
			}
			if !transferFirst && (del != http.StatusNoContent || transfer != http.StatusNotFound) {
				t.Fatalf("delete %d, transfer %d: want 204 then 404", del, transfer)
			}
		})
	}
}

// The router holds an event of the watch FOR UPDATE and then takes KEY SHARE on a matching rule (the
// deliveries FK). A delete of the watch holding that rule FOR UPDATE would wait on the event while the
// router waits on the rule: the delete's NO KEY UPDATE lets the router through.
func TestWatchDeleteRacingRouter(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "on w", "watch_id": w.ID}, http.StatusCreated, &rule)
	var ev int64
	if err := a.pool.QueryRow(context.Background(), `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id`, w.ID).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	router, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = router.Rollback(ctx) }()
	if _, err := router.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, ev); err != nil {
		t.Fatal(err)
	}
	deleted := a.callAsync(alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil)
	a.waitLockWaiters(t, 1)
	if _, err := router.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR KEY SHARE`, rule.ID); err != nil {
		t.Fatalf("router's rule KEY SHARE: %v", err)
	}
	if err := router.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-deleted; code != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", code)
	}
}

// A user who may not delete a watch is refused before any lock: a held rule on it does not make them wait.
func TestWatchDeleteRefusedWithoutLocking(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "on w", "watch_id": w.ID}, http.StatusCreated, &rule)
	ctx := context.Background()
	hold, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, rule.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-a.callAsync(bob, http.MethodDelete, "/api/v1/watches/"+w.ID, nil):
		if code != http.StatusNotFound {
			t.Fatalf("stranger's delete: %d, want 404", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stranger's delete waited on the watch's rule lock")
	}
}
