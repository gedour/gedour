package filter

import (
	"cmp"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// spreadPayload spends the whole byte budget on MaxItems short multibyte strings, the shape that
// makes per-item string work (case mapping above all) heaviest.
func spreadPayload() map[string]any {
	items := make([]any, MaxItems)
	for i := range items {
		items[i] = strings.Repeat("éǅ", maxRest/MaxItems/4)
	}
	return Sanitize(map[string]any{"atag": "v2.104.0", "items": items})
}

// pairsPayload is the adversary of two-string builtins: pairs whose both strings are long, multibyte
// and payload-controlled, e.g. a string made only of the last rune of a long cutset.
func pairsPayload() map[string]any {
	// Eight pairs of two 4 KiB strings spend the byte budget on the longest strings and cutsets.
	pairs := make([]any, 8)
	for i := range pairs {
		pairs[i] = map[string]any{"s": strings.Repeat("ɩ", MaxString/2), "c": strings.Repeat("é", MaxString/2-1) + "ɩ"}
	}
	return Sanitize(map[string]any{"pairs": pairs})
}

var payloads = map[string]func() map[string]any{"max": maxPayload, "spread": spreadPayload, "pairs": pairsPayload}

// fit is build(n) for the largest n whose cost is within the budget: the worst filter of that shape.
func fit(t *testing.T, build func(n int) string) string {
	t.Helper()
	best := ""
	for n := 1; n <= 200; n++ {
		src := build(n)
		c, err := Cost(src)
		if err != nil || c > CostBudget {
			break
		}
		best = src
	}
	if best == "" {
		t.Fatalf("no n fits the budget for %.60q", build(1))
	}
	return best
}

func chain(n int, sep string, part func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = part(i)
	}
	return strings.Join(parts, sep)
}

var regex64 = []string{`[0-9a]{1,8}[.a][0-9a]{1,8}[.a][0-9a]{1,8}b`, `(foo|bar|a)[0-9a]{1,12}-?[a-f]{8}z`, `[a-c]{20}[a-f]{20}z{10}`}

// allowedShapes are the worst filters of each kind the model lets through.
func allowedShapes(t *testing.T) []string {
	const items = "event.payload.items"
	return append(bodyShapes(t), []string{
		fit(t, func(n int) string {
			return "any(" + items + ", {" + chain(n, " || ", func(int) string { return `# contains "zq" || # startsWith "zq"` }) + "})"
		}),
		// The reviewer's: case mapping chained on every item, up to the budget.
		fit(t, func(n int) string {
			return "any(" + items + ", {" + chain(n, " || ", func(int) string { return `lower(upper(#)) == "zq"` }) + "})"
		}),
		fit(t, func(n int) string {
			return chain(n, " + ", func(int) string { return "count(filter(" + items + `, {lower(#) == upper(#)}), {# != ""})` }) + " < 0"
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(i int) string { return fmt.Sprintf(`any(%s, {lower(#) startsWith "zq%d"})`, items, i) })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return "all(" + items + `, {semver_gt(#, "v1") == true})` })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(i int) string { return fmt.Sprintf(`lower(%s[%d]) contains "zq"`, items, i%15) })
		}),
		fit(t, func(n int) string {
			return "any(" + items + ", {" + chain(n, " || ", func(int) string { return `date("2024-01-02") == now()` }) + "})"
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return `event.payload != event.payload` })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return `"zq" in event.payload.items` })
		}),
		fit(t, func(n int) string {
			return "count(" + items + ", {" + chain(n, " && ", func(int) string { return `len(#) > 3 && len(trim(#)) < 2` }) + "}) > 0"
		}),
		`event.payload.items[0] matches "` + regex64[0] + `" || event.payload.items[1] matches "` + regex64[1] +
			`" || event.payload.items[2] matches "` + regex64[2] + `"`,
		// Literal cutsets: a byte set for trim, one comparison for trimPrefix/trimSuffix.
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string {
				return `any(event.payload.pairs, {trim(#.s, "-_. /:=+@#~!?*%") == "zq" || trimPrefix(#.c, "éé") == "zq"})`
			})
		}),
	}...)
}

// refusedShapes each pass every structural cap but the one named (or the budget), so lifting that
// cap lets them compile and turns the timing tests red.
var refusedShapes = map[string]string{
	// The reviewer's: case mapping on every item beyond the budget.
	`any(event.payload.items, {` + chain(25, " || ", func(int) string { return `lower(upper(trim(#))) == "zz"` }) + `})`: "costs",
	`count(event.payload.items, {` + chain(20, " || ", func(int) string { return `lower(#) == upper(#)` }) + `}) > 0`:    "costs",
	// Outer references in a predicate: per item, work on the whole payload.
	`any(event.payload.items, {` + chain(9, " || ", func(int) string { return `lower(upper(event.payload.body)) == ""` }) + `})`:                  "event inside a predicate",
	`any(event.payload.items, {` + chain(12, " || ", func(int) string { return `upper(event.payload.body) == lower(event.payload.body)` }) + `})`: "event inside a predicate",
	// A time zone loaded from disk on every call.
	`any(event.payload.items, {` + chain(18, " || ", func(int) string { return `date("2024", "2006", "Europe/Paris") == now()` }) + `})`: "date() with a time zone",
	// Regular expressions per item, within the count and size caps.
	`any(event.payload.items, {# matches "` + regex64[0] + `" || # matches "` + regex64[1] + `" || # matches "` + regex64[2] + `"})`: "matches inside a predicate",
	// More regular expressions than the count cap, each within the size cap.
	chain(20, " || ", func(i int) string {
		return fmt.Sprintf(`event.payload.items[%d] matches "%s"`, i%15, regex64[i%3])
	}): "more than 3 matches",
	// Only the instruction cap stops this one: every repetition is within 32.
	`event.payload.items[2] matches "` + strings.Repeat("[^x]{32}", 60) + `x" || event.payload.items[3] matches "` + strings.Repeat("[^x]{32}", 60) + `x"`: "a pattern above 64 instructions",
	// A cutset from the payload: every rune tested against every cutset rune, per pair.
	chain(5, " || ", func(int) string { return `any(event.payload.pairs, {trim(#.s, #.c) == "x"})` }): "trim() with a computed",
	`any(event.payload.pairs, {trim(#.s, "éɩ") == "x"})`:                                              "trim() with a computed or non-ASCII",
	// A pattern past the budget on body, within the instruction cap on any other field.
	`event.payload.body matches "` + regex64[0] + `"`: "costs",
	// Several caps catch these; the first one walked wins.
	`any(event.payload.items, {event.payload.body matches "[^x]{1000}x"})`:                          "matches inside a predicate",
	`event.payload.body matches "[^x]{1000}x"`:                                                      "a repetition count above 32",
	`any(event.payload.items, {any(event.payload.items, {toJSON(event.payload) contains "zzzz"})})`: "any() inside another predicate",
}

// compiled is every shape that compiles: all allowed ones (an allowed one refused fails the test),
// and any refused one whose cap was lifted, so it is timed like the others.
func compiled(t *testing.T) (map[string]*Program, []string) {
	t.Helper()
	allowed := allowedShapes(t)
	out := map[string]*Program{}
	for _, src := range allowed {
		p, err := Compile(src)
		if err != nil {
			t.Fatalf("%.70q: refused: %v", src, err)
		}
		out[src] = p
	}
	for src, why := range refusedShapes {
		p, err := Compile(src)
		if err != nil {
			if !strings.Contains(err.Error(), why) {
				t.Errorf("%.70q: refused for %v, want %q", src, err, why)
			}
			continue
		}
		out[src] = p
	}
	return out, allowed
}

// minTime is the fastest of three runs: the cost of the filter, not of a scheduler hiccup.
func minTime(p *Program, env Env, runs int) (time.Duration, error) {
	best := time.Duration(1 << 62)
	var err error
	for range runs {
		start := time.Now()
		_, err = Match(p, env)
		best = min(best, time.Since(start))
	}
	return best, err
}

// Each allowed shape stays under the gate, 2.5 times the 45 ms measured, on every worst payload (004-AC10). The
// cost model is the deterministic guard.
func TestWorstCaseTiming(t *testing.T) {
	progs, allowed := compiled(t)
	for name, payload := range allPayloads() {
		env := sample
		env.Event.Payload = payload()
		var slowest time.Duration
		var slowestSrc string
		for src, p := range progs {
			took, err := minTime(p, env, 3)
			if gate := filterGate * raceSlowdown; took > gate {
				t.Errorf("%s payload: %.70q took %s, past %s", name, src, took, gate)
			}
			// Each payload is the worst case of the shapes reading its root array.
			own := strings.Contains(src, "pairs") == (name == "pairs") && name != "max" && (payloads[name] == nil || !strings.Contains(src, "payload.body") && !strings.Contains(src, "event.type]"))
			if err != nil && slices.Contains(allowed, src) && own {
				t.Errorf("%s payload: %.70q: %v (an error stops the walk early: not a worst case)", name, src, err)
			}
			if slices.Contains(allowed, src) && took > slowest {
				slowest, slowestSrc = took, src
			}
		}
		t.Logf("%s payload: slowest allowed %s: %.80s", name, slowest, slowestSrc)
	}
}

// One user's whole rule set (100 rules) of the same worst filter takes about 4.5 s measured, gated at 2.5 times that,
// on every payload (004-AC10). That spends the router's 2 s batch budget (route.routeBudget), checked between
// events, so such an event ends its batch. Only the slowest shapes of each payload run 100 times: a rule set is
// its filter's time, and the others are far below.
func TestRuleSetBound(t *testing.T) {
	if raceSlowdown > 1 {
		t.Skip("minutes under the race detector; CI runs it without")
	}
	progs, _ := compiled(t)
	for name, payload := range allPayloads() {
		env := sample
		env.Event.Payload = payload()
		srcs := slices.Collect(maps.Keys(progs))
		times := map[string]time.Duration{}
		for _, src := range srcs {
			times[src], _ = minTime(progs[src], env, 1)
		}
		slices.SortFunc(srcs, func(a, b string) int { return cmp.Compare(times[b], times[a]) })
		var slowest time.Duration
		for _, src := range srcs[:3] {
			start := time.Now()
			for range 100 {
				_, _ = Match(progs[src], env)
			}
			took := time.Since(start)
			slowest = max(slowest, took)
			if gate := ruleSetGate * raceSlowdown; took > gate {
				t.Errorf("%s payload: 100 × %.60q took %s, past %s", name, src, took, gate)
			}
		}
		t.Logf("%s payload: slowest rule set %s", name, slowest)
	}
}

// Random allowed filters close to the budget stay under the gate on every payload. CI also runs this without
// the race detector, for the whole filter count and the strict bound.
func TestCostModelProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 2026)) //nolint:gosec // seeded on purpose: the same filters every run
	itemAtoms := []string{`lower(#) contains "zq"`, `upper(#) == "ZQ"`, `trim(#) startsWith "zq"`, `# endsWith "zq"`,
		`len(#) > 100000`, `string(#) != #`, `trimPrefix(#, "é") == "zq"`, `lower(upper(#)) == "zq"`, `semver_gt(#, "v1")`,
		`date("2024-01-02") > now()`, `int(len(#)) < 0`}
	topAtoms := []string{`event.payload.body contains "zq"`, `lower(event.payload.body) contains "zq"`,
		`event.payload[event.type] contains "zq"`, `event.payload.body == string(event.payload.body)`, `event.type == "zq"`, `lower(event.payload.atag) contains "zq"`, `event.payload.items[0] startsWith "zq"`,
		`len(event.payload.items) > 1000`, `event.occurred_at > now() - duration("1h")`, `semver_gt(event.payload.atag, "v9")`,
		`"zq" in event.payload.items`, `event.payload == event.payload && false`, `upper(trim(event.payload.items[1])) == "ZQ"`}
	preds := []string{"any", "none", "count", "filter", "one"}
	component := func() string {
		if r.IntN(3) == 0 {
			return topAtoms[r.IntN(len(topAtoms))]
		}
		body := chain(1+r.IntN(4), " || ", func(int) string { return itemAtoms[r.IntN(len(itemAtoms))] })
		switch p := preds[r.IntN(len(preds))]; p {
		case "count":
			return "count(event.payload.items, {" + body + "}) > 1000"
		case "filter":
			return "len(filter(event.payload.items, {" + body + "})) > 1000"
		default:
			return p + "(event.payload.items, {" + body + "})"
		}
	}
	envs := map[string]Env{}
	for name, payload := range allPayloads() {
		env := sample
		env.Event.Payload = payload()
		envs[name] = env
	}
	checked := 0
	for checked < 200/raceSlowdown {
		// Grow the filter until the next component would cross the budget.
		src := component()
		for {
			next := src + " || " + component()
			if c, err := Cost(next); err != nil || c > CostBudget {
				break
			}
			src = next
		}
		c, err := Cost(src)
		if err != nil || c > CostBudget || c < CostBudget/4 {
			continue
		}
		p, err := Compile(src)
		if err != nil {
			t.Fatalf("%q: cost %d but %v", src, c, err)
		}
		for name, env := range envs {
			if took, _ := minTime(p, env, 5); took > filterGate*raceSlowdown {
				t.Errorf("%s payload: cost %d, %s: %.100q", name, c, took, src)
			}
		}
		checked++
	}
}

// The model's values, exactly: the deterministic guard, independent of the machine running it.
func TestCostTable(t *testing.T) {
	for _, tt := range []struct {
		src  string
		cost int64
	}{
		// ident 32, property "type" 32, member 32, literal 32, == 32 + the shorter side (1 byte).
		{`event.type == "a"`, 161},
		// contains: both sides' bytes, the field bounded at MaxString.
		{`event.type contains "ab"`, 32*5 + MaxString + 2},
		// Case mapping: 3 units per byte.
		{`lower(event.type) == ""`, 32*6 + caseFactor*MaxString},
		// On #, the bytes of all items are paid once (lower: 3 × 64 KiB); the 4 body nodes and the
		// iteration are paid per item (200 × 32 each); the array path costs 5 nodes.
		{`any(event.payload.items, {lower(#) == ""})`, 5*32 + 4*32*MaxItems + caseFactor*maxRest + 32 + 32*MaxItems},
		// 004-AC9: body at 64 KiB, the same field shape at 4 KiB.
		{`event.payload.body contains "ab"`, 32*7 + MaxBody + 2},
		{`lower(event.payload.body) == ""`, 32*8 + caseFactor*MaxBody},
	} {
		got, err := Cost(tt.src)
		if err != nil {
			t.Fatalf("%q: %v", tt.src, err)
		}
		if tt.cost != 0 && got != tt.cost {
			t.Errorf("%q: cost %d, want %d", tt.src, got, tt.cost)
		}
		t.Logf("%8d  %s", got, tt.src)
	}
}

// The reference the editor shows is the allowlist Compile enforces: every construct it claims has an
// example here, and the example compiles.
func TestReference(t *testing.T) {
	r := Reference()
	ops := map[string]string{
		"==": `event.type == "a"`, "!=": `event.type != "a"`, "<": `len(event.type) < 2`, "<=": `len(event.type) <= 2`,
		">": `len(event.type) > 2`, ">=": `len(event.type) >= 2`, "and": `true and false`, "or": `true or false`,
		"&&": `true && false`, "||": `true || false`, "in": `event.type in ["a"]`, "contains": `event.type contains "a"`,
		"startsWith": `event.type startsWith "a"`, "endsWith": `event.type endsWith "a"`, "matches": `event.type matches "^a"`,
		"+": `len(event.type) + 1 > 0`, "-": `now() - duration("1h") < now()`, "..": `any(1..3, {# == 2})`,
		"?:": `(true ? 1 : 2) == 1`, "not": `not true`, "!": `!true`,
	}
	builtins := map[string]string{
		"len": `len(event.type) > 0`, "lower": `lower(event.type) == ""`, "upper": `upper(event.type) == ""`,
		"trim": `trim(event.type, "-_") == ""`, "trimPrefix": `trimPrefix(event.type, "a") == ""`, "trimSuffix": `trimSuffix(event.type, "a") == ""`,
		"int": `int("1") == 1`, "float": `float("1.5") > 1`, "abs": `abs(-1) == 1`, "now": `now() > event.occurred_at`,
		"duration": `duration("1h") > duration("1m")`, "date": `date("2024-01-02", "2006-01-02") < now()`,
		"semver_gt": `semver_gt(event.payload.tag, "v1")`, "string": `string(event.payload.n) == "1"`,
	}
	preds := map[string]string{
		"any": `any(event.payload.items, {# == "a"})`, "all": `all(event.payload.items, {#.name != ""})`,
		"none": `none(event.payload.items, {# == "a"})`, "one": `one(event.payload.items, {# == "a"})`,
		"filter": `len(filter(event.payload.items, {# == "a"})) > 0`, "count": `count(event.payload.items, {# == "a"}) > 0`,
	}
	for _, tt := range []struct {
		claimed  []string
		examples map[string]string
	}{{r.Operators, ops}, {r.Builtins, builtins}, {r.Predicates, preds}} {
		for _, name := range tt.claimed {
			src, ok := tt.examples[name]
			if !ok {
				t.Errorf("%q is in the reference with no example here", name)
				continue
			}
			if _, err := Compile(src); err != nil {
				t.Errorf("%q: its example %q does not compile: %v", name, src, err)
			}
		}
	}
	for name := range scalarBuiltins {
		if !slices.Contains(r.Builtins, name) || !strings.Contains(r.Summary, name) {
			t.Errorf("builtin %s missing from the reference", name)
		}
	}
	if strings.Contains(strings.Join(r.Rules, " "), "#index") {
		t.Error("the rules mention #index, which the reference does not support")
	}
	if r.Cost.Budget != CostBudget || r.Limits.MaxNodes != maxNodes || r.Limits.MaxMatches != maxMatches || r.Summary != Allowed {
		t.Errorf("reference: %+v", r)
	}
}
