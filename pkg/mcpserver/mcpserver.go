// Package mcpserver serves /mcp: a table of tools, each dispatched in process to its REST handler with an API key's principal.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/ratelimit"
)

const (
	keyRate  = 5.0
	keyBurst = 20.0
	maxBody  = 1 << 20
	// A burst of 6, then one every 10 s: a call makes the worker fetch from an upstream, which a request's size does not show.
	limitedRate  = 6.0 / 60
	limitedBurst = 6.0
	// dispatchTimeout stays under the server's 30 s WriteTimeout: a legacy client's call is detached from its request.
	dispatchTimeout = 25 * time.Second
	// Versions compare as strings, as the SDK does; older client SDKs read a 404 as a transport failure, not as the JSON-RPC error it carries.
	notFoundFrom = "2026-07-28"

	outcomeOK       = "ok"
	outcomeError    = "error"
	outcomeRejected = "rejected"
)

const instructions = "gedour reports changes on watched upstream projects. " + untrustedWarning

// servedMethods is what reaches the SDK: its other methods decode their params on a goroutine nothing can recover (see ServeHTTP), for no feature gedour offers.
// ping, notifications/cancelled and notifications/roots/list_changed have no feature either; legacy clients send them, and an error back would fail those clients.
var servedMethods = map[string]bool{
	"initialize": true, "notifications/initialized": true, "ping": true, "server/discover": true, "tools/list": true, "tools/call": true,
	"notifications/cancelled": true, "notifications/roots/list_changed": true,
}

type handler struct {
	authn        *auth.Service
	log          *zap.Logger
	inner        http.Handler
	limits       *ratelimit.Limiter
	limited      *ratelimit.Limiter
	sdk          http.Handler
	servers      map[string]*mcp.Server
	order        map[string]int
	tools        []*tool
	calls        *prometheus.CounterVec
	authFailures prometheus.Counter
}

// caller is the request's key, on the context the SDK hands to tool handlers.
type caller struct {
	auth.KeyPrincipal
	url string
}

type callerKey struct{}

var errInvalidArgument = errors.New("invalid argument")

// New builds the /mcp handler from doc, which must already hold every operation; inner serves the sub-requests, below Guard.
func New(doc *huma.OpenAPI, inner http.Handler, authn *auth.Service, log *zap.Logger, reg prometheus.Registerer, version string) (http.Handler, error) {
	tools, err := buildTools(doc, rows)
	if err != nil {
		return nil, err
	}
	return newHandler(tools, inner, authn, log, reg, version), nil
}

func newHandler(tools []*tool, inner http.Handler, authn *auth.Service, log *zap.Logger, reg prometheus.Registerer, version string) *handler {
	h := &handler{authn: authn, log: log, inner: inner, limits: ratelimit.New(keyRate, keyBurst), limited: ratelimit.New(limitedRate, limitedBurst), servers: map[string]*mcp.Server{}, order: map[string]int{}, tools: tools,
		calls: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "gedour_mcp_tool_calls_total",
			Help: "MCP tool calls by tool and outcome (ok, error: isError, rejected: a protocol error such as a tool above the key's scope).",
		}, []string{"tool", "outcome"}),
		authFailures: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "gedour_mcp_auth_failures_total",
			Help: "Requests to /mcp refused for a missing or invalid API key.",
		}),
	}
	for i, t := range tools {
		h.order[t.name] = i
	}
	for scope, rank := range scopeRank {
		srv := mcp.NewServer(&mcp.Implementation{Name: "gedour", Version: version}, &mcp.ServerOptions{
			Instructions: instructions,
			// A key's tool list is its scope's: no shared cache may serve it to another key.
			SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) { c.CacheScope = "private" },
			// Stateless: no session to notify a list change on.
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		})
		srv.AddReceivingMiddleware(h.wrap)
		for _, t := range tools {
			if scopeRank[t.scope] <= rank {
				mcp.AddTool(srv, t.sdkTool(), h.call(t))
			}
		}
		h.servers[scope] = srv
	}
	h.sdk = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, _ := r.Context().Value(callerKey{}).(*caller)
		if c == nil {
			return nil
		}
		return h.servers[c.Scope]
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true,
		// A proxy hop arrives on loopback with the public Host; Origin and Guard's dev-mode check cover the DNS rebinding this guards against.
		DisableLocalhostProtection: true,
	})
	return h
}

func (t *tool) sdkTool() *mcp.Tool {
	desc := t.desc
	if strings.HasPrefix(t.name, "list_") {
		desc += " " + untrustedWarning
	}
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: t.readOnly, OpenWorldHint: new(t.openWorld)}
	// MCP's default is true, which fits only manage's edits: read and write tools destroy nothing, nor do manage's reads.
	if t.readOnly || t.scope != scopeManage {
		annotations.DestructiveHint = new(false)
	}
	return &mcp.Tool{Name: t.name, Description: desc, InputSchema: t.schema, Annotations: annotations}
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(huma.ErrorModel{Title: http.StatusText(status), Status: status, Detail: detail})
}

func bearer(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

// ServeHTTP checks in this order so each refusal costs the least: Origin, key, rate, body size, then the SDK.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !h.authn.OriginAllowed(origin) {
		writeProblem(w, http.StatusForbidden, "origin not allowed")
		return
	}
	p, err := h.authn.LookupKey(r.Context(), bearer(r))
	switch {
	case errors.Is(err, auth.ErrInvalidKey):
		h.authFailures.Inc()
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, http.StatusUnauthorized, "a valid API key is required")
		return
	case err != nil:
		// A database failure must not read as a bad key: a client would drop a valid one during an outage.
		h.log.Error("mcp_key_lookup_failed", zap.Error(err))
		writeProblem(w, http.StatusInternalServerError, "key lookup failed")
		return
	}
	if !h.limits.Allow(p.KeyID) {
		w.Header().Set("Retry-After", "1")
		writeProblem(w, http.StatusTooManyRequests, "too many requests for this key")
		return
	}
	fields := []zap.Field{zap.String("key_id", p.KeyID), zap.String("user_id", p.User.ID), zap.String("request_id", audit.RequestID(r.Context()))}
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeProblem(w, http.StatusRequestEntityTooLarge, "the request body is over 1 MiB")
			return
		case err != nil:
			writeProblem(w, http.StatusBadRequest, "the request body could not be read")
			return
		// Security: the SDK runs a pre-2025-06-18 batch, which would fit thousands of calls under one rate-limit token.
		case bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")):
			writeProblem(w, http.StatusBadRequest, "JSON-RPC batches are not served")
			return
		}
		// A body it cannot decode, or a client's response, stays the SDK's to refuse. Same decoder as the SDK's, so both read the same method.
		if req := decodeRequest(body); req != nil {
			if !servedMethods[req.Method] {
				// Never the method: the client chooses it.
				h.log.Info("mcp_method_refused", fields...)
				writeMethodNotFound(w, r, req)
				return
			}
			fields = append(fields, zap.String("method", req.Method))
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	// Past here SDK code runs on goroutines of its own (jsonrpc2.handleAsync): a panic there ahead of or behind wrap ends the process, and this line names the key.
	h.log.Info("mcp_request", fields...)
	ctx := context.WithValue(r.Context(), callerKey{}, &caller{KeyPrincipal: p, url: h.authn.PublicBase(r)})
	h.sdk.ServeHTTP(&refusalWriter{ResponseWriter: w}, r.WithContext(ctx))
}

func decodeRequest(body []byte) *jsonrpc.Request {
	msg, err := jsonrpc.DecodeMessage(body)
	if err != nil {
		return nil
	}
	req, _ := msg.(*jsonrpc.Request)
	return req
}

func writeMethodNotFound(w http.ResponseWriter, r *http.Request, req *jsonrpc.Request) {
	data, err := jsonrpc.EncodeMessage(&jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not served"}})
	// A notification has no ID to answer with.
	if err != nil || !req.IsCall() {
		writeProblem(w, http.StatusNotFound, "method not served")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("MCP-Protocol-Version") >= notFoundFrom {
		w.WriteHeader(http.StatusNotFound)
	}
	_, _ = w.Write(data)
}

// wrap guards twice: outside, so observe's own code cannot end the process; inside, so a handler's panic still gets its mcp_tool_call line.
func (h *handler) wrap(next mcp.MethodHandler) mcp.MethodHandler {
	return h.guard(h.observe(h.guard(next)))
}

// observe logs every tools/call and restores the table's order the SDK sorts away.
func (h *handler) observe(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list":
			res, err := next(ctx, method, req)
			// On a refusal the SDK returns a typed nil beside the error.
			if list, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				slices.SortFunc(list.Tools, func(a, b *mcp.Tool) int { return h.order[a.Name] - h.order[b.Name] })
			}
			return res, err
		case "tools/call":
			start := time.Now()
			var res mcp.Result
			var err error
			// Workaround: SDK v1.8.0 panics in ApplyDefaults on a null arguments, which the protocol reads as absent.
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && string(bytes.TrimSpace(params.Arguments)) == "null" {
				params.Arguments = nil
			}
			if c, known := h.dormantCall(ctx, req); known {
				res = errorResult("This key needs its owner to sign in: " + c.url + ". It works again right after.")
			} else {
				res, err = next(ctx, method, req)
			}
			sanitizeError(res, err)
			h.record(ctx, req, res, err, time.Since(start))
			return res, err
		}
		return next(ctx, method, req)
	}
}

// guard keeps a panic from ending the process: the SDK runs handlers on its own goroutine, out of net/http's recover.
func (h *handler) guard(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
		defer func() {
			if v := recover(); v != nil {
				h.log.Error("mcp_handler_panic", zap.String("method", method), zap.String("request_id", audit.RequestID(ctx)), zap.Stack("stack"))
				if method == "tools/call" {
					res, err = errorResult("Internal error. Request ID: "+audit.RequestID(ctx)), nil
					return
				}
				res, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
			}
		}()
		return next(ctx, method, req)
	}
}

// dormantCall spots a dormant key calling a tool of its scope; ahead of the SDK's validation, so bad arguments get the same sentence.
func (h *handler) dormantCall(ctx context.Context, req mcp.Request) (*caller, bool) {
	c, _ := ctx.Value(callerKey{}).(*caller)
	params, _ := req.GetParams().(*mcp.CallToolParamsRaw)
	if c == nil || params == nil || !c.Dormant {
		return nil, false
	}
	i, known := h.order[params.Name]
	return c, known && scopeRank[h.tools[i].scope] <= scopeRank[c.Scope]
}

func (h *handler) record(ctx context.Context, req mcp.Request, res mcp.Result, err error, took time.Duration) {
	outcome := outcomeOK
	if call, ok := res.(*mcp.CallToolResult); err != nil || !ok {
		outcome = outcomeRejected
	} else if call.IsError {
		outcome = outcomeError
	}
	// A client picks the name: anything off the table would grow the metric without bound.
	name := "unknown"
	if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
		if _, known := h.order[params.Name]; known {
			name = params.Name
		}
	}
	h.calls.WithLabelValues(name, outcome).Inc()
	c, _ := ctx.Value(callerKey{}).(*caller)
	if c == nil {
		return
	}
	h.log.Info("mcp_tool_call", zap.String("key_id", c.KeyID), zap.String("user_id", c.User.ID), zap.String("tool", name),
		zap.String("outcome", outcome), zap.Duration("duration", took), zap.String("request_id", audit.RequestID(ctx)))
}

func (h *handler) call(t *tool) mcp.ToolHandlerFor[map[string]any, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		c, ok := ctx.Value(callerKey{}).(*caller)
		if !ok {
			return nil, nil, errors.New("no caller on the context")
		}
		if t.limited && !h.limited.Allow(c.KeyID) {
			return errorResult("Too many calls of this kind for this key: a burst of 6, then one every 10 s, per replica. Try again later."), nil, nil
		}
		rec, err := h.dispatch(ctx, t, c, args)
		switch {
		case errors.Is(err, errInvalidArgument):
			return errorResult(err.Error()), nil, nil
		case err != nil:
			h.log.Error("mcp_dispatch_failed", zap.String("tool", t.name), zap.String("request_id", audit.RequestID(ctx)), zap.Error(err))
		case rec.status >= 200 && rec.status < 300:
			// A nil Out: from any other value the SDK would build a structured field beside the text (see fence).
			return shaped(t, rec.body.String(), args, noticeFor(c, time.Now())), nil, nil
		case rec.status >= 400 && rec.status < 500:
			return problemResult(rec.body.String()), nil, nil
		case rec.status < 500 || rec.status >= 600:
			h.log.Error("mcp_dispatch_unexpected_status", zap.String("tool", t.name), zap.String("request_id", audit.RequestID(ctx)), zap.Int("status", rec.status))
			return errorResult(errInvalidArgument.Error()), nil, nil
		}
		return errorResult("Internal error. Request ID: " + audit.RequestID(ctx)), nil, nil
	}
}

func shaped(t *tool, body string, args map[string]any, notice string) *mcp.CallToolResult {
	var data any
	if body != "" {
		dec := json.NewDecoder(strings.NewReader(body))
		// Keeps large IDs exact: the result must read as the REST body does.
		dec.UseNumber()
		if err := dec.Decode(&data); err != nil {
			return errorResult("the result could not be decoded")
		}
	}
	if t.shape != nil {
		t.shape(data, args)
	}
	return fence(data, notice)
}

// dispatch serves the tool's operation on inner, as REST would for the key's user.
func (h *handler) dispatch(ctx context.Context, t *tool, c *caller, args map[string]any) (*recorder, error) {
	ctx, cancel := context.WithTimeout(audit.WithAPIKey(auth.WithKeyPrincipal(ctx, c.KeyPrincipal), c.KeyID), dispatchTimeout)
	defer cancel()
	path := t.path
	query := url.Values{}
	fields := map[string]any{}
	for name, v := range args {
		switch t.in[name] {
		case "path":
			// A dot segment or an empty value would make the mux redirect to another path.
			if s := scalar(v); s == "" || s == "." || s == ".." {
				return nil, fmt.Errorf("%w: %s", errInvalidArgument, name)
			}
			// Security: escaped, so an argument stays one path segment and cannot reach another route.
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(scalar(v)))
		case "query":
			query.Set(name, scalar(v))
		case "body":
			fields[name] = v
		}
	}
	var body io.Reader
	if t.hasBody {
		raw, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, t.method, "http://gedour"+path+"?"+query.Encode(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{header: http.Header{}}
	h.inner.ServeHTTP(rec, req)
	rec.WriteHeader(http.StatusOK) // net/http's default for a handler that wrote nothing
	return rec, nil
}

func scalar(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
