// Package filter compiles and evaluates the one-line rule filters (expr-lang/expr) against events.
//
// A filter is compiled against the typed Env, so unknown fields and type errors fail on save, and
// returns a boolean. Only an allowlist of expr is accepted (see Allowed): comparisons, logic, a few
// cheap builtins and single-level predicates. Together with Sanitize, which bounds the payload an
// evaluation sees, that caps the cost of one evaluation whatever the user writes or the upstream sends.
package filter

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/checker"
	"github.com/expr-lang/expr/compiler"
	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/optimizer"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"

	"github.com/gedour/gedour/pkg/collect"
)

// MaxLen caps a filter's source, in bytes.
const MaxLen = 2000

const (
	maxNodes = 200
	// Regular expressions: few, small, flat, and never per item.
	maxMatches      = 3
	maxRegexInsts   = 64
	maxRegexRepeats = 32
	// maxRange is the widest constant range a..b a filter may iterate.
	maxRange = 100
	// maxArray caps an array literal.
	maxArray = 50
	// maxCutset caps the ASCII cutset literal of trim().
	maxCutset = 16
	// maxEvalError caps an evaluation error's message.
	maxEvalError = 1 << 10
)

// Payload bounds applied by Sanitize before evaluation.
const (
	MaxString = 4 << 10
	// MaxBody is what a filter reads of the top-level string payload.body, the one long field.
	MaxBody   = collect.MaxNotes
	MaxItems  = 200
	maxValues = 2000
	// maxRest bounds the strings of a payload apart from body, so an equality or a string() over them
	// stays cheap and a long body cannot crowd them out.
	maxRest = 64 << 10
	// maxBytes bounds the strings of a whole payload.
	maxBytes = MaxBody + maxRest
	maxDepth = 8
)

// Ref is the allowlist and its limits, for the rule editor (GET /api/v1/filters/reference).
type Ref struct {
	Operators  []string `json:"operators" doc:"Binary and unary operators"`
	Builtins   []string `json:"builtins" doc:"Functions; date() takes at most 2 arguments"`
	Predicates []string `json:"predicates" doc:"Take an array and a predicate {...} over its items (#)"`
	Limits     Limits   `json:"limits"`
	Cost       CostRef  `json:"cost"`
	Rules      []string `json:"rules" doc:"What the limits mean, one sentence each"`
	Summary    string   `json:"summary" doc:"The one-line form repeated in compile errors"`
}

// Limits are the numeric caps of a filter.
type Limits struct {
	MaxLength       int `json:"max_length" doc:"Characters of source"`
	MaxNodes        int `json:"max_nodes" doc:"Expression nodes"`
	MaxMatches      int `json:"max_matches" doc:"matches operators per filter, none inside a predicate"`
	MaxRegexInsts   int `json:"max_regex_instructions" doc:"Compiled size of a matches pattern"`
	MaxRegexRepeats int `json:"max_regex_repeat" doc:"Largest {n,m} bound in a pattern"`
	MaxRange        int `json:"max_range" doc:"Span of a constant range a..b"`
	MaxArray        int `json:"max_array" doc:"Items of an array literal"`
	PredicateDepth  int `json:"predicate_depth" doc:"Predicates cannot hold another predicate"`
	MaxString       int `json:"max_string" doc:"Bytes of one payload string a filter sees"`
	MaxBody         int `json:"max_body" doc:"Bytes a filter sees of event.payload.body"`
	MaxItems        int `json:"max_items" doc:"Entries of one payload array or map a filter sees"`
}

// CostRef is the static cost model a filter is checked against, in units of about a byte of work.
type CostRef struct {
	Budget         int64 `json:"budget" doc:"Most units one filter may cost"`
	Node           int64 `json:"node" doc:"Each operation; per item inside a predicate"`
	CaseMapping    int64 `json:"case_mapping" doc:"Per byte of lower() and upper()"`
	Semver         int64 `json:"semver" doc:"Per byte of semver_gt()"`
	Date           int64 `json:"date" doc:"Each date() parse"`
	DeepValue      int64 `json:"deep_value" doc:"Per value of a map or array compared with == or in"`
	FieldBytes     int64 `json:"field_bytes" doc:"Bytes assumed for an event or watch field"`
	BodyBytes      int64 `json:"body_bytes" doc:"Bytes assumed for event.payload.body, or for a payload key computed at run time"`
	ItemBytes      int64 `json:"item_bytes" doc:"Bytes assumed for # summed over all items, paid once per use"`
	PredicateItems int64 `json:"predicate_items" doc:"Items a predicate is assumed to run over"`
}

func keys(m map[string]bool) []string { return slices.Sorted(maps.Keys(m)) }

// Reference describes the allowlist from the tables Compile enforces.
func Reference() Ref {
	ops := append(keys(binaryOps), "matches", "+", "-", "..", "?:")
	for _, u := range keys(unaryOps) {
		if !slices.Contains(ops, u) {
			ops = append(ops, u)
		}
	}
	return Ref{
		Operators:  ops,
		Builtins:   append(keys(scalarBuiltins), keys(functions)...),
		Predicates: keys(predicateBuiltins),
		Limits: Limits{MaxLength: MaxLen, MaxNodes: maxNodes, MaxMatches: maxMatches, MaxRegexInsts: maxRegexInsts,
			MaxRegexRepeats: maxRegexRepeats, MaxRange: maxRange, MaxArray: maxArray, PredicateDepth: 1,
			MaxString: MaxString, MaxBody: MaxBody, MaxItems: MaxItems},
		Cost: CostRef{Budget: CostBudget, Node: nodeUnits, CaseMapping: caseFactor, Semver: semverUnits, Date: dateUnits,
			DeepValue: valueUnits, FieldBytes: MaxString, BodyBytes: MaxBody, ItemBytes: maxRest, PredicateItems: MaxItems},
		Rules: []string{
			fmt.Sprintf("A filter costs at most %d units: each operation %d (times %d inside a predicate), string work a unit per byte "+
				"(%d for lower and upper, %d for semver_gt), a field counting %d bytes, event.payload.body %d, and # %d bytes over all items.",
				CostBudget, nodeUnits, MaxItems, caseFactor, semverUnits, MaxString, MaxBody, maxRest),
			fmt.Sprintf("event.payload.body is read up to %d bytes, every other string up to %d; a pattern costs its instructions per byte, so at most %d instructions fit on body.",
				MaxBody, MaxString, CostBudget/MaxBody),
			"On the worst payloads a filter takes about 45 ms, and a user's 100 rules about 4.5 s.",
			"A predicate takes an array: on a string it fails at evaluation, and the rule does not match.",
			"A predicate body reads only its item: # and #.field, plus literals. Compare event or watch fields outside it.",
			"Predicates do not nest.",
			fmt.Sprintf("matches takes a string-literal pattern (at most %d instructions, repetitions up to %d), at most %d times, never inside a predicate.",
				maxRegexInsts, maxRegexRepeats, maxMatches),
			"+ and - work on numbers, times and durations, not strings.",
			fmt.Sprintf("The second argument of trim, trimPrefix and trimSuffix is a string literal; for trim, ASCII and at most %d bytes.", maxCutset),
			fmt.Sprintf("Ranges are integer constants at most %d apart; array literals hold at most %d literals.", maxRange, maxArray),
		},
		Summary: Allowed,
	}
}

// Allowed is the user-facing summary of the allowlist, repeated in compile errors.
var Allowed = fmt.Sprintf("literals, event and watch fields, %s, matches with a string literal (up to %d, outside predicates), "+
	"+ and - on numbers, times and durations, ?:, constant ranges up to %d, %s, and %s with a predicate that reads only # "+
	"and holds no other predicate", strings.Join(keys(binaryOps), " "), maxMatches, maxRange,
	strings.Join(append(keys(scalarBuiltins), keys(functions)...), " "), strings.Join(keys(predicateBuiltins), " "))

// Event is the event as a filter sees it.
type Event struct {
	Type       string         `expr:"type"`
	Payload    map[string]any `expr:"payload"`
	OccurredAt time.Time      `expr:"occurred_at"`
}

// Watch is the event's watch as a filter sees it.
type Watch struct {
	ID   string `expr:"id"`
	Name string `expr:"name"`
	Kind string `expr:"kind"`
}

// Env is everything a filter can read.
type Env struct {
	Event Event `expr:"event"`
	Watch Watch `expr:"watch"`
}

// Error is a compile error with its 1-based position in the source.
type Error struct {
	Line    int
	Column  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (line %d, column %d)", e.Message, e.Line, e.Column)
}

// Program is a compiled filter; nil matches every event.
type Program = vm.Program

func semverGT(params ...any) (any, error) {
	a, _ := params[0].(string)
	b, _ := params[1].(string)
	gt, _ := collect.SemverGreater(a, b)
	return gt, nil
}

// scalarString replaces expr's string(): formatting a whole map or array is the one cheap-looking call
// whose cost grows with the payload.
func scalarString(params ...any) (any, error) {
	switch v := params[0].(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case time.Time:
		return v.Format(time.RFC3339), nil
	case time.Duration:
		return v.String(), nil
	}
	return nil, fmt.Errorf("string() takes a string, number, boolean or time, not %T", params[0])
}

// arrayOnlyFunc refuses a string where a predicate expects an array: expr would walk it byte by byte,
// up to MaxBody iterations where the cost model prices MaxItems.
func arrayOnlyFunc(params ...any) (any, error) {
	if _, ok := params[0].(string); ok {
		return nil, errors.New("a predicate takes an array, not a string")
	}
	return params[0], nil
}

// arrayGuard wraps every predicate's array in array_only. It runs after the allowlist walk, which refuses this
// call in a source; ?: shares one node for its condition and result, so a node may come by twice.
type arrayGuard struct{}

func (arrayGuard) Visit(node *ast.Node) {
	b, ok := (*node).(*ast.BuiltinNode)
	if !ok || !predicateBuiltins[b.Name] {
		return
	}
	if c, ok := b.Arguments[0].(*ast.CallNode); ok {
		if id, ok := c.Callee.(*ast.IdentifierNode); ok && id.Value == "array_only" {
			return
		}
	}
	callee := &ast.IdentifierNode{Value: "array_only"}
	callee.SetType(reflect.TypeFor[func(any) any]())
	call := &ast.CallNode{Callee: callee, Arguments: []ast.Node{b.Arguments[0]}}
	call.SetType(reflect.TypeFor[any]())
	call.SetLocation(b.Arguments[0].Location())
	b.Arguments[0] = call
}

var options = []expr.Option{
	expr.Env(Env{}),
	expr.AsBool(),
	expr.MaxNodes(maxNodes),
	expr.Function("semver_gt", semverGT, new(func(string, string) bool)),
	expr.DisableBuiltin("string"),
	expr.Function("string", scalarString, new(func(any) string)),
	expr.Function("array_only", arrayOnlyFunc, new(func(any) any)),
}

var (
	scalarBuiltins = map[string]bool{"len": true, "lower": true, "upper": true, "trim": true, "trimPrefix": true,
		"trimSuffix": true, "int": true, "float": true, "abs": true, "now": true, "duration": true, "date": true}
	predicateBuiltins = map[string]bool{"any": true, "all": true, "none": true, "one": true, "filter": true, "count": true}
	functions         = map[string]bool{"semver_gt": true, "string": true}
	roots             = map[string]bool{"event": true, "watch": true}
	binaryOps         = map[string]bool{"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "and": true,
		"or": true, "&&": true, "||": true, "in": true, "contains": true, "startsWith": true, "endsWith": true}
	unaryOps = map[string]bool{"not": true, "!": true, "-": true, "+": true}
)

// position turns a rune offset of src into a 1-based line and column.
func position(src string, offset int) (line, col int) {
	line, col = 1, 1
	for i, r := range []rune(src) {
		if i == offset {
			break
		}
		if r == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return line, col
}

// regexCost refuses a pattern whose matching could cost more than a small, fixed program per byte.
func regexCost(pattern string) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "" // the checker reports the syntax error itself
	}
	if repeats(re) > maxRegexRepeats {
		return fmt.Sprintf("a repetition count above %d", maxRegexRepeats)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil || len(prog.Inst) > maxRegexInsts {
		return fmt.Sprintf("a pattern above %d instructions", maxRegexInsts)
	}
	return ""
}

// trimLiteral reports whether the second argument of a trim builtin is a cheap literal: any string
// literal for trimPrefix/trimSuffix (one comparison), an ASCII one of at most 16 bytes for trim (a
// byte set, linear in the input).
func trimLiteral(name string, arg ast.Node) bool {
	lit, ok := arg.(*ast.StringNode)
	if !ok {
		return false
	}
	if name != "trim" {
		return true
	}
	if len(lit.Value) > maxCutset {
		return false
	}
	for i := 0; i < len(lit.Value); i++ {
		if lit.Value[i] >= 0x80 {
			return false
		}
	}
	return true
}

// regexInsts is the compiled size of a pattern, the per-byte cost of matching it.
func regexInsts(pattern string) int64 {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return maxRegexInsts
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return maxRegexInsts
	}
	return int64(len(prog.Inst))
}

// repeats is the largest {n,m} bound in re.
func repeats(re *syntax.Regexp) int {
	n := 0
	if re.Op == syntax.OpRepeat {
		n = max(re.Min, re.Max)
	}
	for _, sub := range re.Sub {
		n = max(n, repeats(sub))
	}
	return n
}

func (a *allow) fail(n ast.Node, what string) {
	if a.err == nil {
		line, col := position(a.src, n.Location().From)
		a.err = &Error{Line: line, Column: col, Message: what + " is not allowed in a filter; allowed: " + Allowed}
	}
}

func isNumber(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8,
		reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

var (
	timeType     = reflect.TypeFor[time.Time]()
	durationType = reflect.TypeFor[time.Duration]()
)

func fixedCost(t reflect.Type) bool {
	return t == timeType || t == durationType || isNumber(t)
}

func isLiteral(n ast.Node) bool {
	switch t := n.(type) {
	case *ast.NilNode, *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode, *ast.StringNode:
		return true
	case *ast.UnaryNode:
		return t.Operator == "-" && isLiteral(t.Node)
	}
	return false
}

// Static cost model, in abstract units (about a byte of work each). Every filter's upper bound is
// computed on the checked tree and refused above CostBudget, so the cost is known before the first
// evaluation, whatever the payload holds within Sanitize's bounds.
const (
	// CostBudget is the most one filter may cost: about 45 ms measured on the worst payloads, 4.5 s for a user's 100 rules.
	CostBudget = 4_000_000
	// nodeUnits is one VM step, worth about this many bytes of string work.
	nodeUnits = 32
	// caseFactor prices lower()/upper() per byte: multibyte case mapping is that much slower than a copy.
	caseFactor = 3
	// dateUnits is one date() parse.
	dateUnits = 64 * nodeUnits
	// semverUnits prices semver_gt per byte: a small regular expression runs over each side.
	semverUnits = 12
	// valueUnits is the per-value overhead of a deep comparison (maps, arrays).
	valueUnits = 16
	// payloadSize bounds a whole sanitized payload, for deep comparisons.
	payloadSize = maxBytes + valueUnits*maxValues
	// fieldSize bounds any part of it: only the payload itself and body can hold body's bytes.
	fieldSize = maxRest + valueUnits*maxValues
)

// val is what the walker knows about a node: its cost so far and bounds on its value.
type val struct {
	cost  int64
	bytes int64 // string bytes of the value; for an item value, the sum over all items
	size  int64 // deep size, for == and in
	item  bool  // derived from # inside a predicate: bytes and size are totals over the items
	event bool  // may be the event itself
	root  bool  // may be the whole payload
}

// allow walks the checked tree, accepts only the allowlist and sums its cost; the first refusal wins.
type allow struct {
	src     string
	err     *Error
	matches int
}

// items prices v's byte-proportional work at its call site: once for an item value (its bound is
// the total over items), per item for anything else inside a predicate.
func items(v val, inPredicate bool) int64 {
	if inPredicate && !v.item {
		return MaxItems
	}
	return 1
}

func byteWork(v val, inPredicate bool) int64 { return v.bytes * items(v, inPredicate) }

func sizeWork(v val, inPredicate bool) int64 { return v.size * items(v, inPredicate) }

// step is the cost of evaluating one node: per item inside a predicate.
func step(inPredicate bool) int64 {
	if inPredicate {
		return nodeUnits * MaxItems
	}
	return nodeUnits
}

func sum(vs ...val) int64 {
	var c int64
	for _, v := range vs {
		c += v.cost
	}
	return c
}

func anyItem(vs ...val) bool {
	return slices.ContainsFunc(vs, func(v val) bool { return v.item })
}

// node checks n and prices it; inPredicate is true inside a predicate body, where # is the item.
func (a *allow) node(n ast.Node, inPredicate bool) val {
	if a.err != nil {
		return val{}
	}
	st := step(inPredicate)
	switch t := n.(type) {
	case *ast.StringNode:
		return val{cost: st, bytes: int64(len(t.Value)), size: int64(len(t.Value))}
	case *ast.NilNode, *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode:
		return val{cost: st, size: 8}
	case *ast.ArrayNode:
		if len(t.Nodes) > maxArray {
			a.fail(t, fmt.Sprintf("an array of more than %d items", maxArray))
		}
		out := val{cost: st}
		for _, x := range t.Nodes {
			if !isLiteral(x) {
				a.fail(x, "a computed array item")
			}
			v := a.node(x, inPredicate)
			out.cost += v.cost
			out.size += v.size + valueUnits
		}
		return out
	case *ast.IdentifierNode:
		switch {
		case !roots[t.Value]:
			a.fail(t, strconv.Quote(t.Value))
		case inPredicate:
			// Per-item work then follows the item's own data, which Sanitize bounds in total.
			a.fail(t, t.Value+" inside a predicate (the body reads only #; compare "+t.Value+" fields outside it)")
		}
		return val{cost: st, bytes: MaxString, size: payloadSize, event: t.Value == "event"}
	case *ast.PointerNode:
		if !inPredicate {
			a.fail(t, "# outside a predicate")
		}
		return val{cost: st, bytes: maxRest, size: fieldSize, item: true}
	case *ast.MemberNode:
		if t.Method {
			a.fail(t, "a method call")
		}
		x, p := a.node(t.Node, inPredicate), a.node(t.Property, inPredicate)
		v := val{cost: st + sum(x, p), bytes: MaxString, size: fieldSize, item: x.item}
		lit, isLit := t.Property.(*ast.StringNode)
		switch {
		case x.item:
			v.bytes = maxRest
		case x.event && (!isLit || lit.Value == "payload"):
			v.root, v.size = true, payloadSize
		case x.root && (!isLit || lit.Value == "body"):
			// A computed key may read body too, whatever it looks like.
			v.bytes = MaxBody
		}
		return v
	case *ast.ChainNode:
		return a.node(t.Node, inPredicate)
	case *ast.UnaryNode:
		if !unaryOps[t.Operator] {
			a.fail(t, "the operator "+t.Operator)
		}
		x := a.node(t.Node, inPredicate)
		return val{cost: st + x.cost, size: 8}
	case *ast.BinaryNode:
		return a.binary(t, inPredicate)
	case *ast.ConditionalNode:
		c, x, y := a.node(t.Cond, inPredicate), a.node(t.Exp1, inPredicate), a.node(t.Exp2, inPredicate)
		return val{cost: st + sum(c, x, y), bytes: max(x.bytes, y.bytes), size: max(x.size, y.size), item: anyItem(x, y),
			event: x.event || y.event, root: x.root || y.root}
	case *ast.CallNode:
		id, ok := t.Callee.(*ast.IdentifierNode)
		if !ok || !functions[id.Value] {
			a.fail(t, "this call")
			return val{}
		}
		args := make([]val, len(t.Arguments))
		work := int64(0)
		for i, x := range t.Arguments {
			args[i] = a.node(x, inPredicate)
			work += byteWork(args[i], inPredicate)
		}
		v := val{cost: st + sum(args...), size: 8}
		if id.Value == "semver_gt" {
			v.cost += semverUnits * work
		} else { // string()
			v.cost += work
			v.bytes, v.size, v.item = args[0].bytes, args[0].bytes, args[0].item
			if !v.item {
				// Numbers and times carry no byte bound of their own.
				v.bytes = max(args[0].bytes, MaxString)
				v.size = v.bytes
			}
		}
		return v
	case *ast.BuiltinNode:
		return a.builtin(t, inPredicate)
	default:
		a.fail(n, "this expression")
	}
	return val{}
}

func (a *allow) binary(t *ast.BinaryNode, inPredicate bool) val {
	switch {
	case binaryOps[t.Operator]:
	case t.Operator == "matches":
		lit, ok := t.Right.(*ast.StringNode)
		a.matches++
		switch {
		case !ok:
			a.fail(t.Right, "matches with a computed pattern")
		case inPredicate:
			a.fail(t, "matches inside a predicate")
		case a.matches > maxMatches:
			a.fail(t, fmt.Sprintf("more than %d matches", maxMatches))
		default:
			if why := regexCost(lit.Value); why != "" {
				a.fail(t.Right, "matches with "+why)
			}
		}
	case t.Operator == "+" || t.Operator == "-":
		// Fixed cost: numbers, times and durations; never strings, whose concatenation grows.
		if !fixedCost(t.Left.Type()) || !fixedCost(t.Right.Type()) {
			a.fail(t, t.Operator+" on anything but numbers, times and durations")
		}
	case t.Operator == "..":
		lo, okLo := t.Left.(*ast.IntegerNode)
		hi, okHi := t.Right.(*ast.IntegerNode)
		if !okLo || !okHi || hi.Value-lo.Value > maxRange {
			a.fail(t, fmt.Sprintf("a range other than two integer constants at most %d apart", maxRange))
		}
	default:
		a.fail(t, "the operator "+t.Operator)
	}
	l, r := a.node(t.Left, inPredicate), a.node(t.Right, inPredicate)
	v := val{cost: step(inPredicate) + sum(l, r), size: 8}
	switch t.Operator {
	case "==", "!=", "<", "<=", ">", ">=":
		// Comparing stops at the shorter side.
		v.cost += min(sizeWork(l, inPredicate), sizeWork(r, inPredicate))
	case "contains", "startsWith", "endsWith":
		v.cost += byteWork(l, inPredicate) + byteWork(r, inPredicate)
	case "in":
		v.cost += sizeWork(r, inPredicate) + byteWork(l, inPredicate)
	case "matches":
		if lit, ok := t.Right.(*ast.StringNode); ok {
			v.cost += regexInsts(lit.Value) * byteWork(l, inPredicate)
		}
	case "..":
		v.cost += maxRange * nodeUnits
		v.size = maxRange * 8
	}
	return v
}

func (a *allow) builtin(t *ast.BuiltinNode, inPredicate bool) val {
	st := step(inPredicate)
	switch {
	case t.Name == "date" && len(t.Arguments) > 2:
		// A time zone argument is loaded from the zone database on every call.
		a.fail(t, "date() with a time zone")
	case strings.HasPrefix(t.Name, "trim") && len(t.Arguments) == 2 && !trimLiteral(t.Name, t.Arguments[1]):
		// strings.Trim with a non-ASCII cutset tests every rune against every cutset rune.
		a.fail(t.Arguments[1], t.Name+"() with a computed or non-ASCII (or over 16 bytes) cutset")
	case scalarBuiltins[t.Name]:
		args := make([]val, len(t.Arguments))
		work := int64(0)
		for i, x := range t.Arguments {
			args[i] = a.node(x, inPredicate)
			work += byteWork(args[i], inPredicate)
		}
		v := val{cost: st + sum(args...), size: 8}
		switch t.Name {
		case "lower", "upper":
			// Case mapping may grow a string by half (ȿ to Ȿ): later operations see the larger bound.
			v.cost += caseFactor * work
			v.bytes, v.item = args[0].bytes*3/2+1, args[0].item
			v.size = v.bytes
		case "trim", "trimPrefix", "trimSuffix":
			v.cost += work
			v.bytes, v.size, v.item = args[0].bytes, args[0].bytes, args[0].item
		case "int", "float", "duration":
			v.cost += work
		case "date":
			v.cost += work + dateUnits*items(val{}, inPredicate)
		}
		return v
	case predicateBuiltins[t.Name]:
		if inPredicate {
			a.fail(t, t.Name+"() inside another predicate")
			return val{}
		}
		if len(t.Arguments) != 2 {
			a.fail(t, t.Name+"() without a predicate")
			return val{}
		}
		p, ok := t.Arguments[1].(*ast.PredicateNode)
		if !ok {
			a.fail(t.Arguments[1], t.Name+"() without a predicate")
			return val{}
		}
		arr, body := a.node(t.Arguments[0], false), a.node(p.Node, true)
		v := val{cost: st + nodeUnits*MaxItems + sum(arr, body), size: 8}
		if t.Name == "filter" {
			v.size = arr.size
		}
		return v
	default:
		a.fail(t, t.Name+"()")
	}
	return val{}
}

// Cost is the static cost of src in model units, or an error when it does not compile.
func Cost(src string) (int64, error) {
	tree, err := check(src)
	if err != nil {
		return 0, err
	}
	a := &allow{src: src}
	v := a.node(tree.Node, false)
	if a.err != nil {
		return 0, a.err
	}
	return v.cost, nil
}

func config() *conf.Config {
	cfg := conf.CreateNew()
	for _, op := range options {
		op(cfg)
	}
	for name := range cfg.Disabled {
		delete(cfg.Builtins, name)
	}
	cfg.Check()
	return cfg
}

func check(src string) (*parser.Tree, error) {
	if len(src) > MaxLen {
		return nil, &Error{Line: 1, Column: 1, Message: fmt.Sprintf("longer than %d characters", MaxLen)}
	}
	tree, err := checker.ParseCheck(src, config())
	if err != nil {
		return nil, fromExpr(err)
	}
	return tree, nil
}

// Compile checks src against Env, the allowlist and the cost budget, then compiles that same checked
// tree; an empty filter compiles to nil (match all).
func Compile(src string) (*Program, error) {
	if src == "" {
		return nil, nil
	}
	tree, err := check(src)
	if err != nil {
		return nil, err
	}
	a := &allow{src: src}
	v := a.node(tree.Node, false)
	if a.err != nil {
		return nil, a.err
	}
	if v.cost > CostBudget {
		return nil, &Error{Line: 1, Column: 1, Message: fmt.Sprintf("costs %d units, above the budget of %d: "+
			"fewer operations, or string work on # (the items) rather than per item", v.cost, CostBudget)}
	}
	ast.Walk(&tree.Node, arrayGuard{})
	cfg := config()
	if err := optimizer.Optimize(&tree.Node, cfg); err != nil {
		return nil, fromExpr(err)
	}
	p, err := compiler.Compile(tree, cfg)
	if err != nil {
		return nil, fromExpr(err)
	}
	return p, nil
}

// goNames turns the checker's Go type names into the names a filter writer sees.
var goNames = strings.NewReplacer("type filter.Event", "event", "type filter.Watch", "watch", "filter.Env", "the filter environment",
	"filter.Event", "event", "filter.Watch", "watch")

func fromExpr(err error) error {
	var fe *file.Error
	if errors.As(err, &fe) {
		return &Error{Line: max(fe.Line, 1), Column: fe.Column + 1, Message: goNames.Replace(fe.Message)}
	}
	return &Error{Line: 1, Column: 1, Message: err.Error()}
}

// Sanitize bounds what an evaluation sees of a payload: strings cut to MaxString bytes and maxRest in
// all, arrays and maps to MaxItems entries (map keys in order, so the cut is the same every time),
// nesting to maxDepth, and maxValues values in all. A top-level string body is cut to MaxBody instead,
// on bytes of its own, so maxBytes holds and the other fields keep theirs. It returns a copy.
func Sanitize(payload map[string]any) map[string]any {
	b := budget{values: maxValues, bytes: maxRest}
	out, _ := b.sanitize(payload, 0).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

type budget struct{ values, bytes int }

func (b *budget) sanitize(v any, depth int) any {
	if b.values <= 0 || depth > maxDepth {
		return nil
	}
	b.values--
	switch t := v.(type) {
	case string:
		n := min(len(t), MaxString, b.bytes)
		b.bytes -= n
		return strings.ToValidUTF8(t[:n], "")
	case []any:
		out := make([]any, 0, min(len(t), MaxItems))
		for _, x := range t[:min(len(t), MaxItems)] {
			out = append(out, b.sanitize(x, depth+1))
		}
		return out
	case map[string]any:
		out := make(map[string]any, min(len(t), MaxItems))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if len(out) == MaxItems {
				break
			}
			if s, ok := t[k].(string); ok && depth == 0 && k == "body" {
				out[k] = strings.ToValidUTF8(s[:min(len(s), MaxBody)], "")
				continue
			}
			out[k] = b.sanitize(t[k], depth+1)
		}
		return out
	}
	return v
}

// Match runs p on env; a runtime error (a payload field of an unexpected type) is returned, never a panic.
func Match(p *Program, env Env) (bool, error) {
	if p == nil {
		return true, nil
	}
	out, err := expr.Run(p, env)
	if err != nil {
		// int(body) quotes its whole input, and this error reaches the logs and the rule preview.
		msg := err.Error()
		return false, errors.New("evaluate filter: " + strings.ToValidUTF8(msg[:min(len(msg), maxEvalError)], ""))
	}
	b, ok := out.(bool)
	if !ok {
		return false, fmt.Errorf("evaluate filter: result is %T, not a boolean", out)
	}
	return b, nil
}
