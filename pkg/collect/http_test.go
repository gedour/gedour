package collect

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/netguard"
)

func fetcher(t *testing.T) *Fetcher {
	t.Helper()
	g, err := netguard.New([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	return &Fetcher{Client: g.Client(netguard.Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1"}}),
		UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}}
}

func cfgJSON(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 001-AC7: every kind runs end to end against the fake upstream: fixture A, then B, then the events.
func TestCollectorsAgainstUpstream(t *testing.T) {
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, tt := range []struct {
		kind  string
		cfg   map[string]any
		types []string
	}{
		{"github_release", map[string]any{"repo": "cli/cli", "include_prereleases": true, "api_url": srv.URL},
			[]string{"release.promoted", "release.published", "release.published"}},
		{"github_pr", map[string]any{"repo": "cli/cli", "number": 14925, "api_url": srv.URL},
			[]string{"pr.draft_changed", "pr.review_changed", "pr.ci_changed"}},
		{"gitlab_release", map[string]any{"base_url": srv.URL, "project": "gitlab-org/gitlab-runner"},
			[]string{"release.published", "release.published", "release.published", "release.published", "release.published"}},
		{"gitlab_mr", map[string]any{"base_url": srv.URL, "project": "gitlab-org/gitlab-runner", "iid": 7764},
			[]string{"mr.state_changed", "mr.approval_changed", "mr.pipeline_changed"}},
		{"oci_tags", map[string]any{"registry": host, "repository": "library/postgres", "tag_regex": `^\d+(\.\d+)?$`, "semver_only": true},
			[]string{"tag.created", "tag.created"}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			c := Collectors()[tt.kind]
			up.SetFixture("a")
			a, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, tt.cfg), nil)
			if err != nil {
				t.Fatal(err)
			}
			up.SetFixture("b")
			f := fetcher(t)
			b, err := c.Collect(t.Context(), f, cfgJSON(t, tt.cfg), nil)
			if err != nil {
				t.Fatal(err)
			}
			// 004-AC1: only the release kinds hand notes over, and they reach the state nowhere.
			if isRelease := strings.HasSuffix(tt.kind, "_release"); isRelease != (f.Notes != nil) || strings.Contains(string(b), "## ") {
				t.Fatalf("notes %v in the state %.80s", f.Notes != nil, b)
			}
			events, err := c.Diff(a, b)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range events {
				got = append(got, e.Type)
			}
			if strings.Join(got, ",") != strings.Join(tt.types, ",") {
				t.Fatalf("events %v, want %v", got, tt.types)
			}
		})
	}
	// The GitLab project path is one URL segment.
	if up.Hits("/api/v4/projects/gitlab-org%2Fgitlab-runner/releases") != 2 {
		t.Fatal("project path not escaped")
	}
}

// 001-AC11: release collectors send the stored ETag; a 304 means unchanged.
func TestConditionalRequests(t *testing.T) {
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	for _, tt := range []struct {
		kind string
		cfg  map[string]any
	}{
		{"github_release", map[string]any{"repo": "cli/cli", "api_url": srv.URL}},
		{"gitlab_release", map[string]any{"base_url": srv.URL, "project": "g/p"}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			up.SetFixture("a")
			c := Collectors()[tt.kind]
			first := fetcher(t)
			if _, err := c.Collect(t.Context(), first, cfgJSON(t, tt.cfg), nil); err != nil {
				t.Fatal(err)
			}
			if len(first.Updates) != 1 {
				t.Fatalf("validators: %v", first.Updates)
			}
			second := fetcher(t)
			second.Cache = first.Updates
			if _, err := c.Collect(t.Context(), second, cfgJSON(t, tt.cfg), nil); !errors.Is(err, ErrNotModified) {
				t.Fatalf("want ErrNotModified, got %v", err)
			}
			up.SetFixture("b")
			if _, err := c.Collect(t.Context(), second, cfgJSON(t, tt.cfg), nil); err != nil {
				t.Fatalf("changed upstream: %v", err)
			}
			// Without a stored state (nil cache) nothing conditional is sent.
			if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, tt.cfg), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Tokens go in each forge's header; the user agent names gedour.
func TestCredentialHeaders(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+"|"+r.Header.Get("Private-Token")+"|"+r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cred := &Credential{Kind: "x", Secret: "s3cret"}
	if _, err := Collectors()["github_release"].Collect(t.Context(), fetcher(t), cfgJSON(t, map[string]any{"repo": "a/b", "api_url": srv.URL}), cred); err != nil {
		t.Fatal(err)
	}
	if _, err := Collectors()["gitlab_release"].Collect(t.Context(), fetcher(t), cfgJSON(t, map[string]any{"base_url": srv.URL, "project": "a/b"}), cred); err != nil {
		t.Fatal(err)
	}
	if seen[0] != "Bearer s3cret||gedour/test" || seen[1] != "|s3cret|gedour/test" {
		t.Fatalf("headers: %q", seen)
	}
}

// 001-AC11: each forge's rate-limit answer becomes a RateLimitError with its reset time.
func TestRateLimited(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := now.Add(17 * time.Minute)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	unix := fmt.Sprint(reset.Unix())
	for _, tt := range []struct {
		name   string
		status int
		h      http.Header
		until  time.Time
		ok     bool
	}{
		{"github primary 403", 403, h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", unix), reset, true},
		{"github primary 429", 429, h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", unix), reset, true},
		{"github secondary retry-after", 403, h("Retry-After", "60"), now.Add(time.Minute), true},
		{"retry-after date on 503", 503, h("Retry-After", reset.Format(http.TimeFormat)), reset, true},
		{"gitlab ratelimit-reset", 429, h("RateLimit-Reset", unix), reset, true},
		{"bare 429", 429, h(), now.Add(time.Minute), true},
		{"403 forbidden is not a rate limit", 403, h("X-RateLimit-Remaining", "12"), time.Time{}, false},
		{"503 without retry-after", 503, h(), time.Time{}, false},
		{"500", 500, h("Retry-After", "5"), time.Time{}, false},
		{"garbage reset", 429, h("RateLimit-Reset", "soon"), now.Add(time.Minute), true},
	} {
		until, ok := rateLimited(tt.status, tt.h, now)
		if ok != tt.ok || !until.Equal(tt.until) {
			t.Errorf("%s: got %v %v", tt.name, until, ok)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", unix)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := Collectors()["github_pr"].Collect(t.Context(), fetcher(t), cfgJSON(t, map[string]any{"repo": "a/b", "number": 1, "api_url": srv.URL}), nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || !rl.Until.Equal(reset) || !strings.Contains(rl.Error(), "rate limited") {
		t.Fatalf("want RateLimitError, got %v", err)
	}
}

func TestFetcherErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, MaxBody+1))
	})
	mux.HandleFunc("/404", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"https://docs.github.com"}`))
	})
	mux.HandleFunc("/500", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("oops\n", 100)))
	})
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := fetcher(t)
	if _, err := f.get(t.Context(), srv.URL+"/big", nil, false); err == nil || !strings.Contains(err.Error(), "larger than 10 MiB") {
		t.Fatalf("size cap: %v", err)
	}
	var he *HTTPError
	if _, err := f.get(t.Context(), srv.URL+"/404", nil, false); !errors.As(err, &he) || he.Status != 404 || !strings.HasSuffix(err.Error(), "HTTP 404: Not Found") {
		t.Fatalf("404: %v", err)
	}
	if _, err := f.get(t.Context(), srv.URL+"/500", nil, false); !errors.As(err, &he) || len([]rune(he.Body)) != 201 || strings.Contains(he.Body, "\n") {
		t.Fatalf("500 snippet: %q", he.Body)
	}
	var v any
	if _, err := f.getJSON(t.Context(), srv.URL+"/bad", nil, false, &v); err == nil {
		t.Fatal("bad JSON accepted")
	}
	// Plain http to a host that is not allowlisted is refused before any request.
	if _, err := f.get(t.Context(), "http://example.test/x", nil, false); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("http scheme: %v", err)
	}
	if _, err := f.get(t.Context(), "http://127.0.0.1:1/x", nil, false); err == nil {
		t.Fatal("connection refused not reported")
	}
	if _, err := f.get(t.Context(), "://", nil, false); err == nil {
		t.Fatal("bad URL accepted")
	}
	// A PR without a head sha cannot be followed further.
	noSHA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"number":1}`)) }))
	defer noSHA.Close()
	if _, err := Collectors()["github_pr"].Collect(t.Context(), f, cfgJSON(t, map[string]any{"repo": "a/b", "number": 1, "api_url": noSHA.URL}), nil); err == nil {
		t.Fatal("PR without head accepted")
	}
}

func ociCfg(srv *httptest.Server) map[string]any {
	return map[string]any{"registry": strings.TrimPrefix(srv.URL, "http://"), "repository": "library/postgres"}
}

// 001-AC12: anonymous Bearer token dance, token endpoint behind a login, and a Basic-only registry.
func TestOCIAuth(t *testing.T) {
	c := Collectors()["oci_tags"]
	cred := &Credential{Kind: "registry_basic", Username: "robot", Secret: "hunter2-long"}
	t.Run("anonymous bearer", func(t *testing.T) {
		up := testupstream.New()
		srv := httptest.NewServer(up)
		defer srv.Close()
		raw, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), nil)
		if err != nil {
			t.Fatal(err)
		}
		var s TagState
		_ = json.Unmarshal(raw, &s)
		if s.Count != 12 || up.Hits("/token") != 1 {
			t.Fatalf("state %+v, token fetches %d (one per run, reused across pages)", s, up.Hits("/token"))
		}
	})
	t.Run("token endpoint needs the credential", func(t *testing.T) {
		up := testupstream.New()
		up.RegistryUser, up.RegistryPass = "robot", "hunter2-long"
		srv := httptest.NewServer(up)
		defer srv.Close()
		if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), nil); err == nil || !strings.Contains(err.Error(), "registry token") {
			t.Fatalf("anonymous on private registry: %v", err)
		}
		if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), cred); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("basic challenge", func(t *testing.T) {
		up := testupstream.New()
		up.RegistryBasic, up.RegistryUser, up.RegistryPass = true, "robot", "hunter2-long"
		srv := httptest.NewServer(up)
		defer srv.Close()
		if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), nil); err == nil || !strings.Contains(err.Error(), "requires a login") {
			t.Fatalf("anonymous basic: %v", err)
		}
		if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), cred); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOCIEdgeCases(t *testing.T) {
	c := Collectors()["oci_tags"]
	serve := func(h http.HandlerFunc) *httptest.Server { return httptest.NewServer(h) }
	for _, tt := range []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"endless pages", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", `</v2/library/postgres/tags/list?last=x&n=1000>; rel="next"`)
			_, _ = w.Write([]byte(`{"tags":["a"]}`))
		}, "more than 20 pages"},
		{"cross-host next", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", `<https://elsewhere.test/v2/x/tags/list?last=a>; rel="next"`)
			_, _ = w.Write([]byte(`{"tags":["a"]}`))
		}, "left the registry"},
		{"too many tags", func(w http.ResponseWriter, _ *http.Request) {
			tags := make([]string, maxTags+1)
			for i := range tags {
				tags[i] = fmt.Sprint(i)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
		}, "more than 20000 tags"},
		{"unsupported challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Negotiate`)
			w.WriteHeader(http.StatusUnauthorized)
		}, "unsupported challenge"},
		{"bad realm", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer service="x"`)
			w.WriteHeader(http.StatusUnauthorized)
		}, "realm"},
		{"bad tags body", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) }, "decode tags"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := serve(tt.h)
			defer srv.Close()
			if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), nil); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want %q, got %v", tt.want, err)
			}
		})
	}
	// A token endpoint answering without a token, and a scope-less challenge defaulting to pull.
	var mux *http.ServeMux
	var scope string
	mux = http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		scope = r.URL.Query().Get("scope")
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, ociCfg(srv)), nil); err == nil || !strings.Contains(err.Error(), "no token") || scope != "repository:library/postgres:pull" {
		t.Fatalf("empty token: %v (scope %q)", err, scope)
	}
}

func TestRegistryBase(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io":       "https://registry-1.docker.io",
		"ghcr.io":         "https://ghcr.io",
		"127.0.0.1:5000":  "http://127.0.0.1:5000",
		"registry.test:1": "https://registry.test:1",
	} {
		if got := registryBase(in, []string{"127.0.0.1"}); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	if _, err := nextLink("https://r.test/v2/x/tags/list", `<%zz>; rel="next"`); err == nil {
		t.Fatal("bad link accepted")
	}
	got, _ := nextLink("https://r.test/v2/x/tags/list?n=1", `</v2/x/tags/list?last=b&n=1>; rel="next"`)
	if u, _ := url.Parse(got); u.Query().Get("last") != "b" || u.Host != "r.test" {
		t.Fatal(got)
	}
}

// Redirect targets count as contacted hosts, also when the guard refuses them.
func TestFetcherRecordsRedirectHosts(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`[]`)) }))
	defer target.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	}))
	defer hop.Close()
	f := fetcher(t)
	if _, err := f.get(t.Context(), hop.URL, nil, false); err != nil {
		t.Fatal(err)
	}
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	if strings.Join(f.Hosts, ",") != host(hop)+","+host(target) {
		t.Fatalf("hosts %v", f.Hosts)
	}
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://api.github.com/x", http.StatusFound)
	}))
	defer refused.Close()
	g := fetcher(t)
	g.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("no") }
	if _, err := g.get(t.Context(), refused.URL, nil, false); err == nil || !slices.Contains(g.Hosts, "api.github.com") {
		t.Fatalf("refused redirect host not recorded: %v %v", err, g.Hosts)
	}
	// A client without a policy keeps net/http's 10-redirect limit.
	loop := httptest.NewServer(nil)
	loop.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loop.URL, http.StatusFound) })
	defer loop.Close()
	h := fetcher(t)
	h.Client.CheckRedirect = nil
	if _, err := h.get(t.Context(), loop.URL, nil, false); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("default policy: %v", err)
	}
}

// The Fetcher keeps the guarded client's redirect policy: a cross-host hop drops credentials and an
// http:// target off the allowlist is refused.
func TestFetcherKeepsGuardRedirectPolicy(t *testing.T) {
	g, err := netguard.New([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"},
		Client: g.Client(netguard.Options{Timeout: 5 * time.Second, MaxRedirects: 3, HTTPHosts: []string{"127.0.0.1"},
			StripOnCrossHost: []string{"Authorization", "Private-Token"}})}
	var auth, token string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, token = r.Header.Get("Authorization"), r.Header.Get("Private-Token")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer target.Close()
	redirect := func(to string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }))
	}
	cross := redirect(target.URL + "/x")
	defer cross.Close()
	hdr := http.Header{"Authorization": {"Bearer s3cret"}, "Private-Token": {"glpat-s3cret"}}
	if _, err := f.get(t.Context(), cross.URL, hdr, false); err != nil || auth != "" || token != "" {
		t.Fatalf("cross-host redirect kept credentials: %v %q %q", err, auth, token)
	}
	down := redirect("http://example.test/x")
	defer down.Close()
	if _, err := f.get(t.Context(), down.URL, nil, false); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("http redirect off the allowlist: %v", err)
	}
}
