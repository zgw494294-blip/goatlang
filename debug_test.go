package goatlang

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

const debugFile = "test.go"

func debugNew(t *testing.T, src string) (*VM, *DebugSession, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	vm := New(WithStdout(out))
	s, err := vm.Debug(nil, debugFile, src)
	if err != nil {
		t.Fatalf("Debug error: %v", err)
	}
	return vm, s, out
}

// loc stringifies the current frame for compact assertions.
func debugLoc(ev DebugEvent) string {
	if len(ev.Frames) == 0 {
		return ev.State
	}
	f := ev.Frames[0]
	return fmt.Sprintf("%s:%d:%s", f.File, f.Line, f.Function)
}

// do sends a command and fails on transport errors; runtime error events are
// returned for the caller to assert on.
func debugDo(t *testing.T, s *DebugSession, cmd string) DebugEvent {
	t.Helper()
	ev, err := s.Command(cmd)
	if err != nil && ev.State != DebugError {
		t.Fatalf("Command %q error: %v", cmd, err)
	}
	return ev
}

func debugSteps(t *testing.T, s *DebugSession) []string {
	t.Helper()
	var stops []string
	for {
		ev := debugDo(t, s, "step")
		if ev.State != DebugPaused {
			stops = append(stops, ev.State)
			return stops
		}
		stops = append(stops, debugLoc(ev))
	}
}

func TestDebug_entry(t *testing.T) {
	src := `func add(a int, b int) int {
	return a + b
}
x := add(1, 2)
x`
	_, s, _ := debugNew(t, src)
	cur := s.Current()
	assert(t, "state", cur.State, DebugPaused)
	assert(t, "reason", cur.Reason, "entry")
	assert(t, "loc", debugLoc(cur), debugFile+":4:main")
	if _, ok := cur.Globals["x"]; ok {
		t.Fatalf("x should not be bound before line 4 executes, got %v", cur.Globals)
	}
	if g, ok := cur.Globals["add"]; !ok || g.Type != "func" {
		t.Fatalf("function declaration add should be bound, got %v", cur.Globals)
	}
	if len(cur.Frames) != 1 || cur.Frames[0].Function != "main" || len(cur.Frames[0].Locals) != 0 {
		t.Fatalf("root frame wrong: %+v", cur.Frames)
	}

	ev := debugDo(t, s, "step")
	assert(t, "into loc", debugLoc(ev), debugFile+":2:add")
	if ev.Frames[0].Locals["a"] != (DebugValue{Type: "int32", Text: "1"}) {
		t.Fatalf("a local got %+v", ev.Frames[0].Locals["a"])
	}
	if ev.Frames[0].Locals["b"] != (DebugValue{Type: "int32", Text: "2"}) {
		t.Fatalf("b local got %+v", ev.Frames[0].Locals["b"])
	}
	caller := ev.Frames[1]
	assert(t, "caller file", caller.File, debugFile)
	assert(t, "caller func", caller.Function, "main")
	assert(t, "caller line is calling statement", caller.Line, 4)

	ev = debugDo(t, s, "step")
	assert(t, "back to root", debugLoc(ev), debugFile+":5:main")
	if ev.Globals["x"] != (DebugValue{Type: "int32", Text: "3"}) {
		t.Fatalf("x global got %+v", ev.Globals["x"])
	}

	ev = debugDo(t, s, "step")
	assert(t, "completed", ev.State, DebugCompleted)
	if len(ev.Results) != 1 || ev.Results[0] != (DebugValue{Type: "int32", Text: "3"}) {
		t.Fatalf("results got %+v", ev.Results)
	}
}

func TestDebug_next_out_recursion(t *testing.T) {
	src := `func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}
v := fib(3)
v`
	_, s, _ := debugNew(t, src)
	// next at root skips the whole recursive computation
	ev := debugDo(t, s, "next")
	assert(t, "next over fib", debugLoc(ev), debugFile+":8:main")
	ev = debugDo(t, s, "next")
	assert(t, "next to end", ev.State, DebugCompleted)
	assert(t, "fib result", ev.Results[0].Text, "2")

	// next inside fib skips the recursive calls and the sibling call in the
	// same expression, landing on an ancestor statement only once.
	_, s2, _ := debugNew(t, src)
	debugDo(t, s2, "step") // line 7 -> line 2 fib(3)
	assert(t, "in fib", debugLoc(s2.Current()), debugFile+":2:fib")
	ev = debugDo(t, s2, "next")
	assert(t, "if to return expr", debugLoc(ev), debugFile+":5:fib")
	ev = debugDo(t, s2, "next")
	assert(t, "siblings skipped, ancestor next stmt", debugLoc(ev), debugFile+":8:main")

	// out runs to completion when issued at root
	_, s3, _ := debugNew(t, src)
	ev = debugDo(t, s3, "out")
	assert(t, "root out completes", ev.State, DebugCompleted)
}

func TestDebug_out(t *testing.T) {
	src := `func inner(a int) int {
	x := a + 1
	return x
}
func outer() int {
	r := inner(41)
	return r
}
y := outer()
y`
	_, s, _ := debugNew(t, src)
	debugDo(t, s, "step") // line 9 -> 6 outer
	debugDo(t, s, "step") // -> line 2 inner
	ev := debugDo(t, s, "out")
	assert(t, "out to caller next stmt", debugLoc(ev), debugFile+":7:outer")
	ev = debugDo(t, s, "out")
	assert(t, "out to root next stmt", debugLoc(ev), debugFile+":10:main")
	ev = debugDo(t, s, "out")
	assert(t, "root out ends", ev.State, DebugCompleted)
	assert(t, "result", ev.Results[0].Text, "42")
}

func TestDebug_controlFlow(t *testing.T) {
	src := `x := 0
if x == 0 {
	x = 1
} else {
	x = 2
}
for i := 0; i < 3; i++ {
	x += i
}
s := []int{10, 20}
var sum int
for _, v := range s {
	sum += v
}
switch sum {
case 30:
	x = x + sum
default:
	x = 0
}
x`
	_, s, _ := debugNew(t, src)
	stops := debugSteps(t, s)
	want := []string{
		debugFile + ":2:main",
		debugFile + ":3:main",
		debugFile + ":7:main",
		debugFile + ":8:main",
		debugFile + ":8:main",
		debugFile + ":8:main",
		debugFile + ":10:main",
		debugFile + ":11:main",
		debugFile + ":12:main",
		debugFile + ":13:main",
		debugFile + ":13:main",
		debugFile + ":15:main",
		debugFile + ":17:main",
		debugFile + ":21:main",
		DebugCompleted,
	}
	if fmt.Sprint(stops) != fmt.Sprint(want) {
		t.Fatalf("steps got %v want %v", stops, want)
	}

	// break is itself a statement entry checkpoint
	src2 := `v := 0
for i := 0; i < 5; i++ {
	if i == 2 {
		break
	}
	v += i
}
v`
	_, s2, _ := debugNew(t, src2)
	stops = debugSteps(t, s2)
	want2 := []string{
		debugFile + ":2:main",
		debugFile + ":3:main",
		debugFile + ":6:main",
		debugFile + ":3:main",
		debugFile + ":6:main",
		debugFile + ":3:main",
		debugFile + ":4:main",
		debugFile + ":8:main",
		DebugCompleted,
	}
	if fmt.Sprint(stops) != fmt.Sprint(want2) {
		t.Fatalf("break steps got %v want %v", stops, want2)
	}
}

func TestDebug_breakpoints(t *testing.T) {
	src := `func unused() int {
	return 99
}
for i := 0; i < 3; i++ {
	print(i)
}
done := 1
done`
	_, s, out := debugNew(t, src)
	// breakpoint in a never called function is bindable
	if err := s.SetBreakpoints(debugFile, []int{2, 5}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	// line 2 never hits; line 5 hits every iteration
	var hits int
	for {
		ev, err := s.Command("continue")
		if err != nil {
			t.Fatalf("continue: %v", err)
		}
		if ev.State == DebugCompleted {
			break
		}
		assert(t, "bp reason", ev.Reason, "breakpoint")
		assert(t, "bp line", ev.Frames[0].Line, 5)
		hits++
	}
	assert(t, "iteration hits", hits, 3)
	assert(t, "stdout", out.String(), "012")

	// validation: wrong file, declaration line, brace line are rejected
	_, s2, _ := debugNew(t, src)
	if err := s2.SetBreakpoints("other.go", []int{5}); err == nil {
		t.Fatalf("expected file mismatch error")
	}
	if err := s2.SetBreakpoints(debugFile, []int{1}); err == nil {
		t.Fatalf("expected error for function declaration line")
	}
	if err := s2.SetBreakpoints(debugFile, []int{3}); err == nil {
		t.Fatalf("expected error for brace-only line")
	}
	if err := s2.SetBreakpoints(debugFile, []int{5, 99}); err == nil {
		t.Fatalf("expected error for out of range line")
	}
	// rejected set must leave previous breakpoints untouched (none yet)
	// empty list clears
	if err := s2.SetBreakpoints(debugFile, []int{5}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	if err := s2.SetBreakpoints(debugFile, nil); err != nil {
		t.Fatalf("clear bp: %v", err)
	}
	ev := debugDo(t, s2, "continue")
	assert(t, "cleared runs to end", ev.State, DebugCompleted)

	// breakpoint does not immediately re-stop the statement it is parked on
	_, s3, _ := debugNew(t, "x := 1\nx\n")
	if err := s3.SetBreakpoints(debugFile, []int{1}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	ev = debugDo(t, s3, "continue")
	assert(t, "no immediate re-stop", ev.State, DebugCompleted)

	// replacement semantics
	_, s4, _ := debugNew(t, src)
	if err := s4.SetBreakpoints(debugFile, []int{7}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	if err := s4.SetBreakpoints(debugFile, []int{5}); err != nil {
		t.Fatalf("replace bp: %v", err)
	}
	ev = debugDo(t, s4, "continue")
	assert(t, "only new bp hits", ev.Frames[0].Line, 5)
}

func TestDebug_breakpointPreemptsNext(t *testing.T) {
	src := `func f(n int) int {
	return n + 1
}
x := f(41)
x`
	_, s, _ := debugNew(t, src)
	if err := s.SetBreakpoints(debugFile, []int{2}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	// next at root would skip the call, but a real breakpoint preempts it
	ev, err := s.Command("next")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	assert(t, "state", ev.State, DebugPaused)
	assert(t, "reason breakpoint wins", ev.Reason, "breakpoint")
	assert(t, "bp inside callee", debugLoc(ev), debugFile+":2:f")
}

func TestDebug_locals(t *testing.T) {
	src := `func f(x int) int {
	y := x + 1
	if x > 0 {
		y := 10
		y
	}
	return y
}
z := f(41)
z`
	_, s, _ := debugNew(t, src)
	debugDo(t, s, "step") // line 9 -> 2
	ev := s.Current()
	if _, ok := ev.Frames[0].Locals["y"]; ok {
		t.Fatalf("y not bound before its declaration: %+v", ev.Frames[0].Locals)
	}
	assert(t, "param", ev.Frames[0].Locals["x"].Text, "41")
	debugDo(t, s, "step") // line 3
	debugDo(t, s, "step") // line 4: inner y not yet bound
	ev = s.Current()
	if ev.Frames[0].Line != 4 {
		t.Fatalf("want line 4 got %d", ev.Frames[0].Line)
	}
	assert(t, "outer y visible during inner decl entry", ev.Frames[0].Locals["y"].Text, "42")
	debugDo(t, s, "step") // line 5
	ev = s.Current()
	assert(t, "inner shadows outer", ev.Frames[0].Locals["y"].Text, "10")
	debugDo(t, s, "step") // line 7
	ev = s.Current()
	assert(t, "outer y restored after block", ev.Frames[0].Locals["y"].Text, "42")

	// during a declaration initializer call the caller still sees the old
	// binding; the new local is not presented early
	src2 := `func g(v int) int {
	return v + 1
}
func f(x int) int {
	a := g(x)
	return a
}
r := f(41)
r`
	_, s2, _ := debugNew(t, src2)
	debugDo(t, s2, "step")      // line 8 -> 5 f
	ev = debugDo(t, s2, "step") // line 5 -> line 2 g (inside g(x) initializer)
	assert(t, "inside g", debugLoc(ev), debugFile+":2:g")
	caller := ev.Frames[1]
	assert(t, "caller at initializer statement", caller.Line, 5)
	assert(t, "caller sees x", caller.Locals["x"].Text, "41")
	if _, ok := caller.Locals["a"]; ok {
		t.Fatalf("a must not be visible while its initializer is running")
	}
}

func TestDebug_methods_funcValues_receiver(t *testing.T) {
	src := `type T struct { X int }
func (rcv *T) Add(n int) int {
	return rcv.X + n
}
t := &T{X: 40}
v := t.Add(2)
fn := func(a int) int { return a * 2 }
w := fn(21)
v + w`
	_, s, _ := debugNew(t, src)
	debugDo(t, s, "continue") // run to end
	ev := s.Current()
	assert(t, "state", ev.State, DebugCompleted)
	if len(ev.Results) != 1 || ev.Results[0].Text != "84" {
		t.Fatalf("42 + 42 got %+v", ev.Results)
	}

	// receiver is a local of the method frame
	_, s2, _ := debugNew(t, src)
	debugDo(t, s2, "step") // line 5 -> line 6 (call statement)
	debugDo(t, s2, "step") // line 6 -> line 3 Add
	ev = s2.Current()
	assert(t, "method frame", debugLoc(ev), debugFile+":3:T.Add")
	if ev.Frames[0].Locals["rcv"].Text != "&{X:40}" {
		t.Fatalf("receiver got %+v", ev.Frames[0].Locals["rcv"])
	}
	if ev.Frames[0].Locals["n"] != (DebugValue{Type: "int32", Text: "2"}) {
		t.Fatalf("method arg got %+v", ev.Frames[0].Locals["n"])
	}
}

func TestDebug_valuesAndTypes(t *testing.T) {
	src := `i := 42
s := "hi"
l := []int{1, 2}
m := map[string]int{"k": 7}
type T struct { X int }
tt := &T{X: 9}
i`
	_, s, _ := debugNew(t, src)
	ev := debugDo(t, s, "continue")
	g := ev.Globals
	cases := map[string]DebugValue{
		"i":  {Type: "int32", Text: "42"},
		"s":  {Type: "string", Text: "hi"},
		"l":  {Type: "[]int32", Text: "[1 2]"},
		"m":  {Type: "map[string]int32", Text: "map[k:7]"},
		"tt": {Type: "T", Text: "&{X:9}"},
	}
	for name, want := range cases {
		if g[name] != want {
			t.Fatalf("%s got %+v want %+v", name, g[name], want)
		}
	}
}

func TestDebug_snapshotIsolation(t *testing.T) {
	src := `func f(x int) int {
	y := x + 1
	return y
}
a := f(1)
b := f(2)`
	_, s, _ := debugNew(t, src)
	debugDo(t, s, "step") // line 5 -> line 2 f(x=1)
	first := s.Current()
	first.Globals["tampered"] = DebugValue{Type: "x", Text: "x"}
	first.Frames[0].Locals["z"] = DebugValue{Type: "x", Text: "x"}
	// step through return, back to root, then enter the second call
	debugDo(t, s, "step") // line 3 return
	debugDo(t, s, "step") // line 6 root (a assigned)
	debugDo(t, s, "step") // line 2 f(x=2)
	second := s.Current()
	if _, ok := second.Globals["tampered"]; ok {
		t.Fatalf("returned global snapshot must be detached")
	}
	if _, ok := second.Frames[0].Locals["z"]; ok {
		t.Fatalf("returned locals snapshot must be detached")
	}
	if first.Frames[0].Locals["x"].Text != "1" || second.Frames[0].Locals["x"].Text != "2" {
		t.Fatalf("historical event changed: first=%v second=%v", first.Frames[0].Locals, second.Frames[0].Locals)
	}
	if first.Globals["a"].Text != "2" {
		// first event was taken before a existed
		if _, ok := first.Globals["a"]; ok {
			t.Fatalf("earlier event must not gain later globals: %+v", first.Globals)
		}
	}
}

func TestDebug_runtimeError(t *testing.T) {
	src := `x := 1
panic("boom")
x = 2`
	_, s, _ := debugNew(t, src)
	ev, err := s.Command("continue")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want error containing boom, got %v", err)
	}
	assert(t, "state", ev.State, DebugError)
	if !strings.Contains(ev.Error, "boom") {
		t.Fatalf("event error got %q", ev.Error)
	}
	assert(t, "effects before error kept", ev.Globals["x"].Text, "1")
	if _, ok := ev.Globals["x"]; !ok {
		t.Fatalf("globals must be inspectable on error: %+v", ev.Globals)
	}
	// terminal state: every command and breakpoint edit rejected, Current
	// keeps the terminal event
	if _, err := s.Command("step"); err == nil {
		t.Fatalf("step after error must fail")
	}
	if _, err := s.Command("abort"); err == nil {
		t.Fatalf("abort after error must fail")
	}
	if err := s.SetBreakpoints(debugFile, []int{1}); err == nil {
		t.Fatalf("set breakpoints after error must fail")
	}
	if s.Current().State != DebugError {
		t.Fatalf("Current terminal changed: %+v", s.Current())
	}
}

func TestDebug_divideByZero(t *testing.T) {
	_, s, _ := debugNew(t, "x := 1 / 0\n")
	ev, err := s.Command("continue")
	if err == nil {
		t.Fatalf("want runtime error")
	}
	assert(t, "state", ev.State, DebugError)
}

func TestDebug_abort(t *testing.T) {
	src := `probe()
x := 1
probe()
y := 2`
	n := 0
	out := &bytes.Buffer{}
	vm := New(WithStdout(out))
	vm.Set("main.probe", NewFunc(0, 0, func(*VM) { n++ }))
	s, err := vm.Debug(nil, debugFile, src)
	if err != nil {
		t.Fatalf("Debug: %v", err)
	}
	if err := s.SetBreakpoints(debugFile, []int{3}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	ev := debugDo(t, s, "continue")
	assert(t, "at bp", ev.Frames[0].Line, 3)
	assert(t, "one native call happened", n, 1)
	ev, _ = s.Command("abort")
	assert(t, "aborted", ev.State, DebugAborted)
	assert(t, "native calls not replayed or continued", n, 1)
	assert(t, "earlier effects kept", ev.Globals["x"].Text, "1")
	if _, ok := ev.Globals["y"]; ok {
		t.Fatalf("statements after abort must not run: %+v", ev.Globals)
	}
	if _, err := s.Command("continue"); err == nil {
		t.Fatalf("command after abort must fail")
	}
	if s.Current().State != DebugAborted {
		t.Fatalf("Current terminal lost")
	}
	// owner VM remains usable for normal Eval
	if _, err := vm.Eval(nil, "after", "z := 42; z"); err != nil {
		t.Fatalf("Eval after abort: %v", err)
	}
}

func TestDebug_unknownCommand(t *testing.T) {
	_, s, _ := debugNew(t, "x := 1\nx\n")
	if _, err := s.Command("gizmo"); err == nil {
		t.Fatalf("unknown command must error")
	}
	// session remains usable
	ev := debugDo(t, s, "step")
	assert(t, "still paused", ev.State, DebugPaused)
	assert(t, "at line 2", ev.Frames[0].Line, 2)
}

func TestDebug_compileError(t *testing.T) {
	calls := 0
	vm := New()
	vm.Set("main.probe", NewFunc(0, 0, func(*VM) { calls++ }))
	s, err := vm.Debug(nil, debugFile, "probe()\nfunc (\n")
	if err == nil {
		t.Fatalf("expected compile error")
	}
	if s != nil {
		t.Fatalf("session must be nil on compile error")
	}
	assert(t, "no business execution", calls, 0)
	if _, err := vm.Eval(nil, "after", "1"); err != nil {
		t.Fatalf("vm unusable after failed debug: %v", err)
	}
}

func TestDebug_multilineStatement(t *testing.T) {
	src := "x := 1 +\n\t2\nx\n"
	_, s, _ := debugNew(t, src)
	assert(t, "entry uses start line", s.Current().Frames[0].Line, 1)
	if err := s.SetBreakpoints(debugFile, []int{2}); err == nil {
		t.Fatalf("continuation-only line must not be bindable")
	}
	ev := debugDo(t, s, "step")
	assert(t, "next statement", ev.Frames[0].Line, 3)
}

// debugValueEqual compares values structurally because map String output is
// order dependent.
func debugValueEqual(g1, g2 *lookup, a, b Value) bool {
	if a.t.str(g1) != b.t.str(g2) {
		return false
	}
	if a.IsNil() || b.IsNil() {
		return a.IsNil() == b.IsNil()
	}
	switch a.Type() {
	case TypeSlice:
		if b.Type() != TypeSlice || a.Len() != b.Len() {
			return false
		}
		ad, bd := a.data(), b.data()
		for i := range ad {
			if !debugValueEqual(g1, g2, ad[i], bd[i]) {
				return false
			}
		}
		return true
	case TypeMap:
		if b.Type() != TypeMap || a.Len() != b.Len() {
			return false
		}
		next := a.Range()
		for {
			k, v, ok := next()
			if !ok {
				break
			}
			bv, found := b.Get(k)
			if !found || !debugValueEqual(g1, g2, v, bv) {
				return false
			}
		}
		return true
	case TypeFunc:
		return b.Type() == TypeFunc
	default:
		return a.String() == b.String()
	}
}

// debugParity runs a program to completion with continue and compares
// results, stdout and globals against a plain Eval on an equivalent VM.
func debugParity(t *testing.T, name, src string, withProbe bool) {
	t.Run(name, func(t *testing.T) {
		out1, out2 := &bytes.Buffer{}, &bytes.Buffer{}
		vm1 := New(WithStdout(out1))
		vm2 := New(WithStdout(out2))
		var n1, n2 int
		if withProbe {
			vm1.Set("main.probe", NewFunc(0, 0, func(*VM) { n1++ }))
			vm2.Set("main.probe", NewFunc(0, 0, func(*VM) { n2++ }))
		}
		rets, err := vm1.Eval(nil, debugFile, src)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		s, err := vm2.Debug(nil, debugFile, src)
		if err != nil {
			t.Fatalf("Debug: %v", err)
		}
		ev, derr := s.Command("continue")
		if derr != nil {
			t.Fatalf("debug continue: %v", derr)
		}
		if ev.State != DebugCompleted {
			t.Fatalf("debug state %s error %q", ev.State, ev.Error)
		}
		if len(ev.Results) != len(rets) {
			t.Fatalf("results len got %d want %d", len(ev.Results), len(rets))
		}
		// ev.Results are snapshots; compare against the live Eval results.
		for i, r := range rets {
			if ev.Results[i].Type != strings.ReplaceAll(r.t.str(vm1.globals), "main.", "") ||
				ev.Results[i].Text != r.String() {
				t.Fatalf("result %d got %+v want %s/%s", i, ev.Results[i], r.t.str(vm1.globals), r.String())
			}
		}
		if out1.String() != out2.String() {
			t.Fatalf("stdout got %q want %q", out2.String(), out1.String())
		}
		// globals parity
		for i, key := range vm1.globals.indexToKey {
			if !isDebugGlobal(key) {
				continue
			}
			a := vm1.globals.Read(i)
			b := vm2.globals.Read(i)
			if !debugValueEqual(vm1.globals, vm2.globals, a, b) {
				t.Fatalf("global %s got %s/%s want %s/%s", key,
					b.t.str(vm2.globals), b.String(), a.t.str(vm1.globals), a.String())
			}
		}
		if withProbe && n1 != n2 {
			t.Fatalf("native calls got %d want %d", n2, n1)
		}
	})
}

func TestDebug_parity(t *testing.T) {
	debugParity(t, "comprehensive", `type T struct { X int }
func (rcv *T) Add(n int) int { return rcv.X + n }
a := []int{1, 2, 3}
m := map[string]int{"k": 4}
sum := 0
for _, v := range a { sum += v }
tt := &T{X: 40}
r := tt.Add(2)
fn := func(x int) int { return x * 2 }
q := fn(21)
b := a[1] + m["k"]
if sum == 6 { sum = 42 }
print("out")
r + q + b + sum`, false)

	debugParity(t, "nestedOrderedCalls", `func first() int { return 7 }
func second(n int) int { return n * 6 }
x := second(first())
x`, false)

	debugParity(t, "variadic", `func f(a ...int) int {
	r := 0
	for _, v := range a { r += v }
	return r
}
f(1, 2, 3, 4) + f([]int{10, 20}...)`, false)

	debugParity(t, "switch", `n := 3
v := 0
switch n {
case 1:
	v = 1
case 3:
	v = 42
default:
	v = 9
}
v`, false)

	debugParity(t, "mapRange", `m := map[int]int{1: 40, 2: 2}
r := 0
for k, v := range m { r += k + v }
r`, false)

	debugParity(t, "shadowing", `x := 1
func f(x int) int {
	x = x + 1
	if x > 0 {
		x := 100
		_ = x
	}
	return x
}
f(41) + x`, false)

	debugParity(t, "multiReturn", `func g() (int, int) { return 6, 7 }
func h() int { a, b := g(); return a * b }
h()`, false)

	debugParity(t, "nativeSideEffects", `probe()
probe()
x := []int{1}
x = append(x, 2)
probe()
len(x)`, true)

	debugParity(t, "containers", `s := []string{"a", "b"}
m := map[string][]int{"k": {1, 2}}
s[0] = "z"
ks := m["k"]
ks[1] = 5
s[0]`, false)
}

func TestDebug_sessionIsolationAndReuse(t *testing.T) {
	vm := New()
	s1, err := vm.Debug(nil, debugFile, "a := 1\n")
	if err != nil {
		t.Fatalf("debug1: %v", err)
	}
	if ev := debugDo(t, s1, "continue"); ev.State != DebugCompleted {
		t.Fatalf("session1: %s %s", ev.State, ev.Error)
	}
	// owner VM works normally after a finished session
	rets, err := vm.Eval(nil, "eval", "b := 2; a + b")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if rets[0].Int() != 3 {
		t.Fatalf("eval got %v", rets[0])
	}
	// clean new session: old state/breakpoints cannot leak (separate
	// session objects)
	s2, err := vm.Debug(nil, debugFile, "c := a + 10\nc\n")
	if err != nil {
		t.Fatalf("debug2: %v", err)
	}
	ev := s2.Current()
	assert(t, "fresh entry", ev.Reason, "entry")
	ev = debugDo(t, s2, "continue")
	if ev.Results[0].Text != "11" {
		t.Fatalf("new session sees owner globals but no session state: %+v", ev.Results)
	}

	// independent VMs do not share bindings
	other := New()
	if _, err := other.Debug(nil, debugFile, "x := 5\n"); err != nil {
		t.Fatalf("other debug: %v", err)
	}
	if vm.Get("main.x").t != TypeNil {
		t.Fatalf("globals leaked across VMs")
	}
}

func TestDebug_declOnlyAndEmpty(t *testing.T) {
	// only declarations: no statement ever executes, session is terminal
	vm := New()
	s, err := vm.Debug(nil, debugFile, "func f() {}\ntype T struct { X int }\n")
	if err != nil {
		t.Fatalf("Debug: %v", err)
	}
	assert(t, "decl only state", s.Current().State, DebugCompleted)
	if _, err := s.Command("step"); err == nil {
		t.Fatalf("command on completed session must fail")
	}
	if err := s.SetBreakpoints(debugFile, nil); err == nil {
		t.Fatalf("breakpoint edit on completed session must fail")
	}
	// empty input is also a completed no-op session
	s2, err := vm.Debug(nil, debugFile, "")
	if err != nil {
		t.Fatalf("empty debug: %v", err)
	}
	assert(t, "empty state", s2.Current().State, DebugCompleted)
}

func TestDebug_initEntry(t *testing.T) {
	src := `func init() {
	x = 42
}
var x int
x`
	_, s, _ := debugNew(t, src)
	// init body is invoked by the unmarked init declaration, so entry is
	// the first statement actually executing, inside init
	ev := s.Current()
	assert(t, "entry in init", debugLoc(ev), debugFile+":2:init")
	if len(ev.Frames) != 2 || ev.Frames[1].Function != "main" {
		t.Fatalf("caller frame wrong: %+v", ev.Frames)
	}
	if _, ok := ev.Globals["x"]; ok {
		t.Fatalf("x must not be bound before init body statement runs")
	}
	ev = debugDo(t, s, "continue")
	assert(t, "result", ev.Results[0].Text, "42")
}

func TestDebug_deadBranchBreakpoint(t *testing.T) {
	src := `x := 1
if false {
	x = 42
}
x`
	_, s, _ := debugNew(t, src)
	// real statement line in a never taken branch is bindable but never hits
	if err := s.SetBreakpoints(debugFile, []int{3}); err != nil {
		t.Fatalf("set bp: %v", err)
	}
	ev := debugDo(t, s, "continue")
	assert(t, "dead branch never hits", ev.State, DebugCompleted)
	assert(t, "value unchanged", ev.Results[0].Text, "1")
}

func TestDebug_abortAtEntry(t *testing.T) {
	_, s, out := debugNew(t, "print(\"never\")\n")
	ev, _ := s.Command("abort")
	assert(t, "aborted at entry", ev.State, DebugAborted)
	if ev.Globals == nil {
		t.Fatalf("globals snapshot expected on abort event")
	}
	assert(t, "no output before abort", out.String(), "")
}

func TestDebug_loopLocals(t *testing.T) {
	src := `r := 0
for i := 0; i < 3; i++ {
	r += i
}
for k, v := range []int{7, 8} {
	r += k + v
}
r`
	_, s, _ := debugNew(t, src)
	var sawI, sawKV bool
	for {
		ev, err := s.Command("step")
		if err != nil && ev.State != DebugError {
			t.Fatalf("step: %v", err)
		}
		if ev.State != DebugPaused {
			break
		}
		l := ev.Frames[0].Locals
		if ev.Frames[0].Line == 3 && !sawI {
			if i, ok := l["i"]; !ok || i.Text != "0" {
				t.Fatalf("loop init var i got %+v", l)
			}
			sawI = true
		}
		if ev.Frames[0].Line == 6 && !sawKV {
			if _, ok := l["k"]; !ok {
				t.Fatalf("range key not visible: %+v", l)
			}
			if _, ok := l["v"]; !ok {
				t.Fatalf("range value not visible: %+v", l)
			}
			if _, ok := l["i"]; ok {
				t.Fatalf("for-scoped i leaked into range frame: %+v", l)
			}
			sawKV = true
		}
	}
	if !sawI || !sawKV {
		t.Fatalf("missing loop checkpoints: for=%v range=%v", sawI, sawKV)
	}
}

func TestDebug_nilGlobalBinding(t *testing.T) {
	// an explicitly assigned nil is a real binding, distinct from a slot
	// that was never written
	_, s, _ := debugNew(t, "x = nil\nx\n")
	ev := debugDo(t, s, "continue")
	assert(t, "completed", ev.State, DebugCompleted)
	if g, ok := ev.Globals["x"]; !ok || g.Type != "any" || g.Text != "nil" {
		t.Fatalf("explicit nil global got %+v ok=%v", g, ok)
	}
}

func TestDebug_noImportsNoFS(t *testing.T) {
	// sys may be nil when the source has no imports
	vm := New()
	s, err := vm.Debug(nil, debugFile, "42\n")
	if err != nil {
		t.Fatalf("Debug: %v", err)
	}
	assert(t, "entry", s.Current().Reason, "entry")
	ev := debugDo(t, s, "continue")
	assert(t, "result", ev.Results[0].Text, "42")
}

func TestDebug_importsStillRun(t *testing.T) {
	vm := New()
	s, err := vm.Debug(mapFS{}, debugFile, `import "fmt"
x := fmt.Sprint(42)
x`)
	if err != nil {
		t.Fatalf("Debug: %v", err)
	}
	ev := debugDo(t, s, "continue")
	if ev.State != DebugCompleted || ev.Results[0].Text != "42" {
		t.Fatalf("imports event: %+v", ev)
	}
}
