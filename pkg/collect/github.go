package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
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
	// Numbers of the issues, PRs and discussions listed while their box is on. One list per box, so checking a box
	// starts a baseline for its kind alone; nil (absent) is no list stored, and keeps a boxes-off state as before 008.
	Issues      *[]int `json:"issues,omitempty"`
	PRs         *[]int `json:"prs,omitempty"`
	Discussions *[]int `json:"discussions,omitempty"`
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
		// An item's subject is its number, which a tag may equal.
		if e.Type != "issue.opened" && e.Type != "pr.opened" && e.Type != "discussion.opened" {
			e.Payload["body"], e.Payload["cves"] = parseNotes(notes[e.Subject])
		}
	}
}

// itemListed and discussionListed are Diff's placeholders for a new issue or PR number and a new discussion
// number: the stored states hold numbers only.
const (
	itemListed       = "item.listed"
	discussionListed = "discussion.listed"
)

// WithItems turns Diff's placeholders into the run's issue.opened, pr.opened and discussion.opened events, and
// drops those whose box is off. Call it before WithNotes.
func WithItems(events []Event, items map[int]Event) []Event {
	out := events[:0]
	for _, e := range events {
		if e.Type == itemListed || e.Type == discussionListed {
			n, _ := strconv.Atoi(e.Subject)
			it, ok := items[n]
			// An issue converted to a discussion keeps its number: the run's item must come from the placeholder's list.
			if !ok || (it.Type == "discussion.opened") != (e.Type == discussionListed) {
				continue
			}
			e = it
		}
		out = append(out, e)
	}
	return out
}

func githubHeaders(cred *Credential) http.Header {
	h := http.Header{"Accept": {"application/vnd.github+json"}, "X-Github-Api-Version": {"2022-11-28"}}
	if cred != nil {
		h.Set("Authorization", "Bearer "+cred.Secret)
	}
	return h
}

func collectGitHubRepo(ctx context.Context, f *Fetcher, cfg watch.GitHubRepoConfig, cred *Credential) (ReleaseState, error) {
	h := githubHeaders(cred)
	items := cfg.NewIssues || cfg.NewPRs
	boxed := items || cfg.NewDiscussions
	// The first page is enough: the newest 50 cover any realistic gap between two runs.
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=50", cfg.APIURL, cfg.Repo)
	// A 304 ends the whole run with no state: with a box on it would skip the other lists, so all are read in full.
	r, err := f.get(ctx, u, h, !boxed)
	if err != nil {
		return ReleaseState{}, err
	}
	s, err := normalizeGitHubReleases(r.body, cfg.IncludePrereleases)
	f.Notes = s.Notes
	if err != nil || !boxed {
		return s, err
	}
	opened := map[int]Event{}
	if items {
		// The list holds PRs too, so one call serves both boxes.
		// ponytail: first page only; past 50 items opened between two runs, the oldest of them are never reported.
		u = fmt.Sprintf("%s/repos/%s/issues?state=all&sort=created&direction=desc&per_page=50", cfg.APIURL, cfg.Repo)
		if r, err = f.get(ctx, u, h, false); err != nil {
			return ReleaseState{}, err
		}
		issues, prs, listed, err := normalizeGitHubItems(r.body, cfg.NewIssues, cfg.NewPRs)
		if err != nil {
			return ReleaseState{}, err
		}
		if cfg.NewIssues {
			s.Issues = &issues
		}
		if cfg.NewPRs {
			s.PRs = &prs
		}
		maps.Copy(opened, listed)
	}
	if cfg.NewDiscussions {
		nums, listed, err := githubDiscussions(ctx, f, cfg, h)
		if err != nil {
			return ReleaseState{}, err
		}
		s.Discussions = &nums
		maps.Copy(opened, listed)
	}
	f.Items = opened
	return s, nil
}

// minDiscussions is the window size below which the page before the last is read too.
const minDiscussions = 50

// githubDiscussions reads the newest discussions. The list is oldest first and ignores sort, so a per_page=1 probe
// gives the total as its last page number; the window is the last per_page=100 page, and the page before it when
// the last holds fewer than minDiscussions.
// ponytail: a window of 50 at least; past that many opened between two runs, the oldest may go unreported.
func githubDiscussions(ctx context.Context, f *Fetcher, cfg watch.GitHubRepoConfig, h http.Header) ([]int, map[int]Event, error) {
	base := fmt.Sprintf("%s/repos/%s/discussions", cfg.APIURL, cfg.Repo)
	get := func(u string) (*response, error) {
		r, err := f.get(ctx, u, h, false)
		if he := (*HTTPError)(nil); errors.As(err, &he) && he.Status == http.StatusGone {
			return nil, fmt.Errorf("discussions are disabled on %s: uncheck \"new discussions\" (%w)", cfg.Repo, err)
		}
		return r, err
	}
	r, err := get(base + "?per_page=1")
	if err != nil {
		return nil, nil, err
	}
	pages := [][]byte{r.body}
	if link := r.header.Get("Link"); link != "" {
		total := lastPageNumber(link)
		if total < 1 {
			return nil, nil, fmt.Errorf("discussions list: no last page number in Link %q", snippet([]byte(link)))
		}
		// Built from the config, not the Link: the probe only tells the count.
		page := func(n int) ([]byte, error) {
			r, err := get(fmt.Sprintf("%s?per_page=100&page=%d", base, n))
			if err != nil {
				return nil, err
			}
			return r.body, nil
		}
		last := (total + 99) / 100
		b, err := page(last)
		if err != nil {
			return nil, nil, err
		}
		pages = [][]byte{b}
		var short []json.RawMessage
		if err := json.Unmarshal(b, &short); err != nil {
			return nil, nil, fmt.Errorf("decode discussions: %w", err)
		}
		if len(short) < minDiscussions && last > 1 {
			if b, err = page(last - 1); err != nil {
				return nil, nil, err
			}
			pages = [][]byte{b, pages[0]}
		}
	}
	nums, opened := []int{}, map[int]Event{}
	for _, b := range pages {
		if err := normalizeGitHubDiscussions(b, &nums, opened); err != nil {
			return nil, nil, err
		}
	}
	// Newest first like the issues list, and the same pages always store the same state.
	slices.Sort(nums)
	slices.Reverse(nums)
	return slices.Compact(nums), opened, nil
}

// lastPageNumber is the page query parameter of a Link header's rel="last" URL, 0 when absent or unreadable.
func lastPageNumber(link string) int {
	m := lastRe.FindStringSubmatch(link)
	if m == nil {
		return 0
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(u.Query().Get("page"))
	return n
}

// normalizeGitHubDiscussions appends a discussions page's numbers to nums and its opened events to opened, by number.
func normalizeGitHubDiscussions(body []byte, nums *[]int, opened map[int]Event) error {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Category struct {
			Name string `json:"name"`
		} `json:"category"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		HTMLURL   string     `json:"html_url"`
		CreatedAt *time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("decode discussions: %w", err)
	}
	for _, d := range raw {
		if d.Number <= 0 {
			continue
		}
		*nums = append(*nums, d.Number)
		labels := []string{}
		for _, l := range d.Labels {
			labels = append(labels, l.Name)
		}
		opened[d.Number] = Event{Type: "discussion.opened", Subject: strconv.Itoa(d.Number), OccurredAt: d.CreatedAt, Payload: map[string]any{
			"number": d.Number, "title": d.Title, "author": d.User.Login, "category": d.Category.Name, "labels": labels,
			"url": d.HTMLURL, "created_at": d.CreatedAt,
		}}
	}
	return nil
}

// normalizeGitHubItems reads an issues list: the numbers of its issues and of its PRs, never nil, and the
// opened event of each item whose box is on, by number.
func normalizeGitHubItems(body []byte, newIssues, newPRs bool) (issues, prs []int, opened map[int]Event, err error) {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		HTMLURL   string     `json:"html_url"`
		CreatedAt *time.Time `json:"created_at"`
		// A pointer: "pull_request": null, as a GitHub-compatible server may send, is an issue.
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, nil, fmt.Errorf("decode issues: %w", err)
	}
	issues, prs, opened = []int{}, []int{}, map[int]Event{}
	for _, it := range raw {
		if it.Number <= 0 {
			continue
		}
		typ, on := "issue.opened", newIssues
		if it.PullRequest != nil {
			typ, on = "pr.opened", newPRs
			prs = append(prs, it.Number)
		} else {
			issues = append(issues, it.Number)
		}
		if !on {
			continue
		}
		labels := []string{}
		for _, l := range it.Labels {
			labels = append(labels, l.Name)
		}
		opened[it.Number] = Event{Type: typ, Subject: strconv.Itoa(it.Number), OccurredAt: it.CreatedAt, Payload: map[string]any{
			"number": it.Number, "title": it.Title, "author": it.User.Login, "labels": labels, "url": it.HTMLURL, "created_at": it.CreatedAt,
		}}
	}
	return issues, prs, opened, nil
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
	var floor time.Time
	for _, r := range old.Releases {
		before[r.Tag] = r
		if r.PublishedAt != nil && r.PublishedAt.After(floor) {
			floor = *r.PublishedAt
		}
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
		case !seen && r.PublishedAt != nil && r.PublishedAt.Before(floor):
			// Older than the newest seen: it slid back into the fetched window (filtered prereleases or
			// deleted releases had pushed it out), it is not new.
		case !seen || prev.Draft:
			events = append(events, Event{Type: "release.published", Subject: r.Tag, OccurredAt: r.PublishedAt, Payload: payload})
		case prev.Prerelease && !r.Prerelease:
			events = append(events, Event{Type: "release.promoted", Subject: r.Tag, OccurredAt: r.PublishedAt, Payload: payload})
		}
	}
	var listed []Event
	for _, n := range append(newNumbers(old.Issues, cur.Issues), newNumbers(old.PRs, cur.PRs)...) {
		listed = append(listed, Event{Type: itemListed, Subject: strconv.Itoa(n)})
	}
	for _, n := range newDiscussions(old.Discussions, cur.Discussions) {
		listed = append(listed, Event{Type: discussionListed, Subject: strconv.Itoa(n)})
	}
	// Issues, PRs and discussions share one numbering: number order is opening order.
	slices.SortFunc(listed, func(a, b Event) int {
		x, _ := strconv.Atoi(a.Subject)
		y, _ := strconv.Atoi(b.Subject)
		return x - y
	})
	return append(events, listed...)
}

// newDiscussions lists the numbers of cur above old's highest, oldest first; no list in old is a baseline.
// A converted issue keeps its lower number anywhere in the list, so only a number past every stored one is new.
func newDiscussions(old, cur *[]int) []int {
	if old == nil || cur == nil {
		return nil
	}
	floor := 0
	if len(*old) > 0 {
		floor = slices.Max(*old)
	}
	var out []int
	for _, n := range *cur {
		if n > floor {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// newNumbers lists the numbers of cur missing from old, oldest first. No list in old is a baseline (a box just
// checked, or a watch stored before the boxes); a number at or below old's lowest slid back into the window.
// ponytail: an empty old list has no floor, so an old item sliding back in after deletions reads as new.
func newNumbers(old, cur *[]int) []int {
	if old == nil || cur == nil {
		return nil
	}
	seen := make(map[int]bool, len(*old))
	for _, n := range *old {
		seen[n] = true
	}
	floor := 0
	if len(*old) > 0 {
		floor = slices.Min(*old)
	}
	var out []int
	for _, n := range *cur {
		if !seen[n] && n > floor {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
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
	// Reactions are the PR body's counts, nil with the box off so a box-off state stays as before 011.
	Reactions *Reactions `json:"reactions,omitempty"`
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
	// ReplyTo is the parent comment's URL on a discussion reply; omitempty keeps issue and PR states as they were.
	ReplyTo string `json:"reply_to,omitempty"`
	// Reactions are nil with the box off, and on an entry standing for a review or for several inline comments.
	Reactions *Reactions `json:"reactions,omitempty"`
}

// Reactions are the counts of GitHub's 8 reaction kinds; total_count and url are dropped.
type Reactions struct {
	PlusOne  int `json:"+1"`
	MinusOne int `json:"-1"`
	Laugh    int `json:"laugh"`
	Hooray   int `json:"hooray"`
	Confused int `json:"confused"`
	Heart    int `json:"heart"`
	Rocket   int `json:"rocket"`
	Eyes     int `json:"eyes"`
}

// reactionKinds names the counts of Reactions.list, in GitHub's order.
var reactionKinds = [...]string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}

func (r *Reactions) list() [len(reactionKinds)]int {
	return [...]int{r.PlusOne, r.MinusOne, r.Laugh, r.Hooray, r.Confused, r.Heart, r.Rocket, r.Eyes}
}

// itemReactions reads the reaction counts of an issue, PR (through its issue) or discussion body; nil when absent.
func itemReactions(body []byte) (*Reactions, error) {
	var raw struct {
		Reactions *Reactions `json:"reactions"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode reactions: %w", err)
	}
	return raw.Reactions, nil
}

// reactedEvents emits typ once per target whose counts rose: the item, then each comment stored in both states.
// No counts on either side (box just checked, a state from before 011, a folded entry) is a baseline.
func reactedEvents(typ string, number int, title, url string, old, cur *Reactions, oldC, curC []PRComment) []Event {
	var events []Event
	rose := func(target, u string, o, c *Reactions) {
		if o == nil || c == nil {
			return
		}
		added := map[string]int{}
		ol, cl := o.list(), c.list()
		for i, k := range reactionKinds {
			if cl[i] > ol[i] {
				added[k] = cl[i] - ol[i]
			}
		}
		if len(added) == 0 {
			return
		}
		counts, _ := json.Marshal(c)
		events = append(events, Event{Type: typ, Subject: u + ">" + string(counts), Payload: map[string]any{
			"number": number, "title": title, "url": u, "target": target, "added": added, "counts": *c,
		}})
	}
	rose("item", url, old, cur)
	before := make(map[string]*Reactions, len(oldC))
	for _, c := range oldC {
		before[c.URL] = c.Reactions
	}
	for _, c := range curC {
		rose("comment", c.URL, before[c.URL], c.Reactions)
	}
	return events
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
	if s.Comments, err = normalizeComments(bodies[0], bodies[3], issue, cfg.Reactions); err != nil {
		return PRState{}, err
	}
	if cfg.Reactions {
		// 011-AC14: the pulls endpoint has no counts; the issues one answers for the PR's conversation item.
		r, err := f.get(ctx, fmt.Sprintf("%s/issues/%d", base, cfg.Number), h, false)
		if err != nil {
			return PRState{}, err
		}
		if s.Reactions, err = itemReactions(r.body); err != nil {
			return PRState{}, err
		}
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
// With reactions, an entry from one inline or conversation comment carries its counts.
func normalizeComments(reviewsBody, reviewCommentsBody, issueCommentsBody []byte, reactions bool) ([]PRComment, error) {
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
		Reactions   *Reactions `json:"reactions"`
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
		c := PRComment{Author: it.User.Login, CreatedAt: *at, URL: it.HTMLURL, Excerpt: excerpt(it.Body)}
		if reactions {
			c.Reactions = it.Reactions
		}
		out = append(out, c)
		return true
	}
	carrier := map[int64]int{} // review id -> index in out of the comment that stands for the review
	for _, r := range reviews {
		r.Reactions = nil // review bodies have no counts
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
		out[i].Reactions = nil // one comment's counts would stand for several
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
	events = append(events, commentEvents("pr.commented", cur.Number, cur.Title, old.Comments, cur.Comments)...)
	return append(events, reactedEvents("pr.reacted", cur.Number, cur.Title, cur.URL, old.Reactions, cur.Reactions, old.Comments, cur.Comments)...)
}

// commentEvents emits typ for each comment of cur missing from old.
func commentEvents(typ string, number int, title string, old, cur []PRComment) []Event {
	var events []Event
	for _, c := range newComments(old, cur) {
		at := c.CreatedAt
		p := map[string]any{"number": number, "title": title, "url": c.URL, "author": c.Author, "excerpt": c.Excerpt}
		if c.ReplyTo != "" {
			p["reply_to"] = c.ReplyTo
		}
		events = append(events, Event{Type: typ, Subject: c.URL, OccurredAt: &at, Payload: p})
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

// IssueState is the normalized state of one GitHub issue.
type IssueState struct {
	Number      int      `json:"number"`
	State       string   `json:"state"`        // open or closed
	StateReason string   `json:"state_reason"` // as GitHub reports it (completed, not_planned, reopened...), empty when null
	Title       string   `json:"title"`
	Labels      []string `json:"labels"` // names, sorted, never nil
	URL         string   `json:"url"`
	// Comments follow PRState.Comments: nil is a state stored before comments were collected.
	Comments  []PRComment `json:"comments"`
	Reactions *Reactions  `json:"reactions,omitempty"` // like PRState.Reactions
}

func collectGitHubIssue(ctx context.Context, f *Fetcher, cfg watch.GitHubIssueConfig, cred *Credential) (IssueState, error) {
	h := githubHeaders(cred)
	base := fmt.Sprintf("%s/repos/%s/issues/%d", cfg.APIURL, cfg.Repo, cfg.Number)
	r, err := f.get(ctx, base, h, false)
	if err != nil {
		return IssueState{}, err
	}
	s, err := normalizeGitHubIssue(r.body)
	if err != nil {
		return IssueState{}, err
	}
	if cfg.Reactions {
		if s.Reactions, err = itemReactions(r.body); err != nil {
			return IssueState{}, err
		}
	}
	comments, err := lastPage(ctx, f, base+"/comments?per_page=100", h)
	if err != nil {
		return IssueState{}, err
	}
	// An issue has conversation comments only: no reviews, no inline comments.
	if s.Comments, err = normalizeComments([]byte(`[]`), []byte(`[]`), comments, cfg.Reactions); err != nil {
		return IssueState{}, err
	}
	return s, nil
}

// normalizeGitHubIssue reads one issue; a PR, which the issues endpoint also answers for, is an error.
func normalizeGitHubIssue(body []byte) (IssueState, error) {
	var raw struct {
		Number      int    `json:"number"`
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
		Title       string `json:"title"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		HTMLURL string `json:"html_url"`
		// A pointer: "pull_request": null, as a GitHub-compatible server may send, is an issue.
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.State == "" {
		return IssueState{}, fmt.Errorf("decode issue: no state (%v)", err)
	}
	if raw.PullRequest != nil {
		return IssueState{}, fmt.Errorf("#%d is a pull request: watch it with a github_pr watch", raw.Number)
	}
	s := IssueState{Number: raw.Number, State: raw.State, StateReason: raw.StateReason, Title: raw.Title, Labels: []string{}, URL: raw.HTMLURL}
	for _, l := range raw.Labels {
		s.Labels = append(s.Labels, l.Name)
	}
	slices.Sort(s.Labels)
	return s, nil
}

// diffIssue emits issue.state_changed (with the reason) and issue.labels_changed, then one issue.commented per new comment.
func diffIssue(old, cur IssueState) []Event {
	base := func() map[string]any {
		return map[string]any{"number": cur.Number, "title": cur.Title, "url": cur.URL}
	}
	events := withStateReason("issue", base, old.State, old.StateReason, cur.State, cur.StateReason, diffFacets("issue", base, []facet{
		{"state_changed", old.State, cur.State},
		{"labels_changed", old.Labels, cur.Labels},
	}))
	events = append(events, commentEvents("issue.commented", cur.Number, cur.Title, old.Comments, cur.Comments)...)
	return append(events, reactedEvents("issue.reacted", cur.Number, cur.Title, cur.URL, old.Reactions, cur.Reactions, old.Comments, cur.Comments)...)
}

// withStateReason adds cur's reason to the <prefix>.state_changed of events, and emits one for a reason change while closed.
func withStateReason(prefix string, base func() map[string]any, oldState, oldReason, curState, curReason string, events []Event) []Event {
	for _, e := range events {
		if e.Type == prefix+".state_changed" {
			e.Payload["state_reason"] = curReason
		}
	}
	// A closed item re-triaged (completed to duplicate...) keeps its state: only the reason tells, and names the subject.
	if oldState == "closed" && curState == "closed" && oldReason != curReason {
		p := base()
		p["from"], p["to"], p["state_reason"] = "closed", "closed", curReason
		events = append([]Event{{Type: prefix + ".state_changed", Subject: fmt.Sprintf("%q>%q", "closed:"+oldReason, "closed:"+curReason), Payload: p}}, events...)
	}
	return events
}

// DiscussionState is the normalized state of one GitHub discussion.
type DiscussionState struct {
	Number      int      `json:"number"`
	State       string   `json:"state"`        // open or closed
	StateReason string   `json:"state_reason"` // as GitHub reports it (resolved, outdated, duplicate, reopened), empty when null
	Title       string   `json:"title"`
	Category    string   `json:"category"` // shown only: a category change emits nothing
	Labels      []string `json:"labels"`   // names, sorted, never nil
	AnswerURL   string   `json:"answer_url"`
	// AnswerChosenBy is stored because diffs only see states: answer_changed carries it.
	AnswerChosenBy string `json:"answer_chosen_by"`
	URL            string `json:"url"`
	// Comments follow PRState.Comments, replies included with ReplyTo set.
	Comments  []PRComment `json:"comments"`
	Reactions *Reactions  `json:"reactions,omitempty"` // like PRState.Reactions
}

func collectGitHubDiscussion(ctx context.Context, f *Fetcher, cfg watch.GitHubDiscussionConfig, cred *Credential) (DiscussionState, error) {
	h := githubHeaders(cred)
	base := fmt.Sprintf("%s/repos/%s/discussions/%d", cfg.APIURL, cfg.Repo, cfg.Number)
	r, err := f.get(ctx, base, h, false)
	// 011-AC8: one numbering for issues, PRs and discussions; a 404 cannot tell those apart from a missing or private repository.
	if he := (*HTTPError)(nil); errors.As(err, &he) && he.Status == http.StatusNotFound {
		return DiscussionState{}, fmt.Errorf("#%d not found in %s: not a discussion (an issue or a PR? watch it with github_issue or github_pr), "+
			"or the repository is missing or private without a token: %w", cfg.Number, cfg.Repo, err)
	}
	if err != nil {
		return DiscussionState{}, err
	}
	s, err := normalizeGitHubDiscussion(r.body)
	if err != nil {
		return DiscussionState{}, err
	}
	if cfg.Reactions {
		if s.Reactions, err = itemReactions(r.body); err != nil {
			return DiscussionState{}, err
		}
	}
	comments, err := lastPage(ctx, f, base+"/comments?per_page=100", h)
	if err != nil {
		return DiscussionState{}, err
	}
	if s.Comments, err = normalizeDiscussionComments(s.URL, comments, cfg.Reactions); err != nil {
		return DiscussionState{}, err
	}
	return s, nil
}

func normalizeGitHubDiscussion(body []byte) (DiscussionState, error) {
	var raw struct {
		Number      int    `json:"number"`
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
		Title       string `json:"title"`
		Category    struct {
			Name string `json:"name"`
		} `json:"category"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		AnswerHTMLURL  string `json:"answer_html_url"`
		AnswerChosenBy struct {
			Login string `json:"login"`
		} `json:"answer_chosen_by"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.State == "" {
		return DiscussionState{}, fmt.Errorf("decode discussion: no state (%v)", err)
	}
	s := DiscussionState{Number: raw.Number, State: raw.State, StateReason: raw.StateReason, Title: raw.Title, Category: raw.Category.Name,
		Labels: []string{}, AnswerURL: raw.AnswerHTMLURL, AnswerChosenBy: raw.AnswerChosenBy.Login, URL: raw.HTMLURL}
	for _, l := range raw.Labels {
		s.Labels = append(s.Labels, l.Name)
	}
	slices.Sort(s.Labels)
	return s, nil
}

// normalizeDiscussionComments normalizes the flat comments list like issue comments, then points each reply at its parent.
func normalizeDiscussionComments(discussionURL string, body []byte, reactions bool) ([]PRComment, error) {
	var raw []struct {
		HTMLURL  string `json:"html_url"`
		ParentID *int64 `json:"parent_id"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode discussion comments: %w", err)
	}
	out, err := normalizeComments([]byte(`[]`), []byte(`[]`), body, reactions)
	if err != nil {
		return nil, err
	}
	parent := map[string]int64{}
	for _, c := range raw {
		if c.ParentID != nil {
			parent[c.HTMLURL] = *c.ParentID
		}
	}
	for i := range out {
		if id, ok := parent[out[i].URL]; ok {
			out[i].ReplyTo = fmt.Sprintf("%s#discussioncomment-%d", discussionURL, id)
		}
	}
	return out, nil
}

// diffDiscussion emits discussion.state_changed (with the reason), answer_changed (with chosen_by) and labels_changed,
// then one discussion.commented per new comment or reply.
func diffDiscussion(old, cur DiscussionState) []Event {
	base := func() map[string]any {
		return map[string]any{"number": cur.Number, "title": cur.Title, "url": cur.URL}
	}
	events := withStateReason("discussion", base, old.State, old.StateReason, cur.State, cur.StateReason, diffFacets("discussion", base, []facet{
		{"state_changed", old.State, cur.State},
		{"answer_changed", old.AnswerURL, cur.AnswerURL},
		{"labels_changed", old.Labels, cur.Labels},
	}))
	for _, e := range events {
		if e.Type == "discussion.answer_changed" {
			e.Payload["chosen_by"] = cur.AnswerChosenBy
		}
	}
	events = append(events, commentEvents("discussion.commented", cur.Number, cur.Title, old.Comments, cur.Comments)...)
	return append(events, reactedEvents("discussion.reacted", cur.Number, cur.Title, cur.URL, old.Reactions, cur.Reactions, old.Comments, cur.Comments)...)
}
