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
// rules, scripts, credentials, shares, inbox and sessions go in one tx, other users' rules on its watches
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
	e.exec(t, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, w, erin)
	ch := e.id(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'ch', 'webhook') RETURNING id::text`, dave)
	daveRule := e.ruleOf(t, dave, "mine", &w)
	e.exec(t, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, daveRule, ch)
	erinCh := e.id(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'erin ch', 'webhook') RETURNING id::text`, erin)
	erinOnW := e.ruleOf(t, erin, "erin on dave's", &w)
	e.exec(t, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, erinOnW, erinCh)
	erinAll := e.ruleOf(t, erin, "erin all", nil)
	erinW := e.watchOf(t, erin, "erin's", nil)
	e.exec(t, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, erinW, dave)
	e.exec(t, `INSERT INTO scripts (owner_id, name, source) VALUES ($1, 's', '')`, dave)
	ev := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, erinW)
	daveEv := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, w)
	e.exec(t, `INSERT INTO inbox_items (user_id, event_id) VALUES ($1, $2), ($1, $4), ($3, $4), ($3, $2)`, dave, ev, erin, daveEv)
	e.exec(t, `INSERT INTO deliveries (event_id, rule_id, channel_id) VALUES ($1, $2, $3)`, daveEv, erinOnW, erinCh)
	// Set straight in the table, so dave's session row stays: the purge must not rely on the disable's revocation.
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)

	resp := e.call(t, http.MethodGet, "/api/v1/admin/users/"+dave+"/purge-preview", token, "")
	var preview purgePreviewOutput
	if err := json.NewDecoder(resp.Body).Decode(&preview.Body); err != nil {
		t.Fatal(err)
	}
	wantCounts := PurgeCounts{Watches: 1, Channels: 1, Rules: 1, OtherUsersRules: 1, Scripts: 1, Credentials: 1, SharesReceived: 1, InboxItems: 2, Sessions: 1}
	p := preview.Body
	if p.Counts != wantCounts || len(p.SharedWatches) != 1 || p.SharedWatches[0].Name != "dave's" || p.SharedWatches[0].ID != w || len(p.SharedWatches[0].SharedWith) != 1 ||
		p.SharedWatches[0].SharedWith[0] != "erin@x.test" || len(p.OtherRules) != 1 || p.OtherRules[0] != (OtherRule{ID: erinOnW, Name: "erin on dave's", Owner: "erin@x.test"}) {
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
		{`SELECT count(*) FROM watch_shares WHERE user_id = $1`, 0},
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
	if err := e.pool.QueryRow(context.Background(), `SELECT actor_user_id::text, before, after FROM audit_log WHERE action = 'user.purge' AND target_id = $1`, dave).
		Scan(&actor, &before, &after); err != nil {
		t.Fatal(err)
	}
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
		{"watch_shares", http.MethodPost, "/api/v1/admin/watches/" + plain + "/transfer", map[string]string{"to_user_id": bob}},
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
// watches' cascades fire in.
func TestPurgeRacingRouter(t *testing.T) {
	for _, flipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "events cascade first", true: "rules cascade first"}[flipped], func(t *testing.T) {
			e := setup(t, false)
			if flipped {
				testpg.FlipEventsCascade(t, e.pool)
			}
			_, token, csrf := e.signedIn(t, "admin", true)
			dave, _, _ := e.signedIn(t, "dave", false)
			erin, _, _ := e.signedIn(t, "erin", false)
			w := e.watchOf(t, dave, "w", nil)
			e.exec(t, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, w, erin)
			r := e.ruleOf(t, erin, "erin on w", &w)
			ev := e.id(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k') RETURNING id::text`, w)
			e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
			ctx := context.Background()
			router := e.begin(t)
			if _, err := router.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR UPDATE`, ev); err != nil {
				t.Fatal(err)
			}
			purge := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", token, csrf, nil)
			e.waitLockWaiters(t, 1)
			if _, err := router.Exec(ctx, `SELECT 1 FROM rules WHERE id = $1 FOR KEY SHARE`, r); err != nil {
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
			if n := e.count(t, `SELECT (after -> 'deleted' ->> 'other_users_rules')::int FROM audit_log WHERE action = 'user.purge'`); n != 1 {
				t.Errorf("audit other_users_rules %d, want 1", n)
			}
		})
	}
}

// 006-AC13, 006-AC18: a sharee's rule created on the purged account's watch, committed while the purge waits
// on that watch, is deleted by the cascade and counted: the audit's other_users_rules matches what went.
func TestPurgeCountsRuleCreatedDuringWait(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	w := e.watchOf(t, dave, "w", nil)
	e.exec(t, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, w, erin)
	early := e.ruleOf(t, erin, "early", &w)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	ctx := context.Background()
	// createRule's statements: the cap lock on its owner, the watch's KEY SHARE, then the insert.
	create := e.begin(t)
	var late string
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
	if err := create.QueryRow(ctx, `INSERT INTO rules (owner_id, name, watch_id) VALUES ($1, 'late', $2) RETURNING id::text`, erin, w).Scan(&late); err != nil {
		t.Fatal(err)
	}
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

// A sharee's rule edit holds the rule, then takes the watch's KEY SHARE (rules API order). The purge locks
// other users' rules before its watches, so it waits on the edit instead of deadlocking with it.
func TestPurgeRacingRuleEdit(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	erin, _, _ := e.signedIn(t, "erin", false)
	w := e.watchOf(t, dave, "w", nil)
	e.exec(t, `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, w, erin)
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
