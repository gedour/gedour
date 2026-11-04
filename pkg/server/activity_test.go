package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type activityWatch struct {
	watchJSON
	NextRunAt           time.Time  `json:"next_run_at"`
	LastRunAt           *time.Time `json:"last_run_at"`
	LastStatus          *string    `json:"last_status"`
	LastError           *string    `json:"last_error"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	Running             bool       `json:"running"`
}

type eventsPage struct {
	Items []struct {
		ID         int64          `json:"id"`
		Type       string         `json:"type"`
		OccurredAt *time.Time     `json:"occurred_at"`
		Payload    map[string]any `json:"payload"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func (a *devApp) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := a.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (a *devApp) countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Events, states and run-now follow the watch's access rules (001-AC5): read for the lists, write to run.
func TestWatchActivity(t *testing.T) {
	a := newDevApp(t)
	alice, bob, carol, dave := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "carol"), a.signIn(t, "dave")
	var w activityWatch
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	if w.LastStatus != nil || w.LastRunAt != nil || w.NextRunAt.IsZero() {
		t.Fatalf("a new watch is pending its first run: %+v", w)
	}
	base := "/api/v1/watches/" + w.ID
	a.mustCall(t, alice, http.MethodPost, base+"/shares", map[string]any{"email": bob.email, "permission": "ro"}, http.StatusCreated, nil)
	a.mustCall(t, alice, http.MethodPost, base+"/shares", map[string]any{"email": dave.email, "permission": "rw"}, http.StatusCreated, nil)

	a.exec(t, `INSERT INTO watch_states (watch_id, state, state_hash) VALUES ($1, '{"releases":[]}', 'h1'), ($1, '{"releases":[{"tag":"v1"}]}', 'h2')`, w.ID)
	for i, tag := range []string{"v1", "v2", "v3"} {
		a.exec(t, `INSERT INTO events (watch_id, type, occurred_at, payload, dedup_key) VALUES ($1, 'release.published', $2, $3, $4)`,
			w.ID, time.Date(2026, 11, i+1, 0, 0, 0, 0, time.UTC), map[string]any{"tag": tag}, tag)
	}
	a.exec(t, `UPDATE watches SET last_run_at = now(), last_status = 'error', last_error = 'HTTP 502', consecutive_failures = 2 WHERE id = $1`, w.ID)

	a.mustCall(t, bob, http.MethodGet, base, nil, http.StatusOK, &w)
	if *w.LastStatus != "error" || *w.LastError != "HTTP 502" || w.ConsecutiveFailures != 2 || w.LastRunAt == nil {
		t.Fatalf("status fields: %+v", w)
	}

	var page eventsPage
	a.mustCall(t, bob, http.MethodGet, base+"/events?limit=2", nil, http.StatusOK, &page)
	if len(page.Items) != 2 || page.Items[0].Payload["tag"] != "v3" || page.Items[1].Payload["tag"] != "v2" || page.NextCursor == "" ||
		page.Items[0].OccurredAt == nil {
		t.Fatalf("page 1: %+v", page)
	}
	cursor := page.NextCursor
	page = eventsPage{}
	a.mustCall(t, alice, http.MethodGet, base+"/events?limit=2&cursor="+cursor, nil, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Items[0].Payload["tag"] != "v1" || page.NextCursor != "" {
		t.Fatalf("page 2: %+v", page)
	}
	for _, c := range []string{"abc", "-1", "0"} {
		a.mustCall(t, alice, http.MethodGet, base+"/events?cursor="+c, nil, http.StatusBadRequest, nil)
	}

	var states struct {
		Items []struct {
			ID    int64           `json:"id"`
			State json.RawMessage `json:"state"`
		} `json:"items"`
	}
	a.mustCall(t, bob, http.MethodGet, base+"/states?limit=5", nil, http.StatusOK, &states)
	if len(states.Items) != 2 || string(states.Items[0].State) != `{"releases":[{"tag":"v1"}]}` {
		t.Fatalf("states: %+v", states)
	}

	for _, path := range []string{"/events", "/states", ""} {
		a.mustCall(t, carol, http.MethodGet, base+path, nil, http.StatusNotFound, nil)
	}

	// Run now: rw and owner, audited; ro gets 403, a stranger 404, a paused watch 409.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.mustCall(t, dave, http.MethodPost, base+"/run", nil, http.StatusAccepted, &w)
	if time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("run now left next_run_at at %v", w.NextRunAt)
	}
	a.mustCall(t, alice, http.MethodPost, base+"/run", nil, http.StatusAccepted, nil)
	a.mustCall(t, bob, http.MethodPost, base+"/run", nil, http.StatusForbidden, nil)
	a.mustCall(t, carol, http.MethodPost, base+"/run", nil, http.StatusNotFound, nil)
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'watch.run_requested'`, w.ID); n != 2 {
		t.Fatalf("run audit rows %d", n)
	}
	a.mustCall(t, alice, http.MethodPost, base+"/pause", nil, http.StatusOK, nil)
	a.mustCall(t, alice, http.MethodPost, base+"/run", nil, http.StatusConflict, nil)

	// Resuming runs at once.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.mustCall(t, dave, http.MethodPost, base+"/resume", nil, http.StatusOK, &w)
	if time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("resume left next_run_at at %v", w.NextRunAt)
	}

	// Audit rows carry the watch's spec only: the worker's fields would make every diff noisy.
	var keys int
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log a, jsonb_object_keys(a.before || a.after) k
		WHERE a.target_id = $1 AND a.action = 'watch.resumed'
		AND k IN ('next_run_at', 'last_run_at', 'last_status', 'last_error', 'consecutive_failures', 'running')`, w.ID).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'watch.resumed' AND after ? 'schedule'`, w.ID); keys != 0 || n != 1 {
		t.Fatalf("resume audit row: %d status keys, %d rows with the spec", keys, n)
	}

	// A claimed watch reads as running.
	if w.Running {
		t.Fatal("idle watch reported running")
	}
	a.exec(t, `UPDATE watches SET claim_token = gen_random_uuid(), leased_until = now() + interval '5 minutes' WHERE id = $1`, w.ID)
	a.mustCall(t, bob, http.MethodGet, base, nil, http.StatusOK, &w)
	if !w.Running {
		t.Fatal("leased watch not reported running")
	}
	a.exec(t, `UPDATE watches SET leased_until = now() - interval '1 second' WHERE id = $1`, w.ID)
	a.mustCall(t, bob, http.MethodGet, base, nil, http.StatusOK, &w)
	if w.Running {
		t.Fatal("expired lease reported running")
	}
}

// A changed target drops the old baseline and runs at once; a rename keeps both.
func TestRetargetResetsBaseline(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w activityWatch
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	base := "/api/v1/watches/" + w.ID
	seed := func() {
		a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
		a.exec(t, `INSERT INTO watch_states (watch_id, state, state_hash) VALUES ($1, '{}', 'h')`, w.ID)
		a.exec(t, `INSERT INTO http_cache (watch_id, url, etag) VALUES ($1, 'https://api.github.com/x', '"e"') ON CONFLICT DO NOTHING`, w.ID)
	}
	seed()
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"name": "renamed", "config": map[string]any{"repo": "golang/go"}}, http.StatusOK, &w)
	if n := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID); n != 1 || time.Until(w.NextRunAt) < 24*time.Hour {
		t.Fatalf("same normalized config reset the watch: states=%d next=%v", n, w.NextRunAt)
	}
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/tools"}}, http.StatusOK, &w)
	if n := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID) +
		a.countRows(t, `SELECT count(*) FROM http_cache WHERE watch_id = $1`, w.ID); n != 0 || time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("retarget kept %d rows, next=%v", n, w.NextRunAt)
	}
	seed()
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"schedule": "30m"}, http.StatusOK, &w)
	if n := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID); n != 1 || time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("schedule change: states=%d next=%v", n, w.NextRunAt)
	}
}
