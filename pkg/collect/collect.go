// Package collect fetches the upstream state of each watch kind and diffs two states into events.
//
// Every kind splits into a fetch (HTTP, through Fetcher) and pure normalize/diff functions that the
// fixture tests drive directly (001-AC7).
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gedour/gedour/pkg/watch"
)

// Credential is a decrypted credential, alive for one run only.
type Credential struct {
	Kind     string
	Username string
	Secret   string `json:"-"`
}

// String keeps the secret out of logs and error messages.
func (c *Credential) String() string { return "credential(" + c.Kind + ", redacted)" }

// Event is one change found by Diff. Subject identifies it within the diff of one state pair; the
// worker prefixes the type and suffixes the previous state ID to make the stored dedup key.
type Event struct {
	Type       string
	Subject    string
	OccurredAt *time.Time
	Payload    map[string]any
}

// Collector is one watch kind. States cross it as JSON: the stored form is the contract.
type Collector interface {
	// Collect fetches the current normalized state; ErrNotModified means unchanged since the last run.
	Collect(ctx context.Context, f *Fetcher, cfg json.RawMessage, cred *Credential) (json.RawMessage, error)
	// Diff lists the events that lead from old to new, in a stable order.
	Diff(old, new json.RawMessage) ([]Event, error)
}

// kind adapts typed collect and diff functions to Collector.
type kind[C, S any] struct {
	collect func(ctx context.Context, f *Fetcher, cfg C, cred *Credential) (S, error)
	diff    func(old, new S) []Event
}

func (k kind[C, S]) Collect(ctx context.Context, f *Fetcher, raw json.RawMessage, cred *Credential) (json.RawMessage, error) {
	var cfg C
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	s, err := k.collect(ctx, f, cfg, cred)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	return b, nil
}

func (k kind[C, S]) Diff(old, new json.RawMessage) ([]Event, error) {
	var o, n S
	if err := json.Unmarshal(old, &o); err != nil {
		return nil, fmt.Errorf("decode old state: %w", err)
	}
	if err := json.Unmarshal(new, &n); err != nil {
		return nil, fmt.Errorf("decode new state: %w", err)
	}
	return k.diff(o, n), nil
}

// Collectors returns the collector of every watch kind.
func Collectors() map[string]Collector {
	return map[string]Collector{
		watch.KindGitHubRepo:    kind[watch.GitHubRepoConfig, ReleaseState]{collectGitHubReleases, diffReleases},
		watch.KindGitHubPR:      kind[watch.GitHubPRConfig, PRState]{collectGitHubPR, diffPR},
		watch.KindGitLabRelease: kind[watch.GitLabReleaseConfig, ReleaseState]{collectGitLabReleases, diffReleases},
		watch.KindGitLabMR:      kind[watch.GitLabMRConfig, MRState]{collectGitLabMR, diffMR},
		watch.KindOCITags:       kind[watch.OCITagsConfig, TagState]{collectOCITags, diffTags},
	}
}

// StripNUL removes NUL from every string (and key) of a JSON document: PostgreSQL's jsonb refuses
// \u0000, and one upstream field carrying it would otherwise fail every run of the watch.
func StripNUL(doc json.RawMessage) (json.RawMessage, error) {
	if !bytes.Contains(doc, []byte(`\u0000`)) {
		return doc, nil
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	b, err := json.Marshal(stripNUL(v))
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	return b, nil
}

func stripNUL(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "\x00", "")
	case []any:
		for i := range t {
			t[i] = stripNUL(t[i])
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[strings.ReplaceAll(k, "\x00", "")] = stripNUL(x)
		}
		return out
	}
	return v
}
