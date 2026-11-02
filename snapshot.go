package goatlang

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
)

// Snapshot binary format (not public and not stable across versions):
//
//	header  := magic(8) version(u16) flags(u16) srcHash(32) impHash(32)
//	           payloadLen(u64) payload checksum(32, over all prior bytes)
//	payload := objectsMeta schemas objectBodies globals stack frames
//
// Values are either inline scalars/strings or references ("r" id) into the
// object table, which preserves container sharing, overlapping sub-slice
// backing arrays and bounded struct cycles.

const (
	sessMagic   = "GOATSESS"
	sessVersion = 1

	kindArray      = byte('A')
	kindSlice      = byte('S')
	kindStringMap  = byte('M')
	kindNumericMap = byte('N')
	kindStruct     = byte('T')
	kindFunc       = byte('F')
	kindBound      = byte('B')
	kindNative     = byte('P')
	kindSliceIter  = byte('I')
	kindStringIter = byte('J')
	kindMapIter    = byte('K')

	nilRef = uint32(0xffffffff)
)

type sessHeader struct {
	sourceHash  [32]byte
	importsHash [32]byte
	payload     []byte
}

// ---------------------------------------------------------------- writer

type encWriter struct {
	buf []byte
}

func (w *encWriter) bytes(b ...byte) { w.buf = append(w.buf, b...) }
func (w *encWriter) u8(v byte)       { w.buf = append(w.buf, v) }
func (w *encWriter) bool8(v bool) {
	if v {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}
func (w *encWriter) u32(v uint32)    { w.buf = binary.BigEndian.AppendUint32(w.buf, v) }
func (w *encWriter) i32(v int32)     { w.u32(uint32(v)) }
func (w *encWriter) u64(v uint64)    { w.buf = binary.BigEndian.AppendUint64(w.buf, v) }
func (w *encWriter) f64(v float64)   { w.u64(math.Float64bits(v)) }
func (w *encWriter) string(s string) { w.u32(uint32(len(s))); w.buf = append(w.buf, s...) }

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// ---------------------------------------------------------------- encoder

type encHdr struct {
	id    int32
	start int32
	size  int32
}

type encBacking struct {
	id     int32
	vt     Type
	end    uintptr
	min    uintptr
	hdrs   []encHdr
	slices []*sliceT
}

type encRec struct {
	kind byte
	obj  any

	// meta
	vt          Type
	kt          Type
	total       int32
	arrID       uint32
	off, length int32
	typeN       int32
	unit        byte
	funcID      int32
	recv, inner uint32
	name        string
	args, rets  int32
	variadic    bool
	n           int32
	sid, mid    uint32

	// body
	elems []Value
	runes []rune
}

type encSchema struct {
	typeN   int
	order   []string
	lookup  map[string]int
	methods []encMethod
}

type encMethod struct {
	idx int
	val Value
}

type encoder struct {
	s *Session
	w *encWriter
	g *lookup

	recs []*encRec
	ptrs map[uintptr]int32

	backings map[uintptr]*encBacking
	backIDs  []uintptr

	nativeByName map[uintptr]string
	schemas      map[int]*encSchema
}

var valueSize = reflect.TypeOf(Value{}).Size()

func snapshotNativeNames(g *lookup) map[uintptr]string {
	res := map[uintptr]string{}
	for i, v := range g.data {
		ft, ok := v.value.(*funcT)
		if !ok || !ft.Native {
			continue
		}
		key := reflect.ValueOf(ft).Pointer()
		if _, exists := res[key]; !exists {
			res[key] = g.indexToKey[i]
		}
	}
	return res
}

func newEncoder(s *Session) *encoder {
	g := s.vm.globals
	return &encoder{
		s:            s,
		w:            &encWriter{},
		g:            g,
		ptrs:         map[uintptr]int32{},
		backings:     map[uintptr]*encBacking{},
		nativeByName: snapshotNativeNames(g),
		schemas:      map[int]*encSchema{},
	}
}

func (e *encoder) alloc(rec *encRec) int32 {
	id := int32(len(e.recs))
	e.recs = append(e.recs, rec)
	return id
}

func (e *encoder) visitFunc(ft *funcT) (int32, error) {
	key := reflect.ValueOf(ft).Pointer()
	if id, ok := e.ptrs[key]; ok {
		return id, nil
	}
	if ft.Bind != nil {
		recv, err := e.visit(ft.Bind.Receiver)
		if err != nil {
			return 0, err
		}
		inner, err := e.visitFunc(ft.Bind.Inner)
		if err != nil {
			return 0, err
		}
		id := e.alloc(&encRec{kind: kindBound, recv: uint32(recv), inner: uint32(inner)})
		e.ptrs[key] = id
		return id, nil
	}
	if ft.Native {
		name, ok := e.nativeByName[key]
		if !ok {
			return 0, errors.New("cannot snapshot native function value: it is not installed as a named global via VM.Set")
		}
		id := e.alloc(&encRec{
			kind: kindNative, name: name,
			args: int32(ft.Args), rets: int32(ft.Rets), variadic: ft.Variadic,
		})
		e.ptrs[key] = id
		return id, nil
	}
	if ft.Unit < 0 || ft.Unit >= len(e.s.prog.units) {
		return 0, errors.New("cannot snapshot function value created outside of the session")
	}
	if _, ok := e.s.prog.funcs[funcKey(ft.Unit, ft.FuncID)]; !ok {
		return 0, errors.New("cannot snapshot function value created outside of the session")
	}
	id := e.alloc(&encRec{kind: kindFunc, unit: byte(ft.Unit), funcID: int32(ft.FuncID)})
	e.ptrs[key] = id
	return id, nil
}

func (e *encoder) addSchema(s *structT) error {
	if _, ok := e.schemas[s.TypeN]; ok {
		return nil
	}
	sc := &encSchema{typeN: s.TypeN, order: s.Order, lookup: s.Lookup}
	if s.Methods != nil {
		for _, p := range s.Methods.pairs {
			if p.distance == 0 {
				continue
			}
			if _, err := e.visit(p.value); err != nil {
				return err
			}
			sc.methods = append(sc.methods, encMethod{idx: p.key, val: p.value})
		}
	}
	e.schemas[s.TypeN] = sc
	return nil
}

func (e *encoder) visit(v Value) (int32, error) {
	if v.value == nil || v.t == TypeString {
		return -1, nil
	}
	key := reflect.ValueOf(v.value).Pointer()
	if id, ok := e.ptrs[key]; ok {
		return id, nil
	}
	switch o := v.value.(type) {
	case *sliceT:
		if o.data == nil {
			id := e.alloc(&encRec{
				kind: kindSlice, obj: o, vt: o.valueType,
				arrID: nilRef,
			})
			e.ptrs[key] = id
			return id, nil
		}
		rv := reflect.ValueOf(o.data)
		ptr := uintptr(rv.Pointer())
		capn := rv.Cap()
		if capn == 0 {
			// Empty non-nil slices share zerobase at runtime; give each its
			// own zero-length array so element types never get merged.
			arrID := e.alloc(&encRec{kind: kindArray, vt: o.valueType, total: 0})
			id := e.alloc(&encRec{
				kind: kindSlice, obj: o, vt: o.valueType,
				arrID: uint32(arrID),
			})
			e.ptrs[key] = id
			return id, nil
		}
		end := ptr + uintptr(capn)*valueSize
		b, ok := e.backings[end]
		if !ok {
			b = &encBacking{id: int32(len(e.recs)), vt: o.valueType, end: end, min: ptr}
			e.backings[end] = b
			e.backIDs = append(e.backIDs, end)
			e.recs = append(e.recs, &encRec{kind: kindArray, vt: o.valueType})
		}
		if ptr < b.min {
			b.min = ptr
		}
		var off int32
		if capn > 0 {
			off = int32((ptr - b.min) / valueSize)
		}
		id := e.alloc(&encRec{
			kind: kindSlice, obj: o, vt: o.valueType,
			arrID: uint32(b.id), off: off, length: int32(rv.Len()),
		})
		e.ptrs[key] = id
		b.hdrs = append(b.hdrs, encHdr{id: id, start: off, size: int32(rv.Len())})
		b.slices = append(b.slices, o)
		for i := 0; i < len(o.data); i++ {
			if _, err := e.visit(o.data[i]); err != nil {
				return 0, err
			}
		}
		return id, nil

	case *stringMap:
		id := e.alloc(&encRec{kind: kindStringMap, obj: o, vt: o.valueType})
		e.ptrs[key] = id
		for _, k := range o.keys {
			if val, ok := o.data[k]; ok {
				if _, err := e.visit(val); err != nil {
					return 0, err
				}
			}
		}
		return id, nil

	case *numericMap:
		id := e.alloc(&encRec{kind: kindNumericMap, obj: o, kt: o.keyType, vt: o.valueType})
		e.ptrs[key] = id
		for _, k := range o.keys {
			if val, ok := o.data[k]; ok {
				if _, err := e.visit(val); err != nil {
					return 0, err
				}
			}
		}
		return id, nil

	case *structT:
		if err := e.addSchema(o); err != nil {
			return 0, err
		}
		id := e.alloc(&encRec{kind: kindStruct, obj: o, typeN: int32(o.TypeN)})
		e.ptrs[key] = id
		for _, p := range o.Fields.pairs {
			if p.distance != 0 {
				if _, err := e.visit(p.value); err != nil {
					return 0, err
				}
			}
		}
		return id, nil

	case *funcT:
		return e.visitFunc(o)

	case *sliceNext:
		var sid uint32 = nilRef
		if o.S.value != nil {
			id, err := e.visit(o.S)
			if err != nil {
				return 0, err
			}
			sid = uint32(id)
		}
		id := e.alloc(&encRec{kind: kindSliceIter, obj: o, sid: sid, n: int32(o.N)})
		e.ptrs[key] = id
		return id, nil

	case *stringNext:
		id := e.alloc(&encRec{kind: kindStringIter, obj: o, n: int32(o.N), runes: o.R})
		e.ptrs[key] = id
		return id, nil

	case *mapNext:
		mid, err := e.visit(o.M)
		if err != nil {
			return 0, err
		}
		for _, k := range o.Keys {
			if _, err := e.visit(k); err != nil {
				return 0, err
			}
		}
		id := e.alloc(&encRec{kind: kindMapIter, obj: o, mid: uint32(mid), n: int32(o.N)})
		e.ptrs[key] = id
		return id, nil

	case *nextT:
		return 0, errors.New("cannot snapshot active iteration over a non-serializable value")

	default:
		if v.t == TypeObject {
			return 0, fmt.Errorf("cannot snapshot external object created with Wrap: %T", v.value)
		}
		return 0, fmt.Errorf("cannot snapshot value of unsupported type %T", v.value)
	}
}

func (e *encoder) writeValue(v Value) error {
	if v.value == nil {
		w := e.w
		w.bytes(0x7a) // 'z'
		w.i32(int32(v.t))
		w.f64(v.num)
		return nil
	}
	if v.t == TypeString {
		e.w.bytes(0x73) // 's'
		e.w.string(string(v.value.(stringT)))
		return nil
	}
	id, err := e.visit(v)
	if err != nil {
		return err
	}
	if id < 0 {
		return errors.New("internal error: value not assigned id")
	}
	e.w.bytes(0x72) // 'r'
	e.w.u32(uint32(id))
	return nil
}

func encodeSnapshot(s *Session) (out []byte, err error) {
	e := newEncoder(s)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cannot snapshot session: %v", r)
		}
	}()

	type globalRoot struct {
		name string
		v    Value
	}
	var globals []globalRoot
	for i, v := range e.g.data {
		name := e.g.indexToKey[i]
		if name == "" || name[0] == '~' {
			continue
		}
		if _, err := e.visit(v); err != nil {
			return nil, err
		}
		globals = append(globals, globalRoot{name, v})
	}
	for _, v := range s.vm.stack {
		if _, err := e.visit(v); err != nil {
			return nil, err
		}
	}

	// Fill backing array bodies with the union of all reachable slots.
	for _, end := range e.backIDs {
		b := e.backings[end]
		total := int((b.end - b.min) / valueSize)
		union := make([]Value, total)
		for i := range union {
			union[i] = newZero(b.vt)
		}
		for hi, h := range b.hdrs {
			sl := b.slices[hi]
			for i := int32(0); i < h.size; i++ {
				union[h.start+i] = sl.data[i]
			}
		}
		e.recs[b.id].total = int32(total)
		e.recs[b.id].elems = union
	}

	w := e.w

	// ---- objects meta ----
	w.u32(uint32(len(e.recs)))
	for id, rec := range e.recs {
		w.u32(uint32(id))
		w.bytes(rec.kind)
		switch rec.kind {
		case kindArray:
			w.i32(int32(rec.vt))
			w.u32(uint32(rec.total))
		case kindSlice:
			w.i32(int32(rec.vt))
			w.u32(rec.arrID)
			w.i32(rec.off)
			w.i32(rec.length)
		case kindStringMap:
			w.i32(int32(rec.vt))
		case kindNumericMap:
			w.i32(int32(rec.kt))
			w.i32(int32(rec.vt))
		case kindStruct:
			w.i32(rec.typeN)
		case kindFunc:
			w.bytes(rec.unit)
			w.u32(uint32(rec.funcID))
		case kindBound:
			w.u32(rec.recv)
			w.u32(rec.inner)
		case kindNative:
			w.string(rec.name)
			w.i32(rec.args)
			w.i32(rec.rets)
			w.bool8(rec.variadic)
		case kindSliceIter:
			w.u32(rec.sid)
			w.i32(rec.n)
		case kindStringIter:
			w.i32(rec.n)
		case kindMapIter:
			w.u32(rec.mid)
			w.i32(rec.n)
		default:
			return nil, fmt.Errorf("internal error: unknown record kind %q", rec.kind)
		}
	}

	// ---- struct schemas ----
	typeNSet := make([]int, 0, len(e.schemas))
	for tn := range e.schemas {
		typeNSet = append(typeNSet, tn)
	}
	sort.Ints(typeNSet)
	w.u32(uint32(len(typeNSet)))
	for _, tn := range typeNSet {
		sc := e.schemas[tn]
		w.i32(int32(tn))
		w.u32(uint32(len(sc.order)))
		for _, name := range sc.order {
			w.string(name)
			w.i32(int32(sc.lookup[name]))
		}
		w.u32(uint32(len(sc.methods)))
		for _, m := range sc.methods {
			w.i32(int32(m.idx))
			id, err := e.visit(m.val)
			if err != nil {
				return nil, err
			}
			w.u32(uint32(id))
		}
	}

	// ---- object bodies ----
	for _, rec := range e.recs {
		w.bytes(rec.kind)
		switch rec.kind {
		case kindArray:
			w.u32(uint32(len(rec.elems)))
			for _, v := range rec.elems {
				if err := e.writeValue(v); err != nil {
					return nil, err
				}
			}
		case kindSlice, kindFunc, kindBound, kindNative, kindSliceIter:
		case kindStringMap:
			obj := rec.obj.(*stringMap)
			seen := map[string]bool{}
			var keys []string
			for _, k := range obj.keys {
				if _, live := obj.data[k]; !live || seen[k] {
					continue
				}
				seen[k] = true
				keys = append(keys, k)
			}
			w.u32(uint32(len(keys)))
			for _, k := range keys {
				w.string(k)
				if err := e.writeValue(obj.data[k]); err != nil {
					return nil, err
				}
			}
		case kindNumericMap:
			obj := rec.obj.(*numericMap)
			seen := map[float64]bool{}
			var keys []float64
			for _, k := range obj.keys {
				if _, live := obj.data[k]; !live || seen[k] {
					continue
				}
				seen[k] = true
				keys = append(keys, k)
			}
			w.u32(uint32(len(keys)))
			for _, k := range keys {
				w.f64(k)
				if err := e.writeValue(obj.data[k]); err != nil {
					return nil, err
				}
			}
		case kindStruct:
			obj := rec.obj.(*structT)
			n := 0
			for _, p := range obj.Fields.pairs {
				if p.distance != 0 {
					n++
				}
			}
			w.u32(uint32(n))
			for _, p := range obj.Fields.pairs {
				if p.distance == 0 {
					continue
				}
				w.i32(int32(p.key))
				if err := e.writeValue(p.value); err != nil {
					return nil, err
				}
			}
		case kindStringIter:
			w.u32(uint32(len(rec.runes)))
			for _, r := range rec.runes {
				w.i32(int32(r))
			}
		case kindMapIter:
			obj := rec.obj.(*mapNext)
			w.u32(uint32(len(obj.Keys)))
			for _, k := range obj.Keys {
				if err := e.writeValue(k); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("internal error: unknown body kind %q", rec.kind)
		}
	}

	// ---- globals ----
	w.u32(uint32(len(globals)))
	for _, gr := range globals {
		w.string(gr.name)
		if err := e.writeValue(gr.v); err != nil {
			return nil, err
		}
	}

	// ---- stack ----
	w.u32(uint32(len(s.vm.stack)))
	for _, v := range s.vm.stack {
		if err := e.writeValue(v); err != nil {
			return nil, err
		}
	}

	// ---- frames (bottom-up) ----
	all := append(append([]frame{}, s.vm.frames...), s.vm.frame)
	w.u32(uint32(len(all)))
	for _, fr := range all {
		w.bytes(byte(fr.Unit))
		w.i32(int32(fr.N))
		w.i32(int32(fr.BaseN))
		if fr.info == nil {
			w.bool8(false)
		} else {
			w.bool8(true)
			w.i32(int32(fr.info.TopN))
			w.i32(int32(fr.info.XRets))
			w.bytes(byte(fr.info.reg.unit))
			w.u32(uint32(fr.info.reg.start))
		}
	}

	payload := w.buf
	out = make([]byte, 0, len(sessMagic)+2+2+64+8+len(payload)+32)
	out = append(out, sessMagic...)
	out = binary.BigEndian.AppendUint16(out, sessVersion)
	out = binary.BigEndian.AppendUint16(out, 0)
	out = append(out, s.sourceHash[:]...)
	out = append(out, s.importsHash[:]...)
	out = binary.BigEndian.AppendUint64(out, uint64(len(payload)))
	out = append(out, payload...)
	sum := sha256Sum(out)
	out = append(out, sum[:]...)
	return out, nil
}

// ---------------------------------------------------------------- header

func decodeHeader(data []byte) (h sessHeader, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("corrupt snapshot: %v", r)
		}
	}()
	const fixed = len(sessMagic) + 2 + 2 + 32 + 32 + 8
	if len(data) < fixed+32 {
		return h, errors.New("corrupt snapshot: truncated")
	}
	p := 0
	if string(data[p:p+len(sessMagic)]) != sessMagic {
		return h, errors.New("corrupt snapshot: bad magic")
	}
	p += len(sessMagic)
	ver := binary.BigEndian.Uint16(data[p:])
	if ver != sessVersion {
		return h, fmt.Errorf("unsupported snapshot version: %v", ver)
	}
	p += 4 // version + flags
	copy(h.sourceHash[:], data[p:p+32])
	p += 32
	copy(h.importsHash[:], data[p:p+32])
	p += 32
	plen := binary.BigEndian.Uint64(data[p:])
	p += 8
	if uint64(len(data)-p-32) != plen {
		return h, errors.New("corrupt snapshot: length mismatch")
	}
	h.payload = data[p : p+int(plen)]
	checksum := data[p+int(plen):]
	sum := sha256Sum(data[:p+int(plen)])
	if string(checksum) != string(sum[:]) {
		return h, errors.New("corrupt snapshot: checksum mismatch")
	}
	return h, nil
}

// ---------------------------------------------------------------- decoder

type typeSchema struct {
	lookup  map[string]int
	order   []string
	methods *intMap
}

type boundMeta struct {
	id          uint32
	recv, inner uint32
}

type decoder struct {
	d *decReader
	s *Session
	g *lookup

	objs   []Value
	arrays map[int32]*backingHolder
	kinds  []byte
	bounds []boundMeta

	schemas map[int]*typeSchema
}

type backingHolder struct {
	vt   Type
	data []Value
}

type decReader struct {
	b []byte
	p int
}

func (d *decReader) need(n int) error {
	if d.p+n > len(d.b) {
		return errors.New("corrupt snapshot: truncated payload")
	}
	return nil
}

func (d *decReader) u8() (byte, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	v := d.b[d.p]
	d.p++
	return v, nil
}

func (d *decReader) bool8() (bool, error) {
	v, err := d.u8()
	if err != nil {
		return false, err
	}
	if v > 1 {
		return false, errors.New("corrupt snapshot: invalid bool")
	}
	return v == 1, nil
}

func (d *decReader) u32() (uint32, error) {
	if err := d.need(4); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(d.b[d.p:])
	d.p += 4
	return v, nil
}

func (d *decReader) i32() (int32, error) {
	v, err := d.u32()
	return int32(v), err
}

func (d *decReader) u64() (uint64, error) {
	if err := d.need(8); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint64(d.b[d.p:])
	d.p += 8
	return v, nil
}

func (d *decReader) f64() (float64, error) {
	v, err := d.u64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(v), nil
}

func (d *decReader) str() (string, error) {
	n, err := d.u32()
	if err != nil {
		return "", err
	}
	if err := d.need(int(n)); err != nil {
		return "", err
	}
	s := string(d.b[d.p : d.p+int(n)])
	d.p += int(n)
	return s, nil
}

func (d *decoder) ref() (Value, error) {
	id, err := d.d.u32()
	if err != nil {
		return Value{}, err
	}
	if id == nilRef {
		return Value{}, errors.New("corrupt snapshot: unexpected nil reference")
	}
	if int(id) >= len(d.objs) {
		return Value{}, fmt.Errorf("corrupt snapshot: reference %v out of range", id)
	}
	return d.objs[id], nil
}

func (d *decoder) readValue() (Value, error) {
	tag, err := d.d.u8()
	if err != nil {
		return Value{}, err
	}
	switch tag {
	case 0x7a: // scalar
		t, err := d.d.i32()
		if err != nil {
			return Value{}, err
		}
		n, err := d.d.f64()
		if err != nil {
			return Value{}, err
		}
		return Value{t: Type(t), num: n}, nil
	case 0x73: // string
		s, err := d.d.str()
		if err != nil {
			return Value{}, err
		}
		return String(s), nil
	case 0x72: // reference
		return d.ref()
	default:
		return Value{}, fmt.Errorf("corrupt snapshot: invalid value tag %v", tag)
	}
}

func decodePayload(s *Session, h sessHeader) (err error) {
	d := &decoder{
		d:       &decReader{b: h.payload},
		s:       s,
		g:       s.vm.globals,
		arrays:  map[int32]*backingHolder{},
		schemas: map[int]*typeSchema{},
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("corrupt snapshot: %v", r)
		}
	}()

	// ---- objects meta: allocate every object up front ----
	n, err := d.d.u32()
	if err != nil {
		return err
	}
	d.objs = make([]Value, n)
	d.kinds = make([]byte, n)
	for id := uint32(0); id < n; id++ {
		rid, err := d.d.u32()
		if err != nil {
			return err
		}
		if rid != id {
			return fmt.Errorf("corrupt snapshot: object id %v out of order (%v)", rid, id)
		}
		kind, err := d.d.u8()
		if err != nil {
			return err
		}
		d.kinds[id] = kind
		switch kind {
		case kindArray:
			vt, err := d.d.i32()
			if err != nil {
				return err
			}
			total, err := d.d.u32()
			if err != nil {
				return err
			}
			d.arrays[int32(id)] = &backingHolder{vt: Type(vt), data: make([]Value, total)}
		case kindSlice:
			vt, err := d.d.i32()
			if err != nil {
				return err
			}
			arrID, err := d.d.u32()
			if err != nil {
				return err
			}
			off, err := d.d.i32()
			if err != nil {
				return err
			}
			length, err := d.d.i32()
			if err != nil {
				return err
			}
			if arrID == nilRef {
				if off != 0 || length != 0 {
					return errors.New("corrupt snapshot: nil slice with non-zero bounds")
				}
				d.objs[id] = Value{t: sliceType(Type(vt))}
				break
			}
			bd, ok := d.arrays[int32(arrID)]
			if !ok {
				return fmt.Errorf("corrupt snapshot: slice %v unknown array %v", id, arrID)
			}
			if off < 0 || length < 0 || int64(off)+int64(length) > int64(len(bd.data)) {
				return errors.New("corrupt snapshot: slice bounds out of range")
			}
			d.objs[id] = newSlice(bd.vt, bd.data[off:int64(off)+int64(length)])
		case kindStringMap:
			vt, err := d.d.i32()
			if err != nil {
				return err
			}
			m := &stringMap{valueType: Type(vt), data: map[string]Value{}}
			d.objs[id] = Value{t: mapType(TypeString, Type(vt)), value: m}
		case kindNumericMap:
			kt, err := d.d.i32()
			if err != nil {
				return err
			}
			vt, err := d.d.i32()
			if err != nil {
				return err
			}
			m := &numericMap{keyType: Type(kt), valueType: Type(vt), data: map[float64]Value{}}
			d.objs[id] = Value{t: mapType(Type(kt), Type(vt)), value: m}
		case kindStruct:
			typeN, err := d.d.i32()
			if err != nil {
				return err
			}
			st := &structT{TypeN: int(typeN), Fields: newIntMap(0)}
			d.objs[id] = Value{t: TypeStruct | Type(typeN<<8), value: st}
		case kindFunc:
			unit, err := d.d.u8()
			if err != nil {
				return err
			}
			fid, err := d.d.u32()
			if err != nil {
				return err
			}
			if int(unit) >= len(s.prog.units) {
				return fmt.Errorf("corrupt snapshot: function unit %v out of range", unit)
			}
			if _, ok := s.prog.funcs[funcKey(int(unit), int(fid))]; !ok {
				return fmt.Errorf("snapshot function is not defined in this source: unit %v id %v", unit, fid)
			}
			d.objs[id] = s.prog.ensureFunc(int(unit), int(fid))
		case kindBound:
			recv, err := d.d.u32()
			if err != nil {
				return err
			}
			inner, err := d.d.u32()
			if err != nil {
				return err
			}
			if recv >= n || inner >= n {
				return errors.New("corrupt snapshot: bound method reference out of range")
			}
			d.bounds = append(d.bounds, boundMeta{id: id, recv: recv, inner: inner})
			d.objs[id] = Value{t: TypeFunc, value: &funcT{}}
		case kindNative:
			name, err := d.d.str()
			if err != nil {
				return err
			}
			args, err := d.d.i32()
			if err != nil {
				return err
			}
			rets, err := d.d.i32()
			if err != nil {
				return err
			}
			variadic, err := d.d.bool8()
			if err != nil {
				return err
			}
			nv := d.g.Get(name)
			ft, ok := nv.value.(*funcT)
			if !ok || !ft.Native {
				return fmt.Errorf("snapshot requires native binding %q, which is missing or not a native function", name)
			}
			if ft.Args != int(args) || ft.Rets != int(rets) || ft.Variadic != variadic {
				return fmt.Errorf("snapshot requires native binding %q with %v args, %v returns, variadic=%v; installed function has %v args, %v returns, variadic=%v",
					name, args, rets, variadic, ft.Args, ft.Rets, ft.Variadic)
			}
			d.objs[id] = nv
		case kindSliceIter:
			sid, err := d.d.u32()
			if err != nil {
				return err
			}
			pn, err := d.d.i32()
			if err != nil {
				return err
			}
			it := &sliceNext{N: int(pn)}
			if sid != nilRef {
				if sid >= n {
					return errors.New("corrupt snapshot: iterator slice out of range")
				}
				it.S = d.objs[sid]
			}
			d.objs[id] = Value{t: typeNext, value: it}
		case kindStringIter:
			pn, err := d.d.i32()
			if err != nil {
				return err
			}
			d.objs[id] = Value{t: typeNext, value: &stringNext{N: int(pn)}}
		case kindMapIter:
			mid, err := d.d.u32()
			if err != nil {
				return err
			}
			pn, err := d.d.i32()
			if err != nil {
				return err
			}
			if mid >= n {
				return errors.New("corrupt snapshot: iterator map out of range")
			}
			d.objs[id] = Value{t: typeNext, value: &mapNext{M: d.objs[mid], N: int(pn)}}
		default:
			return fmt.Errorf("corrupt snapshot: unknown object kind %q", string(kind))
		}
	}

	// ---- struct schemas ----
	sn, err := d.d.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < sn; i++ {
		typeN, err := d.d.i32()
		if err != nil {
			return err
		}
		fieldN, err := d.d.u32()
		if err != nil {
			return err
		}
		sc := &typeSchema{lookup: map[string]int{}}
		for j := uint32(0); j < fieldN; j++ {
			name, err := d.d.str()
			if err != nil {
				return err
			}
			idx, err := d.d.i32()
			if err != nil {
				return err
			}
			sc.lookup[name] = int(idx)
			sc.order = append(sc.order, name)
		}
		methodN, err := d.d.u32()
		if err != nil {
			return err
		}
		methods := newIntMap(int(methodN))
		for j := uint32(0); j < methodN; j++ {
			idx, err := d.d.i32()
			if err != nil {
				return err
			}
			fid, err := d.d.u32()
			if err != nil {
				return err
			}
			if fid >= n {
				return errors.New("corrupt snapshot: method reference out of range")
			}
			fv := d.objs[fid]
			if fv.value == nil {
				return errors.New("corrupt snapshot: nil method")
			}
			methods.Set(int(idx), fv)
		}
		sc.methods = &methods
		d.schemas[int(typeN)] = sc
	}

	// ---- object bodies ----
	for id := uint32(0); id < n; id++ {
		kind, err := d.d.u8()
		if err != nil {
			return err
		}
		if kind != d.kinds[id] {
			return fmt.Errorf("corrupt snapshot: body kind mismatch at %v", id)
		}
		switch kind {
		case kindArray:
			count, err := d.d.u32()
			if err != nil {
				return err
			}
			bd := d.arrays[int32(id)]
			if int(count) != len(bd.data) {
				return errors.New("corrupt snapshot: array length mismatch")
			}
			for i := uint32(0); i < count; i++ {
				v, err := d.readValue()
				if err != nil {
					return err
				}
				bd.data[i] = v
			}
		case kindSlice, kindFunc, kindNative, kindSliceIter, kindBound:
		case kindStringMap:
			count, err := d.d.u32()
			if err != nil {
				return err
			}
			m := d.objs[id].value.(*stringMap)
			keys := make([]string, 0, count)
			for i := uint32(0); i < count; i++ {
				k, err := d.d.str()
				if err != nil {
					return err
				}
				v, err := d.readValue()
				if err != nil {
					return err
				}
				keys = append(keys, k)
				m.data[k] = v.assign(m.valueType)
			}
			m.keys = keys
		case kindNumericMap:
			count, err := d.d.u32()
			if err != nil {
				return err
			}
			m := d.objs[id].value.(*numericMap)
			keys := make([]float64, 0, count)
			for i := uint32(0); i < count; i++ {
				k, err := d.d.f64()
				if err != nil {
					return err
				}
				v, err := d.readValue()
				if err != nil {
					return err
				}
				keys = append(keys, k)
				m.data[k] = v.assign(m.valueType)
			}
			m.keys = keys
		case kindStruct:
			count, err := d.d.u32()
			if err != nil {
				return err
			}
			st := d.objs[id].value.(*structT)
			sc, ok := d.schemas[st.TypeN]
			if !ok {
				return fmt.Errorf("corrupt snapshot: missing schema for struct type %v", st.TypeN)
			}
			st.Lookup = sc.lookup
			st.Order = sc.order
			st.Methods = sc.methods
			st.Fields = newIntMap(int(count))
			for i := uint32(0); i < count; i++ {
				idx, err := d.d.i32()
				if err != nil {
					return err
				}
				v, err := d.readValue()
				if err != nil {
					return err
				}
				st.Fields.Set(int(idx), v)
			}
		case kindStringIter:
			count, err := d.d.u32()
			if err != nil {
				return err
			}
			runes := make([]rune, count)
			for i := uint32(0); i < count; i++ {
				r, err := d.d.i32()
				if err != nil {
					return err
				}
				runes[i] = rune(r)
			}
			d.objs[id].value.(*stringNext).R = runes
		case kindMapIter:
			count, err := d.d.u32()
			if err != nil {
				return err
			}
			keys := make([]Value, count)
			for i := uint32(0); i < count; i++ {
				v, err := d.readValue()
				if err != nil {
					return err
				}
				keys[i] = v
			}
			d.objs[id].value.(*mapNext).Keys = keys
		default:
			return fmt.Errorf("corrupt snapshot: unknown body kind %q", string(kind))
		}
	}

	// Bound methods can be rebuilt now that receivers and inner functions
	// have been fully restored.
	for _, bm := range d.bounds {
		recv := d.objs[bm.recv]
		inner, ok := d.objs[bm.inner].value.(*funcT)
		if !ok {
			return errors.New("corrupt snapshot: bound method inner value is not a function")
		}
		wrapper := newMethod(recv, inner)
		shell := d.objs[bm.id].value.(*funcT)
		*shell = *wrapper.value.(*funcT)
	}

	// ---- globals ----
	gn, err := d.d.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < gn; i++ {
		name, err := d.d.str()
		if err != nil {
			return err
		}
		v, err := d.readValue()
		if err != nil {
			return err
		}
		idx := d.g.Index(name)
		d.g.Write(idx, v)
	}

	// ---- stack ----
	sn2, err := d.d.u32()
	if err != nil {
		return err
	}
	stack := make([]Value, sn2)
	for i := uint32(0); i < sn2; i++ {
		v, err := d.readValue()
		if err != nil {
			return err
		}
		stack[i] = v
	}

	// ---- frames ----
	fn, err := d.d.u32()
	if err != nil {
		return err
	}
	if fn == 0 {
		return errors.New("corrupt snapshot: no frames")
	}
	frs := make([]frame, fn)
	for i := uint32(0); i < fn; i++ {
		unit, err := d.d.u8()
		if err != nil {
			return err
		}
		nv, err := d.d.i32()
		if err != nil {
			return err
		}
		baseN, err := d.d.i32()
		if err != nil {
			return err
		}
		if int(unit) >= len(s.prog.units) {
			return fmt.Errorf("corrupt snapshot: frame unit %v out of range", unit)
		}
		codes := s.prog.units[unit]
		var info *frameInfo
		hasInfo, err := d.d.bool8()
		if err != nil {
			return err
		}
		if hasInfo {
			topN, err := d.d.i32()
			if err != nil {
				return err
			}
			xRets, err := d.d.i32()
			if err != nil {
				return err
			}
			fu, err := d.d.u8()
			if err != nil {
				return err
			}
			fid, err := d.d.u32()
			if err != nil {
				return err
			}
			reg, ok := s.prog.funcs[funcKey(int(fu), int(fid))]
			if !ok {
				return fmt.Errorf("corrupt snapshot: unknown frame function %v:%v", fu, fid)
			}
			codes = reg.body
			info = &frameInfo{reg: reg, TopN: int(topN), XRets: int(xRets)}
		}
		if nv < 0 || int(nv) > len(codes) {
			return errors.New("corrupt snapshot: frame instruction out of range")
		}
		if baseN < 0 || int(baseN) > len(stack) {
			return errors.New("corrupt snapshot: frame base out of range")
		}
		frs[i] = frame{
			Unit: int(unit), Codes: codes, N: int(nv), BaseN: int(baseN), info: info,
		}
	}
	for i := 0; i+1 < len(frs); i++ {
		fr := frs[i]
		if fr.N >= len(fr.Codes) {
			return errors.New("corrupt snapshot: caller frame position out of range")
		}
		s.vm.backtrace = append(s.vm.backtrace, fr.Codes[fr.N].Pos)
	}
	s.vm.stack = stack
	s.vm.frames = frs[:len(frs)-1]
	s.vm.frame = frs[len(frs)-1]
	return nil
}
