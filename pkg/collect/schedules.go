package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"

	"github.com/gedour/gedour/pkg/watch"
)

const (
	workflowsDir     = ".github/workflows"
	workflowActive   = "active"
	workflowInactive = "disabled_inactivity"
	// maxWorkflowPages bounds the workflows list at 1000 entries.
	maxWorkflowPages = 10
	// staleGrace: GitHub delays scheduled runs under load, so an expected time only counts as missed an hour later.
	staleGrace = time.Hour
	// maxMissed caps the missed count reported on the stale transition, which bounds the walk of a minutely cron.
	maxMissed = 1000
	// inactivityCutoff: GitHub turns off a public repository's scheduled workflows after 60 days without activity.
	inactivityCutoff = 60 * 24 * time.Hour
)

// SchedulesState is the normalized state of a github_schedules watch.
type SchedulesState struct {
	// AliveOn is the config's proof-of-life rule: a switch moves last_alive_at with no run behind it.
	AliveOn string `json:"alive_on"`
	// Workflows holds the scheduled workflows, sorted by path, never nil.
	Workflows []ScheduledWorkflow `json:"workflows"`
	// Unscheduled holds the blob sha of each workflow file without a schedule, by path, so an unchanged one is not fetched again.
	Unscheduled map[string]string `json:"unscheduled"`
	// Set only with warn_days: Repo names the subject of schedule.disable_soon, which Diff cannot read from the config.
	Repo           string     `json:"repo,omitempty"`
	Private        *bool      `json:"private,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	DisableAt      *time.Time `json:"disable_at,omitempty"`
	DisableSoon    bool       `json:"disable_soon,omitempty"`
}

// WorkflowCron is one on.schedule entry of a workflow file.
type WorkflowCron struct {
	Cron     string `json:"cron" yaml:"cron"`
	Timezone string `json:"timezone,omitempty" yaml:"timezone"`
}

// ScheduledWorkflow is a workflow whose file on the default branch has an on.schedule.
type ScheduledWorkflow struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Path        string         `json:"path"`
	FileSHA     string         `json:"file_sha"`
	Crons       []WorkflowCron `json:"crons"`
	State       string         `json:"state"`
	URL         string         `json:"url"`
	LastAliveAt *time.Time     `json:"last_alive_at"`
	Stale       bool           `json:"stale"`
	// Set only while stale: between two expected times the state stays the same.
	Missed     int        `json:"missed,omitempty"`
	ExpectedAt *time.Time `json:"expected_at,omitempty"`
	// ReenabledAt is gedour's last re-enable: GitHub's listed updated_at may not move with it.
	ReenabledAt *time.Time `json:"reenabled_at,omitempty"`
	// ReenableError is this run's failed re-enable, for the schedule.disabled payload.
	ReenableError string `json:"reenable_error,omitempty"`
	// ActiveSince is when gedour first saw a stored non-active workflow listed active: a re-enable it did not see succeed
	// may leave updated_at old.
	ActiveSince *time.Time `json:"active_since,omitempty"`
}

type listedWorkflow struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Every call is unconditional: a 304 on one of them would end the run with no state (008).
func collectGitHubSchedules(ctx context.Context, f *Fetcher, cfg watch.GitHubSchedulesConfig, cred *Credential) (SchedulesState, error) {
	h := githubHeaders(cred)
	base := fmt.Sprintf("%s/repos/%s", cfg.APIURL, cfg.Repo)
	var prev SchedulesState
	// An unreadable or missing last state only costs a fetch of every file.
	_ = json.Unmarshal(f.Prev, &prev)
	listed, err := listWorkflows(ctx, f, base, h)
	if err != nil {
		return SchedulesState{}, err
	}
	shas, dirURL, dirHeader, err := workflowFiles(ctx, f, base, h, listed)
	if err != nil {
		return SchedulesState{}, err
	}
	old := make(map[string]ScheduledWorkflow, len(prev.Workflows))
	for _, w := range prev.Workflows {
		old[w.Path] = w
	}
	extra := 0
	if cfg.WarnDays > 0 {
		extra = 2
	}
	if err := checkBudget(listed, shas, prev, old, extra, cfg.AutoEnable, dirURL, dirHeader, f.now(), cred); err != nil {
		return SchedulesState{}, err
	}
	raw := h.Clone()
	raw.Set("Accept", "application/vnd.github.raw+json")
	s := SchedulesState{AliveOn: cfg.AliveOn, Workflows: []ScheduledWorkflow{}, Unscheduled: map[string]string{}}
	for _, lw := range listed {
		sha, ok := shas[lw.Path]
		if !ok {
			continue
		}
		o, known := old[lw.Path]
		crons, cached := cachedCrons(prev, old, lw.Path, sha)
		if !cached {
			r, err := f.get(ctx, base+"/contents/"+(&url.URL{Path: lw.Path}).EscapedPath(), raw, false)
			if err != nil {
				return SchedulesState{}, err
			}
			crons = workflowCrons(r.body)
		}
		if len(crons) == 0 {
			s.Unscheduled[lw.Path] = sha
			continue
		}
		w := ScheduledWorkflow{ID: lw.ID, Name: lw.Name, Path: lw.Path, FileSHA: sha, Crons: crons, State: lw.State, URL: lw.HTMLURL,
			LastAliveAt: o.LastAliveAt, ReenabledAt: o.ReenabledAt, ActiveSince: o.ActiveSince}
		if known && o.State != workflowActive && w.State == workflowActive {
			now := f.now().UTC()
			w.ActiveSince = &now
		}
		if w.State == workflowActive {
			ref := lw.UpdatedAt
			for _, t := range []*time.Time{w.ReenabledAt, w.ActiveSince} {
				if t != nil && t.After(ref) {
					ref = *t
				}
			}
			if err := checkAlive(ctx, f, base, h, cfg, &w, o, ref); err != nil {
				return SchedulesState{}, err
			}
		}
		s.Workflows = append(s.Workflows, w)
	}
	if cfg.WarnDays > 0 {
		if err := checkActivity(ctx, f, base, h, cfg, &s); err != nil {
			return SchedulesState{}, err
		}
	}
	// Writes come after every read: a read failing after a PUT would drop the run that did it.
	if cfg.AutoEnable {
		if err := reenableAll(ctx, f, base, h, cfg.Repo, s.Workflows, old); err != nil {
			return SchedulesState{}, err
		}
	}
	if s.DisableAt != nil {
		warnFrom := s.DisableAt.Add(-time.Duration(cfg.WarnDays) * 24 * time.Hour)
		s.DisableSoon = !f.now().Before(warnFrom) && len(activePaths(s.Workflows)) > 0
	}
	return s, nil
}

// reenableAll re-enables each workflow GitHub disabled for inactivity. A rate limit or a cancelled run before any success
// fails the run like any call; after one, the run keeps what worked and sends no more PUTs, the rest left to the next run.
// A workflow left without a PUT keeps its stored reason: it neither opens nor ends a failure streak.
func reenableAll(ctx context.Context, f *Fetcher, base string, h http.Header, repo string, ws []ScheduledWorkflow, old map[string]ScheduledWorkflow) error {
	stopped, done := false, false
	for i := range ws {
		w := &ws[i]
		if w.State != workflowInactive {
			continue
		}
		if stopped {
			w.ReenableError = old[w.Path].ReenableError
			continue
		}
		err := reenable(ctx, f, base, h, repo, w)
		if done && err != nil {
			// A rate limit or a cut run says nothing of the token's rights: the log keeps it, the streak does not.
			w.ReenableError = old[w.Path].ReenableError
			stopped = true
			continue
		}
		if err != nil {
			return err
		}
		done = done || w.State == workflowActive
	}
	return nil
}

// reenable turns on a workflow GitHub disabled for inactivity. A refused or failed PUT keeps it disabled with the reason and
// the run goes on: a token without Actions write must not hide schedule.disabled. Its reference is now: no runs call, never stale.
func reenable(ctx context.Context, f *Fetcher, base string, h http.Header, repo string, w *ScheduledWorkflow) error {
	r, err := f.do(ctx, http.MethodPut, fmt.Sprintf("%s/actions/workflows/%d/enable", base, w.ID), h, false)
	out := Reenable{Repo: repo, Path: w.Path}
	var he *HTTPError
	switch {
	// GitHub answers 204; a redirect replayed as a GET can end on another 2xx that enabled nothing.
	case err == nil && r.status == http.StatusNoContent:
		now := f.now().UTC()
		out.Status, w.State, w.ReenabledAt = r.status, workflowActive, &now
	case err == nil:
		out.Status, w.ReenableError = r.status, fmt.Sprintf("HTTP %d, expected 204", r.status)
	case errors.As(err, &he):
		out.Status, w.ReenableError = he.Status, fmt.Sprintf("HTTP %d", he.Status)
		if he.Body != "" {
			w.ReenableError += ": " + he.Body
		}
	default:
		w.ReenableError = snippet([]byte(err.Error()))
	}
	out.Err = w.ReenableError
	f.Reenables = append(f.Reenables, out)
	// A rate limit says nothing of the token's rights: the run waits for the reset, like on any other call.
	if rl := (*RateLimitError)(nil); errors.As(err, &rl) || ctx.Err() != nil {
		return err
	}
	return nil
}

// checkActivity reads the repository's visibility and last default-branch commit, and sets when GitHub turns a public
// repository's scheduled workflows off.
func checkActivity(ctx context.Context, f *Fetcher, base string, h http.Header, cfg watch.GitHubSchedulesConfig, s *SchedulesState) error {
	var repo struct {
		Private bool `json:"private"`
	}
	if _, err := f.getJSON(ctx, base, h, false, &repo); err != nil {
		return err
	}
	var commits []struct {
		Commit struct {
			// The committer date is when the commit landed: a rebased or cherry-picked one keeps an older author date.
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	_, err := f.getJSON(ctx, base+"/commits?per_page=1", h, false, &commits)
	// GitHub answers 409 Conflict for an empty repository: no commit yet.
	if he := (*HTTPError)(nil); errors.As(err, &he) && he.Status == http.StatusConflict {
		commits, err = nil, nil
	}
	if err != nil {
		return err
	}
	s.Repo, s.Private = cfg.Repo, &repo.Private
	if len(commits) == 0 {
		return nil
	}
	last := commits[0].Commit.Committer.Date.UTC()
	s.LastActivityAt = &last
	if repo.Private {
		return nil
	}
	at := last.Add(inactivityCutoff)
	s.DisableAt = &at
	return nil
}

func activePaths(ws []ScheduledWorkflow) []string {
	var out []string
	for _, w := range ws {
		if w.State == workflowActive {
			out = append(out, w.Path)
		}
	}
	return out
}

// cachedCrons returns the stored crons of an unchanged workflow file; cached is false when the file must be fetched.
func cachedCrons(prev SchedulesState, old map[string]ScheduledWorkflow, p, sha string) (crons []WorkflowCron, cached bool) {
	if u, ok := prev.Unscheduled[p]; ok && u == sha {
		return nil, true
	}
	if o, ok := old[p]; ok && o.FileSHA == sha {
		return o.Crons, true
	}
	return nil, false
}

// checkBudget stops the run before the per-file calls when the rate limit left cannot cover them: a run cut short
// saves no state, so the next one would fetch every file again and never get through. It waits for the reset like
// any rate limit, or fails when even a full budget is too small.
func checkBudget(listed []listedWorkflow, shas map[string]string, prev SchedulesState, old map[string]ScheduledWorkflow, extra int, autoEnable bool,
	dirURL string, hdr http.Header, now time.Time, cred *Credential) error {
	left, err := strconv.Atoi(hdr.Get("X-RateLimit-Remaining"))
	if err != nil {
		return nil
	}
	need := extra
	for _, lw := range listed {
		sha, ok := shas[lw.Path]
		if !ok {
			continue
		}
		crons, cached := cachedCrons(prev, old, lw.Path, sha)
		if !cached {
			need++
		}
		// Upper bound: a fetched file may turn out unscheduled.
		if !cached || len(crons) > 0 {
			switch {
			case lw.State == workflowActive:
				need++
			case autoEnable && lw.State == workflowInactive:
				need++
			}
		}
	}
	if need <= left {
		return nil
	}
	hint := ""
	if cred == nil {
		hint = " (a github_token credential raises the limit)"
	}
	if limit, err := strconv.Atoi(hdr.Get("X-RateLimit-Limit")); err == nil && need > limit {
		return fmt.Errorf("rate limit: this run needs %s, more than the limit of %d%s", requests(need), limit, hint)
	}
	until, ok := unixHeader(hdr.Get("X-RateLimit-Reset"))
	if !ok {
		until = now.Add(defaultRateLimitWait)
	}
	return fmt.Errorf("rate limit: %d left, this run needs %s%s: %w", left, requests(need), hint, &RateLimitError{Until: until, URL: dirURL})
}

func requests(n int) string {
	if n == 1 {
		return "1 request"
	}
	return strconv.Itoa(n) + " requests"
}

// listWorkflows reads every page of the workflows list, sorted by path.
func listWorkflows(ctx context.Context, f *Fetcher, base string, h http.Header) ([]listedWorkflow, error) {
	var all []listedWorkflow
	u := base + "/actions/workflows?per_page=100"
	for page := 0; u != ""; page++ {
		if page == maxWorkflowPages {
			return nil, fmt.Errorf("workflows list: more than %d pages", maxWorkflowPages)
		}
		var body struct {
			Workflows []listedWorkflow `json:"workflows"`
		}
		r, err := f.getJSON(ctx, u, h, false, &body)
		if err != nil {
			return nil, err
		}
		all = append(all, body.Workflows...)
		if u, err = relLink(u, r.header.Get("Link"), nextRe); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(all, func(a, b listedWorkflow) int { return strings.Compare(a.Path, b.Path) })
	return all, nil
}

// inWorkflowsDir also keeps a crafted "../" path from reaching another API route with the token.
func inWorkflowsDir(p string) bool { return path.Dir(p) == workflowsDir }

// workflowFiles maps each file of the default branch's workflows directory to its blob sha, and returns the
// listing's URL and headers (nil when not read) for the rate budget.
func workflowFiles(ctx context.Context, f *Fetcher, base string, h http.Header, listed []listedWorkflow) (map[string]string, string, http.Header, error) {
	shas := map[string]string{}
	// Only dynamic or deleted workflows: the directory may not exist, so the call is saved.
	if !slices.ContainsFunc(listed, func(w listedWorkflow) bool { return w.State != "deleted" && inWorkflowsDir(w.Path) }) {
		return shas, "", nil, nil
	}
	var dir []struct {
		Path string `json:"path"`
		SHA  string `json:"sha"`
		Type string `json:"type"`
	}
	u := base + "/contents/" + workflowsDir
	r, err := f.getJSON(ctx, u, h, false, &dir)
	// The workflows list already proved the repository: a 404 is a directory gone from the default branch.
	if he := (*HTTPError)(nil); errors.As(err, &he) && he.Status == http.StatusNotFound {
		return shas, "", nil, nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	for _, e := range dir {
		if e.Type == "file" && e.SHA != "" && inWorkflowsDir(e.Path) {
			shas[e.Path] = e.SHA
		}
	}
	return shas, u, r.header, nil
}

// workflowCrons returns a workflow file's on.schedule entries; nil without one, or when the file, a cron or a timezone does not parse.
func workflowCrons(body []byte) []WorkflowCron {
	var doc struct {
		On yaml.Node `yaml:"on"`
	}
	// on as a string or a list never holds a schedule.
	if yaml.Unmarshal(body, &doc) != nil || doc.On.Kind != yaml.MappingNode {
		return nil
	}
	var on struct {
		Schedule []WorkflowCron `yaml:"schedule"`
	}
	if doc.On.Decode(&on) != nil {
		return nil
	}
	for _, c := range on.Schedule {
		if _, err := cronSchedule(c); err != nil {
			return nil
		}
	}
	return on.Schedule
}

var workflowCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// cronSchedule parses one entry in its timezone, UTC without one.
func cronSchedule(c WorkflowCron) (cron.Schedule, error) {
	tz := c.Timezone
	if tz == "" {
		tz = "UTC"
	}
	// The parser reads a bare spec, and "Local", in the server's zone.
	if tz == "Local" {
		return nil, fmt.Errorf("cron %q: timezone Local", c.Cron)
	}
	s, err := workflowCronParser.Parse("CRON_TZ=" + tz + " " + c.Cron)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", c.Cron, err)
	}
	return s, nil
}

// checkAlive reads the last proof-of-life run and marks w stale when missed_runs expected times passed since the later of it and
// notBefore, the workflow's last update or re-enable.
// While o, the stored entry, is already stale with no newer run since, its count is kept so the state does not change run to run.
func checkAlive(ctx context.Context, f *Fetcher, base string, h http.Header, cfg watch.GitHubSchedulesConfig, w *ScheduledWorkflow, o ScheduledWorkflow,
	notBefore time.Time) error {
	u := fmt.Sprintf("%s/actions/workflows/%d/runs?event=schedule&status=%s&per_page=1", base, w.ID, url.QueryEscape(cfg.AliveOn))
	var runs struct {
		WorkflowRuns []struct {
			CreatedAt time.Time `json:"created_at"`
		} `json:"workflow_runs"`
	}
	if _, err := f.getJSON(ctx, u, h, false, &runs); err != nil {
		return err
	}
	w.LastAliveAt = nil
	ref := notBefore
	if len(runs.WorkflowRuns) > 0 {
		t := runs.WorkflowRuns[0].CreatedAt.UTC()
		w.LastAliveAt = &t
		if t.After(ref) {
			ref = t
		}
	}
	scheds := make([]cron.Schedule, 0, len(w.Crons))
	for _, c := range w.Crons {
		if s, err := cronSchedule(c); err == nil {
			scheds = append(scheds, s)
		}
	}
	until := f.now().Add(-staleGrace)
	n, first := missedRuns(scheds, ref, until, cfg.MissedRuns)
	if n < cfg.MissedRuns {
		return nil
	}
	w.Stale = true
	// Kept while its reference holds and it still reaches missed_runs: a newer run, an edit or a switched alive_on moves the first missed time.
	if o.Stale && o.Missed >= cfg.MissedRuns && o.ExpectedAt != nil && o.ExpectedAt.Equal(first) {
		w.Missed, w.ExpectedAt = o.Missed, o.ExpectedAt
		return nil
	}
	n, first = missedRuns(scheds, ref, until, maxMissed)
	w.Missed, w.ExpectedAt = n, &first
	return nil
}

// missedRuns counts the expected times of the union of scheds after ref and up to until, stopping at limit so a
// minutely cron with an old ref stays cheap, and returns the oldest of them.
func missedRuns(scheds []cron.Schedule, ref, until time.Time, limit int) (int, time.Time) {
	var first time.Time
	n := 0
	for t := ref; n < limit; n++ {
		var next time.Time
		for _, s := range scheds {
			// Next is zero for a spec with no time in the next five years (31 February).
			if x := s.Next(t); !x.IsZero() && (next.IsZero() || x.Before(next)) {
				next = x
			}
		}
		if next.IsZero() || next.After(until) {
			break
		}
		if n == 0 {
			first = next.UTC()
		}
		t = next
	}
	return n, first
}

// diffSchedules emits per workflow, by path: schedule.reenabled, schedule.disabled or schedule.enabled, then schedule.stale or schedule.recovered;
// then schedule.disable_soon for the repository. A workflow new to the state reads as healthy before, and an old state
// without disable_soon reads as false, so the first run reports what is already broken; a workflow that left emits nothing.
func diffSchedules(old, cur SchedulesState) []Event {
	prev := make(map[string]ScheduledWorkflow, len(old.Workflows))
	for _, w := range old.Workflows {
		prev[w.Path] = w
	}
	var events []Event
	for _, w := range cur.Workflows {
		o, ok := prev[w.Path]
		if !ok {
			o = ScheduledWorkflow{State: workflowActive}
		}
		add := func(typ string, extra map[string]any) {
			p := map[string]any{"workflow": w.Name, "path": w.Path, "state": w.State, "last_alive_at": w.LastAliveAt, "url": w.URL}
			maps.Copy(p, extra)
			events = append(events, Event{Type: typ, Subject: w.Path, Payload: p})
		}
		switch {
		// Re-enabled this run, from whatever state was stored: never also disabled or enabled.
		case w.ReenabledAt != nil && !sameTime(o.ReenabledAt, w.ReenabledAt):
			add("schedule.reenabled", map[string]any{"from": workflowInactive})
		case o.State == workflowActive && w.State != workflowActive:
			var extra map[string]any
			if w.ReenableError != "" {
				extra = map[string]any{"reenable_error": w.ReenableError}
			}
			add("schedule.disabled", extra)
		case o.State != workflowActive && w.State == workflowActive:
			add("schedule.enabled", nil)
		// A workflow already stored disabled whose re-enable starts failing: once per failure streak, whatever its reason.
		case w.State != workflowActive && w.ReenableError != "" && o.ReenableError == "":
			add("schedule.disabled", map[string]any{"reenable_error": w.ReenableError})
		}
		fresh := newProof(old.AliveOn, cur.AliveOn, o, w)
		switch {
		// A stale job that came back and died again between two runs is stale anew; a new run older than the
		// reference (an edit) leaves the episode where it was.
		case w.Stale && (!o.Stale || fresh && !sameTime(o.ExpectedAt, w.ExpectedAt)):
			add("schedule.stale", map[string]any{"missed": w.Missed, "expected_at": w.ExpectedAt})
		// Only a new proof-of-life run recovers: a raised missed_runs, a switched alive_on, an edit or a re-enable
		// (updated_at moves the reference) clears stale with no run behind it, and a disabled workflow is not stale.
		case o.Stale && !w.Stale && w.State == workflowActive && fresh:
			add("schedule.recovered", nil)
		}
	}
	if cur.DisableSoon && !old.DisableSoon {
		events = append(events, Event{Type: "schedule.disable_soon", Subject: cur.Repo, Payload: map[string]any{
			"repo": cur.Repo, "last_activity_at": cur.LastActivityAt, "disable_at": cur.DisableAt, "workflows": activePaths(cur.Workflows)}})
	}
	return events
}

func sameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// newProof reports a proof-of-life run in w newer than the one in o, read under the same alive_on.
func newProof(oldAliveOn, curAliveOn string, o, w ScheduledWorkflow) bool {
	return oldAliveOn == curAliveOn && w.LastAliveAt != nil && (o.LastAliveAt == nil || w.LastAliveAt.After(*o.LastAliveAt))
}
