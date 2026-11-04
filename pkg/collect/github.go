package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gedour/gedour/pkg/watch"
)

// Release is one release of a GitHub repository or GitLab project.
type Release struct {
	Tag         string     `json:"tag"`
	Name        string     `json:"name"`
	Prerelease  bool       `json:"prerelease"`
	Draft       bool       `json:"draft"`
	PublishedAt *time.Time `json:"published_at"`
	URL         string     `json:"url"`
}

// ReleaseState is the normalized state of a release watch, newest first.
type ReleaseState struct {
	Releases []Release `json:"releases"`
}

func githubHeaders(cred *Credential) http.Header {
	h := http.Header{"Accept": {"application/vnd.github+json"}, "X-Github-Api-Version": {"2022-11-28"}}
	if cred != nil {
		h.Set("Authorization", "Bearer "+cred.Secret)
	}
	return h
}

func collectGitHubReleases(ctx context.Context, f *Fetcher, cfg watch.GitHubReleaseConfig, cred *Credential) (ReleaseState, error) {
	// The first page is enough: the newest 50 cover any realistic gap between two runs.
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=50", cfg.APIURL, cfg.Repo)
	r, err := f.get(ctx, u, githubHeaders(cred), true)
	if err != nil {
		return ReleaseState{}, err
	}
	return normalizeGitHubReleases(r.body, cfg.IncludePrereleases)
}

func normalizeGitHubReleases(body []byte, includePrereleases bool) (ReleaseState, error) {
	var raw []struct {
		TagName     string     `json:"tag_name"`
		Name        string     `json:"name"`
		Prerelease  bool       `json:"prerelease"`
		Draft       bool       `json:"draft"`
		PublishedAt *time.Time `json:"published_at"`
		HTMLURL     string     `json:"html_url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ReleaseState{}, fmt.Errorf("decode releases: %w", err)
	}
	var s ReleaseState
	for _, r := range raw {
		if r.TagName == "" || (r.Prerelease && !includePrereleases) {
			continue
		}
		s.Releases = append(s.Releases, Release{Tag: r.TagName, Name: r.Name, Prerelease: r.Prerelease, Draft: r.Draft,
			PublishedAt: r.PublishedAt, URL: r.HTMLURL})
	}
	sortReleases(s.Releases)
	return s, nil
}

// sortReleases orders newest first; drafts (no date) lead, the tag breaks ties so the state hash is stable.
func sortReleases(rs []Release) {
	slices.SortFunc(rs, func(a, b Release) int {
		at, bt := time.Time{}, time.Time{}
		if a.PublishedAt != nil {
			at = *a.PublishedAt
		}
		if b.PublishedAt != nil {
			bt = *b.PublishedAt
		}
		if c := bt.Compare(at); c != 0 {
			if a.PublishedAt == nil {
				return -1
			}
			if b.PublishedAt == nil {
				return 1
			}
			return c
		}
		return strings.Compare(a.Tag, b.Tag)
	})
}

// diffReleases: a tag seen published for the first time (new, or a draft no more) is
// release.published; a prerelease turned release is release.promoted.
func diffReleases(old, cur ReleaseState) []Event {
	before := map[string]Release{}
	for _, r := range old.Releases {
		before[r.Tag] = r
	}
	var events []Event
	// Oldest first, so the timeline reads in release order.
	for _, r := range slices.Backward(cur.Releases) {
		if r.Draft {
			continue
		}
		payload := map[string]any{"tag": r.Tag, "name": r.Name, "prerelease": r.Prerelease, "url": r.URL}
		prev, seen := before[r.Tag]
		switch {
		case !seen || prev.Draft:
			events = append(events, Event{Type: "release.published", Subject: r.Tag, OccurredAt: r.PublishedAt, Payload: payload})
		case prev.Prerelease && !r.Prerelease:
			events = append(events, Event{Type: "release.promoted", Subject: r.Tag, OccurredAt: r.PublishedAt, Payload: payload})
		}
	}
	return events
}

// PRState is the normalized state of one GitHub pull request.
type PRState struct {
	Number         int    `json:"number"`
	State          string `json:"state"` // open, closed or merged
	Draft          bool   `json:"draft"`
	Title          string `json:"title"`
	ReviewDecision string `json:"review_decision"` // approved, changes_requested, review_required or none
	CI             string `json:"ci"`              // success, failure, pending or none
	HeadSHA        string `json:"head_sha"`
	URL            string `json:"url"`
}

func collectGitHubPR(ctx context.Context, f *Fetcher, cfg watch.GitHubPRConfig, cred *Credential) (PRState, error) {
	h := githubHeaders(cred)
	base := fmt.Sprintf("%s/repos/%s", cfg.APIURL, cfg.Repo)
	pr, err := f.get(ctx, fmt.Sprintf("%s/pulls/%d", base, cfg.Number), h, false)
	if err != nil {
		return PRState{}, err
	}
	var head struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(pr.body, &head); err != nil || head.Head.SHA == "" {
		return PRState{}, fmt.Errorf("decode pull request: no head sha (%v)", err)
	}
	var bodies [3][]byte
	for i, u := range []string{
		// ponytail: first page only; a PR with more than 100 reviews or check runs reads its first 100.
		fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, cfg.Number),
		fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", base, head.Head.SHA),
		fmt.Sprintf("%s/commits/%s/status?per_page=100", base, head.Head.SHA),
	} {
		r, err := f.get(ctx, u, h, false)
		if err != nil {
			return PRState{}, err
		}
		bodies[i] = r.body
	}
	return normalizeGitHubPR(pr.body, bodies[0], bodies[1], bodies[2])
}

func normalizeGitHubPR(prBody, reviewsBody, checksBody, statusBody []byte) (PRState, error) {
	var pr struct {
		Number             int     `json:"number"`
		State              string  `json:"state"`
		Draft              bool    `json:"draft"`
		Merged             bool    `json:"merged"`
		MergedAt           *string `json:"merged_at"`
		Title              string  `json:"title"`
		HTMLURL            string  `json:"html_url"`
		RequestedReviewers []any   `json:"requested_reviewers"`
		RequestedTeams     []any   `json:"requested_teams"`
		Head               struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	var reviews []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State string `json:"state"`
	}
	var checks struct {
		CheckRuns []struct {
			Status     string  `json:"status"`
			Conclusion *string `json:"conclusion"`
		} `json:"check_runs"`
	}
	var status struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	for _, d := range []struct {
		name string
		b    []byte
		v    any
	}{{"pull request", prBody, &pr}, {"reviews", reviewsBody, &reviews}, {"check runs", checksBody, &checks}, {"status", statusBody, &status}} {
		if err := json.Unmarshal(d.b, d.v); err != nil {
			return PRState{}, fmt.Errorf("decode %s: %w", d.name, err)
		}
	}
	s := PRState{Number: pr.Number, State: pr.State, Draft: pr.Draft, Title: pr.Title, HeadSHA: pr.Head.SHA, URL: pr.HTMLURL}
	if pr.Merged || pr.MergedAt != nil {
		s.State = "merged"
	}

	// GitHub's rule: each reviewer's latest approving or blocking review counts; comments do not override it.
	latest := map[string]string{}
	for _, r := range reviews {
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[r.User.Login] = r.State
		case "DISMISSED":
			delete(latest, r.User.Login)
		}
	}
	s.ReviewDecision = "none"
	switch {
	case slices.Contains(mapValues(latest), "CHANGES_REQUESTED"):
		s.ReviewDecision = "changes_requested"
	case len(latest) > 0:
		s.ReviewDecision = "approved"
	case len(pr.RequestedReviewers)+len(pr.RequestedTeams) > 0:
		s.ReviewDecision = "review_required"
	}

	failed, pending := false, false
	for _, c := range checks.CheckRuns {
		switch {
		case c.Status != "completed":
			pending = true
		case c.Conclusion != nil && slices.Contains([]string{"failure", "timed_out", "cancelled", "action_required", "startup_failure"}, *c.Conclusion):
			failed = true
		}
	}
	// The combined status reads "pending" when no status exists at all: only count it when there are some.
	if status.TotalCount > 0 {
		switch status.State {
		case "failure", "error":
			failed = true
		case "pending":
			pending = true
		}
	}
	switch {
	case failed:
		s.CI = "failure"
	case pending:
		s.CI = "pending"
	case len(checks.CheckRuns) > 0 || status.TotalCount > 0:
		s.CI = "success"
	default:
		s.CI = "none"
	}
	return s, nil
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// diffPR emits one event per changed facet: state, draft, review decision, CI.
func diffPR(old, cur PRState) []Event {
	base := func() map[string]any {
		return map[string]any{"number": cur.Number, "title": cur.Title, "url": cur.URL}
	}
	return diffFacets("pr", base, []facet{
		{"state_changed", old.State, cur.State},
		{"draft_changed", old.Draft, cur.Draft},
		{"review_changed", old.ReviewDecision, cur.ReviewDecision},
		{"ci_changed", old.CI, cur.CI},
	})
}

type facet struct {
	event    string
	from, to any
}

// diffFacets turns each changed facet into "<prefix>.<event>" with from/to on top of base's payload.
func diffFacets(prefix string, base func() map[string]any, facets []facet) []Event {
	var events []Event
	for _, f := range facets {
		fromJSON, _ := json.Marshal(f.from)
		toJSON, _ := json.Marshal(f.to)
		if string(fromJSON) == string(toJSON) {
			continue
		}
		p := base()
		p["from"], p["to"] = f.from, f.to
		events = append(events, Event{Type: prefix + "." + f.event, Subject: string(fromJSON) + ">" + string(toJSON), Payload: p})
	}
	return events
}
