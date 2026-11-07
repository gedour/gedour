package collect

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/watch"
)

// summary renders events as "type subject payload" so whole diffs compare in one assertion.
func summary(t *testing.T, events []Event) []string {
	t.Helper()
	out := []string{}
	for _, e := range events {
		p, err := json.Marshal(e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Type+" "+e.Subject+" "+string(p))
	}
	return out
}

func check(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("events:\n got  %q\n want %q", got, want)
	}
}

func tags(rs []Release) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Tag)
	}
	return out
}

func flatten(pages [][]string) []string { return slices.Concat(pages...) }

// 001-AC7: github_repo normalizes fixture A, and B yields exactly the expected events, with and
// without prereleases. Diffing a state with itself yields none.
func TestGitHubReleaseFixtures(t *testing.T) {
	a := testupstream.Fixture("github_repo", "a")
	b := testupstream.Fixture("github_repo", "b")
	for _, tt := range []struct {
		name     string
		pre      bool
		wantA    []string
		wantDiff []string
	}{
		{"with prereleases", true, []string{"v2.105.0", "v2.104.0-rc.1", "v2.103.2", "v2.103.1"}, []string{
			`release.promoted v2.104.0-rc.1 {"name":"GitHub CLI 2.104.0-rc.1","prerelease":false,"tag":"v2.104.0-rc.1","url":"https://github.com/cli/cli/releases/tag/v2.104.0-rc.1"}`,
			`release.published v2.103.3 {"name":"GitHub CLI 2.103.3","prerelease":false,"tag":"v2.103.3","url":"https://github.com/cli/cli/releases/tag/v2.103.3"}`,
			`release.published v2.105.0 {"name":"GitHub CLI 2.105.0","prerelease":false,"tag":"v2.105.0","url":"https://github.com/cli/cli/releases/tag/v2.105.0"}`,
		}},
		// Without prereleases the promoted rc is a first publication.
		{"releases only", false, []string{"v2.105.0", "v2.103.2", "v2.103.1"}, []string{
			`release.published v2.104.0-rc.1 {"name":"GitHub CLI 2.104.0-rc.1","prerelease":false,"tag":"v2.104.0-rc.1","url":"https://github.com/cli/cli/releases/tag/v2.104.0-rc.1"}`,
			`release.published v2.103.3 {"name":"GitHub CLI 2.103.3","prerelease":false,"tag":"v2.103.3","url":"https://github.com/cli/cli/releases/tag/v2.103.3"}`,
			`release.published v2.105.0 {"name":"GitHub CLI 2.105.0","prerelease":false,"tag":"v2.105.0","url":"https://github.com/cli/cli/releases/tag/v2.105.0"}`,
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sa, err := normalizeGitHubReleases(a, tt.pre)
			if err != nil {
				t.Fatal(err)
			}
			sb, err := normalizeGitHubReleases(b, tt.pre)
			if err != nil {
				t.Fatal(err)
			}
			if got := tags(sa.Releases); !slices.Equal(got, tt.wantA) {
				t.Fatalf("state A: %v", got)
			}
			if d := sa.Releases[0]; !d.Draft || d.PublishedAt != nil {
				t.Fatalf("draft: %+v", d)
			}
			check(t, summary(t, diffReleases(sa, sb)), tt.wantDiff)
			check(t, summary(t, diffReleases(sb, sb)), []string{})
		})
	}
	if _, err := normalizeGitHubReleases([]byte(`{"message":"x"}`), true); err == nil {
		t.Fatal("object accepted as release list")
	}
}

// 001-AC7: github_pr folds the PR, its reviews, check runs and statuses into one state.
func TestGitHubPRFixtures(t *testing.T) {
	norm := func(set string) PRState {
		s, err := normalizeGitHubPR(testupstream.Part("github_pr", set, "pull"), testupstream.Part("github_pr", set, "reviews"),
			testupstream.Part("github_pr", set, "check_runs"), testupstream.Part("github_pr", set, "status"))
		if err != nil {
			t.Fatal(err)
		}
		if s.Comments, err = normalizeComments(testupstream.Part("github_pr", set, "reviews"),
			testupstream.Part("github_pr", set, "review_comments"), testupstream.Part("github_pr", set, "issue_comments")); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := norm("a"), norm("b")
	want := PRState{Number: 14925, State: "open", Draft: true, Title: "Preserve existing SSH connection commands",
		ReviewDecision: "review_required", CI: "pending", HeadSHA: "2203164ed0fd16789e7d9e22679cd708a70c13c2", URL: "https://github.com/cli/cli/pull/14925",
		Comments: []PRComment{{Author: "author", CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
			URL: "https://github.com/cli/cli/pull/14925#issuecomment-3500001", Excerpt: "Rebased on trunk."}}}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("state A:\n got  %+v\n want %+v", a, want)
	}
	const base, url = `"number":14925,"title":"Preserve existing SSH connection commands"`, `"url":"https://github.com/cli/cli/pull/14925"`
	check(t, summary(t, diffPR(a, b)), []string{
		`pr.draft_changed true>false {"from":true,` + base + `,"to":false,` + url + `}`,
		`pr.review_changed "review_required">"approved" {"from":"review_required",` + base + `,"to":"approved",` + url + `}`,
		`pr.ci_changed "pending">"success" {"from":"pending",` + base + `,"to":"success",` + url + `}`,
		// Oldest first; the reply's quote is left out of its excerpt.
		`pr.commented https://github.com/cli/cli/pull/14925#issuecomment-3500002 {"author":"maintainer","excerpt":"Thanks, looks good now. One nit below.",` + base + `,"url":"https://github.com/cli/cli/pull/14925#issuecomment-3500002"}`,
		`pr.commented https://github.com/cli/cli/pull/14925#discussion_r2400001 {"author":"maintainer","excerpt":"Nit: rename this flag.",` + base + `,"url":"https://github.com/cli/cli/pull/14925#discussion_r2400001"}`,
		`pr.commented https://github.com/cli/cli/pull/14925#pullrequestreview-13785781 {"author":"other","excerpt":"Second pass done.",` + base + `,"url":"https://github.com/cli/cli/pull/14925#pullrequestreview-13785781"}`,
	})
	check(t, summary(t, diffPR(b, b)), []string{})
	// A state stored before comments were collected is a baseline: no backlog of old comments.
	legacy := b
	legacy.Comments = nil
	check(t, summary(t, diffPR(legacy, b)), []string{})
}

func TestGitHubPRComments(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 5, 10, m, 0, 0, time.UTC) }
	c := func(m int) PRComment { return PRComment{CreatedAt: at(m), URL: fmt.Sprintf("u%02d", m)} }
	window := func(from, to int) []PRComment {
		var out []PRComment
		for m := from; m <= to; m++ {
			out = append(out, c(m))
		}
		return out
	}
	urls := func(cs []PRComment) []string {
		out := []string{}
		for _, x := range cs {
			out = append(out, x.URL)
		}
		return out
	}
	for _, tt := range []struct {
		name     string
		old, cur []PRComment
		want     []string
	}{
		{"first comment on an empty PR", []PRComment{}, window(0, 0), []string{"u00"}},
		{"deleted comment is no event", window(0, 2), window(0, 1), []string{}},
		{"full window slides forward", window(0, maxComments-1), window(2, maxComments+1), []string{fmt.Sprintf("u%02d", maxComments), fmt.Sprintf("u%02d", maxComments+1)}},
		// One comment deleted from a full window lets an older one slide back in.
		{"older comment sliding back in", window(1, maxComments), append(window(0, 4), window(6, maxComments)...), []string{}},
		// A deletion moves the last page back to a full one: its older comments are no news.
		{"shorter last page grows back", window(maxComments+5, maxComments+6), window(maxComments-14, maxComments+5), []string{}},
		{"same comment edited", []PRComment{{CreatedAt: at(0), URL: "u00", Excerpt: "a"}}, []PRComment{{CreatedAt: at(0), URL: "u00", Excerpt: "b"}}, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check(t, urls(newComments(tt.old, tt.cur)), tt.want)
		})
	}

	// Newest maxComments kept, oldest first, across the three sources; textless reviews are skipped.
	var issue []map[string]any
	for m := range maxComments + 5 {
		issue = append(issue, map[string]any{"user": map[string]string{"login": "a"}, "body": "hi", "html_url": fmt.Sprintf("i%02d", m), "created_at": at(m)})
	}
	ib, _ := json.Marshal(issue)
	reviews := `[{"user":{"login":"r"},"body":"","state":"APPROVED","html_url":"r0","submitted_at":"2026-11-05T11:00:00Z"},
		{"user":{"login":"r"},"body":"late review","state":"COMMENTED","html_url":"r1","submitted_at":"2026-11-05T11:01:00Z"}]`
	got, err := normalizeComments([]byte(reviews), []byte(`[]`), ib)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxComments || got[0].URL != "i06" || got[len(got)-1].URL != "r1" {
		t.Fatalf("window %v", urls(got))
	}
	// One review, one comment: a review body carries its inline comments, else the first of them does.
	inline := `[{"id":13,"pull_request_review_id":2,"user":{"login":"q"},"body":"third","html_url":"c13","created_at":"2026-11-05T12:03:00Z"},
		{"id":12,"pull_request_review_id":2,"user":{"login":"q"},"body":"second","html_url":"c12","created_at":"2026-11-05T12:02:00Z"},
		{"id":11,"pull_request_review_id":2,"user":{"login":"q"},"body":"first","html_url":"c11","created_at":"2026-11-05T12:01:00Z"},
		{"id":10,"pull_request_review_id":1,"user":{"login":"r"},"body":"under the body","html_url":"c10","created_at":"2026-11-05T11:00:30Z"},
		{"id":14,"pull_request_review_id":3,"user":{"login":"s"},"body":"a reply","html_url":"c14","created_at":"2026-11-05T12:04:00Z"}]`
	reviews = `[{"id":1,"user":{"login":"r"},"body":"overall fine","state":"COMMENTED","html_url":"r1","submitted_at":"2026-11-05T11:01:00Z"},
		{"id":2,"user":{"login":"q"},"body":"","state":"COMMENTED","html_url":"r2","submitted_at":"2026-11-05T12:05:00Z"},
		{"id":3,"user":{"login":"s"},"body":"","state":"COMMENTED","html_url":"r3","submitted_at":"2026-11-05T12:05:00Z"}]`
	got, err = normalizeComments([]byte(reviews), []byte(inline), []byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	var folded []string
	for _, c := range got {
		folded = append(folded, c.URL+" "+c.Excerpt)
	}
	check(t, folded, []string{"r1 overall fine (+1 inline)", "c11 first (+2 inline)", "c14 a reply"})
	if got, err := normalizeComments([]byte(`[]`), []byte(`[]`), []byte(`[]`)); err != nil || got == nil {
		t.Fatalf("empty must be non-nil: %v %v", got, err)
	}
	if _, err := normalizeComments([]byte(`[]`), []byte(`{}`), []byte(`[]`)); err == nil {
		t.Fatal("bad review comments accepted")
	}
	long := strings.Repeat("é", maxExcerpt+10)
	for in, want := range map[string]string{
		"> quoted\nreply\n\n  second  line": "reply second line",
		"> only a quote":                    "> only a quote",
		long:                                long[:2*maxExcerpt] + "…",
	} {
		if got := excerpt(in); got != want {
			t.Errorf("excerpt(%.20q) = %.40q", in, got)
		}
	}
}

func TestGitHubPRDecisions(t *testing.T) {
	pr := func(extra string) []byte {
		return []byte(`{"number":1,"state":"closed","merged":false,"head":{"sha":"s"},"requested_reviewers":[]` + extra + `}`)
	}
	for _, tt := range []struct {
		name                       string
		pull, reviews, runs, stats string
		state, review, ci          string
	}{
		{"merged, no reviews, no ci", `,"merged":true`, `[]`, `{"check_runs":[]}`, `{"state":"pending","total_count":0}`, "merged", "none", "none"},
		{"closed, changes requested wins", ``, `[{"user":{"login":"a"},"state":"APPROVED"},{"user":{"login":"b"},"state":"CHANGES_REQUESTED"}]`,
			`{"check_runs":[{"status":"completed","conclusion":"timed_out"}]}`, `{"state":"success","total_count":1}`, "closed", "changes_requested", "failure"},
		{"dismissed review clears it", ``, `[{"user":{"login":"a"},"state":"APPROVED"},{"user":{"login":"a"},"state":"DISMISSED"}]`,
			`{"check_runs":[]}`, `{"state":"failure","total_count":2}`, "closed", "none", "failure"},
		{"status pending only", ``, `[]`, `{"check_runs":[]}`, `{"state":"pending","total_count":1}`, "closed", "none", "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := pr("")
			if tt.pull != "" {
				raw = pr(tt.pull)
			}
			s, err := normalizeGitHubPR(raw, []byte(tt.reviews), []byte(tt.runs), []byte(tt.stats))
			if err != nil {
				t.Fatal(err)
			}
			if s.State != tt.state || s.ReviewDecision != tt.review || s.CI != tt.ci {
				t.Fatalf("got %s/%s/%s", s.State, s.ReviewDecision, s.CI)
			}
		})
	}
	if _, err := normalizeGitHubPR([]byte(`[]`), nil, nil, nil); err == nil {
		t.Fatal("bad PR body accepted")
	}
	// Reopening is a state change too.
	check(t, summary(t, diffPR(PRState{State: "closed"}, PRState{State: "open"})), []string{
		`pr.state_changed "closed">"open" {"from":"closed","number":0,"title":"","to":"open","url":""}`,
	})
}

// 001-AC7: gitlab_release.
func TestGitLabReleaseFixtures(t *testing.T) {
	a, err := normalizeGitLabReleases(testupstream.Fixture("gitlab_release", "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := normalizeGitLabReleases(testupstream.Fixture("gitlab_release", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tags(a.Releases); !slices.Equal(got, []string{"v19.3.4", "v19.3.3"}) || a.Releases[0].URL != "https://gitlab.com/gitlab-org/gitlab-runner/-/releases/v19.3.4" {
		t.Fatalf("state A: %+v", a)
	}
	check(t, summary(t, diffReleases(a, b)), []string{
		`release.published v19.3.5 {"name":"v19.3.5","prerelease":false,"tag":"v19.3.5","url":"https://gitlab.com/gitlab-org/gitlab-runner/-/releases/v19.3.5"}`,
		`release.published v19.3.6 {"name":"v19.3.6","prerelease":false,"tag":"v19.3.6","url":"https://gitlab.com/gitlab-org/gitlab-runner/-/releases/v19.3.6"}`,
		`release.published v19.3.7 {"name":"v19.3.7","prerelease":false,"tag":"v19.3.7","url":"https://gitlab.com/gitlab-org/gitlab-runner/-/releases/v19.3.7"}`,
		`release.published v19.3.8 {"name":"v19.3.8","prerelease":false,"tag":"v19.3.8","url":"https://gitlab.com/gitlab-org/gitlab-runner/-/releases/v19.3.8"}`,
		`release.published v19.4.0 {"name":"v19.4.0","prerelease":true,"tag":"v19.4.0","url":"https://gitlab.com/gitlab-org/gitlab-runner/-/releases/v19.4.0"}`,
	})
	if _, err := normalizeGitLabReleases([]byte(`{}`)); err == nil {
		t.Fatal("object accepted")
	}
}

// 001-AC7: gitlab_mr.
func TestGitLabMRFixtures(t *testing.T) {
	norm := func(set string) MRState {
		s, err := normalizeGitLabMR(testupstream.Part("gitlab_mr", set, "merge_request"), testupstream.Part("gitlab_mr", set, "approvals"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := norm("a"), norm("b")
	want := MRState{IID: 7764, State: "opened", Title: "fix: return context cancellation when step-runner readiness closes",
		Pipeline: "running", SHA: "e4187f8317d622c71404c398b1aaab78e2a13a0e", URL: "https://gitlab.com/gitlab-org/gitlab-runner/-/merge_requests/7764"}
	if a != want {
		t.Fatalf("state A:\n got  %+v\n want %+v", a, want)
	}
	const base, url = `"iid":7764,"title":"fix: return context cancellation when step-runner readiness closes"`, `"url":"https://gitlab.com/gitlab-org/gitlab-runner/-/merge_requests/7764"`
	check(t, summary(t, diffMR(a, b)), []string{
		`mr.state_changed "opened">"merged" {"from":"opened",` + base + `,"to":"merged",` + url + `}`,
		`mr.approval_changed {"approved":false,"count":0}>{"approved":true,"count":1} {"from":{"approved":false,"count":0},` + base + `,"to":{"approved":true,"count":1},` + url + `}`,
		`mr.pipeline_changed "running">"success" {"from":"running",` + base + `,"to":"success",` + url + `}`,
	})
	none, err := normalizeGitLabMR([]byte(`{"iid":1,"state":"opened","work_in_progress":true,"head_pipeline":null}`), []byte(`{}`))
	if err != nil || none.Pipeline != "none" || !none.Draft {
		t.Fatalf("no pipeline: %+v %v", none, err)
	}
	for _, bad := range [][2]string{{`[]`, `{}`}, {`{}`, `[]`}} {
		if _, err := normalizeGitLabMR([]byte(bad[0]), []byte(bad[1])); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

// 001-AC7, 001-AC12: oci_tags with tag_regex + semver ordering, and unfiltered.
func TestOCITagsFixtures(t *testing.T) {
	a, b := flatten(testupstream.TagPages("a")), flatten(testupstream.TagPages("b"))
	t.Run("regex and semver", func(t *testing.T) {
		sa, err := normalizeTags(a, `^\d+(\.\d+)?$`, true)
		if err != nil {
			t.Fatal(err)
		}
		sb, _ := normalizeTags(b, `^\d+(\.\d+)?$`, true)
		if want := (TagState{Tags: []string{"10", "17", "17.5", "18.0"}, Count: 4, Highest: "18.0"}); !reflect.DeepEqual(sa, want) {
			t.Fatalf("state A: %+v", sa)
		}
		check(t, summary(t, diffTags(sa, sb)), []string{
			`tag.created 17.6 {"highest":false,"tag":"17.6"}`,
			`tag.created 18.1 {"highest":true,"tag":"18.1"}`,
		})
	})
	t.Run("all tags", func(t *testing.T) {
		sa, _ := normalizeTags(a, "", false)
		sb, _ := normalizeTags(b, "", false)
		if sa.Count != 12 || sa.Tags[0] != "10" || sa.Tags[11] != "latest" || sa.Highest != "18.0" {
			t.Fatalf("state A: %+v", sa)
		}
		check(t, summary(t, diffTags(sa, sb)), []string{
			`tag.created 17.6 {"highest":false,"tag":"17.6"}`,
			`tag.created 17.6-alpine {"highest":false,"tag":"17.6-alpine"}`,
			`tag.created 18.1 {"highest":true,"tag":"18.1"}`,
			`tag.created 18.1-alpine {"highest":false,"tag":"18.1-alpine"}`,
			`tag.created 18.1-trixie {"highest":false,"tag":"18.1-trixie"}`,
			`tag.removed 10-alpine3.13 {"tag":"10-alpine3.13"}`,
		})
	})
	if _, err := normalizeTags(nil, "(", false); err == nil {
		t.Fatal("bad regex accepted")
	}
}

func TestSemver(t *testing.T) {
	ordered := []string{"0.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1", "v1.0.1", "1.2", "1.10.0", "18.1-alpine", "18.1"}
	shuffled := slices.Clone(ordered)
	slices.Reverse(shuffled)
	slices.SortFunc(shuffled, compareTags)
	if !slices.Equal(shuffled, ordered) {
		t.Fatalf("order: %v", shuffled)
	}
	for _, s := range []string{"latest", "1.2.3.4", "v", "1..2", "99999999999999999999999"} {
		if _, ok := parseSemver(s); ok {
			t.Errorf("%q parsed as semver", s)
		}
	}
	if compareTags("18", "v18") >= 0 || compareTags("1.0.0-x", "1.0.0-x") != 0 {
		t.Fatal("tie break")
	}
}

func TestAdapter(t *testing.T) {
	c := Collectors()["github_repo"]
	if _, err := c.Diff([]byte(`{`), []byte(`{}`)); err == nil {
		t.Fatal("bad old state accepted")
	}
	if _, err := c.Diff([]byte(`{}`), []byte(`{`)); err == nil {
		t.Fatal("bad new state accepted")
	}
	if _, err := c.Collect(t.Context(), &Fetcher{}, []byte(`[]`), nil); err == nil {
		t.Fatal("bad config accepted")
	}
	if s := (&Credential{Kind: "github_token", Secret: "ghp_secret"}).String(); s != "credential(github_token, redacted)" {
		t.Fatal(s)
	}
	if len(Collectors()) != len(watch.Kinds) {
		t.Fatal("one collector per kind")
	}
}

// 008-AC9: github_issue normalizes the issue and its comments; a→b closes it as not planned, adds a
// label and a comment.
func TestGitHubIssueFixtures(t *testing.T) {
	norm := func(set string) IssueState {
		s, err := normalizeGitHubIssue(testupstream.Part("github_issue", set, "issue"))
		if err != nil {
			t.Fatal(err)
		}
		if s.Comments, err = normalizeComments([]byte(`[]`), []byte(`[]`), testupstream.Part("github_issue", set, "comments")); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := norm("a"), norm("b")
	const title = "`gh run watch` exits 0 when the run is cancelled"
	want := IssueState{Number: 9400, State: "open", Title: title, Labels: []string{"bug"}, URL: "https://github.com/cli/cli/issues/9400",
		Comments: []PRComment{{Author: "renata-q", CreatedAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
			URL: "https://github.com/cli/cli/issues/9400#issuecomment-3600001", Excerpt: "Same on 2.62.0, Linux."}}}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("state A:\n got  %+v\n want %+v", a, want)
	}
	if b.StateReason != "not_planned" || !slices.Equal(b.Labels, []string{"bug", "wontfix"}) {
		t.Fatalf("state B: %+v", b)
	}
	// 011: the discussion-only reply_to key stays out of stored issue (and PR) states.
	if j, err := json.Marshal(b); err != nil || strings.Contains(string(j), "reply_to") {
		t.Fatalf("issue state JSON %s, %v", j, err)
	}
	const base = `"number":9400,"title":"` + title + `"`
	const url = `"url":"https://github.com/cli/cli/issues/9400"`
	check(t, summary(t, diffIssue(a, b)), []string{
		`issue.state_changed "open">"closed" {"from":"open",` + `"number":9400,"state_reason":"not_planned","title":"` + title + `","to":"closed",` + url + `}`,
		`issue.labels_changed ["bug"]>["bug","wontfix"] {"from":["bug"],` + base + `,"to":["bug","wontfix"],` + url + `}`,
		`issue.commented https://github.com/cli/cli/issues/9400#issuecomment-3600002 {"author":"maintainer","excerpt":"This is by design: a cancelled run is not a failure. Closing.",` +
			base + `,"url":"https://github.com/cli/cli/issues/9400#issuecomment-3600002"}`,
	})
}

// 008-AC9: each facet alone, the reopen, and the baselines.
func TestDiffIssue(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	c := func(n int) PRComment {
		return PRComment{Author: "alice", CreatedAt: at.Add(time.Duration(n) * time.Minute), URL: fmt.Sprintf("u%d", n), Excerpt: "hi"}
	}
	open := IssueState{Number: 12, State: "open", Title: "T", Labels: []string{"bug"}, URL: "x", Comments: []PRComment{c(1)}}
	with := func(f func(*IssueState)) IssueState {
		s := open
		s.Labels = slices.Clone(open.Labels)
		s.Comments = slices.Clone(open.Comments)
		f(&s)
		return s
	}
	closed := with(func(s *IssueState) { s.State, s.StateReason = "closed", "completed" })
	for _, tt := range []struct {
		name     string
		old, cur IssueState
		want     []string
	}{
		{"unchanged", open, open, []string{}},
		{"closed completed", open, closed, []string{`issue.state_changed "open">"closed" {"from":"open","number":12,"state_reason":"completed","title":"T","to":"closed","url":"x"}`}},
		{"closed no reason", open, with(func(s *IssueState) { s.State = "closed" }),
			[]string{`issue.state_changed "open">"closed" {"from":"open","number":12,"state_reason":"","title":"T","to":"closed","url":"x"}`}},
		{"reopened", closed, with(func(s *IssueState) { s.StateReason = "reopened" }),
			[]string{`issue.state_changed "closed">"open" {"from":"closed","number":12,"state_reason":"reopened","title":"T","to":"open","url":"x"}`}},
		{"reason change while closed", closed, with(func(s *IssueState) { s.State, s.StateReason = "closed", "duplicate" }),
			[]string{`issue.state_changed "closed:completed">"closed:duplicate" {"from":"closed","number":12,"state_reason":"duplicate","title":"T","to":"closed","url":"x"}`}},
		{"reason change and label", closed, with(func(s *IssueState) { s.State, s.StateReason, s.Labels = "closed", "not_planned", []string{} }), []string{
			`issue.state_changed "closed:completed">"closed:not_planned" {"from":"closed","number":12,"state_reason":"not_planned","title":"T","to":"closed","url":"x"}`,
			`issue.labels_changed ["bug"]>[] {"from":["bug"],"number":12,"title":"T","to":[],"url":"x"}`}},
		{"reason set on an open issue is no event", open, with(func(s *IssueState) { s.StateReason = "reopened" }), []string{}},
		{"label added", open, with(func(s *IssueState) { s.Labels = []string{"bug", "triage"} }),
			[]string{`issue.labels_changed ["bug"]>["bug","triage"] {"from":["bug"],"number":12,"title":"T","to":["bug","triage"],"url":"x"}`}},
		{"label removed", open, with(func(s *IssueState) { s.Labels = []string{} }),
			[]string{`issue.labels_changed ["bug"]>[] {"from":["bug"],"number":12,"title":"T","to":[],"url":"x"}`}},
		{"new comments", open, with(func(s *IssueState) { s.Comments = []PRComment{c(1), c(2), c(3)} }), []string{
			`issue.commented u2 {"author":"alice","excerpt":"hi","number":12,"title":"T","url":"u2"}`,
			`issue.commented u3 {"author":"alice","excerpt":"hi","number":12,"title":"T","url":"u3"}`}},
		{"comment slid back is old", with(func(s *IssueState) { s.Comments = []PRComment{c(5)} }), with(func(s *IssueState) { s.Comments = []PRComment{c(4), c(5)} }), []string{}},
		{"no comments stored is a baseline", with(func(s *IssueState) { s.Comments = nil }), with(func(s *IssueState) { s.Comments = []PRComment{c(1), c(2)} }), []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check(t, summary(t, diffIssue(tt.old, tt.cur)), tt.want)
		})
	}
	if _, err := normalizeGitHubIssue([]byte(`{"message":"Not Found"}`)); err == nil {
		t.Fatal("issue without state accepted")
	}
}

// 011-AC7: github_discussion normalizes the discussion and its flat comments, a reply pointing at its parent;
// a→b closes it as resolved, picks an answer and adds a reply.
func TestGitHubDiscussionFixtures(t *testing.T) {
	norm := func(set string) DiscussionState {
		s, err := normalizeGitHubDiscussion(testupstream.Part("github_discussion", set, "discussion"))
		if err != nil {
			t.Fatal(err)
		}
		if s.Comments, err = normalizeDiscussionComments(s.URL, testupstream.Part("github_discussion", set, "comments")); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := norm("a"), norm("b")
	const title = "How do I set a default repo per directory?"
	const u = "https://github.com/cli/cli/discussions/9507"
	want := DiscussionState{Number: 9507, State: "open", Title: title, Category: "Q&A", Labels: []string{}, URL: u,
		Comments: []PRComment{
			{Author: "maintainer", CreatedAt: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC), URL: u + "#discussioncomment-4100001",
				Excerpt: "Set `GH_REPO` in a direnv `.envrc` per directory."},
			{Author: "kenji-w", CreatedAt: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC), URL: u + "#discussioncomment-4100002",
				Excerpt: "Does it work with `gh pr create`?", ReplyTo: u + "#discussioncomment-4100001"},
		}}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("state A:\n got  %+v\n want %+v", a, want)
	}
	if b.StateReason != "resolved" || b.AnswerURL != u+"#discussioncomment-4100001" || b.AnswerChosenBy != "kenji-w" {
		t.Fatalf("state B: %+v", b)
	}
	const base = `"number":9507,"title":"` + title + `"`
	check(t, summary(t, diffDiscussion(a, b)), []string{
		`discussion.state_changed "open">"closed" {"from":"open","number":9507,"state_reason":"resolved","title":"` + title + `","to":"closed","url":"` + u + `"}`,
		`discussion.answer_changed "">"` + u + `#discussioncomment-4100001" {"chosen_by":"kenji-w","from":"",` + base + `,"to":"` + u + `#discussioncomment-4100001","url":"` + u + `"}`,
		`discussion.commented ` + u + `#discussioncomment-4100003 {"author":"maintainer","excerpt":"Yes: every ` + "`gh`" + ` command reads ` + "`GH_REPO`" + `.",` +
			`"number":9507,"reply_to":"` + u + `#discussioncomment-4100001","title":"` + title + `","url":"` + u + `#discussioncomment-4100003"}`,
	})
}

// 011-AC7: each facet alone, the reopen, a reason change while closed, the answer chosen and removed, a reply, and the baselines.
func TestDiffDiscussion(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	c := func(n int) PRComment {
		return PRComment{Author: "alice", CreatedAt: at.Add(time.Duration(n) * time.Minute), URL: fmt.Sprintf("u%d", n), Excerpt: "hi"}
	}
	reply := c(2)
	reply.ReplyTo = "u1"
	open := DiscussionState{Number: 12, State: "open", Title: "T", Category: "Q&A", Labels: []string{"bug"}, URL: "x", Comments: []PRComment{c(1)}}
	with := func(f func(*DiscussionState)) DiscussionState {
		s := open
		s.Labels = slices.Clone(open.Labels)
		s.Comments = slices.Clone(open.Comments)
		f(&s)
		return s
	}
	closed := with(func(s *DiscussionState) { s.State, s.StateReason = "closed", "resolved" })
	answered := with(func(s *DiscussionState) { s.AnswerURL, s.AnswerChosenBy = "x#discussioncomment-1", "bob" })
	for _, tt := range []struct {
		name     string
		old, cur DiscussionState
		want     []string
	}{
		{"unchanged", open, open, []string{}},
		{"closed resolved", open, closed, []string{`discussion.state_changed "open">"closed" {"from":"open","number":12,"state_reason":"resolved","title":"T","to":"closed","url":"x"}`}},
		{"reopened", closed, with(func(s *DiscussionState) { s.StateReason = "reopened" }),
			[]string{`discussion.state_changed "closed">"open" {"from":"closed","number":12,"state_reason":"reopened","title":"T","to":"open","url":"x"}`}},
		{"reason change while closed", closed, with(func(s *DiscussionState) { s.State, s.StateReason = "closed", "outdated" }),
			[]string{`discussion.state_changed "closed:resolved">"closed:outdated" {"from":"closed","number":12,"state_reason":"outdated","title":"T","to":"closed","url":"x"}`}},
		{"answer chosen", open, answered,
			[]string{`discussion.answer_changed "">"x#discussioncomment-1" {"chosen_by":"bob","from":"","number":12,"title":"T","to":"x#discussioncomment-1","url":"x"}`}},
		{"answer removed", answered, open,
			[]string{`discussion.answer_changed "x#discussioncomment-1">"" {"chosen_by":"","from":"x#discussioncomment-1","number":12,"title":"T","to":"","url":"x"}`}},
		{"label added", open, with(func(s *DiscussionState) { s.Labels = []string{"bug", "triage"} }),
			[]string{`discussion.labels_changed ["bug"]>["bug","triage"] {"from":["bug"],"number":12,"title":"T","to":["bug","triage"],"url":"x"}`}},
		{"category change is no event", open, with(func(s *DiscussionState) { s.Category = "Ideas" }), []string{}},
		{"comment and reply", open, with(func(s *DiscussionState) { s.Comments = []PRComment{c(1), reply, c(3)} }), []string{
			`discussion.commented u2 {"author":"alice","excerpt":"hi","number":12,"reply_to":"u1","title":"T","url":"u2"}`,
			`discussion.commented u3 {"author":"alice","excerpt":"hi","number":12,"title":"T","url":"u3"}`}},
		{"no comments stored is a baseline", with(func(s *DiscussionState) { s.Comments = nil }), with(func(s *DiscussionState) { s.Comments = []PRComment{c(1), reply} }), []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check(t, summary(t, diffDiscussion(tt.old, tt.cur)), tt.want)
		})
	}
}
