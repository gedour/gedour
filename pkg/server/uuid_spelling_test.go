package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"

	"github.com/gedour/gedour/pkg/secret"
)

// uuidSpellings are the forms huma's uuid format accepts besides the canonical one.
var uuidSpellings = map[string]func(string) string{
	"urn":       func(id string) string { return "urn:uuid:" + id },
	"urn upper": func(id string) string { return strings.ToUpper("urn:uuid:" + id) },
	"braces":    func(id string) string { return "{" + id + "}" },
	"upper":     strings.ToUpper,
	"no hyphen": func(id string) string { return strings.ReplaceAll(id, "-", "") },
}

type openAPIDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
		Parameters  []struct {
			Name, In string
			Required bool
			Schema   struct {
				Format string
				Items  *struct{ Format string }
			}
		} `json:"parameters"`
		RequestBody *struct {
			Required bool `json:"required"`
			Content  map[string]struct {
				Schema *bodySchema `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	} `json:"paths"`
	Components struct {
		Schemas map[string]*bodySchema `json:"schemas"`
	} `json:"components"`
}

type bodySchema struct {
	Ref        string                 `json:"$ref"`
	Type       any                    `json:"type"`
	Format     string                 `json:"format"`
	Items      *bodySchema            `json:"items"`
	Properties map[string]*bodySchema `json:"properties"`
}

var idName = regexp.MustCompile(`_ids?$`)

// idFields lists the request-body properties that carry ids: format uuid, or a string or string list named *_id or *_ids.
func (d *openAPIDoc) idFields() []string {
	deref := func(s *bodySchema) *bodySchema {
		if s != nil && s.Ref != "" {
			return d.Components.Schemas[strings.TrimPrefix(s.Ref, "#/components/schemas/")]
		}
		return s
	}
	isString := func(s *bodySchema) bool {
		if s == nil {
			return false
		}
		switch v := s.Type.(type) {
		case string:
			return v == "string"
		case []any:
			return slices.Contains(v, any("string"))
		}
		return false
	}
	var out []string
	var walk func(prefix string, s *bodySchema)
	walk = func(prefix string, s *bodySchema) {
		s = deref(s)
		if s == nil {
			return
		}
		for name, p := range s.Properties {
			p = deref(p)
			items := deref(p.Items)
			uuidFormat := p.Format == "uuid" || items != nil && items.Format == "uuid"
			if uuidFormat || idName.MatchString(name) && (isString(p) || isString(items)) {
				out = append(out, prefix+name)
			}
			walk(prefix+name+".", p)
			walk(prefix+name+"[].", items)
		}
	}
	for _, ops := range d.Paths {
		for _, op := range ops {
			if op.RequestBody != nil {
				walk(op.OperationID+".", op.RequestBody.Content["application/json"].Schema)
			}
		}
	}
	sort.Strings(out)
	return out
}

// uncoveredUUIDParams lists the uuid parameters canonicalUUIDParams does not rewrite: only scalar path and query ones.
func (d *openAPIDoc) uncoveredUUIDParams() []string {
	var out []string
	for _, ops := range d.Paths {
		for _, op := range ops {
			for _, p := range op.Parameters {
				items := p.Schema.Items != nil && p.Schema.Items.Format == "uuid"
				covered := (p.In == "path" || p.In == "query") && p.Schema.Format == "uuid" && p.Schema.Items == nil
				if (p.Schema.Format == "uuid" || items) && !covered {
					out = append(out, "canonicalUUIDParams does not cover "+op.OperationID+" "+p.In+" "+p.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestUncoveredUUIDParams(t *testing.T) {
	var doc openAPIDoc
	if err := json.Unmarshal([]byte(`{"paths": {"/x/{id}": {"get": {"operationId": "op", "parameters": [
		{"name": "id", "in": "path", "schema": {"type": "string", "format": "uuid"}},
		{"name": "ids", "in": "query", "schema": {"type": "array", "items": {"type": "string", "format": "uuid"}}},
		{"name": "X-Id", "in": "header", "schema": {"type": "string", "format": "uuid"}},
		{"name": "sid", "in": "cookie", "schema": {"type": "string", "format": "uuid"}},
		{"name": "q", "in": "query", "schema": {"type": "string"}}]}}}}`), &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{"canonicalUUIDParams does not cover op cookie sid", "canonicalUUIDParams does not cover op header X-Id",
		"canonicalUUIDParams does not cover op query ids"}
	if got := doc.uncoveredUUIDParams(); !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

// Every operation with a uuid path or query parameter, read from the served OpenAPI document so a new one
// is covered on its own, reaches its handler for each spelling; so does every request-body id field.
func TestUUIDSpellingsReachHandlers(t *testing.T) {
	// Web Push on: a web_push channel config reaches its device check.
	a, _, _ := newPushApp(t)
	admin := a.signIn(t, "admin")
	bob := a.signIn(t, "bob")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	var w watchJSON
	a.mustCall(t, admin, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var r ruleJSON
	a.mustCall(t, admin, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r"}, http.StatusCreated, &r)

	// Minimal valid bodies of the operations that require one: huma refuses the call before the handler otherwise.
	bodies := map[string]any{
		"admin-transfer-watch":     map[string]any{"to_user_id": bob.id},
		"update-channel":           map[string]any{},
		"rotate-credential-secret": map[string]any{"secret": "s3cret-value"},
		"rename-push-device":       map[string]any{"name": "n"},
		"update-rule":              map[string]any{},
		"test-rule":                map[string]any{"event_id": 1},
		"update-script":            map[string]any{},
		"dry-run-script":           map[string]any{},
		"update-watch":             map[string]any{},
	}
	var doc openAPIDoc
	_, raw := a.call(t, admin, http.MethodGet, "/api/openapi.json", nil)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, msg := range doc.uncoveredUUIDParams() {
		t.Error(msg)
	}
	n := 0
	for path, ops := range doc.Paths {
		for method, op := range ops {
			var hasUUID bool
			for _, p := range op.Parameters {
				hasUUID = hasUUID || (p.In == "path" || p.In == "query") && p.Schema.Format == "uuid" && p.Schema.Items == nil
			}
			if !hasUUID {
				continue
			}
			n++
			body := bodies[op.OperationID]
			if body == nil && op.RequestBody != nil && op.RequestBody.Required {
				t.Fatalf("%s: requires a body, add a minimal one to bodies", op.OperationID)
			}
			for name, spell := range uuidSpellings {
				concrete, q := path, url.Values{}
				for _, p := range op.Parameters {
					uuidParam := p.Schema.Format == "uuid"
					switch {
					case p.In == "path" && uuidParam:
						concrete = strings.ReplaceAll(concrete, "{"+p.Name+"}", url.PathEscape(spell(uuid.NewString())))
					case p.In == "query" && uuidParam:
						q.Set(p.Name, spell(uuid.NewString()))
					case p.In == "path" || p.In == "query" && p.Required:
						t.Fatalf("%s: %s parameter %s, extend the test", op.OperationID, p.In, p.Name)
					}
				}
				if len(q) > 0 {
					concrete += "?" + q.Encode()
				}
				// An unknown id is a 404, a filter matching nothing a 2xx: any other answer never reached the lookup.
				if code, raw := a.call(t, admin, strings.ToUpper(method), concrete, body); code != http.StatusNotFound && code/100 != 2 {
					t.Errorf("%s (%s): %d %s", op.OperationID, name, code, raw)
				}
			}
		}
	}
	if n < 37 {
		t.Fatalf("found %d operations with a uuid parameter, want at least 37", n)
	}

	// Body ids, each an unknown one: the handler's lookup answers want, its body holding at (a problem's
	// location or detail). Keys are operation.property.
	bodyCases := map[string]struct {
		method, path string
		body         func(id string) any
		want         int
		at           string
	}{
		// The watch exists: the 404 is the account lookup's.
		"admin-transfer-watch.to_user_id": {http.MethodPost, "/api/v1/admin/watches/" + w.ID + "/transfer",
			func(id string) any { return map[string]any{"to_user_id": id} }, http.StatusNotFound, "no such account"},
		"create-watch.credential_id": {http.MethodPost, "/api/v1/watches", func(id string) any {
			return map[string]any{"name": "x", "kind": "github_repo", "config": map[string]any{"repo": "a/b"}, "credential_id": id}
		}, http.StatusBadRequest, "body.credential_id"},
		"update-watch.credential_id": {http.MethodPatch, "/api/v1/watches/" + w.ID,
			func(id string) any { return map[string]any{"credential_id": id} }, http.StatusBadRequest, "body.credential_id"},
		"create-rule.channel_ids": {http.MethodPost, "/api/v1/rules",
			func(id string) any { return map[string]any{"name": "x", "channel_ids": []string{id}} }, http.StatusBadRequest, "body.channel_ids[0]"},
		"update-rule.channel_ids": {http.MethodPatch, "/api/v1/rules/" + r.ID,
			func(id string) any { return map[string]any{"channel_ids": []string{id}} }, http.StatusBadRequest, "body.channel_ids[0]"},
		"create-rule.watch_ids": {http.MethodPost, "/api/v1/rules",
			func(id string) any { return map[string]any{"name": "x", "watch_ids": []string{id}} }, http.StatusBadRequest, "body.watch_ids[0]"},
		"create-rule.watch_id": {http.MethodPost, "/api/v1/rules",
			func(id string) any { return map[string]any{"name": "x", "watch_id": id} }, http.StatusBadRequest, "body.watch_id"},
		"update-rule.watch_ids": {http.MethodPatch, "/api/v1/rules/" + r.ID,
			func(id string) any { return map[string]any{"watch_ids": []string{id}} }, http.StatusBadRequest, "body.watch_ids[0]"},
		"update-rule.watch_id": {http.MethodPatch, "/api/v1/rules/" + r.ID,
			func(id string) any { return map[string]any{"watch_id": id} }, http.StatusBadRequest, "body.watch_id"},
		// A preview scopes its SQL to the ids: an unknown one matches no event.
		"preview-rule.watch_ids": {http.MethodPost, "/api/v1/rules/preview",
			func(id string) any { return map[string]any{"filter": "true", "watch_ids": []string{id}} }, http.StatusOK, `"items":[]`},
		"preview-rule.watch_id": {http.MethodPost, "/api/v1/rules/preview",
			func(id string) any { return map[string]any{"filter": "true", "watch_id": id} }, http.StatusOK, `"items":[]`},
		// Free-form config (additionalProperties): out of the schema walk, listed by hand.
		"create-channel.config.script_id": {http.MethodPost, "/api/v1/channels", func(id string) any {
			return map[string]any{"name": "x", "type": "script", "config": map[string]any{"script_id": id}}
		}, http.StatusBadRequest, "body.config.script_id"},
		"create-channel.config.devices": {http.MethodPost, "/api/v1/channels", func(id string) any {
			return map[string]any{"name": "x", "type": "web_push", "config": map[string]any{"devices": []string{id}}}
		}, http.StatusBadRequest, "body.config.devices[0]"},
	}
	for _, f := range doc.idFields() {
		if _, ok := bodyCases[f]; !ok {
			t.Errorf("request-body id field %s has no case in bodyCases", f)
		}
	}
	for field, tt := range bodyCases {
		for name, spell := range uuidSpellings {
			caller := admin
			if strings.HasPrefix(field, "preview-rule.") {
				// Previews are rate limited per user.
				caller = a.signIn(t, "preview-"+uuid.NewString())
			}
			if code, raw := a.call(t, caller, tt.method, tt.path, tt.body(spell(uuid.NewString()))); code != tt.want || !strings.Contains(string(raw), tt.at) {
				t.Errorf("%s (%s): %d %s, want %d with %s", field, name, code, raw, tt.want, tt.at)
			}
		}
	}
}

// Another spelling of an existing id reaches the same resource as the canonical one, in a path and in a query,
// and a secret rotated through it is sealed under the canonical id, the AAD every reader opens it with.
func TestUUIDSpellingsSameResource(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var hook channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/a"}}, http.StatusCreated, &hook)
	var cred struct{ ID string }
	a.mustCall(t, alice, http.MethodPost, "/api/v1/credentials", map[string]any{"name": "gh", "kind": "github_token", "secret": "ghp_0123456789abcdef"}, http.StatusCreated, &cred)
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', '{}', 'k')`, w.ID)
	a.exec(t, `INSERT INTO deliveries (event_id, channel_id, status) SELECT id, $1, 'pending' FROM events`, hook.ID)
	get := func(path string) string {
		t.Helper()
		code, raw := a.call(t, alice, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, code, raw)
		}
		return string(raw)
	}
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	opens := func(table, id, want string) bool {
		t.Helper()
		var ct, nonce []byte
		if err := a.pool.QueryRow(context.Background(), `SELECT secret_ciphertext, secret_nonce FROM `+table+` WHERE id = $1`, id).Scan(&ct, &nonce); err != nil {
			t.Fatal(err)
		}
		got, err := box.Open(ct, nonce, []byte(id))
		return err == nil && string(got) == want
	}
	wantWatch, wantDeliveries := get("/api/v1/watches/"+w.ID), get("/api/v1/deliveries?channel_id="+hook.ID)
	if !strings.Contains(wantDeliveries, `"status":"pending"`) {
		t.Fatalf("no delivery to compare: %s", wantDeliveries)
	}
	for name, spell := range uuidSpellings {
		if got := get("/api/v1/watches/" + url.PathEscape(spell(w.ID))); got != wantWatch {
			t.Errorf("watch (%s): %s, want %s", name, got, wantWatch)
		}
		if got := get("/api/v1/deliveries?channel_id=" + url.QueryEscape(spell(hook.ID))); got != wantDeliveries {
			t.Errorf("deliveries (%s): %s, want %s", name, got, wantDeliveries)
		}
		// Two spellings of one channel are one channel, not a duplicate rule_channels row.
		var dup ruleJSON
		if code, raw := a.call(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "dup " + name, "channel_ids": []string{hook.ID, spell(hook.ID)}}); code != http.StatusCreated ||
			json.Unmarshal(raw, &dup) != nil || !slices.Equal(dup.ChannelIDs, []string{hook.ID}) {
			t.Errorf("channel_ids in two spellings (%s): %d %s", name, code, raw)
		}
		sec := "rotated secret, " + name
		if code, raw := a.call(t, alice, http.MethodPut, "/api/v1/credentials/"+url.PathEscape(spell(cred.ID))+"/secret", map[string]any{"secret": sec}); code != http.StatusOK || !opens("credentials", cred.ID, sec) {
			t.Errorf("credential secret (%s): %d %s, or it does not open under the canonical id", name, code, raw)
		}
		if code, raw := a.call(t, alice, http.MethodPatch, "/api/v1/channels/"+url.PathEscape(spell(hook.ID)), map[string]any{"secret": sec}); code != http.StatusOK || !opens("channels", hook.ID, sec) {
			t.Errorf("channel secret (%s): %d %s, or it does not open under the canonical id", name, code, raw)
		}
	}
}

// The wrapper stays transparent to huma's context helpers: humago.Unwrap reaches the request, WithContext moves it.
func TestUUIDParamsContextUnwraps(t *testing.T) {
	op := &huma.Operation{Parameters: []*huma.Param{{Name: "id", In: "path", Schema: &huma.Schema{Format: "uuid"}}}}
	id := uuid.NewString()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	r.SetPathValue("id", "urn:uuid:"+strings.ToUpper(id))
	var got huma.Context
	canonicalUUIDParams(humago.NewContext(op, r, httptest.NewRecorder()), func(c huma.Context) { got = c })
	if got.Param("id") != id {
		t.Fatalf("param: %q", got.Param("id"))
	}
	if req, _ := humago.Unwrap(got); req != r {
		t.Fatal("humago.Unwrap does not reach the request")
	}
	type key struct{}
	moved := huma.WithValue(got, key{}, "v")
	if req, _ := humago.Unwrap(moved); req.Context().Value(key{}) != "v" || moved.Context().Value(key{}) != "v" {
		t.Fatal("huma.WithValue does not reach the request")
	}
	if moved.Param("id") != id {
		t.Fatalf("param after WithValue: %q", moved.Param("id"))
	}

	plain := humago.NewContext(&huma.Operation{}, r, httptest.NewRecorder())
	canonicalUUIDParams(plain, func(c huma.Context) { got = c })
	if got != plain {
		t.Fatal("an operation without uuid params gets a wrapped context")
	}
}
