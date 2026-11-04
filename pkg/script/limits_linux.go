package script

import (
	"bufio"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// addressHeadroom is the address space the child may map past what it holds at startup: the Go runtime
// reserves about 1.2 GiB of address space before main, so a fixed RLIMIT_AS would have to guess it.
const addressHeadroom = 512 << 20

// limitResources caps what the child can take; each limit is best effort, the watchdog and the parent's
// kill stay behind it. RLIMIT_NPROC is not set: it counts every thread of the user, the Go runtime
// included, and the script has no way to start a process anyway.
func limitResources() {
	// The race detector maps terabytes of shadow memory: an address space limit kills it at once.
	if !raceEnabled {
		if vm := vmSize(); vm > 0 {
			setLimit(syscall.RLIMIT_AS, vm+addressHeadroom)
		}
	}
	setLimit(syscall.RLIMIT_NOFILE, 16)
	setLimit(syscall.RLIMIT_FSIZE, 0)
	setLimit(syscall.RLIMIT_CORE, 0)
}

func setLimit(resource int, v uint64) {
	_ = syscall.Setrlimit(resource, &syscall.Rlimit{Cur: v, Max: v})
}

// vmSize is the process's current address space in bytes, from /proc/self/status; 0 if unknown.
func vmSize() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if rest, ok := strings.CutPrefix(s.Text(), "VmSize:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			if err != nil {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}

// limitCPU makes the kernel SIGKILL a CPU-bound child at the hard limit even if every Go timer is
// starved. CPU time counts every thread (the GC's too), so the budget is the self-exit deadline times
// GOMAXPROCS: a child busy on all its threads until its deadline still ends by its own clock.
func limitCPU(timeout time.Duration) {
	soft := uint64(float64(runtime.GOMAXPROCS(0)) * (math.Ceil(timeout.Seconds()) + math.Ceil(selfGrace.Seconds())))
	_ = syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: soft, Max: soft + 1})
}
