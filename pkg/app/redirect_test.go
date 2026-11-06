package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gedour/gedour/pkg/collect"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/netguard"
)

// hostRecorder is the host a key never named: it keeps what reached it.
type hostRecorder struct {
	mu   sync.Mutex
	seen []*http.Request
}

func (h *hostRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, r)
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/token") {
		_, _ = w.Write([]byte(`{"token":"t"}`))
		return
	}
	_, _ = w.Write([]byte(`[]`))
}

// 007-AC36: a known host may send the worker elsewhere, with the worker's real client. The other host reads only the path the known host chose, with no Referer, and a credential only at a registry's token host, on a watch that has one. A key's own watch has none (007-AC34), and one that has it cannot be retargeted (007-AC35).
func TestWorkerRedirectExposesNoCredential(t *testing.T) {
	const secret = "s3cret-token-value"
	cred := &collect.Credential{Kind: "t", Username: "robot", Secret: secret}
	guard, err := netguard.New([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	client := workerClient(config.Config{AllowHTTPHosts: []string{"127.0.0.1"}}, guard)
	for _, tt := range []struct {
		name, kind string
		cfg        func(known string) map[string]any
		cred       *collect.Credential
		// preserve makes the known host redirect the path it was asked, as docker.io does; wantURI is what the other host then reads.
		preserve bool
		wantURI  string
		// challenge makes the known host answer the registry's 401 instead of a redirect.
		challenge   bool
		sentByKnown string
		// wantBasic: the registry's own challenge names the token host, so a watch's Basic credential goes there by design.
		wantBasic bool
	}{
		{name: "github, with a credential", kind: "github_repo", cred: cred, sentByKnown: "Authorization",
			cfg: func(k string) map[string]any { return map[string]any{"repo": "a/b", "api_url": k} }},
		{name: "gitlab, with a credential", kind: "gitlab_release", cred: cred, sentByKnown: "Private-Token",
			cfg: func(k string) map[string]any { return map[string]any{"project": "a/b", "base_url": k} }},
		{name: "github, a key's watch", kind: "github_repo",
			cfg: func(k string) map[string]any { return map[string]any{"repo": "a/b", "api_url": k} }},
		{name: "gitlab, a key's watch", kind: "gitlab_release",
			cfg: func(k string) map[string]any { return map[string]any{"project": "a/b", "base_url": k} }},
		// Why a key gets no path on a known host (007-AC36): it would reach the other host.
		{name: "path-preserving host, a path in the config", kind: "github_repo", preserve: true, wantURI: "/zzDATA/repos/a/b/releases?per_page=50",
			cfg: func(k string) map[string]any { return map[string]any{"repo": "a/b", "api_url": k + "/zzDATA"} }},
		// What stays open: the key's repo still names part of the path, on the host the known host chooses.
		{name: "path-preserving host, a key's watch", kind: "github_repo", preserve: true, wantURI: "/repos/a/b/releases?per_page=50",
			cfg: func(k string) map[string]any { return map[string]any{"repo": "a/b", "api_url": k} }},
		{name: "path-preserving host, with a credential", kind: "gitlab_release", cred: cred, sentByKnown: "Private-Token", preserve: true, wantURI: "/api/v4/projects/a%2Fb/releases?per_page=50",
			cfg: func(k string) map[string]any { return map[string]any{"project": "a/b", "base_url": k} }},
		{name: "registry token realm, a key's watch", kind: "oci_tags", challenge: true,
			cfg: func(k string) map[string]any {
				return map[string]any{"repository": "a/b", "registry": strings.TrimPrefix(k, "http://")}
			}},
		{name: "registry token realm, with a credential", kind: "oci_tags", challenge: true, cred: cred, wantBasic: true,
			cfg: func(k string) map[string]any {
				return map[string]any{"repository": "a/b", "registry": strings.TrimPrefix(k, "http://")}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			other := &hostRecorder{}
			otherSrv := httptest.NewServer(other)
			defer otherSrv.Close()
			var mu sync.Mutex
			var knownSaw http.Header
			known := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if tt.challenge && r.Header.Get("Authorization") == "" {
					w.Header().Set("Www-Authenticate", `Bearer realm="`+otherSrv.URL+`/token",service="s"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if tt.challenge {
					_, _ = w.Write([]byte(`{"tags":[]}`))
					return
				}
				knownSaw = r.Header.Clone()
				to := "/chosen-by-the-known-host"
				if tt.preserve {
					to = r.URL.RequestURI()
				}
				http.Redirect(w, r, otherSrv.URL+to, http.StatusFound) //nolint:gosec // test upstream
			}))
			defer known.Close()

			raw, err := json.Marshal(tt.cfg(known.URL))
			if err != nil {
				t.Fatal(err)
			}
			f := &collect.Fetcher{Client: client, UserAgent: "gedour/test", HTTPHosts: []string{"127.0.0.1"}}
			if _, err := collect.Collectors()[tt.kind].Collect(t.Context(), f, raw, tt.cred); err != nil {
				t.Fatal(err)
			}
			other.mu.Lock()
			defer other.mu.Unlock()
			if len(other.seen) == 0 {
				t.Fatal("the worker did not follow: this test measures nothing")
			}
			if tt.sentByKnown != "" && knownSaw.Get(tt.sentByKnown) == "" {
				t.Fatalf("the known host got no %s: this test measures nothing", tt.sentByKnown)
			}
			for _, r := range other.seen {
				auth := r.Header.Get("Authorization")
				if tt.wantBasic != strings.HasPrefix(auth, "Basic ") || (!tt.wantBasic && auth != "") || r.Header.Get("Private-Token") != "" {
					t.Errorf("credential at the other host, want Basic %v: %v", tt.wantBasic, r.Header)
				}
				if r.Header.Get("Referer") != "" {
					t.Errorf("Referer at the other host: %v", r.Header.Get("Referer"))
				}
				if tt.challenge && r.URL.Query().Get("scope") != "repository:a/b:pull" {
					t.Errorf("token host read scope %q", r.URL.Query().Get("scope"))
				}
				dump, _ := json.Marshal(r.Header)
				if strings.Contains(string(dump)+r.URL.String(), secret) {
					t.Errorf("the secret reached the other host: %s %s", r.URL, dump)
				}
				want := tt.wantURI
				if want == "" {
					want = "/chosen-by-the-known-host"
				}
				if !tt.challenge && r.URL.RequestURI() != want {
					t.Errorf("other host read %s, want %s", r.URL.RequestURI(), want)
				}
			}
			if !strings.Contains(strings.Join(f.Hosts, " "), strings.TrimPrefix(otherSrv.URL, "http://")) {
				t.Errorf("the run log misses the other host: %v", f.Hosts)
			}
		})
	}
}
