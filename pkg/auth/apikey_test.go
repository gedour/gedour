package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/audit"
)

const keysPath = "/api/v1/api-keys"

// createKeyAs posts a key through the API and fails the test unless it answers 201.
func (e *env) createKeyAs(t *testing.T, token, csrf string, body map[string]any) APIKeyCreated {
	t.Helper()
	code, raw := e.send(t, http.MethodPost, keysPath, token, csrf, body)
	if code != http.StatusCreated {
		t.Fatalf("create key: %d %s", code, raw)
	}
	var k APIKeyCreated
	if err := json.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *env) listKeysOf(t *testing.T, token string) []APIKey {
	t.Helper()
	code, raw := e.send(t, http.MethodGet, keysPath, token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("list keys: %d %s", code, raw)
	}
	var out struct{ Items []APIKey }
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Items
}

// keyRow stores a key straight in the table, with clause setting its state (e.g. "revoked_at = now()").
func (e *env) keyRow(t *testing.T, owner, clause string) (raw, id string) {
	t.Helper()
	raw = keyPrefix + randomToken()
	id = e.id(t, `INSERT INTO api_keys (user_id, name, hash, prefix, scope) VALUES ($1, 'k', $2, 'abcdefgh', 'read') RETURNING id::text`, owner, hashToken(raw))
	if clause != "" {
		e.exec(t, `UPDATE api_keys SET `+clause+` WHERE id = $1`, id)
	}
	return raw, id
}

func (e *env) wantInvalidKey(t *testing.T, raw string) {
	t.Helper()
	if p, err := e.svc.LookupKey(context.Background(), raw); err != ErrInvalidKey || p != (KeyPrincipal{}) { //nolint:errorlint // the same value, not one wrapping it
		t.Fatalf("lookup: %+v %v, want ErrInvalidKey", p, err)
	}
}

func (e *env) wantValidKey(t *testing.T, raw string) KeyPrincipal {
	t.Helper()
	p, err := e.svc.LookupKey(context.Background(), raw)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	return p
}

// 007-AC1: the key comes back once with prefix gdr_; the row holds its hash and display prefix, never the key,
// and the list never returns it.
func TestKeyCreateReturnsItOnce(t *testing.T) {
	e := setup(t, false)
	id, token, csrf := e.signedIn(t, "alice", false)
	k := e.createKeyAs(t, token, csrf, map[string]any{"name": "  laptop  "})
	if !regexp.MustCompile(`^gdr_[A-Za-z0-9_-]{43}$`).MatchString(k.Key) {
		t.Fatalf("key format: %q", k.Key)
	}
	// 007-AC2: absent scope and duration are read and 90 days.
	if k.Name != "laptop" || k.Scope != "read" || k.Prefix != k.Key[4:12] || k.Status != "active" || k.ExpiresAt == nil || k.EffectiveUntil == nil {
		t.Fatalf("created %+v", k.APIKey)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND user_id = $2 AND hash = $3 AND prefix = $4 AND expires_at = created_at + interval '90 days'`,
		k.ID, id, hashToken(k.Key), k.Prefix); n != 1 {
		t.Fatal("row does not hold the hash, the prefix and 90 days")
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys k WHERE to_jsonb(k)::text LIKE '%' || $1 || '%'`, k.Key[4:]); n != 0 {
		t.Fatal("a row holds the key")
	}
	_, raw := e.send(t, http.MethodGet, keysPath, token, "", nil)
	for _, leak := range []string{k.Key, k.Key[4:], hex.EncodeToString(hashToken(k.Key)), base64.StdEncoding.EncodeToString(hashToken(k.Key))} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("list holds %q: %s", leak, raw)
		}
	}
	if strings.Contains(string(raw), `"key"`) || strings.Contains(string(raw), `"hash"`) {
		t.Fatalf("list has a key or hash field: %s", raw)
	}
	if got := e.wantValidKey(t, k.Key); got.KeyID != k.ID || got.User.ID != id || got.Scope != "read" || got.Dormant {
		t.Fatalf("lookup %+v", got)
	}
}

// 007-AC2: each duration stores its end date; never stores none and works past 365 days. Scopes are stored as given.
// Invalid values get their 400 in pkg/server, where huma's 422 becomes 400.
func TestKeyDurationsAndScopes(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "alice", false)
	for _, tt := range []struct {
		duration string
		sql      string
	}{
		{"7", `expires_at = created_at + interval '7 days'`},
		{"30", `expires_at = created_at + interval '30 days'`},
		{"90", `expires_at = created_at + interval '90 days'`},
		{"365", `expires_at = created_at + interval '365 days'`},
		{"never", `expires_at IS NULL`},
	} {
		k := e.createKeyAs(t, token, csrf, map[string]any{"name": tt.duration, "duration": tt.duration})
		if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND `+tt.sql, k.ID); n != 1 {
			t.Errorf("duration %s: want %s", tt.duration, tt.sql)
		}
	}
	for _, scope := range []string{"read", "write", "manage"} {
		k := e.createKeyAs(t, token, csrf, map[string]any{"name": "s", "scope": scope, "duration": "never"})
		if got := e.wantValidKey(t, k.Key); got.Scope != scope {
			t.Errorf("scope %s: %q", scope, got.Scope)
		}
		if scope == "read" {
			e.exec(t, `UPDATE api_keys SET created_at = now() - interval '400 days' WHERE id = $1`, k.ID)
			if got := e.wantValidKey(t, k.Key); got.Dormant {
				t.Error("a never key past 365 days stopped working")
			}
			e.exec(t, `DELETE FROM api_keys WHERE expires_at IS NOT NULL OR id <> $1`, k.ID) // room under the cap
		}
	}
}

// The name is 1 to 64 characters after trimming, counted in characters, not bytes.
func TestKeyNameLimits(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "alice", false)
	for name, want := range map[string]int{
		"":                                  400,
		"   ":                               400,
		"a\x00b":                            400,
		strings.Repeat("é", 65):             400,
		strings.Repeat("é", 64):             201,
		" " + strings.Repeat("x", 64) + " ": 201,
	} {
		if code, raw := e.send(t, http.MethodPost, keysPath, token, csrf, map[string]any{"name": name}); code != want {
			t.Errorf("name %q: %d %s, want %d", name, code, raw, want)
		}
	}
}

// 007-AC3: the 11th active key gets 409; revoked and expired ones do not count.
func TestKeyCap(t *testing.T) {
	e := setup(t, false)
	id, token, csrf := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	e.keyRow(t, id, "revoked_at = now()")
	e.keyRow(t, id, "expires_at = now() - interval '1 minute'")
	e.keyRow(t, bob, "")
	for range maxActiveKeys {
		e.createKeyAs(t, token, csrf, map[string]any{"name": "k"})
	}
	if code, raw := e.send(t, http.MethodPost, keysPath, token, csrf, map[string]any{"name": "11th"}); code != http.StatusConflict {
		t.Fatalf("11th key: %d %s", code, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE user_id = $1`, id); n != maxActiveKeys+2 {
		t.Fatalf("%d rows for alice", n)
	}
	// One expiring frees a slot.
	e.exec(t, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = (SELECT id FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now() LIMIT 1)`, id)
	e.createKeyAs(t, token, csrf, map[string]any{"name": "fits again"})
}

// 007-AC3: a creation deletes the caller's keys dead for over 30 days, and nobody else's.
func TestKeyCleanup(t *testing.T) {
	e := setup(t, false)
	id, token, csrf := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	_, oldRevoked := e.keyRow(t, id, "revoked_at = now() - interval '31 days'")
	_, oldExpired := e.keyRow(t, id, "expires_at = now() - interval '31 days'")
	_, recentRevoked := e.keyRow(t, id, "revoked_at = now() - interval '29 days'")
	_, recentExpired := e.keyRow(t, id, "expires_at = now() - interval '29 days'")
	_, live := e.keyRow(t, id, "")
	_, bobs := e.keyRow(t, bob, "revoked_at = now() - interval '31 days'")
	e.createKeyAs(t, token, csrf, map[string]any{"name": "new"})
	for keyID, want := range map[string]int{oldRevoked: 0, oldExpired: 0, recentRevoked: 1, recentExpired: 1, live: 1, bobs: 1} {
		if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1`, keyID); n != want {
			t.Errorf("key %s: %d rows, want %d", keyID, n, want)
		}
	}
}

// 007-AC3: two concurrent creates at 9 keys leave at most 10.
func TestKeyCapConcurrent(t *testing.T) {
	e := setup(t, false)
	id, token, csrf := e.signedIn(t, "alice", false)
	for range maxActiveKeys - 1 {
		e.keyRow(t, id, "")
	}
	a := e.sendAsync(http.MethodPost, keysPath, token, csrf, map[string]any{"name": "a"})
	b := e.sendAsync(http.MethodPost, keysPath, token, csrf, map[string]any{"name": "b"})
	codes := map[int]int{<-a: 1}
	codes[<-b]++
	if codes[http.StatusCreated] != 1 || codes[http.StatusConflict] != 1 {
		t.Fatalf("codes %v", codes)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE user_id = $1`, id); n != maxActiveKeys {
		t.Fatalf("%d keys", n)
	}
}

// A create that passed Guard before a disable committed waits on the user row, then is refused: its key would
// otherwise miss the disable's revocation.
func TestKeyCreateRacingDisable(t *testing.T) {
	e := setup(t, false)
	id, token, csrf := e.signedIn(t, "alice", false)
	blocker := e.begin(t)
	if _, err := blocker.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	got := e.sendAsync(http.MethodPost, keysPath, token, csrf, map[string]any{"name": "late"})
	e.waitLockWaiters(t, 1)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := <-got; code != http.StatusConflict {
		t.Fatalf("create: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE user_id = $1`, id); n != 0 {
		t.Fatalf("%d keys survive the disable", n)
	}
}

// 007-AC4: no session 401 and a bearer key changes nothing, on every route; a session without CSRF 403 on mutations.
func TestKeyRoutesNeedSessionAndCSRF(t *testing.T) {
	e := setup(t, false)
	id, token, _ := e.signedIn(t, "alice", false)
	raw, keyID := e.keyRow(t, id, "")
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, keysPath},
		{http.MethodPost, keysPath},
		{http.MethodDelete, keysPath + "/" + keyID},
	} {
		t.Run(r.method, func(t *testing.T) {
			for name, hdr := range map[string]map[string]string{
				"nothing": nil,
				"bearer":  {"Authorization": "Bearer " + raw},
			} {
				if resp := e.do(t, r.method, r.path, hdr); resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s: %d, want 401", name, resp.StatusCode)
				}
			}
			if r.method == http.MethodGet {
				return
			}
			resp := e.call(t, r.method, r.path, token, "")
			if resp.StatusCode != http.StatusForbidden || problemTitle(t, resp) != CSRFProblemTitle {
				t.Errorf("no CSRF: %d", resp.StatusCode)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE revoked_at IS NULL`); n != 1 {
		t.Fatalf("%d live keys, want the one untouched", n)
	}
}

// 007-AC4: no key route is public; adding one to publicAPI would open it before any session check.
func TestKeyRoutesNotPublic(t *testing.T) {
	for path := range publicAPI {
		if strings.Contains(path, "api-keys") {
			t.Errorf("%s is public", path)
		}
	}
}

// 007-AC5: acting, a create gets 409; list and revoke work on the target and audit on_behalf_of.
func TestKeyWhileActing(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	raw, keyID := e.keyRow(t, alice, "")
	e.keyRow(t, admin, "")
	if code, body := e.actAs(t, token, csrf, alice); code != http.StatusNoContent {
		t.Fatalf("act as: %d %s", code, body)
	}
	if code, body := e.send(t, http.MethodPost, keysPath, token, csrf, map[string]any{"name": "door"}); code != http.StatusConflict {
		t.Fatalf("create: %d %s", code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE name = 'door'`); n != 0 {
		t.Fatal("a key was created while acting")
	}
	items := e.listKeysOf(t, token)
	if len(items) != 1 || items[0].ID != keyID {
		t.Fatalf("list while acting: %+v", items)
	}
	if code, body := e.send(t, http.MethodDelete, keysPath+"/"+keyID, token, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, body)
	}
	e.wantInvalidKey(t, raw)
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'api_key.revoked' AND actor_user_id = $1 AND on_behalf_of = $2 AND target_id = $3`, admin, alice, keyID); n != 1 {
		t.Fatalf("revoke audit rows with on_behalf_of: %d", n)
	}
}

// 007-AC6: another user's key is 404 and stays alive, and does not show in the list.
// 007-AC7: after a revoke the key is invalid; a second revoke is 204 with no second audit row.
func TestKeyRevoke(t *testing.T) {
	e := setup(t, false)
	alice, token, csrf := e.signedIn(t, "alice", false)
	_, bobToken, bobCSRF := e.signedIn(t, "bob", false)
	raw, keyID := e.keyRow(t, alice, "")
	if code, _ := e.send(t, http.MethodDelete, keysPath+"/"+keyID, bobToken, bobCSRF, nil); code != http.StatusNotFound {
		t.Fatalf("bob's revoke: %d", code)
	}
	if code, raw := e.send(t, http.MethodGet, keysPath, bobToken, "", nil); code != http.StatusOK || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("bob lists %d %s, want an empty array", code, raw)
	}
	e.wantValidKey(t, raw)
	for range 2 {
		if code, body := e.send(t, http.MethodDelete, keysPath+"/"+keyID, token, csrf, nil); code != http.StatusNoContent {
			t.Fatalf("revoke: %d %s", code, body)
		}
		e.wantInvalidKey(t, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'api_key.revoked'`); n != 1 {
		t.Fatalf("%d revoke audit rows", n)
	}
	if code, _ := e.send(t, http.MethodDelete, keysPath+"/00000000-0000-0000-0000-000000000000", token, csrf, nil); code != http.StatusNotFound {
		t.Fatalf("unknown key: %d", code)
	}
	if items := e.listKeysOf(t, token); len(items) != 1 || items[0].Status != "revoked" || items[0].RevokedAt == nil {
		t.Fatalf("list: %+v", items)
	}
}

// 007-AC8: the audit rows hold real ID, name, scope and expiry; no row anywhere holds the key or its hash, in any encoding.
func TestKeyAudit(t *testing.T) {
	e := setup(t, false)
	id, token, csrf := e.signedIn(t, "alice", false)
	k := e.createKeyAs(t, token, csrf, map[string]any{"name": "laptop", "scope": "write", "duration": "30"})
	if code, _ := e.send(t, http.MethodDelete, keysPath+"/"+k.ID, token, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	for _, action := range []string{"api_key.created", "api_key.revoked"} {
		var raw []byte
		var actor, target string
		if err := e.pool.QueryRow(context.Background(), `SELECT after::text, actor_user_id::text, target_id FROM audit_log WHERE action = $1 AND target_type = 'api_key'`, action).
			Scan(&raw, &actor, &target); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		var got keyAudit
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), audit.Redacted) {
			t.Errorf("%s redacted: %s", action, raw)
		}
		if actor != id || target != k.ID || got.ID != k.ID || got.Name != "laptop" || got.Scope != "write" || got.ExpiresAt == nil || !got.ExpiresAt.Equal(*k.ExpiresAt) {
			t.Errorf("%s: %s by %s on %s", action, raw, actor, target)
		}
	}
	rows, err := e.pool.Query(context.Background(), `SELECT coalesce(before::text, '') || coalesce(after::text, '') || target_id FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("%d audit rows", len(all))
	}
	sum := hashToken(k.Key)
	for _, row := range all {
		for _, leak := range []string{k.Key, k.Key[4:], hex.EncodeToString(sum), base64.StdEncoding.EncodeToString(sum),
			base64.RawStdEncoding.EncodeToString(sum), base64.RawURLEncoding.EncodeToString(sum)} {
			if strings.Contains(row, leak) {
				t.Errorf("audit row holds %q: %s", leak, row)
			}
		}
	}
}

// 007-AC9: last_used_at moves on use, at most once a minute.
func TestKeyLastUsed(t *testing.T) {
	e := setup(t, false)
	id, _, _ := e.signedIn(t, "alice", false)
	raw, keyID := e.keyRow(t, id, "")
	e.wantValidKey(t, raw)
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND last_used_at IS NOT NULL`, keyID); n != 1 {
		t.Fatal("first use did not move last_used_at")
	}
	e.exec(t, `UPDATE api_keys SET last_used_at = now() - interval '30 seconds' WHERE id = $1`, keyID)
	e.wantValidKey(t, raw)
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND last_used_at < now() - interval '29 seconds'`, keyID); n != 1 {
		t.Fatal("moved within the minute")
	}
	e.exec(t, `UPDATE api_keys SET last_used_at = now() - interval '61 seconds' WHERE id = $1`, keyID)
	e.wantValidKey(t, raw)
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND last_used_at > now() - interval '5 seconds'`, keyID); n != 1 {
		t.Fatal("did not move after the minute")
	}
}

// Every invalid class answers the very same value, and a non-gdr_ string never reaches the database.
func TestKeyLookupInvalidClasses(t *testing.T) {
	e := setup(t, false)
	alice, _, _ := e.signedIn(t, "alice", false)
	disabled, _, _ := e.signedIn(t, "disabled", false)
	purged, _, _ := e.signedIn(t, "purged", false)
	revoked, _ := e.keyRow(t, alice, "revoked_at = now()")
	expired, _ := e.keyRow(t, alice, "expires_at = now() - interval '1 second'")
	ofDisabled, _ := e.keyRow(t, disabled, "")
	ofPurged, _ := e.keyRow(t, purged, "")
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, disabled)
	e.exec(t, `UPDATE users SET purged_at = now() WHERE id = $1`, purged)
	for name, raw := range map[string]string{
		"empty":          "",
		"no prefix":      randomToken(),
		"other prefix":   "ghp_" + randomToken(),
		"prefix only":    keyPrefix,
		"truncated":      keyPrefix + "abc",
		"unknown":        keyPrefix + randomToken(),
		"revoked":        revoked,
		"expired":        expired,
		"disabled owner": ofDisabled,
		"purged owner":   ofPurged,
	} {
		t.Run(name, func(t *testing.T) { e.wantInvalidKey(t, raw) })
	}
}

// 007-AC20's base: a database failure is an error of its own, never ErrInvalidKey; a non-gdr_ string needs no database.
func TestKeyLookupDatabaseError(t *testing.T) {
	e := setup(t, false)
	e.pool.Close()
	if _, err := e.svc.LookupKey(context.Background(), keyPrefix+randomToken()); err == nil || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("with no database: %v", err)
	}
	e.wantInvalidKey(t, "not-a-key")
}

// 007-AC10: a disable revokes the account's keys only; an enable leaves them revoked and a new key can follow.
func TestKeyDisableEnable(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	rawA, a := e.keyRow(t, alice, "")
	_, expired := e.keyRow(t, alice, "expires_at = now() - interval '1 minute'")
	rawB, _ := e.keyRow(t, bob, "")
	if code, body := e.send(t, http.MethodPost, "/api/v1/admin/users/"+alice+"/disable", token, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("disable: %d %s", code, body)
	}
	e.wantInvalidKey(t, rawA)
	e.wantValidKey(t, rawB)
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NOT NULL`, a); n != 1 {
		t.Fatal("not revoked")
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NULL`, expired); n != 1 {
		t.Fatal("an expired key was revoked too: its cleanup clock restarted")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.disable' AND target_id = $1 AND after->>'revoked_api_key_count' = '1'`, alice); n != 1 {
		t.Fatal("the disable's audit row does not hold the real count")
	}
	if code, body := e.send(t, http.MethodPost, "/api/v1/admin/users/"+alice+"/enable", token, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("enable: %d %s", code, body)
	}
	e.wantInvalidKey(t, rawA)
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.enable' AND target_id = $1 AND after ? 'revoked_api_key_count'`, alice); n != 0 {
		t.Fatal("the enable's audit row has a revoked count")
	}
	// A disable with nothing to revoke keeps the row it always wrote.
	if code, body := e.send(t, http.MethodPost, "/api/v1/admin/users/"+alice+"/disable", token, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("second disable: %d %s", code, body)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.disable' AND target_id = $1 AND after ? 'revoked_api_key_count'`, alice); n != 1 {
		t.Fatal("a disable revoking nothing wrote a count")
	}
}

// 007-AC11: the preview counts the keys, the purge deletes them in its tx, the audit rows naming them stay.
func TestKeyPurge(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, daveToken, daveCSRF := e.signedIn(t, "dave", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	k := e.createKeyAs(t, daveToken, daveCSRF, map[string]any{"name": "dave's"})
	e.keyRow(t, dave, "revoked_at = now()")
	rawB, _ := e.keyRow(t, bob, "")
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	code, raw := e.send(t, http.MethodGet, "/api/v1/admin/users/"+dave+"/purge-preview", token, "", nil)
	var preview purgePreviewOutput
	if err := json.Unmarshal(raw, &preview.Body); code != http.StatusOK || err != nil || preview.Body.Counts.APIKeys != 2 {
		t.Fatalf("preview %d: %s (%v)", code, raw, err)
	}
	if code, raw := e.purge(t, token, csrf, dave); code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", code, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE user_id = $1`, dave); n != 0 {
		t.Fatalf("%d keys left", n)
	}
	e.wantInvalidKey(t, k.Key)
	e.wantValidKey(t, rawB)
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'api_key.created' AND target_id = $1 AND after->>'name' = 'dave''s'`, k.ID); n != 1 {
		t.Fatal("the audit row naming the key is gone")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.purge' AND target_id = $1 AND after->'deleted'->>'api_key_count' = '2'`, dave); n != 1 {
		t.Fatal("the purge audit does not hold the real count")
	}
}

// 007-AC12: a sign-in refused for a missing role revokes the user's active keys and says so in the audit row.
func TestKeyDeniedMissingRole(t *testing.T) {
	e := setup(t, false, withRoles)
	alice, aliceToken, _ := e.signedIn(t, "alice-sub", false)
	bob, bobToken, _ := e.signedIn(t, "bob", false)
	raw, id := e.keyRow(t, alice, "")
	raw2, _ := e.keyRow(t, alice, "")
	rawB, _ := e.keyRow(t, bob, "")
	_, dead := e.keyRow(t, alice, "revoked_at = now() - interval '2 days'")
	_, expired := e.keyRow(t, alice, "expires_at = now() - interval '1 minute'")
	e.stub.extra = map[string]any{"roles": []any{"other"}}
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/?auth=forbidden" {
		t.Fatalf("callback: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	e.wantInvalidKey(t, raw)
	e.wantInvalidKey(t, raw2)
	e.wantValidKey(t, rawB)
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at > now() - interval '1 minute'`, id); n != 1 {
		t.Fatal("not revoked")
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at < now() - interval '1 day'`, dead); n != 1 {
		t.Fatal("an old revocation moved")
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NULL`, expired); n != 1 {
		t.Fatal("an expired key was revoked")
	}
	if resp := e.call(t, http.MethodGet, "/api/v1/me", aliceToken, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session after the denial: %d", resp.StatusCode)
	}
	if resp := e.call(t, http.MethodGet, "/api/v1/me", bobToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("another user's session after the denial: %d", resp.StatusCode)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied' AND actor_user_id = $1
		AND after = '{"reason": "missing_role", "revoked_api_key_count": 2, "deleted_session_count": 1}'`, alice); n != 1 {
		t.Fatal("the denial's audit row does not hold the real counts")
	}
	// A second refusal finds nothing to revoke, and its row says only the reason.
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/?auth=forbidden" {
		t.Fatalf("callback: %q", resp.Header.Get("Location"))
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied' AND actor_user_id = $1 AND after = '{"reason": "missing_role"}'`, alice); n != 1 {
		t.Fatal("the second denial's row is not the reason alone")
	}
}

// 007-AC12: a refusal for a disabled account changes nothing more.
func TestKeyDeniedDisabled(t *testing.T) {
	e := setup(t, false, withRoles)
	alice, _, _ := e.signedIn(t, "alice-sub", false)
	_, id := e.keyRow(t, alice, "")
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, alice)
	e.stub.extra = map[string]any{"roles": []any{"gedour-user"}}
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/?auth=forbidden" {
		t.Fatalf("callback: %q", resp.Header.Get("Location"))
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NULL`, id); n != 1 {
		t.Fatal("the disabled refusal touched the key")
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, alice); n != 1 {
		t.Fatal("the disabled refusal touched the session")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied' AND actor_user_id = $1 AND after = '{"reason": "disabled"}'`, alice); n != 1 {
		t.Fatal("denial row")
	}
}

// A missing-role refusal racing a key creation waits for it, then revokes the new key too.
func TestKeyDeniedRacingCreate(t *testing.T) {
	e := setup(t, false, withRoles)
	alice, _, _ := e.signedIn(t, "alice-sub", false)
	ctx := context.Background()
	blocker := e.begin(t)
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, alice); err != nil {
		t.Fatal(err)
	}
	raw := keyPrefix + randomToken()
	if _, err := blocker.Exec(ctx, `INSERT INTO api_keys (user_id, name, hash, prefix, scope) VALUES ($1, 'k', $2, 'abcdefgh', 'read')`, alice, hashToken(raw)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.svc.auditDenied(ctx, "alice-sub", deniedMissingRole) }()
	e.waitLockWaiters(t, 1)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.wantInvalidKey(t, raw)
}

// 007-AC13: a key whose owner has no sign-in within the max age goes dormant, never keys included, and the list says
// so; a sign-in wakes it.
func TestKeyDormant(t *testing.T) {
	e := setup(t, false, withRoles)
	alice, token, csrf := e.signedIn(t, "alice-sub", false)
	never := e.createKeyAs(t, token, csrf, map[string]any{"name": "never", "duration": "never"})
	dated := e.createKeyAs(t, token, csrf, map[string]any{"name": "dated", "duration": "7"})
	if p := e.wantValidKey(t, never.Key); p.Dormant || p.EffectiveUntil == nil {
		t.Fatalf("fresh key: %+v", p)
	}
	byName := func(items []APIKey) map[string]APIKey {
		out := map[string]APIKey{}
		for _, k := range items {
			out[k.Name] = k
		}
		return out
	}
	// A week-old sign-in: the sign-in limit comes first for never, the expiry for the 7-day key.
	e.exec(t, `UPDATE users SET last_login_at = now() - interval '7 days' WHERE id = $1`, alice)
	var login time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT last_login_at FROM users WHERE id = $1`, alice).Scan(&login); err != nil {
		t.Fatal(err)
	}
	limit := login.Add(720 * time.Hour)
	items := byName(e.listKeysOf(t, token))
	if u := items["never"].EffectiveUntil; u == nil || !u.Equal(limit) {
		t.Errorf("never key's effective_until %v, want %v", u, limit)
	}
	if u := items["dated"].EffectiveUntil; u == nil || !u.Equal(*dated.ExpiresAt) {
		t.Errorf("dated key's effective_until %v, want %v", u, dated.ExpiresAt)
	}
	if p := e.wantValidKey(t, never.Key); p.EffectiveUntil == nil || !p.EffectiveUntil.Equal(limit) {
		t.Errorf("lookup's end %v, want %v", p.EffectiveUntil, limit)
	}
	if p := e.wantValidKey(t, dated.Key); p.EffectiveUntil == nil || !p.EffectiveUntil.Equal(*dated.ExpiresAt) {
		t.Errorf("lookup's end on the dated key %v, want its expiry %v", p.EffectiveUntil, dated.ExpiresAt)
	}

	e.exec(t, `UPDATE users SET last_login_at = now() - interval '721 hours' WHERE id = $1`, alice)
	for _, raw := range []string{never.Key, dated.Key} {
		if p := e.wantValidKey(t, raw); !p.Dormant || p.User.ID != alice {
			t.Errorf("past the max age: %+v", p)
		}
	}
	for name, k := range byName(e.listKeysOf(t, token)) {
		if k.Status != "dormant" {
			t.Errorf("%s: status %s", name, k.Status)
		}
	}
	// Never signed in: nothing vouches for the owner.
	e.exec(t, `UPDATE users SET last_login_at = NULL WHERE id = $1`, alice)
	if p := e.wantValidKey(t, never.Key); !p.Dormant {
		t.Error("no sign-in at all is not dormant")
	}

	e.stub.extra = map[string]any{"roles": []any{"gedour-user"}}
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/" {
		t.Fatalf("sign-in: %q", resp.Header.Get("Location"))
	}
	for _, raw := range []string{never.Key, dated.Key} {
		if p := e.wantValidKey(t, raw); p.Dormant {
			t.Errorf("after a sign-in: %+v", p)
		}
	}
	if k := byName(e.listKeysOf(t, token))["never"]; k.Status != "active" {
		t.Errorf("after a sign-in: status %s", k.Status)
	}
}

// 007-AC13: dev mode has no sign-in to renew, so nothing goes dormant and a never key has no effective end.
func TestKeyDevModeSkipsSigninCheck(t *testing.T) {
	e := setup(t, true)
	sess, err := e.svc.devSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE users SET last_login_at = now() - interval '9000 hours' WHERE id = $1`, sess.ID)
	raw, _ := e.keyRow(t, sess.ID, "")
	if p := e.wantValidKey(t, raw); p.Dormant || p.EffectiveUntil != nil {
		t.Fatalf("dev key: %+v", p)
	}
	_, dated := e.keyRow(t, sess.ID, "expires_at = now() + interval '1 day'")
	resp := e.do(t, http.MethodGet, keysPath, nil)
	var out struct{ Items []APIKey }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items %+v", out.Items)
	}
	for _, k := range out.Items {
		if k.Status != "active" || (k.ID == dated) != (k.EffectiveUntil != nil) {
			t.Errorf("dev list: %+v", k)
		}
	}
}

// armBoom makes a statement on a table fail, as a broken database would.
func (e *env) armBoom(t *testing.T, event, table string) {
	t.Helper()
	e.exec(t, `CREATE FUNCTION boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END; $$`)
	e.exec(t, `CREATE TRIGGER boom BEFORE `+event+` ON `+table+` FOR EACH STATEMENT EXECUTE FUNCTION boom()`)
}

// wantOpaque500 fails unless the answer is a 500 that tells the client nothing about the database.
func wantOpaque500(t *testing.T, code int, raw []byte) {
	t.Helper()
	if code != http.StatusInternalServerError || !strings.Contains(string(raw), "internal error") || strings.Contains(string(raw), "boom") || strings.Contains(string(raw), "api_keys") {
		t.Fatalf("%d %s, want an opaque 500", code, raw)
	}
}

// Whichever step of a create fails: an opaque 500, no new key, no audit row, and the cleanup rolled back with it.
func TestKeyCreateFailureRollsBack(t *testing.T) {
	for name, arm := range map[string]func(*env, *testing.T, string){
		"count":   func(e *env, t *testing.T, _ string) { e.exec(t, `ALTER TABLE api_keys RENAME TO gone`) },
		"cleanup": func(e *env, t *testing.T, _ string) { e.armBoom(t, "DELETE", "api_keys") },
		"insert":  func(e *env, t *testing.T, _ string) { e.armBoom(t, "INSERT", "api_keys") },
		"audit":   func(e *env, t *testing.T, _ string) { e.armBoom(t, "INSERT", "audit_log") },
		// pgx cannot scan an infinite timestamp: a row that does not read back must not hand out a key.
		"read back": func(e *env, t *testing.T, id string) {
			e.exec(t, `UPDATE users SET last_login_at = 'infinity' WHERE id = $1`, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, false)
			id, token, csrf := e.signedIn(t, "alice", false)
			e.keyRow(t, id, "revoked_at = now() - interval '31 days'")
			audits := e.count(t, `SELECT count(*) FROM audit_log`)
			arm(e, t, id)
			code, raw := e.send(t, http.MethodPost, keysPath, token, csrf, map[string]any{"name": "new", "duration": "never"})
			wantOpaque500(t, code, raw)
			if strings.Contains(string(raw), keyPrefix) {
				t.Fatalf("a failed create answered a key: %s", raw)
			}
			if name == "count" {
				e.exec(t, `ALTER TABLE gone RENAME TO api_keys`)
			}
			if n := e.count(t, `SELECT count(*) FROM api_keys WHERE name = 'k'`); n != 1 {
				t.Errorf("the dead key's cleanup survived the failure: %d rows", n)
			}
			if n := e.count(t, `SELECT count(*) FROM api_keys WHERE name = 'new'`); n != 0 {
				t.Errorf("%d new keys", n)
			}
			if n := e.count(t, `SELECT count(*) FROM audit_log`); n != audits {
				t.Errorf("%d audit rows added", n-audits)
			}
		})
	}
}

// A create for a session whose user row cannot be locked fails opaque, as a database failure does.
func TestKeyCreateLockFailure(t *testing.T) {
	e := setup(t, false)
	ctx := context.WithValue(context.Background(), ctxKey{}, &session{User: User{ID: "not-a-uuid"}})
	in := &createAPIKeyInput{}
	in.Body.Name, in.Body.Scope, in.Body.Duration = "k", "read", "7"
	var se huma.StatusError
	if _, err := e.svc.createKey(ctx, in); !errors.As(err, &se) || se.GetStatus() != http.StatusInternalServerError {
		t.Fatalf("create: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys`); n != 0 {
		t.Fatalf("%d keys", n)
	}
}

// A list that fails is an opaque 500, never an empty list a page would read as "no keys".
func TestKeyListFailure(t *testing.T) {
	e := setup(t, false)
	id, token, _ := e.signedIn(t, "alice", false)
	e.keyRow(t, id, "")
	e.exec(t, `ALTER TABLE api_keys RENAME TO gone`)
	code, raw := e.send(t, http.MethodGet, keysPath, token, "", nil)
	wantOpaque500(t, code, raw)
}

// A revoke that fails leaves the key working, with no audit row claiming otherwise.
func TestKeyRevokeFailure(t *testing.T) {
	for name, arm := range map[string]func(*env, *testing.T){
		"lock":   func(e *env, t *testing.T) { e.exec(t, `ALTER TABLE api_keys RENAME TO gone`) },
		"update": func(e *env, t *testing.T) { e.armBoom(t, "UPDATE", "api_keys") },
		"audit":  func(e *env, t *testing.T) { e.armBoom(t, "INSERT", "audit_log") },
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, false)
			id, token, csrf := e.signedIn(t, "alice", false)
			_, keyID := e.keyRow(t, id, "")
			arm(e, t)
			code, raw := e.send(t, http.MethodDelete, keysPath+"/"+keyID, token, csrf, nil)
			wantOpaque500(t, code, raw)
			if name == "lock" {
				e.exec(t, `ALTER TABLE gone RENAME TO api_keys`)
			}
			if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NULL`, keyID); n != 1 {
				t.Error("the key was revoked by a failed request")
			}
			if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'api_key.revoked'`); n != 0 {
				t.Errorf("%d audit rows", n)
			}
		})
	}
}

// 007-AC10: revoking is part of the disable: if it fails, the account stays active and nothing is half done.
func TestKeyDisableFailureRollsBack(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	raw, _ := e.keyRow(t, alice, "")
	e.armBoom(t, "UPDATE", "api_keys")
	code, body := e.send(t, http.MethodPost, "/api/v1/admin/users/"+alice+"/disable", token, csrf, nil)
	wantOpaque500(t, code, body)
	if n := e.count(t, `SELECT count(*) FROM users WHERE id = $1 AND disabled_at IS NULL`, alice); n != 1 {
		t.Fatal("the account was disabled while its keys stayed valid")
	}
	if n := e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, alice); n != 1 {
		t.Fatal("sessions were revoked by a failed disable")
	}
	e.exec(t, `DROP TRIGGER boom ON api_keys`)
	e.wantValidKey(t, raw)
}

// 007-AC12: a revocation that fails reports it, and writes no denial row saying it happened.
func TestKeyDeniedFailure(t *testing.T) {
	e := setup(t, false)
	alice, _, _ := e.signedIn(t, "alice-sub", false)
	e.keyRow(t, alice, "")
	e.armBoom(t, "UPDATE", "api_keys")
	if err := e.svc.auditDenied(context.Background(), "alice-sub", deniedMissingRole); err == nil {
		t.Fatal("no error")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied'`); n != 0 {
		t.Fatalf("%d denial rows", n)
	}
}

// A refusal whose user lookup fails writes nothing and revokes nothing; deny logs it and the refusal stands.
func TestKeyDeniedLookupFailure(t *testing.T) {
	e := setup(t, false)
	alice, _, _ := e.signedIn(t, "alice-sub", false)
	_, keyID := e.keyRow(t, alice, "")
	e.exec(t, `ALTER TABLE users RENAME TO gone`)
	if err := e.svc.auditDenied(context.Background(), "alice-sub", deniedMissingRole); err == nil {
		t.Fatal("no error")
	}
	e.exec(t, `ALTER TABLE gone RENAME TO users`)
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NULL`, keyID) + e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'user.login_denied'`); n != 1 {
		t.Fatalf("key alive + denial rows = %d, want 1", n)
	}
}

// 007-AC20's base: past the read, a failed clock write or an unreadable row is still a database error, never invalid.
func TestKeyLookupFailuresAreNotInvalid(t *testing.T) {
	for name, arm := range map[string]func(*env, *testing.T, string){
		"touch": func(e *env, t *testing.T, _ string) { e.armBoom(t, "UPDATE", "api_keys") },
		"scan": func(e *env, t *testing.T, id string) {
			e.exec(t, `UPDATE users SET last_login_at = 'infinity' WHERE id = $1`, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, false)
			id, _, _ := e.signedIn(t, "alice", false)
			raw, _ := e.keyRow(t, id, "")
			arm(e, t, id)
			if p, err := e.svc.LookupKey(context.Background(), raw); err == nil || errors.Is(err, ErrInvalidKey) || p != (KeyPrincipal{}) {
				t.Fatalf("%+v %v, want a database error and no principal", p, err)
			}
		})
	}
}

// A handler reached without Guard answers 401 and does nothing.
func TestKeyHandlersNeedSession(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	_, list := e.svc.listKeys(ctx, nil)
	_, create := e.svc.createKey(ctx, &createAPIKeyInput{})
	_, revoke := e.svc.revokeKey(ctx, &userIDPath{ID: "00000000-0000-0000-0000-000000000000"})
	for name, err := range map[string]error{"list": list, "create": create, "revoke": revoke} {
		var se huma.StatusError
		if !errors.As(err, &se) || se.GetStatus() != http.StatusUnauthorized {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A key's status names what ended it first in meaning: revoked beats expired, expired beats dormant, and the list runs newest first.
func TestKeyListStatusAndOrder(t *testing.T) {
	e := setup(t, false)
	id, token, _ := e.signedIn(t, "alice", false)
	e.exec(t, `UPDATE users SET last_login_at = now() - interval '721 hours' WHERE id = $1`, id)
	want := map[string]string{}
	var newestFirst []string
	for i, tt := range []struct{ clause, status string }{
		{"revoked_at = now(), expires_at = now() - interval '1 day'", "revoked"}, // revoked, expired, dormant
		{"revoked_at = now()", "revoked"},                                        // revoked, dormant
		{"expires_at = now() - interval '1 day'", "expired"},                     // expired, dormant
		{"", "dormant"},
	} {
		_, keyID := e.keyRow(t, id, tt.clause)
		e.exec(t, `UPDATE api_keys SET created_at = now() - make_interval(hours => $2::int) WHERE id = $1`, keyID, i+1)
		want[keyID] = tt.status
		newestFirst = append(newestFirst, keyID)
	}
	items := e.listKeysOf(t, token)
	if len(items) != len(want) {
		t.Fatalf("items %+v", items)
	}
	for i, k := range items {
		if k.ID != newestFirst[i] {
			t.Errorf("position %d: %s, want %s", i, k.ID, newestFirst[i])
		}
		if k.Status != want[k.ID] {
			t.Errorf("position %d: status %s, want %s", i, k.Status, want[k.ID])
		}
	}
}

// Security: the one answer holding the key must stay out of every cache.
func TestKeyCreateNoStore(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "alice", false)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.app.URL+keysPath, strings.NewReader(`{"name":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, csrf)
	req.AddCookie(e.svc.sessionCookie(token, 60))
	resp, err := e.app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

// 007-AC12: a create already past Guard when a refusal ended its session waits on the user row, then gets 401 and no key.
func TestKeyCreateRacingDenial(t *testing.T) {
	e := setup(t, false, withRoles)
	id, token, csrf := e.signedIn(t, "alice-sub", false)
	ctx := context.Background()
	blocker := e.begin(t)
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	got := e.sendAsync(http.MethodPost, keysPath, token, csrf, map[string]any{"name": "late"})
	e.waitLockWaiters(t, 1)
	if _, err := blocker.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-got; code != http.StatusUnauthorized {
		t.Fatalf("create: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM api_keys WHERE user_id = $1`, id); n != 0 {
		t.Fatalf("%d keys survive the refusal", n)
	}
}

// 007-AC12: the revocation outlives a dropped callback.
func TestKeyDenyOutlivesTheRequest(t *testing.T) {
	e := setup(t, false, withRoles)
	id, token, _ := e.signedIn(t, "alice-sub", false)
	raw, _ := e.keyRow(t, id, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.svc.deny(ctx, "alice-sub", deniedMissingRole)
	e.wantInvalidKey(t, raw)
	if resp := e.call(t, http.MethodGet, "/api/v1/me", token, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session after the denial: %d", resp.StatusCode)
	}
}

// A revocation that fails is visible in the logs, naming who still holds keys; a plain denial's failure stays its own event.
func TestKeyDenyFailureIsLogged(t *testing.T) {
	for reason, event := range map[string]string{deniedMissingRole: "login_denied_revocation_lost", deniedDisabled: "login_denied_audit_failed"} {
		t.Run(reason, func(t *testing.T) {
			e := setup(t, false, withRoles)
			e.signedIn(t, "alice-sub", false)
			core, logs := observer.New(zap.ErrorLevel)
			e.svc.log = zap.New(core)
			e.armBoom(t, "INSERT", "audit_log")
			e.svc.deny(context.Background(), "alice-sub", reason)
			entries := logs.FilterMessage(event).All()
			if len(entries) != 1 || entries[0].ContextMap()["oidc_sub"] != "alice-sub" || entries[0].ContextMap()["reason"] != reason {
				t.Fatalf("logged %+v", logs.All())
			}
		})
	}
}

// 007-AC12: the sessions go in the same tx as the revocation: if deleting them fails, the keys stay as they were.
func TestKeyDeniedSessionFailureRollsBack(t *testing.T) {
	e := setup(t, false)
	id, _, _ := e.signedIn(t, "alice-sub", false)
	raw, _ := e.keyRow(t, id, "")
	e.armBoom(t, "DELETE", "sessions")
	if err := e.svc.auditDenied(context.Background(), "alice-sub", deniedMissingRole); err == nil {
		t.Fatal("no error")
	}
	e.wantValidKey(t, raw)
}

// 007-AC14: a key's principal is its user and nothing else: no admin flag, no CSRF token, not acting as anyone.
func TestWithKeyPrincipal(t *testing.T) {
	ctx := WithKeyPrincipal(context.Background(), KeyPrincipal{User: User{ID: "u1"}, KeyID: "k1", Scope: "manage"})
	sess, err := sessionOf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "u1" || sess.isAdmin || sess.admin != nil || sess.csrf != "" || sess.id != nil {
		t.Fatalf("%+v", sess)
	}
	if u, ok := UserFrom(ctx); !ok || u.ID != "u1" {
		t.Fatalf("%+v %v", u, ok)
	}
}
