package collect

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/gedour/gedour/internal/testupstream"
)

func schedulesCfg(apiURL, aliveOn string, missedRuns int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":%q,"missed_runs":%d,"warn_days":0,"auto_enable":false}`, apiURL, aliveOn, missedRuns))
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// schedulesCollect collects once at now with prev as the stored state.
func schedulesCollect(t *testing.T, cfg, prev json.RawMessage, now time.Time, cred *Credential) (json.RawMessage, error) {
	t.Helper()
	f := fetcher(t)
	f.Now = func() time.Time { return now }
	f.Prev = prev
	return Collectors()["github_schedules"].Collect(t.Context(), f, cfg, cred)
}

func schedulesRun(t *testing.T, cfg, prev json.RawMessage, now time.Time) json.RawMessage {
	t.Helper()
	s, err := schedulesCollect(t, cfg, prev, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func schedulesDiff(t *testing.T, old, cur json.RawMessage) []string {
	t.Helper()
	events, err := Collectors()["github_schedules"].Diff(old, cur)
	if err != nil {
		t.Fatal(err)
	}
	return summary(t, events)
}

const (
	mirrorPath = ".github/workflows/mirror.yml"
	mirrorURL  = "https://github.com/acme/widgets/blob/main/.github/workflows/mirror.yml"
)

// 019-AC2, 019-AC3, 019-AC4, 019-AC9: fixture a keeps the two scheduled workflows only; an unchanged second run fetches
// no file and emits nothing; fixture b fetches the one changed file and re-enables mirror.yml. The fake dates the last
// runs from its clock, so the outcome holds years on, past the point where fixed dates would make the yearly crons stale.
func TestGitHubSchedulesFixtures(t *testing.T) {
	for _, start := range []string{"2026-11-04T12:00:00Z", "2029-02-01T12:00:00Z", "2041-06-01T12:00:00Z"} {
		t.Run(start, func(t *testing.T) {
			up := testupstream.New()
			srv := httptest.NewServer(up)
			defer srv.Close()
			cfg := schedulesCfg(srv.URL, "success", 3)
			now := at(start)
			run := func(prev json.RawMessage, now time.Time) json.RawMessage {
				up.SetNow(func() time.Time { return now })
				return schedulesRun(t, cfg, prev, now)
			}
			file := func(name string) int { return up.Hits("/repos/acme/widgets/contents/.github/workflows/" + name) }
			runs := func(id int) int { return up.Hits(fmt.Sprintf("/repos/acme/widgets/actions/workflows/%d/runs", id)) }

			a := run(nil, now)
			var s SchedulesState
			if err := json.Unmarshal(a, &s); err != nil {
				t.Fatal(err)
			}
			// 019-AC2: push-only, on: [push] and the dynamic CodeQL workflow stay out of the state.
			var got []string
			for _, w := range s.Workflows {
				got = append(got, fmt.Sprintf("%s %s %v %v", w.Path, w.State, w.Stale, w.Crons))
			}
			want := []string{".github/workflows/mirror.yml disabled_inactivity false [{0 3 1 1 * }]",
				".github/workflows/yearly.yml active false [{0 1 1 1 * Europe/Paris}]"}
			if strings.Join(got, "|") != strings.Join(want, "|") || len(s.Unscheduled) != 2 || s.Unscheduled[".github/workflows/ci.yml"] == "" || s.Unscheduled[".github/workflows/lint.yml"] == "" {
				t.Fatalf("state %s", a)
			}
			if la := s.Workflows[1].LastAliveAt; la == nil || !la.Equal(now.Add(-time.Hour)) {
				t.Fatalf("yearly last alive %v", la)
			}
			// Runs are read for active scheduled workflows only.
			if runs(1001) != 0 || runs(1002) != 1 || runs(1003) != 0 || runs(1005) != 0 {
				t.Fatalf("runs calls %d %d %d %d", runs(1001), runs(1002), runs(1003), runs(1005))
			}
			// 019-AC9: the first run reports the workflow already disabled, and nothing about the healthy one.
			check(t, schedulesDiff(t, json.RawMessage("null"), a), []string{
				`schedule.disabled .github/workflows/mirror.yml {"last_alive_at":null,"path":".github/workflows/mirror.yml","state":"disabled_inactivity","url":"` + mirrorURL + `","workflow":"Mirror"}`,
			})

			// 019-AC3: unchanged shas, no file fetched again.
			a2 := run(a, now.Add(time.Hour))
			for _, name := range []string{"mirror.yml", "yearly.yml", "ci.yml", "lint.yml"} {
				if file(name) != 1 {
					t.Fatalf("%s fetched %d times", name, file(name))
				}
			}
			// 019-AC4: the unchanged run emits nothing.
			check(t, schedulesDiff(t, a, a2), []string{})

			// 019-AC3: fixture b changes ci.yml's sha only, so only ci.yml is fetched again.
			up.SetFixture("b")
			b := run(a2, now.Add(2*time.Hour))
			for name, n := range map[string]int{"mirror.yml": 1, "yearly.yml": 1, "ci.yml": 2, "lint.yml": 1} {
				if file(name) != n {
					t.Fatalf("%s fetched %d times, want %d", name, file(name), n)
				}
			}
			// 019-AC4: back to active emits one schedule.enabled, with the last success from the runs call.
			check(t, schedulesDiff(t, a2, b), []string{
				`schedule.enabled .github/workflows/mirror.yml {"last_alive_at":"` + now.Add(time.Hour).Format(time.RFC3339) +
					`","path":".github/workflows/mirror.yml","state":"active","url":"` + mirrorURL + `","workflow":"Mirror"}`,
			})
			// 019-AC9: a healthy repository's first run emits nothing.
			check(t, schedulesDiff(t, json.RawMessage("null"), b), []string{})
		})
	}
}

// 019-AC9: a yearly workflow whose last run is four years old: the first run reports it stale beside the disabled one.
func TestGitHubSchedulesFirstRunReports(t *testing.T) {
	up := testupstream.New()
	up.SetNow(func() time.Time { return at("2026-01-01T01:00:12Z") })
	srv := httptest.NewServer(up)
	defer srv.Close()
	a := schedulesRun(t, schedulesCfg(srv.URL, "success", 3), nil, at("2030-01-01T12:00:00Z"))
	check(t, schedulesDiff(t, json.RawMessage("null"), a), []string{
		`schedule.disabled .github/workflows/mirror.yml {"last_alive_at":null,"path":".github/workflows/mirror.yml","state":"disabled_inactivity","url":"` + mirrorURL + `","workflow":"Mirror"}`,
		// Europe/Paris 01:00 on 1 January is 00:00 UTC; 2027, 2028, 2029 and 2030 were missed, past missed_runs.
		`schedule.stale .github/workflows/yearly.yml {"expected_at":"2027-01-01T00:00:00Z","last_alive_at":"2026-01-01T00:00:12Z","missed":4,"path":".github/workflows/yearly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/yearly.yml","workflow":"Yearly report"}`,
	})
}

// A workflow file deleted leaves the directory listing while GitHub still lists the workflow as deleted: it leaves
// the state with no event (019 decisions).
func TestGitHubSchedulesDeletedFile(t *testing.T) {
	var mu sync.Mutex
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		state, files := "active", `[{"path":".github/workflows/gone.yml","sha":"g1","type":"file"},{"path":".github/workflows/keep.yml","sha":"k1","type":"file"}]`
		if deleted {
			state, files = "deleted", `[{"path":".github/workflows/keep.yml","sha":"k1","type":"file"}]`
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = fmt.Fprintf(w, `{"workflows":[{"id":1,"name":"Gone","path":".github/workflows/gone.yml","state":%q,"updated_at":"2026-11-01T00:00:00Z"},
				{"id":2,"name":"Keep","path":".github/workflows/keep.yml","state":"active","updated_at":"2026-11-01T00:00:00Z"}]}`, state)
		case strings.HasSuffix(r.URL.Path, "/contents/.github/workflows"):
			_, _ = w.Write([]byte(files))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/contents/"):
			_, _ = w.Write([]byte("on:\n  schedule:\n    - cron: '0 0 1 1 *'\n"))
		case strings.HasSuffix(r.URL.Path, "/runs"):
			_, _ = w.Write([]byte(`{"workflow_runs":[{"created_at":"2026-11-03T00:00:00Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := schedulesCfg(srv.URL, "success", 3)
	now := at("2026-11-04T12:00:00Z")
	before := schedulesRun(t, cfg, nil, now)
	mu.Lock()
	deleted = true
	mu.Unlock()
	after := schedulesRun(t, cfg, before, now.Add(time.Hour))
	var s SchedulesState
	if err := json.Unmarshal(after, &s); err != nil || len(s.Workflows) != 1 || s.Workflows[0].Path != ".github/workflows/keep.yml" ||
		!strings.Contains(string(before), "gone.yml") {
		t.Fatalf("before %s\nafter %s", before, after)
	}
	check(t, schedulesDiff(t, before, after), []string{})
}

// hourlyUpstream is one hourly workflow whose runs call answers by the status filter.
type hourlyUpstream struct {
	mu        sync.Mutex
	state     string // workflow state, active when empty
	updatedAt string // workflow updated_at, 2026-01-01 when empty
	cron      string // hourly when empty
	success   string // created_at of the last success
	completed string // created_at of the last run, any result
	private   bool
	commit    string // committer date of the last default-branch commit, none when empty
	queries   []string
	paths     []string
	// The re-enable PUT answers putStatus, 204 when 0 (then the workflow is listed active), a dropped connection when -1.
	putStatus int
	putHeader http.Header
	puts      int
}

func (u *hourlyUpstream) set(fn func(u *hourlyUpstream)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fn(u)
}

// putCount reads under the lock: a dropped connection's handler may still be running when the client returns.
func (u *hourlyUpstream) putCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.puts
}

func (u *hourlyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.paths = append(u.paths, r.URL.Path)
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widgets/actions/workflows/7/enable":
		u.puts++
		maps.Copy(w.Header(), u.putHeader)
		switch u.putStatus {
		case 0:
			u.state = ""
			w.WriteHeader(http.StatusNoContent)
		case -1:
			panic(http.ErrAbortHandler)
		default:
			http.Error(w, `{"message":"Resource not accessible by personal access token","documentation_url":"https://docs.github.com"}`, u.putStatus)
		}
	case r.Method != http.MethodGet:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	case r.URL.Path == "/repos/acme/widgets":
		_, _ = fmt.Fprintf(w, `{"full_name":"acme/widgets","private":%t,"pushed_at":"2030-01-01T00:00:00Z"}`, u.private)
	case r.URL.Path == "/repos/acme/widgets/commits" && r.URL.RawQuery == "per_page=1":
		if u.commit == "" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		// The author date is older: the committer date is the one that counts.
		_, _ = fmt.Fprintf(w, `[{"sha":"c1","commit":{"author":{"date":"2020-01-01T00:00:00Z"},"committer":{"date":%q}}}]`, u.commit)
	case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
		state, updated := cmp.Or(u.state, "active"), cmp.Or(u.updatedAt, "2026-01-01T00:00:00Z")
		_, _ = fmt.Fprintf(w, `{"total_count":1,"workflows":[{"id":7,"name":"Nightly","path":".github/workflows/nightly.yml","state":%q,
			"updated_at":%q,"html_url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml"}]}`, state, updated)
	case strings.HasSuffix(r.URL.Path, "/contents/.github/workflows"):
		_, _ = w.Write([]byte(`[{"path":".github/workflows/nightly.yml","sha":"aaa","type":"file"}]`))
	case strings.HasSuffix(r.URL.Path, "/nightly.yml"):
		_, _ = fmt.Fprintf(w, "on:\n  schedule:\n    - cron: '%s'\n", cmp.Or(u.cron, "0 * * * *"))
	case strings.HasSuffix(r.URL.Path, "/actions/workflows/7/runs"):
		u.queries = append(u.queries, r.URL.RawQuery)
		created := u.success
		if r.URL.Query().Get("status") == "completed" {
			created = u.completed
		}
		_, _ = fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"created_at":%q}]}`, created)
	default:
		http.NotFound(w, r)
	}
}

// 019-AC5, 019-AC6, 019-AC8: an hourly workflow last succeeding at 08:05 goes stale between 11:59 and 12:01 with
// alive_on success; a failed run at 11:10 keeps it alive with alive_on completed; a new success recovers it.
func TestGitHubSchedulesStaleness(t *testing.T) {
	up := &hourlyUpstream{success: "2026-11-04T08:05:00Z", completed: "2026-11-04T11:10:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	success, completed := schedulesCfg(srv.URL, "success", 3), schedulesCfg(srv.URL, "completed", 3)
	const nightly = `"path":".github/workflows/nightly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"`

	// 019-AC5: 09:00 and 10:00 are past the hour of grace at 11:59, 11:00 is not yet.
	quiet := schedulesRun(t, success, nil, at("2026-11-04T11:59:00Z"))
	check(t, schedulesDiff(t, json.RawMessage("null"), quiet), []string{})
	stale := schedulesRun(t, success, quiet, at("2026-11-04T12:01:00Z"))
	check(t, schedulesDiff(t, quiet, stale), []string{
		`schedule.stale .github/workflows/nightly.yml {"expected_at":"2026-11-04T09:00:00Z","last_alive_at":"2026-11-04T08:05:00Z","missed":3,` + nightly + `}`,
	})
	// 019-AC6: the failed run counts as proof of life with alive_on completed.
	alive := schedulesRun(t, completed, nil, at("2026-11-04T12:01:00Z"))
	check(t, schedulesDiff(t, json.RawMessage("null"), alive), []string{})
	if want := []string{"event=schedule&status=success&per_page=1", "event=schedule&status=success&per_page=1", "event=schedule&status=completed&per_page=1"}; strings.Join(up.queries, "|") != strings.Join(want, "|") {
		t.Fatalf("runs queries %q", up.queries)
	}

	// No run behind it, no schedule.recovered: stale clears quietly, and a new miss count fires schedule.stale again.
	for _, tt := range []struct {
		name string
		cfg  json.RawMessage
		edit func(u *hourlyUpstream)
	}{
		{"missed_runs raised", schedulesCfg(srv.URL, "success", 5), nil},
		// The failed 11:10 run is later than the 08:05 success, yet it was there before the switch.
		{"alive_on switched", completed, nil},
		// An edit, or a re-enable within one run interval, moves updated_at and so the reference.
		{"workflow edited", success, func(u *hourlyUpstream) { u.updatedAt = "2026-11-04T12:00:00Z" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.edit != nil {
				up.set(tt.edit)
				defer up.set(func(u *hourlyUpstream) { u.updatedAt = "" })
			}
			cur := schedulesRun(t, tt.cfg, stale, at("2026-11-04T12:30:00Z"))
			if !strings.Contains(string(cur), `"stale":false`) {
				t.Fatalf("still stale: %s", cur)
			}
			check(t, schedulesDiff(t, stale, cur), []string{})
		})
	}
	// A raised missed_runs still reached recounts instead of keeping a count below it, with no event.
	for _, tt := range []struct {
		missedRuns, missed int
		now                string
	}{{5, 5, "2026-11-04T14:01:00Z"}, {4, 4, "2026-11-04T13:01:00Z"}, {4, 6, "2026-11-04T15:01:00Z"}} {
		t.Run(fmt.Sprintf("missed_runs raised to %d, still stale at %s", tt.missedRuns, tt.now), func(t *testing.T) {
			cur := schedulesRun(t, schedulesCfg(srv.URL, "success", tt.missedRuns), stale, at(tt.now))
			var s SchedulesState
			if err := json.Unmarshal(cur, &s); err != nil || !s.Workflows[0].Stale || s.Workflows[0].Missed != tt.missed || !s.Workflows[0].ExpectedAt.Equal(at("2026-11-04T09:00:00Z")) {
				t.Fatalf("missed_runs %d: %s", tt.missedRuns, cur)
			}
			check(t, schedulesDiff(t, stale, cur), []string{})
		})
	}
	// A re-enable over two runs: disabled (not stale, not recovered), then enabled with the reference moved.
	up.set(func(u *hourlyUpstream) { u.state = "disabled_inactivity" })
	off := schedulesRun(t, success, stale, at("2026-11-04T12:30:00Z"))
	check(t, schedulesDiff(t, stale, off), []string{
		`schedule.disabled .github/workflows/nightly.yml {"last_alive_at":"2026-11-04T08:05:00Z","path":".github/workflows/nightly.yml","state":"disabled_inactivity","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"}`,
	})
	up.set(func(u *hourlyUpstream) { u.state, u.updatedAt = "", "2026-11-04T12:40:00Z" })
	on := schedulesRun(t, success, off, at("2026-11-04T13:00:00Z"))
	check(t, schedulesDiff(t, off, on), []string{
		`schedule.enabled .github/workflows/nightly.yml {"last_alive_at":"2026-11-04T08:05:00Z",` + nightly + `}`,
	})
	up.set(func(u *hourlyUpstream) { u.updatedAt = "" })

	// 019-AC8: the next run lands.
	up.set(func(u *hourlyUpstream) { u.success = "2026-11-04T13:00:20Z" })
	back := schedulesRun(t, success, stale, at("2026-11-04T13:30:00Z"))
	check(t, schedulesDiff(t, stale, back), []string{
		`schedule.recovered .github/workflows/nightly.yml {"last_alive_at":"2026-11-04T13:00:20Z",` + nightly + `}`,
	})
	t.Run("two-week gap", stalledCounts)
	t.Run("back then dead again", staleAgain)
	// A first run ever lands on a workflow stale with none (last_alive_at null before).
	var never SchedulesState
	if err := json.Unmarshal(stale, &never); err != nil {
		t.Fatal(err)
	}
	never.Workflows[0].LastAliveAt = nil
	b, _ := json.Marshal(never)
	check(t, schedulesDiff(t, b, back), []string{
		`schedule.recovered .github/workflows/nightly.yml {"last_alive_at":"2026-11-04T13:00:20Z",` + nightly + `}`,
	})
}

// stalledCounts, 019-AC5 past the threshold: the stale transition counts every missed time, up to maxMissed, and a
// later run still stale keeps that count, so the state does not change run to run.
func stalledCounts(t *testing.T) {
	const nightly = `"path":".github/workflows/nightly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"`
	up := &hourlyUpstream{success: "2026-10-21T08:05:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := schedulesCfg(srv.URL, "success", 3)
	// Two weeks without a success: 21 October 09:00 to 4 November 11:00, hourly.
	stale := schedulesRun(t, cfg, nil, at("2026-11-04T12:01:00Z"))
	check(t, schedulesDiff(t, json.RawMessage("null"), stale), []string{
		`schedule.stale .github/workflows/nightly.yml {"expected_at":"2026-10-21T09:00:00Z","last_alive_at":"2026-10-21T08:05:00Z","missed":339,` + nightly + `}`,
	})
	later := schedulesRun(t, cfg, stale, at("2026-11-04T15:01:00Z"))
	if string(later) != string(stale) {
		t.Fatalf("state changed while stale:\n%s\n%s", stale, later)
	}
	// A minutely cron months behind (updated_at 1 January is the later reference) stops at the cap.
	up.set(func(u *hourlyUpstream) { u.cron, u.success = "* * * * *", "2025-11-04T12:00:00Z" })
	capped := schedulesRun(t, cfg, nil, at("2026-11-04T12:01:00Z"))
	var s SchedulesState
	if err := json.Unmarshal(capped, &s); err != nil || s.Workflows[0].Missed != maxMissed || !s.Workflows[0].ExpectedAt.Equal(at("2026-01-01T00:01:00Z")) {
		t.Fatalf("capped %s", capped)
	}
}

// staleAgain: a stale job that comes back and dies again is stale anew, counted from its newer run; a newer run seen
// only through a switched alive_on is neither a recovery nor a new stale.
func staleAgain(t *testing.T) {
	const nightly = `"path":".github/workflows/nightly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"`
	up := &hourlyUpstream{success: "2026-11-04T08:05:00Z", completed: "2026-11-04T08:05:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	success, completed := schedulesCfg(srv.URL, "success", 3), schedulesCfg(srv.URL, "completed", 3)
	stale := schedulesRun(t, success, nil, at("2026-11-04T12:01:00Z"))
	check(t, schedulesDiff(t, json.RawMessage("null"), stale), []string{
		`schedule.stale .github/workflows/nightly.yml {"expected_at":"2026-11-04T09:00:00Z","last_alive_at":"2026-11-04T08:05:00Z","missed":3,` + nightly + `}`,
	})
	// 13:00:10 lands, then 14:00 to 17:00 are missed: no run saw the recovery in between.
	up.set(func(u *hourlyUpstream) { u.success, u.completed = "2026-11-04T13:00:10Z", "2026-11-04T13:00:10Z" })
	again := schedulesRun(t, success, stale, at("2026-11-04T18:01:00Z"))
	check(t, schedulesDiff(t, stale, again), []string{
		`schedule.stale .github/workflows/nightly.yml {"expected_at":"2026-11-04T14:00:00Z","last_alive_at":"2026-11-04T13:00:10Z","missed":4,` + nightly + `}`,
	})
	// The same newer run read through a switched alive_on: recounted from it, but nothing is sent.
	switched := schedulesRun(t, completed, stale, at("2026-11-04T18:01:00Z"))
	var s SchedulesState
	if err := json.Unmarshal(switched, &s); err != nil || !s.Workflows[0].Stale || s.Workflows[0].Missed != 4 || !s.Workflows[0].ExpectedAt.Equal(at("2026-11-04T14:00:00Z")) {
		t.Fatalf("switched %s", switched)
	}
	check(t, schedulesDiff(t, stale, switched), []string{})
	// An edit at 12:30 while stale moves the reference: the count restarts after it, with no event.
	up.set(func(u *hourlyUpstream) {
		u.success, u.completed, u.updatedAt = "2026-11-04T08:05:00Z", "2026-11-04T08:05:00Z", "2026-11-04T12:30:00Z"
	})
	defer up.set(func(u *hourlyUpstream) { u.updatedAt = "" })
	edited := schedulesRun(t, success, stale, at("2026-11-04T18:01:00Z"))
	s = SchedulesState{}
	if err := json.Unmarshal(edited, &s); err != nil || !s.Workflows[0].Stale || s.Workflows[0].Missed != 5 || !s.Workflows[0].ExpectedAt.Equal(at("2026-11-04T13:00:00Z")) {
		t.Fatalf("edited %s", edited)
	}
	check(t, schedulesDiff(t, stale, edited), []string{})

	// Edited at 09:30, stale from 10:00: a success older than the edit that shows up later is new proof of life, yet
	// the episode has not moved, so no second schedule.stale.
	for _, tt := range []struct{ staleAt, run, seenAt string }{
		{"2026-11-04T13:01:00Z", "2026-11-04T09:10:00Z", "2026-11-04T13:30:00Z"},
		{"2026-11-04T15:01:00Z", "2026-11-04T09:20:00Z", "2026-11-04T15:30:00Z"},
	} {
		t.Run("older run seen at "+tt.seenAt, func(t *testing.T) {
			up.set(func(u *hourlyUpstream) { u.success, u.updatedAt = "2026-11-04T08:05:00Z", "2026-11-04T09:30:00Z" })
			before := schedulesRun(t, success, nil, at(tt.staleAt))
			up.set(func(u *hourlyUpstream) { u.success = tt.run })
			after := schedulesRun(t, success, before, at(tt.seenAt))
			var s SchedulesState
			if err := json.Unmarshal(after, &s); err != nil || !s.Workflows[0].Stale || !s.Workflows[0].ExpectedAt.Equal(at("2026-11-04T10:00:00Z")) ||
				!s.Workflows[0].LastAliveAt.Equal(at(tt.run)) {
				t.Fatalf("older run %s", after)
			}
			check(t, schedulesDiff(t, before, after), []string{})
		})
	}
}

// 019-AC5, 019-AC7: expected times in UTC, in a timezone across both DST changes, and as a union of cron lines.
func TestMissedRuns(t *testing.T) {
	for _, tt := range []struct {
		name       string
		crons      []WorkflowCron
		ref, until string
		limit      int
		n          int
		first      string
	}{
		{"019-AC5 at 11:59", []WorkflowCron{{Cron: "0 * * * *"}}, "2026-11-04T08:05:00Z", "2026-11-04T10:59:00Z", 3, 2, "2026-11-04T09:00:00Z"},
		{"019-AC5 at 12:01", []WorkflowCron{{Cron: "0 * * * *"}}, "2026-11-04T08:05:00Z", "2026-11-04T11:01:00Z", 3, 3, "2026-11-04T09:00:00Z"},
		// 019-AC7: US DST starts on Sunday 8 March 2026: Friday is 10:30 UTC, Monday 09:30 UTC.
		{"019-AC7 spring EST", []WorkflowCron{{Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}, "2026-03-06T00:00:00Z", "2026-03-06T10:30:00Z", 3, 1, "2026-03-06T10:30:00Z"},
		{"019-AC7 spring EDT", []WorkflowCron{{Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}, "2026-03-06T10:30:00Z", "2026-03-09T09:30:00Z", 3, 1, "2026-03-09T09:30:00Z"},
		{"019-AC7 spring EDT not 10:30", []WorkflowCron{{Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}, "2026-03-06T10:30:00Z", "2026-03-09T09:29:00Z", 3, 0, ""},
		// It ends on Sunday 1 November 2026: Friday 09:30 UTC, Monday 10:30 UTC.
		{"019-AC7 autumn EDT", []WorkflowCron{{Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}, "2026-10-30T00:00:00Z", "2026-10-30T09:30:00Z", 3, 1, "2026-10-30T09:30:00Z"},
		{"019-AC7 autumn EST", []WorkflowCron{{Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}, "2026-10-30T09:30:00Z", "2026-11-02T10:29:00Z", 3, 0, ""},
		{"019-AC7 autumn EST 10:30", []WorkflowCron{{Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}, "2026-10-30T09:30:00Z", "2026-11-02T10:30:00Z", 3, 1, "2026-11-02T10:30:00Z"},
		// 019-AC7: two lines are one union; a time both fire at counts once.
		{"019-AC7 union", []WorkflowCron{{Cron: "0 6 * * *"}, {Cron: "0 18 * * *"}}, "2026-01-01T00:00:00Z", "2026-01-02T23:00:00Z", 10, 4, "2026-01-01T06:00:00Z"},
		{"union overlap", []WorkflowCron{{Cron: "0 */6 * * *"}, {Cron: "0 6 * * *"}, {Cron: "0 6 * * *"}}, "2026-01-01T00:00:00Z", "2026-01-01T23:59:00Z", 10, 3, "2026-01-01T06:00:00Z"},
		{"minutely with an old ref stops at the limit", []WorkflowCron{{Cron: "* * * * *"}}, "2016-01-01T00:00:00Z", "2026-01-01T00:00:00Z", 10, 10, "2016-01-01T00:01:00Z"},
		{"never fires", []WorkflowCron{{Cron: "0 0 30 2 *"}}, "2026-01-01T00:00:00Z", "2030-01-01T00:00:00Z", 3, 0, ""},
		{"no cron", nil, "2026-01-01T00:00:00Z", "2030-01-01T00:00:00Z", 3, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheds := parsedCrons(t, tt.crons)
			n, first := missedRuns(scheds, at(tt.ref), at(tt.until), tt.limit)
			if n != tt.n || (tt.first == "" && !first.IsZero()) || (tt.first != "" && !first.Equal(at(tt.first))) || (!first.IsZero() && first.Location() != time.UTC) {
				t.Fatalf("got %d %v, want %d %s", n, first, tt.n, tt.first)
			}
		})
	}
}

func parsedCrons(t *testing.T, crons []WorkflowCron) []cron.Schedule {
	t.Helper()
	out := make([]cron.Schedule, 0, len(crons))
	for _, c := range crons {
		s, err := cronSchedule(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// 019-AC2: only a map holding schedule[].cron is scheduled; a file, cron or timezone that does not parse leaves it out.
func TestWorkflowCrons(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		want       []WorkflowCron
	}{
		{"schedule", "on:\n  schedule:\n    - cron: '0 3 * * *'\n  push:\n", []WorkflowCron{{Cron: "0 3 * * *"}}},
		{"quoted on and two lines", "\"on\":\n  schedule:\n    - cron: '0 3 * * *'\n    - cron: '30 5 * * 1-5'\n      timezone: America/New_York\n",
			[]WorkflowCron{{Cron: "0 3 * * *"}, {Cron: "30 5 * * 1-5", Timezone: "America/New_York"}}},
		{"on string", "on: push\n", nil},
		{"on list", "on: [push, schedule]\n", nil},
		{"map without schedule", "on:\n  push:\n    branches: [main]\n", nil},
		{"empty schedule", "on:\n  schedule: []\n", nil},
		{"schedule not a list", "on:\n  schedule: '0 3 * * *'\n", nil},
		{"no on", "name: x\njobs: {}\n", nil},
		{"not yaml", "on: [\n", nil},
		{"bad cron", "on:\n  schedule:\n    - cron: '0 3 * *'\n", nil},
		{"no cron key", "on:\n  schedule:\n    - timezone: UTC\n", nil},
		{"seconds field", "on:\n  schedule:\n    - cron: '0 0 3 * * *'\n", nil},
		{"bad timezone", "on:\n  schedule:\n    - cron: '0 3 * * *'\n      timezone: Mars/Olympus\n", nil},
		{"Local timezone", "on:\n  schedule:\n    - cron: '0 3 * * *'\n      timezone: Local\n", nil},
		{"one bad line of two", "on:\n  schedule:\n    - cron: '0 3 * * *'\n    - cron: nope\n", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := workflowCrons([]byte(tt.yaml))
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

// 019-AC10: a failing workflows list, directory listing, file fetch or runs call fails the run with no state.
func TestGitHubSchedulesCallFails(t *testing.T) {
	up := testupstream.New()
	for _, path := range []string{
		"/repos/acme/widgets/actions/workflows",
		"/repos/acme/widgets/contents/.github/workflows",
		"/repos/acme/widgets/contents/.github/workflows/lint.yml",
		"/repos/acme/widgets/actions/workflows/1002/runs",
	} {
		t.Run(path, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				up.ServeHTTP(w, r)
			}))
			defer srv.Close()
			raw, err := Collectors()["github_schedules"].Collect(t.Context(), fetcher(t), schedulesCfg(srv.URL, "success", 3), nil)
			if err == nil || raw != nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "HTTP 500") {
				t.Fatalf("state %s, err %v", raw, err)
			}
		})
	}
}

// The workflows list is read page by page through Link; a repository with dynamic workflows only skips the
// directory listing, which may not exist.
func TestGitHubSchedulesListing(t *testing.T) {
	var contents int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/workflows") && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/actions/workflows?per_page=100&page=2>; rel="next"`, srv.URL))
			_, _ = w.Write([]byte(`{"workflows":[{"id":1,"path":"dynamic/github-code-scanning/codeql","state":"active"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_, _ = w.Write([]byte(`{"workflows":[{"id":2,"path":".github/workflows/gone.yml","state":"deleted"},{"id":3,"path":".github/workflows/../../x","state":"active"}]}`))
		default:
			contents++
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	raw := schedulesRun(t, schedulesCfg(srv.URL, "success", 3), nil, at("2026-11-04T12:00:00Z"))
	if string(raw) != `{"alive_on":"success","workflows":[],"unscheduled":{}}` || contents != 0 {
		t.Fatalf("state %s, contents calls %d", raw, contents)
	}

	// The directory gone from the default branch while the API still lists an active workflow: a 404 is an empty listing.
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/workflows") {
			_, _ = w.Write([]byte(`{"workflows":[{"id":4,"path":".github/workflows/nightly.yml","state":"active"}]}`))
			return
		}
		contents++
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer gone.Close()
	raw = schedulesRun(t, schedulesCfg(gone.URL, "success", 3), nil, at("2026-11-04T12:00:00Z"))
	if string(raw) != `{"alive_on":"success","workflows":[],"unscheduled":{}}` || contents != 1 {
		t.Fatalf("directory gone: state %s, contents calls %d", raw, contents)
	}

	// A Link that never ends stops at maxWorkflowPages.
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, len(r.URL.RawQuery)))
		_, _ = w.Write([]byte(`{"workflows":[]}`))
	}))
	defer loop.Close()
	if _, err := Collectors()["github_schedules"].Collect(t.Context(), fetcher(t), schedulesCfg(loop.URL, "success", 3), nil); err == nil ||
		!strings.Contains(err.Error(), "more than 10 pages") {
		t.Fatalf("err %v", err)
	}
}

// 001-AC11: a first run that the rate limit left cannot cover stops before fetching any file and waits for the reset,
// naming the count; when even a full budget is too small it fails instead. The sha cache then shrinks the next
// run's need. Without the header, no check (the other tests).
func TestGitHubSchedulesRateBudget(t *testing.T) {
	up := testupstream.New()
	var remaining, limit, reset string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/contents/.github/workflows") {
			w.Header().Set("X-RateLimit-Remaining", remaining)
			w.Header().Set("X-RateLimit-Limit", limit)
			w.Header().Set("X-RateLimit-Reset", reset)
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg := schedulesCfg(srv.URL, "success", 3)
	now := at("2026-11-04T12:00:00Z")
	files := func() int {
		n := 0
		for _, f := range []string{"ci", "lint", "mirror", "yearly"} {
			n += up.Hits("/repos/acme/widgets/contents/.github/workflows/" + f + ".yml")
		}
		return n
	}
	gh := &Credential{Kind: "github_token", Secret: "x"}
	// Fixture a: 4 files, plus the runs of yearly, ci and lint, active and not known unscheduled yet.
	for _, tt := range []struct {
		name, remaining, limit, reset string
		cred                          *Credential
		want                          string
		until                         time.Time // zero: a plain failure, no wait
	}{
		{"waits for the reset", "6", "60", "1793797200", nil,
			"rate limit: 6 left, this run needs 7 requests (a github_token credential raises the limit): rate limited by", time.Unix(1793797200, 0)},
		{"with a credential", "6", "5000", "1793797200", gh, "rate limit: 6 left, this run needs 7 requests: rate limited by", time.Unix(1793797200, 0)},
		{"no reset header", "6", "60", "", nil, "rate limit: 6 left, this run needs 7 requests", now.Add(defaultRateLimitWait)},
		{"no limit header", "6", "", "1793797200", nil, "rate limit: 6 left", time.Unix(1793797200, 0)},
		{"over the limit", "6", "6", "1793797200", nil, "rate limit: this run needs 7 requests, more than the limit of 6 (a github_token credential raises the limit)", time.Time{}},
		{"over the limit with a credential", "6", "6", "1793797200", gh, "rate limit: this run needs 7 requests, more than the limit of 6", time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remaining, limit, reset = tt.remaining, tt.limit, tt.reset
			raw, err := schedulesCollect(t, cfg, nil, now, tt.cred)
			var rl *RateLimitError
			waits := errors.As(err, &rl)
			if raw != nil || err == nil || files() != 0 || !strings.HasPrefix(err.Error(), tt.want) || waits == tt.until.IsZero() ||
				(waits && !rl.Until.Equal(tt.until)) || (tt.cred != nil && strings.Contains(err.Error(), "credential")) {
				t.Fatalf("state %s, err %v, files %d", raw, err, files())
			}
		})
	}
	remaining, limit, reset = "7", "60", "1793797200"
	a := schedulesRun(t, cfg, nil, now)
	// Known files: only yearly.yml's runs call is left.
	remaining = "1"
	schedulesRun(t, cfg, a, now.Add(time.Hour))
	remaining = "0"
	if _, err := schedulesCollect(t, cfg, a, now.Add(time.Hour), nil); err == nil || !strings.HasPrefix(err.Error(), "rate limit: 0 left, this run needs 1 request (") {
		t.Fatalf("known files, none left: %v", err)
	}
}

func warnCfg(apiURL string, warnDays int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":%d,"auto_enable":false}`, apiURL, warnDays))
}

// soonEvent is the schedule.disable_soon line for the hourly fake, last commit at last.
func soonEvent(last string) string {
	return `schedule.disable_soon acme/widgets {"disable_at":"` + at(last).Add(60*24*time.Hour).Format(time.RFC3339) + `","last_activity_at":"` + last +
		`","repo":"acme/widgets","workflows":[".github/workflows/nightly.yml"]}`
}

// 019-AC13: a public repository whose last commit is 54 days old, with warn_days 7: the first run sends one
// schedule.disable_soon with disable_at = commit + 60 d, the next run nothing, and a new commit clears the flag.
func TestGitHubSchedulesDisableSoon(t *testing.T) {
	now := at("2026-11-04T12:00:00Z")
	const commit = "2026-09-11T12:00:00Z" // 54 days before now
	up := &hourlyUpstream{success: "2026-11-04T11:05:00Z", commit: commit}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := warnCfg(srv.URL, 7)

	first := schedulesRun(t, cfg, nil, now)
	var s SchedulesState
	if err := json.Unmarshal(first, &s); err != nil || s.Repo != "acme/widgets" || s.Private == nil || *s.Private || !s.DisableSoon ||
		!s.LastActivityAt.Equal(at(commit)) || !s.DisableAt.Equal(at("2026-11-10T12:00:00Z")) {
		t.Fatalf("state %s", first)
	}
	// 019-AC13: this kind reports on its first run, diffed against no state.
	check(t, schedulesDiff(t, json.RawMessage("null"), first), []string{soonEvent(commit)})
	if !slices.Contains(up.paths, "/repos/acme/widgets") || !slices.Contains(up.paths, "/repos/acme/widgets/commits") {
		t.Fatalf("paths %q", up.paths)
	}

	// 019-AC13: still in the window, nothing new.
	next := schedulesRun(t, cfg, first, now.Add(time.Hour))
	check(t, schedulesDiff(t, first, next), []string{})

	// 019-AC13: a new commit moves disable_at and clears the flag, with no event.
	up.set(func(u *hourlyUpstream) { u.commit = "2026-11-04T12:30:00Z" })
	moved := schedulesRun(t, cfg, next, now.Add(2*time.Hour))
	s = SchedulesState{}
	if err := json.Unmarshal(moved, &s); err != nil || s.DisableSoon || !s.DisableAt.Equal(at("2027-01-03T12:30:00Z")) {
		t.Fatalf("moved %s", moved)
	}
	check(t, schedulesDiff(t, next, moved), []string{})
	// 019-AC13: 53 days on, the window opens again and warns again.
	up.set(func(u *hourlyUpstream) { u.success = "2026-12-27T12:05:00Z" })
	again := schedulesRun(t, cfg, moved, at("2026-12-27T12:30:00Z"))
	check(t, schedulesDiff(t, moved, again), []string{soonEvent("2026-11-04T12:30:00Z")})
}

// 019-AC13: the window opens exactly warn_days before disable_at, and only for a public repository with an active
// scheduled workflow and a commit.
func TestGitHubSchedulesDisableSoonCases(t *testing.T) {
	now := at("2026-11-04T12:00:00Z")
	for _, tt := range []struct {
		name     string
		warnDays int
		edit     func(u *hourlyUpstream)
		soon     bool
	}{
		{"window opens at disable_at - warn_days", 7, func(u *hourlyUpstream) { u.commit = "2026-09-12T12:00:00Z" }, true},
		{"a second before the window", 7, func(u *hourlyUpstream) { u.commit = "2026-09-12T12:00:01Z" }, false},
		{"past the cutoff, still active", 1, func(u *hourlyUpstream) { u.commit = "2026-01-01T00:00:00Z" }, true},
		{"warn_days 30", 30, func(u *hourlyUpstream) { u.commit = "2026-10-05T12:00:00Z" }, true},
		{"private, 54 days", 7, func(u *hourlyUpstream) { u.private, u.commit = true, "2026-09-11T12:00:00Z" }, false},
		{"private, past the cutoff", 30, func(u *hourlyUpstream) { u.private, u.commit = true, "2025-01-01T00:00:00Z" }, false},
		{"disabled for inactivity", 7, func(u *hourlyUpstream) { u.state, u.commit = "disabled_inactivity", "2026-09-11T12:00:00Z" }, false},
		{"disabled by hand", 30, func(u *hourlyUpstream) { u.state, u.commit = "disabled_manually", "2026-01-01T00:00:00Z" }, false},
		{"no commit", 30, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			up := &hourlyUpstream{success: "2026-11-04T11:05:00Z"}
			if tt.edit != nil {
				up.set(tt.edit)
			}
			srv := httptest.NewServer(up)
			defer srv.Close()
			raw := schedulesRun(t, warnCfg(srv.URL, tt.warnDays), nil, now)
			var s SchedulesState
			if err := json.Unmarshal(raw, &s); err != nil || s.DisableSoon != tt.soon || (up.private && (s.DisableAt != nil || !*s.Private)) {
				t.Fatalf("state %s", raw)
			}
			var want []string
			for _, e := range schedulesDiff(t, json.RawMessage("null"), raw) {
				if strings.HasPrefix(e, "schedule.disable_soon") {
					want = append(want, e)
				}
			}
			if (len(want) == 1) != tt.soon {
				t.Fatalf("events %q", want)
			}
		})
	}
}

// 019-AC13: with warn_days 0, neither /repos/{repo} nor /commits is called and the state has no repository fields.
func TestGitHubSchedulesNoWarnNoCalls(t *testing.T) {
	up := &hourlyUpstream{success: "2026-11-04T11:05:00Z", commit: "2026-09-11T12:00:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	raw := schedulesRun(t, warnCfg(srv.URL, 0), nil, at("2026-11-04T12:00:00Z"))
	want := []string{"/repos/acme/widgets/actions/workflows", "/repos/acme/widgets/contents/.github/workflows",
		"/repos/acme/widgets/contents/.github/workflows/nightly.yml", "/repos/acme/widgets/actions/workflows/7/runs"}
	if !slices.Equal(up.paths, want) || strings.Contains(string(raw), "disable") || strings.Contains(string(raw), "private") {
		t.Fatalf("paths %q, state %s", up.paths, raw)
	}
	check(t, schedulesDiff(t, json.RawMessage("null"), raw), []string{})
}

// 019-AC13: a state stored before the warning existed (no repository fields) diffs quietly against a run outside the
// window, and a run inside it sends its one schedule.disable_soon; turning warn_days off then on warns again.
func TestGitHubSchedulesDisableSoonFromOldState(t *testing.T) {
	up := &hourlyUpstream{success: "2026-11-04T11:05:00Z", commit: "2026-11-01T00:00:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	now := at("2026-11-04T12:00:00Z")
	old := schedulesRun(t, schedulesCfg(srv.URL, "success", 3), nil, now)
	if strings.Contains(string(old), "disable") {
		t.Fatalf("old %s", old)
	}
	quiet := schedulesRun(t, warnCfg(srv.URL, 7), old, now)
	check(t, schedulesDiff(t, old, quiet), []string{})
	up.set(func(u *hourlyUpstream) { u.commit = "2026-09-11T12:00:00Z" })
	soon := schedulesRun(t, warnCfg(srv.URL, 7), old, now)
	check(t, schedulesDiff(t, old, soon), []string{soonEvent("2026-09-11T12:00:00Z")})
	off := schedulesRun(t, warnCfg(srv.URL, 0), soon, now)
	check(t, schedulesDiff(t, soon, off), []string{})
	on := schedulesRun(t, warnCfg(srv.URL, 7), off, now)
	check(t, schedulesDiff(t, off, on), []string{soonEvent("2026-09-11T12:00:00Z")})
	// Still flagged while the workflow goes stale: only the workflow's event.
	up.set(func(u *hourlyUpstream) { u.success = "2026-11-04T07:05:00Z" })
	stale := schedulesRun(t, warnCfg(srv.URL, 7), on, now)
	if got := schedulesDiff(t, on, stale); len(got) != 1 || !strings.HasPrefix(got[0], "schedule.stale ") {
		t.Fatalf("events %q", got)
	}
}

// 019-AC10: with warn_days, a failing repository or commits call fails the run with no state.
func TestGitHubSchedulesWarnCallFails(t *testing.T) {
	for _, path := range []string{"/repos/acme/widgets", "/repos/acme/widgets/commits"} {
		t.Run(path, func(t *testing.T) {
			up := &hourlyUpstream{success: "2026-11-04T11:05:00Z", commit: "2026-09-11T12:00:00Z"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				up.ServeHTTP(w, r)
			}))
			defer srv.Close()
			raw, err := schedulesCollect(t, warnCfg(srv.URL, 7), nil, at("2026-11-04T12:00:00Z"), nil)
			if err == nil || raw != nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "HTTP 500") {
				t.Fatalf("state %s, err %v", raw, err)
			}
		})
	}
}

// 001-AC11: warn_days adds its 2 calls to the rate budget.
func TestGitHubSchedulesWarnBudget(t *testing.T) {
	up := &hourlyUpstream{success: "2026-11-04T11:05:00Z", commit: "2026-09-11T12:00:00Z"}
	var remaining string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/contents/.github/workflows") {
			w.Header().Set("X-RateLimit-Remaining", remaining)
			w.Header().Set("X-RateLimit-Reset", "1793797200")
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	now := at("2026-11-04T12:00:00Z")
	// First run: nightly.yml and its runs, plus the repository and its last commit.
	remaining = "3"
	if _, err := schedulesCollect(t, warnCfg(srv.URL, 7), nil, now, nil); err == nil || !strings.HasPrefix(err.Error(), "rate limit: 3 left, this run needs 4 requests") {
		t.Fatalf("first run: %v", err)
	}
	a := schedulesRun(t, warnCfg(srv.URL, 0), nil, now)
	remaining = "2"
	if _, err := schedulesCollect(t, warnCfg(srv.URL, 7), a, now, nil); err == nil || !strings.HasPrefix(err.Error(), "rate limit: 2 left, this run needs 3 requests") {
		t.Fatalf("known file: %v", err)
	}
	remaining = "3"
	schedulesRun(t, warnCfg(srv.URL, 7), a, now)
	remaining = "1"
	schedulesRun(t, warnCfg(srv.URL, 0), a, now)
}

// GitHub answers 409 Conflict on the commits of an empty repository: no commit, no warning, the run succeeds. Only that
// call and that status: a 409 on the repository or a 404 on the commits still fail the run.
func TestGitHubSchedulesEmptyRepository(t *testing.T) {
	for _, tt := range []struct {
		path   string
		status int
		ok     bool
	}{
		{"/repos/acme/widgets/commits", http.StatusConflict, true},
		{"/repos/acme/widgets", http.StatusConflict, false},
		{"/repos/acme/widgets/commits", http.StatusNotFound, false},
	} {
		t.Run(fmt.Sprintf("%s %d", tt.path, tt.status), func(t *testing.T) {
			up := &hourlyUpstream{success: "2026-11-04T11:05:00Z", commit: "2026-09-11T12:00:00Z"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tt.path {
					http.Error(w, `{"message":"Git Repository is empty."}`, tt.status)
					return
				}
				up.ServeHTTP(w, r)
			}))
			defer srv.Close()
			raw, err := schedulesCollect(t, warnCfg(srv.URL, 7), nil, at("2026-11-04T12:00:00Z"), nil)
			if !tt.ok {
				if err == nil || raw != nil {
					t.Fatalf("state %s, err %v", raw, err)
				}
				return
			}
			var s SchedulesState
			if err != nil || json.Unmarshal(raw, &s) != nil || s.Private == nil || s.LastActivityAt != nil || s.DisableAt != nil || s.DisableSoon {
				t.Fatalf("state %s, err %v", raw, err)
			}
		})
	}
}

func autoCfg(apiURL string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":0,"auto_enable":true}`, apiURL))
}

// autoCollect collects once with the github_token credential, and returns the run's re-enable outcomes.
func autoCollect(t *testing.T, cfg, prev json.RawMessage, now time.Time) (json.RawMessage, []Reenable, error) {
	t.Helper()
	f := fetcher(t)
	f.Now = func() time.Time { return now }
	f.Prev = prev
	raw, err := Collectors()["github_schedules"].Collect(t.Context(), f, cfg, &Credential{Kind: "github_token", Secret: "ghp_x"})
	return raw, f.Reenables, err
}

func schedulesState(t *testing.T, raw json.RawMessage) map[string]ScheduledWorkflow {
	t.Helper()
	var s SchedulesState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	out := map[string]ScheduledWorkflow{}
	for _, w := range s.Workflows {
		out[w.Path] = w
	}
	return out
}

// 019-AC14: with auto_enable, fixture a's disabled_inactivity mirror.yml gets one PUT with the credential and one
// schedule.reenabled, no schedule.disabled; yearly.yml, disabled by hand, gets no PUT and a schedule.disabled. The
// next run lists it active: no PUT, no schedule.enabled. Disabled again later, it is re-enabled again.
func TestGitHubSchedulesAutoEnable(t *testing.T) {
	now := at("2026-11-04T12:00:00Z")
	up := testupstream.New()
	up.SetNow(func() time.Time { return now })
	up.SetWorkflowState("1002", "disabled_manually")
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := autoCfg(srv.URL)
	put := func(id string) int { return up.Hits("/repos/acme/widgets/actions/workflows/" + id + "/enable") }

	first, re, err := autoCollect(t, cfg, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	got := up.Received()
	if len(got) != 1 || got[0].Endpoint != "enable-1001" || got[0].Header["Authorization"] != "Bearer ghp_x" || put("1002") != 0 {
		t.Fatalf("PUTs %+v, yearly %d", got, put("1002"))
	}
	if want := []Reenable{{Repo: "acme/widgets", Path: mirrorPath, Status: http.StatusNoContent}}; !slices.Equal(re, want) {
		t.Fatalf("outcomes %+v", re)
	}
	ws := schedulesState(t, first)
	m := ws[mirrorPath]
	// Re-enabled now, it cannot be stale: no runs call this run, last_alive_at stays the stored one.
	runs := func() int { return up.Hits("/repos/acme/widgets/actions/workflows/1001/runs") }
	if m.State != "active" || m.ReenabledAt == nil || !m.ReenabledAt.Equal(now) || m.Stale || m.LastAliveAt != nil || runs() != 0 ||
		m.ReenableError != "" || ws[".github/workflows/yearly.yml"].State != "disabled_manually" {
		t.Fatalf("state %s, runs calls %d", first, runs())
	}
	check(t, schedulesDiff(t, json.RawMessage("null"), first), []string{
		`schedule.reenabled .github/workflows/mirror.yml {"from":"disabled_inactivity","last_alive_at":null,"path":".github/workflows/mirror.yml","state":"active","url":"` + mirrorURL + `","workflow":"Mirror"}`,
		`schedule.disabled .github/workflows/yearly.yml {"last_alive_at":null,"path":".github/workflows/yearly.yml","state":"disabled_manually","url":"https://github.com/acme/widgets/blob/main/.github/workflows/yearly.yml","workflow":"Yearly report"}`,
	})

	next, re, err := autoCollect(t, cfg, first, now.Add(time.Hour))
	if err != nil || len(re) != 0 || put("1001") != 1 || runs() != 1 {
		t.Fatalf("next run: err %v, outcomes %+v, PUTs %d, runs calls %d", err, re, put("1001"), runs())
	}
	check(t, schedulesDiff(t, first, next), []string{})

	up.SetWorkflowState("1001", "disabled_inactivity")
	again, _, err := autoCollect(t, cfg, next, now.Add(2*time.Hour))
	if err != nil || put("1001") != 2 {
		t.Fatalf("again: err %v, PUTs %d", err, put("1001"))
	}
	if got := schedulesDiff(t, next, again); len(got) != 1 || !strings.HasPrefix(got[0], "schedule.reenabled "+mirrorPath) {
		t.Fatalf("events %q", got)
	}

	// Off, nothing is written upstream.
	up.SetWorkflowState("1001", "disabled_inactivity")
	if _, re, err := autoCollect(t, schedulesCfg(srv.URL, "success", 3), next, now.Add(3*time.Hour)); err != nil || len(re) != 0 || put("1001") != 2 {
		t.Fatalf("off: err %v, outcomes %+v, PUTs %d", err, re, put("1001"))
	}
}

// 019-AC14: a re-enabled workflow is not stale for the runs it missed before: GitHub's listed updated_at may stay old,
// so the re-enable time is the reference, and the count starts after it.
func TestGitHubSchedulesReenabledStaleness(t *testing.T) {
	// Last success four days ago, updated_at in January: without the re-enable time it would be stale at once.
	up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-10-31T08:05:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := autoCfg(srv.URL)
	run := func(prev json.RawMessage, now string) json.RawMessage {
		t.Helper()
		raw, _, err := autoCollect(t, cfg, prev, at(now))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	on := run(nil, "2026-11-04T12:00:00Z")
	if up.putCount() != 1 {
		t.Fatalf("PUTs %d", up.putCount())
	}
	// 13:00 is missed at 14:30, 14:00 not yet: one of three.
	quiet := run(on, "2026-11-04T14:30:00Z")
	check(t, schedulesDiff(t, on, quiet), []string{})
	stale := run(quiet, "2026-11-04T16:01:00Z")
	check(t, schedulesDiff(t, quiet, stale), []string{
		`schedule.stale .github/workflows/nightly.yml {"expected_at":"2026-11-04T13:00:00Z","last_alive_at":"2026-10-31T08:05:00Z","missed":3,"path":".github/workflows/nightly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"}`,
	})
	if up.putCount() != 1 || !schedulesState(t, stale)[".github/workflows/nightly.yml"].ReenabledAt.Equal(at("2026-11-04T12:00:00Z")) {
		t.Fatalf("PUTs %d, state %s", up.putCount(), stale)
	}
}

// 019-AC15: a refused or failed PUT keeps the workflow disabled with a short reason, the run succeeds, and the next
// run tries again without a second schedule.disabled; a later PUT that works sends schedule.reenabled.
func TestGitHubSchedulesReenableFails(t *testing.T) {
	const reason = "Resource not accessible by personal access token"
	for _, tt := range []struct {
		name   string
		status int
		header http.Header
		want   string // reenable_error prefix
		code   int    // the status logged
	}{
		{"403", http.StatusForbidden, nil, "HTTP 403: " + reason, http.StatusForbidden},
		{"404", http.StatusNotFound, nil, "HTTP 404: " + reason, http.StatusNotFound},
		{"500", http.StatusInternalServerError, nil, "HTTP 500: " + reason, http.StatusInternalServerError},
		{"503 without Retry-After", http.StatusServiceUnavailable, nil, "HTTP 503: " + reason, http.StatusServiceUnavailable},
		{"connection dropped", -1, nil, "PUT http://127.0.0.1:", 0},
		// Only 204 enables: a 2xx from elsewhere, or a redirect the client replays as a GET, did not.
		{"200", http.StatusOK, nil, "HTTP 200, expected 204", http.StatusOK},
		{"202", http.StatusAccepted, nil, "HTTP 202, expected 204", http.StatusAccepted},
		{"302 replayed as GET", http.StatusFound, http.Header{"Location": {"/repos/acme/widgets"}}, "HTTP 200, expected 204", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-11-04T11:05:00Z", putStatus: tt.status, putHeader: tt.header}
			srv := httptest.NewServer(up)
			defer srv.Close()
			cfg := autoCfg(srv.URL)
			first, re, err := autoCollect(t, cfg, nil, at("2026-11-04T12:00:00Z"))
			if err != nil {
				t.Fatalf("the run failed: %v", err)
			}
			w := schedulesState(t, first)[".github/workflows/nightly.yml"]
			if w.State != "disabled_inactivity" || w.ReenabledAt != nil || !strings.HasPrefix(w.ReenableError, tt.want) || len([]rune(w.ReenableError)) > 220 ||
				strings.Contains(w.ReenableError, "ghp_x") || strings.Contains(w.ReenableError, "\n") {
				t.Fatalf("state %s", first)
			}
			if len(re) != 1 || re[0].Err != w.ReenableError || re[0].Status != tt.code || re[0].Path != ".github/workflows/nightly.yml" {
				t.Fatalf("outcomes %+v", re)
			}
			b, _ := json.Marshal(w.ReenableError)
			check(t, schedulesDiff(t, json.RawMessage("null"), first), []string{
				`schedule.disabled .github/workflows/nightly.yml {"last_alive_at":null,"path":".github/workflows/nightly.yml","reenable_error":` + string(b) +
					`,"state":"disabled_inactivity","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"}`,
			})
			// The next run tries again, still failing: nothing new.
			next, _, err := autoCollect(t, cfg, first, at("2026-11-04T13:00:00Z"))
			if err != nil || up.putCount() != 2 {
				t.Fatalf("next run: err %v, PUTs %d", err, up.putCount())
			}
			check(t, schedulesDiff(t, first, next), []string{})
			// It works at last: re-enabled, not enabled.
			up.set(func(u *hourlyUpstream) { u.putStatus = 0 })
			back, _, err := autoCollect(t, cfg, next, at("2026-11-04T14:00:00Z"))
			if err != nil || up.putCount() != 3 {
				t.Fatalf("back: err %v, PUTs %d", err, up.putCount())
			}
			if got := schedulesDiff(t, next, back); len(got) != 1 || !strings.HasPrefix(got[0], `schedule.reenabled .github/workflows/nightly.yml {"from":"disabled_inactivity"`) {
				t.Fatalf("events %q", got)
			}
		})
	}
}

// 019-AC15, 001-AC11: a rate-limited PUT is no refusal: the run waits for the reset like on any other call, with no
// state, and the next run tries again.
func TestGitHubSchedulesReenableRateLimited(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		header http.Header
		until  time.Time
	}{
		{"403 limit spent", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1793797200"}}, time.Unix(1793797200, 0)},
		{"secondary limit", http.StatusForbidden, http.Header{"Retry-After": {"60"}}, at("2026-11-04T12:01:00Z")},
		{"429", http.StatusTooManyRequests, nil, at("2026-11-04T12:00:00Z").Add(defaultRateLimitWait)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-11-04T11:05:00Z", putStatus: tt.status, putHeader: tt.header}
			srv := httptest.NewServer(up)
			defer srv.Close()
			raw, re, err := autoCollect(t, autoCfg(srv.URL), nil, at("2026-11-04T12:00:00Z"))
			var rl *RateLimitError
			if raw != nil || !errors.As(err, &rl) || !rl.Until.Equal(tt.until) || len(re) != 1 || re[0].Err == "" {
				t.Fatalf("state %s, err %v, outcomes %+v", raw, err, re)
			}
			up.set(func(u *hourlyUpstream) { u.putStatus, u.putHeader = 0, nil })
			if _, _, err := autoCollect(t, autoCfg(srv.URL), nil, at("2026-11-04T13:00:00Z")); err != nil || up.putCount() != 2 {
				t.Fatalf("next run: err %v, PUTs %d", err, up.putCount())
			}
		})
	}
}

// 001-AC11: auto_enable adds the PUT of each disabled_inactivity workflow to the rate budget, and no runs call, as a
// workflow re-enabled this run cannot be stale; a disabled_manually one, or auto_enable off, adds nothing.
func TestGitHubSchedulesReenableBudget(t *testing.T) {
	up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-11-04T11:05:00Z", putStatus: http.StatusForbidden}
	var remaining string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/contents/.github/workflows") {
			w.Header().Set("X-RateLimit-Remaining", remaining)
			w.Header().Set("X-RateLimit-Reset", "1793797200")
		}
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	now := at("2026-11-04T12:00:00Z")
	// Known file: only the PUT is left.
	remaining = "9"
	known, _, err := autoCollect(t, autoCfg(srv.URL), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	remaining = "0"
	if _, _, err := autoCollect(t, autoCfg(srv.URL), known, now); err == nil || !strings.HasPrefix(err.Error(), "rate limit: 0 left, this run needs 1 request:") || up.putCount() != 1 {
		t.Fatalf("known file: %v, PUTs %d", err, up.putCount())
	}
	// First run: the file too.
	remaining = "1"
	if _, _, err := autoCollect(t, autoCfg(srv.URL), nil, now); err == nil || !strings.HasPrefix(err.Error(), "rate limit: 1 left, this run needs 2 requests") {
		t.Fatalf("first run: %v", err)
	}
	if _, _, err := autoCollect(t, autoCfg(srv.URL), known, now); err != nil || up.putCount() != 2 {
		t.Fatalf("enough: %v, PUTs %d", err, up.putCount())
	}
	remaining = "0"
	schedulesRun(t, schedulesCfg(srv.URL, "success", 3), known, now)
	up.set(func(u *hourlyUpstream) { u.state = "disabled_manually" })
	if _, _, err := autoCollect(t, autoCfg(srv.URL), known, now); err != nil || up.putCount() != 2 {
		t.Fatalf("disabled by hand: %v, PUTs %d", err, up.putCount())
	}
}

// 019-AC14: the PUTs go out after every read. A read failing anywhere in the run fails it before any PUT, so no
// re-enable is lost with the run; the next run finds the workflow still disabled and sends it.
func TestGitHubSchedulesReenableAfterReads(t *testing.T) {
	for _, tt := range []struct {
		name, path string
		warnDays   int
	}{
		{"file fetch", "/repos/acme/widgets/contents/.github/workflows/yearly.yml", 0},
		{"runs call", "/repos/acme/widgets/actions/workflows/1002/runs", 0},
		{"last commit", "/repos/acme/widgets/commits", 7},
		{"repository", "/repos/acme/widgets", 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := at("2026-11-04T12:00:00Z")
			up := testupstream.New()
			up.SetNow(func() time.Time { return now })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == tt.path {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				up.ServeHTTP(w, r)
			}))
			defer srv.Close()
			cfg := json.RawMessage(fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":%d,"auto_enable":true}`, srv.URL, tt.warnDays))
			raw, re, err := autoCollect(t, cfg, nil, now)
			if err == nil || raw != nil || len(re) != 0 || len(up.Received()) != 0 {
				t.Fatalf("state %s, err %v, outcomes %+v, PUTs %+v", raw, err, re, up.Received())
			}
		})
	}
}

// 019-AC13, 019-AC14: a workflow re-enabled this run is active for the 60-day warning, computed after the PUTs.
func TestGitHubSchedulesReenableThenWarn(t *testing.T) {
	up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-11-04T11:05:00Z", commit: "2026-09-11T12:00:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := json.RawMessage(fmt.Sprintf(`{"repo":"acme/widgets","api_url":%q,"alive_on":"success","missed_runs":3,"warn_days":7,"auto_enable":true}`, srv.URL))
	raw, _, err := autoCollect(t, cfg, nil, at("2026-11-04T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	got := schedulesDiff(t, json.RawMessage("null"), raw)
	if len(got) != 2 || !strings.HasPrefix(got[0], "schedule.reenabled ") || got[1] != soonEvent("2026-09-11T12:00:00Z") {
		t.Fatalf("events %q", got)
	}
}

// eventFor returns the one event line about path, "" when none.
func eventFor(t *testing.T, events []string, path string) string {
	t.Helper()
	var out []string
	for _, e := range events {
		if strings.Contains(e, " "+path+" ") {
			out = append(out, e)
		}
	}
	if len(out) > 1 {
		t.Fatalf("several events for %s: %q", path, out)
	}
	return strings.Join(out, "")
}

// reenableTrio lists three disabled_inactivity workflows, a, b and c (ids 1 to 3); each PUT answers status[id], 204 by default.
type reenableTrio struct {
	mu     sync.Mutex
	status map[string]int
	puts   []string
	// onPut runs before a PUT is answered.
	onPut func(id string, r *http.Request)
}

// putList reads under the lock: a PUT the client gave up on may still be in its handler.
func (u *reenableTrio) putList() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return strings.Join(u.puts, ",")
}

func (u *reenableTrio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case r.Method == http.MethodPut:
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/actions/workflows/"), "/")[0]
		u.puts = append(u.puts, id)
		if u.onPut != nil {
			u.onPut(id, r)
		}
		if st := u.status[id]; st != 0 {
			http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, st)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
		var ws []string
		for i, n := range []string{"a", "b", "c"} {
			ws = append(ws, fmt.Sprintf(`{"id":%d,"name":%q,"path":".github/workflows/%s.yml","state":"disabled_inactivity","updated_at":"2026-01-01T00:00:00Z"}`, i+1, n, n))
		}
		_, _ = fmt.Fprintf(w, `{"workflows":[%s]}`, strings.Join(ws, ","))
	case strings.HasSuffix(r.URL.Path, "/contents/.github/workflows"):
		_, _ = w.Write([]byte(`[{"path":".github/workflows/a.yml","sha":"a","type":"file"},{"path":".github/workflows/b.yml","sha":"b","type":"file"},
			{"path":".github/workflows/c.yml","sha":"c","type":"file"}]`))
	case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/contents/"):
		_, _ = w.Write([]byte("on:\n  schedule:\n    - cron: '0 0 * * *'\n"))
	default:
		http.NotFound(w, r)
	}
}

// 019-AC15: a PUT rate-limited after another worked keeps that success: the run stores it and sends no more PUTs. The
// one limited and one left without a PUT keep their stored reason (the log has the limit), so neither opens a failure streak
// (no reenable_error event, a real error next run sends one) nor ends one. Before any success, the run waits for the
// reset (TestGitHubSchedulesReenableRateLimited).
func TestGitHubSchedulesReenableLimitAfterSuccess(t *testing.T) {
	up := &reenableTrio{status: map[string]int{"2": http.StatusTooManyRequests}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	raw, re, err := autoCollect(t, autoCfg(srv.URL), nil, at("2026-11-04T12:00:00Z"))
	if err != nil || up.putList() != "1,2" || len(re) != 2 || re[0].Err != "" || re[1].Err == "" {
		t.Fatalf("err %v, PUTs %q, outcomes %+v", err, up.putList(), re)
	}
	ws := schedulesState(t, raw)
	a, b, c := ws[".github/workflows/a.yml"], ws[".github/workflows/b.yml"], ws[".github/workflows/c.yml"]
	if a.State != "active" || b.State != "disabled_inactivity" || c.State != "disabled_inactivity" ||
		b.ReenableError != "" || !strings.HasPrefix(re[1].Err, "rate limited by ") || c.ReenableError != "" {
		t.Fatalf("state %s", raw)
	}
	got := schedulesDiff(t, json.RawMessage("null"), raw)
	if len(got) != 3 || !strings.HasPrefix(got[0], "schedule.reenabled .github/workflows/a.yml") ||
		strings.Contains(got[1], "reenable_error") || strings.Contains(got[2], "reenable_error") {
		t.Fatalf("events %q", got)
	}
	// The next run sends the rest: c fails for real, which opens its streak.
	up.mu.Lock()
	up.status = map[string]int{"3": http.StatusForbidden}
	up.mu.Unlock()
	next, _, err := autoCollect(t, autoCfg(srv.URL), raw, at("2026-11-04T13:00:00Z"))
	if err != nil || up.putList() != "1,2,1,2,3" {
		t.Fatalf("next run: err %v, PUTs %q", err, up.putList())
	}
	if e := eventFor(t, schedulesDiff(t, raw, next), ".github/workflows/c.yml"); !strings.HasPrefix(e, "schedule.disabled ") ||
		!strings.Contains(e, `"reenable_error":"HTTP 403: `) {
		t.Fatalf("c next run: %q", e)
	}
	// Stopped again before c: it keeps its stored reason, so its streak goes on with no event and no end.
	up.mu.Lock()
	up.status = map[string]int{"2": http.StatusTooManyRequests}
	up.mu.Unlock()
	third, _, err := autoCollect(t, autoCfg(srv.URL), next, at("2026-11-04T14:00:00Z"))
	if err != nil || up.putList() != "1,2,1,2,3,1,2" {
		t.Fatalf("third run: err %v, PUTs %q", err, up.putList())
	}
	if c3 := schedulesState(t, third)[".github/workflows/c.yml"]; c3.ReenableError != schedulesState(t, next)[".github/workflows/c.yml"].ReenableError {
		t.Fatalf("c third run: %+v", c3)
	}
	if e := eventFor(t, schedulesDiff(t, next, third), ".github/workflows/c.yml"); e != "" {
		t.Fatalf("c third run: %q", e)
	}
}

// 019-AC14: a run cancelled or timed out during a PUT after another one worked keeps that success: the run returns its
// state, the rest left disabled with their stored reason for the next run, so the re-enable is not lost (the worker
// stores it on a timeout or a shutdown).
func TestGitHubSchedulesReenableCancelAfterSuccess(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout time.Duration // 0: cancelled from the second PUT instead
		want    string
	}{
		{"cancelled", 0, "context canceled"},
		// Long enough for the reads, then the second PUT hangs past it.
		{"timed out", 2 * time.Second, "context deadline exceeded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if tt.timeout > 0 {
				ctx, cancel = context.WithTimeout(t.Context(), tt.timeout)
			}
			defer cancel()
			up := &reenableTrio{}
			up.onPut = func(id string, r *http.Request) {
				if id != "2" {
					return
				}
				if tt.timeout == 0 {
					cancel()
				}
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
			}
			srv := httptest.NewServer(up)
			defer srv.Close()
			f := fetcher(t)
			f.Now = func() time.Time { return at("2026-11-04T12:00:00Z") }
			raw, err := Collectors()["github_schedules"].Collect(ctx, f, autoCfg(srv.URL), &Credential{Kind: "github_token", Secret: "ghp_x"})
			if err != nil || up.putList() != "1,2" || len(f.Reenables) != 2 || f.Reenables[0].Err != "" || !strings.Contains(f.Reenables[1].Err, tt.want) {
				t.Fatalf("err %v, PUTs %q, outcomes %+v", err, up.putList(), f.Reenables)
			}
			ws := schedulesState(t, raw)
			a, b, c := ws[".github/workflows/a.yml"], ws[".github/workflows/b.yml"], ws[".github/workflows/c.yml"]
			if a.State != "active" || a.ReenabledAt == nil || b.State != "disabled_inactivity" || b.ReenableError != "" ||
				c.ReenableError != "" {
				t.Fatalf("state %s", raw)
			}
			// c got no PUT: no reenable_error event; a real error on it next run sends one.
			if e := eventFor(t, schedulesDiff(t, json.RawMessage("null"), raw), ".github/workflows/c.yml"); !strings.HasPrefix(e, "schedule.disabled ") ||
				strings.Contains(e, "reenable_error") {
				t.Fatalf("c: %q", e)
			}
			up.mu.Lock()
			up.onPut, up.status = nil, map[string]int{"3": http.StatusForbidden}
			up.mu.Unlock()
			next, _, err := autoCollect(t, autoCfg(srv.URL), raw, at("2026-11-04T13:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if e := eventFor(t, schedulesDiff(t, raw, next), ".github/workflows/c.yml"); !strings.Contains(e, `"reenable_error":"HTTP 403: `) {
				t.Fatalf("c next run: %q", e)
			}
		})
	}
}

// 019-AC15: a workflow already stored disabled whose re-enable fails (auto_enable turned on after schedule.disabled, a
// token without Actions write) sends one schedule.disabled with reenable_error per failure streak: none while it keeps
// failing, whatever the reason, and schedule.reenabled once it works; turned off then on, a new streak sends again.
func TestGitHubSchedulesReenableFailsWhileDisabled(t *testing.T) {
	up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-11-04T11:05:00Z", putStatus: http.StatusForbidden}
	srv := httptest.NewServer(up)
	defer srv.Close()
	const disabled = `schedule.disabled .github/workflows/nightly.yml {"last_alive_at":null,"path":".github/workflows/nightly.yml",`
	off := schedulesRun(t, schedulesCfg(srv.URL, "success", 3), nil, at("2026-11-04T12:00:00Z"))
	run := func(prev json.RawMessage, now string) json.RawMessage {
		t.Helper()
		raw, _, err := autoCollect(t, autoCfg(srv.URL), prev, at(now))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	failing := run(off, "2026-11-04T13:00:00Z")
	check(t, schedulesDiff(t, off, failing), []string{
		disabled + `"reenable_error":"HTTP 403: Resource not accessible by personal access token","state":"disabled_inactivity","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"}`,
	})
	still := run(failing, "2026-11-04T14:00:00Z")
	check(t, schedulesDiff(t, failing, still), []string{})
	// Another reason within the streak: nothing.
	up.set(func(u *hourlyUpstream) { u.putStatus = http.StatusInternalServerError })
	other := run(still, "2026-11-04T15:00:00Z")
	if !strings.Contains(string(other), "HTTP 500") {
		t.Fatalf("state %s", other)
	}
	check(t, schedulesDiff(t, still, other), []string{})
	// auto_enable off ends the streak quietly; on again and failing, a new one.
	quiet := schedulesRun(t, schedulesCfg(srv.URL, "success", 3), other, at("2026-11-04T16:00:00Z"))
	check(t, schedulesDiff(t, other, quiet), []string{})
	again := run(quiet, "2026-11-04T17:00:00Z")
	if got := schedulesDiff(t, quiet, again); len(got) != 1 || !strings.HasPrefix(got[0], disabled+`"reenable_error":"HTTP 500: `) {
		t.Fatalf("events %q", got)
	}
	up.set(func(u *hourlyUpstream) { u.putStatus = 0 })
	back := run(again, "2026-11-04T18:00:00Z")
	if got := schedulesDiff(t, again, back); len(got) != 1 || !strings.HasPrefix(got[0], `schedule.reenabled .github/workflows/nightly.yml {"from":"disabled_inactivity"`) {
		t.Fatalf("events %q", got)
	}
}

// 019-AC15: diffSchedules on stored states alone: a new failure on a workflow already disabled is reported once.
func TestDiffSchedulesReenableStreak(t *testing.T) {
	wf := func(state, reenableErr string) SchedulesState {
		return SchedulesState{Workflows: []ScheduledWorkflow{{Path: "n.yml", State: state, ReenableError: reenableErr}}}
	}
	for _, tt := range []struct {
		name     string
		old, cur SchedulesState
		want     string
	}{
		{"streak starts", wf("disabled_inactivity", ""), wf("disabled_inactivity", "HTTP 403: no"), "schedule.disabled"},
		{"streak goes on", wf("disabled_inactivity", "HTTP 403: no"), wf("disabled_inactivity", "HTTP 403: no"), ""},
		{"reason changes", wf("disabled_inactivity", "HTTP 403: no"), wf("disabled_inactivity", "HTTP 502: down"), ""},
		{"from active, one event", wf("active", ""), wf("disabled_inactivity", "HTTP 403: no"), "schedule.disabled"},
		{"stays disabled, no PUT", wf("disabled_inactivity", ""), wf("disabled_inactivity", ""), ""},
		{"by hand, no PUT", wf("disabled_manually", ""), wf("disabled_manually", ""), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, e := range diffSchedules(tt.old, tt.cur) {
				got = append(got, e.Type)
			}
			if strings.Join(got, ",") != tt.want {
				t.Fatalf("events %q", got)
			}
		})
	}
}

// 019-AC4: a workflow stored disabled and listed active again with an old updated_at (a re-enable gedour did not see
// succeed: a lost PUT answer, or by hand) sends schedule.enabled only: the time gedour saw it active is a staleness
// reference too, and the count starts after it.
func TestGitHubSchedulesActiveSince(t *testing.T) {
	up := &hourlyUpstream{state: "disabled_inactivity", success: "2026-10-31T08:05:00Z"}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := schedulesCfg(srv.URL, "success", 3)
	off := schedulesRun(t, cfg, nil, at("2026-11-04T12:00:00Z"))
	up.set(func(u *hourlyUpstream) { u.state = "" })
	on := schedulesRun(t, cfg, off, at("2026-11-04T13:00:00Z"))
	check(t, schedulesDiff(t, off, on), []string{
		`schedule.enabled .github/workflows/nightly.yml {"last_alive_at":"2026-10-31T08:05:00Z","path":".github/workflows/nightly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"}`,
	})
	w := schedulesState(t, on)[".github/workflows/nightly.yml"]
	if w.ActiveSince == nil || !w.ActiveSince.Equal(at("2026-11-04T13:00:00Z")) || w.ReenabledAt != nil {
		t.Fatalf("state %s", on)
	}
	// 14:00 and 15:00 are past the grace at 16:01, two of three; 16:00 joins them at 17:01.
	quiet := schedulesRun(t, cfg, on, at("2026-11-04T16:01:00Z"))
	check(t, schedulesDiff(t, on, quiet), []string{})
	stale := schedulesRun(t, cfg, quiet, at("2026-11-04T17:01:00Z"))
	check(t, schedulesDiff(t, quiet, stale), []string{
		`schedule.stale .github/workflows/nightly.yml {"expected_at":"2026-11-04T14:00:00Z","last_alive_at":"2026-10-31T08:05:00Z","missed":3,"path":".github/workflows/nightly.yml","state":"active","url":"https://github.com/acme/widgets/blob/main/.github/workflows/nightly.yml","workflow":"Nightly"}`,
	})
	// A workflow first seen active is no return: no active_since, its first run reports it stale (019-AC9).
	if first := schedulesRun(t, cfg, nil, at("2026-11-04T17:01:00Z")); strings.Contains(string(first), "active_since") {
		t.Fatalf("first run %s", first)
	}
}
