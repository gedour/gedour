package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/testpg"
)

// ruleOn inserts owner's rule on the given watches, all of them when none.
func (e *env) ruleOn(t *testing.T, owner, name string, watches ...string) string {
	t.Helper()
	return testpg.InsertRule(t, e.pool, testpg.Rule{Owner: owner, Name: name, Watches: watches})
}

func (e *env) purge(t *testing.T, token, csrf, id string) (int, []byte) {
	t.Helper()
	return e.send(t, http.MethodPost, "/api/v1/admin/users/"+id+"/purge", token, csrf, nil)
}

// 006-AC12: an active account is not purged, and nothing it owns is touched.
func TestPurgeActiveRefused(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	e.watchOf(t, alice, "w", nil)
	audits := e.count(t, `SELECT count(*) FROM audit_log`)
	if code, raw := e.purge(t, token, csrf, alice); code != http.StatusConflict {
		t.Fatalf("purge active: %d %s", code, raw)
	}
	if code, _ := e.purge(t, token, csrf, "00000000-0000-0000-0000-000000000000"); code != http.StatusNotFound {
		t.Fatalf("purge unknown: %d", code)
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE id = $1 AND purged_at IS NULL AND email = 'alice@x.test'`, alice) +
		e.count(t, `SELECT count(*) FROM watches WHERE owner_id = $1`, alice) + e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, alice); n != 3 {
		t.Fatalf("refused purge changed the account (%d)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log`); n != audits {
		t.Fatalf("refused purge wrote %d audit rows", n-audits)
	}
}

// 006-AC13: a disabled account's watches, channels (one used by its rule: rule_channels RESTRICT),
// rules, scripts, credentials, inbox and sessions go in one tx, other users' rules on its watches
// with them; email and name are blanked, every audit row stays, and the list shows it purged.
// 006-AC18: audited with the status before and the counts after. 006-AC14: its sign-in stays refused.
func TestPurgeUser(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave-sub", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	e.exec(t, `UPDATE users SET display_name = 'Dave' WHERE id = $1`, dave)
	cred, _ := e.sealedCredential(t, dave, "gh", "plain-secret-value-123")
	w := e.watchOf(t, dave, "dave's", &cred)
	ch := e.id(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'ch', 'webhook') RETURNING id::text`, dave)
	daveRule := e.ruleOf(t, dave, "mine", &w)
	e.exec(t, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, daveRule, ch)
	erinCh := e.id(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'erin ch', 'webhook') RETURNING id::text`, erin)
	erinOnW := e.ruleOf(t, erin, "erin on dave's", &w)
	e.exec(t, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, erinOnW, erinCh)
	erinAll := e.ruleOf(t, erin, "erin all", nil)
	erinW := e.watchOf(t, erin, "erin's", nil)
	e.exec(t, `INSERT INTO scripts (owner_id, name, source) VALUES ($1, 's', '')`, dave)
	ev := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, erinW)
	daveEv := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, w)
	e.exec(t, `INSERT INTO inbox_items (user_id, event_id) VALUES ($1, $2), ($1, $4), ($3, $4), ($3, $2)`, dave, ev, erin, daveEv)
	e.exec(t, `INSERT INTO deliveries (event_id, rule_id, channel_id) VALUES ($1, $2, $3)`, daveEv, erinOnW, erinCh)
	// Set straight in the table, so dave's session row stays: the purge must not rely on the disable's revocation.
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)

	code, raw := e.send(t, http.MethodGet, "/api/v1/admin/users/"+dave+"/purge-preview", token, "", nil)
	var preview purgePreviewOutput
	if err := json.Unmarshal(raw, &preview.Body); code != http.StatusOK || err != nil {
		t.Fatalf("preview: %d %v", code, err)
	}
	// 009-AC9: the preview names no share.
	noShareKeys(t, raw)
	wantCounts := PurgeCounts{Watches: 1, Channels: 1, Rules: 1, OtherUsersRules: 1, Scripts: 1, Credentials: 1, InboxItems: 2, Sessions: 1}
	p := preview.Body
	if p.Counts != wantCounts || len(p.OtherRules) != 1 || p.OtherRules[0] != (OtherRule{ID: erinOnW, Name: "erin on dave's", Owner: "erin@x.test"}) {
		t.Fatalf("preview %+v", p)
	}

	var disabledAt time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT disabled_at FROM users WHERE id = $1`, dave).Scan(&disabledAt); err != nil {
		t.Fatal(err)
	}
	audits := e.count(t, `SELECT count(*) FROM audit_log`)
	if code, raw := e.purge(t, token, csrf, dave); code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", code, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log`); n != audits+1 {
		t.Fatalf("audit rows %d before, %d after: want only the purge row added", audits, n)
	}
	for _, c := range []struct {
		sql  string
		want int
	}{
		{`SELECT count(*) FROM watches WHERE owner_id = $1`, 0},
		{`SELECT count(*) FROM channels WHERE owner_id = $1`, 0},
		{`SELECT count(*) FROM rules WHERE owner_id = $1`, 0},
		{`SELECT count(*) FROM scripts WHERE owner_id = $1`, 0},
		{`SELECT count(*) FROM credentials WHERE owner_id = $1`, 0},
		{`SELECT count(*) FROM inbox_items WHERE user_id = $1`, 0},
		{`SELECT count(*) FROM sessions WHERE user_id = $1`, 0},
		{`SELECT count(*) FROM users WHERE id = $1 AND email = '' AND display_name = '' AND purged_at IS NOT NULL AND oidc_sub = 'dave-sub'`, 1},
	} {
		if n := e.count(t, c.sql, dave); n != c.want {
			t.Errorf("%s: %d, want %d", c.sql, n, c.want)
		}
	}
	var keptDisabled time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT disabled_at FROM users WHERE id = $1`, dave).Scan(&keptDisabled); err != nil || !keptDisabled.Equal(disabledAt) {
		t.Errorf("disabled_at %v, want %v kept (%v)", keptDisabled, disabledAt, err)
	}
	// Erin loses the rule on dave's watch and what it brought, and keeps everything of her own.
	if n := e.count(t, `SELECT count(*) FROM rules WHERE id = $1`, erinOnW); n != 0 {
		t.Error("erin's rule on the purged watch stayed")
	}
	if n := e.count(t, `SELECT count(*) FROM rules WHERE id = $1`, erinAll) + e.count(t, `SELECT count(*) FROM channels WHERE id = $1`, erinCh) +
		e.count(t, `SELECT count(*) FROM watches WHERE id = $1`, erinW) + e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1`, erin); n != 4 {
		t.Errorf("erin kept %d of her 4 rows", n)
	}

	var before map[string]any
	var after purgeAfter
	var actor string
	var afterRaw []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT actor_user_id::text, before, after, after::text FROM audit_log WHERE action = 'user.purge' AND target_id = $1`, dave).
		Scan(&actor, &before, &after, &afterRaw); err != nil {
		t.Fatal(err)
	}
	// 009-AC9: the purge's audit counts name no share.
	noShareKeys(t, afterRaw)
	if actor != admin || !equalJSON(t, before, map[string]string{"status": "disabled"}) || after.Status != "purged" {
		t.Fatalf("purge audit by %s: before %v after %+v", actor, before, after)
	}
	// What the dialog announced is what the purge deleted, type by type.
	got, want := reflect.ValueOf(after.Deleted), reflect.ValueOf(p.Counts)
	for i := range got.NumField() {
		if got.Field(i).Int() != want.Field(i).Int() {
			t.Errorf("deleted %s: %d, preview said %d", got.Type().Field(i).Name, got.Field(i).Int(), want.Field(i).Int())
		}
	}
	for _, u := range e.adminUsers(t, token) {
		if u.ID == dave && (u.Status != "purged" || u.Email != "" || u.DisplayName != "" || u.Counts != (AdminUserCounts{})) {
			t.Fatalf("listed %+v", u)
		}
	}
	if code, _ := e.purge(t, token, csrf, dave); code != http.StatusConflict {
		t.Fatalf("second purge: %d", code)
	}

	// 006-AC14: a purged account signing in again is refused; the upsert rolls back, so the email and
	// name the provider sent are not stored again, and no session opens.
	e.stub.sub = "dave-sub"
	if resp := e.fullLogin(t, "/"); resp.Header.Get("Location") != "/?auth=forbidden" || cookieNamed(resp, SecureSessionCookie) != nil {
		t.Fatalf("purged sign-in: %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := e.count(t, `SELECT count(*) FROM users WHERE id = $1 AND email = '' AND display_name = ''`, dave) +
		e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, dave); n != 1 {
		t.Fatalf("purged sign-in left personal data or a session (%d)", n)
	}
}

// A database failure in any transfer or purge operation is an opaque 500 that names no table, and the
// purge's tx leaves the account whole.
func TestAdminDataDatabaseFailure(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	withCred := e.watchOf(t, alice, "w", &cred)
	plain := e.watchOf(t, alice, "plain", nil)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, alice)
	for _, tt := range []struct {
		table, method, path string
		body                any
	}{
		{"credentials", http.MethodGet, "/api/v1/admin/users/" + alice + "/watches", nil},
		{"credentials", http.MethodGet, "/api/v1/admin/users/" + alice + "/purge-preview", nil},
		{"credentials", http.MethodPost, "/api/v1/admin/watches/" + withCred + "/transfer", map[string]string{"to_user_id": bob}},
		{"credentials", http.MethodPost, "/api/v1/admin/users/" + alice + "/purge", nil},
		{"audit_log", http.MethodPost, "/api/v1/admin/watches/" + plain + "/transfer", map[string]string{"to_user_id": bob}},
	} {
		e.exec(t, `ALTER TABLE `+tt.table+` RENAME TO gone`)
		code, raw := e.send(t, tt.method, tt.path, token, csrf, tt.body)
		e.exec(t, `ALTER TABLE gone RENAME TO `+tt.table)
		if code != http.StatusInternalServerError || strings.Contains(string(raw), "gone") {
			t.Errorf("%s %s without %s: %d %s", tt.method, tt.path, tt.table, code, raw)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE owner_id = $1`, alice) + e.count(t, `SELECT count(*) FROM users WHERE id = $1 AND purged_at IS NULL`, alice); n != 3 {
		t.Fatalf("failed calls changed alice's data (%d)", n)
	}
}

// The router holds an event of a purged account's watch FOR UPDATE, then takes KEY SHARE on another
// user's rule on it (the deliveries FK). The purge only locks those rules (NO KEY UPDATE) and deletes the
// events before the watches, so the router goes through and the purge then completes, whatever order the
// watches' cascades fire in. 014-AC12: the same with a rule on the purged watch and one of erin's, which the purge
// trims and keeps.
func TestPurgeRacingRouter(t *testing.T) {
	for _, flipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "events cascade first", true: "rule_watches cascade first"}[flipped], func(t *testing.T) {
			e := setup(t, false)
			if flipped {
				testpg.FlipEventsCascade(t, e.pool)
			}
			_, token, csrf := e.signedIn(t, "admin", true)
			dave, _, _ := e.signedIn(t, "dave", false)
			erin, _, _ := e.signedIn(t, "erin", false)
			w := e.watchOf(t, dave, "w", nil)
			r := e.ruleOf(t, erin, "erin on w", &w)
			ew := e.watchOf(t, erin, "erin's", nil)
			both := e.ruleOn(t, erin, "erin on both", w, ew)
			ev := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, w)
			e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
			ctx := context.Background()
			router := e.begin(t)
			if _, err := router.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, ev); err != nil {
				t.Fatal(err)
			}
			purge := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", token, csrf, nil)
			e.waitLockWaiters(t, 1)
			if _, err := router.Exec(ctx, `SELECT 1 FROM rules WHERE id = ANY($1::uuid[]) ORDER BY id FOR KEY SHARE`, []string{r, both}); err != nil {
				t.Fatalf("router's rule KEY SHARE: %v", err)
			}
			if err := router.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if code := <-purge; code != http.StatusNoContent {
				t.Fatalf("purge: %d, want 204", code)
			}
			if n := e.count(t, `SELECT count(*) FROM rules WHERE id = $1`, r); n != 0 {
				t.Error("erin's rule on the purged watch survived")
			}
			if n := e.count(t, `SELECT count(*) FROM rule_watches WHERE rule_id = $1 AND watch_id = $2`, both, ew) +
				e.count(t, `SELECT count(*) FROM rule_watches WHERE watch_id = $1`, w); n != 1 {
				t.Error("erin's rule on both watches did not keep hers alone")
			}
			if n := e.count(t, `SELECT (after -> 'deleted' ->> 'other_users_rules')::int FROM audit_log WHERE action = 'user.purge'`); n != 1 {
				t.Errorf("audit other_users_rules %d, want 1", n)
			}
		})
	}
}

// 006-AC13, 006-AC18: another user's rule created on the purged account's watch, committed while the purge waits
// on that watch, is deleted by the cascade and counted: the audit's other_users_rules matches what went.
func TestPurgeCountsRuleCreatedDuringWait(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	w := e.watchOf(t, dave, "w", nil)
	early := e.ruleOf(t, erin, "early", &w)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	ctx := context.Background()
	// createRule's statements: the cap lock on its owner, the watch's KEY SHARE, then the insert.
	create := e.begin(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, []any{erin}},
		{`SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, []any{w}},
	} {
		if _, err := create.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	late := testpg.InsertRule(t, create, testpg.Rule{Owner: erin, Name: "late", Watches: []string{w}})
	purge := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	if err := create.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-purge; code != http.StatusNoContent {
		t.Fatalf("purge: %d, want 204", code)
	}
	if n := e.count(t, `SELECT count(*) FROM rules WHERE id IN ($1, $2)`, early, late); n != 0 {
		t.Fatalf("%d of erin's rules on the purged watch survived", n)
	}
	if n := e.count(t, `SELECT (after -> 'deleted' ->> 'other_users_rules')::int FROM audit_log WHERE action = 'user.purge'`); n != 2 {
		t.Fatalf("audit other_users_rules %d, want 2 (both deleted)", n)
	}
}

// Another user's rule edit holds the rule, then takes the watch's KEY SHARE (rules API order). The purge locks
// other users' rules before its watches, so it waits on the edit instead of deadlocking with it.
func TestPurgeRacingRuleEdit(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	w := e.watchOf(t, dave, "w", nil)
	r := e.ruleOf(t, erin, "erin on w", &w)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	ctx := context.Background()
	edit := e.begin(t)
	if _, err := edit.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, r); err != nil {
		t.Fatal(err)
	}
	purge := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	if _, err := edit.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w); err != nil {
		t.Fatalf("rule edit's watch check: %v", err)
	}
	if err := edit.Commit(ctx); err != nil {
		t.Fatalf("rule edit commit: %v", err)
	}
	if code := <-purge; code != http.StatusNoContent {
		t.Fatalf("purge: %d, want 204", code)
	}
}

// Each account has an inbox item on the other's event: a purge deletes its own items, then its events
// with the other's items on them. dave's session, held by a test tx, parks dave's purge between the two;
// purges serialize, so erin's waits instead of taking the other half.
func TestCrossedPurgesInbox(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	evd := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, e.watchOf(t, dave, "dave's", nil))
	eve := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, e.watchOf(t, erin, "erin's", nil))
	e.exec(t, `INSERT INTO inbox_items (user_id, event_id) VALUES ($1, $2), ($3, $4)`, dave, eve, erin, evd)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id IN ($1, $2)`, dave, erin)
	ctx := context.Background()
	hold := e.begin(t)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM sessions WHERE user_id = $1 FOR UPDATE`, dave); err != nil {
		t.Fatal(err)
	}
	pd := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	pe := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+erin+"/purge", token, csrf, nil)
	e.waitLockWaiters(t, 2)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if d, er := <-pd, <-pe; d != http.StatusNoContent || er != http.StatusNoContent {
		t.Fatalf("purges %d and %d, want 204 both", d, er)
	}
}

// 006-AC13, 006-AC18, 009-AC9: one count per type, each a different number, so a count read or written into
// another type's field shows in the preview and in the audit's deleted counts. Dave's inbox items sit on
// erin's events (left by a transfer): his own items are counted, not the items on his watches.
func TestPurgeCountsPerType(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	other, _, _ := e.signedIn(t, "other-admin", true)
	w := e.watchOf(t, dave, "w", nil)
	erinW := e.watchOf(t, erin, "erin's", nil)
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO channels (owner_id, name, type) SELECT $1, 'c' || g, 'webhook' FROM generate_series(1, 2) g`, []any{dave}},
		{`INSERT INTO rules (owner_id, name, all_watches) SELECT $1, 'r' || g, true FROM generate_series(1, 3) g`, []any{dave}},
		{`INSERT INTO scripts (owner_id, name, source) SELECT $1, 's' || g, '' FROM generate_series(1, 5) g`, []any{dave}},
		{`INSERT INTO credentials (id, owner_id, name, kind, secret_ciphertext, secret_nonce, key_version)
			SELECT gen_random_uuid(), $1, 'k' || g, 'github_token', '\x00', '\x00', 1 FROM generate_series(1, 6) g`, []any{dave}},
		{`INSERT INTO events (watch_id, type, payload, dedup_key) SELECT $1, 't', '{}', 'k' || g FROM generate_series(1, 7) g`, []any{erinW}},
		{`INSERT INTO inbox_items (user_id, event_id) SELECT $1, id FROM events WHERE watch_id = $2`, []any{dave, erinW}},
		// signedIn opened the first of dave's 8 sessions.
		{`INSERT INTO sessions (id, user_id, csrf_token, expires_at) SELECT sha256(('d' || g)::bytea), $1, 'x', now() + interval '1 hour'
			FROM generate_series(2, 8) g`, []any{dave}},
		{`INSERT INTO sessions (id, user_id, csrf_token, expires_at, is_admin, acting_as) SELECT sha256(('a' || g)::bytea), $1, 'x',
			now() + interval '1 hour', true, $2 FROM generate_series(1, 9) g`, []any{other, dave}},
		{`INSERT INTO api_keys (user_id, name, hash, prefix, scope) SELECT $1, 'k' || g, sha256(('k' || g)::bytea), 'p', 'read'
			FROM generate_series(1, 10) g`, []any{dave}},
		{`UPDATE users SET disabled_at = now() WHERE id = $1`, []any{dave}},
	} {
		e.exec(t, s.sql, s.args...)
	}
	for range 4 {
		e.ruleOn(t, erin, "", w)
	}
	want := PurgeCounts{Watches: 1, Channels: 2, Rules: 3, OtherUsersRules: 4, Scripts: 5, Credentials: 6, InboxItems: 7,
		Sessions: 8, ActingSessions: 9, APIKeys: 10}

	code, raw := e.send(t, http.MethodGet, "/api/v1/admin/users/"+dave+"/purge-preview", token, "", nil)
	var preview purgePreviewOutput
	if err := json.Unmarshal(raw, &preview.Body); code != http.StatusOK || err != nil || preview.Body.Counts != want {
		t.Fatalf("preview %d %v: %+v, want %+v", code, err, preview.Body.Counts, want)
	}
	if code, raw := e.purge(t, token, csrf, dave); code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", code, raw)
	}
	var after purgeAfter
	if err := e.pool.QueryRow(context.Background(), `SELECT after FROM audit_log WHERE action = 'user.purge' AND target_id = $1`, dave).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after.Deleted != want {
		t.Fatalf("audit deleted %+v, want %+v", after.Deleted, want)
	}
}

// 014-AC9: the purge deletes and lists the other users' rules whose every watch was the account's; a rule keeping
// another watch, one of a third user's included, only loses the purged ones and stays.
func TestPurgeKeepsRulesWithAnotherWatch(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	frank, _, _ := e.signedIn(t, "frank", false)
	w1, w2 := e.watchOf(t, dave, "w1", nil), e.watchOf(t, dave, "w2", nil)
	ew, fw := e.watchOf(t, erin, "erin's", nil), e.watchOf(t, frank, "frank's", nil)
	gone := e.ruleOn(t, erin, "dave's only", w1, w2)
	kept := e.ruleOn(t, erin, "dave's and mine", w1, ew)
	moved := e.ruleOn(t, erin, "dave's and frank's", w2, fw)
	all := e.ruleOn(t, erin, "all")
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)

	code, raw := e.send(t, http.MethodGet, "/api/v1/admin/users/"+dave+"/purge-preview", token, "", nil)
	var preview purgePreviewOutput
	if err := json.Unmarshal(raw, &preview.Body); code != http.StatusOK || err != nil {
		t.Fatalf("preview: %d %v", code, err)
	}
	if p := preview.Body; p.Counts.OtherUsersRules != 1 || len(p.OtherRules) != 1 || p.OtherRules[0].ID != gone {
		t.Fatalf("preview: %+v", p)
	}
	if code, raw := e.purge(t, token, csrf, dave); code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", code, raw)
	}
	if n := e.count(t, `SELECT count(*) FROM rules WHERE id = $1`, gone); n != 0 {
		t.Error("the rule on dave's watches only survived")
	}
	for id, want := range map[string]string{kept: ew, moved: fw} {
		var got []string
		if err := e.pool.QueryRow(context.Background(), `SELECT array_agg(watch_id::text) FROM rules r JOIN rule_watches rw ON rw.rule_id = r.id
			WHERE r.id = $1 AND NOT r.all_watches`, id).Scan(&got); err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("rule %s after the purge: %v %v, want [%s]", id, got, err, want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM rules WHERE id = $1`, all); n != 1 {
		t.Error("erin's all-watches rule went")
	}
	if n := e.count(t, `SELECT (after -> 'deleted' ->> 'other_users_rules')::int FROM audit_log WHERE action = 'user.purge'`); n != 1 {
		t.Errorf("audit other_users_rules %d, want 1", n)
	}
}
