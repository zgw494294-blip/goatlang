package goatlang

import (
	"strings"
	"testing"
)

// driveSession runs src, restarting from a snapshot at every checkpoint
// (simulating a new process each time), and returns the final values as
// strings plus stdout.
func driveSession(t *testing.T, src string, install func(*VM)) (string, string) {
	t.Helper()
	stdout := &strings.Builder{}
	vm := New(WithStdout(writerFunc(func(p []byte) (int, error) { stdout.Write(p); return len(p), nil })))
	if install != nil {
		install(vm)
	}
	s, err := vm.NewSession(nil, "test.goat", src)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	pauses := 0
	for {
		err := s.Resume()
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if s.Status() == StatusDone {
			break
		}
		b, err := s.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		pauses++
		vm2 := New(WithStdout(writerFunc(func(p []byte) (int, error) { stdout.Write(p); return len(p), nil })))
		if install != nil {
			install(vm2)
		}
		s, err = vm2.RestoreSession(nil, "test.goat", src, b)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
	}
	var ss []string
	for _, v := range s.Values() {
		ss = append(ss, v.String())
	}
	return strings.Join(ss, " "), stdout.String()
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestStress_quicksort(t *testing.T) {
	src := `
func qsort(a []int, lo, hi int) {
	if lo >= hi {
		return
	}
	checkpoint()
	p := a[hi]
	i := lo
	for j := lo; j < hi; j++ {
		if a[j] < p {
			tmp := a[i]
			a[i] = a[j]
			a[j] = tmp
			i++
		}
	}
	tmp := a[i]
	a[i] = a[hi]
	a[hi] = tmp
	qsort(a, lo, i-1)
	qsort(a, i+1, hi)
}
a := []int{9, 3, 7, 1, 8, 2, 5, 4, 6, 0}
qsort(a, 0, 9)
chk := 0
for i := 0; i < 10; i++ {
	if a[i] != i {
		chk = -1
	}
	chk += a[i] * (i + 1)
}
chk
`
	got, _ := driveSession(t, src, nil)
	// exact permutation 0..9 in order: weighted sum = 330
	if got != "330" {
		t.Fatalf("qsort got %v", got)
	}
}

func TestStress_tree(t *testing.T) {
	src := `
type Node struct {
	V int
	L *Node
	R *Node
}
func insert(n *Node, v int) *Node {
	checkpoint()
	if n == nil {
		return &Node{V: v}
	}
	if v < n.V {
		n.L = insert(n.L, v)
	} else {
		n.R = insert(n.R, v)
	}
	return n
}
func sum(n *Node) int {
	if n == nil {
		return 0
	}
	return n.V + sum(n.L) + sum(n.R)
}
var root *Node
for _, v := range []int{5, 2, 8, 1, 4, 7, 9, 3, 6, 0} {
	root = insert(root, v)
}
r := sum(root)
r
`
	got, _ := driveSession(t, src, nil)
	if got != "45" {
		t.Fatalf("tree sum got %v", got)
	}
}

func TestStress_methodsAndRanges(t *testing.T) {
	src := `
type Box struct {
	vals []int
}
func (b *Box) add(v int) {
	b.vals = append(b.vals, v)
	checkpoint()
}
func (b *Box) total() int {
	t := 0
	for _, v := range b.vals {
		t += v
	}
	return t
}
b := &Box{}
for i := 1; i <= 10; i++ {
	b.add(i)
}
s := ""
for _, x := range "abcdef" {
	s += string(x)
}
r := b.total()
r
`
	got, _ := driveSession(t, src, nil)
	if got != "55" {
		t.Fatalf("methods got %v", got)
	}
}

func TestStress_multiReturnNative(t *testing.T) {
	src := `
func step(x int) (int, int) {
	checkpoint()
	return x + 1, x * 2
}
a, b := 0, 0
for i := 0; i < 6; i++ {
	d, e := step(i)
	a += d
	b += e
}
a * 100 + b
`
	got, _ := driveSession(t, src, nil)
	// a = sum(i+1, i 0..5) = 21; b = sum(i*2)=30
	if got != "2130" {
		t.Fatalf("multi got %v", got)
	}
}

func TestStress_fib(t *testing.T) {
	src := `
func fib(n int) int {
	if n < 2 {
		return n
	}
	checkpoint()
	return fib(n-1) + fib(n-2)
}
r := fib(10)
r
`
	got, _ := driveSession(t, src, nil)
	if got != "55" {
		t.Fatalf("fib got %v", got)
	}
}
