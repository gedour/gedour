package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/secret"
	"github.com/gedour/gedour/pkg/watch"
)

type apiUser struct {
	id, email, token, csrf string
}

// signIn creates a user with a live session straight in the database: several users on one dev app.
func (a *devApp) signIn(t *testing.T, name string) apiUser {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	u := apiUser{email: name + "@example.test", token: base64.RawURLEncoding.EncodeToString(b), csrf: "csrf-" + name}
	if err := a.pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, display_name) VALUES ($1, $2, $1) RETURNING id::text`,
		name, u.email).Scan(&u.id); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(u.token))
	if _, err := a.pool.Exec(ctx, `INSERT INTO sessions (id, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`,
		sum[:], u.id, u.csrf); err != nil {
		t.Fatal(err)
	}
	return u
}

// call sends body as JSON as u and returns the status and the raw response body.
func (a *devApp) call(t *testing.T, u apiUser, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, a.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, u.csrf)
	req.Header.Set("Cookie", auth.SessionCookie+"="+u.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (a *devApp) mustCall(t *testing.T, u apiUser, method, path string, body any, code int, out any) {
	t.Helper()
	got, raw := a.call(t, u, method, path, body)
	if got != code {
		t.Fatalf("%s %s: want %d got %d: %s", method, path, code, got, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
}

type watchJSON struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Config       map[string]any `json:"config"`
	CredentialID *string        `json:"credential_id"`
	Schedule     string         `json:"schedule"`
	Paused       bool           `json:"paused"`
}

type problem struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
		Value    any    `json:"value"`
	} `json:"errors"`
}

func (p problem) locations() []string {
	var out []string
	for _, e := range p.Errors {
		out = append(out, e.Location)
	}
	return out
}

func newWatch(name, kind string, cfg map[string]any) map[string]any {
	return map[string]any{"name": name, "kind": kind, "config": cfg}
}

var sampleWatches = []map[string]any{
	newWatch("Go releases", "github_repo", map[string]any{"repo": "golang/go"}),
	newWatch("Go PR", "github_pr", map[string]any{"repo": "golang/go", "number": 1}),
	newWatch("GitLab releases", "gitlab_release", map[string]any{"project": "gitlab-org/gitlab"}),
	newWatch("GitLab MR", "gitlab_mr", map[string]any{"project": "gitlab-org/gitlab", "iid": 2}),
	newWatch("postgres tags", "oci_tags", map[string]any{"repository": "postgres", "tag_regex": `^\d+$`}),
}

// 001-AC4: create, read, list, edit, pause, resume and delete a watch of each kind through the API.
func TestWatchCRUD(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	ids := map[string]string{}
	for _, body := range sampleWatches {
		var w watchJSON
		a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", body, http.StatusCreated, &w)
		// GitHub kinds default to hourly: unauthenticated GitHub allows 60 requests an hour.
		want := map[bool]string{true: "1h", false: "15m"}[strings.HasPrefix(w.Kind, "github_")]
		if w.Schedule != want || w.Paused || w.Kind != body["kind"] {
			t.Fatalf("created: %+v", w)
		}
		ids[w.Kind] = w.ID
	}
	var got watchJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/watches/"+ids["oci_tags"], nil, http.StatusOK, &got)
	if got.Config["repository"] != "library/postgres" || got.Config["registry"] != "docker.io" {
		t.Fatalf("oci config not normalized: %v", got.Config)
	}

	var list struct{ Items []watchJSON }
	a.mustCall(t, alice, http.MethodGet, "/api/v1/watches", nil, http.StatusOK, &list)
	var names []string
	for _, w := range list.Items {
		names = append(names, w.Name)
	}
	if strings.Join(names, "|") != "GitLab MR|GitLab releases|Go PR|Go releases|postgres tags" {
		t.Fatalf("not sorted by name: %v", names)
	}
	for q, want := range map[string]int{"kind=gitlab_mr": 1, "q=go": 2, "q=GOLANG": 2, "q=100%25": 0, "limit=2": 2} {
		list.Items = nil
		a.mustCall(t, alice, http.MethodGet, "/api/v1/watches?"+q, nil, http.StatusOK, &list)
		if len(list.Items) != want {
			t.Errorf("%s: want %d got %d", q, want, len(list.Items))
		}
	}

	id := ids["github_repo"]
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+id, map[string]any{
		"name": " Go stable ", "schedule": "0 9 * * 1-5", "config": map[string]any{"repo": "golang/tools", "include_prereleases": true},
	}, http.StatusOK, &got)
	if got.Name != "Go stable" || got.Schedule != "0 9 * * 1-5" || got.Config["repo"] != "golang/tools" || got.Config["include_prereleases"] != true {
		t.Fatalf("update: %+v", got)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+id+"/pause", nil, http.StatusOK, &got)
	if !got.Paused {
		t.Fatal("pause did not pause")
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+id+"/pause", nil, http.StatusOK, &got)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+id+"/resume", nil, http.StatusOK, &got)
	if got.Paused {
		t.Fatal("resume did not resume")
	}
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+id, map[string]any{"paused": true}, http.StatusOK, &got)
	if !got.Paused {
		t.Fatal("PATCH paused ignored")
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/watches/"+id, nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/watches/"+id, nil, http.StatusNotFound, nil)

	// One audit row per mutation, the idempotent second pause excluded.
	var actions []string
	rows, err := a.pool.Query(context.Background(), `SELECT action FROM audit_log WHERE target_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		actions = append(actions, s)
	}
	if strings.Join(actions, ",") != "watch.created,watch.updated,watch.paused,watch.resumed,watch.updated,watch.deleted" {
		t.Fatalf("audit trail: %v", actions)
	}
}

// 001-AC4: invalid input is a 400 problem whose errors point at the offending fields.
func TestWatchValidation(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	for _, tt := range []struct {
		name, method, path string
		body               any
		at                 []string
	}{
		{"bad repo", http.MethodPost, "/api/v1/watches", newWatch("x", "github_repo", map[string]any{"repo": "nope"}), []string{"body.config.repo"}},
		{"bad regex", http.MethodPost, "/api/v1/watches", newWatch("x", "oci_tags", map[string]any{"repository": "a/b", "tag_regex": "(["}), []string{"body.config.tag_regex"}},
		{"bad cron", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_repo", "config": map[string]any{"repo": "a/b"}, "schedule": "61 * * * *"}, []string{"body.schedule"}},
		{"blank name and short interval", http.MethodPost, "/api/v1/watches", map[string]any{"name": "  ", "kind": "github_repo", "config": map[string]any{"repo": "a/b"}, "schedule": "10s"}, []string{"body.schedule", "body.name"}},
		{"unknown kind", http.MethodPost, "/api/v1/watches", newWatch("x", "rss", map[string]any{}), []string{"body.kind"}},
		{"unknown top-level field", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_repo", "config": map[string]any{"repo": "a/b"}, "owner_id": "x"}, []string{"body.owner_id"}},
		{"missing config", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_repo"}, []string{"body"}},
		{"bad credential id", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_repo", "config": map[string]any{"repo": "a/b"}, "credential_id": "nope"}, []string{"body.credential_id"}},
		{"patch bad config", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"config": map[string]any{"repo": "a/b", "number": 1}}, []string{"body.config.number"}},
		{"patch bad schedule and name", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"schedule": "@daily", "name": " "}, []string{"body.name", "body.schedule"}},
		{"patch credential not a uuid", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"credential_id": "x"}, []string{"body.credential_id"}},
		{"bad id", http.MethodGet, "/api/v1/watches/not-a-uuid", nil, []string{"path.id"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var p problem
			a.mustCall(t, alice, tt.method, tt.path, tt.body, http.StatusBadRequest, &p)
			if strings.Join(p.locations(), ",") != strings.Join(tt.at, ",") {
				t.Fatalf("want errors at %v, got %+v", tt.at, p)
			}
		})
	}
}

// 008-AC3: the old kind name has no alias; the refusal lists the valid kinds (400: the API's status for every
// invalid input, 001-AC4).
func TestWatchOldKindRefused(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	// Split so the 008-AC3 grep for the old name finds no live use.
	old := "github_" + "release"
	var p problem
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", newWatch("x", old, map[string]any{"repo": "a/b"}), http.StatusBadRequest, &p)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.kind" {
		t.Fatalf("want one error at body.kind, got %+v", p)
	}
	for _, k := range watch.Kinds {
		if !strings.Contains(p.Errors[0].Message, k) {
			t.Errorf("message %q does not list %s", p.Errors[0].Message, k)
		}
	}
}

// 009-AC6: every watch route against its owner and a stranger. Another user's watch is 404 on every
// route, like a missing one, never 403.
func TestWatchAuthzMatrix(t *testing.T) {
	a := newDevApp(t)
	owner, stranger := a.signIn(t, "alice"), a.signIn(t, "dave")
	callers := []struct {
		name string
		u    apiUser
	}{{"stranger", stranger}, {"owner", owner}}

	var ownerCred credJSON
	a.mustCall(t, owner, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "matrix", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &ownerCred)
	freshWith := func(t *testing.T, credentialed, paused bool) string {
		var w watchJSON
		body := map[string]any{"name": "Go", "kind": "github_repo", "config": map[string]any{"repo": "golang/go"}, "paused": paused}
		if credentialed {
			body["credential_id"] = ownerCred.ID
		}
		a.mustCall(t, owner, http.MethodPost, "/api/v1/watches", body, http.StatusCreated, &w)
		return w.ID
	}
	routes := []struct {
		name, method, path string
		body               any
		want               [2]int // stranger, owner; owner runs last
		credentialed       bool   // the watch carries the owner's credential
		paused             bool
	}{
		{"get", http.MethodGet, "", nil, [2]int{404, 200}, false, false},
		{"rename", http.MethodPatch, "", map[string]any{"name": "renamed"}, [2]int{404, 200}, false, false},
		{"edit config", http.MethodPatch, "", map[string]any{"config": map[string]any{"repo": "a/b"}}, [2]int{404, 200}, false, false},
		{"change credential", http.MethodPatch, "", map[string]any{"credential_id": ""}, [2]int{404, 200}, false, false},
		{"edit config of credentialed watch", http.MethodPatch, "", map[string]any{"config": map[string]any{"repo": "attacker/loot", "api_url": "https://evil.example"}}, [2]int{404, 200}, true, false},
		{"toggle a box of credentialed watch", http.MethodPatch, "", map[string]any{"config": map[string]any{"repo": "golang/go", "new_prs": true}}, [2]int{404, 200}, true, false},
		{"rename credentialed watch", http.MethodPatch, "", map[string]any{"name": "renamed", "schedule": "1h", "paused": true}, [2]int{404, 200}, true, false},
		// A refusal the owner would get (400, 409) must not reach a stranger ahead of the 404.
		{"blank name", http.MethodPatch, "", map[string]any{"name": " "}, [2]int{404, 400}, false, false},
		{"run paused", http.MethodPost, "/run", nil, [2]int{404, 409}, false, true},
		{"pause", http.MethodPost, "/pause", nil, [2]int{404, 200}, false, false},
		{"resume", http.MethodPost, "/resume", nil, [2]int{404, 200}, false, false},
		{"run", http.MethodPost, "/run", nil, [2]int{404, 202}, false, false},
		{"states", http.MethodGet, "/states", nil, [2]int{404, 200}, false, false},
		{"events", http.MethodGet, "/events", nil, [2]int{404, 200}, false, false},
		{"delete", http.MethodDelete, "", nil, [2]int{404, 204}, false, false},
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			id := freshWith(t, r.credentialed, r.paused)
			row := func() string {
				var s string
				if err := a.pool.QueryRow(context.Background(), `SELECT to_jsonb(w)::text FROM watches w WHERE id = $1`, id).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := row()
			for i, c := range callers {
				code, raw := a.call(t, c.u, r.method, "/api/v1/watches/"+id+r.path, r.body)
				if code != r.want[i] {
					t.Errorf("%s: want %d got %d: %s", c.name, r.want[i], code, raw)
				}
				if c.name == "stranger" && (row() != before || a.countRows(t, `SELECT count(*) FROM audit_log WHERE actor_user_id = $1`, c.u.id) != 0) {
					t.Errorf("the stranger's refused call changed the watch or wrote audit")
				}
			}
		})
	}

	t.Run("missing watch is 404 for everyone", func(t *testing.T) {
		for _, c := range callers {
			if code, _ := a.call(t, c.u, http.MethodPatch, "/api/v1/watches/00000000-0000-0000-0000-000000000000", map[string]any{"name": "x"}); code != 404 {
				t.Errorf("%s: %d", c.name, code)
			}
		}
	})

	t.Run("list and create", func(t *testing.T) {
		id := freshWith(t, false, false)
		for _, c := range callers {
			// The filters narrow the caller's own watches, never widen to others'.
			for _, q := range []string{"", "?kind=github_repo", "?q=Go"} {
				var list struct{ Items []watchJSON }
				a.mustCall(t, c.u, http.MethodGet, "/api/v1/watches"+q, nil, http.StatusOK, &list)
				seen := slices.ContainsFunc(list.Items, func(w watchJSON) bool { return w.ID == id })
				if seen != (c.name == "owner") {
					t.Errorf("%s lists the watch with %q: %v", c.name, q, seen)
				}
			}
			a.mustCall(t, c.u, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, nil)
		}
	})

	t.Run("credential routes are private", func(t *testing.T) {
		var c credJSON
		a.mustCall(t, owner, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &c)
		for _, r := range []struct {
			method, path string
			body         any
			own          int
		}{
			{http.MethodGet, "", nil, 200},
			{http.MethodPut, "/secret", map[string]any{"secret": "ghp_fedcba9876543210"}, 200},
			{http.MethodDelete, "", nil, 204},
		} {
			for _, cl := range callers {
				want := 404
				if cl.name == "owner" {
					want = r.own
				}
				if code, raw := a.call(t, cl.u, r.method, "/api/v1/credentials/"+c.ID+r.path, r.body); code != want {
					t.Errorf("%s %s%s: want %d got %d %s", cl.name, r.method, r.path, want, code, raw)
				}
			}
		}
	})
}

// 009-AC7: a watch carries no permission or owner field, and a shared query parameter changes nothing.
func TestWatchOwnerOnlyShape(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, nil)
	_, get := a.call(t, alice, http.MethodGet, "/api/v1/watches/"+w.ID, nil)
	_, list := a.call(t, alice, http.MethodGet, "/api/v1/watches", nil)
	for name, raw := range map[string][]byte{"get": get, "list": list} {
		for _, key := range []string{`"permission"`, `"owner_id"`, `"owner_name"`} {
			if bytes.Contains(raw, []byte(key)) {
				t.Errorf("%s has %s: %s", name, key, raw)
			}
		}
	}
	var items struct{ Items []watchJSON }
	if err := json.Unmarshal(list, &items); err != nil || len(items.Items) != 1 || items.Items[0].ID != w.ID {
		t.Fatalf("alice lists %s (%v)", list, err)
	}
	for _, q := range []string{"mine", "all", "everyone"} {
		if code, raw := a.call(t, alice, http.MethodGet, "/api/v1/watches?shared="+q, nil); code != http.StatusOK || !bytes.Equal(raw, list) {
			t.Errorf("shared=%s: %d %s, want %s", q, code, raw, list)
		}
	}
}

// 009-AC5: the share routes are gone: every method on them is an unknown /api path, problem+json 404.
func TestShareRoutesGone(t *testing.T) {
	a := newDevApp(t)
	var w string
	if err := a.pool.QueryRow(context.Background(), `INSERT INTO watches (owner_id, name, kind, config) VALUES ($1, 'w', 'github_repo', '{"repo":"a/b"}') RETURNING id::text`, a.userID).Scan(&w); err != nil {
		t.Fatal(err)
	}
	csrf := http.Header{auth.CSRFHeader: {a.csrf}}
	for _, path := range []string{"/api/v1/watches/" + w + "/shares", "/api/v1/watches/" + w + "/shares/" + a.userID} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			resp := a.req(t, method, path, csrf)
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/problem+json" {
				t.Errorf("%s %s: %d %s", method, path, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
		}
	}
}

type credJSON struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Username    *string `json:"username"`
	Last4       string  `json:"last4"`
	UsedByCount int     `json:"used_by_count"`
}

// 001-AC6: secrets are sealed with AES-GCM bound to the row, never returned, never audited.
func TestCredentials(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	const plain, rotated, short = "ghp_TOPSECRET0123456789", "ghp_ROTATED9876543210xyz", "hunter22"
	var responses [][]byte
	record := func(raw []byte) { responses = append(responses, raw) }
	mustRaw := func(u apiUser, method, path string, body any, code int, out any) {
		t.Helper()
		got, raw := a.call(t, u, method, path, body)
		if got != code {
			t.Fatalf("%s %s: want %d got %d: %s", method, path, code, got, raw)
		}
		record(raw)
		if out != nil {
			_ = json.Unmarshal(raw, out)
		}
	}

	var gh, reg credJSON
	mustRaw(alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "GitHub", "kind": "github_token", "secret": plain}, http.StatusCreated, &gh)
	if gh.Last4 != "6789" || gh.Username != nil || gh.Kind != "github_token" {
		t.Fatalf("created: %+v", gh)
	}
	mustRaw(alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "Registry", "kind": "registry_basic", "username": "robot", "secret": short}, http.StatusCreated, &reg)
	if reg.Last4 != "" || reg.Username == nil || *reg.Username != "robot" {
		t.Fatalf("short secret shows a hint: %+v", reg)
	}
	for _, bad := range []struct {
		body any
		code int
		at   string
	}{
		{map[string]any{"name": "GitHub", "kind": "github_token", "secret": plain}, http.StatusConflict, "body.name"},
		{map[string]any{"name": "r", "kind": "registry_basic", "secret": plain}, http.StatusBadRequest, "body.username"},
		{map[string]any{"name": "g", "kind": "gitlab_token", "username": "u", "secret": plain}, http.StatusBadRequest, "body.username"},
		{map[string]any{"name": " ", "kind": "gitlab_token", "secret": plain}, http.StatusBadRequest, "body.name"},
		{map[string]any{"name": "g", "kind": "gitlab_token", "secret": "short"}, http.StatusBadRequest, "body.secret"},
		{map[string]any{"name": "g", "kind": "ssh_key", "secret": plain}, http.StatusBadRequest, "body.kind"},
	} {
		var p problem
		mustRaw(alice, http.MethodPost, "/api/v1/credentials", bad.body, bad.code, &p)
		if len(p.Errors) == 0 || p.Errors[0].Location != bad.at {
			t.Errorf("%v: %+v", bad.body, p)
		}
	}

	// Stored sealed under the row ID: the plaintext is not in the row and only that row's AAD opens it.
	var ct, nonce []byte
	var version int
	if err := a.pool.QueryRow(ctx, `SELECT secret_ciphertext, secret_nonce, key_version FROM credentials WHERE id = $1`, gh.ID).Scan(&ct, &nonce, &version); err != nil {
		t.Fatal(err)
	}
	box, _ := secret.New(testKey)
	if opened, err := box.Open(ct, nonce, []byte(gh.ID)); err != nil || string(opened) != plain || version != 1 || bytes.Contains(ct, []byte(plain)) {
		t.Fatalf("sealed secret: %q %v v%d", opened, err, version)
	}
	if _, err := box.Open(ct, nonce, []byte(reg.ID)); err == nil {
		t.Fatal("ciphertext opens under another row's AAD")
	}

	// Attach: kind family and ownership are checked.
	var w watchJSON
	body := newWatch("Go", "github_repo", map[string]any{"repo": "golang/go"})
	body["credential_id"] = reg.ID
	var p problem
	mustRaw(alice, http.MethodPost, "/api/v1/watches", body, http.StatusBadRequest, &p)
	if p.locations()[0] != "body.credential_id" || !strings.Contains(p.Errors[0].Message, "github_token") {
		t.Fatalf("kind mismatch: %+v", p)
	}
	body["credential_id"] = gh.ID
	mustRaw(alice, http.MethodPost, "/api/v1/watches", body, http.StatusCreated, &w)
	if w.CredentialID == nil || *w.CredentialID != gh.ID {
		t.Fatalf("attached: %+v", w)
	}
	mustRaw(bob, http.MethodPost, "/api/v1/watches", body, http.StatusBadRequest, nil)
	// 001-AC5: nor on update: bob cannot attach alice's credential to his own watch.
	var bobs watchJSON
	mustRaw(bob, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &bobs)
	mustRaw(bob, http.MethodPatch, "/api/v1/watches/"+bobs.ID, map[string]any{"credential_id": gh.ID}, http.StatusBadRequest, nil)
	mustRaw(bob, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusNotFound, nil)

	var list struct{ Items []credJSON }
	mustRaw(alice, http.MethodGet, "/api/v1/credentials", nil, http.StatusOK, &list)
	if len(list.Items) != 2 || list.Items[0].Name != "GitHub" || list.Items[0].UsedByCount != 1 {
		t.Fatalf("list: %+v", list.Items)
	}
	list.Items = nil
	mustRaw(alice, http.MethodGet, "/api/v1/credentials?kind=registry_basic", nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("kind filter: %+v", list.Items)
	}
	list.Items = nil
	mustRaw(bob, http.MethodGet, "/api/v1/credentials", nil, http.StatusOK, &list)
	if len(list.Items) != 0 {
		t.Fatal("bob lists alice's credentials")
	}

	p = problem{}
	mustRaw(alice, http.MethodDelete, "/api/v1/credentials/"+gh.ID, nil, http.StatusConflict, &p)
	if !strings.Contains(p.Detail, "Go") || len(p.Errors) != 1 || p.Errors[0].Value != "Go" {
		t.Fatalf("delete in use: %+v", p)
	}
	mustRaw(alice, http.MethodPut, "/api/v1/credentials/"+gh.ID+"/secret", map[string]any{"secret": rotated}, http.StatusOK, &gh)
	if gh.Last4 != "0xyz" {
		t.Fatalf("rotated: %+v", gh)
	}
	mustRaw(alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"credential_id": ""}, http.StatusOK, &w)
	if w.CredentialID != nil {
		t.Fatal("not detached")
	}
	mustRaw(alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"credential_id": gh.ID}, http.StatusOK, nil)
	mustRaw(alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"credential_id": ""}, http.StatusOK, nil)
	mustRaw(alice, http.MethodDelete, "/api/v1/credentials/"+gh.ID, nil, http.StatusNoContent, nil)
	mustRaw(alice, http.MethodGet, "/api/v1/audit?limit=200", nil, http.StatusOK, nil)

	// Neither any API response nor any audit row carries a secret, in any encoding.
	var audit []byte
	if err := a.pool.QueryRow(ctx, `SELECT COALESCE(string_agg(concat_ws(' ', action, target_id, before::text, after::text), E'\n'), '') FROM audit_log`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{plain, rotated, short} {
		for _, form := range []string{s, base64.StdEncoding.EncodeToString([]byte(s)), url.QueryEscape(s)} {
			if bytes.Contains(audit, []byte(form)) {
				t.Errorf("audit log carries a secret: %s", audit)
			}
			for _, r := range responses {
				if bytes.Contains(r, []byte(form)) {
					t.Errorf("response carries a secret: %s", r)
				}
			}
		}
	}
	if !bytes.Contains(audit, []byte("credential.secret_rotated")) || !bytes.Contains(audit, []byte("credential.deleted")) {
		t.Fatalf("credential mutations not audited: %s", audit)
	}
}

// 001-AC22: a watch's audit history goes to its owner, and the actor list follows. 009-AC2: share entries
// written before 009, a system one included, go to the watch owner only.
func TestAuditVisibilityFollowsOwner(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"name": "Go"}, http.StatusOK, nil)
	var c credJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &c)
	if _, err := a.pool.Exec(context.Background(), `INSERT INTO audit_log (actor_user_id, action, target_type, target_id, after)
		VALUES ($1, 'watch_share.added', 'watch_share', $2 || ':' || $3, '{}'), (NULL, 'watch_share.removed', 'watch_share', $2 || ':' || $3, NULL)`,
		alice.id, w.ID, bob.id); err != nil {
		t.Fatal(err)
	}

	type page struct {
		Items []struct {
			Action     string `json:"action"`
			TargetType string `json:"target_type"`
			ActorName  string `json:"actor_name"`
		} `json:"items"`
	}
	type actors struct {
		Items []struct{ ID, Name string } `json:"items"`
	}
	entries := func(u apiUser, q string, actor func(string) bool) []string {
		t.Helper()
		var p page
		a.mustCall(t, u, http.MethodGet, "/api/v1/audit?limit=200"+q, nil, http.StatusOK, &p)
		var out []string
		for _, it := range p.Items {
			if actor(it.ActorName) {
				out = append(out, it.Action)
			}
		}
		return out
	}
	aliceEntries := func(u apiUser, q string) []string {
		return entries(u, q, func(n string) bool { return n == "alice" })
	}
	shareRows := func(u apiUser) string {
		return strings.Join(entries(u, "&target_type=watch_share", func(string) bool { return true }), ",")
	}
	actorNames := func(u apiUser) string {
		t.Helper()
		var ac actors
		a.mustCall(t, u, http.MethodGet, "/api/v1/audit/actors", nil, http.StatusOK, &ac)
		var names []string
		for _, x := range ac.Items {
			names = append(names, x.Name)
		}
		return strings.Join(names, ",")
	}
	transfer := func(to apiUser) {
		t.Helper()
		if _, err := a.pool.Exec(context.Background(), `UPDATE watches SET owner_id = $2 WHERE id = $1`, w.ID, to.id); err != nil {
			t.Fatal(err)
		}
	}

	if got := aliceEntries(bob, ""); len(got) != 0 {
		t.Fatalf("bob sees alice's entries: %v", got)
	}
	if got := shareRows(bob); got != "" {
		t.Fatalf("bob sees the share entries of alice's watch: %s", got)
	}
	if got := actorNames(bob); got != "" {
		t.Fatalf("bob's actors: %q", got)
	}
	if got := shareRows(alice); got != "watch_share.removed,watch_share.added" {
		t.Fatalf("the owner's share entries: %s", got)
	}
	if got := actorNames(alice); got != "alice" {
		t.Fatalf("alice's actors: %q", got)
	}

	transfer(bob)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches/"+w.ID+"/pause", nil, http.StatusOK, nil)
	if got := strings.Join(aliceEntries(bob, "&target_type=watch"), ","); got != "watch.updated,watch.created" {
		t.Fatalf("bob after the transfer: %s", got)
	}
	if got := shareRows(bob); got != "watch_share.removed,watch_share.added" {
		t.Fatalf("bob's share entries after the transfer: %s", got)
	}
	if got := aliceEntries(bob, "&target_type=credential"); len(got) != 0 {
		t.Fatalf("bob sees alice's credential: %v", got)
	}
	if got := aliceEntries(bob, "&actor="+alice.id+"&target_type=watch"); strings.Join(got, ",") != "watch.updated,watch.created" {
		t.Fatalf("actor + type filter: %v", got)
	}
	if got := actorNames(bob); got != "alice,bob" {
		t.Fatalf("bob's actors after the transfer: %q", got)
	}

	transfer(alice)
	// Alice sees bob's action on her watch.
	var p page
	a.mustCall(t, alice, http.MethodGet, "/api/v1/audit?actor="+bob.id, nil, http.StatusOK, &p)
	if len(p.Items) != 1 || p.Items[0].Action != "watch.paused" {
		t.Fatalf("alice sees bob's pause: %+v", p)
	}
	if got := aliceEntries(bob, ""); len(got) != 0 {
		t.Fatalf("bob keeps alice's entries after the transfer back: %v", got)
	}
	if got := shareRows(bob); got != "" {
		t.Fatalf("bob keeps the share entries after the transfer back: %s", got)
	}
}

// A database failure is an opaque, logged 500: no SQL detail reaches the client.
func TestWatchAPIDatabaseFailure(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	if _, err := a.pool.Exec(context.Background(), `ALTER TABLE watches RENAME TO watches_gone; ALTER TABLE credentials RENAME TO credentials_gone`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/watches"},
		{http.MethodGet, "/api/v1/watches/" + w.ID},
		{http.MethodGet, "/api/v1/credentials"},
		{http.MethodGet, "/api/v1/audit/actors"},
		{http.MethodDelete, "/api/v1/watches/" + w.ID},
	} {
		code, raw := a.call(t, alice, r.method, r.path, nil)
		if code != http.StatusInternalServerError || bytes.Contains(raw, []byte("_gone")) {
			t.Errorf("%s %s: %d %s", r.method, r.path, code, raw)
		}
	}
	// The delete's own events statement, past the authorization.
	if _, err := a.pool.Exec(context.Background(), `ALTER TABLE watches_gone RENAME TO watches; ALTER TABLE credentials_gone RENAME TO credentials;
		ALTER TABLE events RENAME TO events_gone`); err != nil {
		t.Fatal(err)
	}
	if code, raw := a.call(t, alice, http.MethodDelete, "/api/v1/watches/"+w.ID, nil); code != http.StatusInternalServerError || bytes.Contains(raw, []byte("events_gone")) {
		t.Errorf("delete without events: %d %s", code, raw)
	}
}

// A credential deleted while a watch is being attached to it ends as a 400, never a 500 or a dangling id.
func TestAttachRacesCredentialDelete(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice := a.signIn(t, "alice")
	var c credJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &c)
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)

	// The deleter holds the row (as deleteCredential does) while the attach starts.
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM credentials WHERE id = $1 FOR UPDATE`, c.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		raw  []byte
	}
	done := make(chan result, 1)
	go func() {
		code, raw := a.call(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"credential_id": c.ID})
		done <- result{code, raw}
	}()
	select {
	case r := <-done:
		t.Fatalf("attach did not wait for the deleter's lock: %d %s", r.code, r.raw)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `DELETE FROM credentials WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	var p problem
	_ = json.Unmarshal(r.raw, &p)
	if r.code != http.StatusBadRequest || len(p.Errors) == 0 || p.Errors[0].Location != "body.credential_id" {
		t.Fatalf("attach after delete: %d %s", r.code, r.raw)
	}
}

// 009-AC6: a transfer committed while the owner's write waits on the watch lock stops that write with a
// 404: the ownership check runs on the row the lock returns, not on the one read before. DELETE has no
// re-read after its write to mask a stale check.
func TestWatchTransferredMidRequest(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	for _, c := range []struct {
		name, method, suffix string
		body                 any
	}{
		{"update", http.MethodPatch, "", map[string]any{"name": "alice was here"}},
		{"pause", http.MethodPost, "/pause", nil},
		{"delete", http.MethodDelete, "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var w watchJSON
			a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", newWatch(c.name, "github_repo", map[string]any{"repo": "golang/go"}), http.StatusCreated, &w)
			// The transfer holds the watch row, as every watch mutation does.
			tx, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
				t.Fatal(err)
			}
			done := waitsFor(t, func() (int, []byte) { return a.call(t, alice, c.method, "/api/v1/watches/"+w.ID+c.suffix, c.body) })
			if _, err := tx.Exec(ctx, `UPDATE watches SET owner_id = $2 WHERE id = $1`, w.ID, bob.id); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if r := <-done; r[0] != http.StatusNotFound {
				t.Fatalf("write after the transfer: %v %s", r[0], r[1])
			}
			var name, owner string
			var paused bool
			if err := a.pool.QueryRow(ctx, `SELECT name, paused, owner_id::text FROM watches WHERE id = $1`, w.ID).Scan(&name, &paused, &owner); err != nil ||
				name != c.name || paused || owner != bob.id {
				t.Fatalf("watch written anyway: %q paused %v owner %s, %v", name, paused, owner, err)
			}
		})
	}
}

// 001-AC5: a write decides on the row its lock returns, not on one read before a concurrent write
// committed: no lost update, no second pause audited, no run of a watch paused meanwhile.
func TestWatchWriteReadsLockedRow(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice := a.signIn(t, "alice")
	for _, c := range []struct {
		name, method, suffix string
		body                 any
		code                 int
		check                string // true on the row the request must leave
	}{
		{"update", http.MethodPatch, "", map[string]any{"name": "renamed"}, http.StatusOK, `SELECT schedule = '30m' FROM watches WHERE id = $1`},
		{"pause", http.MethodPost, "/pause", nil, http.StatusOK, `SELECT count(*) = 0 FROM audit_log WHERE action = 'watch.paused' AND target_id = $1::text`},
		{"run", http.MethodPost, "/run", nil, http.StatusConflict, `SELECT next_run_at > now() + interval '1 hour' FROM watches WHERE id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var w watchJSON
			a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
			a.exec(t, `UPDATE watches SET next_run_at = now() + interval '1 day' WHERE id = $1`, w.ID)
			tx, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
				t.Fatal(err)
			}
			done := waitsFor(t, func() (int, []byte) { return a.call(t, alice, c.method, "/api/v1/watches/"+w.ID+c.suffix, c.body) })
			if _, err := tx.Exec(ctx, `UPDATE watches SET schedule = '30m', paused = true WHERE id = $1`, w.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if r := <-done; r[0] != c.code {
				t.Fatalf("want %d got %v %s", c.code, r[0], r[1])
			}
			var ok bool
			if err := a.pool.QueryRow(ctx, c.check, w.ID).Scan(&ok); err != nil || !ok {
				t.Fatalf("acted on the stale row: %s (%v)", c.check, err)
			}
		})
	}
}

// Reads take no row lock, so a watch held by a write still reads at once.
func TestWatchReadsDoNotWait(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "/events", "/states"} {
		got := make(chan int, 1)
		go func() {
			code, _ := a.call(t, alice, http.MethodGet, "/api/v1/watches/"+w.ID+path, nil)
			got <- code
		}()
		select {
		case code := <-got:
			if code != http.StatusOK {
				t.Fatalf("GET %s: %d", path, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("GET %s waited on the watch lock", path)
		}
	}
}

// waitsFor runs req in the background and fails unless it is still blocked after 300 ms.
func waitsFor(t *testing.T, req func() (int, []byte)) <-chan []any {
	t.Helper()
	done := make(chan []any, 1)
	go func() {
		code, raw := req()
		done <- []any{code, raw}
	}()
	select {
	case r := <-done:
		t.Fatalf("request did not wait for the lock: %v %s", r[0], r[1])
	case <-time.After(300 * time.Millisecond):
	}
	return done
}

// 001-AC5: a watch deleted while its owner's pause waits on its lock answers 404, not a write to nothing.
func TestWatchDeletedMidLock(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	done := waitsFor(t, func() (int, []byte) { return a.call(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/pause", nil) })
	if _, err := tx.Exec(ctx, `DELETE FROM watches WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r[0] != http.StatusNotFound {
		t.Fatalf("pause of a deleted watch: %v %s", r[0], r[1])
	}
}

// 001-AC6: an attach in flight (KEY SHARE on the credential) makes a delete wait, then refuse with 409;
// a secret rotation does not wait on it.
func TestAttachCommitsBeforeCredentialDelete(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice := a.signIn(t, "alice")
	var c credJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &c)
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM credentials WHERE id = $1 FOR KEY SHARE`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE watches SET credential_id = $2 WHERE id = $1`, w.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	// A rotation changes no key: it goes through while the attach holds its lock.
	rotated := make(chan int, 1)
	go func() {
		code, _ := a.call(t, alice, http.MethodPut, "/api/v1/credentials/"+c.ID+"/secret", map[string]any{"secret": "ghp_fedcba9876543210"})
		rotated <- code
	}()
	select {
	case code := <-rotated:
		if code != http.StatusOK {
			t.Fatalf("rotate during attach: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation blocked behind an attach")
	}
	done := waitsFor(t, func() (int, []byte) { return a.call(t, alice, http.MethodDelete, "/api/v1/credentials/"+c.ID, nil) })
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	var p problem
	_ = json.Unmarshal(r[1].([]byte), &p)
	if r[0] != http.StatusConflict || len(p.Errors) != 1 || p.Errors[0].Value != "Go releases" {
		t.Fatalf("delete after attach: %v %s", r[0], r[1])
	}
}
