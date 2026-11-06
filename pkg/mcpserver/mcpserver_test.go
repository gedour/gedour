package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/config"
)

type nested struct {
	Label string `json:"label"`
}

type sampleInput struct {
	ID    string `path:"id"`
	Limit int    `query:"limit" maximum:"100" default:"80"`
	Body  struct {
		Name string `json:"name"`
		Sub  nested `json:"sub,omitempty"`
	}
}

type noDefaultInput struct {
	Limit int `query:"limit" maximum:"100"`
}

// sampleDoc holds a body-taking operation, one without limit and an admin one.
func sampleDoc() *huma.OpenAPI {
	cfg := huma.DefaultConfig("t", "1")
	cfg.CreateHooks = nil
	api := humago.New(http.NewServeMux(), cfg)
	huma.Register(api, huma.Operation{OperationID: "sample", Method: http.MethodPost, Path: "/api/v1/sample/{id}"},
		func(context.Context, *sampleInput) (*struct{}, error) { return nil, nil })
	huma.Register(api, huma.Operation{OperationID: "no-default", Method: http.MethodGet, Path: "/api/v1/no-default"},
		func(context.Context, *noDefaultInput) (*struct{}, error) { return nil, nil })
	huma.Register(api, huma.Operation{OperationID: "bare", Method: http.MethodGet, Path: "/api/v1/bare"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	huma.Register(api, huma.Operation{OperationID: "admin-thing", Method: http.MethodGet, Path: auth.AdminPrefix + "thing"},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	return api.OpenAPI()
}

// 007-AC14: a row naming an unknown operation or an admin path never starts, nor does any other row that cannot hold.
func TestBuildToolsRefuses(t *testing.T) {
	for _, tt := range []struct {
		name string
		r    row
		want string
	}{
		{"unknown operation", row{name: "x", op: "nope", scope: scopeRead}, "unknown operation"},
		{"admin path", row{name: "x", op: "admin-thing", scope: scopeRead}, auth.AdminPrefix},
		{"unknown scope", row{name: "x", op: "bare", scope: "admin"}, "unknown scope"},
		{"limit on an operation without one", row{name: "x", op: "bare", scope: scopeRead, limitMax: 5}, "no limit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildTools(sampleDoc(), []row{tt.r}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := buildTools(sampleDoc(), []row{{name: "ok", op: "bare", scope: scopeRead}}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRefs(map[string]any{"$ref": "#/components/schemas/A"}, map[string]any{"components": map[string]any{"schemas": map[string]any{
		"A": map[string]any{"properties": map[string]any{"a": map[string]any{"$ref": "#/components/schemas/A"}}},
	}}}, nil); err == nil {
		t.Fatal("a recursive schema was accepted")
	}
	if _, err := resolveRefs(map[string]any{"$ref": "#/components/schemas/B"}, map[string]any{}, nil); err == nil {
		t.Fatal("an unknown schema was accepted")
	}
}

// The real table builds against nothing but its own operations: a typo in it fails here too.
func TestCatalogOrder(t *testing.T) {
	want := []string{"count_inbox", "list_inbox", "list_watches", "get_watch", "list_watch_events", "list_watch_states", "list_rules", "get_rule", "list_deliveries"}
	if len(rows) != len(want) {
		t.Fatalf("%d rows", len(rows))
	}
	for i, r := range rows {
		if r.name != want[i] {
			t.Errorf("row %d: %s, want %s", i, r.name, want[i])
		}
	}
}

// 007-AC25: path, query and body flatten; $ref resolves; limit caps apply; unknown fields are refused.
func TestSchema(t *testing.T) {
	tools, err := buildTools(sampleDoc(), []row{{name: "s", op: "sample", scope: scopeWrite, limitDefault: 20, limitMax: 50}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tools[0].schema)
	if strings.Contains(string(raw), "$ref") {
		t.Fatalf("$ref left: %s", raw)
	}
	var s struct {
		Properties map[string]struct {
			Type       any            `json:"type"`
			Default    float64        `json:"default"`
			Maximum    float64        `json:"maximum"`
			Properties map[string]any `json:"properties"`
		} `json:"properties"`
		Required []string `json:"required"`
		Extra    *bool    `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Extra == nil || *s.Extra || len(s.Properties) != 4 || s.Properties["limit"].Maximum != 50 || s.Properties["limit"].Default != 20 ||
		s.Properties["sub"].Properties["label"] == nil || strings.Join(s.Required, ",") != "id,name" {
		t.Fatalf("%s", raw)
	}
	// Without its own default, a tool's cap still pulls the operation's default under it.
	tools, err = buildTools(sampleDoc(), []row{{name: "s", op: "sample", scope: scopeWrite, limitMax: 50}})
	if err != nil {
		t.Fatal(err)
	}
	if got := tools[0].schema["properties"].(map[string]any)["limit"].(map[string]any)["default"]; got != 50 {
		t.Fatalf("default %v", got)
	}
}

// An operation with no default keeps none under a tool's cap: a default of 0 would be below its own minimum. A row's own default still applies.
func TestLimitWithoutDefault(t *testing.T) {
	limitOf := func(r row) map[string]any {
		tools, err := buildTools(sampleDoc(), []row{r})
		if err != nil {
			t.Fatal(err)
		}
		return tools[0].schema["properties"].(map[string]any)["limit"].(map[string]any)
	}
	limit := limitOf(row{name: "n", op: "no-default", scope: scopeRead, limitMax: 50})
	if _, ok := limit["default"]; ok || limit["maximum"] != 50 {
		t.Fatalf("%v", limit)
	}
	if limit := limitOf(row{name: "n", op: "no-default", scope: scopeRead, limitDefault: 20, limitMax: 50}); limit["default"] != 20 {
		t.Fatalf("%v", limit)
	}
}

// Keys differing only by a stripped character collapse: the same one wins on every call, or a result would change between identical calls.
func TestSanitizeCollisionDeterministic(t *testing.T) {
	strip := string(rune(0x200B))
	for range 200 {
		got := sanitize(map[string]any{"key": "plain", "k" + strip + "ey": "first byte differs", "ke" + strip + "y": "second byte differs"}).(map[string]any)
		// The last in byte order wins: "k" + U+200B sorts after "ke".
		if len(got) != 1 || got["key"] != "first byte differs" {
			t.Fatalf("%v", got)
		}
	}
}

// 007-AC28: every listed class leaves values and map keys, at any depth; tab, LF and CR stay, as do variation selectors.
func TestSanitize(t *testing.T) {
	for name, r := range map[string]rune{
		"C0": 0x01, "ESC": 0x1B, "DEL": 0x7F, "C1 low": 0x80, "C1 high": 0x9F, "soft hyphen": 0xAD, "LRE": 0x202A, "RLO": 0x202E, "LRI": 0x2066, "PDI": 0x2069,
		"ZWSP": 0x200B, "ZWNJ": 0x200C, "ZWJ": 0x200D, "word joiner": 0x2060, "BOM": 0xFEFF, "arabic number sign": 0x0600,
		"line separator": 0x2028, "paragraph separator": 0x2029, "tag low": 0xE0000, "tag A": 0xE0041, "tag high": 0xE007F,
	} {
		t.Run(name, func(t *testing.T) {
			bad := string(r)
			in := map[string]any{"a" + bad + "b": []any{"c" + bad + "d", map[string]any{"k" + bad: "v" + bad}}, "n": json.Number("1"), "z": nil}
			got, _ := json.Marshal(sanitize(in))
			if want := `{"ab":["cd",{"k":"v"}],"n":1,"z":null}`; string(got) != want {
				t.Fatalf("%s, want %s", got, want)
			}
		})
	}
	nbsp, vs16 := string(rune(0xA0)), string(rune(0xFE0F))
	kept := "a\tb\nc\rd" + nbsp + "e" + vs16 + "f"
	if got := sanitize(kept); got != kept {
		t.Fatalf("%q", got)
	}
}

// 007-AC29: true exactly when as many as the limit came back; the other side has no key at all.
func TestMarkTruncated(t *testing.T) {
	data := func(n int) map[string]any {
		return map[string]any{"items": make([]any, n)}
	}
	for _, tt := range []struct {
		name string
		n    int
		args map[string]any
		want bool
	}{
		{"at the limit", 2, map[string]any{"limit": float64(2)}, true},
		{"under it", 1, map[string]any{"limit": float64(2)}, false},
		{"no limit known", 2, nil, false},
		{"empty", 0, map[string]any{"limit": float64(2)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := data(tt.n)
			markTruncated(d, tt.args)
			if _, got := d["truncated"]; got != tt.want {
				t.Fatalf("%v", d)
			}
		})
	}
}

// 007-AC29: the cut is visible and stays on a rune boundary, at exactly the limit nothing changes.
func TestCutInboxBodies(t *testing.T) {
	item := func(body string) any {
		return map[string]any{"items": []any{map[string]any{"event": map[string]any{"payload": map[string]any{"body": body}}}}}
	}
	body := func(d any) string {
		return items(d)[0].(map[string]any)["event"].(map[string]any)["payload"].(map[string]any)["body"].(string)
	}
	d := item(strings.Repeat("x", maxBodyInList-1) + "é" + strings.Repeat("y", 100))
	cutInboxBodies(d, nil)
	got := body(d)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, cutMarker) || len(got) != maxBodyInList-1+len(cutMarker) {
		t.Fatalf("len %d, valid %v", len(got), utf8.ValidString(got))
	}
	exact := strings.Repeat("x", maxBodyInList)
	d = item(exact)
	cutInboxBodies(d, nil)
	if body(d) != exact {
		t.Fatal("a body at the limit was cut")
	}
	cutInboxBodies(map[string]any{"items": []any{"odd", map[string]any{"event": 1}}}, nil) // shapes must not panic on an unexpected document
}

// 007-AC40: the window's two sides and each wording.
func TestNoticeFor(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	const day = 24 * time.Hour
	for _, tt := range []struct {
		name     string
		end, exp *time.Time
		want     string
	}{
		{"no end", nil, nil, ""},
		{"8 days", at(8 * day), at(8 * day), ""},
		{"just over 7 days", at(7*day + time.Second), nil, ""},
		{"exactly 7 days, sign-in", at(7 * day), nil, "This key stops on 2026-01-08 unless its owner signs in at https://u"},
		{"7 days, expiry first", at(7 * day), at(7 * day), "This key expires on 2026-01-08: create a new one at https://u"},
		{"sign-in before expiry", at(2 * day), at(5 * day), "This key stops on 2026-01-03 unless its owner signs in at https://u"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &caller{url: "https://u"}
			c.EffectiveUntil, c.ExpiresAt = tt.end, tt.exp
			if got := noticeFor(c, now); got != tt.want {
				t.Fatalf("%q", got)
			}
		})
	}
}

func testHandler(t *testing.T, inner http.Handler, rs []row) *handler {
	t.Helper()
	authn, err := auth.New(config.Config{DevUser: "dev@local"}, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	tools, err := buildTools(sampleDoc(), rs)
	if err != nil {
		t.Fatal(err)
	}
	return newHandler(tools, inner, authn, zap.NewNop(), nil, "test")
}

func callCtx(p auth.KeyPrincipal) context.Context {
	return context.WithValue(audit.WithRequestID(context.Background(), "req-1"), callerKey{}, &caller{KeyPrincipal: p, url: "https://u"})
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*mcp.TextContent).Text
}

// 007-AC13: a dormant key reads no data: its call never reaches the SDK's handler, whatever its arguments.
func TestDormantNeverDispatches(t *testing.T) {
	h := testHandler(t, http.NotFoundHandler(), []row{{name: "b", op: "bare", scope: scopeRead}, {name: "w", op: "bare", scope: scopeWrite}})
	var hits atomic.Int32
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		hits.Add(1)
		return &mcp.CallToolResult{}, nil
	}
	call := func(p auth.KeyPrincipal, name string) mcp.Result {
		res, err := h.observe(next)(callCtx(p), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := call(auth.KeyPrincipal{Dormant: true, Scope: scopeRead}, "b").(*mcp.CallToolResult)
	if !res.IsError || hits.Load() != 0 {
		t.Fatalf("res %+v hits %d", res, hits.Load())
	}
	if got, want := textOf(t, res), "This key needs its owner to sign in: https://u. It works again right after."; got != want {
		t.Fatalf("%q", got)
	}
	for _, tt := range []struct {
		name string
		p    auth.KeyPrincipal
		tool string
	}{
		{"awake", auth.KeyPrincipal{Scope: scopeRead}, "b"},
		{"above its scope", auth.KeyPrincipal{Dormant: true, Scope: scopeRead}, "w"},
		{"off the table", auth.KeyPrincipal{Dormant: true, Scope: scopeRead}, "x"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hits.Store(0)
			call(tt.p, tt.tool)
			if hits.Load() != 1 {
				t.Fatal("not passed on to the SDK")
			}
		})
	}
}

// A 5xx says the request ID and nothing the handler wrote.
func TestServerSideFailure(t *testing.T) {
	h := testHandler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "leak: pg password", 500) }), []row{{name: "b", op: "bare", scope: scopeRead}})
	res, _, err := h.call(&tool{row: row{name: "b"}, method: "GET", path: "/api/v1/bare"})(callCtx(auth.KeyPrincipal{}), nil, nil)
	if err != nil || !res.IsError || textOf(t, res) != "Internal error. Request ID: req-1" {
		t.Fatalf("%+v %v", res, err)
	}
}

// Any status but 2xx, 4xx and 5xx is logged and reads as a bad argument: neither the Location nor a false "internal error" reaches the caller.
func TestUnexpectedStatus(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusNotModified, 101, 600} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := testHandler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/leak")
				w.WriteHeader(status)
			}), []row{{name: "b", op: "bare", scope: scopeRead}})
			core, logs := observer.New(zap.ErrorLevel)
			h.log = zap.New(core)
			res, _, err := h.call(&tool{row: row{name: "b"}, method: "GET", path: "/api/v1/bare"})(callCtx(auth.KeyPrincipal{}), nil, nil)
			if err != nil || !res.IsError || textOf(t, res) != "invalid argument" {
				t.Fatalf("%+v %v", res, err)
			}
			lines := logs.FilterMessage("mcp_dispatch_unexpected_status").All()
			if len(lines) != 1 || lines[0].ContextMap()["status"] != int64(status) || lines[0].ContextMap()["tool"] != "b" {
				t.Fatalf("%v", logs.All())
			}
		})
	}
}

// A path argument that would make the mux redirect is refused by name, before any sub-request.
func TestDispatchRefusesDotSegments(t *testing.T) {
	var hits atomic.Int32
	h := testHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }), []row{{name: "s", op: "sample", scope: scopeRead}})
	tools, err := buildTools(sampleDoc(), []row{{name: "s", op: "sample", scope: scopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", ".."} {
		res, _, err := h.call(tools[0])(callCtx(auth.KeyPrincipal{}), nil, map[string]any{"id": id})
		if err != nil || !res.IsError || textOf(t, res) != "invalid argument: id" || hits.Load() != 0 {
			t.Fatalf("%q: %+v %v hits %d", id, res, err, hits.Load())
		}
	}
	// Only the whole value counts: these are ordinary segments.
	for _, id := range []string{"...", ".a", "a."} {
		if res, _, err := h.call(tools[0])(callCtx(auth.KeyPrincipal{}), nil, map[string]any{"id": id}); err != nil || res.IsError {
			t.Fatalf("%q: %+v %v", id, res, err)
		}
	}
}

// panicRequest panics in observe's own code, where it reads the params: outside any handler.
type panicRequest struct{ *mcp.CallToolRequest }

func (panicRequest) GetParams() mcp.Params { panic("leak: pg password") }

// A panic on the SDK's goroutine would end the process, wherever it starts: in a handler, or in observe's own code.
func TestPanicIsContained(t *testing.T) {
	h := testHandler(t, http.NotFoundHandler(), []row{{name: "b", op: "bare", scope: scopeRead}})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "b"}}
	listReq := &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}}
	ok := func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.CallToolResult{}, nil }
	panics := func(context.Context, string, mcp.Request) (mcp.Result, error) { panic("leak: pg password") }
	wantInternal := func(t *testing.T, res mcp.Result, err error) {
		t.Helper()
		call, isCall := res.(*mcp.CallToolResult)
		if err != nil || !isCall || !call.IsError || textOf(t, call) != "Internal error. Request ID: req-1" {
			t.Fatalf("%+v %v", res, err)
		}
	}
	t.Run("handler on a call", func(t *testing.T) {
		res, err := h.wrap(panics)(callCtx(auth.KeyPrincipal{}), "tools/call", req)
		wantInternal(t, res, err)
	})
	t.Run("observe on a call", func(t *testing.T) {
		res, err := h.wrap(ok)(callCtx(auth.KeyPrincipal{}), "tools/call", panicRequest{req})
		wantInternal(t, res, err)
	})
	var werr *jsonrpc.Error
	t.Run("handler on another method", func(t *testing.T) {
		if _, err := h.wrap(panics)(callCtx(auth.KeyPrincipal{}), "tools/list", listReq); !errors.As(err, &werr) || werr.Code != jsonrpc.CodeInternalError {
			t.Fatalf("%v", err)
		}
	})
	t.Run("observe's sort", func(t *testing.T) {
		nils := func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return &mcp.ListToolsResult{Tools: []*mcp.Tool{nil, nil}}, nil
		}
		if _, err := h.wrap(nils)(callCtx(auth.KeyPrincipal{}), "tools/list", listReq); !errors.As(err, &werr) || werr.Code != jsonrpc.CodeInternalError {
			t.Fatalf("%v", err)
		}
	})
	// A handler's panic is still one mcp_tool_call line.
	core, logs := observer.New(zap.InfoLevel)
	h.log = zap.New(core)
	if res, err := h.wrap(panics)(callCtx(auth.KeyPrincipal{}), "tools/call", req); err != nil || res == nil {
		t.Fatalf("%v %v", res, err)
	}
	if n := len(logs.FilterMessage("mcp_tool_call").All()); n != 1 {
		t.Fatalf("%d lines", n)
	}
}

// The SDK answers a refusal with a typed nil result and an error: observe must not read through it (no guard here, so a regression ends the test binary).
func TestObserveTypedNil(t *testing.T) {
	h := testHandler(t, http.NotFoundHandler(), []row{{name: "b", op: "bare", scope: scopeRead}})
	refused := errors.New("refused")
	for _, tt := range []struct {
		method string
		req    mcp.Request
		nilRes mcp.Result
	}{
		{"tools/list", &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}}, (*mcp.ListToolsResult)(nil)},
		{"tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "b"}}, (*mcp.CallToolResult)(nil)},
	} {
		next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return tt.nilRes, refused }
		if _, err := h.observe(next)(callCtx(auth.KeyPrincipal{}), tt.method, tt.req); !errors.Is(err, refused) {
			t.Fatalf("%s: %v", tt.method, err)
		}
	}
}

// Any isError text leaves observe sanitized, whoever wrote it: the SDK's validation quotes the caller's input.
func TestErrorTextSanitized(t *testing.T) {
	h := testHandler(t, http.NotFoundHandler(), []row{{name: "b", op: "bare", scope: scopeRead}})
	dirty := "bad" + string(rune(0x202E)) + "field\tok"
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return textResult(dirty, true), nil }
	res, err := h.observe(next)(callCtx(auth.KeyPrincipal{}), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "b"}})
	if err != nil || textOf(t, res.(*mcp.CallToolResult)) != "badfield\tok" {
		t.Fatalf("%+v %v", res, err)
	}
	// A successful result's text is the fence's business: not touched here.
	next = func(context.Context, string, mcp.Request) (mcp.Result, error) { return textResult(dirty, false), nil }
	if res, _ := h.observe(next)(callCtx(auth.KeyPrincipal{}), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "b"}}); textOf(t, res.(*mcp.CallToolResult)) != dirty {
		t.Fatal("a success was rewritten")
	}
}

// 007-AC14: the sub-request carries the key's user, no CSRF token and no admin flag; its arguments land where the operation reads them.
func TestDispatchRequestShape(t *testing.T) {
	var got *http.Request
	var body []byte
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	h := testHandler(t, inner, []row{{name: "s", op: "sample", scope: scopeRead}})
	tools, err := buildTools(sampleDoc(), []row{{name: "s", op: "sample", scope: scopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := callCtx(auth.KeyPrincipal{User: auth.User{ID: "u1"}})
	res, _, err := h.call(tools[0])(ctx, nil, map[string]any{"id": "../../admin/users", "limit": float64(1000000), "name": "n"})
	if err != nil || res.IsError {
		t.Fatalf("%+v %v", res, err)
	}
	// An escaped argument stays one segment: never a way onto another route.
	if got.Method != http.MethodPost || got.URL.EscapedPath() != "/api/v1/sample/..%2F..%2Fadmin%2Fusers" || got.URL.Query().Get("limit") != "1000000" {
		t.Fatalf("%s %s", got.Method, got.URL)
	}
	if string(body) != `{"name":"n"}` || got.Header.Get("Content-Type") != "application/json" || got.Header.Get(auth.CSRFHeader) != "" {
		t.Fatalf("body %s headers %v", body, got.Header)
	}
	if u, ok := auth.UserFrom(got.Context()); !ok || u.ID != "u1" {
		t.Fatalf("user %+v %v", u, ok)
	}
	if audit.RequestID(got.Context()) != "req-1" {
		t.Fatal("the outer request ID was lost")
	}
}

// 007-AC25: a 4xx carries the detail and each field, sanitized on its way out; a body that is no problem says nothing of it.
func TestProblemResult(t *testing.T) {
	h := testHandler(t, http.NotFoundHandler(), []row{{name: "b", op: "bare", scope: scopeRead}})
	body := `{"detail":"bad` + string(rune(0x202E)) + `","errors":[{"location":"query.limit","message":"too big","value":"secret"}]}`
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return problemResult(body), nil }
	res, err := h.observe(next)(callCtx(auth.KeyPrincipal{}), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	call := res.(*mcp.CallToolResult)
	if got := textOf(t, call); !call.IsError || got != "bad\nquery.limit: too big" {
		t.Fatalf("%q", got)
	}
	if got := textOf(t, problemResult("<html>")); got != "the request was refused" {
		t.Fatalf("%q", got)
	}
}

// Every line an operator would look for after "Request ID: X" carries X.
func TestLogsCarryRequestID(t *testing.T) {
	reqCall := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "b"}}
	bare := &tool{row: row{name: "b"}, method: "GET", path: "/api/v1/bare"}
	for _, tt := range []struct {
		name    string
		inner   http.HandlerFunc
		tool    *tool
		message string
	}{
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, bare, "mcp_tool_call"},
		{"dispatch failure", nil, &tool{row: row{name: "b"}, method: "BAD METHOD", path: "/api/v1/bare"}, "mcp_dispatch_failed"},
		{"unexpected status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(301) }, bare, "mcp_dispatch_unexpected_status"},
		{"panic", func(http.ResponseWriter, *http.Request) { panic("x") }, bare, "mcp_handler_panic"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := testHandler(t, tt.inner, []row{{name: "b", op: "bare", scope: scopeRead}})
			core, logs := observer.New(zap.InfoLevel)
			h.log = zap.New(core)
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				res, _, err := h.call(tt.tool)(ctx, nil, nil)
				return res, err
			}
			if _, err := h.wrap(next)(callCtx(auth.KeyPrincipal{}), "tools/call", reqCall); err != nil {
				t.Fatal(err)
			}
			lines := logs.FilterMessage(tt.message).All()
			if len(lines) != 1 || lines[0].ContextMap()["request_id"] != "req-1" {
				t.Fatalf("%v", logs.All())
			}
			if call := logs.FilterMessage("mcp_tool_call").All(); len(call) != 1 || call[0].ContextMap()["request_id"] != "req-1" {
				t.Fatalf("%v", logs.All())
			}
		})
	}
}

// 007-AC23: scopes nest: a tool above the key's scope is absent from the list and -32602 on call, and order is the table's, not the SDK's.
func TestScopes(t *testing.T) {
	h := testHandler(t, http.NewServeMux(), []row{
		{name: "z_read", op: "bare", scope: scopeRead},
		{name: "m_write", op: "bare", scope: scopeWrite},
		{name: "a_manage", op: "bare", scope: scopeManage},
	})
	for _, tt := range []struct {
		scope   string
		want    string
		refused string
	}{
		{scopeRead, "z_read", "m_write"},
		{scopeWrite, "z_read,m_write", "a_manage"},
		{scopeManage, "z_read,m_write,a_manage", ""},
	} {
		t.Run(tt.scope, func(t *testing.T) {
			ct, st := mcp.NewInMemoryTransports()
			ctx := context.Background()
			if _, err := h.servers[tt.scope].Connect(ctx, st, nil); err != nil {
				t.Fatal(err)
			}
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			list, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
			}
			if got := strings.Join(names, ","); got != tt.want {
				t.Fatalf("%s", got)
			}
			if list.CacheScope != "private" {
				t.Fatalf("cacheScope %q", list.CacheScope)
			}
			if tt.refused != "" {
				_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tt.refused})
				var werr *jsonrpc.Error
				if !errors.As(err, &werr) || werr.Code != jsonrpc.CodeInvalidParams {
					t.Fatalf("%v", err)
				}
			}
		})
	}
}

// 007-AC19: only a two-field Bearer header holds a key; the query string never does.
func TestBearerParsing(t *testing.T) {
	for header, want := range map[string]string{"": "", "Bearer": "", "Basic gdr_x": "", "Bearer a b": "", "Bearer gdr_x": "gdr_x", "bearer  gdr_x": "gdr_x"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp?key=gdr_query", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := bearer(r); got != want {
			t.Errorf("%q: %q", header, got)
		}
	}
}
