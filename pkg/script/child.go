package script

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/dop251/goja"
)

// Child memory: the GC works hard past softMemory; past hardMemory the watchdog ends the process.
const (
	softMemory = 128 << 20
	hardMemory = 192 << 20
	// maxCallStack bounds JS recursion, which goja would otherwise grow on the heap.
	maxCallStack = 1000
	exitMemory   = 3
	// exitStuck ends a child whose script did not stop when interrupted (a native call such as a
	// backtracking regexp never checks the interrupt).
	exitStuck = 4
	// exitGrace is how long an interrupted child may take to stop on its own before it exits.
	exitGrace = 200 * time.Millisecond
)

var (
	errTimeout    = errors.New("timeout")
	errParentGone = errors.New("parent gone")
)

// MaybeSandbox runs the sandbox and exits when the process was started as `<binary> sandbox`; main
// and the TestMain of packages that run scripts call it first.
func MaybeSandbox() {
	if len(os.Args) == 2 && os.Args[1] == Subcommand {
		os.Exit(Main())
	}
}

// Main is the `gedour sandbox` process: resource limits first, then one run over stdin/stdout.
func Main() int {
	limitResources()
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(softMemory)
	go watchdog()
	h := hooks{exit: os.Exit, onRun: limitRun, selfGrace: selfGrace, exitGrace: exitGrace}
	if err := serve(os.Stdin, os.Stdout, h); err != nil {
		return 2
	}
	return 0
}

// hooks are what a child process does that an in-process test must not: exit, set limits.
type hooks struct {
	// exit ends the process once an interrupt was not enough; nil in-process.
	exit func(int)
	// onRun sees the run's timeout before the script starts.
	onRun     func(timeout time.Duration)
	selfGrace time.Duration
	exitGrace time.Duration
}

// limitRun bounds what the kernel and the regexp engine let a run use, both final when Go-level
// timers are starved: a SIGKILL past the hard CPU limit (the Go runtime ignores the soft limit's
// SIGXCPU), a RangeError past the match timeout.
func limitRun(timeout time.Duration) {
	regexp2.DefaultMatchTimeout = timeout
	limitCPU(timeout)
}

// watchdog exits as soon as the heap passes hardMemory: a single huge allocation outruns any GC.
func watchdog() {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	for range time.Tick(2 * time.Millisecond) {
		metrics.Read(s)
		if s[0].Value.Uint64() > hardMemory {
			os.Exit(exitMemory)
		}
	}
}

// Serve runs the script of the first message read from in and reports on out. It holds no process
// state, so tests run it in-process.
func Serve(in io.Reader, out io.Writer) error {
	return serve(in, out, hooks{selfGrace: selfGrace, exitGrace: exitGrace})
}

func serve(in io.Reader, out io.Writer, h hooks) error {
	dec := json.NewDecoder(in)
	var run message
	if err := dec.Decode(&run); err != nil {
		return fmt.Errorf("sandbox: read run: %w", err)
	}
	if run.T != "run" || run.Input == nil {
		return fmt.Errorf("sandbox: expected a run message, got %q", run.T)
	}
	bw := bufio.NewWriter(out)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	timeout := time.Duration(run.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = Timeout
	}
	if h.onRun != nil {
		h.onRun(timeout)
	}
	c := &child{vm: goja.New(), bw: bw, enc: enc, replies: make(chan message, 1), halt: make(chan struct{}), timeout: timeout, hooks: h}
	go c.listen(dec)
	// The parent interrupts, then kills; this only covers a parent that died or stopped reading.
	stop := time.AfterFunc(timeout+h.selfGrace, func() { c.stop(errTimeout) })
	defer stop.Stop()
	m := c.run(run.Source, *run.Input)
	select {
	case <-c.halt:
		// Interrupted inside a native call (a fetch): the error it threw is the interrupt's.
		if m.Outcome == OutcomeError {
			m = c.timedOut()
		}
	default:
	}
	return c.send(m)
}

type child struct {
	vm       *goja.Runtime
	bw       *bufio.Writer
	enc      *json.Encoder
	replies  chan message
	halt     chan struct{}
	haltOnce sync.Once
	timeout  time.Duration
	hooks    hooks
	lines    int
	bytes    int
	cut      bool
	fetches  int
}

func (c *child) interrupt(why error) {
	c.vm.Interrupt(why)
	c.haltOnce.Do(func() { close(c.halt) })
}

// stop interrupts the script and, in a child process, exits shortly after whatever the script does:
// with the parent gone or past the deadline nobody else will end it.
func (c *child) stop(why error) {
	c.interrupt(why)
	if c.hooks.exit != nil {
		time.AfterFunc(c.hooks.exitGrace, func() { c.hooks.exit(exitStuck) })
	}
}

// listen reads the parent's messages until EOF: fetch replies and the interrupt.
func (c *child) listen(dec *json.Decoder) {
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			c.stop(errParentGone)
			return
		}
		switch m.T {
		case "interrupt":
			c.interrupt(errTimeout)
		case "fetch":
			// One fetch is outstanding at most; a late reply after an interrupt is dropped.
			select {
			case c.replies <- m:
			default:
			}
		}
	}
}

func (c *child) send(m message) error {
	if err := c.enc.Encode(m); err != nil {
		return fmt.Errorf("sandbox: write: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		return fmt.Errorf("sandbox: write: %w", err)
	}
	return nil
}

func done(outcome, errMsg, result string) message {
	return message{T: "done", Outcome: outcome, Error: errMsg, Result: result}
}

func (c *child) run(src string, in Input) message {
	vm := c.vm
	vm.SetMaxCallStackSize(maxCallStack)
	prog, err := goja.Compile("script.js", src, false)
	if err != nil {
		return done(OutcomeError, err.Error(), "")
	}
	if err := c.install(in); err != nil {
		return c.failure(err)
	}
	if _, err := vm.RunProgram(prog); err != nil {
		return c.failure(err)
	}
	main, ok := goja.AssertFunction(vm.Get("main"))
	if !ok {
		return done(OutcomeError, "the script must define function main(event, ctx)", "")
	}
	ctx := vm.NewObject()
	for _, k := range []string{"watch", "rule", "delivery_id"} {
		_ = ctx.Set(k, vm.Get(k))
	}
	v, err := main(goja.Undefined(), vm.Get("event"), ctx)
	if err != nil {
		return c.failure(err)
	}
	if p, ok := v.Export().(*goja.Promise); ok {
		switch p.State() {
		case goja.PromiseStateFulfilled:
			v = p.Result()
		case goja.PromiseStateRejected:
			return done(OutcomeError, "main rejected: "+p.Result().String(), "")
		default:
			return done(OutcomeError, "main returned a promise that never settled (there is no event loop)", "")
		}
	}
	out, err := c.stringify(v)
	if err != nil {
		return c.failure(fmt.Errorf("result: %w", err))
	}
	return done(OutcomeOK, "", out)
}

func (c *child) timedOut() message {
	return done(OutcomeTimeout, fmt.Sprintf("stopped after %s", c.timeout), "")
}

func (c *child) failure(err error) message {
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		return c.timedOut()
	}
	var so *goja.StackOverflowError
	if errors.As(err, &so) {
		return done(OutcomeError, fmt.Sprintf("RangeError: maximum call stack size exceeded (%d calls)%s", maxCallStack, so.Error()), "")
	}
	return done(OutcomeError, err.Error(), "")
}

// install sets the script's globals: plain JS values parsed from JSON, so no Go object is reachable.
func (c *child) install(in Input) error {
	vm := c.vm
	b, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("input: %w", err)
	}
	parse, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
	v, err := parse(goja.Undefined(), vm.ToValue(string(b)))
	if err != nil {
		return err
	}
	o := v.ToObject(vm)
	for _, k := range []string{"event", "watch", "rule", "delivery_id"} {
		if err := vm.Set(k, o.Get(k)); err != nil {
			return err
		}
	}
	if err := vm.Set("log", c.log); err != nil {
		return err
	}
	h := vm.NewObject()
	if err := h.Set("fetch", c.fetch); err != nil {
		return err
	}
	return vm.Set("http", h)
}

func (c *child) stringify(v goja.Value) (string, error) {
	if v == nil || goja.IsUndefined(v) {
		return "", nil
	}
	f, _ := goja.AssertFunction(c.vm.Get("JSON").ToObject(c.vm).Get("stringify"))
	s, err := f(goja.Undefined(), v)
	if err != nil {
		return "", err
	}
	if goja.IsUndefined(s) {
		return "", nil
	}
	return clip(s.String(), maxResult), nil
}

// clip cuts s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func (c *child) text(v goja.Value) string {
	if o, ok := v.(*goja.Object); ok && o.ClassName() != "Error" && o.ClassName() != "Function" {
		if s, err := c.stringify(o); err == nil && s != "" {
			return s
		}
	}
	if v == nil {
		return "undefined"
	}
	return v.String()
}

// log joins its arguments into one captured line; past 200 lines or 16 KiB the rest is dropped.
func (c *child) log(call goja.FunctionCall) goja.Value {
	if c.cut {
		return goja.Undefined()
	}
	parts := make([]string, len(call.Arguments))
	for i, a := range call.Arguments {
		parts[i] = c.text(a)
	}
	line := clip(strings.Join(parts, " "), maxLogBytes)
	if c.lines >= maxLogLines || c.bytes+len(line) > maxLogBytes {
		c.cut = true
		line = "[log truncated: at most 200 lines and 16 KiB]"
	}
	c.lines++
	c.bytes += len(line)
	if err := c.send(message{T: "log", Line: line}); err != nil {
		panic(c.vm.NewGoError(err))
	}
	return goja.Undefined()
}

func (c *child) throw(format string, a ...any) *goja.Object {
	o, err := c.vm.New(c.vm.Get("Error"), c.vm.ToValue(fmt.Sprintf(format, a...)))
	if err != nil {
		return c.vm.NewGoError(err)
	}
	return o
}

func defined(v goja.Value) bool {
	return v != nil && !goja.IsUndefined(v) && !goja.IsNull(v)
}

// fetch asks the parent to make the request and blocks until it answers (001-AC17).
func (c *child) fetch(call goja.FunctionCall) goja.Value {
	vm := c.vm
	if c.fetches >= MaxFetches {
		panic(c.throw("http.fetch: at most %d calls per run", MaxFetches))
	}
	c.fetches++
	req := FetchRequest{Method: "GET", URL: call.Argument(0).String(), Headers: map[string]string{}}
	if opts := call.Argument(1); defined(opts) {
		o := opts.ToObject(vm)
		if m := o.Get("method"); defined(m) {
			req.Method = strings.ToUpper(m.String())
		}
		if h := o.Get("headers"); defined(h) {
			ho := h.ToObject(vm)
			for _, k := range ho.Keys() {
				req.Headers[k] = ho.Get(k).String()
			}
		}
		if b := o.Get("body"); defined(b) {
			req.Body = b.String()
		}
	}
	if len(req.Body) > maxFetchBody {
		panic(c.throw("http.fetch: body over %d KiB", maxFetchBody>>10))
	}
	if err := c.send(message{T: "fetch", Req: &req}); err != nil {
		panic(c.vm.NewGoError(err))
	}
	select {
	case m := <-c.replies:
		if m.Error != "" || m.Res == nil {
			panic(c.throw("http.fetch: %s", m.Error))
		}
		res := vm.NewObject()
		hdr := vm.NewObject()
		for k, v := range m.Res.Headers {
			_ = hdr.Set(k, v)
		}
		_ = res.Set("status", m.Res.Status)
		_ = res.Set("ok", m.Res.Status >= 200 && m.Res.Status <= 299)
		_ = res.Set("headers", hdr)
		_ = res.Set("text", m.Res.Text)
		return res
	case <-c.halt:
		panic(c.throw("http.fetch: interrupted"))
	}
}
