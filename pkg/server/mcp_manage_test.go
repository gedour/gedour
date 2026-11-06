package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/route"
)

var manageTools = []string{"get_filter_reference", "list_channels", "create_watch", "update_watch", "create_rule", "update_rule"}

// manage calls a tool with a fresh key: the burst of 6 is per key, and most of these tests are not about it.
func (m *mcpApp) manage(t *testing.T, u apiUser, name string, args map[string]any) toolResult {
	t.Helper()
	key, _ := m.key(t, u.id, "manage", "")
	return m.tool(t, key, name, args, nil)
}

// asKey serves an operation on the REST handlers with a key's context and no schema in the way: what the handler alone refuses.
func (m *mcpApp) asKey(t *testing.T, u apiUser, method, path string, body any) (int, string) {
	t.Helper()
	_, keyID := m.key(t, u.id, "manage", "")
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("t", "1")
	cfg.CreateHooks = nil
	testWatches(t, m.pool).Register(humago.New(mux, cfg), auth.UserID)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx := audit.WithAPIKey(auth.WithKeyPrincipal(t.Context(), auth.KeyPrincipal{User: auth.User{ID: u.id}}), keyID)
	req := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (m *mcpApp) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	m.scan(t, sql, args, &n)
	return n
}

func (m *mcpApp) watchConfig(t *testing.T, id string) map[string]any {
	t.Helper()
	var cfg map[string]any
	m.scan(t, `SELECT config FROM watches WHERE id = $1`, []any{id}, &cfg)
	return cfg
}

func (m *mcpApp) credential(t *testing.T, u apiUser, kind string) string {
	t.Helper()
	var c credJSON
	body := map[string]any{"name": kind + " " + u.id, "kind": kind, "secret": "ghp_0123456789abcdef"}
	if kind == "registry_basic" {
		body["username"] = "robot"
	}
	m.mustCall(t, u, http.MethodPost, "/api/v1/credentials", body, http.StatusCreated, &c)
	return c.ID
}

// walkNames lists every key of a JSON value, at any depth, with what each property holds.
func walkNames(v any, visit func(key string, val any)) {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			visit(k, e)
			walkNames(e, visit)
		}
	case []any:
		for _, e := range v {
			walkNames(e, visit)
		}
	}
}

// 007-AC23, 007-AC25, 007-AC39: each scope's tools in the table's order; no credential, url or host input anywhere, config only where it must be; no delete, share or credential tool at any scope.
func TestMCPManageCatalog(t *testing.T) {
	m := newMCPApp(t)
	alice := m.user(t, "alice")
	for _, tt := range []struct {
		scope string
		want  []string
	}{
		{"read", readTools},
		{"write", slices.Concat(readTools, writeTools)},
		{"manage", slices.Concat(readTools, writeTools, manageTools)},
	} {
		t.Run(tt.scope, func(t *testing.T) {
			key, _ := m.key(t, alice.id, tt.scope, "")
			list, err := m.sdkClient(t, key, modernProtocol).ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
				for _, bad := range []string{"delete", "share", "credential", "script", "preview", "key"} {
					if strings.Contains(tool.Name, bad) {
						t.Errorf("%s exists", tool.Name)
					}
				}
				raw, err := json.Marshal(tool.InputSchema)
				if err != nil {
					t.Fatal(err)
				}
				var schema map[string]any
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
				props, _ := schema["properties"].(map[string]any)
				_, hasConfig := props["config"]
				if wantConfig := tool.Name == "create_watch" || tool.Name == "update_watch"; hasConfig != wantConfig {
					t.Errorf("%s: config %v", tool.Name, hasConfig)
				}
				walkNames(props, func(name string, _ any) {
					lower := strings.ToLower(name)
					if strings.Contains(lower, "url") || strings.Contains(lower, "host") || lower == "credential_id" {
						t.Errorf("%s: input %q", tool.Name, name)
					}
				})
			}
			if !slices.Equal(names, tt.want) || list.CacheScope != "private" {
				t.Fatalf("%v %q", names, list.CacheScope)
			}
			if tt.scope != "manage" {
				return
			}
			for _, tool := range list.Tools {
				if !slices.Contains(manageTools, tool.Name) {
					continue
				}
				a := tool.Annotations
				mutation := !slices.Contains(manageTools[:2], tool.Name)
				if a.ReadOnlyHint == mutation {
					t.Errorf("%s: readOnly %v", tool.Name, a.ReadOnlyHint)
				}
				if (a.DestructiveHint == nil) != mutation || (a.DestructiveHint != nil && *a.DestructiveHint) {
					t.Errorf("%s: destructive %v", tool.Name, a.DestructiveHint)
				}
				if *a.OpenWorldHint != (tool.Name == "create_watch" || tool.Name == "update_watch") {
					t.Errorf("%s: openWorld %v", tool.Name, *a.OpenWorldHint)
				}
			}
		})
	}
	// 007-AC23: above scope, -32602 and nothing changes.
	var w watchJSON
	m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	writeKey, _ := m.key(t, alice.id, "write", "")
	for name, args := range map[string]map[string]any{
		"get_filter_reference": {},
		"list_channels":        {},
		"create_watch":         sampleWatches[0],
		"update_watch":         {"id": w.ID, "paused": true},
		"create_rule":          {"name": "r"},
		"update_rule":          {"id": w.ID, "enabled": false},
	} {
		code, r := m.rpc(t, writeKey, "tools/call", map[string]any{"name": name, "arguments": args}, nil)
		if code != http.StatusOK || r.Error == nil || r.Error.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("%s: %d %+v", name, code, r.Error)
		}
	}
	if n := m.count(t, `SELECT (SELECT count(*) FROM watches) + (SELECT count(*) FROM rules) + (SELECT count(*) FROM watches WHERE paused)`); n != 1 {
		t.Fatalf("a write key changed something: %d", n)
	}
}

// 007-AC34: a key attaches no credential, even its owner's, through the tool and through the handler alone; a session still does.
func TestMCPManageCredentialRefused(t *testing.T) {
	m := newMCPApp(t)
	alice := m.user(t, "alice")
	cred := m.credential(t, alice, "github_token")
	var w watchJSON
	m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	withCred := map[string]any{"name": "w", "kind": "github_repo", "config": map[string]any{"repo": "a/b"}, "credential_id": cred}
	before := m.count(t, `SELECT count(*) FROM watches`)
	attached := func() int { return m.count(t, `SELECT count(*) FROM watches WHERE credential_id IS NOT NULL`) }

	if res := m.manage(t, alice, "create_watch", withCred); !res.IsError || !strings.Contains(res.text(), "credential_id") {
		t.Fatalf("create: %+v", res)
	}
	if res := m.manage(t, alice, "update_watch", map[string]any{"id": w.ID, "credential_id": cred}); !res.IsError || !strings.Contains(res.text(), "credential_id") {
		t.Fatalf("update: %+v", res)
	}
	for _, tt := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"create", http.MethodPost, "/api/v1/watches", withCred},
		{"attach", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"credential_id": cred}},
		{"detach", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"credential_id": ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if code, body := m.asKey(t, alice, tt.method, tt.path, tt.body); code != http.StatusForbidden || !strings.Contains(body, "credential") {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
	if m.count(t, `SELECT count(*) FROM watches`) != before || attached() != 0 {
		t.Fatal("a refused call changed a watch")
	}
	// The same calls on a session.
	m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", withCred, http.StatusCreated, nil)
	m.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"credential_id": cred}, http.StatusOK, nil)
	if attached() != 2 {
		t.Fatalf("%d attached on a session", attached())
	}
	// A key cannot detach the one a session attached.
	if code, _ := m.asKey(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"credential_id": ""}); code != http.StatusForbidden || attached() != 2 {
		t.Fatalf("detach %d, %d attached", code, attached())
	}
}

// 007-AC35: the owner's key cannot change the config of a watch that has a credential, on any target field or any other; name, schedule and paused work; a session passes.
func TestMCPManageCredentialConfig(t *testing.T) {
	m := newMCPApp(t)
	alice := m.user(t, "alice")
	for _, tt := range []struct {
		field, kind, credKind string
		cfg                   map[string]any
		moved                 string
	}{
		{"api_url", "github_repo", "github_token", map[string]any{"repo": "a/b"}, "https://gitlab.com"},
		{"base_url", "gitlab_release", "gitlab_token", map[string]any{"project": "a/b"}, "https://api.github.com"},
		{"registry", "oci_tags", "registry_basic", map[string]any{"repository": "a/b"}, "ghcr.io"},
	} {
		t.Run(tt.field, func(t *testing.T) {
			var w watchJSON
			m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", map[string]any{"name": "w " + tt.field, "kind": tt.kind, "config": tt.cfg, "credential_id": m.credential(t, alice, tt.credKind)}, http.StatusCreated, &w)
			stored := m.watchConfig(t, w.ID)
			moved := maps.Clone(tt.cfg)
			moved[tt.field] = tt.moved
			if res := m.manage(t, alice, "update_watch", map[string]any{"id": w.ID, "config": moved}); !res.IsError || !strings.Contains(res.text(), "has a credential") {
				t.Fatalf("%+v", res)
			}
			// The owner's token reads what the config names: a same-host move is a retarget too.
			other := maps.Clone(stored)
			for k := range tt.cfg {
				other[k] = "c/d"
			}
			if res := m.manage(t, alice, "update_watch", map[string]any{"id": w.ID, "config": other}); !res.IsError || !strings.Contains(res.text(), "has a credential") {
				t.Fatalf("%+v", res)
			}
			if got := m.watchConfig(t, w.ID); !reflect.DeepEqual(got, stored) {
				t.Fatalf("%v", got)
			}
			for _, edit := range []map[string]any{{"name": "renamed " + tt.field}, {"schedule": "2h"}, {"paused": true}, {"config": stored}} {
				edit["id"] = w.ID
				if res := m.manage(t, alice, "update_watch", edit); res.IsError {
					t.Fatalf("%v: %+v", edit, res)
				}
			}
			var got watchJSON
			m.mustCall(t, alice, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusOK, &got)
			if got.Name != "renamed "+tt.field || got.Schedule != "2h" || !got.Paused || got.CredentialID == nil {
				t.Fatalf("%+v", got)
			}
			m.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"config": moved}, http.StatusOK, nil)
			if got := m.watchConfig(t, w.ID); got[tt.field] != tt.moved {
				t.Fatalf("a session did not move it: %v", got)
			}
		})
	}
}

// hostCase passes with no reason; else its refusal must say that reason, as the validator and the known-host check locate the same field.
type hostCase struct{ name, value, reason string }

const (
	reasonKnown    = "known host"
	reasonQuery    = "must not have a query or fragment"
	reasonRegistry = "expected a registry host"
)

// registryStored is what the worker reads: registryBase maps only a bare docker.io to Docker Hub's API host.
var registryStored = map[string]string{"docker.io:443": "docker.io", "index.docker.io:443": "docker.io", "registry-1.docker.io:443": "docker.io", "doc\u212Aer.io": "docker.io", "index.docker.io": "docker.io", "registry-1.docker.io": "docker.io"}

// Saved below for alice, bob and carol. A refused value is refused on the exact string, never on a suffix or prefix. {default} is the field's kind's own default host.
var urlHostCases = []hostCase{
	{"unknown", "https://evil.example", reasonKnown},
	{"own default", "https://{default}", ""},
	{"docker.io on a url kind", "https://docker.io", reasonKnown},
	{"ghcr.io on a url kind", "https://ghcr.io", reasonKnown},
	{"owner's watch", "https://forge.alice.example", ""},
	{"owner's host with a path not saved", "https://forge.alice.example/api/v3", reasonKnown},
	{"owner's saved path", "https://ghe.alice.example/api/v3", ""},
	{"owner's saved path, trailing slash", "https://ghe.alice.example/api/v3/", ""},
	{"owner's saved path, other host case", "https://GHE.alice.example:443/api/v3", ""},
	{"owner's saved path, other case", "https://ghe.alice.example/API/v3", reasonKnown},
	{"owner's saved path, encoded", "https://ghe.alice.example/%61pi/v3", reasonKnown},
	{"owner's saved path, cut", "https://ghe.alice.example/api", reasonKnown},
	{"owner's saved path, longer", "https://ghe.alice.example/api/v3/DATA", reasonKnown},
	{"owner's saved host, no path", "https://ghe.alice.example", reasonKnown},
	{"default host, trailing slash", "https://{default}/", ""},
	{"default host with a path", "https://{default}/DATA", reasonKnown},
	{"docker.io with a path", "https://docker.io/zzDATA", reasonKnown},
	{"ghcr.io with a path", "https://ghcr.io/zzDATA", reasonKnown},
	{"empty query", "https://{default}?", reasonQuery},
	{"empty fragment", "https://{default}#", reasonQuery},
	{"path then empty query", "https://{default}/DATA?", reasonQuery},
	{"saved path then empty fragment", "https://ghe.alice.example/api/v3#", reasonQuery},
	{"owner's registry host", "https://reg.alice.example:5000", ""},
	{"another user's watch", "https://bob-only.example", reasonKnown},
	{"a shared watch", "https://shared-only.example", reasonKnown},
	{"uppercase", "https://{DEFAULT}", ""},
	{"trailing dot", "https://{default}.", ""},
	{"default port", "https://{default}:443", ""},
	{"trailing dot and default port", "https://{default}.:443", ""},
	{"uppercase owner host", "https://FORGE.alice.example:443/", ""},
	{"other port", "https://{default}:8443", reasonKnown},
	{"owner host on another port", "https://forge.alice.example:8443", reasonKnown},
	{"two trailing dots", "https://{default}..", reasonKnown},
	{"suffix shape", "https://{default}.evil.example", reasonKnown},
	{"prefix shape", "https://evil-{default}", reasonKnown},
	{"joined prefix", "https://x{default}", reasonKnown},
	{"subdomain of a known host", "https://evil.{default}", reasonKnown},
	{"userinfo", "https://{default}@evil.example", "must not carry credentials"},
	{"host in the path", "https://evil.example/{default}", reasonKnown},
	{"host in the query", "https://evil.example?x={default}", reasonQuery},
	{"host in the fragment", "https://evil.example#{default}", reasonQuery},
	{"plain http", "http://{default}", "must use https"},
	{"cyrillic homoglyph", "https://g\u0456tlab.com", reasonKnown},
	{"dotted capital i, folds to gitlab.com", "https://g\u0130tlab.com", reasonKnown},
	{"dotted capital i, folds to api.github.com", "https://api.g\u0130thub.com", reasonKnown},
	{"kelvin sign, folds to docker.io", "https://doc\u212Aer.io", reasonKnown},
	{"fullwidth stop", "https://gitlab\u3002com", reasonKnown},
	{"fullwidth letters", "https://\uFF47itlab.com", reasonKnown},
	{"percent-encoded homoglyph", "https://g%D1%96tlab.com", reasonKnown},
	{"punycode of a known host's homoglyph", "https://xn--gtlab-9oc.com", reasonKnown},
	{"owner's unicode host as saved", "https://b\u00FCcher.alice.example", ""},
	{"its punycode, not saved", "https://xn--bcher-kva.alice.example", reasonKnown},
	{"owner's punycode host as saved", "https://xn--mnchen-3ya.alice.example", ""},
	{"its unicode, not saved", "https://m\u00FCnchen.alice.example", reasonKnown},
}

var registryHostCases = []hostCase{
	{"unknown", "evil.example", reasonKnown},
	{"absent means docker.io", "", ""},
	{"default docker", "docker.io", ""},
	{"docker default port", "docker.io:443", ""},
	{"docker alias default port", "index.docker.io:443", ""},
	{"docker api host default port", "registry-1.docker.io:443", ""},
	{"docker other port", "docker.io:8443", reasonKnown},
	{"docker alias", "registry-1.docker.io", ""},
	{"default ghcr", "ghcr.io", ""},
	{"owner's registry", "reg.alice.example:5000", ""},
	{"owner's api host", "forge.alice.example", ""},
	{"owner's host saved with a path", "ghe.alice.example", reasonKnown},
	{"owner's registry without its port", "reg.alice.example", reasonKnown},
	{"owner's registry on another port", "reg.alice.example:5001", reasonKnown},
	{"another user's watch", "bob-only.example", reasonKnown},
	{"a shared watch", "shared-only.example", reasonKnown},
	{"uppercase", "GHCR.IO", ""},
	{"default port", "ghcr.io:443", ""},
	{"other port", "ghcr.io:8443", reasonKnown},
	{"trailing dot, the validator refuses", "ghcr.io.", reasonRegistry},
	{"suffix shape", "ghcr.io.evil.example", reasonKnown},
	{"prefix shape", "evil-ghcr.io", reasonKnown},
	{"userinfo", "ghcr.io@evil.example", reasonRegistry},
	{"scheme", "https://ghcr.io", reasonRegistry},
	{"cyrillic homoglyph", "ghcr.\u0456o", reasonRegistry},
	// The validator folds it before saving, so the worker reads plain ASCII: docker.io, not a homoglyph.
	{"kelvin sign, folded by the validator", "doc\u212Aer.io", ""},
	{"github default", "api.github.com", reasonKnown},
	{"gitlab default", "gitlab.com", reasonKnown},
	{"github default folded by the validator", "api.g\u0130thub.com", reasonKnown},
}

// 007-AC36: unknown, default, the owner's, another user's and a shared watch's host, and each normalization and bypass shape, on each field, on create and on update; a session passes.
func TestMCPManageKnownHosts(t *testing.T) {
	m := newMCPApp(t)
	alice, bob, carol := m.user(t, "alice"), m.user(t, "bob"), m.user(t, "carol")
	seed := func(u apiUser, kind string, cfg map[string]any) string {
		var w watchJSON
		m.mustCall(t, u, http.MethodPost, "/api/v1/watches", newWatch("seed", kind, cfg), http.StatusCreated, &w)
		return w.ID
	}
	// Every default-host watch saves its default: these must not open it to another kind.
	for _, w := range sampleWatches {
		seed(alice, w["kind"].(string), w["config"].(map[string]any))
	}
	seed(alice, "oci_tags", map[string]any{"repository": "a/b", "registry": "ghcr.io"})
	seed(alice, "github_repo", map[string]any{"repo": "a/b", "api_url": "https://forge.alice.example"})
	seed(alice, "github_repo", map[string]any{"repo": "a/b", "api_url": "https://ghe.alice.example/api/v3"})
	seed(alice, "oci_tags", map[string]any{"repository": "a/b", "registry": "reg.alice.example:5000"})
	seed(alice, "gitlab_release", map[string]any{"project": "a/b", "base_url": "https://b\u00FCcher.alice.example"})
	seed(alice, "gitlab_release", map[string]any{"project": "a/b", "base_url": "https://xn--mnchen-3ya.alice.example"})
	seed(bob, "github_repo", map[string]any{"repo": "a/b", "api_url": "https://bob-only.example"})
	seed(bob, "oci_tags", map[string]any{"repository": "a/b", "registry": "bob-only.example"})
	var shared string
	for _, tt := range []struct {
		kind string
		cfg  map[string]any
	}{
		{"github_repo", map[string]any{"repo": "a/b", "api_url": "https://shared-only.example"}},
		{"oci_tags", map[string]any{"repository": "a/b", "registry": "shared-only.example"}},
	} {
		id := seed(carol, tt.kind, tt.cfg)
		if tt.kind == "github_repo" {
			shared = id
		}
		m.mustCall(t, carol, http.MethodPost, "/api/v1/watches/"+id+"/shares", map[string]any{"email": alice.email, "permission": "rw"}, http.StatusCreated, nil)
	}
	seed(carol, "github_repo", map[string]any{"repo": "a/b", "api_url": "https://carol-only.example"})
	for _, f := range []struct {
		field, kind, def string
		base             map[string]any
		changed          map[string]any
		cases            []hostCase
	}{
		{"api_url", "github_repo", "api.github.com", map[string]any{"repo": "a/b"}, map[string]any{"repo": "c/d"},
			append(slices.Clone(urlHostCases), hostCase{"gitlab default", "https://gitlab.com", reasonKnown}, hostCase{"gitlab default, normalized", "https://GITLAB.com.:443/", reasonKnown})},
		{"api_url", "github_pr", "api.github.com", map[string]any{"repo": "a/b", "number": 1}, map[string]any{"repo": "c/d", "number": 1},
			[]hostCase{{"own default", "https://{default}", ""}, {"gitlab default", "https://gitlab.com", reasonKnown}, {"docker.io", "https://docker.io", reasonKnown}, {"ghcr.io", "https://ghcr.io", reasonKnown}}},
		{"base_url", "gitlab_release", "gitlab.com", map[string]any{"project": "a/b"}, map[string]any{"project": "c/d"},
			append(slices.Clone(urlHostCases), hostCase{"github default", "https://api.github.com", reasonKnown}, hostCase{"github default, normalized", "https://API.github.com.:443/", reasonKnown})},
		{"base_url", "gitlab_mr", "gitlab.com", map[string]any{"project": "a/b", "iid": 1}, map[string]any{"project": "c/d", "iid": 1},
			[]hostCase{{"own default", "https://{default}", ""}, {"github default", "https://api.github.com", reasonKnown}, {"docker.io", "https://docker.io", reasonKnown}, {"ghcr.io", "https://ghcr.io", reasonKnown}}},
		{"registry", "oci_tags", "docker.io", map[string]any{"repository": "a/b"}, map[string]any{"repository": "c/d"}, registryHostCases},
	} {
		t.Run(f.kind, func(t *testing.T) {
			var target watchJSON
			m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", newWatch("target "+f.kind, f.kind, f.base), http.StatusCreated, &target)
			original := m.watchConfig(t, target.ID)
			owned := func() int { return m.count(t, `SELECT count(*) FROM watches WHERE owner_id = $1`, alice.id) }
			for _, c := range f.cases {
				t.Run(c.name, func(t *testing.T) {
					c.value = strings.NewReplacer("{default}", f.def, "{DEFAULT}", strings.ToUpper(f.def)).Replace(c.value)
					cfg := maps.Clone(f.base)
					if c.value != "" {
						cfg[f.field] = c.value
					}
					before := owned()
					ok := c.reason == ""
					res := m.manage(t, alice, "create_watch", newWatch("made", f.kind, cfg))
					if res.IsError == ok {
						t.Fatalf("create: %+v", res)
					}
					if !ok && (!strings.Contains(res.text(), c.reason) || !strings.Contains(res.text(), "body.config."+f.field) || owned() != before) {
						t.Fatalf("create refused for another reason than %q, or still created: %+v", c.reason, res)
					}
					want, checksStored := registryStored[c.value]
					if checksStored {
						if got := res.data(t).(map[string]any)["config"].(map[string]any)[f.field]; got != want {
							t.Fatalf("create stored %v, want %s", got, want)
						}
					}

					m.exec(t, `UPDATE watches SET config = $2 WHERE id = $1`, target.ID, original)
					moved := maps.Clone(f.changed)
					if c.value != "" {
						moved[f.field] = c.value
					}
					res = m.manage(t, alice, "update_watch", map[string]any{"id": target.ID, "config": moved})
					if res.IsError == ok {
						t.Fatalf("update: %+v", res)
					}
					got := m.watchConfig(t, target.ID)
					if ok == reflect.DeepEqual(got, original) {
						t.Fatalf("update ok %v, config %v", ok, got)
					}
					if !ok && (!strings.Contains(res.text(), c.reason) || !strings.Contains(res.text(), "body.config."+f.field)) {
						t.Fatalf("update refused for another reason than %q: %+v", c.reason, res)
					}
					if checksStored && got[f.field] != want {
						t.Fatalf("update stored %v, want %s", got[f.field], want)
					}
				})
			}
			// The same refused calls on a session, on the same watch: REST has no host rule.
			unknown := "https://evil.example"
			if f.field == "registry" {
				unknown = "evil.example"
			}
			moved := maps.Clone(f.changed)
			moved[f.field] = unknown
			m.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+target.ID, map[string]any{"config": moved}, http.StatusOK, nil)
			m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", newWatch("session", f.kind, moved), http.StatusCreated, nil)
			// The handler alone refuses too.
			m.exec(t, `DELETE FROM watches WHERE config->>$1 = $2`, f.field, unknown)
			if code, body := m.asKey(t, alice, http.MethodPost, "/api/v1/watches", newWatch("direct", f.kind, moved)); code != http.StatusForbidden || !strings.Contains(body, "body.config."+f.field) {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
	// A sharee's key is judged on the key owner's hosts, never the watch owner's, and only when the config moves.
	t.Run("a sharee's key", func(t *testing.T) {
		stored := m.watchConfig(t, shared)
		if res := m.manage(t, alice, "update_watch", map[string]any{"id": shared, "name": "renamed by alice"}); res.IsError {
			t.Fatalf("rename: %+v", res)
		}
		for name, url := range map[string]string{"carol's host of this watch": "https://shared-only.example", "a host only carol owns": "https://carol-only.example"} {
			res := m.manage(t, alice, "update_watch", map[string]any{"id": shared, "config": map[string]any{"repo": "c/d", "api_url": url}})
			if !res.IsError || !strings.Contains(res.text(), reasonKnown) || !strings.Contains(res.text(), "body.config.api_url") {
				t.Fatalf("%s: %+v", name, res)
			}
		}
		if got := m.watchConfig(t, shared); !reflect.DeepEqual(got, stored) {
			t.Fatalf("%v", got)
		}
		// Carol's key is on her own hosts, on the same watch.
		if res := m.manage(t, carol, "update_watch", map[string]any{"id": shared, "config": map[string]any{"repo": "c/d", "api_url": "https://carol-only.example"}}); res.IsError {
			t.Fatalf("the owner: %+v", res)
		}
	})
}

// 007-AC41: update_watch cannot walk past the rate-limit hold: schedule, unpause and retarget leave next_run_at alone on a key, and move it on a session or with no hold.
func TestMCPManageHold(t *testing.T) {
	m := newMCPApp(t)
	alice := m.user(t, "alice")
	var w watchJSON
	m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	hold := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	set := func(status string, at time.Time) {
		m.exec(t, `UPDATE watches SET last_status = $2, next_run_at = $3, paused = true, schedule = '1h' WHERE id = $1`, w.ID, status, at)
	}
	nextRun := func() time.Time {
		var at time.Time
		m.scan(t, `SELECT next_run_at FROM watches WHERE id = $1`, []any{w.ID}, &at)
		return at
	}
	edits := map[string]func(n int) map[string]any{
		"schedule": func(int) map[string]any { return map[string]any{"schedule": "3h"} },
		"unpause":  func(int) map[string]any { return map[string]any{"paused": false} },
		"retarget": func(n int) map[string]any {
			return map[string]any{"config": map[string]any{"repo": fmt.Sprintf("a/b%d", n)}}
		},
	}
	n := 0
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			n++
			set("rate_limited", hold)
			args := edit(n)
			args["id"] = w.ID
			res := m.manage(t, alice, "update_watch", args)
			if res.IsError || !nextRun().Equal(hold) {
				t.Fatalf("held: %+v, next %v, want %v", res, nextRun(), hold)
			}
			n++
			set("rate_limited", hold)
			m.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, edit(n), http.StatusOK, nil)
			if nextRun().After(time.Now()) {
				t.Fatalf("a session did not override: %v", nextRun())
			}
			// No hold to respect: another failure's backoff, or a hold already over.
			for _, tt := range []struct {
				status string
				at     time.Time
			}{{"error", hold}, {"rate_limited", time.Now().Add(-time.Minute).UTC().Truncate(time.Second)}} {
				n++
				set(tt.status, tt.at)
				args := edit(n)
				args["id"] = w.ID
				if res := m.manage(t, alice, "update_watch", args); res.IsError || nextRun().Equal(tt.at) || nextRun().After(time.Now()) {
					t.Fatalf("%s: %+v, next %v", tt.status, res, nextRun())
				}
			}
		})
	}
	// Name alone moved nothing before this rule, and still does not.
	set("error", hold)
	if res := m.manage(t, alice, "update_watch", map[string]any{"id": w.ID, "name": "renamed"}); res.IsError || !nextRun().Equal(hold) {
		t.Fatalf("%+v", res)
	}
}

// 007-AC37: create_rule and update_rule keep the REST checks.
func TestMCPManageRuleChecks(t *testing.T) {
	m := newMCPApp(t)
	alice, bob := m.user(t, "alice"), m.user(t, "bob")
	var bobHook, aliceHook struct {
		ID string `json:"id"`
	}
	hook := map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/a"}}
	m.mustCall(t, bob, http.MethodPost, "/api/v1/channels", hook, http.StatusCreated, &bobHook)
	m.mustCall(t, alice, http.MethodPost, "/api/v1/channels", hook, http.StatusCreated, &aliceHook)
	var bobWatch, readable watchJSON
	m.mustCall(t, bob, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &bobWatch)
	m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &readable)
	var mine, bobs struct {
		ID string `json:"id"`
	}
	m.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "mine"}, http.StatusCreated, &mine)
	m.mustCall(t, bob, http.MethodPost, "/api/v1/rules", map[string]any{"name": "bobs"}, http.StatusCreated, &bobs)
	for _, tt := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"another user's channel", map[string]any{"channel_ids": []string{bobHook.ID}}, "body.channel_ids[0]"},
		{"an unreadable watch", map[string]any{"watch_id": bobWatch.ID}, "body.watch_id"},
		{"a bad filter", map[string]any{"filter": `event.type ==`}, "body.filter"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := m.count(t, `SELECT count(*) FROM rules`)
			create := maps.Clone(tt.args)
			create["name"] = "made"
			if res := m.manage(t, alice, "create_rule", create); !res.IsError || !strings.Contains(res.text(), tt.want) {
				t.Fatalf("create: %+v", res)
			}
			update := maps.Clone(tt.args)
			update["id"] = mine.ID
			if res := m.manage(t, alice, "update_rule", update); !res.IsError || !strings.Contains(res.text(), tt.want) {
				t.Fatalf("update: %+v", res)
			}
			if m.count(t, `SELECT count(*) FROM rules`) != before {
				t.Fatal("created")
			}
		})
	}
	if res := m.manage(t, alice, "update_rule", map[string]any{"id": bobs.ID, "enabled": false}); !res.IsError || !strings.Contains(strings.ToLower(res.text()), "not found") {
		t.Fatalf("another user's rule: %+v", res)
	}
	ok := map[string]any{"name": "works", "watch_id": readable.ID, "channel_ids": []string{aliceHook.ID}, "filter": `event.type == "release.published"`, "priority": "high"}
	if res := m.manage(t, alice, "create_rule", ok); res.IsError {
		t.Fatalf("%+v", res)
	}
	m.exec(t, `INSERT INTO rules (owner_id, name) SELECT $1, 'filler ' || g FROM generate_series(1, $2::int - (SELECT count(*)::int FROM rules WHERE owner_id = $1)) g`, alice.id, route.MaxRules)
	if res := m.manage(t, alice, "create_rule", map[string]any{"name": "one too many"}); !res.IsError || !strings.Contains(res.text(), "delete one first") {
		t.Fatalf("MaxRules: %+v", res)
	}
}

// 007-AC38: nothing of a channel leaves but its id, name and type: the canary URL and last4 appear nowhere in the raw result.
func TestMCPManageListChannels(t *testing.T) {
	m := newMCPApp(t)
	alice := m.user(t, "alice")
	const canaryURL, last4 = "https://hook-canary.example/canary-path", "Q9Z7"
	var hook map[string]any
	m.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": canaryURL}, "secret": "a-long-signing-secret-" + last4}, http.StatusCreated, &hook)
	// Control: REST holds both, so their absence below comes from the shaper.
	if hook["last4"] != last4 {
		t.Fatalf("%v", hook)
	}
	key, _ := m.key(t, alice.id, "manage", "")
	resp, raw := m.post(t, key, rpcBody(t, "tools/call", map[string]any{"name": "list_channels", "arguments": map[string]any{}}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	for _, leak := range []string{"canary", last4, "config", "last4", "last_delivery", "created_at"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("%q in %s", leak, raw)
		}
	}
	res := m.tool(t, key, "list_channels", nil, nil)
	got, _ := res.data(t).(map[string]any)["items"].([]any)
	want := map[string]any{"id": hook["id"], "name": "Hook", "type": "webhook"}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("%v", got)
	}
}

// 007-AC39: each mutation's usual audit row with the key's ID, the watch cap on a key only, and the budget shared with run_watch.
func TestMCPManageAuditCapBudget(t *testing.T) {
	m := newMCPApp(t)
	alice := m.user(t, "alice")
	key, keyID := m.key(t, alice.id, "manage", "")
	idOf := func(res toolResult) string {
		t.Helper()
		if res.IsError {
			t.Fatalf("%+v", res)
		}
		id, _ := res.data(t).(map[string]any)["id"].(string)
		return id
	}
	w := idOf(m.tool(t, key, "create_watch", newWatch("made", "github_repo", map[string]any{"repo": "a/b"}), nil))
	idOf(m.tool(t, key, "update_watch", map[string]any{"id": w, "paused": true}, nil))
	r := idOf(m.tool(t, key, "create_rule", map[string]any{"name": "made"}, nil))
	idOf(m.tool(t, key, "update_rule", map[string]any{"id": r, "enabled": false}, nil))
	for action, target := range map[string]string{"watch.created": w, "watch.updated": w, "rule.created": r, "rule.updated": r} {
		if n := m.count(t, `SELECT count(*) FROM audit_log WHERE action = $1 AND target_id = $2 AND api_key_id = $3`, action, target, keyID); n != 1 {
			t.Errorf("%s: %d rows", action, n)
		}
	}
	// A burst mixed with run_watch: two more spend the 6, then each of the five refuses, and nothing ran.
	idOf(m.tool(t, key, "run_watch", map[string]any{"id": sessionWatch(t, m, alice)}, nil))
	idOf(m.tool(t, key, "update_rule", map[string]any{"id": r, "enabled": true}, nil))
	rows := m.count(t, `SELECT count(*) FROM audit_log`)
	for name, args := range map[string]map[string]any{
		"create_watch": newWatch("over", "github_repo", map[string]any{"repo": "a/b"}),
		"update_watch": {"id": w, "name": "over"},
		"create_rule":  {"name": "over"},
		"update_rule":  {"id": r, "name": "over"},
		"run_watch":    {"id": w},
	} {
		if res := m.tool(t, key, name, args, nil); !res.IsError || !strings.Contains(res.text(), "a burst of 6, then one every 10 s, per replica") {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	if m.count(t, `SELECT count(*) FROM audit_log`) != rows {
		t.Fatal("a refused call ran")
	}
	// Reads spend none of it.
	if res := m.tool(t, key, "list_channels", nil, nil); res.IsError {
		t.Fatalf("%+v", res)
	}

	m.exec(t, `INSERT INTO watches (owner_id, name, kind, config, schedule) SELECT $1, 'filler ' || g, 'github_repo', '{"repo":"a/b","api_url":"https://api.github.com"}', '1h'
		FROM generate_series(1, 200 - (SELECT count(*)::int FROM watches WHERE owner_id = $1)) g`, alice.id)
	over := newWatch("201st", "github_repo", map[string]any{"repo": "a/b"})
	if res := m.manage(t, alice, "create_watch", over); !res.IsError || !strings.Contains(res.text(), "200 watches") {
		t.Fatalf("201st: %+v", res)
	}
	if n := m.count(t, `SELECT count(*) FROM watches WHERE owner_id = $1`, alice.id); n != 200 {
		t.Fatalf("%d watches", n)
	}
	// REST has no cap, and an edit is no create.
	m.mustCall(t, alice, http.MethodPost, "/api/v1/watches", over, http.StatusCreated, nil)
	if res := m.manage(t, alice, "update_watch", map[string]any{"id": w, "name": "still editable"}); res.IsError {
		t.Fatalf("%+v", res)
	}
}

func sessionWatch(t *testing.T, m *mcpApp, u apiUser) string {
	t.Helper()
	var w watchJSON
	m.mustCall(t, u, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	return w.ID
}
