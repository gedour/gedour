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
	"github.com/gedour/gedour/pkg/route"
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
	user, admin, acting := session("user-token", false), session("admin-token", true), session("acting-token", true)
	actAs := func() {
		if _, err := pool.Exec(ctx, `UPDATE sessions SET acting_as = (SELECT id FROM users WHERE oidc_sub = 'user-token') WHERE user_id = (SELECT id FROM users WHERE oidc_sub = $1)`, acting); err != nil {
			t.Fatal(err)
		}
	}

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
			// 006-AC15: acting as a user, every admin operation is 409 but the way out.
			actAs()
			stop := m == http.MethodDelete && path == "/api/v1/admin/act-as"
			if resp := call(m, concrete, acting); (resp.StatusCode == http.StatusConflict) == stop || resp.StatusCode == http.StatusForbidden {
				t.Errorf("%s %s while acting: %d", m, path, resp.StatusCode)
			}
		}
	}
	if n < 9 {
		t.Fatalf("found %d admin operations, want at least 9", n)
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
	if _, err := purge.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, dave.id); err != nil {
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
	testpg.WaitLockWaiters(t, a.pool, n)
}

// A rule edit holds its rule FOR UPDATE, then takes the watch's KEY SHARE (the UI always sends watch_id).
// A delete of that watch locking the watch first would wait on the rule in its cascade: the delete locks
// the rules first, so the edit goes through and the delete then completes.
func TestWatchDeleteRacingRuleEdit(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "on w", "watch_id": w.ID}, http.StatusCreated, &rule)
	ctx := context.Background()
	edit, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edit.Rollback(ctx) }()
	if _, err := edit.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, rule.ID); err != nil {
		t.Fatal(err)
	}
	deleted := a.callAsync(alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil)
	a.waitLockWaiters(t, 1)
	if _, err := edit.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w.ID); err != nil {
		t.Fatalf("rule edit's watch check: %v", err)
	}
	if err := edit.Commit(ctx); err != nil {
		t.Fatalf("rule edit commit: %v", err)
	}
	if code := <-deleted; code != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", code)
	}
}

// The router holds an event of the watch FOR UPDATE and then takes KEY SHARE on a matching rule (the
// deliveries FK). A delete of the watch holding that rule FOR UPDATE would wait on the event while the
// router waits on the rule: the delete's NO KEY UPDATE and its events deleted before the watch let the
// router through, whatever order the watch's cascades fire in.
func TestWatchDeleteRacingRouter(t *testing.T) {
	for _, flipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "events cascade first", true: "rules cascade first"}[flipped], func(t *testing.T) {
			a := newDevApp(t)
			if flipped {
				testpg.FlipEventsCascade(t, a.pool)
			}
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
		})
	}
}

// The purged account's rules on a watch it was shared, the higher id inserted first: a purge deleting them
// in scan order would hold the higher and wait on the lower while the watch's delete, locking them in id
// order, holds the lower and waits on the higher. A KEY SHARE (the router's) on the lower parks the purge.
func TestPurgeRacingWatchDelete(t *testing.T) {
	a := newDevApp(t)
	alice, dave, admin := a.signIn(t, "alice"), a.signIn(t, "dave"), a.signIn(t, "admin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	const low, high = "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	for _, id := range []string{high, low} {
		a.exec(t, `INSERT INTO rules (id, owner_id, name, watch_id) VALUES ($1, $2, $3, $4)`, id, dave.id, id, w.ID)
	}
	a.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave.id)
	ctx := context.Background()
	hold, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR KEY SHARE`, low); err != nil {
		t.Fatal(err)
	}
	purge := a.callAsync(admin, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/purge", nil)
	a.waitLockWaiters(t, 1)
	del := a.callAsync(alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil)
	a.waitLockWaiters(t, 2)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if p, d := <-purge, <-del; p != http.StatusNoContent || d != http.StatusNoContent {
		t.Fatalf("purge %d, delete %d: want 204 both", p, d)
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

// 006-AC10: the old owner's rule on a transferred watch stays but routes nothing and reads with a null
// watch_name; once the new owner shares the watch back, it routes again.
func TestTransferKeepsRulesOfOldOwner(t *testing.T) {
	a := newDevApp(t)
	alice, bob, admin := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "admin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "on w", "watch_id": w.ID}, http.StatusCreated, &rule)
	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w.ID+"/transfer", map[string]any{"to_user_id": bob.id}, http.StatusOK, nil)

	var rules struct {
		Items []struct {
			ID        string  `json:"id"`
			WatchID   *string `json:"watch_id"`
			WatchName *string `json:"watch_name"`
		} `json:"items"`
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules", nil, http.StatusOK, &rules)
	if len(rules.Items) != 1 || rules.Items[0].ID != rule.ID || rules.Items[0].WatchID == nil || *rules.Items[0].WatchID != w.ID || rules.Items[0].WatchName != nil {
		t.Fatalf("alice's rules after the transfer: %+v", rules.Items)
	}
	router := route.NewRouter(a.pool, zap.NewNop(), route.NewMetrics(prometheus.NewRegistry()), time.Second)
	routed := func(key string) int {
		t.Helper()
		var ev int64
		if err := a.pool.QueryRow(context.Background(), `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', $2) RETURNING id`, w.ID, key).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		if n, err := router.RouteOnce(context.Background()); err != nil || n != 1 {
			t.Fatalf("route: %d %v", n, err)
		}
		return a.countRows(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, alice.id, ev)
	}
	if n := routed("k1"); n != 0 {
		t.Fatal("the old owner's rule routed an event of a watch it can no longer read")
	}
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": alice.email, "permission": "ro"}, http.StatusCreated, nil)
	if n := routed("k2"); n != 1 {
		t.Fatal("the old owner's rule did not route once the watch was shared back")
	}
}

// A transfer and the owner's delete of the same watch queue on its row, in either order: the first wins,
// the second answers 404, never a deadlock 500.
func TestTransferRacingWatchDelete(t *testing.T) {
	for _, transferFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "transfer first", false: "delete first"}[transferFirst], func(t *testing.T) {
			a := newDevApp(t)
			alice, bob, admin := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "admin")
			a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
			var w watchJSON
			a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
			a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "on w", "watch_id": w.ID}, http.StatusCreated, nil)
			ctx := context.Background()
			hold, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(ctx) }()
			if _, err := hold.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
				t.Fatal(err)
			}
			transfer := func() <-chan int {
				return a.callAsync(admin, http.MethodPost, "/api/v1/admin/watches/"+w.ID+"/transfer", map[string]any{"to_user_id": bob.id})
			}
			del := func() <-chan int { return a.callAsync(alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil) }
			var tr, dl <-chan int
			if transferFirst {
				tr = transfer()
				a.waitLockWaiters(t, 1)
				dl = del()
			} else {
				dl = del()
				a.waitLockWaiters(t, 1)
				tr = transfer()
			}
			a.waitLockWaiters(t, 2)
			if err := hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := []int{<-tr, <-dl}
			if transferFirst && (got[0] != http.StatusOK || got[1] != http.StatusNotFound) {
				t.Fatalf("transfer %d, delete %d: want 200 then 404", got[0], got[1])
			}
			if !transferFirst && (got[1] != http.StatusNoContent || got[0] != http.StatusNotFound) {
				t.Fatalf("delete %d, transfer %d: want 204 then 404", got[1], got[0])
			}
		})
	}
}

// Run-all locks a rw sharee's watches in id order, as a purge locks the owner's: a purge holding the lower
// watch and then taking the higher one does not deadlock with it. The higher id is inserted first, so an
// unordered scan would lock it first.
func TestRunAllRacingPurgeLocks(t *testing.T) {
	a := newDevApp(t)
	dave, erin := a.signIn(t, "dave"), a.signIn(t, "erin")
	const low, high = "00000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff"
	for _, id := range []string{high, low} {
		a.exec(t, `INSERT INTO watches (id, owner_id, name, kind, config) VALUES ($1, $2, $3, 'github_release', '{"repo":"a/b"}')`, id, dave.id, id)
		a.exec(t, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'rw')`, id, erin.id)
	}
	ctx := context.Background()
	purge, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = purge.Rollback(ctx) }()
	if _, err := purge.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, low); err != nil {
		t.Fatal(err)
	}
	run := a.callAsync(erin, http.MethodPost, "/api/v1/watches/run", nil)
	a.waitLockWaiters(t, 1)
	if _, err := purge.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, high); err != nil {
		t.Fatalf("purge's second watch lock: %v", err)
	}
	if err := purge.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-run; code != http.StatusAccepted {
		t.Fatalf("run-all: %d, want 202", code)
	}
}

// waitQueryLocked blocks until a backend of this database waits on a lock in a statement matching pattern.
func (a *devApp) waitQueryLocked(t *testing.T, pattern string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for a.countRows(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`, pattern) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no backend locked in %q", pattern)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Router versus purge, through the real writers. The router read dave's rule while he
// was active; a held channel parks it on an earlier rule's delivery, which leaves room for a disable and a
// purge. The purge deletes the rule and waits on the router's event; the router, let go, waits on the rule.
// PostgreSQL kills the purge, which waited first, and its retry answers 204 instead of 500.
func TestPurgeRacingRouterRetries(t *testing.T) {
	a := newDevApp(t)
	retries := testpg.AllowDeadlockRetries(t)
	admin, dave, erin := a.signIn(t, "admin"), a.signIn(t, "dave"), a.signIn(t, "erin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	var w watchJSON
	a.mustCall(t, dave, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, dave, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": erin.email, "permission": "ro"}, http.StatusCreated, nil)
	rule := func(u apiUser, name, channel string) {
		t.Helper()
		a.mustCall(t, u, http.MethodPost, "/api/v1/rules", map[string]any{"name": name, "channel_ids": []string{channel}}, http.StatusCreated, nil)
	}
	channel := func(u apiUser, name string) string {
		t.Helper()
		var c channelJSON
		a.mustCall(t, u, http.MethodPost, "/api/v1/channels", map[string]any{"name": name, "type": "webhook", "config": map[string]any{"url": "https://hook.test/x"}}, http.StatusCreated, &c)
		return c.ID
	}
	first := channel(erin, "erin's")
	rule(erin, "first", first)
	rule(dave, "second", channel(dave, "dave's"))
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k')`, w.ID)

	ctx := context.Background()
	hold, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, first); err != nil {
		t.Fatal(err)
	}
	router := route.NewRouter(a.pool, zap.NewNop(), route.NewMetrics(prometheus.NewRegistry()), time.Hour)
	routed := make(chan error, 1)
	go func() {
		_, err := router.RouteOnce(ctx)
		routed <- err
	}()
	a.waitQueryLocked(t, "INSERT INTO deliveries%")
	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/disable", nil, http.StatusNoContent, nil)
	purge := a.callAsync(admin, http.MethodPost, "/api/v1/admin/users/"+dave.id+"/purge", nil)
	a.waitQueryLocked(t, "%DELETE FROM events%")
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-purge; code != http.StatusNoContent {
		t.Fatalf("purge: %d, want 204", code)
	}
	if err := <-routed; err != nil {
		t.Fatalf("router: %v", err)
	}
	if n := retries(); n != 1 {
		t.Fatalf("%d retries, want the purge's 1", n)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'user.purge'`); n != 1 {
		t.Fatalf("%d purge audit rows, want 1", n)
	}
	if n := a.countRows(t, `SELECT count(*) FROM rules WHERE owner_id = $1`, dave.id); n != 0 {
		t.Fatalf("%d of dave's rules survived", n)
	}
}

// Delete versus rule edit. The delete holds the rules it found and waits on the watch;
// a rule created now and then locked by an edit escapes its first lock. The delete's cascade waits on that
// rule while the edit waits on the watch. The delete is real, the edit's two statements are driven by hand:
// nothing parks a real edit between them. Its long timeout leaves detection, and the retry, to the delete.
func TestWatchDeleteRacingNewRuleRetries(t *testing.T) {
	a := newDevApp(t)
	retries := testpg.AllowDeadlockRetries(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	ctx := context.Background()
	hold, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w.ID); err != nil {
		t.Fatal(err)
	}
	deleted := a.callAsync(alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil)
	a.waitQueryLocked(t, "SELECT 1 FROM watches%")
	var rule struct {
		ID string `json:"id"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "late", "watch_id": w.ID}, http.StatusCreated, &rule)
	edit, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edit.Rollback(ctx) }()
	for _, sql := range []string{`SET LOCAL deadlock_timeout = '5s'`, `SELECT 1 FROM rules WHERE id = '` + rule.ID + `' FOR UPDATE`} {
		if _, err := edit.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a.waitQueryLocked(t, "DELETE FROM watches%")
	if _, err := edit.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w.ID); err != nil {
		t.Fatalf("edit's watch check: %v", err)
	}
	if err := edit.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-deleted; code != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", code)
	}
	if n := retries(); n != 1 {
		t.Fatalf("%d retries, want the delete's 1", n)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'watch.deleted'`); n != 1 {
		t.Fatalf("%d delete audit rows, want 1", n)
	}
}
