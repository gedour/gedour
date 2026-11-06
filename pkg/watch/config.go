// Package watch serves watches, their credentials and shares over the API, with per-kind validation.
package watch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Watch kinds.
const (
	KindGitHubRepo    = "github_repo"
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
var Kinds = []string{KindGitHubRepo, KindGitHubPR, KindGitLabRelease, KindGitLabMR, KindOCITags}

// CredentialKinds lists the credential kinds.
var CredentialKinds = []string{CredGitHubToken, CredGitLabToken, CredRegistryBasic}

// CredentialKindFor is the only credential kind a watch of this kind accepts.
func CredentialKindFor(kind string) string {
	switch kind {
	case KindGitHubRepo, KindGitHubPR:
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

// GitHubRepoConfig is the normalized config of a github_repo watch.
type GitHubRepoConfig struct {
	Repo               string `json:"repo"`
	IncludePrereleases bool   `json:"include_prereleases"`
	APIURL             string `json:"api_url"`
	// omitempty: a config with both boxes off stays as stored before them, so saving it unchanged is no retarget.
	NewIssues bool `json:"new_issues,omitempty"`
	NewPRs    bool `json:"new_prs,omitempty"`
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

// defaultHosts need no watch of the owner's for a key to target them, with no path and on their own kind only: docker.io and ghcr.io redirect a chosen path to other hosts, and a GitHub or GitLab kind's path holds a key's repo or project.
var defaultHosts = map[string][]string{
	KindGitHubRepo:    {"api.github.com"},
	KindGitHubPR:      {"api.github.com"},
	KindGitLabRelease: {"gitlab.com"},
	KindGitLabMR:      {"gitlab.com"},
	KindOCITags:       {dockerHub, "ghcr.io"},
}

func isDefaultHost(target string) bool {
	for _, hosts := range defaultHosts {
		if slices.Contains(hosts, target) {
			return true
		}
	}
	return false
}

// targetOf returns the field of a normalized config that decides where the worker sends requests, and its value.
func targetOf(cfg map[string]any) (field, raw string) {
	for _, f := range []string{"api_url", "base_url", "registry"} {
		if s, ok := cfg[f].(string); ok {
			return f, s
		}
	}
	return "", ""
}

// boxKeys only add requests to the same target: changing them is no retarget.
var boxKeys = []string{"new_issues", "new_prs"}

// sameTarget compares two normalized configs, the new-items boxes left out.
func sameTarget(a, b map[string]any) bool {
	strip := func(m map[string]any) map[string]any {
		out := maps.Clone(m)
		for _, k := range boxKeys {
			delete(out, k)
		}
		return out
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// comparableTarget is a target's host, non-default port and path.
// Security: only ASCII folds. unicode.ToLower maps U+0130 to i and U+212A to k, and the HTTP client would resolve that homoglyph elsewhere; a non-ASCII host compares as saved, with no IDNA.
func comparableTarget(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, u.Hostname()), ".")
	if port := u.Port(); port != "" && (u.Scheme != "https" || port != "443") && (u.Scheme != "http" || port != "80") {
		host = net.JoinHostPort(host, port)
	}
	return host + strings.TrimRight(u.EscapedPath(), "/")
}

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
	target := newConfig(kind)
	if target == nil {
		fe.base = "body.kind"
		fe.add("", "unknown kind, want one of "+strings.Join(Kinds, ", "), kind)
		return nil, fe.errs
	}
	if !decodeStrict(raw, target, fe) {
		return nil, fe.errs
	}
	switch c := target.(type) {
	case *GitHubRepoConfig:
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

func newConfig(kind string) any {
	switch kind {
	case KindGitHubRepo:
		return &GitHubRepoConfig{}
	case KindGitHubPR:
		return &GitHubPRConfig{}
	case KindGitLabRelease:
		return &GitLabReleaseConfig{}
	case KindGitLabMR:
		return &GitLabMRConfig{}
	case KindOCITags:
		return &OCITagsConfig{}
	}
	return nil
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// ConfigFields lists each kind's config fields, from the structs, for a reader with no schema of config to go on.
func ConfigFields() string {
	kinds := make([]string, 0, len(Kinds))
	for _, kind := range Kinds {
		t := reflect.TypeOf(newConfig(kind)).Elem()
		names := make([]string, 0, t.NumField())
		for i := range t.NumField() {
			names = append(names, jsonName(t.Field(i)))
		}
		kinds = append(kinds, kind+": "+strings.Join(names, ", "))
	}
	return strings.Join(kinds, "; ")
}

var typeNames = map[reflect.Kind]string{reflect.String: "a string", reflect.Bool: "true or false", reflect.Int: "an integer"}

// decodeStrict maps raw onto target, reporting each unknown key and each mistyped field.
func decodeStrict(raw map[string]any, target any, fe *fieldErrors) bool {
	known := map[string]bool{}
	t := reflect.TypeOf(target).Elem()
	for i := range t.NumField() {
		known[jsonName(t.Field(i))] = true
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
	// On the raw string: url.Parse reads an empty query or fragment as none, yet a trailing ? or # cuts the collector's own path.
	case strings.ContainsAny(raw, "?#"):
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
	// With :443 left on, registryBase reaches the web host docker.io, not registry-1.docker.io.
	switch strings.TrimSuffix(c.Registry, ":443") {
	case dockerHub, "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		c.Registry = dockerHub
	}
	if c.Registry == "" {
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
