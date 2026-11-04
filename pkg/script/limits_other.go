//go:build !linux

package script

import "time"

// limitResources has no rlimits to set outside Linux: the watchdog and the parent's kill remain.
func limitResources() {}

func limitCPU(time.Duration) {}
