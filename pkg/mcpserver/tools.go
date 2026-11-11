package mcpserver

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/gedour/gedour/pkg/auth"
	"github.com/gedour/gedour/pkg/watch"
)

const (
	scopeRead   = "read"
	scopeWrite  = "write"
	scopeManage = "manage"
)

// scopeRank nests the scopes: a key reaches every tool at or below its rank.
var scopeRank = map[string]int{scopeRead: 0, scopeWrite: 1, scopeManage: 2}

// row is one tool: its data path is the REST handler of the operation.
type row struct {
	name, op, scope, desc string
	readOnly              bool
	// openWorld: the call makes gedour reach outside itself.
	openWorld bool
	// limited: the call spends the key's budget of costly calls, on top of the request limit.
	limited bool
	// limitDefault and limitMax narrow the operation's limit; 0 keeps the operation's value.
	limitDefault, limitMax int
	shape                  func(data any, args map[string]any)
	// hidden inputs leave the schema; the handler still refuses them, as a schema is no access control.
	hidden []string
}

const (
	budgetNote = " A burst of 6, then one every 10 s, one budget for run_watch and the manage edits, per replica."
	filterNote = " Filters run over event and watch: get_filter_reference gives the language, list_watch_events shows real field names."
)

// configNote gives a model what the schema cannot: config is free-form, and each wrong guess spends the budget.
var configNote = " config fields by kind: " + watch.ConfigFields() + "."

// rows is the catalog, in tools/list order: client prompt caches depend on it.
var rows = []row{
	{name: "count_inbox", op: "count-inbox", scope: scopeRead, readOnly: true, desc: "Count the unread and read items in the inbox, archive excluded."},
	{name: "list_inbox", op: "list-inbox", scope: scopeRead, readOnly: true, limitDefault: 20, limitMax: 50, shape: cutInboxBodies,
		desc: "List inbox items with their event and watch; a long event body is cut."},
	{name: "list_watches", op: "list-watches", scope: scopeRead, readOnly: true, limitDefault: 50, limitMax: 200, shape: markTruncated,
		desc: "List the watches of the key's owner; truncated: true means there may be more, narrow with kind or q."},
	{name: "get_watch", op: "get-watch", scope: scopeRead, readOnly: true, desc: "Read one watch."},
	{name: "list_watch_events", op: "list-watch-events", scope: scopeRead, readOnly: true, limitMax: 50, desc: "List the events a watch fired, newest first."},
	{name: "list_watch_states", op: "list-watch-states", scope: scopeRead, readOnly: true, limitMax: 50, desc: "List the upstream states a watch observed, newest first."},
	{name: "list_rules", op: "list-rules", scope: scopeRead, readOnly: true, desc: "List the routing rules of the key's owner."},
	{name: "get_rule", op: "get-rule", scope: scopeRead, readOnly: true, desc: "Read one routing rule."},
	{name: "list_deliveries", op: "list-deliveries", scope: scopeRead, readOnly: true, limitMax: 50, shape: dropDeliveryDiagnostics,
		desc: "List notification deliveries, newest first, without script logs or errors."},
	{name: "mark_inbox_read", op: "read-inbox", scope: scopeWrite, desc: "Mark inbox items read. ids is an array of inbox item IDs from list_inbox; IDs of items that are not the key owner's are ignored."},
	{name: "mark_inbox_unread", op: "unread-inbox", scope: scopeWrite, desc: "Mark inbox items unread, taking archived ones out of the archive. ids is an array of inbox item IDs from list_inbox; IDs of items that are not the key owner's are ignored."},
	{name: "archive_inbox", op: "archive-inbox", scope: scopeWrite, desc: "Archive inbox items. ids is an array of inbox item IDs from list_inbox; IDs of items that are not the key owner's are ignored."},
	{name: "unarchive_inbox", op: "unarchive-inbox", scope: scopeWrite, desc: "Take inbox items out of the archive. ids is an array of inbox item IDs from list_inbox; IDs of items that are not the key owner's are ignored."},
	{name: "run_watch", op: "run-watch", scope: scopeWrite, openWorld: true, limited: true,
		desc: "Schedule a run of a watch now; the worker runs it, this call does not wait; refused while the upstream's rate limit holds the watch." + budgetNote},
	{name: "get_filter_reference", op: "get-filter-reference", scope: scopeManage, readOnly: true, desc: "What a rule's filter may use, and its limits."},
	{name: "list_channels", op: "list-channels", scope: scopeManage, readOnly: true, shape: briefChannels, desc: "List the channels of the key's owner: id, name and type only, for a rule's channel_ids."},
	{name: "create_watch", op: "create-watch", scope: scopeManage, openWorld: true, limited: true, hidden: []string{"credential_id"},
		desc: "Create a watch, without a credential. Its api_url, base_url or registry must name, with no path, the kind's default (GitHub kinds api.github.com, GitLab kinds gitlab.com, oci_tags docker.io or ghcr.io), or the host and path of another watch the key's owner owns. Refused past 200 owned watches." + configNote + budgetNote},
	{name: "update_watch", op: "update-watch", scope: scopeManage, openWorld: true, limited: true, hidden: []string{"credential_id"},
		desc: "Change a watch's name, config, schedule or paused state; pausing is how to stop a watch. A new config must name a known host and path, as in create_watch, and the config of a watch that has a credential cannot change. Never moves a run ahead of the upstream's rate limit." + configNote + budgetNote},
	{name: "create_rule", op: "create-rule", scope: scopeManage, limited: true, desc: "Create a routing rule. channel_ids come from list_channels." + filterNote + budgetNote},
	{name: "update_rule", op: "update-rule", scope: scopeManage, limited: true, desc: "Change a routing rule; disabling is how to stop one." + filterNote + budgetNote},
}

// tool is a row resolved against the OpenAPI document.
type tool struct {
	row
	method, path string
	// in maps each input field to where the operation reads it: path, query or body.
	in      map[string]string
	hasBody bool
	schema  map[string]any
}

// buildTools resolves rows against doc, and fails on what must never reach production.
func buildTools(doc *huma.OpenAPI, rows []row) ([]*tool, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal OpenAPI: %w", err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("unmarshal OpenAPI: %w", err)
	}
	tools := make([]*tool, 0, len(rows))
	for _, r := range rows {
		if _, ok := scopeRank[r.scope]; !ok {
			return nil, fmt.Errorf("tool %s: unknown scope %q", r.name, r.scope)
		}
		t := &tool{row: r}
		if err := t.resolve(d); err != nil {
			return nil, fmt.Errorf("tool %s: %w", r.name, err)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func (t *tool) resolve(doc map[string]any) error {
	op, err := t.findOperation(doc)
	if err != nil {
		return err
	}
	// Security: a sub-request skips Guard's admin check.
	if strings.HasPrefix(t.path, auth.AdminPrefix) {
		return fmt.Errorf("operation %s is under %s", t.op, auth.AdminPrefix)
	}
	t.in = map[string]string{}
	props := map[string]any{}
	required := []string{}
	add := func(name, in string, schema any, req bool) error {
		if _, dup := t.in[name]; dup {
			return fmt.Errorf("input %q appears twice", name)
		}
		t.in[name], props[name] = in, schema
		if req {
			required = append(required, name)
		}
		return nil
	}
	params, _ := op["parameters"].([]any)
	for _, p := range params {
		param, _ := p.(map[string]any)
		name, _ := param["name"].(string)
		in, _ := param["in"].(string)
		schema, err := resolveRefs(param["schema"], doc, nil)
		if err != nil {
			return err
		}
		req, _ := param["required"].(bool)
		if err := add(name, in, schema, req); err != nil {
			return err
		}
	}
	if body := bodySchema(op); body != nil {
		t.hasBody = true
		resolved, err := resolveRefs(body, doc, nil)
		if err != nil {
			return err
		}
		fields, _ := resolved.(map[string]any)
		bodyProps, _ := fields["properties"].(map[string]any)
		bodyRequired, _ := fields["required"].([]any)
		for _, name := range slices.Sorted(maps.Keys(bodyProps)) {
			if err := add(name, "body", bodyProps[name], slices.Contains(bodyRequired, any(name))); err != nil {
				return err
			}
		}
	}
	if t.limitMax != 0 {
		limit, ok := props["limit"].(map[string]any)
		if !ok {
			return fmt.Errorf("operation %s has no limit", t.op)
		}
		limit["maximum"] = t.limitMax
		if def, _ := limit["default"].(float64); def > 0 {
			limit["default"] = min(int(def), t.limitMax)
		}
		if t.limitDefault != 0 {
			limit["default"] = t.limitDefault
		}
	}
	for _, name := range t.hidden {
		if _, ok := t.in[name]; !ok {
			return fmt.Errorf("hidden input %q is not an input of %s", name, t.op)
		}
		delete(t.in, name)
		delete(props, name)
		required = slices.DeleteFunc(required, func(r string) bool { return r == name })
	}
	slices.Sort(required)
	t.schema = map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		t.schema["required"] = required
	}
	return nil
}

func (t *tool) findOperation(doc map[string]any) (map[string]any, error) {
	paths, _ := doc["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, o := range methods {
			op, ok := o.(map[string]any)
			if ok && op["operationId"] == t.op {
				t.method, t.path = strings.ToUpper(method), path
				return op, nil
			}
		}
	}
	return nil, fmt.Errorf("unknown operation %s", t.op)
}

func bodySchema(op map[string]any) any {
	body, _ := op["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	return media["schema"]
}

// resolveRefs inlines huma's $ref: a tool's inputSchema travels without the document's components.
func resolveRefs(v any, doc map[string]any, stack []string) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if slices.Contains(stack, ref) {
				return nil, fmt.Errorf("recursive schema %s", ref)
			}
			components, _ := doc["components"].(map[string]any)
			schemas, _ := components["schemas"].(map[string]any)
			target, ok := schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
			if !ok {
				return nil, fmt.Errorf("unknown schema %s", ref)
			}
			return resolveRefs(target, doc, append(stack, ref))
		}
		out := make(map[string]any, len(v))
		for k, e := range v {
			r, err := resolveRefs(e, doc, stack)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			r, err := resolveRefs(e, doc, stack)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// recorder keeps a sub-request's answer; dispatch never streams.
type recorder struct {
	status int
	header http.Header
	body   strings.Builder
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}
