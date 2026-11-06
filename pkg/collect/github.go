package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
	// Notes by tag: out of the JSON, so an edit of the notes alone changes neither the stored state nor its hash.
	Notes map[string]string `json:"-"`
}

const (
	// MaxNotes caps payload.body of a release event.
	MaxNotes = 64 << 10
	maxCVEs  = 200
	// maxCVELen drops longer matches: no real id is that long, and it would escape every size bound.
	maxCVELen = 32
)

var cveID = regexp.MustCompile(`(?i)\bCVE-[0-9]{4}-[0-9]+\b`)

// parseNotes turns raw release notes into payload.body and payload.cves; the ids come from the whole
// notes, before the cut. cves is never nil, so a filter always sees a list.
func parseNotes(notes string) (body string, cves []string) {
	// jsonb refuses NUL, and the notes do not go through the state's StripNUL.
	notes = strings.ReplaceAll(notes, "\x00", "")
	cves = []string{}
	seen := map[string]bool{}
	for rest := notes; len(cves) < maxCVEs; {
		loc := cveID.FindStringIndex(rest)
		if loc == nil {
			break
		}
		m := rest[loc[0]:loc[1]]
		// A match ends on a word boundary, so the text left starts outside a word: \b still reads right there.
		rest = rest[loc[1]:]
		if id := strings.ToUpper(m); len(m) <= maxCVELen && !seen[id] {
			seen[id] = true
			cves = append(cves, id)
		}
	}
	if len(notes) > MaxNotes {
		notes = strings.ToValidUTF8(notes[:MaxNotes], "")
	}
	return notes, cves
}

// WithNotes adds body and cves to the events of a release watch: Diff reads stored states, which hold no
// notes. A nil map means another kind of watch.
func WithNotes(events []Event, notes map[string]string) {
	if notes == nil {
		return
	}
	for _, e := range events {
		e.Payload["body"], e.Payload["cves"] = parseNotes(notes[e.Subject])
	}
}

func githubHeaders(cred *Credential) http.Header {
	h := http.Header{"Accept": {"application/vnd.github+json"}, "X-Github-Api-Version": {"2022-11-28"}}
	if cred != nil {
		h.Set("Authorization", "Bearer "+cred.Secret)
	}
	return h
}

func collectGitHubReleases(ctx context.Context, f *Fetcher, cfg watch.GitHubRepoConfig, cred *Credential) (ReleaseState, error) {
	// The first page is enough: the newest 50 cover any realistic gap between two runs.
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=50", cfg.APIURL, cfg.Repo)
	r, err := f.get(ctx, u, githubHeaders(cred), true)
	if err != nil {
		return ReleaseState{}, err
	}
	s, err := normalizeGitHubReleases(r.body, cfg.IncludePrereleases)
	f.Notes = s.Notes
	return s, err
}

func normalizeGitHubReleases(body []byte, includePrereleases bool) (ReleaseState, error) {
	var raw []struct {
		TagName     string     `json:"tag_name"`
		Name        string     `json:"name"`
		Prerelease  bool       `json:"prerelease"`
		Draft       bool       `json:"draft"`
		PublishedAt *time.Time `json:"published_at"`
		HTMLURL     string     `json:"html_url"`
		Body        string     `json:"body"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ReleaseState{}, fmt.Errorf("decode releases: %w", err)
	}
	s := ReleaseState{Notes: map[string]string{}}
	for _, r := range raw {
		if r.TagName == "" || (r.Prerelease && !includePrereleases) {
			continue
		}
		s.Releases = append(s.Releases, Release{Tag: r.TagName, Name: r.Name, Prerelease: r.Prerelease, Draft: r.Draft,
			PublishedAt: r.PublishedAt, URL: r.HTMLURL})
		s.Notes[r.TagName] = r.Body
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
	// Comments are the newest maxComments, oldest first. nil is a state stored before comments were
	// collected: the diff then takes the current ones as its baseline instead of announcing them.
	Comments []PRComment `json:"comments"`
}

// PRComment is one comment on a PR: conversation, inline review comment, or review body.
type PRComment struct {
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	// URL identifies the comment: the three kinds number their ids apart.
	URL     string `json:"url"`
	Excerpt string `json:"excerpt"`
}

const (
	maxComments = 20
	maxExcerpt  = 280
)

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
	var bodies [4][]byte
	for i, u := range []string{
		// ponytail: first page only; a PR with more than 100 reviews or check runs reads its first 100.
		fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, cfg.Number),
		fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", base, head.Head.SHA),
		fmt.Sprintf("%s/commits/%s/status?per_page=100", base, head.Head.SHA),
		// This endpoint sorts newest first; the issue comments one has no sort option.
		fmt.Sprintf("%s/pulls/%d/comments?per_page=100&sort=created&direction=desc", base, cfg.Number),
	} {
		r, err := f.get(ctx, u, h, false)
		if err != nil {
			return PRState{}, err
		}
		bodies[i] = r.body
	}
	issue, err := lastPage(ctx, f, fmt.Sprintf("%s/issues/%d/comments?per_page=100", base, cfg.Number), h)
	if err != nil {
		return PRState{}, err
	}
	s, err := normalizeGitHubPR(pr.body, bodies[0], bodies[1], bodies[2])
	if err != nil {
		return PRState{}, err
	}
	if s.Comments, err = normalizeComments(bodies[0], bodies[3], issue); err != nil {
		return PRState{}, err
	}
	return s, nil
}

// lastPage reads the Link rel="last" page of an oldest-first list, where its newest items sit.
// ponytail: last page only; more than 100 comments in one run interval loses the overflow.
func lastPage(ctx context.Context, f *Fetcher, u string, h http.Header) ([]byte, error) {
	r, err := f.get(ctx, u, h, false)
	if err != nil {
		return nil, err
	}
	last, err := relLink(u, r.header.Get("Link"), lastRe)
	if err != nil || last == "" {
		return r.body, err
	}
	if r, err = f.get(ctx, last, h, false); err != nil {
		return nil, err
	}
	return r.body, nil
}

// normalizeComments merges review bodies, inline comments and conversation comments into the newest
// maxComments, oldest first. The result is never nil, so the next diff knows comments were collected.
// One review is one comment: its inline comments fold into its body, or into the first of them.
func normalizeComments(reviewsBody, reviewCommentsBody, issueCommentsBody []byte) ([]PRComment, error) {
	type item struct {
		ID       int64 `json:"id"`
		ReviewID int64 `json:"pull_request_review_id"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		Body        string     `json:"body"`
		HTMLURL     string     `json:"html_url"`
		CreatedAt   *time.Time `json:"created_at"`
		SubmittedAt *time.Time `json:"submitted_at"`
	}
	var reviews, inline, issue []item
	for _, d := range []struct {
		name string
		b    []byte
		v    *[]item
	}{{"reviews", reviewsBody, &reviews}, {"review comments", reviewCommentsBody, &inline}, {"issue comments", issueCommentsBody, &issue}} {
		if err := json.Unmarshal(d.b, d.v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", d.name, err)
		}
	}
	out := []PRComment{}
	add := func(it item, at *time.Time) bool {
		// An approval without text, or a pending review, is not a comment.
		if at == nil || it.HTMLURL == "" || strings.TrimSpace(it.Body) == "" {
			return false
		}
		out = append(out, PRComment{Author: it.User.Login, CreatedAt: *at, URL: it.HTMLURL, Excerpt: excerpt(it.Body)})
		return true
	}
	carrier := map[int64]int{} // review id -> index in out of the comment that stands for the review
	for _, r := range reviews {
		if add(r, r.SubmittedAt) {
			carrier[r.ID] = len(out) - 1
		}
	}
	slices.SortStableFunc(inline, func(a, b item) int {
		if a.CreatedAt == nil || b.CreatedAt == nil {
			return 0 // dropped by add anyway
		}
		return a.CreatedAt.Compare(*b.CreatedAt)
	})
	more := map[int]int{}
	for _, c := range inline {
		if i, ok := carrier[c.ReviewID]; ok && c.ReviewID != 0 {
			more[i]++
			continue
		}
		if add(c, c.CreatedAt) && c.ReviewID != 0 {
			carrier[c.ReviewID] = len(out) - 1
		}
	}
	for i, n := range more {
		out[i].Excerpt += fmt.Sprintf(" (+%d inline)", n)
	}
	for _, c := range issue {
		add(c, c.CreatedAt)
	}
	slices.SortFunc(out, func(a, b PRComment) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.URL, b.URL)
	})
	return out[max(len(out)-maxComments, 0):], nil
}

// excerpt is the comment on one line, quoted lines (a reply's "> ...") left out, cut at maxExcerpt runes.
func excerpt(body string) string {
	var lines []string
	for l := range strings.Lines(body) {
		if !strings.HasPrefix(strings.TrimSpace(l), ">") {
			lines = append(lines, l)
		}
	}
	s := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	if s == "" { // a comment that only quotes
		s = strings.Join(strings.Fields(body), " ")
	}
	if r := []rune(s); len(r) > maxExcerpt {
		s = string(r[:maxExcerpt]) + "…"
	}
	return s
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

// diffPR emits one event per changed facet (state, draft, review decision, CI), then one per new comment.
func diffPR(old, cur PRState) []Event {
	base := func() map[string]any {
		return map[string]any{"number": cur.Number, "title": cur.Title, "url": cur.URL}
	}
	events := diffFacets("pr", base, []facet{
		{"state_changed", old.State, cur.State},
		{"draft_changed", old.Draft, cur.Draft},
		{"review_changed", old.ReviewDecision, cur.ReviewDecision},
		{"ci_changed", old.CI, cur.CI},
	})
	for _, c := range newComments(old.Comments, cur.Comments) {
		at := c.CreatedAt
		events = append(events, Event{Type: "pr.commented", Subject: c.URL, OccurredAt: &at, Payload: map[string]any{
			"number": cur.Number, "title": cur.Title, "url": c.URL, "author": c.Author, "excerpt": c.Excerpt,
		}})
	}
	return events
}

// newComments lists the comments of cur missing from old. A comment older than old's oldest entry slid
// back into the window (a deletion, a shorter last page), it is not new.
func newComments(old, cur []PRComment) []PRComment {
	if old == nil {
		return nil
	}
	seen := make(map[string]bool, len(old))
	for _, c := range old {
		seen[c.URL] = true
	}
	var floor time.Time
	if len(old) > 0 {
		floor = old[0].CreatedAt
	}
	var out []PRComment
	for _, c := range cur {
		if !seen[c.URL] && !c.CreatedAt.Before(floor) {
			out = append(out, c)
		}
	}
	return out
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
