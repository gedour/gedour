package collect

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gedour/gedour/internal/testupstream"
)

func items(t *testing.T, set string, issues, prs bool) (ReleaseState, map[int]Event) {
	t.Helper()
	is, ps, opened, err := normalizeGitHubItems(testupstream.Fixture("github_issues", set), issues, prs)
	if err != nil {
		t.Fatal(err)
	}
	var s ReleaseState
	if issues {
		s.Issues = &is
	}
	if prs {
		s.PRs = &ps
	}
	return s, opened
}

func ptr(n ...int) *[]int { return &n }

// 008-AC5: fixture B adds one issue and one PR; each box reports its own kind of item, with the payload.
func TestGitHubItemsFixtures(t *testing.T) {
	const issue = `issue.opened 9513 {"author":"alice-q","created_at":"2026-11-02T14:22:09Z","labels":["bug","enhancement"],` +
		`"number":9513,"title":"` + "`gh pr checks --watch` exits 0 on a cancelled run" + `","url":"https://github.com/cli/cli/issues/9513"}`
	const pr = `pr.opened 9514 {"author":"dependabot[bot]","created_at":"2026-11-03T06:00:05Z","labels":[],` +
		`"number":9514,"title":"Bump github.com/spf13/cobra to 1.11.0","url":"https://github.com/cli/cli/pull/9514"}`
	for _, tt := range []struct {
		name        string
		issues, prs bool
		want        []string
	}{
		{"both", true, true, []string{issue, pr}},
		{"issues only", true, false, []string{issue}},
		{"prs only", false, true, []string{pr}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := items(t, "a", tt.issues, tt.prs)
			b, opened := items(t, "b", tt.issues, tt.prs)
			if (a.Issues != nil && !slices.Equal(*a.Issues, []int{9511, 9509})) || (a.PRs != nil && !slices.Equal(*a.PRs, []int{9512, 9508})) {
				t.Fatalf("lists A: %v %v", a.Issues, a.PRs)
			}
			check(t, summary(t, WithItems(diffReleases(a, b), opened)), tt.want)
			check(t, summary(t, WithItems(diffReleases(b, b), opened)), []string{})
		})
	}
	if _, _, _, err := normalizeGitHubItems([]byte(`{"message":"x"}`), true, true); err == nil {
		t.Fatal("object accepted as issue list")
	}
}

// 008-AC6, 008-AC7: which numbers are new, from the stored list and the current one.
func TestNewNumbers(t *testing.T) {
	for _, tt := range []struct {
		name     string
		old, cur *[]int
		want     []int
	}{
		{"008-AC6 no list stored (before 008)", nil, ptr(5, 4), nil},
		{"boxes switched off", ptr(5, 4), nil, nil},
		{"empty list stored, first item", ptr(), ptr(1), []int{1}},
		{"two new, oldest first", ptr(10, 8), ptr(12, 11, 10, 8), []int{11, 12}},
		{"gap above the floor is new", ptr(10, 8), ptr(10, 9, 8), []int{9}},
		{"008-AC7 below the oldest stored", ptr(10, 8), ptr(10, 8, 7), nil},
		{"008-AC7 window slid back after a deletion", ptr(12, 11, 10), ptr(12, 10, 9, 3), nil},
		{"008-AC7 stored list unsorted", ptr(8, 10), ptr(10, 8, 7, 11), []int{11}},
		{"unchanged", ptr(3, 2), ptr(3, 2), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := newNumbers(tt.old, tt.cur); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// 008-AC6, 008-AC7: the opened placeholders of a diff, per kind list, through box toggles.
func TestItemDiff(t *testing.T) {
	st := func(issues, prs *[]int) ReleaseState { return ReleaseState{Issues: issues, PRs: prs} }
	for _, tt := range []struct {
		name     string
		old, cur ReleaseState
		want     []string
	}{
		{"008-AC6 new_prs checked while new_issues on: a PR opened before the check is no event",
			st(ptr(10, 9), nil), st(ptr(10, 9), ptr(11)), nil},
		{"008-AC6 swap issues for prs", st(ptr(10, 9), nil), st(nil, ptr(11, 8)), nil},
		{"008-AC6 swap prs for issues", st(nil, ptr(11, 8)), st(ptr(12, 10), nil), nil},
		{"new_issues unchecked, new_prs stays on", st(ptr(10), ptr(11)), st(nil, ptr(13, 11)), []string{"13"}},
		{"both on, both kinds new, oldest first", st(ptr(10), ptr(11)), st(ptr(14, 12, 10), ptr(13, 11)), []string{"12", "13", "14"}},
		{"008-AC7 per list floor", st(ptr(10, 9), ptr(20)), st(ptr(10, 9, 8), ptr(20, 15)), nil},
		{"empty lists stored, first items", st(ptr(), ptr()), st(ptr(2), ptr(1)), []string{"1", "2"}},
		{"both unchecked", st(ptr(10), ptr(11)), st(nil, nil), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, e := range diffReleases(tt.old, tt.cur) {
				got = append(got, e.Subject)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// 008-AC6: a state stored without numbers, then the current list: no opened event, releases still diff.
func TestItemsBaselineOnOldState(t *testing.T) {
	old, err := normalizeGitHubReleases(testupstream.Fixture("github_repo", "a"), true)
	if err != nil {
		t.Fatal(err)
	}
	cur, opened := items(t, "b", true, true)
	cur.Releases = old.Releases
	if ev := WithItems(diffReleases(old, cur), opened); len(ev) != 0 {
		t.Fatalf("events %v", summary(t, ev))
	}
}

// Placeholders without a run item are dropped; release events pass through untouched, and their notes skip items.
func TestWithItems(t *testing.T) {
	rel := Event{Type: "release.published", Subject: "v1", Payload: map[string]any{}}
	got := WithItems([]Event{{Type: itemListed, Subject: "7"}, rel, {Type: itemListed, Subject: "8"}},
		map[int]Event{8: {Type: "pr.opened", Subject: "8", Payload: map[string]any{"number": 8}}})
	WithNotes(got, map[string]string{"v1": "CVE-2026-1", "8": "CVE-2026-2"})
	check(t, summary(t, got), []string{`release.published v1 {"body":"CVE-2026-1","cves":["CVE-2026-1"]}`, `pr.opened 8 {"number":8}`})
	// An issue converted to a discussion keeps its number: each placeholder takes the item of its own list only.
	run := map[int]Event{3: {Type: "discussion.opened", Subject: "3", Payload: map[string]any{}}, 4: {Type: "issue.opened", Subject: "4", Payload: map[string]any{}}}
	got = WithItems([]Event{{Type: itemListed, Subject: "3"}, {Type: discussionListed, Subject: "4"}, {Type: discussionListed, Subject: "3"}}, run)
	WithNotes(got, map[string]string{"3": "CVE-2026-3"})
	check(t, summary(t, got), []string{`discussion.opened 3 {}`})
	if ev := WithItems([]Event{{Type: itemListed, Subject: "1"}}, nil); len(ev) != 0 {
		t.Fatalf("nil items kept %v", ev)
	}
}

// 008-AC4: both boxes off, a github_repo run makes the one releases request, conditional, and stores no
// numbers; a box on adds the issues request and stores them.
func TestGitHubRepoRequests(t *testing.T) {
	up := testupstream.New()
	srv := httptest.NewServer(up)
	defer srv.Close()
	c := Collectors()["github_repo"]
	for _, tt := range []struct {
		name          string
		cfg           map[string]any
		issueHits     int
		discHits      int
		lists         string // the state after "releases":[...]
		wantValidator bool
	}{
		// 011-AC1: boxes off, no discussions request.
		{"boxes off", map[string]any{"repo": "cli/cli", "api_url": srv.URL}, 0, 0, `}`, true},
		{"new_issues", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_issues": true}, 1, 0, `,"issues":[9511,9509]}`, false},
		{"new_prs", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_prs": true}, 1, 0, `,"prs":[9512,9508]}`, false},
		{"both", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_issues": true, "new_prs": true}, 1, 0, `,"issues":[9511,9509],"prs":[9512,9508]}`, false},
		// 011-AC2: the discussions box alone reads the releases unconditionally, and the discussions (probe and page), not the issues.
		{"new_discussions", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_discussions": true}, 0, 2, `,"discussions":[9510,9507]}`, false},
		{"all boxes", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_issues": true, "new_prs": true, "new_discussions": true}, 1, 2,
			`,"issues":[9511,9509],"prs":[9512,9508],"discussions":[9510,9507]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rel0, iss0, disc0 := up.Hits("/repos/cli/cli/releases"), up.Hits("/repos/cli/cli/issues"), up.Hits("/repos/cli/cli/discussions")
			f := fetcher(t)
			b, err := c.Collect(t.Context(), f, cfgJSON(t, tt.cfg), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r, i, d := up.Hits("/repos/cli/cli/releases")-rel0, up.Hits("/repos/cli/cli/issues")-iss0, up.Hits("/repos/cli/cli/discussions")-disc0; r != 1 || i != tt.issueHits || d != tt.discHits {
				t.Fatalf("hits: releases %d, issues %d, discussions %d", r, i, d)
			}
			if !strings.HasSuffix(string(b), `]`+tt.lists) || strings.Count(string(b), "[") != 1+strings.Count(tt.lists, "[") {
				t.Fatalf("state %s", b)
			}
			if got := len(f.Updates) == 1; got != tt.wantValidator {
				t.Fatalf("validators %v", f.Updates)
			}
			if (f.Items != nil) != (tt.issueHits+tt.discHits > 0) {
				t.Fatalf("items %v", f.Items)
			}
		})
	}
}

// A box on: the releases URL's stored ETag is not sent, so a 304 on it cannot skip the issues list, and an
// empty list stores [] (stored, nothing in it), not nothing.
func TestGitHubRepoItemsIgnoreReleaseETag(t *testing.T) {
	var inm []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm = append(inm, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"same"`)
		if r.Header.Get("If-None-Match") == `"same"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	f := fetcher(t)
	f.Cache = map[string]Validators{srv.URL + "/repos/o/r/releases?per_page=50": {ETag: `"same"`},
		srv.URL + "/repos/o/r/issues?state=all&sort=created&direction=desc&per_page=50": {ETag: `"same"`}}
	b, err := Collectors()["github_repo"].Collect(t.Context(), f, cfgJSON(t, map[string]any{"repo": "o/r", "api_url": srv.URL, "new_prs": true}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inm) != 2 || inm[0] != "" || inm[1] != "" {
		t.Fatalf("If-None-Match sent: %q", inm)
	}
	if string(b) != `{"releases":null,"prs":[]}` {
		t.Fatalf("state %s", b)
	}
	// Boxes off, the same cache answers 304: the run is unchanged, as before 008.
	f.Updates = nil
	if _, err := Collectors()["github_repo"].Collect(t.Context(), f, cfgJSON(t, map[string]any{"repo": "o/r", "api_url": srv.URL}), nil); !errors.Is(err, ErrNotModified) {
		t.Fatalf("boxes off: %v", err)
	}
	var st ReleaseState
	if err := json.Unmarshal(b, &st); err != nil || st.PRs == nil || len(*st.PRs) != 0 || st.Issues != nil {
		t.Fatalf("decoded %+v %v", st, err)
	}
}

// An explicit "pull_request": null, as a GitHub-compatible server may send, is an issue.
func TestPullRequestNullIsIssue(t *testing.T) {
	_, _, opened, err := normalizeGitHubItems([]byte(`[{"number":1,"pull_request":null},{"number":2,"pull_request":{}},{"number":3}]`), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if opened[1].Type != "issue.opened" || opened[2].Type != "pr.opened" || opened[3].Type != "issue.opened" {
		t.Fatalf("%v", opened)
	}
}

// 011-AC2: fixture B adds one discussion, reported once with its payload; A to A and B to B are quiet.
func TestGitHubDiscussionsFixtures(t *testing.T) {
	disc := func(set string) (ReleaseState, map[int]Event) {
		t.Helper()
		nums, opened := []int{}, map[int]Event{}
		if err := normalizeGitHubDiscussions(testupstream.Fixture("github_discussions", set), &nums, opened); err != nil {
			t.Fatal(err)
		}
		return ReleaseState{Discussions: &nums}, opened
	}
	a, _ := disc("a")
	b, opened := disc("b")
	check(t, summary(t, WithItems(diffReleases(a, b), opened)), []string{`discussion.opened 9515 {"author":"alice-q","category":"Ideas",` +
		`"created_at":"2026-11-03T11:47:30Z","labels":["enhancement"],"number":9515,"title":"Idea: ` + "`gh run watch --notify`" + `",` +
		`"url":"https://github.com/cli/cli/discussions/9515"}`})
	check(t, summary(t, WithItems(diffReleases(b, b), opened)), []string{})
	if ev := opened[9515]; ev.OccurredAt == nil || ev.OccurredAt.Format(time.RFC3339) != "2026-11-03T11:47:30Z" {
		t.Fatalf("occurred at %v", ev.OccurredAt)
	}
	if err := normalizeGitHubDiscussions([]byte(`{"message":"x"}`), new([]int), map[int]Event{}); err == nil {
		t.Fatal("object accepted as discussion list")
	}
}

// 011-AC3, 011-AC5: baseline and floor of the discussions list, alongside the issues one.
func TestDiscussionDiff(t *testing.T) {
	st := func(issues, disc *[]int) ReleaseState { return ReleaseState{Issues: issues, Discussions: disc} }
	for _, tt := range []struct {
		name     string
		old, cur ReleaseState
		want     []string
	}{
		{"011-AC3 box just checked: baseline", st(ptr(10), nil), st(ptr(10), ptr(12, 11)), nil},
		{"011-AC3 state stored before 011", ReleaseState{}, st(nil, ptr(12)), nil},
		{"one new discussion", st(nil, ptr(11, 9)), st(nil, ptr(12, 11, 9)), []string{"discussion.listed 12"}},
		{"011-AC5 below the oldest stored", st(nil, ptr(11, 9)), st(nil, ptr(11, 9, 8)), nil},
		{"issues and discussions interleave in number order", st(ptr(10), ptr(11)), st(ptr(14, 12, 10), ptr(13, 11)),
			[]string{"item.listed 12", "discussion.listed 13", "item.listed 14"}},
		{"box unchecked", st(nil, ptr(11)), st(nil, nil), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, e := range diffReleases(tt.old, tt.cur) {
				got = append(got, e.Type+" "+e.Subject)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// discussionList serves an oldest-first discussions list like GitHub: per_page and page, a Link rel="last" past one
// page, and counts requests by "per_page/page"; failPage ("1/1" is the probe) answers status instead.
type discussionList struct {
	mu       sync.Mutex
	nums     []int
	hits     map[string]int
	failPage string
	status   int
	link     string // overrides the Link header when set
}

func (d *discussionList) remove(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nums = slices.DeleteFunc(d.nums, func(x int) bool { return x == n })
}

func (d *discussionList) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/repos/o/r/releases" {
		_, _ = w.Write([]byte(`[]`))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page := max(1, func() int { n, _ := strconv.Atoi(r.URL.Query().Get("page")); return n }())
	key := fmt.Sprintf("%d/%d", per, page)
	d.hits[key]++
	if key == d.failPage {
		w.WriteHeader(d.status)
		_, _ = w.Write([]byte(`{"message":"Discussions are disabled for this repo"}`))
		return
	}
	if last := (len(d.nums) + per - 1) / per; last > 1 {
		w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/repositories/1/discussions?per_page=%d&page=%d>; rel="last"`, per, last))
	}
	if d.link != "" {
		w.Header().Set("Link", d.link)
	}
	items := []map[string]any{}
	for _, n := range d.nums[min((page-1)*per, len(d.nums)):min(page*per, len(d.nums))] {
		items = append(items, map[string]any{"number": n, "title": fmt.Sprint("d", n), "user": map[string]any{"login": "u"},
			"category": map[string]any{"name": "General"}, "labels": []any{}, "html_url": fmt.Sprint("https://github.com/o/r/discussions/", n),
			"created_at": "2026-11-01T00:00:00Z"})
	}
	_ = json.NewEncoder(w).Encode(items)
}

func discussionPages(t *testing.T, total int) (*httptest.Server, *discussionList) {
	t.Helper()
	d := &discussionList{hits: map[string]int{}}
	for n := 1; n <= total; n++ {
		d.nums = append(d.nums, n)
	}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return srv, d
}

func runDiscussions(t *testing.T, srv *httptest.Server) (ReleaseState, map[int]Event, error) {
	t.Helper()
	f := fetcher(t)
	b, err := Collectors()["github_repo"].Collect(t.Context(), f, cfgJSON(t, map[string]any{"repo": "o/r", "api_url": srv.URL, "new_discussions": true}), nil)
	if err != nil {
		if b != nil || f.Items != nil {
			t.Fatalf("partial result on %v: %s %v", err, b, f.Items)
		}
		return ReleaseState{}, nil, err
	}
	var st ReleaseState
	if err := json.Unmarshal(b, &st); err != nil || st.Discussions == nil {
		t.Fatalf("state %s %v", b, err)
	}
	return st, f.Items, nil
}

// 011-AC4: the window read, by list size: a per_page=1 probe, the last per_page=100 page, and the page before it
// when the last is short; never a page twice, at most 3 discussions requests, the probe item never stored.
func TestGitHubDiscussionsPaging(t *testing.T) {
	for _, tt := range []struct {
		name  string
		total int
		pages map[string]int
		lo    int // the oldest number stored, 0 for none
	}{
		{"empty list: the probe alone", 0, map[string]int{"1/1": 1}, 0},
		{"one discussion: the probe alone", 1, map[string]int{"1/1": 1}, 1},
		{"one page", 14, map[string]int{"1/1": 1, "100/1": 1}, 1},
		{"011-AC4 last page of 14 after a full page", 114, map[string]int{"1/1": 1, "100/2": 1, "100/1": 1}, 1},
		{"011-AC4 last page of 14 after two full pages", 214, map[string]int{"1/1": 1, "100/3": 1, "100/2": 1}, 101},
		{"last page holds 50: the page before is not read", 250, map[string]int{"1/1": 1, "100/3": 1}, 201},
		{"last page full", 300, map[string]int{"1/1": 1, "100/3": 1}, 201},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, d := discussionPages(t, tt.total)
			st, _, err := runDiscussions(t, srv)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(d.hits, tt.pages) {
				t.Fatalf("pages read %v, want %v", d.hits, tt.pages)
			}
			got := *st.Discussions
			if tt.lo == 0 {
				if len(got) != 0 {
					t.Fatalf("window %v", got)
				}
				return
			}
			if len(got) != tt.total-tt.lo+1 || got[0] != tt.total || slices.Min(got) != tt.lo {
				t.Fatalf("window of %d, %d..%d", len(got), slices.Min(got), got[0])
			}
			if tt.total != 214 {
				return
			}
			// 011-AC4: from 199 to 214 discussions, #200 ends the page before the last and is reported.
			srv, d = discussionPages(t, 199)
			old, _, err := runDiscussions(t, srv)
			if err != nil {
				t.Fatal(err)
			}
			for n := 200; n <= 214; n++ {
				d.nums = append(d.nums, n)
			}
			cur, items, err := runDiscussions(t, srv)
			if err != nil {
				t.Fatal(err)
			}
			ev := summary(t, WithItems(diffReleases(old, cur), items))
			if len(ev) != 15 || !strings.HasPrefix(ev[0], "discussion.opened 200 ") {
				t.Fatalf("events %q", ev)
			}
		})
	}
}

// 011-AC5: a deletion shifts the pages; what slides into the window is below the floor, so nothing is new.
func TestGitHubDiscussionsDeletion(t *testing.T) {
	seq := func(lo, hi int, more ...int) []int {
		var out []int
		for n := lo; n <= hi; n++ {
			out = append(out, n)
		}
		return append(out, more...)
	}
	for _, tt := range []struct {
		name    string
		nums    []int
		deleted int
		pages   map[string]int // second run
	}{
		{"011-AC5 deletion in the oldest page", seq(1, 250), 5, map[string]int{"1/1": 1, "100/3": 1, "100/2": 1}},
		{"011-AC5 deletion on the last page: 50 to 49, the page before is read", seq(1, 250), 210, map[string]int{"1/1": 1, "100/3": 1, "100/2": 1}},
		// GitHub lists a converted issue last, with its old number.
		{"011-AC5 converted issue at the end, then the last page goes 50 to 49", seq(1000, 1248, 500), 1240, map[string]int{"1/1": 1, "100/3": 1, "100/2": 1}},
		// The converted issue heads the last page, then a deletion widens the window.
		{"011-AC5 converted issue first in the window, then the last page goes 50 to 49", append(seq(1000, 1199, 500), seq(1200, 1248)...), 1005, map[string]int{"1/1": 1, "100/3": 1, "100/2": 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, d := discussionPages(t, 0)
			d.nums = tt.nums
			old, _, err := runDiscussions(t, srv)
			if err != nil {
				t.Fatal(err)
			}
			d.remove(tt.deleted)
			d.hits = map[string]int{}
			cur, items, err := runDiscussions(t, srv)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(d.hits, tt.pages) {
				t.Fatalf("pages read %v, want %v", d.hits, tt.pages)
			}
			if ev := summary(t, WithItems(diffReleases(old, cur), items)); len(ev) != 0 {
				t.Fatalf("%d events, first %q", len(ev), ev[0])
			}
		})
	}
}

// 011-AC6: a 410 or a failing discussions call, on any page, fails the run with no state and no items; the 410
// names the box. A Link with no readable last page number fails too.
func TestGitHubDiscussionsFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		total    int
		failPage string
		status   int
		link     string
		want     string
	}{
		{"011-AC6 disabled", 14, "1/1", http.StatusGone, "", `discussions are disabled on o/r: uncheck "new discussions"`},
		{"011-AC6 server error", 14, "1/1", http.StatusInternalServerError, "", "HTTP 500"},
		{"011-AC6 last page fails", 214, "100/3", http.StatusInternalServerError, "", "HTTP 500"},
		{"011-AC6 page before the last fails", 214, "100/2", http.StatusGone, "", `uncheck "new discussions"`},
		{"Link without a page", 214, "", 0, `<https://api.github.com/repositories/1/discussions?per_page=1>; rel="last"`, "no last page number"},
		{"Link without rel=last", 214, "", 0, `<https://api.github.com/repositories/1/discussions?per_page=1&page=2>; rel="next"`, "no last page number"},
		{"Link with page 0", 214, "", 0, `<https://api.github.com/repositories/1/discussions?per_page=1&page=0>; rel="last"`, "no last page number"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, d := discussionPages(t, tt.total)
			d.failPage, d.status, d.link = tt.failPage, tt.status, tt.link
			if _, _, err := runDiscussions(t, srv); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

// 011-AC5: only a number past every stored one is new, wherever a converted issue's lower number sits.
func TestNewDiscussions(t *testing.T) {
	for _, tt := range []struct {
		old, cur *[]int
		want     []int
	}{
		{nil, &[]int{3}, nil},
		{&[]int{}, &[]int{3}, []int{3}},
		{&[]int{1248, 1200, 500}, &[]int{1250, 1249, 1248, 1199, 1100, 500}, []int{1249, 1250}},
		{&[]int{12, 11, 10}, &[]int{12, 11, 10, 3}, nil},
		// A converted issue numbered above every stored discussion is reported, as on an empty list.
		{&[]int{1002, 1001, 1000}, &[]int{1300, 1002, 1001, 1000}, []int{1300}},
		{&[]int{}, &[]int{30, 20, 10}, []int{10, 20, 30}},
	} {
		if got := newDiscussions(tt.old, tt.cur); !slices.Equal(got, tt.want) {
			t.Errorf("newDiscussions(%v, %v) = %v, want %v", tt.old, tt.cur, got, tt.want)
		}
	}
}
