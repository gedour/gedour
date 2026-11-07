package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/testpg"
)

const testKey = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="

// send is call with a JSON body; it returns the status and the raw answer.
func (e *env) send(t *testing.T, method, path, token, csrf string, body any) (int, []byte) {
	t.Helper()
	var rd bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&rd).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.app.URL+path, &rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e.svc.sessionCookie(token, 60))
	req.Header.Set(CSRFHeader, csrf)
	resp, err := e.app.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func (e *env) id(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// sealedCredential stores a credential whose secret is really sealed (AAD = id), and returns its id and ciphertext.
func (e *env) sealedCredential(t *testing.T, owner, name, plain string) (string, []byte) {
	t.Helper()
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	id := e.id(t, `SELECT gen_random_uuid()::text`)
	ct, nonce := box.Seal([]byte(plain), []byte(id))
	e.exec(t, `INSERT INTO credentials (id, owner_id, name, kind, secret_ciphertext, secret_nonce, key_version) VALUES ($1, $2, $3, 'github_token', $4, $5, $6)`,
		id, owner, name, ct, nonce, secret.KeyVersion)
	return id, ct
}

func (e *env) watchOf(t *testing.T, owner, name string, cred *string) string {
	t.Helper()
	return e.id(t, `INSERT INTO watches (owner_id, name, kind, config, credential_id) VALUES ($1, $2, 'github_repo', '{}', $3) RETURNING id::text`,
		owner, name, cred)
}

func (e *env) ruleOf(t *testing.T, owner, name string, watch *string) string {
	t.Helper()
	return e.id(t, `INSERT INTO rules (owner_id, name, watch_id) VALUES ($1, $2, $3) RETURNING id::text`, owner, name, watch)
}

func (e *env) transfer(t *testing.T, token, csrf, watch, to string) (int, []byte) {
	t.Helper()
	return e.send(t, http.MethodPost, "/api/v1/admin/watches/"+watch+"/transfer", token, csrf, map[string]string{"to_user_id": to})
}

// 006-AC10: the watch, its credential (renamed on a clash) and nothing of the old owner's move; the old
// owner's rules on it and other users' rules stay. 006-AC18: audited with before and after.
// 009-AC9: the user's watches and the transfer result name no share.
func TestTransferWatch(t *testing.T) {
	e := setup(t, false)
	admin, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	carol, _, _ := e.signedIn(t, "carol", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	e.sealedCredential(t, bob, "gh", "x-secret-1")
	e.sealedCredential(t, bob, "gh (transferred)", "x-secret-2")
	w := e.watchOf(t, alice, "w", &cred)
	aliceOnW := e.ruleOf(t, alice, "on w", &w)
	ch := e.id(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'ch', 'webhook') RETURNING id::text`, alice)
	e.exec(t, `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, aliceOnW, ch)
	aliceAll := e.ruleOf(t, alice, "all", nil)
	carolOnW := e.ruleOf(t, carol, "carol on w", &w)
	// Created after w but listed first, by name; carol's own watch stays out of alice's list.
	plain := e.watchOf(t, alice, "a plain", nil)
	e.watchOf(t, carol, "carol's", nil)

	var listed struct{ Items []AdminWatch }
	_, raw := e.send(t, http.MethodGet, "/api/v1/admin/users/"+alice+"/watches", token, "", nil)
	if err := json.Unmarshal(raw, &listed); err != nil || len(listed.Items) != 2 {
		t.Fatalf("alice's watches: %v %+v", err, listed)
	}
	noShareKeys(t, raw)
	if got := listed.Items[0]; got.ID != plain || got.CredentialName != nil {
		t.Fatalf("listed first %+v", got)
	}
	if got := listed.Items[1]; got.ID != w || got.CredentialName == nil || *got.CredentialName != "gh" {
		t.Fatalf("listed second %+v", got)
	}

	code, raw := e.transfer(t, token, csrf, w, bob)
	if code != http.StatusOK {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	noShareKeys(t, raw)
	var res TransferResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	want := TransferResult{OwnerID: bob, CredentialID: &cred, CredentialMoved: true}
	if res.CredentialName == nil || *res.CredentialName != "gh (transferred 2)" {
		t.Fatalf("credential name %v", res.CredentialName)
	}
	want.CredentialName = res.CredentialName
	if !equalJSON(t, res, want) {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	for _, c := range []struct {
		sql  string
		want int
	}{
		{`SELECT count(*) FROM watches WHERE id = '` + w + `' AND owner_id = '` + bob + `' AND credential_id = '` + cred + `'`, 1},
		{`SELECT count(*) FROM credentials WHERE id = '` + cred + `' AND owner_id = '` + bob + `' AND name = 'gh (transferred 2)'`, 1},
		{`SELECT count(*) FROM rule_channels WHERE channel_id = '` + ch + `'`, 1},
		{`SELECT count(*) FROM rules WHERE id IN ('` + aliceOnW + `', '` + aliceAll + `', '` + carolOnW + `')`, 3},
	} {
		if n := e.count(t, c.sql); n != c.want {
			t.Errorf("%s: %d, want %d", c.sql, n, c.want)
		}
	}
	var before, after map[string]any
	var actor string
	if err := e.pool.QueryRow(context.Background(), `SELECT actor_user_id::text, before, after FROM audit_log WHERE action = 'watch.transfer' AND target_id = $1`, w).
		Scan(&actor, &before, &after); err != nil {
		t.Fatal(err)
	}
	if actor != admin || !equalJSON(t, before, map[string]any{"owner_id": alice, "credential_id": cred}) || !equalJSON(t, after, want) {
		t.Fatalf("audit by %s: before %v after %v", actor, before, after)
	}

	// Back to alice: the name stays (no clash there).
	if code, raw := e.transfer(t, token, csrf, w, alice); code != http.StatusOK || !strings.Contains(string(raw), `"credential_name":"gh (transferred 2)"`) {
		t.Fatalf("transfer back: %d %s", code, raw)
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var na, nb any
	_ = json.Unmarshal(ja, &na)
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(jb, &nb)
	x, _ := json.Marshal(na)
	y, _ := json.Marshal(nb)
	return bytes.Equal(x, y)
}

// A watch without a credential moves alone.
func TestTransferWithoutCredential(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	w := e.watchOf(t, alice, "w", nil)
	code, raw := e.transfer(t, token, csrf, w, bob)
	if code != http.StatusOK || !strings.Contains(string(raw), `"credential_moved":false`) || !strings.Contains(string(raw), `"credential_id":null`) {
		t.Fatalf("transfer: %d %s", code, raw)
	}
}

// 009-AC12: a watch rescued from a disabled account does not jump the queue with the due time it went stale on;
// a future one (a rate-limit hold) or a leased one stays.
func TestTransferKeepsQueueOrder(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	// expired: a crashed worker's lease, past its end, holds nothing.
	for name, due := range map[string]string{"stale": "2000-01-01", "held": "2999-01-01", "leased": "2000-01-01", "expired": "2000-01-01"} {
		w := e.watchOf(t, alice, name, nil)
		if _, err := e.pool.Exec(context.Background(), `UPDATE watches SET next_run_at = $2,
			leased_until = CASE name WHEN 'leased' THEN now() + interval '1 hour' WHEN 'expired' THEN now() - interval '1 hour' END WHERE id = $1`, w, due); err != nil {
			t.Fatal(err)
		}
		if code, raw := e.transfer(t, token, csrf, w, bob); code != http.StatusOK {
			t.Fatalf("transfer %s: %d %s", name, code, raw)
		}
	}
	var stale, held, leased int
	if err := e.pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE name IN ('stale', 'expired') AND next_run_at BETWEEN now() - interval '1 minute' AND now()),
		count(*) FILTER (WHERE name = 'held' AND next_run_at = '2999-01-01'),
		count(*) FILTER (WHERE name = 'leased' AND next_run_at = '2000-01-01')
		FROM watches WHERE owner_id = $1`, bob).Scan(&stale, &held, &leased); err != nil {
		t.Fatal(err)
	}
	if stale != 2 || held != 1 || leased != 1 {
		t.Fatalf("stale due times rescheduled: %d, future kept: %d, leased kept: %d; want 2, 1, 1", stale, held, leased)
	}
}

func TestFreeName(t *testing.T) {
	for _, tt := range []struct {
		taken []string
		want  string
	}{
		{nil, "gh"},
		{[]string{"other"}, "gh"},
		{[]string{"gh"}, "gh (transferred)"},
		{[]string{"gh", "gh (transferred)"}, "gh (transferred 2)"},
		{[]string{"gh", "gh (transferred)", "gh (transferred 2)"}, "gh (transferred 3)"},
		{[]string{"gh (transferred)"}, "gh"},
	} {
		if got := freeName("gh", tt.taken); got != tt.want {
			t.Errorf("freeName(%q): %q, want %q", tt.taken, got, tt.want)
		}
	}
}

// snapshot is every row a transfer could change, as text, to assert a refusal changed nothing.
func (e *env) snapshot(t *testing.T) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT concat_ws('|',
		(SELECT string_agg(id || owner_id::text || coalesce(credential_id::text, '') || updated_at::text, ',' ORDER BY id) FROM watches),
		(SELECT string_agg(id || owner_id::text || name || updated_at::text, ',' ORDER BY id) FROM credentials),
		(SELECT string_agg(id::text, ',' ORDER BY id) FROM rules),
		(SELECT count(*) FROM audit_log))`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// 006-AC11: a credential another watch of the old owner uses refuses the transfer, naming those watches,
// and changes nothing.
func TestTransferSharedCredentialRefused(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	w := e.watchOf(t, alice, "w", &cred)
	e.watchOf(t, alice, "Zed", &cred)
	e.watchOf(t, alice, "also", &cred)
	e.ruleOf(t, alice, "on w", &w)
	snap := e.snapshot(t)
	code, raw := e.transfer(t, token, csrf, w, bob)
	var p struct {
		Detail string
		Errors []struct{ Value string }
	}
	_ = json.Unmarshal(raw, &p)
	var names []string
	for _, x := range p.Errors {
		names = append(names, x.Value)
	}
	if code != http.StatusConflict || !slices.Equal(names, []string{"also", "Zed"}) || !strings.Contains(p.Detail, "also, Zed") {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	if got := e.snapshot(t); got != snap {
		t.Fatalf("refused transfer changed rows:\n%s\n%s", snap, got)
	}
}

// 006-AC10: refusals: the new owner disabled, purged or already the owner (409), the current owner
// purged (409), an unknown watch or account (404); none changes anything.
func TestTransferRefusals(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	disabled, _, _ := e.signedIn(t, "disabled", false)
	purged, _, _ := e.signedIn(t, "purged", false)
	ghost, _, _ := e.signedIn(t, "ghost", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id IN ($1, $2)`, disabled, purged)
	e.exec(t, `UPDATE users SET purged_at = now() WHERE id IN ($1, $2)`, purged, ghost)
	w := e.watchOf(t, alice, "w", nil)
	// A purged owner still holding a watch cannot happen through the API; the check guards it anyway.
	orphan := e.watchOf(t, ghost, "orphan", nil)
	const none = "00000000-0000-0000-0000-000000000000"
	snap := e.snapshot(t)
	for _, tt := range []struct {
		name, watch, to string
		want            int
	}{
		{"target disabled", w, disabled, http.StatusConflict},
		{"target purged", w, purged, http.StatusConflict},
		{"target already owner", w, alice, http.StatusConflict},
		{"target already owner, upper case", w, strings.ToUpper(alice), http.StatusConflict},
		{"source purged", orphan, bob, http.StatusConflict},
		{"unknown watch", none, bob, http.StatusNotFound},
		{"unknown user", w, none, http.StatusNotFound},
	} {
		if code, raw := e.transfer(t, token, csrf, tt.watch, tt.to); code != tt.want {
			t.Errorf("%s: %d %s, want %d", tt.name, code, raw, tt.want)
		}
	}
	if got := e.snapshot(t); got != snap {
		t.Fatalf("refused transfers changed rows:\n%s\n%s", snap, got)
	}
}

// Every spelling format:"uuid" accepts names the same account: the transfer goes through, answering its
// canonical form.
func TestTransferDestinationSpellings(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	w := e.watchOf(t, alice, "w", nil)
	owner := alice
	for _, spell := range []func(string) string{
		strings.ToUpper,
		func(id string) string { return "{" + id + "}" },
		func(id string) string { return strings.ReplaceAll(id, "-", "") },
		func(id string) string { return "urn:uuid:" + id },
	} {
		to := map[string]string{alice: bob, bob: alice}[owner]
		if code, raw := e.transfer(t, token, csrf, w, spell(to)); code != http.StatusOK || !strings.Contains(string(raw), `"owner_id":"`+to+`"`) {
			t.Fatalf("transfer to %s: %d %s, want 200 to %s", spell(to), code, raw, to)
		}
		if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND owner_id = $2`, w, to); n != 1 {
			t.Fatalf("the watch did not move to %s", to)
		}
		owner = to
	}
}

// An id huma's format check let through but uuid.Parse refuses is a 400, before anything else runs.
func TestTransferUnparsedDestination(t *testing.T) {
	in := &transferInput{}
	in.Body.ToUserID = "not-a-uuid"
	var se huma.StatusError
	if _, err := (&Service{}).transferWatch(context.Background(), in); !errors.As(err, &se) || se.GetStatus() != http.StatusBadRequest {
		t.Fatalf("unparsed destination: %v, want 400", err)
	}
}

// 006-AC18: neither the transfer nor the purge audit rows carry the secret, its ciphertext or the
// purged account's email and name.
func TestAdminAuditCarriesNoSecret(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	e.exec(t, `UPDATE users SET display_name = 'Alice Liddell' WHERE id = $1`, alice)
	const plain = "plain-secret-value-123"
	cred, ct := e.sealedCredential(t, alice, "gh", plain)
	w := e.watchOf(t, alice, "w", &cred)
	other := e.watchOf(t, alice, "other", nil)
	e.ruleOf(t, alice, "r", &other)
	if code, raw := e.transfer(t, token, csrf, w, bob); code != http.StatusOK {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, alice)
	if code, raw := e.send(t, http.MethodPost, "/api/v1/admin/users/"+alice+"/purge", token, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("purge: %d %s", code, raw)
	}
	var dump string
	if err := e.pool.QueryRow(context.Background(), `SELECT string_agg(coalesce(before::text, '') || coalesce(after::text, ''), ' ')
		FROM audit_log WHERE action IN ('watch.transfer', 'user.purge')`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dump, `"credential_moved": true`) || !strings.Contains(dump, `"credential_count": 0`) {
		t.Fatalf("audit rows missing or redacted: %s", dump)
	}
	for _, leak := range []string{plain, hex.EncodeToString(ct), base64.StdEncoding.EncodeToString(ct), "alice@x.test", "Alice Liddell", "[redacted]"} {
		if strings.Contains(dump, leak) {
			t.Errorf("audit holds %q: %s", leak, dump)
		}
	}
}

// sendRaw is send without t, for goroutines; a transport error answers 0.
func (e *env) sendRaw(method, path, token, csrf string, body any) (int, []byte) {
	var rd bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&rd).Encode(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.app.URL+path, &rd)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e.svc.sessionCookie(token, 60))
	req.Header.Set(CSRFHeader, csrf)
	resp, err := e.app.Client().Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

// sendAsync is sendRaw in a goroutine, answering the status.
func (e *env) sendAsync(method, path, token, csrf string, body any) <-chan int {
	out := make(chan int, 1)
	go func() {
		code, _ := e.sendRaw(method, path, token, csrf, body)
		out <- code
	}()
	return out
}

// lockWaiters closes once n backends of this database wait on a lock (polling stops after 10 s).
func (e *env) lockWaiters(n int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			var got int
			if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err == nil && got >= n {
				close(done)
				return
			}
		}
	}()
	return done
}

// waitLockWaiters blocks until n backends of this database wait on a lock.
func (e *env) waitLockWaiters(t *testing.T, n int) {
	t.Helper()
	testpg.WaitLockWaiters(t, e.pool, n)
}

// A transfer racing a purge of the watch's owner waits on the owner's row, never deadlocks: the purge
// holds the user while blocked on its channel, the transfer queues behind it, and once the purge ends
// the transfer finds the watch gone.
func TestTransferRacingPurge(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	dave, _, _ := e.signedIn(t, "dave", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	w := e.watchOf(t, dave, "w", nil)
	ch := e.id(t, `INSERT INTO channels (owner_id, name, type) VALUES ($1, 'ch', 'webhook') RETURNING id::text`, dave)
	e.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, dave)
	ctx := context.Background()
	blocker, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, ch); err != nil {
		t.Fatal(err)
	}
	purge := e.sendAsync(http.MethodPost, "/api/v1/admin/users/"+dave+"/purge", token, csrf, nil)
	e.waitLockWaiters(t, 1)
	transfer := e.sendAsync(http.MethodPost, "/api/v1/admin/watches/"+w+"/transfer", token, csrf, map[string]string{"to_user_id": bob})
	e.waitLockWaiters(t, 2)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// The transfer gets the users once the purge committed: the owner is purged, so 409.
	if p, tr := <-purge, <-transfer; p != http.StatusNoContent || tr != http.StatusConflict {
		t.Fatalf("purge %d, transfer %d: want 204 and 409", p, tr)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1`, w); n != 0 {
		t.Fatal("the purged account's watch survived")
	}
}

// A rename at the new owner, which takes no user lock, landing on the name the transfer picked is a 409
// to retry, not a 500.
func TestTransferNameRaceIsConflict(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	w := e.watchOf(t, alice, "w", &cred)
	other, _ := e.sealedCredential(t, bob, "other", "x-secret-1")
	ctx := context.Background()
	rename, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rename.Rollback(ctx) }()
	if _, err := rename.Exec(ctx, `UPDATE credentials SET name = 'gh' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	transfer := e.sendAsync(http.MethodPost, "/api/v1/admin/watches/"+w+"/transfer", token, csrf, map[string]string{"to_user_id": bob})
	// The transfer did not see the uncommitted rename, picked "gh", and waits on the unique index.
	e.waitLockWaiters(t, 1)
	if err := rename.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-transfer; code != http.StatusConflict {
		t.Fatalf("transfer: %d, want 409", code)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND owner_id = $2`, w, alice); n != 1 {
		t.Fatal("the refused transfer moved the watch")
	}
}

// begin opens a tx the test drives by hand, rolled back at the end unless committed.
func (e *env) begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// 006-AC11: an attach of the credential to another watch of the old owner, committed while the transfer
// waits on the credential, is seen: 409 naming that watch, nothing moved.
func TestTransferSeesAttachCommittedDuringWait(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	w1 := e.watchOf(t, alice, "w1", &cred)
	w2 := e.watchOf(t, alice, "late", nil)
	ctx := context.Background()
	// The watch update path: watch row lock, credential KEY SHARE, then the attach.
	attach := e.begin(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, []any{w2}},
		{`SELECT 1 FROM credentials WHERE id = $1 FOR KEY SHARE`, []any{cred}},
		{`UPDATE watches SET credential_id = $2 WHERE id = $1`, []any{w2, cred}},
	} {
		if _, err := attach.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	transfer := make(chan []byte, 1)
	go func() {
		_, raw := e.sendRaw(http.MethodPost, "/api/v1/admin/watches/"+w1+"/transfer", token, csrf, map[string]string{"to_user_id": bob})
		transfer <- raw
	}()
	e.waitLockWaiters(t, 1)
	if err := attach.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	raw := <-transfer
	var p struct {
		Status int
		Errors []struct{ Value string }
	}
	_ = json.Unmarshal(raw, &p)
	if p.Status != http.StatusConflict || len(p.Errors) != 1 || p.Errors[0].Value != "late" {
		t.Fatalf("transfer: %s", raw)
	}
	if n := e.count(t, `SELECT count(*) FROM credentials WHERE id = $1 AND owner_id = $2`, cred, alice) +
		e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND owner_id = $2`, w1, alice); n != 2 {
		t.Fatal("the refused transfer moved the watch or its credential")
	}
}

// A same-name credential the new owner creates while the transfer runs (its FK KEY SHARE is not blocked
// by the transfer's FOR SHARE) collides on the unique index: 409 to retry, not a 500.
func TestTransferNameCreateIsConflict(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	w := e.watchOf(t, alice, "w", &cred)
	ctx := context.Background()
	create := e.begin(t)
	if _, err := create.Exec(ctx, `INSERT INTO credentials (id, owner_id, name, kind, secret_ciphertext, secret_nonce, key_version)
		VALUES (gen_random_uuid(), $1, 'gh', 'github_token', '\x00', '\x00', 1)`, bob); err != nil {
		t.Fatal(err)
	}
	transfer := e.sendAsync(http.MethodPost, "/api/v1/admin/watches/"+w+"/transfer", token, csrf, map[string]string{"to_user_id": bob})
	e.waitLockWaiters(t, 1)
	if err := create.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-transfer; code != http.StatusConflict {
		t.Fatalf("transfer: %d, want 409", code)
	}
	if n := e.count(t, `SELECT count(*) FROM watches WHERE id = $1 AND owner_id = $2`, w, alice); n != 1 {
		t.Fatal("the refused transfer moved the watch")
	}
}

// A retry rebuilds its result: the credential detached since the failed attempt is not reported as moved.
func TestTransferRetryDropsStaleResult(t *testing.T) {
	e := setup(t, false)
	_, token, csrf := e.signedIn(t, "admin", true)
	alice, _, _ := e.signedIn(t, "alice", false)
	bob, _, _ := e.signedIn(t, "bob", false)
	cred, _ := e.sealedCredential(t, alice, "gh", "plain-secret-value-123")
	w := e.watchOf(t, alice, "w", &cred)
	retries := testpg.AllowDeadlockRetries(t)
	release := testpg.ForceDeadlockOnce(t, e.pool, "audit_log")
	transfer := e.sendAsync(http.MethodPost, "/api/v1/admin/watches/"+w+"/transfer", token, csrf, map[string]string{"to_user_id": bob})
	e.waitLockWaiters(t, 1)
	detached := make(chan error, 1)
	go func() {
		_, err := e.pool.Exec(context.Background(), `UPDATE watches SET credential_id = NULL WHERE id = $1`, w)
		detached <- err
	}()
	e.waitLockWaiters(t, 2)
	release()
	if err := <-detached; err != nil {
		t.Fatal(err)
	}
	if code := <-transfer; code != http.StatusOK {
		t.Fatalf("transfer: %d, want 200", code)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log WHERE action = 'watch.transfer' AND after ->> 'credential_moved' = 'false' AND after ->> 'credential_name' IS NULL`); n != 1 || retries() != 1 {
		t.Fatalf("%d clean transfer rows, %d retries: want 1 and 1", n, retries())
	}
	if n := e.count(t, `SELECT count(*) FROM credentials WHERE id = $1 AND owner_id = $2`, cred, alice); n != 1 {
		t.Fatal("the rolled-back attempt moved the credential")
	}
}
