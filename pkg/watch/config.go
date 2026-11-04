// Package watch serves watches, their credentials and shares over the API, with per-kind validation.
package watch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Watch kinds.
const (
	KindGitHubRelease = "github_release"
	KindGitHubPR      = "github_pr"
	KindGitLabRelease = "gitlab_release"
	KindGitLabMR      = "gitlab_mr"
	KindOCITags       = "oci_tags"
)

// Credential kinds.
const (
	CredGitHubToken   = "github_token"
	CredGitLabToken   = "gitlab_token"
	CredRegistryBasic = "registry_basic"
)

// Kinds lists the watch kinds in display order.
var Kinds = []string{KindGitHubRelease, KindGitHubPR, KindGitLabRelease, KindGitLabMR, KindOCITags}

// CredentialKinds lists the credential kinds.
var CredentialKinds = []string{CredGitHubToken, CredGitLabToken, CredRegistryBasic}

// CredentialKindFor is the only credential kind a watch of this kind accepts.
func CredentialKindFor(kind string) string {
	switch kind {
	case KindGitHubRelease, KindGitHubPR:
		return CredGitHubToken
	case KindGitLabRelease, KindGitLabMR:
		return CredGitLabToken
	default:
		return CredRegistryBasic
	}
}

const (
	defaultGitHubAPI = "https://api.github.com"
	defaultGitLab    = "https://gitlab.com"
	dockerHub        = "docker.io"
	maxTagRegex      = 256
)

// GitHubReleaseConfig is the normalized config of a github_release watch.
type GitHubReleaseConfig struct {
	Repo               string `json:"repo"`
	IncludePrereleases bool   `json:"include_prereleases"`
	APIURL             string `json:"api_url"`
}

// GitHubPRConfig is the normalized config of a github_pr watch.
type GitHubPRConfig struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	APIURL string `json:"api_url"`
}

// GitLabReleaseConfig is the normalized config of a gitlab_release watch.
type GitLabReleaseConfig struct {
	BaseURL string `json:"base_url"`
	Project string `json:"project"`
}

// GitLabMRConfig is the normalized config of a gitlab_mr watch.
type GitLabMRConfig struct {
	BaseURL string `json:"base_url"`
	Project string `json:"project"`
	IID     int    `json:"iid"`
}

// OCITagsConfig is the normalized config of an oci_tags watch.
type OCITagsConfig struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	TagRegex   string `json:"tag_regex,omitempty"`
	SemverOnly bool   `json:"semver_only"`
}

var (
	// GitHub logins: alphanumerics and single hyphens; repository names add '.' and '_'.
	githubRepoRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	// GitLab namespaces nest: group/sub/project, segments never start with '-' (that is the /-/ route marker).
	gitlabProjectRe = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_.][A-Za-z0-9_.-]*)+$`)
	registryRe      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?$`)
	// OCI distribution spec repository name grammar.
	ociRepoRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
)

// fieldErrors collects field-level problems located under a base path such as "body.config".
type fieldErrors struct {
	base string
	errs []error
}

func (f *fieldErrors) add(field, msg string, value any) {
	loc := f.base
	if field != "" {
		loc += "." + field
	}
	f.errs = append(f.errs, &huma.ErrorDetail{Location: loc, Message: msg, Value: value})
}

// validator checks and normalizes watch configs; httpHosts may be reached over plain http.
type validator struct {
	httpHosts []string
}

// Config checks raw against kind's schema and returns its normalized form (defaults filled), or
// field errors located under "body.config".
func (v validator) Config(kind string, raw map[string]any) (map[string]any, []error) {
	fe := &fieldErrors{base: "body.config"}
	var target any
	switch kind {
	case KindGitHubRelease:
		target = &GitHubReleaseConfig{}
	case KindGitHubPR:
		target = &GitHubPRConfig{}
	case KindGitLabRelease:
		target = &GitLabReleaseConfig{}
	case KindGitLabMR:
		target = &GitLabMRConfig{}
	case KindOCITags:
		target = &OCITagsConfig{}
	default:
		fe.base = "body.kind"
		fe.add("", "unknown kind, want one of "+strings.Join(Kinds, ", "), kind)
		return nil, fe.errs
	}
	if !decodeStrict(raw, target, fe) {
		return nil, fe.errs
	}
	switch c := target.(type) {
	case *GitHubReleaseConfig:
		c.Repo = v.repo(c.Repo, fe)
		c.APIURL = v.url("api_url", c.APIURL, defaultGitHubAPI, fe)
	case *GitHubPRConfig:
		c.Repo = v.repo(c.Repo, fe)
		c.APIURL = v.url("api_url", c.APIURL, defaultGitHubAPI, fe)
		positive("number", c.Number, fe)
	case *GitLabReleaseConfig:
		c.BaseURL = v.url("base_url", c.BaseURL, defaultGitLab, fe)
		c.Project = project(c.Project, fe)
	case *GitLabMRConfig:
		c.BaseURL = v.url("base_url", c.BaseURL, defaultGitLab, fe)
		c.Project = project(c.Project, fe)
		positive("iid", c.IID, fe)
	case *OCITagsConfig:
		ociTags(c, fe)
	}
	if len(fe.errs) > 0 {
		return nil, fe.errs
	}
	b, _ := json.Marshal(target)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, nil
}

var typeNames = map[reflect.Kind]string{reflect.String: "a string", reflect.Bool: "true or false", reflect.Int: "an integer"}

// decodeStrict maps raw onto target, reporting each unknown key and each mistyped field.
func decodeStrict(raw map[string]any, target any, fe *fieldErrors) bool {
	known := map[string]bool{}
	t := reflect.TypeOf(target).Elem()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		known[name] = true
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !known[k] {
			fe.add(k, "unknown field", nil)
		}
	}
	if len(fe.errs) > 0 {
		return false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		fe.add("", "not a JSON object", nil)
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(target); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			fe.add(te.Field, "expected "+typeNames[te.Type.Kind()], nil)
		} else {
			fe.add("", err.Error(), nil)
		}
		return false
	}
	return true
}

func (v validator) repo(s string, fe *fieldErrors) string {
	s = strings.TrimSpace(s)
	_, name, _ := strings.Cut(s, "/")
	if !githubRepoRe.MatchString(s) || name == "." || name == ".." {
		fe.add("repo", `expected "owner/name", e.g. golang/go`, s)
	}
	return s
}

func project(s string, fe *fieldErrors) string {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if len(s) > 255 || !gitlabProjectRe.MatchString(s) {
		fe.add("project", `expected a project path "group/project" or "group/sub/project"`, s)
	}
	return s
}

func positive(field string, n int, fe *fieldErrors) {
	if n <= 0 {
		fe.add(field, "must be a positive integer", n)
	}
}

// url checks an API base URL: https, or http for an allowlisted host; no credentials, query or fragment.
func (v validator) url(field, raw, fallback string, fe *fieldErrors) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Opaque != "":
		fe.add(field, "expected an absolute URL, e.g. "+fallback, raw)
	case u.User != nil:
		fe.add(field, "must not carry credentials; attach a credential instead", nil)
	case u.RawQuery != "" || u.Fragment != "":
		fe.add(field, "must not have a query or fragment", raw)
	case u.Scheme == "https":
	case u.Scheme == "http" && slices.Contains(v.httpHosts, strings.ToLower(u.Hostname())):
	default:
		fe.add(field, "must use https", raw)
	}
	return strings.TrimRight(raw, "/")
}

func ociTags(c *OCITagsConfig, fe *fieldErrors) {
	c.Registry = strings.ToLower(strings.TrimSpace(c.Registry))
	switch c.Registry {
	case "", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		c.Registry = dockerHub
	}
	if !registryRe.MatchString(c.Registry) {
		fe.add("registry", "expected a registry host, e.g. ghcr.io or registry.example.com:5000", c.Registry)
	}
	c.Repository = strings.Trim(strings.TrimSpace(c.Repository), "/")
	// Docker Hub's official images live under library/: "postgres" is "library/postgres".
	if c.Registry == dockerHub && c.Repository != "" && !strings.Contains(c.Repository, "/") {
		c.Repository = "library/" + c.Repository
	}
	if len(c.Repository) > 255 || !ociRepoRe.MatchString(c.Repository) {
		fe.add("repository", "expected a lowercase repository path, e.g. library/postgres", c.Repository)
	}
	if len(c.TagRegex) > maxTagRegex {
		fe.add("tag_regex", fmt.Sprintf("at most %d characters", maxTagRegex), nil)
	} else if _, err := regexp.Compile(c.TagRegex); err != nil {
		fe.add("tag_regex", "invalid regular expression: "+err.Error(), c.TagRegex)
	}
}
