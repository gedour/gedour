package server

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/gedour/gedour/pkg/route"
	"github.com/gedour/gedour/pkg/testpg"
)

// routedTo inserts an event on watch w, routes it and reports whether it reached u's inbox.
func (a *devApp) routedTo(t *testing.T, u apiUser, w string) bool {
	t.Helper()
	var ev int64
	if err := a.pool.QueryRow(context.Background(), `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', gen_random_uuid()::text) RETURNING id`, w).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	router := route.NewRouter(a.pool, zap.NewNop(), route.NewMetrics(prometheus.NewRegistry()), time.Second)
	if n, err := router.RouteOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("route: %d %v", n, err)
	}
	return a.countRows(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, u.id, ev) == 1
}

func (a *devApp) watches(t *testing.T, u apiUser, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		var w watchJSON
		a.mustCall(t, u, http.MethodPost, "/api/v1/watches", sampleWatches[i], http.StatusCreated, &w)
		ids[i] = w.ID
	}
	return ids
}

func sortedIDs(ids ...string) []string {
	return slices.Sorted(slices.Values(ids))
}

// 014-AC1: a rule on [A, B] routes A and B, not C. 014-AC3: a duplicate is stored once. 014-AC5: the output lists
// the watches sorted, with names; watch_id and watch_name are null on a several-watch rule.
func TestRuleOnSeveralWatches(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	w := a.watches(t, alice, 3)
	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "ab", "watch_ids": []string{w[1], w[0], w[1]}}, http.StatusCreated, &r)
	if !slices.Equal(r.WatchIDs, sortedIDs(w[0], w[1])) || r.WatchID != nil || r.WatchName != nil || len(r.Watches) != 2 {
		t.Fatalf("created: %+v", r)
	}
	names := map[string]string{w[0]: sampleWatches[0]["name"].(string), w[1]: sampleWatches[1]["name"].(string)}
	for i, x := range r.Watches {
		if x.ID != r.WatchIDs[i] || x.Name == nil || *x.Name != names[x.ID] {
			t.Errorf("watches[%d]: %+v", i, x)
		}
	}
	if n := a.countRows(t, `SELECT count(*) FROM rule_watches WHERE rule_id = $1`, r.ID); n != 2 {
		t.Errorf("%d rule_watches rows, want 2", n)
	}
	for i, want := range []bool{true, true, false} {
		if got := a.routedTo(t, alice, w[i]); got != want {
			t.Errorf("watch %d routed %v, want %v", i, got, want)
		}
	}
	var got ruleJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+r.ID, nil, http.StatusOK, &got)
	if !slices.Equal(got.WatchIDs, r.WatchIDs) {
		t.Fatalf("read back: %+v", got)
	}
}

// 014-AC2: watch_ids absent or [] covers every watch of the owner, one created after the rule included.
// 014-AC5: watch_ids and watches are [] and watch_id is null on such a rule, set on a one-watch rule.
func TestRuleOnAllWatches(t *testing.T) {
	a := newDevApp(t)
	for name, body := range map[string]map[string]any{
		"absent": {"name": "all"},
		"empty":  {"name": "all", "watch_ids": []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			u := a.signIn(t, name)
			code, raw := a.call(t, u, http.MethodPost, "/api/v1/rules", body)
			if code != http.StatusCreated || !strings.Contains(string(raw), `"watch_ids":[]`) || !strings.Contains(string(raw), `"watches":[]`) ||
				!strings.Contains(string(raw), `"watch_id":null`) {
				t.Fatalf("create: %d %s", code, raw)
			}
			w := a.watches(t, u, 1)[0]
			if !a.routedTo(t, u, w) {
				t.Fatal("a watch created after the all-watches rule was not routed")
			}
		})
	}
	u := a.signIn(t, "one")
	w := a.watches(t, u, 1)[0]
	var r ruleJSON
	a.mustCall(t, u, http.MethodPost, "/api/v1/rules", map[string]any{"name": "one", "watch_ids": []string{w}}, http.StatusCreated, &r)
	if r.WatchID == nil || *r.WatchID != w || r.WatchName == nil || *r.WatchName != sampleWatches[0]["name"] {
		t.Fatalf("one-watch rule: %+v", r)
	}
}

// 014-AC3: a watch the caller does not own, an unknown one or a non-UUID is a 400 at body.watch_ids[i]; past 500
// items is a 400. 014-AC4: watch_id with watch_ids is a 400, on create, update and preview.
func TestRuleWatchIDsRefused(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	mine := a.watches(t, alice, 1)[0]
	bobs := a.watches(t, bob, 1)[0]
	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r"}, http.StatusCreated, &r)
	const unknown = "00000000-0000-4000-8000-000000000000"
	many := make([]string, 501)
	for i := range many {
		many[i] = mine
	}
	for _, tt := range []struct {
		name string
		body map[string]any
		locs []string
	}{
		{"another user's", map[string]any{"watch_ids": []string{mine, bobs}}, []string{"body.watch_ids[1]"}},
		{"unknown after a duplicate", map[string]any{"watch_ids": []string{mine, mine, unknown}}, []string{"body.watch_ids[2]"}},
		{"not a UUID", map[string]any{"watch_ids": []string{"nope", mine}}, []string{"body.watch_ids[0]"}},
		{"two bad", map[string]any{"watch_ids": []string{bobs, mine, unknown}}, []string{"body.watch_ids[0]", "body.watch_ids[2]"}},
		{"alias to another user's", map[string]any{"watch_id": bobs}, []string{"body.watch_id"}},
		{"both", map[string]any{"watch_ids": []string{mine}, "watch_id": mine}, []string{"body.watch_id"}},
		{"both empty", map[string]any{"watch_ids": []string{}, "watch_id": ""}, []string{"body.watch_id"}},
		{"501 items", map[string]any{"watch_ids": many}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			create := map[string]any{"name": "made"}
			for k, v := range tt.body {
				create[k] = v
			}
			for _, c := range []struct {
				method, path string
				body         map[string]any
			}{
				{http.MethodPost, "/api/v1/rules", create},
				{http.MethodPatch, "/api/v1/rules/" + r.ID, tt.body},
			} {
				p := a.problemOf(t, alice, c.method, c.path, c.body, http.StatusBadRequest)
				if tt.locs != nil && !slices.Equal(p.locations(), tt.locs) {
					t.Errorf("%s %s: locations %v, want %v", c.method, c.path, p.locations(), tt.locs)
				}
			}
		})
	}
	// The preview reads only the caller's events, so another user's watch is no error there.
	for _, body := range []map[string]any{
		{"filter": "", "watch_ids": []string{mine}, "watch_id": mine},
		{"filter": "", "watch_ids": []string{"nope"}},
		{"filter": "", "watch_id": "nope"},
		{"filter": "", "watch_ids": many},
	} {
		a.problemOf(t, alice, http.MethodPost, "/api/v1/rules/preview", body, http.StatusBadRequest)
	}
	if n := a.countRows(t, `SELECT count(*) FROM rules`); n != 1 {
		t.Fatalf("%d rules, want the one created first", n)
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+r.ID, nil, http.StatusOK, &r)
	if len(r.WatchIDs) != 0 {
		t.Fatalf("refused updates changed the scope: %+v", r)
	}
}

// 014-AC4: watch_id A stores [A], watch_id "" stores all watches, on create and update. 014-AC6: watch_ids replaces
// the list, [] widens it to all watches, and an update sending neither keeps it.
func TestRuleScopeUpdate(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	w := a.watches(t, alice, 3)
	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r", "watch_id": w[0]}, http.StatusCreated, &r)
	if !slices.Equal(r.WatchIDs, []string{w[0]}) {
		t.Fatalf("create with watch_id: %+v", r)
	}
	var all ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "all", "watch_id": ""}, http.StatusCreated, &all)
	if all.WatchIDs == nil || len(all.WatchIDs) != 0 || a.countRows(t, `SELECT count(*) FROM rules WHERE id = $1 AND all_watches`, all.ID) != 1 {
		t.Fatalf("create with watch_id \"\": %+v", all)
	}
	for _, step := range []struct {
		body map[string]any
		want []string
	}{
		{map[string]any{"watch_ids": []string{w[2], w[1]}}, sortedIDs(w[1], w[2])},
		{map[string]any{"name": "renamed", "enabled": false}, sortedIDs(w[1], w[2])},
		{map[string]any{"watch_id": w[0]}, []string{w[0]}},
		{map[string]any{"filter": `event.type == "t"`}, []string{w[0]}},
		{map[string]any{"watch_id": ""}, []string{}},
		{map[string]any{"watch_ids": []string{w[1]}}, []string{w[1]}},
		{map[string]any{"watch_ids": []string{}}, []string{}},
	} {
		var got ruleJSON
		a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, step.body, http.StatusOK, &got)
		if !slices.Equal(got.WatchIDs, step.want) || got.WatchIDs == nil {
			t.Fatalf("after %v: %v, want %v", step.body, got.WatchIDs, step.want)
		}
		allWatches := len(step.want) == 0
		if n := a.countRows(t, `SELECT count(*) FROM rules WHERE id = $1 AND all_watches = $2`, r.ID, allWatches) +
			a.countRows(t, `SELECT count(*) FROM rule_watches WHERE rule_id = $1`, r.ID); n != 1+len(step.want) {
			t.Fatalf("after %v: stored scope disagrees (%d)", step.body, n)
		}
	}
	var audited int
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'rule.updated' AND target_id = $1
		AND after -> 'watch_ids' = '[]'::jsonb AND before -> 'watch_ids' = jsonb_build_array($2::text)`, r.ID, w[1]).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit of the last scope change: %d %v", audited, err)
	}
}

// 014-AC7: deleting A takes it out of a rule on [A, B], which keeps routing B; a rule on [A] alone goes with it.
func TestWatchDeleteLeavesRules(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	w := a.watches(t, alice, 2)
	var ab, onlyA ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "ab", "watch_ids": []string{w[0], w[1]}}, http.StatusCreated, &ab)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "a", "watch_ids": []string{w[0]}}, http.StatusCreated, &onlyA)
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/watches/"+w[0], nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+onlyA.ID, nil, http.StatusNotFound, nil)
	var got ruleJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+ab.ID, nil, http.StatusOK, &got)
	if !slices.Equal(got.WatchIDs, []string{w[1]}) || got.WatchID == nil || *got.WatchID != w[1] {
		t.Fatalf("rule on [A, B] after A's delete: %+v", got)
	}
	if n := a.countRows(t, `SELECT count(*) FROM rules WHERE id = $1 AND NOT all_watches`, ab.ID); n != 1 {
		t.Fatal("the rule widened to all watches")
	}
	if !a.routedTo(t, alice, w[1]) {
		t.Fatal("the rule stopped routing B")
	}
}

// 014-AC8: after A moves to bob, alice's rule on [A, B] routes B only and shows A without a name; it routes A
// again once A comes back.
func TestTransferKeepsSeveralWatchRule(t *testing.T) {
	a := newDevApp(t)
	alice, bob, admin := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "admin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	w := a.watches(t, alice, 2)
	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "ab", "watch_ids": []string{w[0], w[1]}}, http.StatusCreated, &r)
	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w[0]+"/transfer", map[string]any{"to_user_id": bob.id}, http.StatusOK, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+r.ID, nil, http.StatusOK, &r)
	if len(r.Watches) != 2 {
		t.Fatalf("after the transfer: %+v", r)
	}
	for _, x := range r.Watches {
		if (x.ID == w[0]) != (x.Name == nil) {
			t.Errorf("watch %s named %v", x.ID, x.Name)
		}
	}
	if a.routedTo(t, alice, w[0]) {
		t.Error("alice's rule routed the transferred watch")
	}
	if !a.routedTo(t, alice, w[1]) {
		t.Error("alice's rule stopped routing the watch she kept")
	}
	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w[0]+"/transfer", map[string]any{"to_user_id": alice.id}, http.StatusOK, nil)
	if !a.routedTo(t, alice, w[0]) {
		t.Error("alice's rule did not route the watch once back")
	}
}

// 014-AC8, 014-AC6: a rule keeps a transferred watch through an update that sends its list back, and can drop
// the kept one; another user's watch it never listed is still refused. Once back, A routes again.
func TestRuleUpdateKeepsTransferredWatch(t *testing.T) {
	a := newDevApp(t)
	alice, bob, admin := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "admin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	w := a.watches(t, alice, 2)
	c := a.watches(t, bob, 1)[0]
	var r, kept ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "ab", "watch_ids": []string{w[0], w[1]}}, http.StatusCreated, &r)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "kept", "watch_ids": []string{w[0], w[1]}}, http.StatusCreated, &kept)
	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w[0]+"/transfer", map[string]any{"to_user_id": bob.id}, http.StatusOK, nil)

	var got ruleJSON
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"name": "renamed", "watch_ids": []string{w[0], w[1]}}, http.StatusOK, &got)
	if got.Name != "renamed" || !slices.Equal(got.WatchIDs, sortedIDs(w[0], w[1])) {
		t.Fatalf("round trip with the transferred watch: %+v", got)
	}
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"watch_ids": []string{w[0]}}, http.StatusOK, &got)
	if !slices.Equal(got.WatchIDs, []string{w[0]}) {
		t.Fatalf("keeping the transferred watch alone: %+v", got)
	}
	p := a.problemOf(t, alice, http.MethodPatch, "/api/v1/rules/"+kept.ID, map[string]any{"watch_ids": []string{w[0], w[1], c}}, http.StatusBadRequest)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.watch_ids[2]" {
		t.Fatalf("adding bob's watch: %+v", p)
	}
	// A watch the rule dropped is a new one again: it must be owned to come back.
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+kept.ID, map[string]any{"watch_ids": []string{w[1]}}, http.StatusOK, nil)
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/rules/"+kept.ID, map[string]any{"watch_ids": []string{w[0], w[1]}}, http.StatusBadRequest)

	a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w[0]+"/transfer", map[string]any{"to_user_id": alice.id}, http.StatusOK, nil)
	if !a.routedTo(t, alice, w[0]) {
		t.Error("the kept rule did not route the watch once back")
	}
}

// 014-AC12: alice edits her rule on [A, B], sending A back though A moved to bob, while bob deletes A. Edit first:
// it keeps A, then the delete takes A out of the rule. Delete first: the edit no longer finds A listed, so
// sending it is a 400. Either way no deadlock (testpg fails on an unallowed retry) and no row on a deleted watch.
func TestRuleEditKeepingTransferredWatchRacingDelete(t *testing.T) {
	a := newDevApp(t)
	alice, bob, admin := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "admin")
	a.exec(t, `UPDATE sessions SET is_admin = true WHERE user_id = $1`, admin.id)
	ctx := context.Background()
	for _, editFirst := range []bool{true, false} {
		// A func per case: its deferred rollback frees a held lock when a check fails, or the waiters hang to the go test timeout.
		func() {
			w := a.watches(t, alice, 2)
			var r ruleJSON
			a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "race " + strconv.FormatBool(editFirst), "watch_ids": w}, http.StatusCreated, &r)
			a.mustCall(t, admin, http.MethodPost, "/api/v1/admin/watches/"+w[0]+"/transfer", map[string]any{"to_user_id": bob.id}, http.StatusOK, nil)
			hold, err := a.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(ctx) }()
			var edited, deleted <-chan int
			body := map[string]any{"name": "edited", "watch_ids": w}
			if editFirst {
				// The edit holds the rule and waits on A; the delete then waits on the rule.
				if _, err := hold.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR UPDATE`, w[0]); err != nil {
					t.Fatal(err)
				}
				edited = a.callAsync(alice, http.MethodPatch, "/api/v1/rules/"+r.ID, body)
				a.waitQueryLocked(t, "%FOR KEY SHARE OF w")
				deleted = a.callAsync(bob, http.MethodDelete, "/api/v1/watches/"+w[0], nil)
				a.waitQueryLocked(t, "SELECT 1 FROM rules WHERE id IN (SELECT rule_id FROM rule_watches%")
			} else {
				// The delete holds the rule and waits on A; the edit then waits on the rule.
				if _, err := hold.Exec(ctx, `SELECT 1 FROM watches WHERE id = $1 FOR KEY SHARE`, w[0]); err != nil {
					t.Fatal(err)
				}
				deleted = a.callAsync(bob, http.MethodDelete, "/api/v1/watches/"+w[0], nil)
				a.waitQueryLocked(t, "%FROM watches w%FOR UPDATE OF w")
				edited = a.callAsync(alice, http.MethodPatch, "/api/v1/rules/"+r.ID, body)
				a.waitQueryLocked(t, "%FROM rules WHERE id = $1 AND owner_id%")
			}
			if err := hold.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			wantEdit := http.StatusOK
			if !editFirst {
				wantEdit = http.StatusBadRequest
			}
			if code := <-edited; code != wantEdit {
				t.Fatalf("edit first %v: edit %d, want %d", editFirst, code, wantEdit)
			}
			if code := <-deleted; code != http.StatusNoContent {
				t.Fatalf("edit first %v: delete %d, want 204", editFirst, code)
			}
			var got ruleJSON
			a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+r.ID, nil, http.StatusOK, &got)
			if !slices.Equal(got.WatchIDs, []string{w[1]}) {
				t.Fatalf("edit first %v: rule after the race: %+v", editFirst, got)
			}
		}()
	}
	if n := a.countRows(t, `SELECT count(*) FROM rule_watches rw LEFT JOIN watches x ON x.id = rw.watch_id WHERE x.id IS NULL`); n != 0 {
		t.Fatalf("%d rule_watches rows on a deleted watch", n)
	}
}

// 014-AC10: the preview on [A, B] evaluates the last events of A and B only, however busy C is; watch_id scopes it
// too. Another user's watch adds no event.
func TestPreviewOnSeveralWatches(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	w := a.watches(t, alice, 3)
	bw := a.watches(t, bob, 1)[0]
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'bob')`, bw)
	for _, x := range []struct {
		watch string
		n     int
	}{{w[0], 2}, {w[1], 3}, {w[2], 30}} {
		a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) SELECT $1, 't', '{}', 'k' || g FROM generate_series(1, $2::int) g`, x.watch, x.n)
	}
	for _, tt := range []struct {
		body map[string]any
		want map[string]int
	}{
		{map[string]any{"filter": "", "watch_ids": []string{w[1], w[0]}}, map[string]int{w[0]: 2, w[1]: 3}},
		{map[string]any{"filter": "", "watch_id": w[0]}, map[string]int{w[0]: 2}},
		{map[string]any{"filter": "", "watch_ids": []string{}}, map[string]int{w[2]: 20}},
		// Bob's watch is no 400: a rule may keep a transferred watch, and it adds no event.
		{map[string]any{"filter": "", "watch_ids": []string{w[0], bw}}, map[string]int{w[0]: 2}},
	} {
		var pv previewJSON
		a.mustCall(t, alice, http.MethodPost, "/api/v1/rules/preview", tt.body, http.StatusOK, &pv)
		got := map[string]int{}
		for _, it := range pv.Items {
			got[it.Watch.ID]++
		}
		if len(got) != len(tt.want) {
			t.Errorf("%v: %v, want %v", tt.body, got, tt.want)
		}
		for k, v := range tt.want {
			if got[k] != v {
				t.Errorf("%v: %v, want %v", tt.body, got, tt.want)
			}
		}
	}
}

// 014-AC6, 014-AC12: a PATCH of watch_ids waiting behind another edit of the same rule applies its list over that
// edit's, and audits the list that edit left as before.
func TestRuleScopeUpdateAfterConcurrentEdit(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	w := a.watches(t, alice, 2)
	r := testpg.InsertRule(t, a.pool, testpg.Rule{Owner: alice.id, Name: "r", Watches: []string{w[0]}})
	ctx := context.Background()
	edit, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edit.Rollback(ctx) }()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM rules WHERE id = $1 FOR UPDATE`, []any{r}},
		{`UPDATE rules SET updated_at = now() WHERE id = $1`, []any{r}},
		{`DELETE FROM rule_watches WHERE rule_id = $1`, []any{r}},
		{`INSERT INTO rule_watches (rule_id, watch_id) VALUES ($1, $2)`, []any{r, w[1]}},
	} {
		if _, err := edit.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	patched := a.callAsync(alice, http.MethodPatch, "/api/v1/rules/"+r, map[string]any{"watch_ids": []string{w[0]}})
	a.waitLockWaiters(t, 1)
	if err := edit.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-patched; code != http.StatusOK {
		t.Fatalf("patch: %d, want 200", code)
	}
	var got ruleJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+r, nil, http.StatusOK, &got)
	if !slices.Equal(got.WatchIDs, []string{w[0]}) {
		t.Fatalf("rule after the PATCH: %v, want [%s]", got.WatchIDs, w[0])
	}
	var before []string
	if err := a.pool.QueryRow(ctx, `SELECT ARRAY(SELECT jsonb_array_elements_text(before -> 'watch_ids')) FROM audit_log
		WHERE action = 'rule.updated' AND target_id = $1`, r).Scan(&before); err != nil || !slices.Equal(before, []string{w[1]}) {
		t.Fatalf("audit before %v %v, want [%s]", before, err, w[1])
	}
}

// 014-AC3, 014-AC4: an owned watch's id in upper case is that watch, in watch_ids and watch_id, on create, update
// and preview; it is stored, deduplicated and returned canonical. A refusal echoes the id as sent.
func TestRuleWatchIDsUpperCase(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	w := a.watches(t, alice, 2)
	bobs := a.watches(t, bob, 1)[0]
	for _, sent := range []string{strings.ToUpper(bobs), "{" + bobs + "}", "urn:uuid:" + bobs} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r", "watch_ids": []string{w[0], sent}}, http.StatusBadRequest)
		if len(p.Errors) != 1 || p.Errors[0].Location != "body.watch_ids[1]" || p.Errors[0].Value != sent {
			t.Fatalf("another user's watch sent as %q: %+v", sent, p.Errors)
		}
	}
	// 014-AC3: a duplicate in two spellings is refused once, at its first index, as first sent.
	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r", "watch_ids": []string{w[0], strings.ToUpper(bobs), bobs}}, http.StatusBadRequest)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.watch_ids[1]" || p.Errors[0].Value != strings.ToUpper(bobs) {
		t.Fatalf("another user's watch sent twice: %+v", p.Errors)
	}
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 't', '{}', 'k'), ($2, 't', '{}', 'k')`, w[0], w[1])
	up := strings.ToUpper
	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r", "watch_ids": []string{up(w[0]), w[0]}}, http.StatusCreated, &r)
	if !slices.Equal(r.WatchIDs, []string{w[0]}) {
		t.Fatalf("create with watch_ids: %v", r.WatchIDs)
	}
	var alias ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "alias", "watch_id": up(w[1])}, http.StatusCreated, &alias)
	if !slices.Equal(alias.WatchIDs, []string{w[1]}) {
		t.Fatalf("create with watch_id: %v", alias.WatchIDs)
	}
	for _, step := range []struct {
		body map[string]any
		want []string
	}{
		{map[string]any{"watch_ids": []string{up(w[1]), up(w[0])}}, sortedIDs(w[0], w[1])},
		{map[string]any{"watch_id": up(w[1])}, []string{w[1]}},
	} {
		a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, step.body, http.StatusOK, &r)
		if !slices.Equal(r.WatchIDs, step.want) {
			t.Fatalf("update %v: %v, want %v", step.body, r.WatchIDs, step.want)
		}
	}
	for _, body := range []map[string]any{
		{"filter": "", "watch_ids": []string{up(w[0])}},
		{"filter": "", "watch_id": up(w[0])},
	} {
		var pv previewJSON
		a.mustCall(t, alice, http.MethodPost, "/api/v1/rules/preview", body, http.StatusOK, &pv)
		if len(pv.Items) != 1 || pv.Items[0].Watch.ID != w[0] {
			t.Fatalf("preview %v: %+v", body, pv.Items)
		}
	}
}
