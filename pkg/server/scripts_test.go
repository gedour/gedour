package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/auth"
)

type scriptJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int    `json:"size"`
	Source   string `json:"source"`
	Channels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channels"`
}

type dryRunJSON struct {
	OK         bool     `json:"ok"`
	Outcome    string   `json:"outcome"`
	Error      string   `json:"error"`
	Logs       []string `json:"logs"`
	Result     string   `json:"result"`
	DurationMS int64    `json:"duration_ms"`
	Fetches    []struct {
		Method   string            `json:"method"`
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		BodySize int               `json:"body_size"`
	} `json:"fetches"`
}

const hookScript = `function main(event, ctx) {
	log("got", event.type, event.payload.tag);
	const r = http.fetch(HOOK, {method: "POST", headers: {"Authorization": "Bearer s3cret"}, body: JSON.stringify({tag: event.payload.tag})});
	return {status: r.status, watch: ctx.watch.name};
}`

// 001-AC17, 001-AC21: scripts are checked on save, private to their owner, capped, audited by hash,
// and cannot go while a channel runs them; a script channel needs one of the owner's scripts.
func TestScripts(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")

	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": " ", "source": "function main() {\n  return 1 +\n}"}, http.StatusBadRequest)
	if !slices.Equal(p.locations(), []string{"body.name", "body.source"}) {
		t.Fatalf("invalid script: %+v", p)
	}
	if pos, _ := p.Errors[1].Value.(map[string]any); pos["line"] != 3.0 || pos["column"] != 1.0 || !strings.Contains(p.Errors[1].Message, "line 3") {
		t.Fatalf("syntax error position: %+v", p.Errors[1])
	}
	p = a.problemOf(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "big", "source": strings.Repeat("x", 65537)}, http.StatusBadRequest)
	if !slices.Contains(p.locations(), "body.source") {
		t.Fatalf("oversized: %+v", p)
	}

	src := `function main() { return "secret-marker" }`
	var s scriptJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "Notify", "source": src}, http.StatusCreated, &s)
	if s.Source != src || s.Size != len(src) || len(s.Channels) != 0 {
		t.Fatalf("created: %+v", s)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "notify", "source": src}, http.StatusCreated, nil)
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/scripts/"+s.ID, map[string]any{"name": "notify"}, http.StatusConflict)
	a.problemOf(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "Notify", "source": src}, http.StatusConflict)

	var list struct{ Items []scriptJSON }
	a.mustCall(t, alice, http.MethodGet, "/api/v1/scripts", nil, http.StatusOK, &list)
	if len(list.Items) != 2 || list.Items[0].Source != "" || list.Items[0].Size != len(src) {
		t.Fatalf("list: %+v", list)
	}
	// 001-AC5 style: someone else's script is a 404.
	a.problemOf(t, bob, http.MethodGet, "/api/v1/scripts/"+s.ID, nil, http.StatusNotFound)
	a.problemOf(t, bob, http.MethodPatch, "/api/v1/scripts/"+s.ID, map[string]any{"name": "x"}, http.StatusNotFound)
	a.problemOf(t, bob, http.MethodDelete, "/api/v1/scripts/"+s.ID, nil, http.StatusNotFound)
	a.problemOf(t, bob, http.MethodPost, "/api/v1/scripts/"+s.ID+"/dry-run", map[string]any{"sample": map[string]any{"type": "x"}}, http.StatusNotFound)
	a.problemOf(t, alice, http.MethodGet, "/api/v1/scripts/not-a-uuid", nil, http.StatusBadRequest)

	var upd scriptJSON
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/scripts/"+s.ID, map[string]any{"source": `function main() { return 2 }`}, http.StatusOK, &upd)
	if upd.Name != "Notify" || !strings.Contains(upd.Source, "return 2") {
		t.Fatalf("update: %+v", upd)
	}
	p = a.problemOf(t, alice, http.MethodPatch, "/api/v1/scripts/"+s.ID, map[string]any{"source": "function ("}, http.StatusBadRequest)
	if !slices.Equal(p.locations(), []string{"body.source"}) {
		t.Fatalf("update syntax: %+v", p)
	}

	// 001-AC21: audited, with a hash and length instead of the source.
	var auditText string
	if err := a.pool.QueryRow(t.Context(), `SELECT string_agg(action || coalesce(before::text, '') || coalesce(after::text, ''), ' ') FROM audit_log WHERE target_type = 'script'`).Scan(&auditText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditText, "secret-marker") || !strings.Contains(auditText, "source_sha256") || !strings.Contains(auditText, "script.updated") {
		t.Fatalf("audit: %s", auditText)
	}

	// Script channels: one of the owner's scripts, no secret.
	var bobScript scriptJSON
	a.mustCall(t, bob, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "Bob's", "source": src}, http.StatusCreated, &bobScript)
	for _, tt := range []struct {
		body map[string]any
		loc  string
	}{
		{map[string]any{"name": "c", "type": "script", "config": map[string]any{"script_id": bobScript.ID}}, "body.config.script_id"},
		{map[string]any{"name": "c", "type": "script", "config": map[string]any{"script_id": "nope"}}, "body.config.script_id"},
		{map[string]any{"name": "c", "type": "script", "config": map[string]any{"script_id": s.ID}, "secret": "abc"}, "body.secret"},
	} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", tt.body, http.StatusBadRequest)
		if !slices.Contains(p.locations(), tt.loc) {
			t.Errorf("%v: %+v", tt.body, p)
		}
	}
	var ch channelJSON
	// Any uuid spelling is stored canonical, so the in-use check below still finds the channel.
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Runner", "type": "script", "config": map[string]any{"script_id": "{" + strings.ToUpper(s.ID) + "}"}}, http.StatusCreated, &ch)
	if ch.Type != "script" || ch.Config["script_id"] != s.ID {
		t.Fatalf("script channel: %+v", ch)
	}
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/channels/"+ch.ID, map[string]any{"config": map[string]any{"script_id": bobScript.ID}}, http.StatusBadRequest)
	var got scriptJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/scripts/"+s.ID, nil, http.StatusOK, &got)
	if len(got.Channels) != 1 || got.Channels[0].Name != "Runner" {
		t.Fatalf("channels of the script: %+v", got)
	}
	p = a.problemOf(t, alice, http.MethodDelete, "/api/v1/scripts/"+s.ID, nil, http.StatusConflict)
	if len(p.Errors) != 1 || p.Errors[0].Value != "Runner" || !strings.Contains(p.Detail, "Runner") {
		t.Fatalf("delete in use: %+v", p)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/channels/"+ch.ID, nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/scripts/"+s.ID, nil, http.StatusNoContent, nil)
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'script.deleted' AND target_id = $1`, s.ID); n != 1 {
		t.Fatalf("delete audited %d times", n)
	}

	// The cap: 50 scripts per user.
	a.exec(t, `INSERT INTO scripts (owner_id, name, source) SELECT $1, 'bulk ' || i, '' FROM generate_series(1, 49) i`, alice.id)
	p = a.problemOf(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "one more", "source": src}, http.StatusConflict)
	if !strings.Contains(p.Detail, "50 scripts") {
		t.Fatalf("cap: %+v", p)
	}
}

// 001-AC18: a dry run runs in the sandbox on a sample or an event of the caller's watches, shows logs, result and the
// recorded fetches, and sends nothing: the receiver sees no request. Dry runs are rate limited and
// leave no audit row.
func TestScriptDryRun(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var hits atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(hook.Close)
	src := "const HOOK = \"" + hook.URL + "/x\";\n" + hookScript

	var dr dryRunJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": src,
		"sample": map[string]any{"type": "release.published", "payload": map[string]any{"tag": "v9"}, "watch": map[string]any{"name": "Demo", "kind": "github_repo"}}}, http.StatusOK, &dr)
	if !dr.OK || dr.Outcome != "ok" || dr.Result != `{"status":200,"watch":"Demo"}` || !slices.Equal(dr.Logs, []string{"got release.published v9"}) {
		t.Fatalf("dry run: %+v", dr)
	}
	if len(dr.Fetches) != 1 || dr.Fetches[0].Method != "POST" || dr.Fetches[0].URL != hook.URL+"/x" || dr.Fetches[0].Headers["Authorization"] != "[redacted]" || dr.Fetches[0].BodySize != len(`{"tag":"v9"}`) {
		t.Fatalf("fetches: %+v", dr.Fetches)
	}
	if hits.Load() != 0 {
		t.Fatalf("a dry run reached the receiver %d times", hits.Load())
	}

	// A saved script, on a real event of the caller's watch; someone else's event is refused.
	var s scriptJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "Hook", "source": src}, http.StatusCreated, &s)
	var wid string
	if err := a.pool.QueryRow(t.Context(), `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'cli/cli', 'github_repo', '{}') RETURNING id::text`, alice.id).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	var eid int64
	if err := a.pool.QueryRow(t.Context(), `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', '{"tag": "v2.105.0"}', 'k') RETURNING id`, wid).Scan(&eid); err != nil {
		t.Fatal(err)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts/"+s.ID+"/dry-run", map[string]any{"event_id": eid}, http.StatusOK, &dr)
	if !dr.OK || dr.Logs[0] != "got release.published v2.105.0" || dr.Result != `{"status":200,"watch":"cli/cli"}` {
		t.Fatalf("event dry run: %+v", dr)
	}
	p := a.problemOf(t, bob, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": src, "event_id": eid}, http.StatusBadRequest)
	if !slices.Contains(p.locations(), "body.event_id") {
		t.Fatalf("another user's event: %+v", p)
	}
	p = a.problemOf(t, bob, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": "function main( {", "sample": map[string]any{"type": "x"}}, http.StatusBadRequest)
	if !slices.Contains(p.locations(), "body.source") {
		t.Fatalf("syntax: %+v", p)
	}
	p = a.problemOf(t, bob, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": src}, http.StatusBadRequest)
	if !slices.Contains(p.locations(), "body") {
		t.Fatalf("no event: %+v", p)
	}
	// A throwing script is a failed run, not a failed request.
	a.mustCall(t, bob, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": "function main() { log('a'); throw new Error('nope') }", "sample": map[string]any{"type": "x"}}, http.StatusOK, &dr)
	if dr.OK || dr.Outcome != "error" || !strings.Contains(dr.Error, "nope") || dr.Logs[0] != "a" {
		t.Fatalf("throwing: %+v", dr)
	}
	if hits.Load() != 0 || a.countRows(t, `SELECT count(*) FROM audit_log WHERE action LIKE '%dry%'`) != 0 {
		t.Fatal("dry runs sent or audited something")
	}

	// 1 per second, burst 3: bob has spent 3 (one was refused before the limiter, two after).
	codes := map[int]int{}
	for range 4 {
		code, _ := a.call(t, bob, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": "function main() {}", "sample": map[string]any{"type": "x"}})
		codes[code]++
	}
	if codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("no 429 after a burst: %v", codes)
	}
}

// 001-AC17: testing a script channel is a live run: its fetch is sent, and the response carries the
// run's logs and error.
func TestScriptChannelTest(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var hits atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(hook.Close)
	var s scriptJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "Live",
		"source": `function main(e) { const r = http.fetch("` + hook.URL + `/t"); log("test", e.type, r.status); if (e.payload.message) throw new Error("after the fetch") }`}, http.StatusCreated, &s)
	var ch channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Live", "type": "script", "config": map[string]any{"script_id": s.ID}}, http.StatusCreated, &ch)
	var res struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		Logs  []string `json:"logs"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusOK, &res)
	if res.OK || !strings.Contains(res.Error, "after the fetch") || !slices.Equal(res.Logs, []string{"test gedour.test 200"}) || hits.Load() != 1 {
		t.Fatalf("test of a script channel: %+v, %d hits", res, hits.Load())
	}
}

// 001-AC18: interactive runs take their own 2 sandboxes: a third concurrent one is a 429 at once, and a
// client that goes away kills its child, freeing the slot before the 5 s timeout.
func TestScriptDryRunSlots(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	free := a.fillSandboxes(t, alice)
	time.Sleep(500 * time.Millisecond)
	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": "function main() {}", "sample": map[string]any{"type": "x"}}, http.StatusTooManyRequests)
	if !strings.Contains(p.Detail, "sandbox for interactive runs is busy") {
		t.Fatalf("full: %+v", p)
	}
	start := time.Now()
	free()
	var dr dryRunJSON
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, raw := a.call(t, bob, http.MethodPost, "/api/v1/scripts/dry-run", map[string]any{"source": "function main() { return 1 }", "sample": map[string]any{"type": "x"}})
		if code == http.StatusOK {
			_ = json.Unmarshal(raw, &dr)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slots still taken %s after the clients left: %d %s", time.Since(start), code, raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if dr.Result != "1" || time.Since(start) > 3*time.Second {
		t.Fatalf("after cancel: %+v in %s", dr, time.Since(start))
	}
}

// fillSandboxes takes both interactive sandboxes with looping dry runs of u; the returned func makes
// their clients leave, which kills the children.
func (a *devApp) fillSandboxes(t *testing.T, u apiUser) func() {
	t.Helper()
	loop := map[string]any{"source": "function main() { while (true) {} }", "sample": map[string]any{"type": "x"}}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			b, _ := json.Marshal(loop)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.srv.URL+"/api/v1/scripts/dry-run", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CSRF-Token", u.csrf)
			req.Header.Set("Cookie", auth.SessionCookie+"="+u.token)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		})
	}
	time.Sleep(500 * time.Millisecond)
	return func() { cancel(); wg.Wait() }
}

// 001-AC17: with every interactive sandbox busy, a script channel test is a 429 that writes no
// channel.tested row, so it does not start the 10 s interval: the next test runs as soon as one is free.
func TestScriptChannelTestBusy(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var s scriptJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/scripts", map[string]any{"name": "Quick", "source": `function main() { log("ran") }`}, http.StatusCreated, &s)
	var ch channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Quick", "type": "script", "config": map[string]any{"script_id": s.ID}}, http.StatusCreated, &ch)
	free := a.fillSandboxes(t, alice)
	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusTooManyRequests)
	if !strings.Contains(p.Detail, "busy") {
		t.Fatalf("busy test: %+v", p)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'channel.tested'`); n != 0 {
		t.Fatalf("a refused test wrote %d channel.tested rows", n)
	}
	free()
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, raw := a.call(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil)
		if code == http.StatusOK {
			if !strings.Contains(string(raw), `"ran"`) {
				t.Fatalf("test run: %s", raw)
			}
			break
		}
		if !strings.Contains(string(raw), "busy") || time.Now().After(deadline) {
			t.Fatalf("after the sandboxes freed: %d %s", code, raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'channel.tested'`); n != 1 {
		t.Fatalf("%d channel.tested rows", n)
	}
}
