package goatlang

import (
	"fmt"
	"io/fs"
)

// funcReg describes one script function body inside a prepared program. The
// body is a sub-slice of a compilation unit's instructions; start is the
// absolute index of the FUNC instruction in that unit.
type funcReg struct {
	prog  *program
	unit  int
	start int

	nargs int
	rets  int
	slots int

	types []instruction
	body  []instruction
}

// program is the prepared, resumable form of a source: imported packages run
// as unit 0, the main source as unit 1. Function bodies are registered up
// front so function values can be rebuilt without re-executing code.
type program struct {
	units [][]instruction
	slots []int
	funcs map[uint64]*funcReg
}

func funcKey(unit, id int) uint64 { return uint64(unit)<<32 | uint64(uint32(id)) }

func (p *program) lookup(unit, id int) *funcReg {
	r, ok := p.funcs[funcKey(unit, id)]
	if !ok {
		panic(fmt.Sprintf("internal error: unknown function %v:%v", unit, id))
	}
	return r
}

// ensureFunc returns a working function value for a registered function.
func (p *program) ensureFunc(unit, id int) Value {
	r := p.lookup(unit, id)
	return r.value()
}

func (r *funcReg) value() Value {
	args, _ := splitParams(r.prog.units[r.unit][r.start].A)
	f := newFunc(int(args), r.rets, mkFunc(r.nargs, r.rets, r.slots, r.slice(), r.unit, r.start))
	if args < 0 {
		f.getFunc().VariadicType = Type(r.types[r.nargs-1].A)
	}
	ft := f.value.(*funcT)
	ft.Unit = r.unit
	ft.FuncID = r.start
	return f
}

// slice returns the full FUNC token window (arg/ret type instructions then
// body), matching what the FUNC opcode sees at runtime.
func (r *funcReg) slice() []instruction {
	unit := r.prog.units[r.unit]
	args, rets := splitParams(unit[r.start].A)
	nargs := int(args)
	if nargs < 0 {
		nargs = -nargs
	}
	jump := int(unit[r.start].C)
	return unit[r.start+1 : r.start+1+nargs+int(rets)+jump]
}

// buildProgram registers every function body in the given units.
func buildProgram(units [][]instruction, slots []int) *program {
	p := &program{
		units: units,
		slots: slots,
		funcs: map[uint64]*funcReg{},
	}
	for u, codes := range units {
		for idx := 0; idx < len(codes); idx++ {
			ins := codes[idx]
			if ins.Code != codeFunc {
				continue
			}
			args, rets := splitParams(ins.A)
			nargs := int(args)
			if nargs < 0 {
				nargs = -nargs
			}
			retsN := int(rets)
			jump := int(ins.C)
			window := codes[idx+1 : idx+1+nargs+retsN+jump]
			reg := &funcReg{
				prog:  p,
				unit:  u,
				start: idx,
				nargs: nargs,
				rets:  retsN,
				slots: int(ins.B),
				types: window[:nargs+retsN],
				body:  window[nargs+retsN:],
			}
			p.funcs[funcKey(u, idx)] = reg
		}
	}
	return p
}

// prepareProgram tokenizes, parses, loads imports and compiles exactly like
// VM.Eval, but never executes anything. Imports compile into unit 0 and the
// main source (package main) into unit 1.
func prepareProgram(g *lookup, sys fs.FS, fname, input string) (prog *program, err error) {
	const pkgName = "main"
	tokens, err := tokenize(fname, input)
	if err != nil {
		return nil, fmt.Errorf("error in tokenize: %w", err)
	}
	tree, err := parse(tokens)
	if err != nil {
		return nil, fmt.Errorf("error in parse: %w", err)
	}
	pkgs, err := loadImports(sys, "", tree)
	if err != nil {
		return nil, fmt.Errorf("error in loadImports: %w", err)
	}
	importCodes, importSlots, err := compilePkgs(g, pkgs[:len(pkgs)-1], true)
	if err != nil {
		return nil, fmt.Errorf("error in compile (imports): %w", err)
	}
	cmp := &compiler{
		Globals:     g,
		Locals:      newLookup(),
		Imports:     map[string]string{},
		Optimize:    true,
		PackageName: pkgName,
		ExportName:  pkgName,
	}
	mainCodes, mainSlots, err := cmp.run(pkgs[len(pkgs)-1])
	if err != nil {
		return nil, fmt.Errorf("error in compile: %w", err)
	}
	return buildProgram([][]instruction{importCodes, mainCodes}, []int{importSlots, mainSlots}), nil
}
