package collect

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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
		lists         string // the state after "releases":[...]
		wantValidator bool
	}{
		{"boxes off", map[string]any{"repo": "cli/cli", "api_url": srv.URL}, 0, `}`, true},
		{"new_issues", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_issues": true}, 1, `,"issues":[9511,9509]}`, false},
		{"new_prs", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_prs": true}, 1, `,"prs":[9512,9508]}`, false},
		{"both", map[string]any{"repo": "cli/cli", "api_url": srv.URL, "new_issues": true, "new_prs": true}, 1, `,"issues":[9511,9509],"prs":[9512,9508]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rel0, iss0 := up.Hits("/repos/cli/cli/releases"), up.Hits("/repos/cli/cli/issues")
			f := fetcher(t)
			b, err := c.Collect(t.Context(), f, cfgJSON(t, tt.cfg), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r, i := up.Hits("/repos/cli/cli/releases")-rel0, up.Hits("/repos/cli/cli/issues")-iss0; r != 1 || i != tt.issueHits {
				t.Fatalf("hits: releases %d, issues %d", r, i)
			}
			if !strings.HasSuffix(string(b), `]`+tt.lists) || strings.Count(string(b), "[") != 1+strings.Count(tt.lists, "[") {
				t.Fatalf("state %s", b)
			}
			if got := len(f.Updates) == 1; got != tt.wantValidator {
				t.Fatalf("validators %v", f.Updates)
			}
			if (f.Items != nil) != (tt.issueHits == 1) {
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
