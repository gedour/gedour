package collect

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
)

// Lenient semver, as registries tag: "v1.2.3", "18.1", "17" and "18.1-alpine" (prerelease "alpine")
// all parse. A small parser over Masterminds/semver: same leniency, no new dependency.
var semverRe = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

type version struct {
	nums [3]uint64
	pre  []string
}

func parseSemver(s string) (version, bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range 3 {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return version{}, false
		}
		v.nums[i] = n
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

// compareSemver orders by semver precedence: a prerelease sorts before its release, numeric
// identifiers numerically and before alphanumeric ones.
func compareSemver(a, b version) int {
	for i := range 3 {
		if c := cmp.Compare(a.nums[i], b.nums[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, xerr := strconv.ParseUint(a.pre[i], 10, 64)
		y, yerr := strconv.ParseUint(b.pre[i], 10, 64)
		var c int
		switch {
		case xerr == nil && yerr == nil:
			c = cmp.Compare(x, y)
		case xerr == nil:
			c = -1
		case yerr == nil:
			c = 1
		default:
			c = strings.Compare(a.pre[i], b.pre[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a.pre), len(b.pre))
}

// compareTags orders semver tags by precedence, the raw string breaking ties ("18" vs "v18").
func compareTags(a, b string) int {
	va, _ := parseSemver(a)
	vb, _ := parseSemver(b)
	if c := compareSemver(va, vb); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// SemverGreater reports whether a has a higher semver precedence than b; ok is false when either
// does not parse.
func SemverGreater(a, b string) (greater, ok bool) {
	va, okA := parseSemver(a)
	vb, okB := parseSemver(b)
	if !okA || !okB {
		return false, false
	}
	return compareSemver(va, vb) > 0, true
}
