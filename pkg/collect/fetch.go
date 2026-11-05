package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gedour/gedour/pkg/netguard"
)

// MaxBody caps one upstream response body.
const MaxBody = 10 << 20

// ErrNotModified reports a 304 on a conditional request: the upstream state did not change.
var ErrNotModified = errors.New("not modified")

// RateLimitError asks the scheduler to wait until Until before the next run (001-AC11).
type RateLimitError struct {
	Until time.Time
	URL   string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited by %s until %s", e.URL, e.Until.UTC().Format(time.RFC3339))
}

// HTTPError is a non-2xx upstream answer.
type HTTPError struct {
	Status int
	URL    string
	Header http.Header
	Body   string // first bytes, for the watch's last error
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Validators are the conditional-request headers remembered for one URL.
type Validators struct {
	ETag         string
	LastModified string
}

// Fetcher performs one run's requests: guarded client, response cap, rate-limit detection and
// conditional requests.
type Fetcher struct {
	Client    *http.Client
	UserAgent string
	// HTTPHosts may be reached over plain http; every other URL needs https.
	HTTPHosts []string
	// Cache holds last run's validators; nil disables conditional requests (no stored state).
	Cache map[string]Validators
	// Updates collects validators to store once the run succeeds.
	Updates map[string]Validators
	// Hosts lists the hosts this run contacted, in first-contact order, for the run log.
	Hosts []string
	Now   func() time.Time
	// Notes holds a release watch's notes by tag, for this run's events only; nil for other kinds.
	Notes map[string]string
}

type response struct {
	header http.Header
	body   []byte
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// get fetches rawURL; conditional sends the cached validators and returns ErrNotModified on 304.
func (f *Fetcher) get(ctx context.Context, rawURL string, hdr http.Header, conditional bool) (*response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if err := netguard.CheckScheme(u.Scheme, u.Hostname(), f.HTTPHosts); err != nil {
		return nil, err
	}
	f.addHost(u.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", f.UserAgent)
	if conditional {
		if v, ok := f.Cache[rawURL]; ok {
			if v.ETag != "" {
				req.Header.Set("If-None-Match", v.ETag)
			}
			if v.LastModified != "" {
				req.Header.Set("If-Modified-Since", v.LastModified)
			}
		}
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", rawURL, err)
	}
	if len(body) > MaxBody {
		return nil, fmt.Errorf("GET %s: response larger than %d MiB", rawURL, MaxBody>>20)
	}
	if until, ok := rateLimited(resp.StatusCode, resp.Header, f.now()); ok {
		return nil, &RateLimitError{Until: until, URL: rawURL}
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && conditional:
		return nil, ErrNotModified
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &HTTPError{Status: resp.StatusCode, URL: rawURL, Header: resp.Header, Body: snippet(body)}
	}
	if conditional {
		v := Validators{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
		if v != (Validators{}) {
			if f.Updates == nil {
				f.Updates = map[string]Validators{}
			}
			f.Updates[rawURL] = v
		}
	}
	return &response{header: resp.Header, body: body}, nil
}

func (f *Fetcher) addHost(h string) {
	if !slices.Contains(f.Hosts, h) {
		f.Hosts = append(f.Hosts, h)
	}
}

// client wraps the redirect policy so redirect targets land in Hosts too, refused ones included.
func (f *Fetcher) client() *http.Client {
	c := *f.Client
	next := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		f.addHost(req.URL.Host)
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 { // net/http's default policy
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

func (f *Fetcher) getJSON(ctx context.Context, rawURL string, hdr http.Header, conditional bool, out any) (*response, error) {
	r, err := f.get(ctx, rawURL, hdr, conditional)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", rawURL, err)
	}
	return r, nil
}

// snippet keeps an upstream error message short and on one line.
func snippet(b []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	s := string(b)
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		s = m.Message
	}
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\x00", "")), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// defaultRateLimitWait applies to a 429 that says nothing about when to come back.
const defaultRateLimitWait = time.Minute

// rateLimited reads the forge's rate-limit answer: Retry-After (seconds or HTTP date) on 403/429/503,
// GitHub's X-RateLimit-Remaining 0 + X-RateLimit-Reset on 403/429, GitLab's RateLimit-Reset on 429.
func rateLimited(status int, h http.Header, now time.Time) (time.Time, bool) {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return time.Time{}, false
	}
	if ra := h.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return now.Add(time.Duration(secs) * time.Second), true
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t, true
		}
	}
	if status == http.StatusServiceUnavailable {
		return time.Time{}, false
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if t, ok := unixHeader(h.Get("X-RateLimit-Reset")); ok {
			return t, true
		}
	}
	if status == http.StatusTooManyRequests {
		if t, ok := unixHeader(h.Get("RateLimit-Reset")); ok {
			return t, true
		}
		return now.Add(defaultRateLimitWait), true
	}
	return time.Time{}, false
}

func unixHeader(v string) (time.Time, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}
