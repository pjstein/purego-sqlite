package sqlite3

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"unsafe"
)

// Stmt is a compiled SQL statement.
//
// Parameter indexes are 1-based and column indexes are 0-based, because that
// is what SQLite's own documentation says. Translating one of them here would
// only mean every reader has to translate it back.
type Stmt struct {
	conn   *Conn
	stmt   uintptr
	sql    string
	ncol   int32
	sawRow bool
}

var errStmtClosed = errors.New("sqlite3: statement is closed")

// emptyByte gives bind calls a valid non-nil pointer for zero-length values.
// sqlite3_bind_text with a NULL pointer binds SQL NULL, which is a different
// value from the empty string.
var emptyByte byte

func (s *Stmt) check() error {
	if s == nil || s.stmt == 0 {
		return errStmtClosed
	}
	return s.conn.check()
}

func (s *Stmt) err(rc Code) error { return s.conn.err(rc, s.sql) }

// SQL is the text this statement was compiled from.
func (s *Stmt) SQL() string { return s.sql }

// ExpandedSQL is the statement text with its bound parameters substituted in,
// which is what belongs in a log line about a failing query.
func (s *Stmt) ExpandedSQL() string {
	if s.check() != nil {
		return ""
	}
	p := lib.expandedSQL(s.stmt)
	if p == 0 {
		return ""
	}
	out := cString(p)
	lib.free(p)
	return out
}

// ReadOnly reports whether the statement makes no direct changes to the
// database.
func (s *Stmt) ReadOnly() bool {
	return s.check() == nil && lib.stmtReadonly(s.stmt) != 0
}

// Busy reports whether the statement is mid-execution: it has returned at
// least one row and has neither run to completion nor been reset.
func (s *Stmt) Busy() bool {
	return s.check() == nil && lib.stmtBusy(s.stmt) != 0
}

// Step advances the statement, returning true when a row is available and
// false when the statement has run to completion.
//
// A statement that returns false, or an error, has stopped; call Reset before
// running it again.
func (s *Stmt) Step() (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	rc := cStep(s.stmt)
	switch rc {
	case Row:
		if !s.sawRow {
			// Column count is fixed for a compiled statement, but a v2/v3
			// statement silently recompiles itself after a schema change, and
			// the new program may have a different shape. Reading it once per
			// execution keeps the bounds checks honest without paying a call
			// per accessor.
			s.sawRow = true
			s.ncol = cColumnCount(s.stmt)
		}
		return true, nil
	case Done:
		return false, nil
	default:
		return false, s.err(rc)
	}
}

// Run executes a statement that returns no rows and resets it, ready to be
// bound and run again. It is the shape a prepared insert is used in.
func (s *Stmt) Run() error {
	for {
		row, err := s.Step()
		if err != nil {
			s.Reset()
			return err
		}
		if !row {
			return s.Reset()
		}
	}
}

// Reset returns the statement to its pre-execution state. Bindings survive a
// reset; use [Stmt.ClearBindings] to drop them.
//
// Reset reports the error of the statement's most recent execution, so an
// error already returned by Step will surface here a second time. That is
// SQLite's behaviour and hiding it would hide errors that only Reset can see.
func (s *Stmt) Reset() error {
	if err := s.check(); err != nil {
		return err
	}
	s.sawRow = false
	rc := cReset(s.stmt)
	return s.err(rc)
}

// ClearBindings sets every parameter back to NULL.
func (s *Stmt) ClearBindings() error {
	if err := s.check(); err != nil {
		return err
	}
	return s.err(cClearBindings(s.stmt))
}

// Close finalizes the statement. It is safe to call more than once.
func (s *Stmt) Close() error {
	if s == nil || s.stmt == 0 {
		return nil
	}
	stmt := s.stmt
	s.stmt = 0
	if s.conn != nil && s.conn.stmts != nil {
		delete(s.conn.stmts, s)
	}
	rc := Code(lib.finalize(stmt))
	if rc != OK && s.conn != nil && s.conn.db != 0 {
		return s.conn.err(rc, s.sql)
	}
	return nil
}

// ---- binding ----

// BindCount is the number of parameters in the statement.
func (s *Stmt) BindCount() int {
	if s.check() != nil {
		return 0
	}
	return int(lib.bindParameterCount(s.stmt))
}

// BindIndex is the 1-based index of a named parameter, or 0 if there is no
// such parameter. The name includes its prefix, as in ":id" or "@id".
func (s *Stmt) BindIndex(name string) int {
	if s.check() != nil {
		return 0
	}
	return int(lib.bindParameterIndex(s.stmt, name))
}

// BindName is the name of the parameter at a 1-based index, or "" for a
// positional parameter.
func (s *Stmt) BindName(i int) string {
	if s.check() != nil {
		return ""
	}
	return lib.bindParameterName(s.stmt, int32(i))
}

// BindInt64 binds an integer to the 1-based parameter i.
func (s *Stmt) BindInt64(i int, v int64) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.err(cBindInt64(s.stmt, int32(i), v))
}

// BindFloat binds a floating-point value to the 1-based parameter i.
func (s *Stmt) BindFloat(i int, v float64) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.err(Code(lib.bindDouble(s.stmt, int32(i), v)))
}

// BindBool binds a boolean as SQLite's 0 or 1, which is how SQLite represents
// truth: it has no boolean type.
func (s *Stmt) BindBool(i int, v bool) error {
	if v {
		return s.BindInt64(i, 1)
	}
	return s.BindInt64(i, 0)
}

// BindText binds a string. The string may contain NUL bytes and any UTF-8
// sequence; the length is passed explicitly rather than inferred from a
// terminator.
func (s *Stmt) BindText(i int, v string) error {
	if err := s.check(); err != nil {
		return err
	}
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(unsafe.StringData(v))
	}
	rc := cBindText(s.stmt, int32(i), p, int32(len(v)))
	runtime.KeepAlive(v)
	return s.err(rc)
}

// BindBlob binds a byte slice. A nil slice binds SQL NULL; a non-nil empty
// slice binds a zero-length blob, which is a distinct value.
func (s *Stmt) BindBlob(i int, v []byte) error {
	if err := s.check(); err != nil {
		return err
	}
	if v == nil {
		return s.BindNull(i)
	}
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(&v[0])
	}
	rc := cBindBlob(s.stmt, int32(i), p, int32(len(v)))
	runtime.KeepAlive(v)
	return s.err(rc)
}

// BindZeroBlob binds a blob of n zero bytes without materializing it, for
// reserving space to be filled in later.
func (s *Stmt) BindZeroBlob(i, n int) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.err(cBindZeroblob(s.stmt, int32(i), int32(n)))
}

// BindNull binds SQL NULL.
func (s *Stmt) BindNull(i int) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.err(cBindNull(s.stmt, int32(i)))
}

// Bind binds every argument positionally, starting at parameter 1.
//
// Supported: nil, bool, the sized and unsized integer types, float32, float64,
// string, and []byte. A uint64 above math.MaxInt64 is rejected rather than
// wrapped to a negative integer, because SQLite has no unsigned type and a
// silent wrap is a corrupted value.
//
// time.Time is deliberately absent: SQLite stores time as whatever the
// application decides -- Unix seconds, Julian day, or ISO-8601 text -- and a
// binding that picked one for you would be picking your schema.
func (s *Stmt) Bind(args ...any) error {
	for i, a := range args {
		idx := i + 1
		var err error
		switch v := a.(type) {
		case nil:
			err = s.BindNull(idx)
		case bool:
			err = s.BindBool(idx, v)
		case int:
			err = s.BindInt64(idx, int64(v))
		case int8:
			err = s.BindInt64(idx, int64(v))
		case int16:
			err = s.BindInt64(idx, int64(v))
		case int32:
			err = s.BindInt64(idx, int64(v))
		case int64:
			err = s.BindInt64(idx, v)
		case uint:
			if uint64(v) > math.MaxInt64 {
				return fmt.Errorf("sqlite3: parameter %d: uint %d exceeds SQLite's signed 64-bit integer range", idx, v)
			}
			err = s.BindInt64(idx, int64(v))
		case uint8:
			err = s.BindInt64(idx, int64(v))
		case uint16:
			err = s.BindInt64(idx, int64(v))
		case uint32:
			err = s.BindInt64(idx, int64(v))
		case uint64:
			if v > math.MaxInt64 {
				return fmt.Errorf("sqlite3: parameter %d: uint64 %d exceeds SQLite's signed 64-bit integer range", idx, v)
			}
			err = s.BindInt64(idx, int64(v))
		case float32:
			err = s.BindFloat(idx, float64(v))
		case float64:
			err = s.BindFloat(idx, v)
		case string:
			err = s.BindText(idx, v)
		case []byte:
			err = s.BindBlob(idx, v)
		default:
			return fmt.Errorf("sqlite3: parameter %d: cannot bind %T", idx, a)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- columns ----

// ColumnCount is the number of columns in the result set, available as soon as
// the statement is prepared.
func (s *Stmt) ColumnCount() int {
	if s.check() != nil {
		return 0
	}
	return int(cColumnCount(s.stmt))
}

// DataCount is the number of values in the current row, which is zero unless
// the last Step returned a row.
func (s *Stmt) DataCount() int {
	if s.check() != nil {
		return 0
	}
	return int(cDataCount(s.stmt))
}

// ColumnName is the name of column i, as SQLite assigned it: the AS alias when
// there is one, otherwise something unspecified that should not be relied on.
func (s *Stmt) ColumnName(i int) string {
	if s.check() != nil {
		return ""
	}
	return lib.columnName(s.stmt, int32(i))
}

// ColumnDeclType is the declared type of column i in the CREATE TABLE that
// defined it, or "" for an expression.
func (s *Stmt) ColumnDeclType(i int) string {
	if s.check() != nil {
		return ""
	}
	return lib.columnDecltype(s.stmt, int32(i))
}

// inRow reports whether column i can be read from the current row. SQLite
// documents out-of-range column access as undefined; this package makes it
// defined, returning the zero value.
func (s *Stmt) inRow(i int) bool {
	return s.stmt != 0 && i >= 0 && int32(i) < s.ncol
}

// ColumnType is the storage class of the value in column i of the current row.
func (s *Stmt) ColumnType(i int) ColumnType {
	if !s.inRow(i) {
		return TypeNull
	}
	return cColumnType(s.stmt, int32(i))
}

// ColumnIsNull reports whether column i of the current row holds SQL NULL.
func (s *Stmt) ColumnIsNull(i int) bool { return s.ColumnType(i) == TypeNull }

// ColumnInt64 reads column i as an integer, converting per SQLite's rules.
func (s *Stmt) ColumnInt64(i int) int64 {
	if !s.inRow(i) {
		return 0
	}
	return cColumnInt64(s.stmt, int32(i))
}

// ColumnInt reads column i as a Go int.
func (s *Stmt) ColumnInt(i int) int { return int(s.ColumnInt64(i)) }

// ColumnBool reads column i as a boolean: false for zero, true otherwise.
func (s *Stmt) ColumnBool(i int) bool { return s.ColumnInt64(i) != 0 }

// ColumnFloat reads column i as a float, converting per SQLite's rules.
func (s *Stmt) ColumnFloat(i int) float64 {
	if !s.inRow(i) {
		return 0
	}
	return lib.columnDouble(s.stmt, int32(i))
}

// ColumnText reads column i as a string, copied into Go memory. Text
// containing NUL bytes survives intact: the length comes from SQLite, not from
// scanning for a terminator.
func (s *Stmt) ColumnText(i int) string {
	if !s.inRow(i) {
		return ""
	}
	// Order matters: the pointer must be fetched before the length, because
	// fetching the pointer can convert the value and change its length.
	p := cColumnTextPtr(s.stmt, int32(i))
	n := cColumnBytes(s.stmt, int32(i))
	return string(cBytes(p, n))
}

// ColumnBlob reads column i as a byte slice, copied into Go memory. It returns
// nil for SQL NULL and a non-nil empty slice for a zero-length blob.
func (s *Stmt) ColumnBlob(i int) []byte {
	if !s.inRow(i) {
		return nil
	}
	if cColumnType(s.stmt, int32(i)) == TypeNull {
		return nil
	}
	p := cColumnBlobPtr(s.stmt, int32(i))
	n := cColumnBytes(s.stmt, int32(i))
	out := make([]byte, n)
	copy(out, cBytes(p, n))
	return out
}

// ColumnBytes is the byte length of column i as text or blob.
func (s *Stmt) ColumnBytes(i int) int {
	if !s.inRow(i) {
		return 0
	}
	return int(cColumnBytes(s.stmt, int32(i)))
}

// ColumnRawBlob returns column i as a slice aliasing SQLite's own memory, with
// no copy.
//
// The slice is valid only until the next Step, Reset, Close, or any column
// accessor on this statement that forces a type conversion. Write to it and
// you are writing into SQLite's buffer. Use [Stmt.ColumnBlob] unless the copy
// is measurably the problem.
func (s *Stmt) ColumnRawBlob(i int) []byte {
	if !s.inRow(i) {
		return nil
	}
	p := cColumnBlobPtr(s.stmt, int32(i))
	n := cColumnBytes(s.stmt, int32(i))
	return cBytes(p, n)
}

// ColumnRawText returns column i's UTF-8 bytes aliasing SQLite's own memory,
// under the same lifetime rules as [Stmt.ColumnRawBlob].
func (s *Stmt) ColumnRawText(i int) []byte {
	if !s.inRow(i) {
		return nil
	}
	p := cColumnTextPtr(s.stmt, int32(i))
	n := cColumnBytes(s.stmt, int32(i))
	return cBytes(p, n)
}
