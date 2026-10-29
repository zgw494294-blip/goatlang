package goatlang

import (
	"errors"
	"fmt"
	"strings"
)

// Public debug API
//
// A DebugSession executes one compiled main source exactly once on a child
// VM that shares the owner VM's globals and stdout. Execution runs on a
// goroutine that parks itself at statement entry checkpoints; each Command
// resumes the same real execution, so side effects are never replayed.

// DebugValue is a detached historical snapshot of one script value. It only
// carries a readable type name and the same text Value.String produces, so
// mutating a returned snapshot can never touch a live binding and later
// execution can never rewrite an earlier event.
type DebugValue struct {
	Type string
	Text string
}

// DebugFrame is one script activation at a pause. Frames run from the
// currently executing script activation back to the root. Caller locations
// point at the calling statement; host native function internals never
// appear. Locals only contains parameters, the receiver and local
// variables that are both in lexical scope and already bound at the pause.
type DebugFrame struct {
	File     string
	Function string
	Line     int
	Locals   map[string]DebugValue
}

// DebugEvent is one state transition of a debug session.
//
// State is "paused", "completed", "error" or "aborted". While paused, Reason
// is "entry", "breakpoint" or "step" ("step" covers step, next and out);
// terminal reasons are not constrained. Frames, Globals and Results are
// snapshots taken at the moment the event was produced.
type DebugEvent struct {
	State   string
	Reason  string
	Error   string
	Frames  []DebugFrame
	Globals map[string]DebugValue
	Results []DebugValue
}

// DebugState values.
const (
	DebugPaused    = "paused"
	DebugCompleted = "completed"
	DebugError     = "error"
	DebugAborted   = "aborted"
)

// Debug commands accepted by DebugSession.Command.
const (
	debugCmdContinue = "continue"
	debugCmdStep     = "step"
	debugCmdNext     = "next"
	debugCmdOut      = "out"
	debugCmdAbort    = "abort"
)

const debugRootFrame = "main"

// step modes used internally between checkpoints.
type debugMode int

const (
	modeEntry debugMode = iota
	modeRun
	modeStep
	modeNext
	modeOut
	modeAbort
)

// debugAbort unwinds the parked execution when the user aborts.
type debugAbort struct{}

func (debugAbort) Error() string { return "aborted" }

type debugCommand struct {
	mode debugMode
}

// debugger is the runtime half shared by every frame of the child VM.
type debugger struct {
	dvm   *VM
	file  string
	slots int

	// executable statement lines of the debugged file
	lines map[int]bool
	// active breakpoints: statement lines
	breaks map[int]bool
	// marker id -> local environment
	envs map[reg]*dbgEnv
	// global slot indices known to be bound
	bound map[int]bool

	pauseCh chan DebugEvent
	cmdCh   chan debugCommand

	mode debugMode
	// depth and activation id when next/out was issued
	depth0  int
	nextAct int
	// actSeq hands out unique activation ids; root is 0.
	actSeq int
}

func debugValueOf(g *lookup, x Value) DebugValue {
	// Type names are stored package-qualified ("main.T"); present them
	// without the package prefix, matching the script spelling.
	typ := strings.ReplaceAll(x.t.str(g), "main.", "")
	return DebugValue{Type: typ, Text: x.String()}
}

// isDebugGlobal reports whether a global key belongs to the debugged package
// and is referenced without an additional qualifier (methods, stored as
// T.M, are excluded).
func isDebugGlobal(key string) bool {
	const prefix = "main."
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	return !strings.ContainsRune(key[len(prefix):], '.')
}

func (d *debugger) globalsSnapshot() map[string]DebugValue {
	g := d.dvm.globals
	out := map[string]DebugValue{}
	for i, key := range g.indexToKey {
		if !d.bound[i] || !isDebugGlobal(key) {
			continue
		}
		out[key[len("main."):]] = debugValueOf(g, g.Read(i))
	}
	return out
}

// localsSnapshot materializes the visible bindings of one activation at the
// given register base. The env chain resolves shadowing innermost first.
func (d *debugger) localsSnapshot(baseN int, env *dbgEnv) map[string]DebugValue {
	out := map[string]DebugValue{}
	seen := map[string]bool{}
	for ; env != nil; env = env.Prev {
		if seen[env.Name] {
			continue
		}
		seen[env.Name] = true
		idx := baseN + env.Slot
		if idx < 0 || idx >= len(d.dvm.stack) {
			continue
		}
		out[env.Name] = debugValueOf(d.dvm.globals, d.dvm.stack[idx])
	}
	return out
}

// debugFuncName turns the compiler's qualified function name into a
// recognizable script name; the root uses a non-empty identifier.
func debugFuncName(raw string) string {
	if raw == "" {
		return debugRootFrame
	}
	return strings.TrimPrefix(raw, "main.")
}

func (d *debugger) buildFrames() []DebugFrame {
	v := d.dvm
	cur := v.frame.Codes[v.frame.N]
	file, fn, line, _ := cur.Pos.info(v.globals)
	frames := []DebugFrame{{
		File:     file,
		Function: debugFuncName(fn),
		Line:     line,
		Locals:   d.localsSnapshot(v.frame.BaseN, d.envs[reg(v.dbgStmt)]),
	}}
	// Caller activations: dbgActs and backtrace are pushed together by
	// mkFunc, so index i pairs with backtrace i.
	for i := len(v.dbgActs) - 1; i >= 0; i-- {
		act := v.dbgActs[i]
		cfile, cfn, cline, _ := v.backtrace[i].info(v.globals)
		frames = append(frames, DebugFrame{
			File:     cfile,
			Function: debugFuncName(cfn),
			Line:     cline,
			Locals:   d.localsSnapshot(act.baseN, d.envs[reg(act.stmt)]),
		})
	}
	return frames
}

func (d *debugger) pausedEvent(reason string) DebugEvent {
	return DebugEvent{
		State:   DebugPaused,
		Reason:  reason,
		Frames:  d.buildFrames(),
		Globals: d.globalsSnapshot(),
	}
}

func (d *debugger) terminalEvent(state, reason, errMsg string) DebugEvent {
	e := DebugEvent{
		State:   state,
		Reason:  reason,
		Error:   errMsg,
		Globals: d.globalsSnapshot(),
	}
	if state == DebugCompleted {
		for _, x := range d.dvm.stack[d.slots:] {
			e.Results = append(e.Results, debugValueOf(d.dvm.globals, x))
		}
	}
	return e
}

// tick is the statement entry hook invoked before every instruction of the
// child VM. Non-marker instructions return immediately.
func (d *debugger) tick(v *VM, ins *instruction) {
	if !ins.Stmt {
		return
	}
	file, _, line, _ := ins.Pos.info(v.globals)
	if file != d.file {
		return
	}
	seq := int(ins.A)
	depth := len(v.backtrace)
	v.dbgStmt = seq

	stop := false
	reason := ""
	switch {
	case d.breaks[line]:
		// Real breakpoints always win over stepping filters.
		stop, reason = true, "breakpoint"
	case d.mode == modeEntry:
		stop, reason = true, "entry"
	case d.mode == modeStep:
		stop, reason = true, "step"
	case d.mode == modeNext:
		// Same activation: its next statement. Shallower activation: the
		// issued one returned, so this is an ancestor statement. Same-depth
		// sibling or cousin activations (recursion, later calls in the same
		// expression) are skipped.
		if depth < d.depth0 || (depth == d.depth0 && v.dbgActID == d.nextAct) {
			stop, reason = true, "step"
		}
	case d.mode == modeOut && depth < d.depth0:
		stop, reason = true, "step"
	}
	if !stop {
		return
	}

	d.pauseCh <- d.pausedEvent(reason)

	cmd := <-d.cmdCh
	d.mode = cmd.mode
	if cmd.mode == modeAbort {
		panic(debugAbort{})
	}
}

// didBind records global slots written by global binding instructions, so a
// later snapshot distinguishes an explicitly bound nil from a never written
// slot.
func (d *debugger) didBind(c code, idx int) {
	switch c {
	case codeGlobalSet, codeGlobalZero, codeGlobalFunc, codeGlobalStruct:
		d.bound[idx] = true
	}
}

// loop runs the child VM to its terminal state, converting panics into
// error events except for the abort sentinel.
func (d *debugger) loop() {
	var ev DebugEvent
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(debugAbort); ok {
					ev = d.terminalEvent(DebugAborted, DebugAborted, "")
					return
				}
				ev = d.terminalEvent(DebugError, DebugError, d.dvm.btErr(r).Error())
			}
		}()
		d.dvm.exec()
		ev = d.terminalEvent(DebugCompleted, DebugCompleted, "")
	}()
	d.pauseCh <- ev
}

// DebugSession is an explicitly selected, single-source debugging session.
// Its zero value is not usable; create instances with VM.Debug.
type DebugSession struct {
	d   *debugger
	cur DebugEvent
}

func newDebugSession(owner *VM, file string, codes []instruction, slots int, envs map[reg]*dbgEnv,
	allLines map[string]map[int]bool) *DebugSession {
	d := &debugger{
		file:    file,
		slots:   slots,
		lines:   map[int]bool{},
		breaks:  map[int]bool{},
		envs:    envs,
		bound:   map[int]bool{},
		pauseCh: make(chan DebugEvent),
		cmdCh:   make(chan debugCommand),
		mode:    modeEntry,
	}
	for line := range allLines[file] {
		d.lines[line] = true
	}
	// Record globals already bound before this session (by imports, prior
	// Eval calls, host registration) while slots merely allocated by
	// compilation stay unbound.
	for i, key := range owner.globals.indexToKey {
		if isDebugGlobal(key) && owner.globals.data[i] != (Value{}) {
			d.bound[i] = true
		}
	}
	d.dvm = &VM{
		globals: owner.globals,
		stdout:  owner.stdout,
		stack:   make([]Value, slots),
		frame:   frame{Codes: codes},
		dbg:     d,
	}
	s := &DebugSession{d: d}
	go d.loop()
	s.cur = <-d.pauseCh
	return s
}

// Current returns the most recent event. After the session reaches a
// terminal state it keeps returning that terminal event.
func (s *DebugSession) Current() DebugEvent {
	return s.cur
}

// SetBreakpoints replaces the breakpoints for file. lines must be executable
// statement lines of the debugged source; lines inside branches that never
// run or functions that are never called may be bound, but they can never
// produce a fabricated hit. An empty list clears all breakpoints. A file
// mismatch or any non-executable line returns an error and leaves the
// previous breakpoints untouched.
func (s *DebugSession) SetBreakpoints(file string, lines []int) error {
	if s.cur.State != DebugPaused {
		return fmt.Errorf("debug: cannot set breakpoints in state %q", s.cur.State)
	}
	if file != s.d.file {
		return fmt.Errorf("debug: unknown file %q (session debugs %q)", file, s.d.file)
	}
	for _, line := range lines {
		if !s.d.lines[line] {
			return fmt.Errorf("debug: line %d is not an executable statement in %q", line, file)
		}
	}
	next := map[int]bool{}
	for _, line := range lines {
		next[line] = true
	}
	s.d.breaks = next
	return nil
}

// Command resumes execution: "continue", "step", "next", "out" or "abort".
// It returns the event at the next pause or terminal state. Runtime errors
// are reported both as an error-state event and as a non-nil error.
func (s *DebugSession) Command(command string) (DebugEvent, error) {
	if s.cur.State != DebugPaused {
		return s.cur, fmt.Errorf("debug: cannot execute command %q in state %q", command, s.cur.State)
	}
	var mode debugMode
	switch command {
	case debugCmdContinue:
		mode = modeRun
	case debugCmdStep:
		mode = modeStep
	case debugCmdNext:
		mode = modeNext
	case debugCmdOut:
		mode = modeOut
	case debugCmdAbort:
		mode = modeAbort
	default:
		return s.cur, fmt.Errorf("debug: unknown command %q", command)
	}
	s.d.depth0 = len(s.d.dvm.backtrace)
	s.d.nextAct = s.d.dvm.dbgActID
	s.d.cmdCh <- debugCommand{mode: mode}
	s.cur = <-s.d.pauseCh
	var err error
	if s.cur.State == DebugError {
		err = errors.New(s.cur.Error)
	}
	return s.cur, err
}
