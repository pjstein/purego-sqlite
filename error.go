package sqlite3

import (
	"errors"
	"strconv"
)

// Code is a SQLite result code. Both primary codes (SQLITE_BUSY) and extended
// codes (SQLITE_BUSY_SNAPSHOT) are represented; the low 8 bits of an extended
// code are its primary code.
//
// Code implements error, so it can be used directly with errors.Is against an
// error returned by this package:
//
//	if errors.Is(err, sqlite3.Busy) { ... }
//
// Matching is deliberately asymmetric in the useful direction: a primary code
// matches any of its extended codes, an extended code matches only itself.
type Code int32

// Primary result codes.
const (
	OK         Code = 0
	ErrorCode  Code = 1
	Internal   Code = 2
	Perm       Code = 3
	Abort      Code = 4
	Busy       Code = 5
	Locked     Code = 6
	NoMem      Code = 7
	ReadOnly   Code = 8
	Interrupt  Code = 9
	IOErr      Code = 10
	Corrupt    Code = 11
	NotFound   Code = 12
	Full       Code = 13
	CantOpen   Code = 14
	Protocol   Code = 15
	Empty      Code = 16
	Schema     Code = 17
	TooBig     Code = 18
	Constraint Code = 19
	Mismatch   Code = 20
	Misuse     Code = 21
	NoLFS      Code = 22
	Auth       Code = 23
	Format     Code = 24
	Range      Code = 25
	NotADB     Code = 26
	Notice     Code = 27
	Warning    Code = 28
	Row        Code = 100
	Done       Code = 101
)

// Extended result codes this package's users are most likely to branch on.
// Any extended code SQLite returns is preserved on [Error]; these are named
// for convenience, not as an exhaustive list.
const (
	BusyRecovery          Code = Busy | (1 << 8)
	BusySnapshot          Code = Busy | (2 << 8)
	BusyTimeout           Code = Busy | (3 << 8)
	ConstraintCheck       Code = Constraint | (1 << 8)
	ConstraintForeignKey  Code = Constraint | (3 << 8)
	ConstraintNotNull     Code = Constraint | (5 << 8)
	ConstraintPrimaryKey  Code = Constraint | (6 << 8)
	ConstraintTrigger     Code = Constraint | (7 << 8)
	ConstraintUnique      Code = Constraint | (8 << 8)
	ConstraintRowID       Code = Constraint | (10 << 8)
	ReadOnlyRollback      Code = ReadOnly | (1 << 8)
	CantOpenNotempdir     Code = CantOpen | (1 << 8)
	IOErrWrite            Code = IOErr | (3 << 8)
	IOErrFsync            Code = IOErr | (4 << 8)
	NoticeRecoverWAL      Code = Notice | (1 << 8)
	NoticeRecoverRollback Code = Notice | (2 << 8)
)

// Primary returns the primary result code underlying c. For a primary code it
// returns c unchanged.
func (c Code) Primary() Code { return c & 0xff }

// IsExtended reports whether c carries extended detail beyond its primary code.
func (c Code) IsExtended() bool { return c > 0xff && c != Row && c != Done }

func (c Code) Error() string { return c.String() }

func (c Code) String() string {
	if s := sqliteErrstr(c); s != "" {
		if c.IsExtended() {
			return s + " (" + strconv.Itoa(int(c)) + ")"
		}
		return s
	}
	return "sqlite3: unknown error " + strconv.Itoa(int(c))
}

// Error is a failed SQLite call: the codes SQLite reported, the message it
// produced, and the SQL in flight when the call failed, where this package
// knows it.
type Error struct {
	Code     Code   // primary result code
	Extended Code   // extended result code; equals Code when SQLite had no detail
	Message  string // sqlite3_errmsg text
	SQL      string // statement text, when the failure came from one
}

func (e *Error) Error() string {
	s := "sqlite3: " + e.Code.String()
	if e.Extended != e.Code && e.Extended != 0 {
		s += " [" + strconv.Itoa(int(e.Extended)) + "]"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.SQL != "" {
		s += " (in " + truncate(e.SQL, 120) + ")"
	}
	return s
}

// Is lets errors.Is match an *Error against a bare [Code]. A primary code
// matches when the error's primary code is equal; an extended code matches
// only the identical extended code.
func (e *Error) Is(target error) bool {
	var c Code
	if !errors.As(target, &c) {
		return false
	}
	if c.IsExtended() {
		return e.Extended == c
	}
	return e.Code.Primary() == c.Primary()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// errCode builds an Error from a result code alone, for calls made before a
// connection exists (or after it is gone) where sqlite3_errmsg is unavailable.
func errCode(rc Code, sql string) error {
	if rc == OK {
		return nil
	}
	return &Error{Code: rc.Primary(), Extended: rc, Message: sqliteErrstr(rc), SQL: sql}
}
