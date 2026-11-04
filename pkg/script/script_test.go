package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
)

func TestMain(m *testing.M) {
	MaybeSandbox()
	os.Exit(m.Run())
}

func sampleInput() Input {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	return InputOf(notify.Delivery{
		ID:    42,
		Event: notify.Event{Type: "release.published", OccurredAt: &at, Payload: map[string]any{"tag": "v1.2.3", "url": "https://github.com/cli/cli/releases/v1.2.3"}},
		Watch: notify.Watch{ID: "w1", Name: "cli/cli", Kind: "github_release"},
		Rule:  &notify.Rule{ID: "r1", Name: "Releases"},
	})
}

// inProcess runs Serve in a goroutine against a parent session, both in this process: the child's
// logic is measured by coverage here, the subprocess tests below cover the process boundary.
func inProcess(t *testing.T, r *Runner, src string, dry bool) (*message, Result) {
	t.Helper()
	toChild, childIn := io.Pipe()
	childOut, fromChild := io.Pipe()
	served := make(chan error, 1)
	go func() {
		err := Serve(toChild, fromChild)
		_ = fromChild.Close()
		served <- err
	}()
	s := &session{r: r, dry: dry, deadline: time.Now().Add(r.o.Timeout), enc: json.NewEncoder(childIn)}
	final, protocol := s.serve(t.Context(), src, sampleInput(), childOut)
	_ = childIn.Close()
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if protocol != "" {
		t.Fatalf("protocol: %s", protocol)
	}
	return final, s.res
}

func testGuard(allow ...string) *netguard.Guard {
	g, _ := netguard.New(allow)
	return g
}

func testRunner(t *testing.T, o Options) *Runner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if o.Exe == "" {
		o.Exe, o.Args = exe, []string{Subcommand}
	}
	if o.Guard == nil {
		o.Guard = testGuard()
	}
	if o.Client == nil {
		o.Client = o.Guard.Client(netguard.Options{Timeout: fetchTimeout, HTTPHosts: o.HTTPHosts, NoRedirects: true})
	}
	return NewRunner(o)
}

// 001-AC17: main gets the event and a context, logs and returns a JSON result; the script has no
// require, process, filesystem, environment or timers.
func TestServe(t *testing.T) {
	r := testRunner(t, Options{})
	for _, tt := range []struct {
		name, src, outcome, errPart, result string
		logs                                []string
	}{
		{name: "result and logs", outcome: OutcomeOK,
			src: `function main(e, ctx) {
				log("tag", e.payload.tag, {n: 1}, [1, 2], null, undefined, new Error("x"));
				log(event.summary, watch.name, rule.name, delivery_id, ctx.watch.kind, ctx.delivery_id);
				return {ok: true, url: e.url};
			}`,
			logs:   []string{`tag v1.2.3 {"n":1} [1,2] null undefined Error: x`, "v1.2.3 published cli/cli Releases 42 github_release 42"},
			result: `{"ok":true,"url":"https://github.com/cli/cli/releases/v1.2.3"}`},
		{name: "no capability", outcome: OutcomeOK,
			src: `function main() {
				return [typeof require, typeof process, typeof os, typeof fs, typeof module, typeof exports,
					typeof setTimeout, typeof setInterval, typeof console, typeof globalThis.Deno, typeof fetch, typeof XMLHttpRequest].join(",");
			}`,
			result: `"undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined"`},
		{name: "no result", outcome: OutcomeOK, src: `function main() {}`},
		{name: "async main", outcome: OutcomeOK, src: `async function main() { return 7 }`, result: "7"},
		{name: "rejected", outcome: OutcomeError, src: `async function main() { throw new Error("nope") }`, errPart: "main rejected: Error: nope"},
		{name: "never settles", outcome: OutcomeError, src: `function main() { return new Promise(() => {}) }`, errPart: "never settled"},
		{name: "throws", outcome: OutcomeError, src: "function main() {\n  throw new Error('boom');\n}", errPart: "Error: boom"},
		{name: "top-level throw", outcome: OutcomeError, src: "throw new TypeError('early')", errPart: "TypeError: early"},
		{name: "missing main", outcome: OutcomeError, src: `var x = 1`, errPart: "must define function main"},
		{name: "syntax", outcome: OutcomeError, src: `function main( {`, errPart: "Unexpected"},
		{name: "unserializable", outcome: OutcomeError, src: `function main() { return 1n }`, errPart: "result"},
		{name: "deep recursion", outcome: OutcomeError, src: `function f() { return f() } function main() { f() }`, errPart: "maximum call stack size exceeded"},
		{name: "result clipped", outcome: OutcomeOK, src: `function main() { return "é".repeat(20000) }`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			final, res := inProcess(t, r, tt.src, false)
			if final.Outcome != tt.outcome || !strings.Contains(final.Error, tt.errPart) {
				t.Fatalf("outcome %q error %q, want %q containing %q", final.Outcome, final.Error, tt.outcome, tt.errPart)
			}
			if tt.name == "result clipped" {
				if len(final.Result) > maxResult || !strings.HasSuffix(final.Result, "…") || !json.Valid([]byte(`"`+strings.TrimSuffix(final.Result[1:], "…")+`"`)) {
					t.Fatalf("result not clipped on a rune: %d bytes", len(final.Result))
				}
				return
			}
			if final.Result != tt.result {
				t.Fatalf("result %s, want %s", final.Result, tt.result)
			}
			if tt.logs != nil && strings.Join(res.Logs, "\n") != strings.Join(tt.logs, "\n") {
				t.Fatalf("logs %q, want %q", res.Logs, tt.logs)
			}
		})
	}
}

// 001-AC17: logs stop at 200 lines and 16 KiB, with one notice.
func TestLogCaps(t *testing.T) {
	r := testRunner(t, Options{})
	for _, tt := range []struct{ src string }{
		{`function main() { for (let i = 0; i < 1000; i++) log(i) }`},
		{`function main() { for (let i = 0; i < 1000; i++) log("x".repeat(1000)) }`},
		{`function main() { log("y".repeat(100000)) ; log("after") }`},
	} {
		final, res := inProcess(t, r, tt.src, false)
		total := 0
		for _, l := range res.Logs {
			total += len(l)
		}
		if final.Outcome != OutcomeOK || len(res.Logs) > maxLogLines+1 || total > maxLogBytes+64 ||
			!strings.HasPrefix(res.Logs[len(res.Logs)-1], "[log truncated") && len(res.Logs) != 2 {
			t.Fatalf("%s: %d lines, %d bytes, last %q", tt.src, len(res.Logs), total, res.Logs[len(res.Logs)-1])
		}
	}
}

type upstream struct {
	*httptest.Server
	hits atomic.Int64
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/echo", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponse+1)))
		case "/slow":
			time.Sleep(3 * time.Second)
		default:
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Seen", r.Method+" "+r.Header.Get("X-Test")+" "+r.Header.Get("User-Agent"))
			_, _ = fmt.Fprintf(w, "echo %s", b)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// 001-AC17: http.fetch goes through the parent and the SSRF guard: loopback, private and metadata
// targets are refused, redirects come back unfollowed, calls, bodies and responses are capped.
func TestFetch(t *testing.T) {
	up := newUpstream(t)
	open := testRunner(t, Options{Guard: testGuard("127.0.0.0/8"), HTTPHosts: []string{"127.0.0.1"}, UserAgent: "gedour/test"})
	closed := testRunner(t, Options{HTTPHosts: []string{"127.0.0.1", "localhost", "169.254.169.254", "10.0.0.1"}})
	call := func(url, opts string) string {
		return fmt.Sprintf(`function main() { try { const r = http.fetch(%q, %s); return [r.status, r.ok, r.headers["x-seen"] || "", r.text].join("|") } catch (e) { return String(e) } }`, url, opts)
	}
	for _, tt := range []struct {
		name   string
		r      *Runner
		src    string
		result string
	}{
		{"post", open, call(up.URL+"/echo", `{method: "post", headers: {"X-Test": "1"}, body: "hi"}`), "POST 1 gedour/test|echo hi"},
		{"get default", open, call(up.URL+"/echo", "undefined"), "200|true|GET  gedour/test|echo "},
		{"redirect not followed", open, call(up.URL+"/redirect", "{}"), "302|false||"},
		{"response cap", open, call(up.URL+"/big", "{}"), "response over 1 MiB"},
		{"loopback literal", closed, call(up.URL+"/echo", "{}"), "127.0.0.1 is a private"},
		{"loopback by name", closed, call(strings.Replace(up.URL, "127.0.0.1", "localhost", 1)+"/echo", "{}"), "destination not allowed"},
		{"metadata", closed, call("http://169.254.169.254/latest/meta-data/", "{}"), "169.254.169.254 is a private"},
		{"private", closed, call("http://10.0.0.1/", "{}"), "10.0.0.1 is a private"},
		{"plain http", closed, call("http://example.com/", "{}"), "needs https"},
		{"not a url", open, call("/relative", "{}"), "absolute http(s) URL"},
		{"method", open, call(up.URL, `{method: "CONNECT"}`), "method \"CONNECT\""},
		{"host header", open, call(up.URL, `{headers: {host: "evil"}}`), "header \"host\" is set by gedour"},
		{"hop header", open, call(up.URL, `{headers: {"Transfer-Encoding": "chunked"}}`), "header \"Transfer-Encoding\""},
		{"body cap", open, call(up.URL, `{method: "POST", body: "x".repeat(300000)}`), "body over 256 KiB"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			final, _ := inProcess(t, tt.r, tt.src, false)
			var got string
			_ = json.Unmarshal([]byte(final.Result), &got)
			if final.Outcome != OutcomeOK || !strings.Contains(got, tt.result) {
				t.Fatalf("%s: %+v", tt.name, final)
			}
		})
	}
	before := up.hits.Load()
	final, res := inProcess(t, open, `function main() { let n = 0; try { for (;;) { http.fetch("`+up.URL+`/echo"); n++ } } catch (e) { return n + " " + e } }`, false)
	if !strings.HasPrefix(final.Result, `"10 Error: http.fetch: at most 10`) || up.hits.Load()-before != 10 || len(res.Fetches) != 10 {
		t.Fatalf("fetch cap: %s, %d hits, %d recorded", final.Result, up.hits.Load()-before, len(res.Fetches))
	}
}

// 001-AC18: a dry run records each fetch, secrets redacted, and the upstream sees nothing.
func TestDryRunFetch(t *testing.T) {
	up := newUpstream(t)
	r := testRunner(t, Options{Guard: testGuard("127.0.0.0/8"), HTTPHosts: []string{"127.0.0.1"}})
	final, res := inProcess(t, r, `function main(e) {
		const a = http.fetch("`+up.URL+`/hook", {method: "POST", headers: {"Authorization": "Bearer s3cret", "X-Api-Key": "k", "Content-Type": "application/json"}, body: JSON.stringify(e)});
		http.fetch("`+strings.Replace(up.URL, "http://", "http://bot:pa55@", 1)+`/other?token=t0k&page=2&api%5Fkey=k3y");
		http.fetch("`+strings.Replace(up.URL, "http://", "http://ghp_x1@", 1)+`/cb?sig=s1g&id=7#access_token=at0k&state=ok");
		return [a.status, a.text, Object.keys(a.headers).length];
	}`, true)
	if final.Outcome != OutcomeOK || final.Result != `[200,"",0]` {
		t.Fatalf("dry run: %+v", final)
	}
	if up.hits.Load() != 0 {
		t.Fatalf("dry run reached the upstream %d times", up.hits.Load())
	}
	b, _ := json.Marshal(res.Fetches)
	if len(res.Fetches) != 3 || res.Fetches[0].Method != "POST" || res.Fetches[0].BodySize < 50 || strings.Contains(string(b), "s3cret") ||
		res.Fetches[0].Headers["Authorization"] != "[redacted]" || res.Fetches[0].Headers["X-Api-Key"] != "[redacted]" ||
		res.Fetches[0].Headers["Content-Type"] != "application/json" || res.Fetches[1].Method != "GET" ||
		res.Fetches[1].URL != strings.Replace(up.URL, "http://", "http://bot:[redacted]@", 1)+"/other?token=[redacted]&page=2&api%5Fkey=[redacted]" ||
		res.Fetches[2].URL != strings.Replace(up.URL, "http://", "http://[redacted]@", 1)+"/cb?sig=[redacted]&id=7#access_token=[redacted]&state=ok" {
		t.Fatalf("recorded: %s", b)
	}
	for _, leak := range []string{"pa55", "t0k", "k3y", "ghp_x1", "s1g", "at0k"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("recorded fetches leak %q: %s", leak, b)
		}
	}
	// The guard still answers in a dry run: what would be refused for real is refused here too.
	final, _ = inProcess(t, testRunner(t, Options{HTTPHosts: []string{"10.9.1.1"}}), `function main() { try { http.fetch("http://10.9.1.1/") } catch (e) { return String(e) } }`, true)
	if !strings.Contains(final.Result, "private") {
		t.Fatalf("dry run guard: %+v", final)
	}
}

// redos backtracks for ages in regexp2 without checking goja's interrupt.
const redos = `function main() { return /^(a+)+$/.test("a".repeat(40) + "!") }`

func runMessage(t *testing.T, src string, timeout time.Duration) []byte {
	t.Helper()
	b, err := json.Marshal(message{T: "run", Source: src, Input: &Input{Event: Event{Type: "x"}}, TimeoutMS: timeout.Milliseconds()})
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// The exit nets themselves, in-process with a recorded exit and the regexp timeout out of the way
// (it is the child's last net, set by Main only): EOF and the deadline each end a stuck run.
func TestServeExitNets(t *testing.T) {
	saved := regexp2.DefaultMatchTimeout
	regexp2.DefaultMatchTimeout = 5 * time.Second
	t.Cleanup(func() { regexp2.DefaultMatchTimeout = saved })
	for _, tt := range []struct {
		name  string
		close bool
	}{{"eof", true}, {"deadline", false}} {
		t.Run(tt.name, func(t *testing.T) {
			toChild, childIn := io.Pipe()
			codes := make(chan int, 2)
			h := hooks{exit: func(c int) { codes <- c }, selfGrace: 100 * time.Millisecond, exitGrace: 100 * time.Millisecond}
			go func() { _ = serve(toChild, io.Discard, h) }()
			timeout := 100 * time.Millisecond
			if tt.close {
				timeout = 10 * time.Second
			}
			if _, err := childIn.Write(runMessage(t, redos, timeout)); err != nil {
				t.Fatal(err)
			}
			if tt.close {
				_ = childIn.Close()
			}
			select {
			case c := <-codes:
				if c != exitStuck {
					t.Fatalf("exit %d", c)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no exit")
			}
		})
	}
}

// 001-AC17: interactive runs (dry runs, tests) have their own slots, answer ErrBusy when they are
// taken instead of queueing, leave deliveries alone, and die with their caller's context.
func TestSandboxInteractive(t *testing.T) {
	r := testRunner(t, Options{Timeout: 3 * time.Second, Interactive: 1, Concurrency: 1})
	started := make(chan int, 4)
	r.onStart = func(pid int) { started <- pid }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan Result, 1)
	go func() {
		res, _ := r.Interactive(ctx, `function main() { while (true) {} }`, sampleInput(), true)
		done <- res
	}()
	<-started
	if _, err := r.Interactive(t.Context(), `function main() {}`, sampleInput(), true); !errors.Is(err, ErrBusy) {
		t.Fatalf("second interactive run: %v", err)
	}
	if res := r.Run(t.Context(), `function main() { return 1 }`, sampleInput()); res.Outcome != OutcomeOK {
		t.Fatalf("delivery beside a busy interactive slot: %+v", res)
	}
	start := time.Now()
	cancel()
	res := <-done
	if res.Outcome != OutcomeKilled || !strings.Contains(res.Error, "cancelled") || time.Since(start) > time.Second {
		t.Fatalf("cancelled interactive run: %+v after %s", res, time.Since(start))
	}
	if res, err := r.Interactive(t.Context(), `function main() { return 2 }`, sampleInput(), false); err != nil || res.Result != "2" {
		t.Fatalf("slot not freed: %+v %v", res, err)
	}
}

func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// 001-AC17: an infinite loop stops at the timeout; memory bombs kill the child, never the parent.
func TestSandboxLimits(t *testing.T) {
	loops := testRunner(t, Options{Timeout: time.Second})
	// Bombs get time to blow up even under the race detector, which runs JS about 10 times slower.
	bombs := testRunner(t, Options{Timeout: 4 * time.Second})
	deflt := testRunner(t, Options{})
	before := heapInUse()
	for _, tt := range []struct {
		name, src string
		r         *Runner
		outcome   string
		max       time.Duration
	}{
		{"infinite loop", `function main() { while (true) {} }`, loops, OutcomeTimeout, time.Second + killGrace},
		{"loop at top level", `for (;;) {}`, loops, OutcomeTimeout, time.Second + killGrace},
		{"loop catching", `function main() { for (;;) { try { while (true) {} } catch (e) {} } }`, loops, OutcomeTimeout, time.Second + killGrace},
		// Garbage keeps the GC busy on every thread: the CPU limit must not beat the 5 s wall clock.
		{"allocating loop", `function main() { let keep = []; for (let i = 0; ; i++) { keep.push({i, a: [i, i], s: "x" + i}); if (keep.length > 100000) keep = [] } }`, deflt, OutcomeTimeout, Timeout + killGrace},
		{"array of strings", `function main() { let a = []; for (;;) a.push("x".repeat(1e7) + a.length) }`, bombs, OutcomeKilled, 4 * time.Second},
		{"one huge string", `function main() { return "x".repeat(2 ** 30).length }`, bombs, OutcomeKilled, 4 * time.Second},
		{"huge buffer", `function main() { return new ArrayBuffer(2 ** 31).byteLength }`, bombs, OutcomeKilled, 4 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := tt.r.Run(t.Context(), tt.src, sampleInput())
			t.Logf("%s: %s after %s: %s", tt.name, res.Outcome, res.Duration, res.Error)
			// Under the race detector (no RLIMIT_AS, a GC slowed down near the soft limit) one allocation
			// can come and go between two watchdog polls, and the array bomb crawls into the timeout:
			// the run still ends, the memory goes with the child.
			if raceEnabled && (tt.name == "huge buffer" && res.Outcome == OutcomeOK || tt.name == "array of strings" && res.Outcome == OutcomeTimeout) {
				return
			}
			if res.Outcome != tt.outcome {
				t.Fatalf("outcome %s (%s), want %s", res.Outcome, res.Error, tt.outcome)
			}
			if res.Duration > tt.max+time.Second {
				t.Fatalf("took %s", res.Duration)
			}
		})
	}
	after := heapInUse()
	t.Logf("parent heap in use: %d KiB before, %d KiB after", before>>10, after>>10)
	if after > before+16<<20 {
		t.Fatalf("parent heap grew from %d to %d bytes", before, after)
	}
}

// 001-AC17: a child that ignores the interrupt is killed; one that breaks the protocol, floods or crashes ends
// the run as killed with the reason.
func TestSandboxMisbehaving(t *testing.T) {
	for _, tt := range []struct {
		name, script, outcome, errPart string
	}{
		{"ignores interrupt", "exec sleep 30", OutcomeTimeout, "stopped after"},
		{"malformed", "echo notjson", OutcomeKilled, "malformed message"},
		{"unknown message", `echo '{"t":"shell"}'`, OutcomeKilled, `unknown message "shell"`},
		{"bad outcome", `echo '{"t":"done","outcome":"great"}'`, OutcomeKilled, "without an answer"},
		{"crash", "exit 7", OutcomeKilled, "exit status 7"},
		{"line flood", "head -c 3000000 /dev/zero", OutcomeKilled, "a message over 2 MiB"},
		{"output flood", `x=$(head -c 1000000 /dev/zero | tr '\0' x); while :; do printf '{"t":"log","line":"%s"}\n' "$x"; done`, OutcomeKilled, "output over 8 MiB"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sh, err := exec.LookPath("sh")
			if err != nil {
				t.Skip("no sh")
			}
			// 2 s: under the race detector the parent decodes the flood slowly.
			r := testRunner(t, Options{Exe: sh, Args: []string{"-c", "PATH=/usr/bin:/bin; " + tt.script}, Timeout: 2 * time.Second})
			start := time.Now()
			res := r.Run(t.Context(), "function main() {}", sampleInput())
			if res.Outcome != tt.outcome || !strings.Contains(res.Error, tt.errPart) {
				t.Fatalf("%+v", res)
			}
			if time.Since(start) > 4*time.Second {
				t.Fatalf("took %s", time.Since(start))
			}
			if len(res.Logs) > maxLogLines+1 {
				t.Fatalf("%d log lines kept", len(res.Logs))
			}
		})
	}
	r := testRunner(t, Options{Exe: "/nonexistent/gedour"})
	if res := r.Run(t.Context(), "", sampleInput()); res.Outcome != OutcomeKilled || !strings.Contains(res.Error, "did not start") {
		t.Fatalf("missing binary: %+v", res)
	}
	// Every slot taken: a cancelled caller gives up waiting.
	full := testRunner(t, Options{Concurrency: 1})
	full.sem <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res := full.Run(ctx, "", sampleInput()); res.Outcome != OutcomeKilled || !strings.Contains(res.Error, "cancelled before start") {
		t.Fatalf("cancelled: %+v", res)
	}
}

// 001-AC17: a run cancelled by its caller (shutdown) is killed at once.
func TestSandboxCancel(t *testing.T) {
	r := testRunner(t, Options{Timeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	res := r.Run(ctx, `function main() { while (true) {} }`, sampleInput())
	if res.Outcome != OutcomeKilled || !strings.Contains(res.Error, "cancelled") || res.Duration > 3*time.Second {
		t.Fatalf("%+v", res)
	}
}

// 001-AC17: a fetch still waiting at the deadline does not hold the run past it.
func TestSandboxFetchTimeout(t *testing.T) {
	up := newUpstream(t)
	r := testRunner(t, Options{Timeout: time.Second, Guard: testGuard("127.0.0.0/8"), HTTPHosts: []string{"127.0.0.1"}})
	res := r.Run(t.Context(), `function main() { http.fetch("`+up.URL+`/slow") }`, sampleInput())
	if res.Outcome != OutcomeTimeout || res.Duration > 2500*time.Millisecond {
		t.Fatalf("%+v", res)
	}
}

// 001-AC17: runs beyond the concurrency bound wait for a free slot.
func TestSandboxConcurrency(t *testing.T) {
	r := testRunner(t, Options{Timeout: 500 * time.Millisecond, Concurrency: 2})
	start := time.Now()
	done := make(chan Result, 4)
	for range 4 {
		go func() {
			done <- r.Run(context.Background(), `function main() { while (true) {} }`, sampleInput())
		}()
	}
	for range 4 {
		if res := <-done; res.Outcome != OutcomeTimeout {
			t.Fatalf("%+v", res)
		}
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("4 runs of 500 ms with 2 slots took %s", d)
	}
}

// 001-AC17: syntax is checked on save, with the error's position.
func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		src          string
		line, column int
		ok           bool
	}{
		{src: "function main() { return 1 }", ok: true},
		{src: "function main() {\n  return 1 +\n}", line: 3, column: 1},
		{src: "let a = ;", line: 1, column: 9},
		{src: strings.Repeat(" ", MaxSource+1)},
	} {
		err := Check(tt.src)
		if tt.ok {
			if err != nil {
				t.Fatalf("%q: %v", tt.src, err)
			}
			continue
		}
		se, ok := err.(*SyntaxError)
		if !ok || se.Line != tt.line || se.Column != tt.column || se.Error() == "" {
			t.Fatalf("%q: %#v", tt.src, err)
		}
	}
	// Deep nesting parses without blowing the parent's stack.
	start := time.Now()
	_ = Check(strings.Repeat("(", 30000) + "1" + strings.Repeat(")", 30000))
	_ = Check(strings.Repeat("[", 60000))
	if time.Since(start) > 5*time.Second {
		t.Fatalf("deep nesting took %s", time.Since(start))
	}
}

func TestResultErr(t *testing.T) {
	if (Result{Outcome: OutcomeOK}).Err() != nil || (Result{Outcome: OutcomeError, Error: "x"}).Err().Error() != "script error: x" {
		t.Fatal("Err")
	}
	if clip("abc", 10) != "abc" || clip("ééé", 5) != "é…" {
		t.Fatalf("clip: %q", clip("ééé", 5))
	}
}
