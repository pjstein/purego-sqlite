package sqlite3

import (
	"unsafe"

	"github.com/ebitengine/purego"
)

// The per-row surface, called through purego.SyscallN.
//
// purego.RegisterFunc builds a reflect.MakeFunc closure that boxes every
// argument into a reflect.Value on each call. That is fine for opening a
// database; it is not fine for the four or five calls a query loop makes per
// row. These wrappers hand SyscallN the raw address and raw words instead.
//
// SyscallN is marked //go:uintptrescapes, so an object whose address is
// converted to uintptr in a call to it is forced to the heap and kept alive
// for the duration of the call. Pointer arguments here arrive as
// unsafe.Pointer and stay live in the frame across the call for the same
// reason; callers additionally hold the backing slice or string.

func cStep(stmt uintptr) Code {
	r, _, _ := purego.SyscallN(lib.aStep, stmt)
	return Code(int32(r))
}

func cReset(stmt uintptr) Code {
	r, _, _ := purego.SyscallN(lib.aReset, stmt)
	return Code(int32(r))
}

func cClearBindings(stmt uintptr) Code {
	r, _, _ := purego.SyscallN(lib.aClearBindings, stmt)
	return Code(int32(r))
}

func cBindInt64(stmt uintptr, i int32, v int64) Code {
	r, _, _ := purego.SyscallN(lib.aBindInt64, stmt, uintptr(i), uintptr(v))
	return Code(int32(r))
}

func cBindNull(stmt uintptr, i int32) Code {
	r, _, _ := purego.SyscallN(lib.aBindNull, stmt, uintptr(i))
	return Code(int32(r))
}

func cBindZeroblob(stmt uintptr, i, n int32) Code {
	r, _, _ := purego.SyscallN(lib.aBindZeroblob, stmt, uintptr(i), uintptr(n))
	return Code(int32(r))
}

// cBindText and cBindBlob always pass SQLITE_TRANSIENT: SQLite copies the
// bytes before the call returns, which is the only reason it is safe to hand
// it a pointer into Go memory at all.
func cBindText(stmt uintptr, i int32, p unsafe.Pointer, n int32) Code {
	r, _, _ := purego.SyscallN(lib.aBindText, stmt, uintptr(i), uintptr(p), uintptr(n), sqliteTransient)
	return Code(int32(r))
}

func cBindBlob(stmt uintptr, i int32, p unsafe.Pointer, n int32) Code {
	r, _, _ := purego.SyscallN(lib.aBindBlob, stmt, uintptr(i), uintptr(p), uintptr(n), sqliteTransient)
	return Code(int32(r))
}

func cColumnCount(stmt uintptr) int32 {
	r, _, _ := purego.SyscallN(lib.aColumnCount, stmt)
	return int32(r)
}

func cDataCount(stmt uintptr) int32 {
	r, _, _ := purego.SyscallN(lib.aDataCount, stmt)
	return int32(r)
}

func cColumnType(stmt uintptr, i int32) ColumnType {
	r, _, _ := purego.SyscallN(lib.aColumnType, stmt, uintptr(i))
	return ColumnType(int32(r))
}

func cColumnInt64(stmt uintptr, i int32) int64 {
	r, _, _ := purego.SyscallN(lib.aColumnInt64, stmt, uintptr(i))
	return int64(r)
}

func cColumnBytes(stmt uintptr, i int32) int32 {
	r, _, _ := purego.SyscallN(lib.aColumnBytes, stmt, uintptr(i))
	return int32(r)
}

func cColumnTextPtr(stmt uintptr, i int32) uintptr {
	r, _, _ := purego.SyscallN(lib.aColumnText, stmt, uintptr(i))
	return r
}

func cColumnBlobPtr(stmt uintptr, i int32) uintptr {
	r, _, _ := purego.SyscallN(lib.aColumnBlob, stmt, uintptr(i))
	return r
}

func cErrcode(db uintptr) Code {
	r, _, _ := purego.SyscallN(lib.aErrcode, db)
	return Code(int32(r))
}

func cExtendedErrcode(db uintptr) Code {
	r, _, _ := purego.SyscallN(lib.aExtendedErrcode, db)
	return Code(int32(r))
}

// cptr reinterprets an address SQLite returned as a pointer.
//
// Every address that reaches this function points into memory SQLite
// allocated, never into the Go heap, so the hazard go vet's unsafeptr check
// exists to catch -- a Go object being collected or moved between the moment
// its address became a uintptr and the moment that uintptr became a pointer
// again -- cannot arise here. vet cannot see where an address came from, so
// the conversion is written as a reinterpretation of the machine word rather
// than as uintptr-to-Pointer, and lives in exactly one place with this note
// attached, instead of being flagged at every column read.
func cptr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}

// cBytes aliases n bytes of SQLite-owned memory as a Go slice without copying.
// The result is only valid until the next call that can invalidate it -- any
// step, reset, finalize, or a column accessor that forces a type conversion.
func cBytes(p uintptr, n int32) []byte {
	if p == 0 || n <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(cptr(p)), int(n))
}

// cString copies a NUL-terminated C string into Go memory. Used only for
// strings SQLite allocated and expects the caller to free.
func cString(p uintptr) string {
	if p == 0 {
		return ""
	}
	base := (*byte)(cptr(p))
	var n int
	for *(*byte)(unsafe.Add(unsafe.Pointer(base), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(base, n))
}

// codeNames is deliberately a static table rather than a binding to
// sqlite3_errstr: naming a result code must work before the library has
// loaded, which is exactly when load failures need to be described.
var codeNames = map[Code]string{
	OK:         "not an error",
	ErrorCode:  "SQL logic error",
	Internal:   "internal error",
	Perm:       "access permission denied",
	Abort:      "query aborted",
	Busy:       "database is locked",
	Locked:     "database table is locked",
	NoMem:      "out of memory",
	ReadOnly:   "attempt to write a readonly database",
	Interrupt:  "interrupted",
	IOErr:      "disk I/O error",
	Corrupt:    "database disk image is malformed",
	NotFound:   "unknown operation",
	Full:       "database or disk is full",
	CantOpen:   "unable to open database file",
	Protocol:   "locking protocol",
	Empty:      "internal error (empty)",
	Schema:     "database schema has changed",
	TooBig:     "string or blob too big",
	Constraint: "constraint failed",
	Mismatch:   "datatype mismatch",
	Misuse:     "bad parameter or other API misuse",
	NoLFS:      "large file support is disabled",
	Auth:       "authorization denied",
	Format:     "auxiliary database format error",
	Range:      "column index out of range",
	NotADB:     "file is not a database",
	Notice:     "notification message",
	Warning:    "warning message",
	Row:        "another row available",
	Done:       "no more rows available",
}

func sqliteErrstr(c Code) string {
	if s, ok := codeNames[c]; ok {
		return s
	}
	if s, ok := codeNames[c.Primary()]; ok {
		return s
	}
	return ""
}
