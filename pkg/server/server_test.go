package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"
)

var build = BuildInfo{Version: "v1.2.3", CommitHash: "abc1234", BuildTimestamp: "2026-11-04T00:00:00Z", Builder: "go1.27", ProjectURL: "https://example.test"}

func pingOK(context.Context) error   { return nil }
func pingDown(context.Context) error { return errors.New("connection refused") }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

// 001-AC27: /healthz is 200 when PG answers, 503 otherwise, on both listeners.
func TestHealthz(t *testing.T) {
	ui := fstest.MapFS{"index.html": {Data: []byte("SPA")}}
	tests := []struct {
		name string
		ping PingFunc
		code int
	}{
		{"db up", pingOK, http.StatusOK},
		{"db down", pingDown, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlers := map[string]http.Handler{
				"main":    New(zap.NewNop(), build, tt.ping, ui),
				"metrics": Metrics(zap.NewNop(), NewRegistry(build), tt.ping),
			}
			for listener, h := range handlers {
				rec := get(t, h, "/healthz")
				if rec.Code != tt.code {
					t.Errorf("%s: want %d got %d", listener, tt.code, rec.Code)
				}
				if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
					t.Errorf("%s: want JSON, got %q", listener, rec.Header().Get("Content-Type"))
				}
			}
		})
	}
}

// 001-AC27: the metrics listener exposes the baseline gedour metric family.
func TestMetrics(t *testing.T) {
	rec := get(t, Metrics(zap.NewNop(), NewRegistry(build), pingOK), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`gedour_build_info{commit="abc1234",version="v1.2.3"} 1`, "go_goroutines"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// /metrics must never leak onto the public listener.
	if rec := get(t, New(zap.NewNop(), build, pingOK, fstest.MapFS{}), "/metrics"); strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Error("main listener serves metrics")
	}
}

func TestAPI(t *testing.T) {
	h := New(zap.NewNop(), build, pingOK, fstest.MapFS{"index.html": {Data: []byte("<html>SPA</html>")}})

	t.Run("version", func(t *testing.T) {
		rec := get(t, h, "/api/v1/version")
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		var got BuildInfo
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got != build {
			t.Fatalf("want %+v got %+v", build, got)
		}
	})

	t.Run("openapi", func(t *testing.T) {
		rec := get(t, h, "/api/openapi.json")
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		var doc struct {
			Info  struct{ Version string }
			Paths map[string]any
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.Paths["/api/v1/version"]; !ok || doc.Info.Version != build.Version {
			t.Fatalf("openapi lacks version op or info: %+v", doc)
		}
	})

	t.Run("no $schema injection", func(t *testing.T) {
		for _, path := range []string{"/api/v1/version", "/api/openapi.json"} {
			if body := get(t, h, path).Body.String(); strings.Contains(body, "$schema") {
				t.Errorf("%s carries $schema: %s", path, body)
			}
		}
		if rec := get(t, h, "/api/schemas/BuildInfo.json"); rec.Code != http.StatusNotFound {
			t.Errorf("schemas endpoint still served: %d", rec.Code)
		}
	})
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return rec
}

func TestRouting(t *testing.T) {
	h := New(zap.NewNop(), build, pingOK, fstest.MapFS{"index.html": {Data: []byte("<html>SPA</html>")}})
	tests := []struct {
		method, path string
		code         int
		ctype        string
		allow        string
	}{
		{http.MethodGet, "/api/nope", http.StatusNotFound, "application/problem+json", ""},
		{http.MethodGet, "/api/v1/nope", http.StatusNotFound, "application/problem+json", ""},
		{http.MethodPost, "/api/v1/nope", http.StatusNotFound, "application/problem+json", ""},
		{http.MethodPost, "/api/v1/version", http.StatusMethodNotAllowed, "application/problem+json", "GET, HEAD"},
		{http.MethodDelete, "/api/openapi.json", http.StatusMethodNotAllowed, "application/problem+json", "GET, HEAD"},
		{http.MethodHead, "/api/v1/version", http.StatusOK, "application/json", ""},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, "text/plain", "GET, HEAD"},
		{http.MethodHead, "/healthz", http.StatusOK, "application/json", ""},
		{http.MethodPost, "/inbox", http.StatusMethodNotAllowed, "text/plain", "GET, HEAD"},
		{http.MethodGet, "/inbox", http.StatusOK, "text/html", ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(t, h, tt.method, tt.path)
			if rec.Code != tt.code {
				t.Fatalf("code: want %d got %d (%s)", tt.code, rec.Code, rec.Body.String())
			}
			if !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.ctype) {
				t.Errorf("content-type: want %s got %q", tt.ctype, rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("Allow") != tt.allow {
				t.Errorf("allow: want %q got %q", tt.allow, rec.Header().Get("Allow"))
			}
			if strings.HasPrefix(tt.ctype, "application/problem+json") {
				var p struct {
					Status int
					Title  string
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Status != tt.code || p.Title == "" {
					t.Errorf("not a problem document: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	const script = "\n  (function(){document.documentElement.classList.add('dark')})();\n"
	sum := sha256.Sum256([]byte(script))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	ui := fstest.MapFS{
		"index.html":           {Data: []byte("<html><head><script>" + script + "</script><script type=module src=/assets/a.js></script></head></html>")},
		"assets/app-a1.js":     {Data: []byte("1")},
		"assets/font-b2.woff2": {Data: []byte("1")},
	}
	h := New(zap.NewNop(), build, pingOK, ui)
	for _, path := range []string{"/", "/inbox", "/assets/app-a1.js", "/assets/font-b2.woff2", "/api/v1/version", "/api/nope", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			hd := get(t, h, path).Header()
			for k, v := range map[string]string{
				"X-Content-Type-Options": "nosniff",
				"X-Frame-Options":        "DENY",
				"Referrer-Policy":        "same-origin",
			} {
				if hd.Get(k) != v {
					t.Errorf("%s: want %q got %q", k, v, hd.Get(k))
				}
			}
			csp := hd.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "script-src 'self' " + wantHash + ";"} {
				if !strings.Contains(csp, want) {
					t.Errorf("CSP %q lacks %q", csp, want)
				}
			}
		})
	}

	// The hash follows the served script: a different script must not keep the old hash.
	ui["index.html"] = &fstest.MapFile{Data: []byte("<script>other()</script>")}
	if csp := get(t, New(zap.NewNop(), build, pingOK, ui), "/").Header().Get("Content-Security-Policy"); strings.Contains(csp, wantHash) {
		t.Fatalf("CSP kept a stale hash: %s", csp)
	}
}

func TestHealthzRealServer(t *testing.T) {
	srv := httptest.NewServer(New(zap.NewNop(), build, pingOK, fstest.MapFS{}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/healthz", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}
