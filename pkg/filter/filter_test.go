package filter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var sample = Env{
	Event: Event{Type: "release.published", Payload: map[string]any{"tag": "v2.104.0", "prerelease": false, "highest": true},
		OccurredAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	Watch: Watch{ID: "w1", Name: "cli/cli", Kind: "github_release"},
}

// 001-AC13: valid filters compile against the typed env and evaluate to a boolean.
func TestMatch(t *testing.T) {
	for _, tt := range []struct {
		src  string
		want bool
	}{
		{"", true},
		{`event.type == "release.published"`, true},
		{`event.type startsWith "pr."`, false},
		{`not event.payload.prerelease && watch.kind == "github_release"`, true},
		{`event.payload.highest`, true},
		{`semver_gt(event.payload.tag, "v2.100.0")`, true},
		{`semver_gt(event.payload.tag, "v3")`, false},
		{`semver_gt("x", "1")`, false},
		{`event.occurred_at > date("2026-10-01")`, true},
		{`watch.name matches "^cli/" and watch.id == "w1"`, true},
		{"event.type == \"release.published\"\n  && event.payload.tag != \"v1\"", true},
	} {
		p, err := Compile(tt.src)
		if err != nil {
			t.Fatalf("%q: %v", tt.src, err)
		}
		got, err := Match(p, sample)
		if err != nil || got != tt.want {
			t.Errorf("%q: got %v, %v; want %v", tt.src, got, err, tt.want)
		}
	}
}

// 001-AC13: syntax and type errors carry a 1-based line and column.
func TestCompileErrors(t *testing.T) {
	for _, tt := range []struct {
		src          string
		line, column int
		msg          string
	}{
		{`event.type ==`, 1, 13, "unexpected token EOF"},
		{`event.nope == "x"`, 1, 7, "event has no field nope"},
		{`event.type`, 1, 1, "expected bool"},
		{`event.type == 1`, 1, 12, "invalid operation"},
		{"event.type == \"x\" &&\n  watch.nme == \"y\"", 2, 9, "watch has no field nme"},
		{`semver_gt(1, 2)`, 1, 11, "cannot use int"},
		{strings.Repeat("a", MaxLen+1), 1, 1, "longer than"},
		{strings.Repeat("1 + ", 300) + "1 > 0", 1, 403, "exceeds maximum allowed nodes"},
	} {
		_, err := Compile(tt.src)
		var fe *Error
		if !errors.As(err, &fe) {
			t.Fatalf("%q: want *Error, got %v", tt.src, err)
		}
		if fe.Line != tt.line || fe.Column != tt.column || !strings.Contains(fe.Message, tt.msg) {
			t.Errorf("%q: got %d:%d %q, want %d:%d ~%q", tt.src, fe.Line, fe.Column, fe.Message, tt.line, tt.column, tt.msg)
		}
		if !strings.Contains(fe.Error(), "(line ") {
			t.Errorf("%q: message lacks the position: %s", tt.src, fe.Error())
		}
	}
}

// A payload of an unexpected type is an evaluation error, not a panic.
func TestRuntimeError(t *testing.T) {
	p, err := Compile(`event.payload.tag > 3`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Match(p, sample); err == nil {
		t.Fatal("want an evaluation error")
	}
	p, err = Compile(`event.payload.prerelease`)
	if err != nil {
		t.Fatal(err)
	}
	env := sample
	env.Event.Payload = map[string]any{"prerelease": "yes"}
	if _, err := Match(p, env); err == nil {
		t.Fatal("want an error on a non-boolean result")
	}
}

// Outside the allowlist, a filter is refused at compile time, with its position and what is allowed.
func TestAllowlistRefuses(t *testing.T) {
	heavy := "any(1..999000, {" + strings.Repeat(`event.type contains "x" || `, 9) + `event.type contains "x"})`
	for _, tt := range []struct {
		src, what string
		column    int
	}{
		{heavy, "a range other than", 6},
		{`any(0..101, {# > 1})`, "a range other than", 6},
		{`any(1..len(event.type), {# > 1})`, "a range other than", 6},
		{`any(event.payload.a, {any(#.b, {# == 1})})`, "any() inside another predicate", 23},
		{`any(event.payload.a, {count(event.payload.b, {# == 1}) > 0})`, "count() inside another predicate", 23},
		{`toJSON(event.payload) contains "x"`, "toJSON()", 1},
		{`any(event.payload.a, {toJSON(event.payload) contains #})`, "toJSON()", 23},
		{`event.type matches event.watch_typo`, "", 0},
		{`event.type matches event.type`, "matches with a computed pattern", 26},
		{`event.type + "x" == "y"`, "+ on anything but numbers, times and durations", 12},
		{`event.payload.n + 1 > 2`, "+ on anything but numbers, times and durations", 17},
		{`len(map(event.payload.a, {#})) > 0`, "map()", 5},
		{`len(sort(event.payload.a)) > 0`, "sort()", 5},
		{`reduce(event.payload.a, {#acc}, 0) > 0`, "reduce()", 1},
		{`len(groupBy(event.payload.a, {#})) > 0`, "groupBy()", 5},
		{`join(event.payload.a, ",") == ""`, "join()", 1},
		{`len(split(event.type, "")) > 1`, "split()", 5},
		{`len(repeat("x", 10)) > 1`, "repeat()", 5},
		{`fromJSON(event.type) == nil`, "fromJSON()", 1},
		{`toBase64(event.type) == ""`, "toBase64()", 1},
		{`fromBase64(event.type) == ""`, "fromBase64()", 1},
		{`let x = event.type; x == "a"`, "this expression", 5},
		{`{"a": 1}.a == 1`, "this expression", 1},
		{`event.type[0:2] == "re"`, "this expression", 11},
		{`(event.payload.tag ?? "x") == "x"`, "the operator ??", 20},
		{`event.type in [` + strings.Repeat(`"x", `, 50) + `"x"]`, "an array of more than 50 items", 15},
		{`event.type in [watch.name]`, "a computed array item", 22},
		{`# == 1`, "", 0},
		{`any(event.payload.a, .b > 2 ** 40)`, "the operator **", 29},
		{`any(event.payload.a, {# matches "^v"})`, "matches inside a predicate", 25},
		{`any(event.payload.a, {# == event.type})`, "event inside a predicate (the body reads only #; compare event fields outside it)", 28},
		{`count(event.payload.a, {len(watch.name) > len(#)}) > 0`, "watch inside a predicate", 29},
		{`date("2024", "2006", "Europe/Paris") == now()`, "date() with a time zone", 1},
		{`trim(event.type, event.payload.c) == ""`, "trim() with a computed or non-ASCII (or over 16 bytes) cutset", 32},
		{`trim(event.type, "é") == ""`, "trim() with a computed or non-ASCII", 18},
		{`trim(event.type, "` + strings.Repeat("-", 17) + `") == ""`, "trim() with a computed or non-ASCII", 18},
		{`trimPrefix(event.type, event.type) == ""`, "trimPrefix() with a computed", 30},
		{`trimSuffix(event.type, watch.name) == ""`, "trimSuffix() with a computed", 30},
		{`event.type matches "a" || event.type matches "b" || event.type matches "c" || event.type matches "d"`, "more than 3 matches", 90},
		{`event.payload.body matches "[^x]{1000}x"`, "matches with a repetition count above 32", 28},
		{`event.payload.body matches "x{0,33}"`, "matches with a repetition count above 32", 28},
		{`event.payload.body matches "[^x]{32}[^y]{32}x"`, "matches with a pattern above 64 instructions", 28},
	} {
		_, err := Compile(tt.src)
		var fe *Error
		if !errors.As(err, &fe) {
			t.Errorf("%.60q: want a compile error, got %v", tt.src, err)
			continue
		}
		if tt.what == "" {
			continue
		}
		if !strings.HasPrefix(fe.Message, tt.what) || !strings.Contains(fe.Message, "is not allowed in a filter; allowed: "+Allowed) {
			t.Errorf("%.60q: message %q, want %q + the allowlist", tt.src, fe.Message, tt.what)
		}
		if fe.Line != 1 || fe.Column != tt.column {
			t.Errorf("%.60q: at %d:%d, want 1:%d", tt.src, fe.Line, fe.Column, tt.column)
		}
	}
}

func TestAllowlistAccepts(t *testing.T) {
	for _, src := range []string{
		`any(1..100, {# == 3})`,
		`count(event.payload.items, {.name startsWith "v" && len(.name) > 1}) >= 2`,
		`event.payload.tag matches "^v[0-9]+$" && none(event.payload.items, {# startsWith "v"})`,
		`len(filter(event.payload.items, {lower(#) == "x"})) == 0`,
		`event.type in ["a", "b", -1, 2.5, nil, true]`,
		`(event.payload.highest ? 1 : 2) + abs(-3) == 4`,
		`not (event.type endsWith ".removed") and watch.kind != "oci_tags"`,
		`event.occurred_at > now() - duration("24h") && event.occurred_at < date("2100-01-01")`,
		`string(event.payload.tag) == trim(" v1 ") && int("3") + float("1.5") > 4 && upper(trimPrefix(trimSuffix(watch.name, "x"), "y")) != ""`,
		`event.payload?.missing?.deep == nil`,
		`event.payload["tag"] == "v1" && event.payload.items[0] == "x"`,
		// The editor's reference (web/src/lib/filter.ts) inserts these as written.
		`event.payload.tag matches "^v[0-9]+[.]"`,
		`event.occurred_at > now() - duration("24h")`,
		`semver_gt(event.payload.tag, "v1.0.0")`,
	} {
		if _, err := Compile(src); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
}

// maxPayload is the largest payload Sanitize lets through, built from an even larger upstream one.
func maxPayload() map[string]any {
	str := func(i int) string { return fmt.Sprintf("%04d", i) + strings.Repeat("a", 4<<10) }
	items := make([]any, 10_000)
	for i := range items {
		items[i] = str(i)
	}
	objs := make([]any, 10_000)
	for i := range objs {
		objs[i] = map[string]any{"name": str(i), "tags": items[:300]}
	}
	raw := map[string]any{"body": strings.Repeat("b", 1<<20), "atag": "v2.104.0", "items": items, "objs": objs}
	// Up to the 200-key cap, so objs (sorted last) still makes it in.
	for i := range 190 {
		raw[fmt.Sprintf("k%03d", i)] = str(i)
	}
	return Sanitize(raw)
}

func TestSanitize(t *testing.T) {
	deep := any("leaf")
	for range 20 {
		deep = []any{deep}
	}
	many := map[string]any{}
	for i := range 500 {
		many[strings.Repeat("k", i+1)] = i
	}
	got := Sanitize(map[string]any{
		"s":     strings.Repeat("é", MaxString),
		"arr":   make([]any, 1000),
		"many":  many,
		"deep":  deep,
		"n":     1.5,
		"short": "x",
	})
	if s := got["s"].(string); len(s) > MaxString || !utf8.ValidString(s) {
		t.Errorf("string: %d bytes", len(s))
	}
	if n := len(got["arr"].([]any)); n != MaxItems {
		t.Errorf("array: %d items", n)
	}
	if n := len(got["many"].(map[string]any)); n != MaxItems {
		t.Errorf("map: %d keys", n)
	}
	d, levels := got["deep"], 0
	for {
		a, ok := d.([]any)
		if !ok || len(a) == 0 {
			break
		}
		d, levels = a[0], levels+1
	}
	if levels > maxDepth || d != nil {
		t.Errorf("depth %d, leaf %v", levels, d)
	}
	if got["n"] != 1.5 || got["short"] != "x" {
		t.Errorf("scalars: %v", got)
	}
	// The value budget caps the total, however the payload spreads it.
	wide := map[string]any{}
	for i := range MaxItems {
		wide[strings.Repeat("w", i+1)] = make([]any, MaxItems)
	}
	total := 0
	for _, v := range Sanitize(wide) {
		if a, ok := v.([]any); ok {
			for _, x := range a {
				if x != nil {
					total++
				}
			}
		}
	}
	if total > maxValues {
		t.Errorf("%d values survived, budget %d", total, maxValues)
	}
	if out := Sanitize(nil); out == nil || len(out) != 0 {
		t.Errorf("nil payload: %v", out)
	}
}

// The cut of a too-large payload is the same on every run: map keys go in order.
func TestSanitizeDeterministic(t *testing.T) {
	raw := map[string]any{}
	for i := range 300 {
		raw[fmt.Sprintf("key-%03d", 299-i)] = []any{fmt.Sprint(i), map[string]any{"a": i, "b": "x", "c": []any{1, 2, 3, 4, 5, 6, 7, 8, 9}}}
	}
	first, _ := json.Marshal(Sanitize(raw))
	for range 50 {
		if got, _ := json.Marshal(Sanitize(raw)); string(got) != string(first) {
			t.Fatal("Sanitize kept different keys or values on another run")
		}
	}
	if !strings.Contains(string(first), `"key-000"`) || strings.Contains(string(first), `"key-299"`) {
		t.Fatalf("kept not the first keys in order: %.120s", first)
	}
}

// Every string a filter reads, matches included, is at most MaxString bytes, or MaxBody for body.
func TestSanitizedStringsBounded(t *testing.T) {
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if len(x) > MaxString {
				t.Fatalf("a %d-byte string reached the filter", len(x))
			}
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
	p := maxPayload()
	if n := len(p["body"].(string)); n != MaxBody {
		t.Fatalf("body cut to %d bytes", n)
	}
	delete(p, "body")
	walk(p)
}
