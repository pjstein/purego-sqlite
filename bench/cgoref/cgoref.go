//go:build cgo && darwin && arm64

// Package cgoref is a cgo binding to the *same* shipped SQLite library that
// the purego binding drives.
//
// It exists for one measurement: the purego tax. Comparing purego against a
// pure-Go SQLite would confound the calling convention with the entire
// implementation. Comparing it against cgo over the identical dylib, with the
// same number of individual calls crossing the boundary per row, isolates what
// the foreign-function mechanism itself costs.
//
// It is deliberately not a usable SQLite binding, and it lives in the
// benchmark module so that nothing shippable acquires a cgo dependency.
package cgoref

/*
#cgo LDFLAGS: -L${SRCDIR}/../../lib/darwin_arm64 -lsqlite3 -Wl,-rpath,${SRCDIR}/../../lib/darwin_arm64

#include <stdlib.h>
#include <stdint.h>

// Declared rather than #included: sqlite3.h lives in the gitignored upstream
// source tree, and this file must build from a fresh clone with only the
// committed dylib present.
typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;

extern int sqlite3_open_v2(const char*, sqlite3**, int, const char*);
extern int sqlite3_close_v2(sqlite3*);
extern int sqlite3_exec(sqlite3*, const char*, void*, void*, char**);
extern int sqlite3_prepare_v2(sqlite3*, const char*, int, sqlite3_stmt**, const char**);
extern int sqlite3_step(sqlite3_stmt*);
extern int sqlite3_reset(sqlite3_stmt*);
extern int sqlite3_finalize(sqlite3_stmt*);
extern int sqlite3_bind_int64(sqlite3_stmt*, int, int64_t);
extern int sqlite3_bind_text(sqlite3_stmt*, int, const char*, int, void*);
extern int sqlite3_bind_blob(sqlite3_stmt*, int, const void*, int, void*);
extern int64_t sqlite3_column_int64(sqlite3_stmt*, int);
extern const unsigned char* sqlite3_column_text(sqlite3_stmt*, int);
extern const void* sqlite3_column_blob(sqlite3_stmt*, int);
extern int sqlite3_column_bytes(sqlite3_stmt*, int);
extern int sqlite3_libversion_number(void);
extern const char* sqlite3_libversion(void);
extern const char* sqlite3_errmsg(sqlite3*);

// SQLITE_TRANSIENT is ((sqlite3_destructor_type)-1): SQLite copies the bytes
// before the call returns. Built in C so the -1-as-pointer is C's, not a Go
// uintptr conversion that go vet would rightly question.
static void* sqlite_transient(void) { return (void*)(-1); }

// Calls a function pointer n times from C, for measuring what a C-to-Go
// crossing costs with nothing behind it. This is how the purego callback floor
// gets a number instead of an adjective.
static void cgoref_call_n(void (*f)(void), int n) {
  for (int i = 0; i < n; i++) {
    f();
  }
}

// Same, for a callback taking one pointer argument -- the shape the binding's
// marshaled dispatch actually uses.
static void cgoref_call_n_arg(void (*f)(void *), void *arg, int n) {
  for (int i = 0; i < n; i++) {
    f(arg);
  }
}

// The row-batching shim, which lives in the same dylib. Declared here so the
// cgo reference can be measured at the same call granularity as the purego
// binding -- otherwise "purego with batching beats cgo without it" would be a
// comparison of two different designs wearing the label of a comparison of two
// calling conventions.
typedef struct {
  int64_t ival;
  double fval;
  const void *ptr;
  int32_t len;
  int32_t type;
} rowbatch_col;
extern int rowbatch_step(sqlite3_stmt *, int, rowbatch_col *);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Available reports that this build has the cgo reference compiled in.
const Available = true

const (
	openReadWrite = 0x00000002
	openCreate    = 0x00000004
	resultOK      = 0
	resultRow     = 100
	resultDone    = 101
)

// transient is SQLITE_TRANSIENT: SQLite copies the bytes during the call,
// which is what makes passing a pointer into Go memory safe.
var transient = C.sqlite_transient()

// Conn is a connection to the shipped SQLite library through cgo.
type Conn struct{ db *C.sqlite3 }

// Open opens a database, creating it if necessary.
func Open(path string) (*Conn, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	var db *C.sqlite3
	rc := C.sqlite3_open_v2(cpath, &db, C.int(openReadWrite|openCreate), nil)
	if rc != resultOK {
		if db != nil {
			msg := C.GoString(C.sqlite3_errmsg(db))
			C.sqlite3_close_v2(db)
			return nil, errors.New("cgoref: open: " + msg)
		}
		return nil, errors.New("cgoref: open failed")
	}
	return &Conn{db: db}, nil
}

func (c *Conn) Close() error {
	if c.db == nil {
		return nil
	}
	C.sqlite3_close_v2(c.db)
	c.db = nil
	return nil
}

func (c *Conn) err(what string) error {
	return errors.New("cgoref: " + what + ": " + C.GoString(C.sqlite3_errmsg(c.db)))
}

// Exec runs SQL returning no rows.
func (c *Conn) Exec(sql string) error {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	if rc := C.sqlite3_exec(c.db, csql, nil, nil, nil); rc != resultOK {
		return c.err("exec")
	}
	return nil
}

// Stmt is a compiled statement.
type Stmt struct {
	c    *Conn
	stmt *C.sqlite3_stmt
}

// Prepare compiles a statement.
func (c *Conn) Prepare(sql string) (*Stmt, error) {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	var st *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(c.db, csql, C.int(len(sql)), &st, nil); rc != resultOK {
		return nil, c.err("prepare")
	}
	return &Stmt{c: c, stmt: st}, nil
}

func (s *Stmt) Close() error {
	if s.stmt == nil {
		return nil
	}
	C.sqlite3_finalize(s.stmt)
	s.stmt = nil
	return nil
}

func (s *Stmt) BindInt64(param int, v int64) error {
	if rc := C.sqlite3_bind_int64(s.stmt, C.int(param), C.int64_t(v)); rc != resultOK {
		return s.c.err("bind_int64")
	}
	return nil
}

func (s *Stmt) BindText(param int, v string) error {
	p := (*C.char)(nil)
	if len(v) > 0 {
		p = (*C.char)(unsafe.Pointer(unsafe.StringData(v)))
	} else {
		p = (*C.char)(unsafe.Pointer(&emptyByte))
	}
	if rc := C.sqlite3_bind_text(s.stmt, C.int(param), p, C.int(len(v)), transient); rc != resultOK {
		return s.c.err("bind_text")
	}
	return nil
}

var emptyByte byte

func (s *Stmt) BindBlob(param int, v []byte) error {
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(&v[0])
	}
	if rc := C.sqlite3_bind_blob(s.stmt, C.int(param), p, C.int(len(v)), transient); rc != resultOK {
		return s.c.err("bind_blob")
	}
	return nil
}

// Step advances the statement, reporting whether a row is available.
func (s *Stmt) Step() (bool, error) {
	switch rc := C.sqlite3_step(s.stmt); rc {
	case resultRow:
		return true, nil
	case resultDone:
		return false, nil
	default:
		return false, s.c.err("step")
	}
}

func (s *Stmt) Reset() error {
	if rc := C.sqlite3_reset(s.stmt); rc != resultOK {
		return s.c.err("reset")
	}
	return nil
}

func (s *Stmt) ColumnInt64(col int) int64 {
	return int64(C.sqlite3_column_int64(s.stmt, C.int(col)))
}

func (s *Stmt) ColumnText(col int) string {
	p := C.sqlite3_column_text(s.stmt, C.int(col))
	n := C.sqlite3_column_bytes(s.stmt, C.int(col))
	if p == nil || n <= 0 {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(p)), n)
}

func (s *Stmt) ColumnBlob(col int) []byte {
	p := C.sqlite3_column_blob(s.stmt, C.int(col))
	n := C.sqlite3_column_bytes(s.stmt, C.int(col))
	if p == nil || n <= 0 {
		return nil
	}
	return C.GoBytes(p, n)
}

// Col mirrors the shim's rowbatch_col.
type Col = C.rowbatch_col

// NewRowBuffer allocates a row buffer for StepRow.
func NewRowBuffer(ncol int) []Col { return make([]Col, ncol) }

// StepRow steps the statement and, on a row, fills buf in a single crossing
// through the same shim the purego binding uses.
func (s *Stmt) StepRow(buf []Col) (bool, error) {
	rc := C.rowbatch_step(s.stmt, C.int(len(buf)), &buf[0])
	switch rc {
	case resultRow:
		return true, nil
	case resultDone:
		return false, nil
	default:
		return false, s.c.err("rowbatch_step")
	}
}

// ColValue reads one filled column out of a row buffer.
func ColValue(c Col) (typ int, ival int64, fval float64, b []byte) {
	typ = int(c._type)
	ival = int64(c.ival)
	fval = float64(c.fval)
	if c.ptr != nil && c.len > 0 {
		b = C.GoBytes(c.ptr, c.len)
	}
	return
}

// CallN invokes fn from C n times. fn must be a purego callback address --
// executable code purego allocated, never Go heap memory, which is why
// reinterpreting the word as a pointer is sound here. See cptr in the binding
// for the same argument at more length.
func CallN(fn uintptr, n int) {
	C.cgoref_call_n((*[0]byte)(*(*unsafe.Pointer)(unsafe.Pointer(&fn))), C.int(n))
}

// CallNArg invokes fn from C n times, passing arg each time.
func CallNArg(fn uintptr, arg unsafe.Pointer, n int) {
	C.cgoref_call_n_arg((*[0]byte)(*(*unsafe.Pointer)(unsafe.Pointer(&fn))), arg, C.int(n))
}

// NullCall is the cheapest call the library offers: no arguments, an integer
// return, and no work behind it. It measures the boundary crossing and
// nothing else.
func NullCall() int { return int(C.sqlite3_libversion_number()) }

// Version reports the library version, confirming that this binding and the
// purego one really are driving the same file.
func Version() string { return C.GoString(C.sqlite3_libversion()) }
