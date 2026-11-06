package filter

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/expr-lang/expr/ast"

	"github.com/gedour/gedour/pkg/collect"
)

// nInsts is a pattern of exactly n compiled instructions that keeps n/2 threads alive on text of a and b:
// the slowest kind per instruction. It never matches, for want of a z.
func nInsts(t *testing.T, prefix string, n int) string {
	t.Helper()
	for k := n / 2; k > 0; k-- {
		for pad := 1; pad < n; pad++ {
			if src := prefix + strings.Repeat("[ab]?", k) + strings.Repeat("z", pad); regexInsts(src) == int64(n) {
				return src
			}
		}
	}
	t.Fatalf("no pattern of %d instructions", n)
	return ""
}

func withBody(body string) map[string]any {
	raw := spreadPayload()
	raw["body"] = body
	return Sanitize(raw)
}

func repeatTo(s string, n int) string { return strings.Repeat(s, n/len(s)+1)[:n] }

// bodyPayloads are 64 KiB bodies against the patterns they waste the most time on, next to a full byte
// budget in the other fields.
var bodyPayloads = map[string]func() map[string]any{
	"body a":         func() map[string]any { return withBody(repeatTo("a", MaxBody)) },
	"body ab":        func() map[string]any { return withBody(repeatTo("ab", MaxBody)) },
	"body near-miss": func() map[string]any { return withBody(repeatTo("CVE-2026-", MaxBody)) },
	"body multibyte": func() map[string]any { return withBody(strings.Repeat("ɩ", MaxBody/2)) },
	"body digits":    func() map[string]any { return withBody(repeatTo("1.2.3-", MaxBody)) },
}

func allPayloads() map[string]func() map[string]any {
	out := map[string]func() map[string]any{}
	for _, m := range []map[string]func() map[string]any{payloads, bodyPayloads} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// bodyShapes are the worst filters on body the budget lets through.
func bodyShapes(t *testing.T) []string {
	t.Helper()
	const body = "event.payload.body"
	s := []string{
		// 004-AC10: the sizes the budget was set for.
		body + ` matches "` + nInsts(t, "", 60) + `"`,
		body + ` matches "` + nInsts(t, "(?i)", 60) + `"`,
		// Three at once, sharing the budget.
		body + ` matches "` + nInsts(t, "", 20) + `" || ` + body + ` matches "` + nInsts(t, "(?i)", 20) + `" || ` + body + ` matches "` + regex64[2][:6] + `z"`,
		`event.payload[event.type] matches "` + nInsts(t, "", 60) + `"`,
		body + ` matches "CVE-[0-9]{4}-[0-9]+x"`,
		// The worst measured: chained Unicode classes, 3,997,920 units, about 45 ms on 64 KiB of a.
		body + ` matches "(?i)[\\pL\\pN]{29}[\\pL\\pN]{29}z"`,
		body + ` matches "(?:\\pL?){29}z"`,
		`semver_gt(` + body + `, "v1")`,
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return `lower(` + body + `) == upper(` + body + `)` })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return `lower(` + body + `) contains "zq"` })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return body + ` == string(` + body + `)` })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return body + ` contains ` + body })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return `event.payload == event.payload` })
		}),
		fit(t, func(n int) string {
			return chain(n, " || ", func(int) string { return body + ` in event.payload.items` })
		}),
		fit(t, func(n int) string { return chain(n, " || ", func(int) string { return `len(trim(` + body + `)) < 1` }) }),
	}
	return s
}

// Gates, in references (cost_test.go). The worst shape measures 28 to 31 references with and without the race detector
// and coverage; a gate is 2.5 times that, since the proportion can differ between CPUs.
const (
	filterGate  = 80.0
	ruleSetGate = 100 * filterGate
)

// 004-AC10: the budget's exact edge: 60 instructions pass, the first size above the budget is refused
// with its computed cost.
func TestBodyPatternBudget(t *testing.T) {
	if CostBudget != 4_000_000 {
		t.Fatalf("budget %d", CostBudget)
	}
	ok := `event.payload.body matches "` + nInsts(t, "", 60) + `"`
	if _, err := Compile(ok); err != nil {
		t.Fatalf("a 60-instruction pattern is refused: %v", err)
	}
	if _, err := Compile(`event.payload.body matches "` + nInsts(t, "", int(CostBudget/MaxBody)) + `"`); err != nil {
		t.Fatalf("the size the reference announces is refused: %v", err)
	}
	over := `event.payload.body matches "` + nInsts(t, "", int(CostBudget/MaxBody)+1) + `"`
	c, cerr := Cost(over)
	_, err := Compile(over)
	if cerr != nil || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("costs %d units, above the budget of 4000000", c)) {
		t.Fatalf("cost %d, %v; compile: %v", c, cerr, err)
	}
	// The same pattern on any other field keeps its old room.
	if _, err := Compile(strings.Replace(over, "payload.body", "payload.tag", 1)); err != nil {
		t.Fatalf("on tag: %v", err)
	}
}

// bodySpellings all read body: each is priced for 64 KiB, and each really reads past 4 KiB.
var bodySpellings = []string{
	`event.payload.body`,
	`event.payload["body"]`,
	`event["payload"]["body"]`,
	`event?.payload?.body`,
	`event.payload?.body`,
	`(true ? event.payload : event.payload).body`,
	`(true ? event : event).payload.body`,
	`(true ? event.payload.body : "")`,
	`(false ? "" : event.payload.body)`,
	`event.payload[event.type]`,
	`event.payload[watch.name]`,
	`event.payload[lower("BODY")]`,
	`event.payload[trim(" body ")]`,
	`event.payload[string("body")]`,
	`string(event.payload.body)`,
	`trim(event.payload.body)`,
	`trimPrefix(event.payload.body, "x")`,
	`string(trim(string(event.payload["body"])))`,
}

// 004-AC9: nothing reads 64 KiB while priced at 4 KiB, however it is spelled; a mark at 60 KiB is seen.
func TestBodyPricedHoweverSpelled(t *testing.T) {
	env := sample
	env.Event.Type = "body"
	env.Watch.Name = "body"
	env.Event.Payload = Sanitize(map[string]any{"tag": "v1", "body": strings.Repeat("x", 60<<10) + "MARK" + strings.Repeat("y", 10<<10)})
	if n := len(env.Event.Payload["body"].(string)); n != MaxBody {
		t.Fatalf("%d bytes of body", n)
	}
	for _, s := range bodySpellings {
		src := s + ` contains "MARK"`
		c, err := Cost(src)
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if c < MaxBody {
			t.Errorf("%s: priced %d, under %d", s, c, MaxBody)
		}
		p, err := Compile(src)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got, err := Match(p, env); err != nil || !got {
			t.Errorf("%s: matched %v, %v", s, got, err)
		}
	}
	// Past the cut, nothing.
	raw := map[string]any{"body": strings.Repeat("x", MaxBody) + "MARK"}
	env.Event.Payload = Sanitize(raw)
	p, _ := Compile(`event.payload.body contains "MARK"`)
	if got, err := Match(p, env); err != nil || got {
		t.Errorf("past the cut: %v, %v", got, err)
	}
}

// 004-AC9: every other field keeps its price, nested keys named body included.
func TestOtherFieldsKeepTheirPrice(t *testing.T) {
	for _, tt := range []struct {
		src  string
		cost int64
	}{
		{`event.payload.tag contains "ab"`, 32*7 + MaxString + 2},
		{`event.payload["tag"] contains "ab"`, 32*7 + MaxString + 2},
		{`event.payload.body contains "ab"`, 32*7 + MaxBody + 2},
		{`event.payload.items.body contains "ab"`, 32*9 + MaxString + 2},
		{`event.payload.items[0] contains "ab"`, 32*9 + MaxString + 2},
		{`event.type contains "ab"`, 32*5 + MaxString + 2},
		{`watch.name contains "ab"`, 32*5 + MaxString + 2},
		// string() and trim() hand the bound on: the next operation pays for 64 KiB too.
		{`string(event.payload.body) contains "ab"`, 32*8 + 2*MaxBody + 2},
		{`trim(event.payload.body) contains "ab"`, 32*8 + 2*MaxBody + 2},
		// Deep sizes: the whole payload holds body, a part of it does not.
		{`event.payload == event.payload`, 32*7 + payloadSize},
		{`event.payload.items == event.payload.items`, 32*11 + fieldSize},
		{`event.payload.body == event.payload.body`, 32*11 + fieldSize},
		// Items are bounded apart from body: # is priced as before.
		{`any(event.payload.items, {# contains "ab"})`, 5*32 + 32 + 4*32*MaxItems + maxRest + 2*MaxItems},
	} {
		got, err := Cost(tt.src)
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		if got != tt.cost {
			t.Errorf("%s: cost %d, want %d", tt.src, got, tt.cost)
		}
	}
	if int64(maxBytes) != MaxBody+maxRest || maxBytes != 128<<10 || maxRest != 64<<10 {
		t.Fatalf("bounds %d, %d", maxBytes, maxRest)
	}
	r := Reference()
	if r.Limits.MaxBody != MaxBody || r.Cost.BodyBytes != MaxBody || r.Cost.ItemBytes != maxRest || !strings.Contains(strings.Join(r.Rules, " "), "at most 61 instructions") {
		t.Fatalf("reference: %+v", r)
	}
}

// A predicate over a string would walk it byte by byte, 64 KiB iterations priced as 200: refused at
// evaluation, on the other strings too.
func TestPredicateOnString(t *testing.T) {
	env := sample
	env.Event.Payload = Sanitize(map[string]any{"tag": "v1", "body": strings.Repeat("a", MaxBody), "items": []any{"a"}})
	inner := chain(30, " || ", func(i int) string { return fmt.Sprintf("# == %d", i) })
	for _, src := range []string{
		`any(event.payload.body, {` + inner + `})`,
		`count(event.payload["body"], {` + inner + `}) > 0`,
		`len(filter(event.payload[watch.kind], {true})) > 0`,
		`any(event.payload.tag, {true})`,
		`one((true ? event.payload.body : event.payload.items), {true})`,
		// ?: shares one node for condition and result: refused neither on save nor left unguarded.
		`any(event.payload.body, {true}) ?: false`,
	} {
		if src == `len(filter(event.payload[watch.kind], {true})) > 0` {
			env.Watch.Kind = "body"
		}
		p, err := Compile(src)
		if err != nil {
			t.Fatalf("%.60q: %v", src, err)
		}
		got, err := Match(p, env)
		if err == nil || !strings.Contains(err.Error(), "a predicate takes an array") || got {
			t.Errorf("%.60q: %v, %v", src, got, err)
		}
	}
	// The whole payload is no array either: refused on save, or expr fails on it without looping.
	for _, src := range []string{`any(event.type == "" ? event.payload.items : event.payload, {# == ""})`, `any(true ? event.payload : event.payload.items, {# == ""})`} {
		if p, err := Compile(src); err == nil {
			if _, err := Match(p, env); err == nil {
				t.Errorf("%s walked the payload map", src)
			}
		}
	}
	// Arrays and ranges still work, wrapped.
	for _, src := range []string{`any(event.payload.items, {# == "a"})`, `any(event.payload.items, {# == "a"}) ?: false`,
		`any(filter(event.payload.items, {true}), {# == "a"}) ?: false`, `any(1..3, {# == 2})`, `count([1, 2], {# > 1}) == 1`, `len(filter(event.payload.items, {true})) == 1`} {
		p, err := Compile(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got, err := Match(p, env); err != nil || !got {
			t.Errorf("%s: %v, %v", src, got, err)
		}
	}
	// A source cannot call the guard itself.
	if _, err := Compile(`array_only(event.payload.items) == []`); err == nil || !strings.Contains(err.Error(), "this call") {
		t.Errorf("array_only in a source: %v", err)
	}
	if slices.Contains(Reference().Builtins, "array_only") {
		t.Error("array_only in the reference")
	}
}

// ?: holds one node twice and the walk visits it twice: the array is wrapped once.
func TestArrayGuardWrapsOnce(t *testing.T) {
	tree, err := check(`any(event.payload.items, {true}) ?: false`)
	if err != nil {
		t.Fatal(err)
	}
	cond, ok := tree.Node.(*ast.ConditionalNode)
	if !ok || cond.Cond != cond.Exp1 {
		t.Fatalf("?: no longer shares its node: %T", tree.Node)
	}
	ast.Walk(&tree.Node, arrayGuard{})
	wrap, ok := cond.Cond.(*ast.BuiltinNode).Arguments[0].(*ast.CallNode)
	if !ok {
		t.Fatal("not wrapped")
	}
	if _, twice := wrap.Arguments[0].(*ast.CallNode); twice {
		t.Fatal("wrapped twice")
	}
}

// 004-AC9: body has its own bytes: at any size and key order, it cuts into no other field, and no other
// field cuts into it. Only top-level strings count as body.
func TestSanitizeBody(t *testing.T) {
	huge := strings.Repeat("é", MaxBody)
	items := make([]any, 40)
	for i := range items {
		items[i] = strings.Repeat("i", MaxString) // 160 KiB asked: capped
	}
	raw := map[string]any{"a": strings.Repeat("a", 2<<10), "atag": "v1", "body": huge, "items": items,
		"anested": map[string]any{"body": strings.Repeat("n", MaxBody)}}
	out := Sanitize(raw)
	body := out["body"].(string)
	if len(body) > MaxBody || len(body) < MaxBody-1 || !utf8.ValidString(body) {
		t.Fatalf("body: %d bytes, valid %v", len(body), utf8.ValidString(body))
	}
	if out["atag"] != "v1" || len(out["a"].(string)) != 2<<10 {
		t.Fatalf("body crowded out a field: %.40v", out["atag"])
	}
	if n := len(out["anested"].(map[string]any)["body"].(string)); n != MaxString {
		t.Fatalf("a nested body kept %d bytes", n)
	}
	total := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			total += len(x)
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(out)
	if total > maxBytes || total-len(body) > maxRest {
		t.Fatalf("%d bytes in all, %d apart from body", total, total-len(body))
	}
	if int64(total-len(body)) < maxRest-MaxString {
		t.Fatalf("only %d bytes for the rest: body took some", total-len(body))
	}
	// A body of another type is an ordinary field.
	if got := Sanitize(map[string]any{"body": []any{strings.Repeat("x", MaxBody)}})["body"].([]any)[0].(string); len(got) != MaxString {
		t.Fatalf("list body: %d bytes", len(got))
	}
}

// Ids of 5000 digits, which sort before name and url, would take the other fields' whole byte budget.
func TestLongIDsDoNotCrowdOut(t *testing.T) {
	var notes strings.Builder
	for i := range 20 {
		fmt.Fprintf(&notes, "CVE-2026-%d%s ", i, strings.Repeat("7", 5000))
	}
	e := []collect.Event{{Subject: "v1", Payload: map[string]any{"tag": "v1", "name": "One", "url": "https://x/v1"}}}
	collect.WithNotes(e, map[string]string{"v1": notes.String()})
	b, err := json.Marshal(e[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	p := Sanitize(stored)
	if p["tag"] != "v1" || p["name"] != "One" || p["url"] != "https://x/v1" {
		t.Fatalf("crowded out: %q %q %q", p["tag"], p["name"], p["url"])
	}
}

// 004-AC4, 004-AC5: what a filter sees of the fields, after the JSON round trip.
func TestCVEFilters(t *testing.T) {
	cves := Sanitize(map[string]any{"body": "fixes CVE-2026-1234, BREAKING CHANGE: yes", "cves": []any{"CVE-2026-1234", "CVE-2026-2"}})
	empty := Sanitize(map[string]any{"body": "", "cves": []any{}})
	for _, tt := range []struct {
		src        string
		on, onNone bool
	}{
		{`"CVE-2026-1234" in event.payload.cves`, true, false},
		{`len(event.payload.cves) > 0`, true, false},
		{`len(event.payload.cves) == 0`, false, true},
		{`event.payload.body matches "CVE-[0-9]{4}-[0-9]+"`, true, false},
		{`event.payload.body matches "(?i)breaking.change"`, true, false},
		{`event.payload.body == ""`, false, true},
		{`any(event.payload.cves, {# startsWith "CVE-2026-"})`, true, false},
	} {
		p, err := Compile(tt.src)
		if err != nil {
			t.Fatalf("%s: %v", tt.src, err)
		}
		for _, c := range []struct {
			payload map[string]any
			want    bool
		}{{cves, tt.on}, {empty, tt.onNone}} {
			env := sample
			env.Event.Payload = c.payload
			if got, err := Match(p, env); err != nil || got != c.want {
				t.Errorf("%s on %v: %v, %v; want %v", tt.src, c.payload["cves"], got, err, c.want)
			}
		}
	}
}

// The budget only grew, so a saved filter stays accepted, including the shapes whose price went up a little
// (whole-payload deep size, string() of a case mapping). Pricing body at 64 KiB is the known exception: a long
// pattern on body or on a computed key is refused, and the router logs it at start.
func TestSavedFiltersStayAccepted(t *testing.T) {
	for _, src := range []string{
		chain(10, " || ", func(int) string { return `event.payload != event.payload` }),
		chain(10, " || ", func(int) string { return `"zq" in event.payload` }),
		chain(10, " || ", func(int) string { return `"zq" in event.payload.items` }),
		chain(14, " || ", func(int) string { return `string(lower(event.type)) contains "zq"` }),
		`event.payload.items[0] matches "` + regex64[0] + `" || event.payload.items[1] matches "` + regex64[1] + `" || event.payload.items[2] matches "` + regex64[2] + `"`,
	} {
		if _, err := Compile(src); err != nil {
			t.Errorf("%.60q: %v", src, err)
		}
	}
	if _, err := Compile(knownRefusedFilter); err == nil || !strings.Contains(err.Error(), "above the budget") {
		t.Errorf("known refusal: %v", err)
	}
}

// knownRefusedFilter passed the old model (a computed key priced at 4 KiB) and is over the budget now.
const knownRefusedFilter = `event.payload[event.type] matches "(?:[ab]?){30}zz"`

// An error quoting its input (strconv in int(), date()) stays short for the logs and the preview.
func TestEvalErrorClipped(t *testing.T) {
	env := sample
	env.Event.Payload = Sanitize(map[string]any{"body": strings.Repeat("é", MaxBody/2)})
	p, err := Compile(`int(event.payload.body) > 0`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Match(p, env)
	if err == nil || len(err.Error()) > maxEvalError+32 || !utf8.ValidString(err.Error()) {
		t.Fatalf("error of %d bytes", len(fmt.Sprint(err)))
	}
}
