# goatlang
Goatlang is a subset of Go written in Go for scripting in Go

## Debugging

`VM.Debug` starts an explicit, single-source statement-stepping debug
session. The embedded script runs for real exactly once (same globals, stdout
and native functions as `Eval`); the session parks before the first statement
that actually executes, before that statement has any effect, and each command
resumes that same execution, so side effects are never replayed. Plain
`Eval`, `Call`, `Load` and friends keep their original behavior.

```go
vm := goatlang.New(goatlang.WithStdout(os.Stdout))

session, err := vm.Debug(fsys, "example.go", src)
if err != nil { /* tokenize/parse/compile error: nothing in the source ran */ }

ev := session.Current()
// ev.State is "paused", ev.Reason is "entry"
// ev.Frames[0] has File, Function, Line and Locals
// ev.Globals holds currently bound script globals

// breakpoints use original file lines that hold executable statements
if err := session.SetBreakpoints("example.go", []int{12}); err != nil {
    // unknown file, or a non-executable line
}

ev, err = session.Command("continue") // "step" | "next" | "out" | "abort"
```

- States: `paused`, `completed`, `error`, `aborted`.
- Pause reasons: `entry`, `breakpoint`, `step` (`step` covers step, next and
  out). Terminal events carry no constrained reason.
- Commands:
  - `continue` runs to the next breakpoint or a terminal state,
  - `step` stops at the next executed statement, entering calls,
  - `next` stops at the next statement of the same function activation, or an
    ancestor statement when the activation returns; recursive calls and later
    calls inside the same expression are skipped, while a real breakpoint
    always preempts the stepping filter,
  - `out` runs until the current activation returns, or to completion at the
    root,
  - `abort` ends the run as `aborted`, keeping effects already produced and
    running no further statements.
- Checkpoints are statement entries only (assignments, declarations, calls,
  returns, `break`/`continue`, and the entries and actually entered branches
  or loop bodies of `if`/`switch`/`for`/`range`). Unreached branches, uncalled
  function bodies, function/type declarations, comments and blank lines never
  stop. A multi-line statement reports its start line.
- `SetBreakpoints` replaces all breakpoints for the file (empty list clears
  them). Lines in untaken branches or uncalled functions may be bound, but
  only statements that actually execute can hit. The file must match the
  session file and every line must be executable; otherwise the previous
  breakpoints are left untouched. Breakpoints can only be changed while
  paused.
- `DebugEvent.Frames` runs from the current script activation to the root
  (`Function` is the recognizable script function name, the root is `main`);
  host native frames never appear. `Locals` lists only parameters, the
  receiver and locals that are in lexical scope and already bound.
- `DebugValue` (`Type`, `Text`) is a historical snapshot detached from the
  running value: continuing execution cannot rewrite earlier events, and
  mutating a returned map cannot change live bindings. `Text` matches
  `Value.String` for scalars, slices, maps and struct pointers.
- The terminal `completed` event carries `Results` identical in order, type
  and value to what `Eval` returns, and terminal events still expose
  `Globals`. Runtime errors produce an `error` event (and `Command` also
  returns the error); `Current()` keeps returning the terminal event and any
  further command or breakpoint change is rejected.
- After completion, error or abort, the VM can still be used for normal
  `Eval` calls and fresh debug sessions; separate VMs never share sessions,
  breakpoints or bindings.

Debugging covers one synchronous source file (functions, function values,
methods, ordered nested calls, local shadowing, conditionals, loops and
containers). Multi-file/module stepping, expression evaluation, reverse
execution, DAP, concurrent use and pausing inside native functions are not
provided.
