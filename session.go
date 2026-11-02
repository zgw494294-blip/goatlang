package goatlang

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
)

// Session statuses.
const (
	StatusReady  = "ready"
	StatusPaused = "paused"
	StatusDone   = "done"
	StatusFailed = "failed"
	StatusClosed = "closed"
)

// Session is an explicitly selected, persistable execution of one source
// program. It is created ready via VM.NewSession, runs until the next
// checkpoint() with Resume, and its paused state can be exported with
// Snapshot and continued in another process through VM.RestoreSession.
type Session struct {
	parent *VM
	vm     *VM
	prog   *program

	status string
	final  []Value

	fname  string
	source string

	sourceHash  [sha256.Size]byte
	importsHash [sha256.Size]byte
}

// recordingFS wraps an fs.FS and hashes every file read while preparing a
// program, so restoring can verify imported sources match as well.
type recordingFS struct {
	fs      fs.FS
	content map[string][]byte
}

func (r *recordingFS) Open(name string) (fs.File, error) {
	return r.fs.Open(name)
}

func (r *recordingFS) ReadFile(name string) ([]byte, error) {
	b, err := fs.ReadFile(r.fs, name)
	if err != nil {
		return nil, err
	}
	if r.content == nil {
		r.content = map[string][]byte{}
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	r.content[name] = cp
	return b, nil
}

type globFS interface {
	Glob(pattern string) ([]string, error)
}

func (r *recordingFS) Glob(pattern string) ([]string, error) {
	if g, ok := r.fs.(globFS); ok {
		return g.Glob(pattern)
	}
	return fs.Glob(r.fs, pattern)
}

func (r *recordingFS) hash() [sha256.Size]byte {
	names := make([]string, 0, len(r.content))
	for name := range r.content {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		io.WriteString(h, name)
		h.Write([]byte{0})
		h.Write(r.content[name])
		h.Write([]byte{0})
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func sourceHash(fname, source string) [sha256.Size]byte {
	h := sha256.New()
	io.WriteString(h, fname)
	h.Write([]byte{0})
	io.WriteString(h, source)
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// NewSession prepares source for resumable execution without running it.
// sys is used to resolve imports exactly like Eval. The returned session is
// in the ready status.
func (v *VM) NewSession(sys fs.FS, fname, source string) (*Session, error) {
	rec := &recordingFS{fs: sys}
	prog, err := prepareProgram(v.globals, rec, fname, source)
	if err != nil {
		return nil, err
	}
	s := &Session{
		parent:      v,
		prog:        prog,
		status:      StatusReady,
		fname:       fname,
		source:      source,
		sourceHash:  sourceHash(fname, source),
		importsHash: rec.hash(),
	}
	s.vm = s.newExecVM()
	return s, nil
}

func (s *Session) newExecVM() *VM {
	return &VM{
		globals:   s.parent.globals,
		stdout:    s.parent.stdout,
		resumable: true,
		prog:      s.prog,
	}
}

// Status returns the current session status: ready, paused, done, failed or
// closed.
func (s *Session) Status() string { return s.status }

// Values returns the final results of a finished session, with the same
// meaning as the values returned by VM.Eval. It returns nil unless the
// session reached done.
func (s *Session) Values() []Value {
	if s.status != StatusDone {
		return nil
	}
	return s.final
}

// Close ends a ready or paused session without running it further.
func (s *Session) Close() error {
	switch s.status {
	case StatusReady, StatusPaused:
		s.status = StatusClosed
		s.vm = nil
		return nil
	case StatusClosed:
		return errors.New("session already closed")
	default:
		return fmt.Errorf("cannot close session with status %q", s.status)
	}
}

// Resume runs until the next checkpoint() (paused), normal completion (done)
// or a script error (failed). Resuming a terminal session returns an error
// without executing anything.
func (s *Session) Resume() (err error) {
	switch s.status {
	case StatusReady:
		s.vm.stack = make([]Value, s.prog.slots[0])
		s.vm.frame = frame{Unit: 0, Codes: s.prog.units[0]}
	case StatusPaused:
		// frames and stack are already in place
	default:
		return fmt.Errorf("cannot resume session with status %q", s.status)
	}
	s.vm.stdout = s.parent.stdout
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(suspendPanic); ok {
				s.status = StatusPaused
				return
			}
			err = s.vm.btErr(r)
			s.status = StatusFailed
		}
	}()
	s.vm.exec()
	slots := s.prog.slots[len(s.prog.slots)-1]
	rets := s.vm.stack[slots:]
	final := make([]Value, len(rets))
	copy(final, rets)
	s.final = final
	s.status = StatusDone
	return nil
}

// Snapshot exports independently storable bytes for a paused session.
func (s *Session) Snapshot() ([]byte, error) {
	if s.status != StatusPaused {
		return nil, fmt.Errorf("snapshot requires status %q, session is %q", StatusPaused, s.status)
	}
	return encodeSnapshot(s)
}

// RestoreSession validates data against fname/source and the native bindings
// installed on v, rebuilds the paused session, and returns it in paused
// status. On any validation error no native business function is called and
// nothing is written to stdout.
func (v *VM) RestoreSession(sys fs.FS, fname, source string, data []byte) (*Session, error) {
	hdr, err := decodeHeader(data)
	if err != nil {
		return nil, err
	}
	wantSrc := sourceHash(fname, source)
	if hdr.sourceHash != wantSrc {
		return nil, errors.New("snapshot source does not match fname/source arguments")
	}
	rec := &recordingFS{fs: sys}
	prog, err := prepareProgram(v.globals, rec, fname, source)
	if err != nil {
		return nil, err
	}
	if hdr.importsHash != rec.hash() {
		return nil, errors.New("snapshot imports do not match the files resolved from sys")
	}
	s := &Session{
		parent:      v,
		prog:        prog,
		status:      StatusPaused,
		fname:       fname,
		source:      source,
		sourceHash:  hdr.sourceHash,
		importsHash: hdr.importsHash,
	}
	s.vm = s.newExecVM()
	if err := decodePayload(s, hdr); err != nil {
		return nil, err
	}
	return s, nil
}
