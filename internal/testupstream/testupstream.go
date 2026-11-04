// Package testupstream is a fake GitHub, GitLab and OCI registry serving recorded fixtures, for the
// collector tests and the e2e run. Fixture set "a" is the first observation, "b" the changed one.
// For the e2e channels it also stands in for the Telegram Bot API and webhook receivers.
package testupstream

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

//go:embed fixtures/*.json
var fixtures embed.FS

// Fixture returns fixtures/<kind>_<set>.json; it panics on a missing file (a test bug).
func Fixture(kind, set string) []byte {
	b, err := fixtures.ReadFile(fmt.Sprintf("fixtures/%s_%s.json", kind, set))
	if err != nil {
		panic(err)
	}
	return b
}

// Part returns one top-level key of a bundled fixture (PR, MR and tag fixtures hold several responses).
func Part(kind, set, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(Fixture(kind, set), &m); err != nil {
		panic(err)
	}
	return m[key]
}

// TagPages returns the tag lists of an oci_tags fixture, page by page.
func TagPages(set string) [][]string {
	var pages []struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(Part("oci_tags", set, "pages"), &pages); err != nil {
		panic(err)
	}
	out := make([][]string, len(pages))
	for i, p := range pages {
		out[i] = p.Tags
	}
	return out
}

// Server serves the current fixture set.
type Server struct {
	// RegistryUser and RegistryPass, when set, make the token endpoint require that basic login.
	RegistryUser, RegistryPass string
	// RegistryBasic challenges with Basic instead of the Bearer token dance.
	RegistryBasic bool

	mu       sync.Mutex
	set      string
	hits     map[string]int
	mux      *http.ServeMux
	received []Received
}

// Received is one request a fake channel endpoint got.
type Received struct {
	// Endpoint is "telegram" or the webhook's name (POST /_hooks/{name}).
	Endpoint string            `json:"endpoint"`
	Header   map[string]string `json:"header"`
	Body     string            `json:"body"`
}

// maxReceived bounds what the fake keeps: the oldest requests go first.
const maxReceived = 200

// New returns a server on fixture set "a".
func New() *Server {
	s := &Server{set: "a", hits: map[string]int{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /repos/{o}/{r}/releases", s.static("github_release", ""))
	s.mux.HandleFunc("GET /repos/{o}/{r}/pulls/{n}", s.static("github_pr", "pull"))
	s.mux.HandleFunc("GET /repos/{o}/{r}/pulls/{n}/reviews", s.static("github_pr", "reviews"))
	s.mux.HandleFunc("GET /repos/{o}/{r}/commits/{sha}/check-runs", s.static("github_pr", "check_runs"))
	s.mux.HandleFunc("GET /repos/{o}/{r}/commits/{sha}/status", s.static("github_pr", "status"))
	s.mux.HandleFunc("GET /api/v4/projects/{p}/releases", s.static("gitlab_release", ""))
	s.mux.HandleFunc("GET /api/v4/projects/{p}/merge_requests/{iid}", s.static("gitlab_mr", "merge_request"))
	s.mux.HandleFunc("GET /api/v4/projects/{p}/merge_requests/{iid}/approvals", s.static("gitlab_mr", "approvals"))
	s.mux.HandleFunc("GET /v2/{rest...}", s.tags)
	s.mux.HandleFunc("GET /token", s.token)
	// Bot API under /tg (GEDOUR_TELEGRAM_API_URL=<fake>/tg): /tg/bot<token>/sendMessage. A mux wildcard
	// must span a whole segment, hence the prefix check.
	s.mux.HandleFunc("POST /tg/{bot}/sendMessage", s.receive(func(r *http.Request) string {
		if !strings.HasPrefix(r.PathValue("bot"), "bot") {
			return "fail-not-telegram"
		}
		return "telegram"
	}))
	// Names starting with "fail" answer 500, to drive retries.
	s.mux.HandleFunc("POST /_hooks/{name}", s.receive(func(r *http.Request) string { return r.PathValue("name") }))
	s.mux.HandleFunc("GET /_received", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(append([]Received{}, s.received...))
	})
	s.mux.HandleFunc("POST /_fixture/{set}", func(w http.ResponseWriter, r *http.Request) {
		s.SetFixture(r.PathValue("set"))
		w.WriteHeader(http.StatusNoContent)
	})
	return s
}

func (s *Server) receive(endpoint func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		name := endpoint(r)
		hdr := map[string]string{}
		for k := range r.Header {
			hdr[k] = r.Header.Get(k)
		}
		s.mu.Lock()
		s.received = append(s.received, Received{Endpoint: name, Header: hdr, Body: string(body)})
		if len(s.received) > maxReceived {
			s.received = s.received[1:]
		}
		s.mu.Unlock()
		if strings.HasPrefix(name, "fail") {
			http.Error(w, "receiver down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// Received lists what the fake channel endpoints got, oldest first.
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}

// SetFixture switches every route to fixture set ("a" or "b").
func (s *Server) SetFixture(set string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = set
}

// Hits counts requests by escaped path.
func (s *Server) Hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *Server) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.EscapedPath()]++
	s.mu.Unlock()
	s.mux.ServeHTTP(w, r)
}

// static serves a fixture (or one part of it) with an ETag, honouring If-None-Match.
func (s *Server) static(kind, part string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := Fixture(kind, s.current())
		if part != "" {
			body = Part(kind, s.current(), part)
		}
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if s.RegistryUser != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != s.RegistryUser || p != s.RegistryPass {
			http.Error(w, `{"details":"incorrect username or password"}`, http.StatusUnauthorized)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + r.URL.Query().Get("scope"), "expires_in": 300})
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	repo, ok := strings.CutSuffix(r.PathValue("rest"), "/tags/list")
	if !ok {
		http.NotFound(w, r)
		return
	}
	want := "Bearer tok-repository:" + repo + ":pull"
	challenge := fmt.Sprintf(`Bearer realm="http://%s/token",service="fake-registry",scope="repository:%s:pull"`, r.Host, repo)
	if s.RegistryBasic {
		req := &http.Request{Header: http.Header{}}
		req.SetBasicAuth(s.RegistryUser, s.RegistryPass)
		want, challenge = req.Header.Get("Authorization"), `Basic realm="fake-registry"`
	}
	if r.Header.Get("Authorization") != want {
		w.Header().Set("WWW-Authenticate", challenge)
		http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
		return
	}
	pages := TagPages(s.current())
	page := 0
	if last := r.URL.Query().Get("last"); last != "" {
		for i, p := range pages {
			if len(p) > 0 && p[len(p)-1] == last {
				page = i + 1
			}
		}
	}
	if page >= len(pages) {
		page = len(pages) - 1
	}
	if page+1 < len(pages) {
		last := pages[page][len(pages[page])-1]
		w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?last=%s&n=1000>; rel="next"`, repo, last))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": pages[page]})
}
