package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":           {Data: []byte("<html>SPA</html>")},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
		"favicon.svg":          {Data: []byte("<svg/>")},
		"sw.js":                {Data: []byte("self.addEventListener")},
	}
	h := Handler(fsys)
	tests := []struct {
		path     string
		code     int
		cache    string
		bodyHas  string
		ctypeHas string
	}{
		{"/", 200, cacheNone, "SPA", "text/html"},
		{"/index.html", 200, cacheNone, "SPA", "text/html"},
		{"/watches", 200, cacheNone, "SPA", "text/html"},
		{"/settings/profile", 200, cacheNone, "SPA", "text/html"},
		{"/assets", 200, cacheNone, "SPA", "text/html"},
		{"/assets/app-abc123.js", 200, cacheImmutable, "console.log", "javascript"},
		{"/assets/missing.js", 404, "", "", ""},
		{"/favicon.svg", 200, cacheNone, "<svg/>", "image/svg"},
		{"/../../etc/passwd", 200, cacheNone, "SPA", "text/html"},
		// 010-AC12: the service worker is JavaScript, never the SPA, and is re-checked on every load.
		{"/sw.js", 200, cacheNone, "self.addEventListener", "javascript"},
		{"/./sw.js", 200, cacheNone, "self.addEventListener", "javascript"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))
			if rec.Code != tt.code {
				t.Fatalf("code: want %d got %d", tt.code, rec.Code)
			}
			if tt.cache != "" && rec.Header().Get("Cache-Control") != tt.cache {
				t.Errorf("cache: want %q got %q", tt.cache, rec.Header().Get("Cache-Control"))
			}
			if !strings.Contains(rec.Body.String(), tt.bodyHas) {
				t.Errorf("body %q lacks %q", rec.Body.String(), tt.bodyHas)
			}
			if !strings.Contains(rec.Header().Get("Content-Type"), tt.ctypeHas) {
				t.Errorf("content-type %q lacks %q", rec.Header().Get("Content-Type"), tt.ctypeHas)
			}
		})
	}
}

// 010-AC12: a build without sw.js answers 404, so the browser never registers index.html as a worker.
func TestHandlerNoServiceWorker(t *testing.T) {
	h := Handler(fstest.MapFS{"index.html": {Data: []byte("<html>SPA</html>")}})
	for _, p := range []string{"/sw.js", "/sw.js/", "//sw.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "SPA") {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
}

func TestHandlerNotBuilt(t *testing.T) {
	h := Handler(fstest.MapFS{".gitkeep": {}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without a build, got %d", rec.Code)
	}
}

func TestInlineScriptHashes(t *testing.T) {
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
	}
	const theme = "\n      (function () { document.documentElement.classList.add('dark'); })();\n    "
	tests := []struct {
		name string
		fsys fstest.MapFS
		want []string
	}{
		{"no index", fstest.MapFS{}, nil},
		{"external only", fstest.MapFS{"index.html": {Data: []byte(`<script type="module" crossorigin src="/assets/a.js"></script>`)}}, nil},
		{"inline and external", fstest.MapFS{"index.html": {Data: []byte(`<head><script>` + theme + `</script><SCRIPT SRC="/x.js"></SCRIPT></head>`)}}, []string{sum(theme)}},
		{"two inline", fstest.MapFS{"index.html": {Data: []byte("<script>a()</script><script type=\"text/javascript\">b()</script>")}}, []string{sum("a()"), sum("b()")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InlineScriptHashes(tt.fsys)
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Fatalf("want %v got %v", tt.want, got)
			}
		})
	}
}
