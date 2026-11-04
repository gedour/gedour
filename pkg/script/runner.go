package script

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/netguard"
)

// DefaultConcurrency is how many deliveries one process runs at once; interactive runs (dry runs,
// channel tests) have their own InteractiveSlots, so a burst of them never delays deliveries.
const (
	DefaultConcurrency = 4
	InteractiveSlots   = 2
)

// ErrBusy is an interactive run refused because every interactive slot is taken.
var ErrBusy = errors.New("every script sandbox for interactive runs is busy: retry in a few seconds")

// Options configure a Runner.
type Options struct {
	// Exe and Args start a child: this binary and Subcommand.
	Exe  string
	Args []string
	// Client performs real fetches: a netguard client that follows no redirect.
	Client *http.Client
	// Guard refuses literal private addresses before any request, so a dry run shows it too.
	Guard     *netguard.Guard
	HTTPHosts []string
	UserAgent string
	// Timeout is the wall clock of a run; Timeout when zero.
	Timeout time.Duration
	// Concurrency bounds the delivery children alive at once; DefaultConcurrency when zero.
	Concurrency int
	// Interactive bounds the dry-run and test children; InteractiveSlots when zero.
	Interactive int
	Metrics     *Metrics
}

// Runner starts one sandbox child per run.
type Runner struct {
	o           Options
	sem         chan struct{}
	interactive chan struct{}
	// onStart sees each child's PID; tests inspect the child through it.
	onStart func(pid int)
}

// NewRunner builds a runner.
func NewRunner(o Options) *Runner {
	if o.Timeout <= 0 {
		o.Timeout = Timeout
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Interactive <= 0 {
		o.Interactive = InteractiveSlots
	}
	return &Runner{o: o, sem: make(chan struct{}, o.Concurrency), interactive: make(chan struct{}, o.Interactive)}
}

// Fetch is one http.fetch call as recorded; header values that may be secrets are redacted.
type Fetch struct {
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	BodySize int               `json:"body_size" doc:"Request body length in bytes"`
}

// Result is the outcome of one run.
type Result struct {
	Outcome  string
	Error    string
	Logs     []string
	Result   string
	Fetches  []Fetch
	Duration time.Duration
}

// Err is nil for a run that returned; otherwise its error.
func (r Result) Err() error {
	if r.Outcome == OutcomeOK {
		return nil
	}
	return fmt.Errorf("script %s: %s", r.Outcome, r.Error)
}

// Run runs a delivery's src on in, waiting for a free delivery slot.
func (r *Runner) Run(ctx context.Context, src string, in Input) Result {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return Result{Outcome: OutcomeKilled, Error: "cancelled before start: " + ctx.Err().Error()}
	}
	defer func() { <-r.sem }()
	return r.timed(ctx, src, in, false)
}

// Interactive runs src for a waiting user, or answers ErrBusy at once. A dry run makes no request:
// http.fetch records the call and answers 200 with an empty body (001-AC18). ctx is the request's:
// a client gone kills the child.
func (r *Runner) Interactive(ctx context.Context, src string, in Input, dry bool) (Result, error) {
	release, ok := r.TryReserve()
	if !ok {
		return Result{}, ErrBusy
	}
	defer release()
	return r.RunReserved(ctx, src, in, dry), nil
}

// TryReserve takes an interactive slot without waiting, for a caller that must know before it commits
// to a run; release frees it (once, however often it is called).
func (r *Runner) TryReserve() (release func(), ok bool) {
	select {
	case r.interactive <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-r.interactive }) }, true
	default:
		return func() {}, false
	}
}

// RunReserved runs src on the interactive slot TryReserve gave; the caller releases it.
func (r *Runner) RunReserved(ctx context.Context, src string, in Input, dry bool) Result {
	return r.timed(ctx, src, in, dry)
}

func (r *Runner) timed(ctx context.Context, src string, in Input, dry bool) Result {
	start := time.Now()
	res := r.run(ctx, src, in, dry, start)
	res.Duration = time.Since(start)
	r.o.Metrics.observe(res)
	return res
}

type session struct {
	r        *Runner
	dry      bool
	deadline time.Time
	mu       sync.Mutex
	enc      *json.Encoder
	res      Result
	logBytes int
	fetches  int
	// expired: a fetch ran into the run's deadline, so the error the script then threw is the timeout's.
	expired bool
}

func (s *session) write(m message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(m)
}

func (r *Runner) run(ctx context.Context, src string, in Input, dry bool, start time.Time) Result {
	// An empty environment and / as cwd: the child holds no secret and no path of ours.
	cmd := exec.Command(r.o.Exe, r.o.Args...) //nolint:gosec,noctx // our own binary; the kill below bounds it
	cmd.Env = []string{}
	cmd.Dir = "/"
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{Outcome: OutcomeKilled, Error: "sandbox: " + err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Outcome: OutcomeKilled, Error: "sandbox: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return Result{Outcome: OutcomeKilled, Error: "sandbox did not start: " + err.Error()}
	}
	if r.onStart != nil {
		r.onStart(cmd.Process.Pid)
	}
	s := &session{r: r, dry: dry, deadline: start.Add(r.o.Timeout), enc: json.NewEncoder(stdin)}
	var timedOut atomic.Bool
	interrupt := time.AfterFunc(r.o.Timeout, func() {
		timedOut.Store(true)
		_ = s.write(message{T: "interrupt"})
	})
	defer interrupt.Stop()
	kill := time.AfterFunc(r.o.Timeout+killGrace, func() { _ = cmd.Process.Kill() })
	defer kill.Stop()
	stopCtx := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stopCtx()

	final, protocol := s.serve(ctx, src, in, stdout)
	_ = stdin.Close()
	// Done or not, the child has nothing left to say: it never outlives its answer.
	_ = cmd.Process.Kill()
	waitErr := cmd.Wait()

	res := s.res
	switch {
	case final != nil && final.Outcome == OutcomeError && (s.expired || timedOut.Load()):
		res.Outcome, res.Error = OutcomeTimeout, fmt.Sprintf("stopped after %s", r.o.Timeout)
	case final != nil && slices.Contains([]string{OutcomeOK, OutcomeError, OutcomeTimeout}, final.Outcome):
		res.Outcome, res.Error, res.Result = final.Outcome, clip(final.Error, maxErrorLen), clip(final.Result, maxResult)
	case timedOut.Load():
		res.Outcome, res.Error = OutcomeTimeout, fmt.Sprintf("stopped after %s", r.o.Timeout)
	case ctx.Err() != nil:
		res.Outcome, res.Error = OutcomeKilled, "cancelled: "+ctx.Err().Error()
	case protocol != "":
		res.Outcome, res.Error = OutcomeKilled, "sandbox protocol error: "+protocol
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == exitMemory:
		res.Outcome, res.Error = OutcomeKilled, fmt.Sprintf("killed: over the %d MiB memory limit", hardMemory>>20)
	default:
		res.Outcome, res.Error = OutcomeKilled, fmt.Sprintf("sandbox ended without an answer (%v): out of memory or crashed", waitErr)
	}
	return res
}

const maxErrorLen = 4 << 10

// serve sends the run and answers the child until it is done; protocol names a broken exchange.
func (s *session) serve(ctx context.Context, src string, in Input, stdout io.Reader) (final *message, protocol string) {
	if err := s.write(message{T: "run", Source: src, Input: &in, TimeoutMS: s.r.o.Timeout.Milliseconds()}); err != nil {
		return nil, ""
	}
	br := bufio.NewReaderSize(stdout, maxMessage)
	total := 0
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Sprintf("a message over %d MiB", maxMessage>>20)
		}
		if err != nil {
			return nil, ""
		}
		if total += len(line); total > maxOutput {
			return nil, fmt.Sprintf("output over %d MiB", maxOutput>>20)
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, "malformed message"
		}
		switch m.T {
		case "log":
			s.log(m.Line)
		case "fetch":
			reply := message{T: "fetch"}
			if res, err := s.fetch(ctx, m.Req); err != nil {
				reply.Error = err.Error()
			} else {
				reply.Res = res
			}
			if err := s.write(reply); err != nil {
				return nil, ""
			}
		case "done":
			return &m, ""
		default:
			return nil, fmt.Sprintf("unknown message %q", clip(m.T, 32))
		}
	}
}

// log keeps the child's own caps (200 lines, 16 KiB, plus its truncation notice) whatever it sends.
func (s *session) log(line string) {
	line = clip(line, maxLogBytes)
	if len(s.res.Logs) > maxLogLines || s.logBytes+len(line) > maxLogBytes+64 {
		return
	}
	s.logBytes += len(line)
	s.res.Logs = append(s.res.Logs, line)
}

var allowedMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// refusedHeaders are set by the transport, or would make the request mean something else.
var refusedHeaders = []string{"host", "content-length", "connection", "keep-alive", "proxy-connection",
	"proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade"}

const maxHeaders = 50

// checkFetch refuses what can be refused before any request; the dial-time guard is the real SSRF check.
func (s *session) checkFetch(req *FetchRequest) (*url.URL, error) {
	if !slices.Contains(allowedMethods, req.Method) {
		return nil, fmt.Errorf("method %q: use one of %s", clip(req.Method, 16), strings.Join(allowedMethods, ", "))
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("an absolute http(s) URL is required")
	}
	if err := netguard.CheckScheme(u.Scheme, u.Hostname(), s.r.o.HTTPHosts); err != nil {
		return nil, err
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && s.r.o.Guard != nil && !s.r.o.Guard.Allowed(ip) {
		return nil, fmt.Errorf("%w: %s is a private, loopback or reserved address", netguard.ErrBlocked, ip)
	}
	if len(req.Headers) > maxHeaders {
		return nil, fmt.Errorf("at most %d headers", maxHeaders)
	}
	for k := range req.Headers {
		if slices.Contains(refusedHeaders, strings.ToLower(k)) {
			return nil, fmt.Errorf("header %q is set by gedour, not by the script", clip(k, 64))
		}
	}
	if len(req.Body) > maxFetchBody {
		return nil, fmt.Errorf("body over %d KiB", maxFetchBody>>10)
	}
	return u, nil
}

// secretish names have their value replaced in the recorded fetches: the audit log's rule, plus
// URL signatures and sessions.
func secretish(name string) bool {
	n := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
	return audit.IsSecretKey(name) || n == "sig" || strings.Contains(n, "signature") || strings.Contains(n, "session")
}

func record(req *FetchRequest) Fetch {
	h := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		if secretish(k) {
			v = audit.Redacted
		}
		h[k] = v
	}
	return Fetch{Method: req.Method, URL: redactURL(req.URL), Headers: h, BodySize: len(req.Body)}
}

// redactURL hides the URL's userinfo (a token alone as user, or the password) and the values of
// secret-looking keys in the query and in a key=value fragment (OAuth implicit-flow tokens).
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return raw
	}
	u.RawQuery = redactPairs(u.RawQuery)
	frag := redactPairs(u.EscapedFragment())
	u.Fragment, u.RawFragment = "", ""
	info := ""
	if u.User != nil {
		info = audit.Redacted
		if _, has := u.User.Password(); has {
			info = u.User.Username() + ":" + audit.Redacted
		}
		u.User = nil
	}
	out := u.String()
	if info != "" {
		out = strings.Replace(out, "//", "//"+info+"@", 1)
	}
	if frag != "" {
		out += "#" + frag
	}
	return out
}

// redactPairs replaces the value of each secret-looking key of an &-separated key=value list.
func redactPairs(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		k, _, _ := strings.Cut(p, "=")
		name, err := url.QueryUnescape(k)
		if err != nil {
			name = k
		}
		if secretish(name) {
			parts[i] = k + "=" + audit.Redacted
		}
	}
	return strings.Join(parts, "&")
}

func (s *session) fetch(ctx context.Context, req *FetchRequest) (*FetchResponse, error) {
	if req == nil {
		return nil, errors.New("no request")
	}
	if s.fetches++; s.fetches > MaxFetches {
		return nil, fmt.Errorf("at most %d calls per run", MaxFetches)
	}
	u, err := s.checkFetch(req)
	if err != nil {
		return nil, err
	}
	s.res.Fetches = append(s.res.Fetches, record(req))
	if s.dry {
		return &FetchResponse{Status: http.StatusOK, Headers: map[string]string{}}, nil
	}
	ctx, cancel := context.WithDeadline(ctx, s.deadline)
	defer cancel()
	ctx, cancelFetch := context.WithTimeout(ctx, fetchTimeout)
	defer cancelFetch()
	hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), strings.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	if hr.Header.Get("User-Agent") == "" {
		hr.Header.Set("User-Agent", s.r.o.UserAgent)
	}
	resp, err := s.r.o.Client.Do(hr)
	if err != nil && !time.Now().Before(s.deadline) {
		s.expired = true
	}
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s %s: %w", req.Method, u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read: %w", req.Method, u.Host, err)
	}
	if len(body) > maxResponse {
		return nil, fmt.Errorf("%s %s: response over %d MiB", req.Method, u.Host, maxResponse>>20)
	}
	hdr := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		hdr[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return &FetchResponse{Status: resp.StatusCode, Headers: hdr, Text: string(body)}, nil
}

// Metrics count script runs by outcome (001-AC31's per-channel view lives in the delivery metrics).
type Metrics struct {
	runs     *prometheus.CounterVec
	duration prometheus.Histogram
}

// NewMetrics registers the script metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gedour_script_runs_total",
			Help: "Script runs (deliveries, tests and dry runs) by outcome: ok, error (threw), timeout, killed (memory, crash, protocol).",
		}, []string{"outcome"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gedour_script_run_duration_seconds",
			Help:    "Wall clock of script runs, sandbox start included.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
	}
	for _, o := range []string{OutcomeOK, OutcomeError, OutcomeTimeout, OutcomeKilled} {
		m.runs.WithLabelValues(o)
	}
	reg.MustRegister(m.runs, m.duration)
	return m
}

func (m *Metrics) observe(r Result) {
	if m == nil {
		return
	}
	m.runs.WithLabelValues(r.Outcome).Inc()
	m.duration.Observe(r.Duration.Seconds())
}
