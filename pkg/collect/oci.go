package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/watch"
)

const (
	maxTagPages = 20
	maxTags     = 20000
)

// TagState is the normalized state of an oci_tags watch.
type TagState struct {
	Tags  []string `json:"tags"` // semver order with semver_only, lexical otherwise
	Count int      `json:"count"`
	// Highest is the highest semver tag, "" when none parses.
	Highest string `json:"highest"`
}

// registryBase maps a registry name to its API origin: Docker Hub's API host is not docker.io.
func registryBase(registry string, httpHosts []string) string {
	host := registry
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	scheme := "https"
	if netguard.CheckScheme("http", name, httpHosts) == nil {
		scheme = "http"
	}
	return scheme + "://" + host
}

func collectOCITags(ctx context.Context, f *Fetcher, cfg watch.OCITagsConfig, cred *Credential) (TagState, error) {
	base := registryBase(cfg.Registry, f.HTTPHosts)
	next := fmt.Sprintf("%s/v2/%s/tags/list?n=1000", base, cfg.Repository)
	auth := "" // reused across pages: one token per run
	var all []string
	for page := 0; next != ""; page++ {
		if page == maxTagPages {
			return TagState{}, fmt.Errorf("more than %d pages of tags", maxTagPages)
		}
		r, err := f.get(ctx, next, authHeader(auth), false)
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusUnauthorized && auth == "" {
			if auth, err = registryAuth(ctx, f, he.Header.Get("Www-Authenticate"), cfg.Repository, cred); err != nil {
				return TagState{}, err
			}
			r, err = f.get(ctx, next, authHeader(auth), false)
		}
		if err != nil {
			return TagState{}, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil {
			return TagState{}, fmt.Errorf("decode tags: %w", err)
		}
		if all = append(all, body.Tags...); len(all) > maxTags {
			return TagState{}, fmt.Errorf("more than %d tags", maxTags)
		}
		if next, err = nextLink(next, r.header.Get("Link")); err != nil {
			return TagState{}, err
		}
	}
	return normalizeTags(all, cfg.TagRegex, cfg.SemverOnly)
}

func authHeader(v string) http.Header {
	if v == "" {
		return http.Header{}
	}
	return http.Header{"Authorization": {v}}
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// registryAuth answers a 401 challenge: Bearer goes through the token endpoint (anonymously, or with
// the registry_basic credential), Basic sends the credential directly.
func registryAuth(ctx context.Context, f *Fetcher, challenge, repo string, cred *Credential) (string, error) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if cred == nil {
			return "", errors.New("registry requires a login: attach a registry credential")
		}
		return "Basic " + basicToken(cred), nil
	case "bearer":
	default:
		return "", fmt.Errorf("registry answered 401 with an unsupported challenge %q", scheme)
	}
	params := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(rest, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("registry token realm %q is not a URL", params["realm"])
	}
	q := realm.Query()
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	h := http.Header{"Accept": {"application/json"}}
	if cred != nil {
		h.Set("Authorization", "Basic "+basicToken(cred))
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if _, err := f.getJSON(ctx, realm.String(), h, false, &tok); err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return "", errors.New("registry token endpoint returned no token")
	}
	return "Bearer " + tok.Token, nil
}

func basicToken(c *Credential) string {
	r := &http.Request{Header: http.Header{}}
	r.SetBasicAuth(c.Username, c.Secret)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

var (
	nextRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)
	lastRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?last"?`)
)

// nextLink resolves the Link rel="next" page; it must stay on the registry's host, which holds the token.
func nextLink(current, link string) (string, error) { return relLink(current, link, nextRe) }

// relLink resolves the Link page rel matches; it must stay on the current host, which holds the credential.
func relLink(current, link string, rel *regexp.Regexp) (string, error) {
	m := rel.FindStringSubmatch(link)
	if m == nil {
		return "", nil
	}
	cur, _ := url.Parse(current)
	ref, err := url.Parse(m[1])
	if err != nil {
		return "", fmt.Errorf("bad Link header: %w", err)
	}
	next := cur.ResolveReference(ref)
	if next.Host != cur.Host || next.Scheme != cur.Scheme {
		return "", fmt.Errorf("pagination left %s for %s", cur.Host, next.Host)
	}
	return next.String(), nil
}

func normalizeTags(tags []string, tagRegex string, semverOnly bool) (TagState, error) {
	re, err := regexp.Compile(tagRegex)
	if err != nil {
		return TagState{}, fmt.Errorf("tag_regex: %w", err)
	}
	s := TagState{Tags: []string{}}
	var highest *version
	for _, t := range slices.Compact(slices.Sorted(slices.Values(tags))) {
		if !re.MatchString(t) {
			continue
		}
		v, ok := parseSemver(t)
		if semverOnly && !ok {
			continue
		}
		if ok && (highest == nil || compareSemver(v, *highest) > 0 || (compareSemver(v, *highest) == 0 && t > s.Highest)) {
			highest, s.Highest = &v, t
		}
		s.Tags = append(s.Tags, t)
	}
	if semverOnly {
		slices.SortFunc(s.Tags, compareTags)
	}
	s.Count = len(s.Tags)
	return s, nil
}

// diffTags emits tag.created per new tag (flagging the one that is the new highest semver) and
// tag.removed per vanished tag.
func diffTags(old, cur TagState) []Event {
	had := map[string]bool{}
	for _, t := range old.Tags {
		had[t] = true
	}
	has := map[string]bool{}
	var events []Event
	for _, t := range cur.Tags {
		has[t] = true
		if !had[t] {
			highest := t == cur.Highest && cur.Highest != old.Highest
			events = append(events, Event{Type: "tag.created", Subject: t, Payload: map[string]any{"tag": t, "highest": highest}})
		}
	}
	for _, t := range old.Tags {
		if !has[t] {
			events = append(events, Event{Type: "tag.removed", Subject: t, Payload: map[string]any{"tag": t}})
		}
	}
	return events
}
