package script

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Linux only: /proc, rlimits and F_SETPIPE_SZ.

// 004-AC8: a script reads both fields of a 64 KiB release, across the process boundary.
func TestScriptSeesNotes(t *testing.T) {
	r := testRunner(t, Options{Metrics: NewMetrics(prometheus.NewRegistry())})
	in := sampleInput()
	in.Event.Payload["body"] = strings.Repeat("n", 64<<10)
	in.Event.Payload["cves"] = []any{"CVE-2026-1"}
	res := r.Run(t.Context(), `function main(e) { return e.payload.body.length + " " + e.payload.cves[0] }`, in)
	if res.Outcome != OutcomeOK || res.Result != `"65536 CVE-2026-1"` {
		t.Fatalf("%+v", res)
	}
}

// 001-AC17: through the real subprocess: an empty environment, cwd /, and a result.
func TestSandboxProcess(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := testRunner(t, Options{Metrics: NewMetrics(reg)})
	var environ, cwd string
	r.onStart = func(pid int) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			t.Error(err)
		}
		environ = string(b)
		cwd, _ = os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	}
	t.Setenv("GEDOUR_ENCRYPTION_KEY", "must-not-leak")
	res := r.Run(t.Context(), `function main(e) { log("hi"); return e.type }`, sampleInput())
	if res.Outcome != OutcomeOK || res.Result != `"release.published"` || len(res.Logs) != 1 {
		t.Fatalf("run: %+v", res)
	}
	if environ != "" || cwd != "/" {
		t.Fatalf("child environ %q, cwd %q", environ, cwd)
	}
	if testutil.ToFloat64(r.o.Metrics.runs.WithLabelValues(OutcomeOK)) != 1 {
		t.Fatal("ok run not counted")
	}
}

// limitsOf reads soft and hard of each /proc/<pid>/limits line named in names, once they are all set
// (the CPU limit comes with the run message).
func limitsOf(t *testing.T, pid int) map[string][2]string {
	t.Helper()
	for range 200 {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
		if err != nil {
			t.Error(err)
			return nil
		}
		out := map[string][2]string{}
		for _, line := range strings.Split(string(b), "\n") {
			for _, name := range []string{"Max cpu time", "Max address space", "Max open files", "Max file size", "Max core file size"} {
				if rest, ok := strings.CutPrefix(line, name); ok {
					f := strings.Fields(rest)
					out[name] = [2]string{f[0], f[1]}
				}
			}
		}
		if out["Max cpu time"][0] != "unlimited" {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the child never set its CPU limit")
	return nil
}

// 001-AC17: the child caps its address space (not under the race detector), files, writes, core
// dumps and CPU time (GOMAXPROCS × (ceil(timeout) + 2 s) soft, + 1 s hard).
func TestSandboxRlimits(t *testing.T) {
	r := testRunner(t, Options{Timeout: 1500 * time.Millisecond})
	limits := make(chan map[string][2]string, 1)
	r.onStart = func(pid int) { go func() { limits <- limitsOf(t, pid) }() }
	if res := r.Run(t.Context(), `function main() { while (true) {} }`, sampleInput()); res.Outcome != OutcomeTimeout {
		t.Fatalf("%+v", res)
	}
	l := <-limits
	want := map[string][2]string{"Max cpu time": {"8", "9"}, "Max open files": {"16", "16"}, "Max file size": {"0", "0"}, "Max core file size": {"0", "0"}}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("%s: %v, want %v", k, l[k], v)
		}
	}
	as := l["Max address space"]
	if raceEnabled != (as[0] == "unlimited") || as[0] != as[1] {
		t.Errorf("address space %v (race detector %v)", as, raceEnabled)
	}
}

// startChild starts this binary as a sandbox child, outside the Runner: no interrupt, no kill, and a
// stdout nobody reads (a parent that stopped reading).
func startChild(t *testing.T) (*exec.Cmd, io.WriteCloser, *os.File) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, Subcommand) //nolint:gosec,noctx // the test binary itself
	cmd.Env = []string{}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, in, out.(*os.File)
}

// exitCode waits up to within for cmd; -1 when it is still running.
func exitCode(t *testing.T, cmd *exec.Cmd, within time.Duration) int {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		t.Logf("exited after %s: %v", time.Since(start), cmd.ProcessState)
		return cmd.ProcessState.ExitCode()
	case <-time.After(within):
		return -1
	}
}

// 001-AC17: a child its parent no longer drives ends on its own. Each subtest proves one net:
// stdin EOF (exit 4, the script stuck in catastrophic regexp backtracking), the regexp match timeout
// (the script gets a RangeError and the child ends normally), and the timeout+selfGrace self-exit
// (exit 4: a busy loop interrupted, then stuck writing its answer to a full stdout pipe).
func TestSandboxStuckChild(t *testing.T) {
	t.Run("eof exit", func(t *testing.T) {
		cmd, in, _ := startChild(t)
		if _, err := in.Write(runMessage(t, redos, 10*time.Second)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		_ = in.Close()
		if code := exitCode(t, cmd, 2*time.Second); code != exitStuck {
			t.Fatalf("exit code %d, want %d", code, exitStuck)
		}
	})
	t.Run("regexp match timeout", func(t *testing.T) {
		cmd, in, _ := startChild(t)
		if _, err := in.Write(runMessage(t, redos, time.Second)); err != nil {
			t.Fatal(err)
		}
		// Before the self-exit: the match itself gave up.
		if code := exitCode(t, cmd, time.Second+selfGrace); code != 0 {
			t.Fatalf("exit code %d, want 0", code)
		}
	})
	t.Run("deadline exit", func(t *testing.T) {
		cmd, in, out := startChild(t)
		// One page of pipe: the log below nearly fills it and the answer then cannot be written.
		size, _, errno := syscall.Syscall(syscall.SYS_FCNTL, out.Fd(), 1031 /* F_SETPIPE_SZ */, 4096)
		if errno != 0 || size != 4096 {
			t.Skipf("F_SETPIPE_SZ gave %d (%v), the log below must fill the pipe exactly", size, errno)
		}
		// {"t":"log","line":"…"}\n is 23 bytes plus the line: 4073 + 23 = 4096, a full pipe.
		src := `function main() { log("x".repeat(4073)); while (true) {} }`
		if _, err := in.Write(runMessage(t, src, time.Second)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if code := exitCode(t, cmd, time.Second+selfGrace+2*time.Second); code != exitStuck {
			t.Fatalf("exit code %d, want %d", code, exitStuck)
		}
		if d := time.Since(start); d < time.Second+selfGrace {
			t.Fatalf("exited after %s, before its deadline", d)
		}
	})
}
