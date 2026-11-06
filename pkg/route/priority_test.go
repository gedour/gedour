package route

import (
	"context"
	"testing"
)

func (e *env) ruleAt(t *testing.T, owner string, watch *string, filter, priority string) string {
	t.Helper()
	id := e.rule(t, owner, watch, filter)
	e.setPriority(t, id, priority)
	return id
}

func (e *env) setPriority(t *testing.T, rule, priority string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE rules SET priority = $2 WHERE id = $1`, rule, priority); err != nil {
		t.Fatal(err)
	}
}

// itemPriority fails unless the user has exactly one item for the event.
func (e *env) itemPriority(t *testing.T, user string, event int64) string {
	t.Helper()
	if n := e.count(t, `SELECT count(*) FROM inbox_items WHERE user_id = $1 AND event_id = $2`, user, event); n != 1 {
		t.Fatalf("%d inbox items, want 1", n)
	}
	var p string
	e.scalar(t, &p, `SELECT priority FROM inbox_items WHERE user_id = $1 AND event_id = $2`, user, event)
	return p
}

// 005-AC2, 005-AC3: one item per user, high when any rule that matched is high, whatever the order they run in.
func TestRoutingPriority(t *testing.T) {
	const (
		match  = ""
		miss   = `event.type == "none"`
		broken = `event.payload.tag > 3`
	)
	type rule struct{ filter, priority string }
	e := newEnv(t)
	for _, tt := range []struct {
		name  string
		rules []rule
		want  string
	}{
		{"high", []rule{{match, priorityHigh}}, priorityHigh},
		{"normal", []rule{{match, priorityNormal}}, priorityNormal},
		{"normal then high", []rule{{match, priorityNormal}, {match, priorityHigh}}, priorityHigh},
		{"high then normal", []rule{{match, priorityHigh}, {match, priorityNormal}}, priorityHigh},
		{"high between normals", []rule{{match, priorityNormal}, {match, priorityHigh}, {match, priorityNormal}}, priorityHigh},
		{"a high rule that misses", []rule{{miss, priorityHigh}, {match, priorityNormal}}, priorityNormal},
		{"a high rule that fails", []rule{{broken, priorityHigh}, {match, priorityNormal}}, priorityNormal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			user := e.user(t, tt.name)
			w := e.watch(t, user)
			for _, r := range tt.rules {
				e.ruleAt(t, user, &w, r.filter, r.priority)
			}
			ev := e.event(t, w, "release.published", "v1")
			routeAll(t, e.router())
			if got := e.itemPriority(t, user, ev); got != tt.want {
				t.Fatalf("priority %q, want %q", got, tt.want)
			}
		})
	}
}

// 005-AC4: two users watching the same target each get the priority of their own rules, whichever rule runs first.
func TestRoutingPriorityPerUser(t *testing.T) {
	for _, tt := range []struct {
		name  string
		first bool // alice's rule is created, so run, first
		alice string
		bob   string
	}{
		{name: "alice high first", first: true, alice: priorityHigh, bob: priorityNormal},
		{name: "alice high last", alice: priorityHigh, bob: priorityNormal},
		{name: "bob high first", alice: priorityNormal, bob: priorityHigh},
		{name: "bob high last", first: true, alice: priorityNormal, bob: priorityHigh},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			alice, bob := e.user(t, "alice"), e.user(t, "bob")
			wa, wb := e.watch(t, alice), e.watch(t, bob)
			if tt.first {
				e.ruleAt(t, alice, &wa, "", tt.alice)
				e.ruleAt(t, bob, &wb, "", tt.bob)
			} else {
				e.ruleAt(t, bob, &wb, "", tt.bob)
				e.ruleAt(t, alice, &wa, "", tt.alice)
			}
			ea, eb := e.event(t, wa, "release.published", "v1"), e.event(t, wb, "release.published", "v1")
			routeAll(t, e.router())
			if a, b := e.itemPriority(t, alice, ea), e.itemPriority(t, bob, eb); a != tt.alice || b != tt.bob {
				t.Fatalf("alice %q, bob %q; want %q, %q", a, b, tt.alice, tt.bob)
			}
		})
	}
}

// 005-AC6: a rule's edit applies to the next events; stored items keep their level, even when replayed.
func TestPrioritySetOnce(t *testing.T) {
	for _, tt := range []struct{ from, to string }{
		{priorityHigh, priorityNormal},
		{priorityNormal, priorityHigh},
	} {
		t.Run(tt.from+" to "+tt.to, func(t *testing.T) {
			e := newEnv(t)
			alice := e.user(t, "alice")
			w := e.watch(t, alice)
			r := e.ruleAt(t, alice, &w, "", tt.from)
			old := e.event(t, w, "release.published", "v1")
			routeAll(t, e.router())

			e.setPriority(t, r, tt.to)
			next := e.event(t, w, "release.published", "v2")
			routeAll(t, e.router())
			if got := e.itemPriority(t, alice, next); got != tt.to {
				t.Fatalf("next event: %q, want %q", got, tt.to)
			}
			if got := e.itemPriority(t, alice, old); got != tt.from {
				t.Fatalf("existing item: %q, want %q", got, tt.from)
			}

			if _, err := e.pool.Exec(context.Background(), `UPDATE events SET routed_at = NULL WHERE id = $1`, old); err != nil {
				t.Fatal(err)
			}
			routeAll(t, e.router())
			if got := e.itemPriority(t, alice, old); got != tt.from {
				t.Fatalf("replayed item: %q, want %q", got, tt.from)
			}
		})
	}
}
