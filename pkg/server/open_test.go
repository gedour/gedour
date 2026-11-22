package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/route"
)

// 021-AC6: /r and /r.js answer the same embedded bytes to every request, with or without a frontend build,
// never the SPA; nosniff and the CSP come with them.
func TestReadOnOpenPage(t *testing.T) {
	for name, ui := range map[string]fstest.MapFS{"no build": {}, "built": {"index.html": {Data: []byte("<html>SPA</html>")}}} {
		h := newHandler(t, pingOK, ui)
		for path, tt := range map[string]struct {
			ctype string
			body  []byte
		}{
			"/r":    {"text/html; charset=utf-8", openPage},
			"/r.js": {"text/javascript; charset=utf-8", openScript},
		} {
			var first []byte
			for _, target := range []string{path, path + "?event=1", path + "?a=b"} {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
				if target == path+"?a=b" {
					req.Header.Set("Cookie", auth.SessionCookie+"=x")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				hd := rec.Header()
				if rec.Code != http.StatusOK || hd.Get("Content-Type") != tt.ctype || hd.Get("X-Content-Type-Options") != "nosniff" ||
					!strings.Contains(hd.Get("Content-Security-Policy"), "script-src 'self'") || !bytes.Equal(rec.Body.Bytes(), tt.body) {
					t.Fatalf("%s %s: %d %v %q", name, target, rec.Code, hd, rec.Body.String())
				}
				if first == nil {
					first = rec.Body.Bytes()
				} else if !bytes.Equal(first, rec.Body.Bytes()) {
					t.Fatalf("%s %s: body differs", name, target)
				}
			}
		}
	}
	page := string(openPage)
	for _, banned := range []string{"<style", "style=", "data:", "<script>"} {
		if strings.Contains(page, banned) {
			t.Errorf("/r holds %q, which the CSP refuses", banned)
		}
	}
	if !strings.Contains(page, `<script src="/r.js" defer></script>`) || !strings.Contains(page, `<noscript>`) || !strings.Contains(page, `href="/inbox"`) {
		t.Errorf("/r: %s", page)
	}
	if !strings.Contains(string(openScript), "location.replace(") || strings.Contains(string(openScript), "innerHTML") {
		t.Errorf("/r.js: %s", openScript)
	}
}

// signLink computes a read-on-open link the way the spec states it, apart from pkg/notify's code.
func signLink(key []byte, id int64, owner string) string {
	msg := strconv.FormatInt(id, 10) + "." + owner
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// openApp is a dev app whose sender holds a key, with alice's watch, events and inbox items.
type openApp struct {
	*devApp
	key   []byte
	alice apiUser
	watch string
}

func newOpenApp(t *testing.T) *openApp {
	t.Helper()
	a := &openApp{devApp: newDevApp(t)}
	a.key, _ = notify.DeriveOpenKey(make([]byte, 32))
	a.sender.OpenKey = a.key
	a.alice = a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, a.alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.watch = w.ID
	return a
}

// event inserts an event of alice's watch with url (absent when empty), in her inbox when item is set.
func (a *openApp) event(t *testing.T, url string, item bool) int64 {
	t.Helper()
	payload := map[string]any{"tag": "v1"}
	if url != "" {
		payload["url"] = url
	}
	var id int64
	if err := a.pool.QueryRow(context.Background(), `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', $2, gen_random_uuid()::text) RETURNING id`,
		a.watch, payload).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if item {
		a.exec(t, `INSERT INTO inbox_items (user_id, event_id) VALUES ($1, $2)`, a.alice.id, id)
	}
	return id
}

// post sends body to POST /api/v1/open, signed in as u when given, else with no session.
func (a *openApp) post(t *testing.T, u *apiUser, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.srv.URL+"/api/v1/open", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if u != nil {
		req.Header.Set("Cookie", auth.SessionCookie+"="+u.token)
		req.Header.Set(auth.CSRFHeader, u.csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

func linkBody(link string) string {
	b, _ := json.Marshal(map[string]string{"link": link})
	return string(b)
}

// opened posts link with no session and wants 200 and url.
func (a *openApp) opened(t *testing.T, link, url string) {
	t.Helper()
	code, hd, raw := a.post(t, nil, linkBody(link))
	var out struct{ URL string }
	_ = json.Unmarshal(raw, &out)
	if code != http.StatusOK || out.URL != url || hd.Get("Cache-Control") != "no-store" {
		t.Fatalf("open %s: %d %v %s, want 200 %s no-store", link, code, hd, raw, url)
	}
}

// refused posts body with no session and wants the one 404 of an invalid link, with no URL.
func (a *openApp) refused(t *testing.T, body string) {
	t.Helper()
	code, _, raw := a.post(t, nil, body)
	var p struct {
		Detail string
		URL    *string `json:"url"`
	}
	_ = json.Unmarshal(raw, &p)
	if code != http.StatusNotFound || p.Detail != "this link is not valid" || p.URL != nil {
		t.Fatalf("%s: %d %s, want the invalid-link 404", body, code, raw)
	}
}

// state is alice's item of event as "unread", "archived unread" or its read_at.
func (a *openApp) state(t *testing.T, event int64) string {
	t.Helper()
	var s string
	if err := a.pool.QueryRow(context.Background(), `SELECT COALESCE(read_at::text, 'unread') || CASE WHEN archived_at IS NULL THEN '' ELSE ' archived' END
		FROM inbox_items WHERE user_id = $1 AND event_id = $2`, a.alice.id, event).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// 021-AC7: a valid link with no session marks the owner's unread item read, tells her open inbox stream the new
// count, and answers the stored URL, not cached.
func TestReadOnOpenMarksRead(t *testing.T) {
	a := newOpenApp(t)
	ev := a.event(t, "https://github.com/o/r/releases/v1", true)
	link := signLink(a.key, ev, a.alice.id)
	if _, _, ok := a.sender.VerifyOpen(link); !ok {
		t.Fatal("setup: signLink disagrees with the sender")
	}
	_, events := openStream(t, a.replica(t, time.Hour, false), a.alice)
	if e := next(t, events, "inbox", 2*time.Second); e.data != `{"unread":1}` {
		t.Fatalf("initial count: %+v", e)
	}
	a.opened(t, link, "https://github.com/o/r/releases/v1")
	if s := a.state(t, ev); s == "unread" || strings.Contains(s, "archived") {
		t.Fatalf("item %s, want read", s)
	}
	if e := next(t, events, "inbox", 2*time.Second); e.data != `{"unread":0}` {
		t.Fatalf("after open: %+v", e)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action LIKE '%open%'`); n != 0 {
		t.Fatalf("%d audit rows", n)
	}
}

// 021-AC8: an item already read keeps its first read_at; an archived unread one gets read_at and stays archived;
// a readable event with no inbox item answers its URL and changes nothing.
// Only a changed read mark notifies the owner's open tabs: an item already read or no item at all is silent.
func TestReadOnOpenItemStates(t *testing.T) {
	a := newOpenApp(t)
	notified := a.listen(t)
	read := a.event(t, "https://x.test/read", true)
	a.exec(t, `UPDATE inbox_items SET read_at = '2026-01-02 03:04:05+00' WHERE event_id = $1`, read)
	before := a.state(t, read)
	a.opened(t, signLink(a.key, read, a.alice.id), "https://x.test/read")
	if s := a.state(t, read); s != before {
		t.Errorf("read item: %s, was %s", s, before)
	}
	if got := notified(300 * time.Millisecond); got != "" {
		t.Errorf("already read: notified %q", got)
	}
	archived := a.event(t, "https://x.test/archived", true)
	a.exec(t, `UPDATE inbox_items SET archived_at = now() WHERE event_id = $1`, archived)
	a.opened(t, signLink(a.key, archived, a.alice.id), "https://x.test/archived")
	if s := a.state(t, archived); !strings.HasSuffix(s, " archived") || strings.HasPrefix(s, "unread") {
		t.Errorf("archived item: %s", s)
	}
	if got := notified(2 * time.Second); got != a.alice.id {
		t.Errorf("archived unread: notified %q, want the owner %s", got, a.alice.id)
	}
	none := a.event(t, "https://x.test/none", false)
	var items string
	snapshot := `SELECT COALESCE(json_agg(i ORDER BY i.id)::text, '') FROM inbox_items i`
	_ = a.pool.QueryRow(context.Background(), snapshot).Scan(&items)
	a.opened(t, signLink(a.key, none, a.alice.id), "https://x.test/none")
	var after string
	_ = a.pool.QueryRow(context.Background(), snapshot).Scan(&after)
	if after != items {
		t.Errorf("inbox changed: %s, was %s", after, items)
	}
	if got := notified(300 * time.Millisecond); got != "" {
		t.Errorf("no inbox item: notified %q", got)
	}
}

// listen LISTENs on the inbox channel and returns a wait for the next notification's payload, "" after d.
func (a *openApp) listen(t *testing.T) func(d time.Duration) string {
	t.Helper()
	conn, err := a.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(context.Background(), "LISTEN "+route.InboxChannel); err != nil {
		t.Fatal(err)
	}
	return func(d time.Duration) string {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return ""
		}
		return n.Payload
	}
}

// 021-AC9: a body without link, a non-string link or bad JSON is a 400, over 1 KiB a 413; every link that does
// not verify, or whose stored URL fails the signing check, is the same 404 and changes nothing.
func TestReadOnOpenRefusals(t *testing.T) {
	a := newOpenApp(t)
	ev := a.event(t, "https://x.test/ok", true)
	link := signLink(a.key, ev, a.alice.id)
	for _, body := range []string{`{}`, `{"link":1}`, `{"link":null}`, `{"link":`, `nope`, `[]`} {
		if code, _, raw := a.post(t, nil, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, code, raw)
		}
	}
	if code, _, _ := a.post(t, nil, linkBody(link+strings.Repeat("A", 1024))); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over 1 KiB: %d, want 413", code)
	}
	bob := a.signIn(t, "bob")
	tag := link[strings.LastIndexByte(link, '.')+1:]
	bad := []string{
		link[:len(link)-1] + "B", link[:len(link)-1],
		strconv.FormatInt(ev+1, 10) + link[len(strconv.FormatInt(ev, 10)):],
		strconv.FormatInt(ev, 10) + "." + bob.id + "." + tag,
		strings.ToUpper(link), "x", "", link + ".", link + strings.Repeat("A", 900),
		signLink(make([]byte, 32), ev, a.alice.id),
		signLink(a.key, ev+1000, a.alice.id),
	}
	for _, url := range []string{"", "ftp://x.test/a", "http://[::1", "https:///no-host", "javascript:alert(1)"} {
		bad = append(bad, signLink(a.key, a.event(t, url, true), a.alice.id))
	}
	for _, l := range bad {
		a.refused(t, linkBody(l))
	}
	if n := a.countRows(t, `SELECT count(*) FROM inbox_items WHERE read_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d items read by refused links", n)
	}
	// A service without a key verifies nothing.
	a.sender.OpenKey = nil
	a.refused(t, linkBody(link))
	a.sender.OpenKey = a.key
	a.opened(t, link, "https://x.test/ok")
}

// 021-AC10: the link refuses while the watch is someone else's or deleted, or the owner disabled, and works
// again once the watch is back or the owner re-enabled.
func TestReadOnOpenAccess(t *testing.T) {
	a := newOpenApp(t)
	bob := a.signIn(t, "bob")
	ev := a.event(t, "https://x.test/a", true)
	link := signLink(a.key, ev, a.alice.id)
	for _, tt := range []struct {
		name    string
		off, on func()
	}{
		{"transferred",
			func() { a.exec(t, `UPDATE watches SET owner_id = $2 WHERE id = $1`, a.watch, bob.id) },
			func() { a.exec(t, `UPDATE watches SET owner_id = $2 WHERE id = $1`, a.watch, a.alice.id) }},
		{"disabled",
			func() { a.exec(t, `UPDATE users SET disabled_at = now() WHERE id = $1`, a.alice.id) },
			func() { a.exec(t, `UPDATE users SET disabled_at = NULL WHERE id = $1`, a.alice.id) }},
	} {
		a.exec(t, `UPDATE inbox_items SET read_at = NULL`)
		tt.off()
		a.refused(t, linkBody(link))
		if s := a.state(t, ev); s != "unread" {
			t.Fatalf("%s: item %s", tt.name, s)
		}
		tt.on()
		a.opened(t, link, "https://x.test/a")
		if s := a.state(t, ev); s == "unread" {
			t.Fatalf("%s back: still unread", tt.name)
		}
	}
	a.exec(t, `DELETE FROM inbox_items WHERE event_id = $1`, ev)
	a.exec(t, `DELETE FROM events WHERE watch_id = $1`, a.watch)
	a.exec(t, `DELETE FROM watches WHERE id = $1`, a.watch)
	a.refused(t, linkBody(link))
}

// 021-AC11: another signed-in user posting the link marks the owner's item read, never their own.
func TestReadOnOpenOtherSession(t *testing.T) {
	a := newOpenApp(t)
	bob := a.signIn(t, "bob")
	ev := a.event(t, "https://x.test/a", true)
	a.exec(t, `INSERT INTO inbox_items (user_id, event_id) VALUES ($1, $2)`, bob.id, ev)
	code, _, raw := a.post(t, &bob, linkBody(signLink(a.key, ev, a.alice.id)))
	if code != http.StatusOK {
		t.Fatalf("as bob: %d %s", code, raw)
	}
	if s := a.state(t, ev); s == "unread" {
		t.Error("alice's item still unread")
	}
	if n := a.countRows(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND read_at IS NULL`, bob.id); n != 1 {
		t.Error("bob's item was marked read")
	}
}
