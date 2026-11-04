// Package server builds gedour's HTTP handlers: the app (API, UI, health) and the metrics listener.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/route"
	"github.com/gedour/gedour/pkg/watch"
	"github.com/gedour/gedour/pkg/webui"
)

func init() {
	// The spec answers 400 to every invalid input (001-AC4, 001-AC22); huma defaults to 422.
	base := huma.NewError
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		if status == http.StatusUnprocessableEntity {
			status = http.StatusBadRequest
		}
		return base(status, msg, errs...)
	}
}

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version        string `json:"version" doc:"Release version"`
	CommitHash     string `json:"commit" doc:"Git commit the binary was built from"`
	BuildTimestamp string `json:"build_timestamp" doc:"Build time, ISO 8601"`
	Builder        string `json:"builder" doc:"Go toolchain that built the binary"`
	ProjectURL     string `json:"project_url" doc:"Source repository"`
}

// PingFunc reports whether the database is reachable.
type PingFunc func(ctx context.Context) error

type versionOutput struct {
	Body BuildInfo
}

// New returns the main handler: /healthz, /auth, the guarded /api surface and the SPA at /, behind security headers.
func New(log *zap.Logger, build BuildInfo, ping PingFunc, ui fs.FS, authn *auth.Service, pool *pgxpool.Pool, watches *watch.Service, routes *route.Service) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", Healthz(log, ping))

	cfg := huma.DefaultConfig("gedour", build.Version)
	cfg.OpenAPIPath = "/api/openapi"
	// Docs UI pulls scripts from a CDN; the JSON document is the contract.
	cfg.DocsPath = ""
	// The default create hook injects `$schema` URLs built from the request Host; drop it and its endpoint.
	cfg.CreateHooks = nil
	cfg.SchemasPath = ""
	api := humago.New(mux, cfg)
	huma.Register(api, huma.Operation{
		OperationID: "get-version",
		Method:      http.MethodGet,
		Path:        "/api/v1/version",
		Summary:     "Build information of the running server",
		Tags:        []string{"meta"},
	}, func(context.Context, *struct{}) (*versionOutput, error) {
		return &versionOutput{Body: build}, nil
	})
	authn.Register(api, mux)
	audit.Register(api, pool, log, auth.UserID)
	watches.Register(api, auth.UserID)
	routes.Register(api, mux, auth.UserID, authn.SessionAlive)

	// Per-method catch-alls: a method-less "/api/" would conflict with "GET /".
	fallback := apiFallback(api, mux)
	for _, m := range append(apiMethods, http.MethodOptions) {
		mux.Handle(m+" /api/", fallback)
	}
	// GET only: other methods on non-API paths get the mux's 405 instead of the SPA.
	mux.Handle("GET /", webui.Handler(ui))
	return secureHeaders(contentSecurityPolicy(webui.InlineScriptHashes(ui)), requestID(authn.Guard(mux)))
}

// requestID tags each request so its audit rows and the client's error report can be matched.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(audit.WithRequestID(r.Context(), id)))
	})
}

var apiMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// apiFallback answers problem+json for /api paths no operation matched: 405 when the path exists
// under another method, 404 otherwise. Never the SPA's HTML.
func apiFallback(api huma.API, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, m := range apiMethods {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); !strings.HasSuffix(pattern, " /api/") {
				allowed = append(allowed, m)
			}
		}
		ctx := humago.NewContext(nil, r, w)
		if len(allowed) > 0 {
			if slices.Contains(allowed, http.MethodGet) {
				allowed = append(allowed, http.MethodHead)
			}
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			_ = huma.WriteErr(api, ctx, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		_ = huma.WriteErr(api, ctx, http.StatusNotFound, "no API route matches this path")
	})
}

func contentSecurityPolicy(scriptHashes []string) string {
	scriptSrc := strings.Join(append([]string{"'self'"}, scriptHashes...), " ")
	return "default-src 'self'; script-src " + scriptSrc +
		"; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
}

func secureHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// Healthz answers 200 when the database answers a ping, 503 otherwise.
func Healthz(log *zap.Logger, ping PingFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ping(ctx); err != nil {
			log.Warn("healthz_db_unreachable", zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// NewRegistry returns a registry with Go/process collectors and gedour_build_info.
func NewRegistry(build BuildInfo) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gedour_build_info",
		Help: "Build information of the running binary; always 1.",
	}, []string{"version", "commit"})
	buildInfo.WithLabelValues(build.Version, build.CommitHash).Set(1)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
	)
	return reg
}

// Metrics returns the metrics-port handler; it carries /healthz too so worker-only pods have a probe.
func Metrics(log *zap.Logger, reg *prometheus.Registry, ping PingFunc) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.Handle("GET /healthz", Healthz(log, ping))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
