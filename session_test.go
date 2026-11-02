package goatlang

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func resumeToDone(t *testing.T, vm *VM, src string, opts ...func(*VM)) (string, []byte, *bytes.Buffer) {
	t.Helper()
	stdout := &bytes.Buffer{}
	vm.stdout = stdout
	s, err := vm.NewSession(nil, "test.goat", src)
	if err != nil {
		t.Fatalf("NewSession error: %v", err)
	}
	if s.Status() != StatusReady {
		t.Fatalf("status got %v want ready", s.Status())
	}
	var data []byte
	for {
		if err := s.Resume(); err != nil {
			t.Fatalf("Resume error: %v", err)
		}
		if s.Status() == StatusDone {
			break
		}
		if s.Status() != StatusPaused {
			t.Fatalf("unexpected status %v", s.Status())
		}
		b, err := s.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot error: %v", err)
		}
		if len(b) == 0 {
			t.Fatalf("empty snapshot")
		}
		data = b
		// Simulate a brand new process: new VM with the same named bindings.
		vm2 := New(WithStdout(stdout))
		for _, o := range opts {
			o(vm2)
		}
		s2, err := vm2.RestoreSession(nil, "test.goat", src, b)
		if err != nil {
			t.Fatalf("RestoreSession error: %v", err)
		}
		if s2.Status() != StatusPaused {
			t.Fatalf("restored status got %v want paused", s2.Status())
		}
		s = s2
		vm = vm2
	}
	values(t, s)
	var ss []string
	for _, v := range s.Values() {
		ss = append(ss, v.String())
	}
	return strings.Join(ss, " "), data, stdout
}

func values(t *testing.T, s *Session) {
	t.Helper()
	if s.Status() == StatusReady || s.Status() == StatusPaused {
		if s.Values() != nil {
			t.Fatalf("Values must be nil before done")
		}
	}
}

func TestSession_basic(t *testing.T) {
	src := `
x := 40
checkpoint()
x + 2
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "result", got, "42")
}

func TestSession_noCheckpoint(t *testing.T) {
	vm := New()
	s, err := vm.NewSession(nil, "test.goat", "6 * 7")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	assert(t, "status", s.Status(), StatusDone)
	assert(t, "value", s.Values()[0].Int(), 42)
}

func TestSession_twoCheckpoints(t *testing.T) {
	src := `
for i := 0; i < 3; i++ {
	checkpoint()
	println(i)
}
`
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	s, _ := vm.NewSession(nil, "test.goat", src)
	for i := 0; i < 3; i++ {
		if err := s.Resume(); err != nil {
			t.Fatalf("Resume %v: %v", i, err)
		}
		assert(t, "paused "+string(rune('0'+i)), s.Status(), StatusPaused)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("final Resume: %v", err)
	}
	assert(t, "done", s.Status(), StatusDone)
	assert(t, "stdout", stdout.String(), "0\n1\n2\n")
}

func TestSession_forLoop(t *testing.T) {
	src := `
sum := 0
for i := 0; i < 10; i++ {
	sum += i
	if i == 4 {
		checkpoint()
	}
}
sum
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "sum", got, "45")
}

func TestSession_sliceRange(t *testing.T) {
	src := `
s := []int{1, 2, 3, 4, 5}
sum := 0
for i, v := range s {
	sum += v
	if i == 2 {
		checkpoint()
	}
}
sum
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "sum", got, "15")
}

func TestSession_stringRange(t *testing.T) {
	src := `
r := 0
for i, c := range "abc" {
	r += i + int(c)
	if i == 1 {
		checkpoint()
	}
}
r
`
	got, _, _ := resumeToDone(t, New(), src)
	// 0+97 + 1+98 + 2+99 = 297
	assert(t, "runes", got, "297")
}

func TestSession_mapRange(t *testing.T) {
	src := `
m := map[int]int{1: 10, 2: 20, 3: 30, 4: 40}
sum := 0
n := 0
for k, v := range m {
	sum += k + v
	n++
	if n == 2 {
		checkpoint()
	}
}
sum
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "sum", got, "110")
}

func TestSession_recursion(t *testing.T) {
	src := `
func f(n int) int {
	if n == 0 {
		return 0
	}
	checkpoint()
	return 1 + f(n-1)
}
r := f(5)
r
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "recursive", got, "5")
}

func TestSession_nestedCalls(t *testing.T) {
	src := `
func inner(x int) int {
	checkpoint()
	return x + 2
}
func outer(x int) int {
	return inner(x*2) + 10
}
r := outer(15)
r
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "nested", got, "42")
}

func TestSession_pendingExpression(t *testing.T) {
	src := `
func mid() int {
	checkpoint()
	return 40
}
x := mid() + 2
x
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "pending", got, "42")
}

func TestSession_multiReturn(t *testing.T) {
	src := `
func pair() (int, int) {
	checkpoint()
	return 40, 2
}
a, b := pair()
a + b
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "multi", got, "42")
}

func TestSession_globalsAndContainers(t *testing.T) {
	src := `
g := map[string]int{"a": 1}
s := []int{1, 2, 3}
g["b"] = 2
s[2] = 30
checkpoint()
g["a"] + g["b"] + s[2]
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "state", got, "33")
}

func TestSession_sharedSubSlice(t *testing.T) {
	src := `
a := []int{1, 2, 3, 4}
b := a[1:3]
checkpoint()
b[0] = 40
b = append(b, 5)
a[1] + a[3]
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "shared", got, "45")
}

func TestSession_structCycle(t *testing.T) {
	src := `
type T struct {
	N int
	Next *T
}
t := &T{N: 1}
t.Next = t
checkpoint()
t.Next.Next.Next.N
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "cycle", got, "1")
}

func TestSession_boundMethod(t *testing.T) {
	src := `
type T struct { X int }
func (t *T) M() int {
	checkpoint()
	return t.X
}
func run() int {
	t := &T{X: 42}
	return t.M()
}
r := run()
r
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "method", got, "42")
}

func TestSession_funcInContainer(t *testing.T) {
	src := `
func g(x int) int {
	checkpoint()
	return x * 2
}
type T struct {
	F func(int) int
}
t := &T{F: g}
sl := []func(int) int{g}
t.F(20) + sl[0](1)
`
	install := func(vm *VM) {}
	got, _, _ := resumeToDone(t, New(), src, install)
	assert(t, "funcValues", got, "42")
}

func TestSession_pausedGetSet(t *testing.T) {
	src := `
x := 40
checkpoint()
x + 2
`
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	s, _ := vm.NewSession(nil, "test.goat", src)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if vm.Get("main.x").Int() != 40 {
		t.Fatalf("Get at pause got %v want 40", vm.Get("main.x"))
	}
	vm.Set("main.x", Int(41))
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	vm2 := New()
	s2, err := vm2.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	// Resume must continue past the checkpoint, not stop at it again.
	if s2.Status() != StatusDone {
		t.Fatalf("status after resume got %v want done", s2.Status())
	}
	assert(t, "result", s2.Values()[0].Int(), 43)
}

func TestSession_nativeOnce(t *testing.T) {
	src := `
n := work()
checkpoint()
r := more()
n + r
`
	stdout := &bytes.Buffer{}
	mkVM := func() (*VM, *int, *int) {
		workCalls := 0
		moreCalls := 0
		vm := New(WithStdout(stdout))
		vm.Set("main.work", NewFunc(0, 1, func(vm *VM) Value {
			workCalls++
			fmt.Fprintln(stdout, "work")
			return Int(40)
		}))
		vm.Set("main.more", NewFunc(0, 1, func(vm *VM) Value {
			moreCalls++
			fmt.Fprintln(stdout, "more")
			return Int(2)
		}))
		return vm, &workCalls, &moreCalls
	}
	vm1, work1, _ := mkVM()
	s, err := vm1.NewSession(nil, "test.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume1: %v", err)
	}
	assert(t, "work calls pre snapshot", *work1, 1)
	assert(t, "stdout pre snapshot", stdout.String(), "work\n")
	data, _ := s.Snapshot()

	vm2, work2, more2 := mkVM()
	s2, err := vm2.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	assert(t, "completed call not replayed", *work2, 0)
	assert(t, "pending call executed once", *more2, 1)
	assert(t, "stdout total", stdout.String(), "work\nmore\n")
	assert(t, "result", s2.Values()[0].Int(), 42)
}

func TestSession_nativeInContainer(t *testing.T) {
	src := `
x := []func(int) int{double}
checkpoint()
r := x[0](21)
r
`
	install := func(vm *VM) {
		vm.Set("main.double", NewFunc(1, 1, func(vm *VM, args []Value) Value {
			return args[0].opAdd(args[0])
		}))
	}
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	install(vm)
	s, _ := vm.NewSession(nil, "test.goat", src)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New()
	install(vm2)
	s2, err := vm2.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	assert(t, "native container", s2.Values()[0].Int(), 42)
}

func TestSession_independentRestores(t *testing.T) {
	src := `
x := 1
checkpoint()
x
`
	vm := New()
	s, _ := vm.NewSession(nil, "test.goat", src)
	s.Resume()
	data, _ := s.Snapshot()
	vmA := New()
	sa, err := vmA.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("restore A: %v", err)
	}
	vmB := New()
	sb, err := vmB.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("restore B: %v", err)
	}
	vmA.Set("main.x", Int(99))
	if vmB.Get("main.x").Int() != 1 {
		t.Fatalf("restores share mutable heap: %v", vmB.Get("main.x"))
	}
	if vmA.Get("main.x").Int() != 99 {
		t.Fatalf("set lost: %v", vmA.Get("main.x"))
	}
	_ = sa
	_ = sb
}

func TestSession_stateMachine(t *testing.T) {
	vm := New()
	s, err := vm.NewSession(nil, "test.goat", "checkpoint()")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := s.Snapshot(); err == nil {
		t.Fatalf("Snapshot in ready must error")
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if s.Status() != StatusPaused {
		t.Fatalf("want paused")
	}
	b1, err := s.Snapshot()
	if err != nil || len(b1) == 0 {
		t.Fatalf("paused snapshot: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume done: %v", err)
	}
	if s.Status() != StatusDone {
		t.Fatalf("want done")
	}
	if err := s.Resume(); err == nil {
		t.Fatalf("Resume done must error")
	}
	if _, err := s.Snapshot(); err == nil {
		t.Fatalf("Snapshot done must error")
	}
	if err := s.Close(); err == nil {
		t.Fatalf("Close done must error")
	}
	if s.Status() != StatusDone {
		t.Fatalf("terminal status changed")
	}

	vm2 := New()
	s2, _ := vm2.NewSession(nil, "test.goat", "checkpoint()")
	if err := s2.Close(); err != nil {
		t.Fatalf("Close ready: %v", err)
	}
	if s2.Status() != StatusClosed {
		t.Fatalf("want closed")
	}
	if err := s2.Resume(); err == nil {
		t.Fatalf("Resume closed must error")
	}
	if _, err := s2.Snapshot(); err == nil {
		t.Fatalf("Snapshot closed must error")
	}
	if err := s2.Close(); err == nil {
		t.Fatalf("double Close must error")
	}
}

func TestSession_failed(t *testing.T) {
	src := `
checkpoint()
panic("boom")
`
	vm := New()
	s, _ := vm.NewSession(nil, "test.goat", src)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume pause: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New()
	s2, _ := vm2.RestoreSession(nil, "test.goat", src, data)
	err := s2.Resume()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom error, got %v", err)
	}
	assert(t, "failed", s2.Status(), StatusFailed)
	if err := s2.Resume(); err == nil {
		t.Fatalf("Resume failed must error")
	}
	if s2.Status() != StatusFailed {
		t.Fatalf("failed status changed")
	}
}

func TestSession_restoreErrors(t *testing.T) {
	src := `
x := work()
checkpoint()
x
`
	install := func(vm *VM) {
		vm.Set("main.work", NewFunc(0, 1, func(vm *VM) Value { return Int(42) }))
	}
	vm := New()
	install(vm)
	s, _ := vm.NewSession(nil, "test.goat", src)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	data, _ := s.Snapshot()

	t.Run("differentSource", func(t *testing.T) {
		vm2 := New()
		install(vm2)
		_, err := vm2.RestoreSession(nil, "test.goat", src+"\n", data)
		if err == nil || !strings.Contains(err.Error(), "source") {
			t.Fatalf("want source error, got %v", err)
		}
	})

	t.Run("missingBinding", func(t *testing.T) {
		stdout := &bytes.Buffer{}
		calls := 0
		vm2 := New(WithStdout(stdout))
		// work not installed; install a sentinel native to detect calls.
		vm2.Set("main.sentinel", NewFunc(0, 0, func(vm *VM) { calls++ }))
		_, err := vm2.RestoreSession(nil, "test.goat", src, data)
		if err == nil || !strings.Contains(err.Error(), "main.work") {
			t.Fatalf("want missing binding error, got %v", err)
		}
		if calls != 0 || stdout.Len() != 0 {
			t.Fatalf("native side effects during failed restore")
		}
	})

	t.Run("incompatibleBinding", func(t *testing.T) {
		vm2 := New()
		vm2.Set("main.work", NewFunc(1, 1, func(vm *VM, args []Value) Value { return args[0] }))
		_, err := vm2.RestoreSession(nil, "test.goat", src, data)
		if err == nil || !strings.Contains(err.Error(), "main.work") {
			t.Fatalf("want incompatible binding error, got %v", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		bad := make([]byte, len(data))
		copy(bad, data)
		bad[len(bad)-1] ^= 0xff
		vm2 := New()
		install(vm2)
		if _, err := vm2.RestoreSession(nil, "test.goat", src, bad); err == nil {
			t.Fatalf("want corrupt error")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		vm2 := New()
		install(vm2)
		if _, err := vm2.RestoreSession(nil, "test.goat", src, data[:20]); err == nil {
			t.Fatalf("want truncated error")
		}
	})

	t.Run("garbage", func(t *testing.T) {
		vm2 := New()
		install(vm2)
		if _, err := vm2.RestoreSession(nil, "test.goat", src, []byte("not a snapshot at all")); err == nil {
			t.Fatalf("want garbage error")
		}
	})
}

func TestSession_wrapError(t *testing.T) {
	src := `
checkpoint()
1
`
	vm := New()
	vm.Set("ext", Wrap(&errorT{}))
	s, err := vm.NewSession(nil, "test.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := s.Snapshot(); err == nil || !strings.Contains(err.Error(), "Wrap") {
		t.Fatalf("want Wrap error, got %v", err)
	}
}

func TestSession_imports(t *testing.T) {
	sys := mapFS{
		"util/util.go": `package util; func Add(a, b int) int { return a + b }`,
	}
	src := `
import "util"
x := util.Add(40, 0)
checkpoint()
r := util.Add(x, 2)
r
`
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	s, err := vm.NewSession(sys, "main.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New(WithStdout(stdout))
	s2, err := vm2.RestoreSession(sys, "main.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	assert(t, "import call", s2.Values()[0].Int(), 42)
}

func TestSession_typesPreserved(t *testing.T) {
	src := `
b := byte(7)
f := 3.5
s := "xy"
bo := true
checkpoint()
__type(b) + " " + __type(f) + " " + __type(s) + " " + __type(bo)
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "types", got, "uint8 float64 string bool")
}

func TestSession_nilAndPrimitives(t *testing.T) {
	src := `
type T struct { X int }
var sl []int
var mp map[string]int
var fn func()
var st *T
u := 1
fl := 2.5
s := "hi"
b := false
n := sl
checkpoint()
__type(sl) + "|" + __type(mp) + "|" + __type(st) + "|" + s + "|" + __type(fl) + "|" + __type(b)
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "values", got, "[]int32|map[string]int32|main.T|hi|float64|bool")
}

func TestSession_variadic(t *testing.T) {
	src := `
func sum(a ...int) int {
	checkpoint()
	return a[0] + a[1] + a[2]
}
sl := []int{40, 1, 2}
r := sum(sl...)
r
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "variadic", got, "43")
}

func TestSession_lastStatementCheckpoint(t *testing.T) {
	src := `
func f() {
	x := 42
	checkpoint()
}
f()
`
	// checkpoint is the final statement: resume finishes the function and
	// the program.
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	s, err := vm.NewSession(nil, "test.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume1: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New()
	s2, err := vm2.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	assert(t, "done", s2.Status(), StatusDone)
}

func TestSession_nilRange(t *testing.T) {
	src := `
var sl []int
n := 0
for _, v := range sl {
	n += v
}
checkpoint()
n
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "nil range", got, "0")
}

func TestSession_sharedGlobalContainers(t *testing.T) {
	src := `
m := map[string]int{"k": 1}
a := []int{1, 2, 3}
checkpoint()
m["k"] = 41
a[1] = 41
m["k"] + a[1]
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "shared", got, "82")
}

func TestSession_overlappingSubSlices(t *testing.T) {
	src := `
a := []int{1, 2, 3, 4, 5}
b := a[1:3]
c := a[2:4]
checkpoint()
b = append(b, 40)
x := c[1]
// b grew within the shared capacity, so a[3]/c[1] changed
y := a[3]
// append beyond a's capacity allocates an independent array
z := append(a, 99)
z[0] = 7
x + y + a[0]
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "overlap", got, "81")
}

func TestSession_backtraceAfterRestore(t *testing.T) {
	src := `
func g() {
	panic("deep")
}
func f() {
	g()
}
checkpoint()
f()
`
	vm := New()
	s, _ := vm.NewSession(nil, "test.goat", src)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New()
	s2, err := vm2.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	err = s2.Resume()
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "deep") {
		t.Fatalf("want deep panic, got %v", err)
	}
	if !strings.Contains(err.Error(), "main.g") || !strings.Contains(err.Error(), "main.f") {
		t.Fatalf("want backtrace with main.g/main.f, got %v", err)
	}
	assert(t, "failed", s2.Status(), StatusFailed)
}

func TestSession_importMismatch(t *testing.T) {
	sys1 := mapFS{"util/util.go": `package util; func V() int { return 40 }`}
	sys2 := mapFS{"util/util.go": `package util; func V() int { return 41 }`}
	src := `
import "util"
x := util.V()
checkpoint()
x
`
	vm := New()
	s, err := vm.NewSession(sys1, "main.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New()
	if _, err := vm2.RestoreSession(sys2, "main.goat", src, data); err == nil {
		t.Fatalf("want import mismatch error")
	}
}

func TestSession_evalCheckpointNoop(t *testing.T) {
	vm := New()
	rets, err := vm.Eval(nil, "test", "checkpoint(); 42")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	assert(t, "eval result", rets[0].Int(), 42)
}

func TestSession_stdoutAcrossResume(t *testing.T) {
	src := `
println("a")
checkpoint()
println("b")
`
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	s, _ := vm.NewSession(nil, "test.goat", src)
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume1: %v", err)
	}
	data, _ := s.Snapshot()
	vm2 := New(WithStdout(stdout))
	s2, err := vm2.RestoreSession(nil, "test.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	assert(t, "stdout", stdout.String(), "a\nb\n")
}

func TestSession_boundMethodArgs(t *testing.T) {
	src := `
type T struct { X int }
func (t *T) Add(a, b int) int {
	checkpoint()
	return t.X + a + b
}
func run() (int, int) {
	tt := &T{X: 30}
	return tt.Add(10, 2), 0
}
a, b := run()
a + b
`
	got, _, _ := resumeToDone(t, New(), src)
	assert(t, "method args", got, "42")
}

func TestSession_checkpointInImportInit(t *testing.T) {
	sys := mapFS{
		"util/util.go": `package util
func init() {
	checkpoint()
}
var X = 42
`,
	}
	src := `
import "util"
util.X
`
	stdout := &bytes.Buffer{}
	vm := New(WithStdout(stdout))
	s, err := vm.NewSession(sys, "main.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume1: %v", err)
	}
	assert(t, "paused in imports", s.Status(), StatusPaused)
	data, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	vm2 := New()
	s2, err := vm2.RestoreSession(sys, "main.goat", src, data)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	assert(t, "result", s2.Values()[0].Int(), 42)
}
