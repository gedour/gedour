package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gedour/gedour/pkg/watch"
)

func gitlabHeaders(cred *Credential) http.Header {
	h := http.Header{"Accept": {"application/json"}}
	if cred != nil {
		h.Set("Private-Token", cred.Secret)
	}
	return h
}

func gitlabProject(base, project string) string {
	return fmt.Sprintf("%s/api/v4/projects/%s", base, url.PathEscape(project))
}

func collectGitLabReleases(ctx context.Context, f *Fetcher, cfg watch.GitLabReleaseConfig, cred *Credential) (ReleaseState, error) {
	r, err := f.get(ctx, gitlabProject(cfg.BaseURL, cfg.Project)+"/releases?per_page=50", gitlabHeaders(cred), true)
	if err != nil {
		return ReleaseState{}, err
	}
	s, err := normalizeGitLabReleases(r.body)
	f.Notes = s.Notes
	return s, err
}

func normalizeGitLabReleases(body []byte) (ReleaseState, error) {
	var raw []struct {
		TagName         string     `json:"tag_name"`
		Name            string     `json:"name"`
		ReleasedAt      *time.Time `json:"released_at"`
		UpcomingRelease bool       `json:"upcoming_release"`
		Description     string     `json:"description"`
		Links           struct {
			Self string `json:"self"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ReleaseState{}, fmt.Errorf("decode releases: %w", err)
	}
	s := ReleaseState{Notes: map[string]string{}}
	for _, r := range raw {
		if r.TagName == "" {
			continue
		}
		// An upcoming release (released_at in the future) is GitLab's closest thing to a prerelease.
		s.Releases = append(s.Releases, Release{Tag: r.TagName, Name: r.Name, Prerelease: r.UpcomingRelease,
			PublishedAt: r.ReleasedAt, URL: r.Links.Self})
		s.Notes[r.TagName] = r.Description
	}
	sortReleases(s.Releases)
	return s, nil
}

// Approvals is a merge request's approval summary.
type Approvals struct {
	Approved bool `json:"approved"`
	Count    int  `json:"count"`
}

// MRState is the normalized state of one GitLab merge request.
type MRState struct {
	IID       int       `json:"iid"`
	State     string    `json:"state"` // opened, closed, merged or locked
	Draft     bool      `json:"draft"`
	Title     string    `json:"title"`
	Approvals Approvals `json:"approvals"`
	Pipeline  string    `json:"pipeline"` // GitLab's pipeline status, or none
	SHA       string    `json:"sha"`
	URL       string    `json:"url"`
}

func collectGitLabMR(ctx context.Context, f *Fetcher, cfg watch.GitLabMRConfig, cred *Credential) (MRState, error) {
	h := gitlabHeaders(cred)
	base := fmt.Sprintf("%s/merge_requests/%d", gitlabProject(cfg.BaseURL, cfg.Project), cfg.IID)
	mr, err := f.get(ctx, base, h, false)
	if err != nil {
		return MRState{}, err
	}
	ap, err := f.get(ctx, base+"/approvals", h, false)
	if err != nil {
		return MRState{}, err
	}
	return normalizeGitLabMR(mr.body, ap.body)
}

func normalizeGitLabMR(mrBody, approvalsBody []byte) (MRState, error) {
	var mr struct {
		IID            int    `json:"iid"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		WorkInProgress bool   `json:"work_in_progress"`
		Title          string `json:"title"`
		SHA            string `json:"sha"`
		WebURL         string `json:"web_url"`
		HeadPipeline   *struct {
			Status string `json:"status"`
		} `json:"head_pipeline"`
	}
	var ap struct {
		Approved   bool  `json:"approved"`
		ApprovedBy []any `json:"approved_by"`
	}
	if err := json.Unmarshal(mrBody, &mr); err != nil {
		return MRState{}, fmt.Errorf("decode merge request: %w", err)
	}
	if err := json.Unmarshal(approvalsBody, &ap); err != nil {
		return MRState{}, fmt.Errorf("decode approvals: %w", err)
	}
	s := MRState{IID: mr.IID, State: mr.State, Draft: mr.Draft || mr.WorkInProgress, Title: mr.Title, SHA: mr.SHA, URL: mr.WebURL,
		Approvals: Approvals{Approved: ap.Approved, Count: len(ap.ApprovedBy)}, Pipeline: "none"}
	if mr.HeadPipeline != nil && mr.HeadPipeline.Status != "" {
		s.Pipeline = mr.HeadPipeline.Status
	}
	return s, nil
}

func diffMR(old, cur MRState) []Event {
	base := func() map[string]any {
		return map[string]any{"iid": cur.IID, "title": cur.Title, "url": cur.URL}
	}
	return diffFacets("mr", base, []facet{
		{"state_changed", old.State, cur.State},
		{"draft_changed", old.Draft, cur.Draft},
		{"approval_changed", old.Approvals, cur.Approvals},
		{"pipeline_changed", old.Pipeline, cur.Pipeline},
	})
}
