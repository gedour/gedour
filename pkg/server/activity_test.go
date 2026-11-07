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

// Events, states and run-now follow the watch's access rule (001-AC5): its owner only.
func TestWatchActivity(t *testing.T) {
	a := newDevApp(t)
	alice, carol := a.signIn(t, "alice"), a.signIn(t, "carol")
	var w activityWatch
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	if w.LastStatus != nil || w.LastRunAt != nil || w.NextRunAt.IsZero() {
		t.Fatalf("a new watch is pending its first run: %+v", w)
	}
	base := "/api/v1/watches/" + w.ID

	a.exec(t, `INSERT INTO watch_states (watch_id, state, state_hash) VALUES ($1, '{"releases":[]}', 'h1'), ($1, '{"releases":[{"tag":"v1"}]}', 'h2')`, w.ID)
	for i, tag := range []string{"v1", "v2", "v3"} {
		a.exec(t, `INSERT INTO events (watch_id, type, occurred_at, payload, dedup_key) VALUES ($1, 'release.published', $2, $3, $4)`,
			w.ID, time.Date(2026, 11, i+1, 0, 0, 0, 0, time.UTC), map[string]any{"tag": tag}, tag)
	}
	a.exec(t, `UPDATE watches SET last_run_at = now(), last_status = 'error', last_error = 'HTTP 502', consecutive_failures = 2 WHERE id = $1`, w.ID)

	a.mustCall(t, alice, http.MethodGet, base, nil, http.StatusOK, &w)
	if *w.LastStatus != "error" || *w.LastError != "HTTP 502" || w.ConsecutiveFailures != 2 || w.LastRunAt == nil {
		t.Fatalf("status fields: %+v", w)
	}

	var page eventsPage
	a.mustCall(t, alice, http.MethodGet, base+"/events?limit=2", nil, http.StatusOK, &page)
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
	a.mustCall(t, alice, http.MethodGet, base+"/states?limit=5", nil, http.StatusOK, &states)
	if len(states.Items) != 2 || string(states.Items[0].State) != `{"releases":[{"tag":"v1"}]}` {
		t.Fatalf("states: %+v", states)
	}

	for _, path := range []string{"/events", "/states", ""} {
		a.mustCall(t, carol, http.MethodGet, base+path, nil, http.StatusNotFound, nil)
	}

	// Run now: the owner, audited; a stranger gets 404, a paused watch 409.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.mustCall(t, alice, http.MethodPost, base+"/run", nil, http.StatusAccepted, &w)
	if time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("run now left next_run_at at %v", w.NextRunAt)
	}
	a.mustCall(t, alice, http.MethodPost, base+"/run", nil, http.StatusAccepted, nil)
	a.mustCall(t, carol, http.MethodPost, base+"/run", nil, http.StatusNotFound, nil)
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'watch.run_requested'`, w.ID); n != 2 {
		t.Fatalf("run audit rows %d", n)
	}
	a.mustCall(t, alice, http.MethodPost, base+"/pause", nil, http.StatusOK, nil)
	a.mustCall(t, alice, http.MethodPost, base+"/run", nil, http.StatusConflict, nil)

	// Resuming runs at once.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.mustCall(t, alice, http.MethodPost, base+"/resume", nil, http.StatusOK, &w)
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
	a.mustCall(t, alice, http.MethodGet, base, nil, http.StatusOK, &w)
	if !w.Running {
		t.Fatal("leased watch not reported running")
	}
	a.exec(t, `UPDATE watches SET leased_until = now() - interval '1 second' WHERE id = $1`, w.ID)
	a.mustCall(t, alice, http.MethodGet, base, nil, http.StatusOK, &w)
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
	// 008: checking a new-items box keeps the states, drops the ETags and makes the watch due for its baseline.
	seed()
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/tools", "new_issues": true}}, http.StatusOK, &w)
	if s, c := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID),
		a.countRows(t, `SELECT count(*) FROM http_cache WHERE watch_id = $1`, w.ID); s != 1 || c != 0 || time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("box checked: states=%d cache=%d next=%v", s, c, w.NextRunAt)
	}
	// A box staying on is no change; the second box going on is.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/tools", "new_issues": true}}, http.StatusOK, &w)
	if time.Until(w.NextRunAt) < 24*time.Hour {
		t.Fatalf("box staying on made the watch due: next=%v", w.NextRunAt)
	}
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/tools", "new_issues": true, "new_prs": true}}, http.StatusOK, &w)
	if time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("second box checked: next=%v", w.NextRunAt)
	}
	// 011-AC3: checking new_discussions makes the watch due too, and keeps the states.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/tools", "new_issues": true, "new_prs": true, "new_discussions": true}}, http.StatusOK, &w)
	if s := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID); s != 1 || time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("discussions box checked: states=%d next=%v", s, w.NextRunAt)
	}
	// Unchecking changes nothing on the schedule.
	a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
	a.exec(t, `INSERT INTO http_cache (watch_id, url, etag) VALUES ($1, 'https://api.github.com/x', '"e"')`, w.ID)
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/tools"}}, http.StatusOK, &w)
	if s, c := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID),
		a.countRows(t, `SELECT count(*) FROM http_cache WHERE watch_id = $1`, w.ID); s != 1 || c != 0 || time.Until(w.NextRunAt) < 24*time.Hour {
		t.Fatalf("box unchecked: states=%d cache=%d next=%v", s, c, w.NextRunAt)
	}
	// A paused watch stays paused: its first run after the resume takes the baseline.
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"paused": true, "config": map[string]any{"repo": "golang/tools", "new_issues": true}}, http.StatusOK, &w)
	if a.countRows(t, `SELECT count(*) FROM watches WHERE id = $1 AND paused`, w.ID) != 1 {
		t.Fatal("box checked on a paused watch resumed it")
	}
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"paused": false, "config": map[string]any{"repo": "golang/tools"}}, http.StatusOK, &w)
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/go", "new_prs": true}}, http.StatusOK, &w)
	if n := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID); n != 0 || time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("repo and box changed: states=%d next=%v", n, w.NextRunAt)
	}
	seed()
	a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"schedule": "30m"}, http.StatusOK, &w)
	if n := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID); n != 1 || time.Until(w.NextRunAt) > time.Minute {
		t.Fatalf("schedule change: states=%d next=%v", n, w.NextRunAt)
	}
}

// 011-AC13: toggling reactions keeps the states, and checking it does not make the watch due: the next run baselines it.
func TestReactionsBoxKeepsSchedule(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var w activityWatch
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, &w)
	base := "/api/v1/watches/" + w.ID
	a.exec(t, `INSERT INTO watch_states (watch_id, state, state_hash) VALUES ($1, '{}', 'h')`, w.ID)
	for _, on := range []bool{true, false} {
		a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days' WHERE id = $1`, w.ID)
		w.Config = nil // decoding merges into a map
		a.mustCall(t, alice, http.MethodPatch, base, map[string]any{"config": map[string]any{"repo": "golang/go", "number": 1, "reactions": on}}, http.StatusOK, &w)
		if s := a.countRows(t, `SELECT count(*) FROM watch_states WHERE watch_id = $1`, w.ID); s != 1 || time.Until(w.NextRunAt) < 24*time.Hour || (w.Config["reactions"] == true) != on {
			t.Fatalf("reactions %v: states=%d next=%v", on, s, w.NextRunAt)
		}
	}
}

// Run all: every watch the caller owns, paused ones left out, one audit row each.
func TestRunAllWatches(t *testing.T) {
	a := newDevApp(t)
	alice, bob, dave := a.signIn(t, "alice"), a.signIn(t, "bob"), a.signIn(t, "dave")
	var w1, w2, w3, w4 activityWatch
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w1)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, &w2)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[2], http.StatusCreated, &w3)
	a.mustCall(t, dave, http.MethodPost, "/api/v1/watches", sampleWatches[3], http.StatusCreated, &w4)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w3.ID+"/pause", nil, http.StatusOK, nil)
	later := func() { a.exec(t, `UPDATE watches SET next_run_at = now() + interval '3 days'`) }
	due := func() int {
		return a.countRows(t, `SELECT count(*) FROM watches WHERE next_run_at < now() + interval '1 minute'`)
	}
	var got struct {
		Scheduled int `json:"scheduled"`
	}

	later()
	a.mustCall(t, dave, http.MethodPost, "/api/v1/watches/run", nil, http.StatusAccepted, &got)
	if got.Scheduled != 1 || due() != 1 || a.countRows(t, `SELECT count(*) FROM watches WHERE id = $1 AND next_run_at < now() + interval '1 minute'`, w4.ID) != 1 {
		t.Fatalf("dave: scheduled %d, due %d", got.Scheduled, due())
	}
	later()
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches/run", nil, http.StatusAccepted, &got)
	if got.Scheduled != 0 || due() != 0 {
		t.Fatalf("stranger: scheduled %d, due %d", got.Scheduled, due())
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/run", nil, http.StatusAccepted, &got)
	if got.Scheduled != 2 || due() != 2 {
		t.Fatalf("owner: scheduled %d, due %d (paused must stay)", got.Scheduled, due())
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'watch.run_requested' AND after->>'name' <> ''`); n != 3 {
		t.Fatalf("run audit rows %d", n)
	}
}
