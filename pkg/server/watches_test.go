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
	"strings"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/secret"
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
	OwnerName    string         `json:"owner_name"`
	Permission   string         `json:"permission"`
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
	newWatch("Go releases", "github_release", map[string]any{"repo": "golang/go"}),
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
		if w.Permission != "owner" || w.Schedule != "15m" || w.Paused || w.OwnerName != "alice" || w.Kind != body["kind"] {
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
	for q, want := range map[string]int{"kind=gitlab_mr": 1, "q=go": 2, "q=GOLANG": 2, "q=100%25": 0, "shared=mine": 5, "shared=with_me": 0, "limit=2": 2} {
		list.Items = nil
		a.mustCall(t, alice, http.MethodGet, "/api/v1/watches?"+q, nil, http.StatusOK, &list)
		if len(list.Items) != want {
			t.Errorf("%s: want %d got %d", q, want, len(list.Items))
		}
	}

	id := ids["github_release"]
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
		{"bad repo", http.MethodPost, "/api/v1/watches", newWatch("x", "github_release", map[string]any{"repo": "nope"}), []string{"body.config.repo"}},
		{"bad regex", http.MethodPost, "/api/v1/watches", newWatch("x", "oci_tags", map[string]any{"repository": "a/b", "tag_regex": "(["}), []string{"body.config.tag_regex"}},
		{"bad cron", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_release", "config": map[string]any{"repo": "a/b"}, "schedule": "61 * * * *"}, []string{"body.schedule"}},
		{"blank name and short interval", http.MethodPost, "/api/v1/watches", map[string]any{"name": "  ", "kind": "github_release", "config": map[string]any{"repo": "a/b"}, "schedule": "10s"}, []string{"body.schedule", "body.name"}},
		{"unknown kind", http.MethodPost, "/api/v1/watches", newWatch("x", "rss", map[string]any{}), []string{"body.kind"}},
		{"unknown top-level field", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_release", "config": map[string]any{"repo": "a/b"}, "owner_id": "x"}, []string{"body.owner_id"}},
		{"missing config", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_release"}, []string{"body"}},
		{"bad credential id", http.MethodPost, "/api/v1/watches", map[string]any{"name": "x", "kind": "github_release", "config": map[string]any{"repo": "a/b"}, "credential_id": "nope"}, []string{"body.credential_id"}},
		{"patch bad config", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"config": map[string]any{"repo": "a/b", "number": 1}}, []string{"body.config.number"}},
		{"patch bad schedule and name", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"schedule": "@daily", "name": " "}, []string{"body.name", "body.schedule"}},
		{"patch credential not a uuid", http.MethodPatch, "/api/v1/watches/" + w.ID, map[string]any{"credential_id": "x"}, []string{"body.credential_id"}},
		{"bad id", http.MethodGet, "/api/v1/watches/not-a-uuid", nil, []string{"path.id"}},
		{"bad list filter", http.MethodGet, "/api/v1/watches?shared=everyone", nil, []string{"query.shared"}},
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

// 001-AC5: every watch route against owner, rw share, ro share and stranger. A watch the caller cannot
// see is 404 (never 403), one they can see but not change is 403.
func TestWatchAuthzMatrix(t *testing.T) {
	a := newDevApp(t)
	owner, rw, ro, stranger := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "carol"), a.signIn(t, "dave")
	a.signIn(t, "erin")
	callers := []struct {
		name string
		u    apiUser
	}{{"stranger", stranger}, {"ro", ro}, {"rw", rw}, {"owner", owner}}

	var ownerCred credJSON
	a.mustCall(t, owner, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "matrix", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &ownerCred)
	freshWith := func(t *testing.T, credentialed bool) string {
		var w watchJSON
		body := map[string]any{"name": "Go", "kind": "github_release", "config": map[string]any{"repo": "golang/go"}}
		if credentialed {
			body["credential_id"] = ownerCred.ID
		}
		a.mustCall(t, owner, http.MethodPost, "/api/v1/watches", body, http.StatusCreated, &w)
		a.mustCall(t, owner, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": rw.email, "permission": "rw"}, http.StatusCreated, nil)
		a.mustCall(t, owner, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": ro.email, "permission": "ro"}, http.StatusCreated, nil)
		return w.ID
	}
	fresh := func(t *testing.T) string { return freshWith(t, false) }
	routes := []struct {
		name, method, path string
		body               any
		want               [4]int // stranger, ro, rw, owner; owner runs last
		credentialed       bool   // the watch carries the owner's credential
	}{
		{"get", http.MethodGet, "", nil, [4]int{404, 200, 200, 200}, false},
		{"rename", http.MethodPatch, "", map[string]any{"name": "renamed"}, [4]int{404, 403, 200, 200}, false},
		{"edit config", http.MethodPatch, "", map[string]any{"config": map[string]any{"repo": "a/b"}}, [4]int{404, 403, 200, 200}, false},
		{"change credential", http.MethodPatch, "", map[string]any{"credential_id": ""}, [4]int{404, 403, 403, 200}, false},
		// The owner's token follows the config: only the owner may retarget a credentialed watch.
		{"edit config of credentialed watch", http.MethodPatch, "", map[string]any{"config": map[string]any{"repo": "attacker/loot", "api_url": "https://evil.example"}}, [4]int{404, 403, 403, 200}, true},
		{"resend same config of credentialed watch", http.MethodPatch, "", map[string]any{"config": map[string]any{"repo": "golang/go"}}, [4]int{404, 403, 200, 200}, true},
		{"rename credentialed watch", http.MethodPatch, "", map[string]any{"name": "renamed", "schedule": "1h", "paused": true}, [4]int{404, 403, 200, 200}, true},
		{"pause", http.MethodPost, "/pause", nil, [4]int{404, 403, 200, 200}, false},
		{"resume", http.MethodPost, "/resume", nil, [4]int{404, 403, 200, 200}, false},
		{"list shares", http.MethodGet, "/shares", nil, [4]int{404, 200, 200, 200}, false},
		{"add share", http.MethodPost, "/shares", map[string]any{"email": "erin@example.test", "permission": "ro"}, [4]int{404, 403, 403, 201}, false},
		{"update share", http.MethodPut, "/shares/" + ro.id, map[string]any{"permission": "rw"}, [4]int{404, 403, 403, 200}, false},
		{"remove share", http.MethodDelete, "/shares/" + ro.id, nil, [4]int{404, 403, 403, 204}, false},
		{"delete", http.MethodDelete, "", nil, [4]int{404, 403, 403, 204}, false},
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			id := freshWith(t, r.credentialed)
			for i, c := range callers {
				code, raw := a.call(t, c.u, r.method, "/api/v1/watches/"+id+r.path, r.body)
				if code != r.want[i] {
					t.Errorf("%s: want %d got %d: %s", c.name, r.want[i], code, raw)
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
		id := fresh(t)
		for _, c := range callers {
			var list struct{ Items []watchJSON }
			a.mustCall(t, c.u, http.MethodGet, "/api/v1/watches", nil, http.StatusOK, &list)
			var seen *watchJSON
			for i := range list.Items {
				if list.Items[i].ID == id {
					seen = &list.Items[i]
				}
			}
			switch {
			case c.name == "stranger" && seen != nil:
				t.Error("stranger lists the watch")
			case c.name == "owner" && (seen == nil || seen.Permission != "owner"):
				t.Errorf("owner: %+v", seen)
			case (c.name == "ro" || c.name == "rw") && (seen == nil || seen.Permission != c.name || seen.OwnerName != "alice"):
				t.Errorf("%s: %+v", c.name, seen)
			}
			a.mustCall(t, c.u, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, nil)
		}
		var list struct{ Items []watchJSON }
		a.mustCall(t, rw, http.MethodGet, "/api/v1/watches?shared=with_me", nil, http.StatusOK, &list)
		if len(list.Items) == 0 || list.Items[0].Permission != "rw" {
			t.Fatalf("with_me: %+v", list.Items)
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

// 001-AC5: sharing is by exact email of a signed-in user, never with oneself, and once per user.
func TestShares(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	shares := "/api/v1/watches/" + w.ID + "/shares"

	var p problem
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": "ALICE@example.test", "permission": "ro"}, http.StatusBadRequest, &p)
	if p.locations()[0] != "body.email" {
		t.Fatalf("self share: %+v", p)
	}
	p = problem{}
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": "nobody@example.test", "permission": "ro"}, http.StatusNotFound, &p)
	if p.Detail != "no user with that email has signed in yet" {
		t.Fatalf("unknown email: %+v", p)
	}
	// Exact match only: a prefix finds nobody.
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": "bob@", "permission": "ro"}, http.StatusNotFound, nil)
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": "carol", "permission": "admin"}, http.StatusBadRequest, nil)

	var sh struct {
		UserID     string `json:"user_id"`
		Email      string `json:"email"`
		Permission string `json:"permission"`
	}
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": " Bob@Example.test ", "permission": "ro"}, http.StatusCreated, &sh)
	if sh.UserID != bob.id || sh.Permission != "ro" || sh.Email != bob.email {
		t.Fatalf("share: %+v", sh)
	}
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": bob.email, "permission": "rw"}, http.StatusConflict, nil)

	var got watchJSON
	a.mustCall(t, bob, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusOK, &got)
	if got.Permission != "ro" {
		t.Fatalf("bob: %+v", got)
	}
	// A sharee lists only their own share; the owner lists them all.
	carol := a.signIn(t, "carol")
	a.mustCall(t, alice, http.MethodPost, shares, map[string]any{"email": carol.email, "permission": "rw"}, http.StatusCreated, nil)
	var list struct{ Items []struct{ Email string } }
	for u, want := range map[*apiUser]string{&bob: bob.email, &carol: carol.email, &alice: bob.email + "," + carol.email} {
		list.Items = nil
		a.mustCall(t, *u, http.MethodGet, shares, nil, http.StatusOK, &list)
		var emails []string
		for _, it := range list.Items {
			emails = append(emails, it.Email)
		}
		if strings.Join(emails, ",") != want {
			t.Errorf("%s lists %v, want %s", u.email, emails, want)
		}
	}
	a.mustCall(t, alice, http.MethodDelete, shares+"/"+carol.id, nil, http.StatusNoContent, nil)

	a.mustCall(t, alice, http.MethodPut, shares+"/"+bob.id, map[string]any{"permission": "rw"}, http.StatusOK, &sh)
	a.mustCall(t, bob, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"name": "bob's name"}, http.StatusOK, &got)
	if sh.Permission != "rw" || got.Name != "bob's name" || got.Permission != "rw" {
		t.Fatalf("rw: %+v %+v", sh, got)
	}
	a.mustCall(t, alice, http.MethodPut, shares+"/"+alice.id, map[string]any{"permission": "rw"}, http.StatusNotFound, nil)
	a.mustCall(t, alice, http.MethodDelete, shares+"/"+bob.id, nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodDelete, shares+"/"+bob.id, nil, http.StatusNotFound, nil)
	a.mustCall(t, bob, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusNotFound, nil)

	// The database refuses an owner share even past the API.
	if _, err := a.pool.Exec(context.Background(), `INSERT INTO watch_shares (watch_id, user_id, permission) VALUES ($1, $2, 'ro')`, w.ID, alice.id); err == nil {
		t.Fatal("owner share inserted")
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
	body := newWatch("Go", "github_release", map[string]any{"repo": "golang/go"})
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
	mustRaw(alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "rw"}, http.StatusCreated, nil)
	mustRaw(bob, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusOK, nil)

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

// 001-AC22: a sharee sees a watch's audit history while the share lasts, and the actor list follows.
func TestAuditVisibilityWidens(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"name": "Go"}, http.StatusOK, nil)
	var c credJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &c)

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
	aliceEntries := func(u apiUser, q string) []string {
		t.Helper()
		var p page
		a.mustCall(t, u, http.MethodGet, "/api/v1/audit?limit=200"+q, nil, http.StatusOK, &p)
		var out []string
		for _, it := range p.Items {
			if it.ActorName == "alice" {
				out = append(out, it.Action)
			}
		}
		return out
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

	if got := aliceEntries(bob, ""); len(got) != 0 {
		t.Fatalf("bob sees alice's entries before any share: %v", got)
	}
	if got := actorNames(bob); got != "" {
		t.Fatalf("bob's actors before share: %q", got)
	}
	if got := actorNames(alice); got != "alice" {
		t.Fatalf("alice's actors: %q", got)
	}

	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "rw"}, http.StatusCreated, nil)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches/"+w.ID+"/pause", nil, http.StatusOK, nil)
	// Another sharee's share entries stay between the owner and that sharee.
	carol := a.signIn(t, "carol")
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": carol.email, "permission": "ro"}, http.StatusCreated, nil)
	a.mustCall(t, alice, http.MethodPut, "/api/v1/watches/"+w.ID+"/shares/"+carol.id, map[string]any{"permission": "rw"}, http.StatusOK, nil)
	got := strings.Join(aliceEntries(bob, ""), ",")
	if got != "watch_share.added,watch.updated,watch.created" {
		t.Fatalf("bob after share: %s", got)
	}
	if got := strings.Join(aliceEntries(carol, "&target_type=watch_share"), ","); got != "watch_share.updated,watch_share.added" {
		t.Fatalf("carol's own share entries: %s", got)
	}
	if got := strings.Join(aliceEntries(alice, "&target_type=watch_share"), ","); got != "watch_share.updated,watch_share.added,watch_share.added" {
		t.Fatalf("the owner sees every share entry: %s", got)
	}
	if got := aliceEntries(bob, "&target_type=credential"); len(got) != 0 {
		t.Fatalf("bob sees alice's credential: %v", got)
	}
	if got := aliceEntries(bob, "&actor="+alice.id+"&target_type=watch"); strings.Join(got, ",") != "watch.updated,watch.created" {
		t.Fatalf("actor + type filter: %v", got)
	}
	if got := actorNames(bob); got != "alice,bob" {
		t.Fatalf("bob's actors after share: %q", got)
	}
	// Alice sees bob's action on her watch.
	var p page
	a.mustCall(t, alice, http.MethodGet, "/api/v1/audit?actor="+bob.id, nil, http.StatusOK, &p)
	if len(p.Items) != 1 || p.Items[0].Action != "watch.paused" {
		t.Fatalf("alice sees bob's pause: %+v", p)
	}

	a.mustCall(t, alice, http.MethodDelete, "/api/v1/watches/"+w.ID+"/shares/"+bob.id, nil, http.StatusNoContent, nil)
	if got := aliceEntries(bob, ""); len(got) != 0 {
		t.Fatalf("bob keeps alice's entries after the share ends: %v", got)
	}
}

// A database failure is an opaque, logged 500: no SQL detail reaches the client.
func TestWatchAPIDatabaseFailure(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	if _, err := a.pool.Exec(context.Background(), `ALTER TABLE watch_shares RENAME TO watch_shares_gone; ALTER TABLE credentials RENAME TO credentials_gone`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/watches"},
		{http.MethodGet, "/api/v1/watches/" + w.ID},
		{http.MethodGet, "/api/v1/watches/" + w.ID + "/shares"},
		{http.MethodGet, "/api/v1/credentials"},
		{http.MethodGet, "/api/v1/audit/actors"},
		{http.MethodDelete, "/api/v1/watches/" + w.ID},
	} {
		code, raw := a.call(t, alice, r.method, r.path, nil)
		if code != http.StatusInternalServerError || bytes.Contains(raw, []byte("watch_shares")) || bytes.Contains(raw, []byte("credentials")) {
			t.Errorf("%s %s: %d %s", r.method, r.path, code, raw)
		}
	}
}

// 001-AC5 / 001-AC6: a rw sharee cannot send the owner's token elsewhere; the stored target is untouched.
func TestSharedCredentialTarget(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var c credJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &c)
	var w, plain watchJSON
	body := newWatch("Private", "github_release", map[string]any{"repo": "alice/private"})
	body["credential_id"] = c.ID
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", body, http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &plain)
	for _, id := range []string{w.ID, plain.ID} {
		a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+id+"/shares", map[string]any{"email": bob.email, "permission": "rw"}, http.StatusCreated, nil)
	}
	for _, cfg := range []map[string]any{
		{"repo": "alice/private", "api_url": "https://evil.example"},
		{"repo": "alice/other-private"},
		{"repo": "alice/private", "include_prereleases": true},
	} {
		var p problem
		a.mustCall(t, bob, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"config": cfg, "name": "x"}, http.StatusForbidden, &p)
	}
	var got watchJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/watches/"+w.ID, nil, http.StatusOK, &got)
	if got.Config["api_url"] != "https://api.github.com" || got.Config["repo"] != "alice/private" || got.Name != "Private" {
		t.Fatalf("credentialed watch changed: %+v", got)
	}
	// Without a credential there is nothing of the owner's to leak: rw may retarget.
	a.mustCall(t, bob, http.MethodPatch, "/api/v1/watches/"+plain.ID, map[string]any{"config": map[string]any{"repo": "golang/tools"}}, http.StatusOK, &got)
	if got.Config["repo"] != "golang/tools" {
		t.Fatalf("plain watch: %+v", got)
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

// 001-AC5: a share revoked or downgraded while a sharee's write waits on the watch lock stops that
// write: the access check reads the shares after the lock, not before.
func TestShareChangedMidRequest(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change string
		want   int
	}{
		{"revoked", `DELETE FROM watch_shares WHERE watch_id = $1 AND user_id = $2`, http.StatusNotFound},
		{"downgraded", `UPDATE watch_shares SET permission = 'ro' WHERE watch_id = $1 AND user_id = $2`, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newDevApp(t)
			ctx := context.Background()
			alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
			var w watchJSON
			a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
			a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "rw"}, http.StatusCreated, nil)

			// The owner's change holds the watch row, as every watch mutation does.
			tx, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan int, 1)
			go func() {
				code, _ := a.call(t, bob, http.MethodPatch, "/api/v1/watches/"+w.ID, map[string]any{"name": "bob was here"})
				done <- code
			}()
			select {
			case code := <-done:
				t.Fatalf("bob's write did not wait for the lock: %d", code)
			case <-time.After(300 * time.Millisecond):
			}
			if _, err := tx.Exec(ctx, tt.change, w.ID, bob.id); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != tt.want {
				t.Fatalf("bob's write after the share was %s: want %d got %d", tt.name, tt.want, code)
			}
			var name string
			if err := a.pool.QueryRow(ctx, `SELECT name FROM watches WHERE id = $1`, w.ID).Scan(&name); err != nil || name != "Go releases" {
				t.Fatalf("watch written anyway: %q %v", name, err)
			}
		})
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

// 001-AC5: a watch deleted while a sharee's pause waits on its lock answers 404, not a write to nothing.
func TestWatchDeletedMidLock(t *testing.T) {
	a := newDevApp(t)
	ctx := context.Background()
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "rw"}, http.StatusCreated, nil)
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	done := waitsFor(t, func() (int, []byte) { return a.call(t, bob, http.MethodPost, "/api/v1/watches/"+w.ID+"/pause", nil) })
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
