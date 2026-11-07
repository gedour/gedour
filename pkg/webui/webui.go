// Package webui serves the embedded single-page app with an index.html fallback.
package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNone      = "no-cache"
)

// Handler serves files from fsys; unknown non-asset paths get index.html so client routes survive a reload.
func Handler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				// Vite content-hashes everything under assets/, so those names never change content.
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", cacheImmutable)
				} else {
					w.Header().Set("Cache-Control", cacheNone)
				}
				http.ServeFileFS(w, r, fsys, name) //nolint:gosec // fs.FS refuses "..", name is already path.Clean-ed
				return
			}
			// A missing hashed asset or service worker must 404, not return HTML the browser would try to execute.
			if strings.HasPrefix(name, "assets/") || name == "sw.js" {
				http.NotFound(w, r)
				return
			}
		}
		index, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.Error(w, "frontend not built: run `make web`", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", cacheNone)
		_, _ = w.Write(index)
	})
}

var inlineScript = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)

// InlineScriptHashes returns the CSP source ('sha256-…') of every inline script in index.html,
// so the policy follows whatever the build actually ships.
func InlineScriptHashes(fsys fs.FS) []string {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return nil
	}
	var hashes []string
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		if bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
			continue
		}
		sum := sha256.Sum256(m[2])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return hashes
}
