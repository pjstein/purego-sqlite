package sqlite3

import (
	"errors"
	"runtime"
	"time"
	"unsafe"
)

// Conn is a connection to a SQLite database.
//
// A Conn and every Stmt prepared on it belong to one goroutine at a time; see
// the package documentation on concurrency. Separate Conn values are safe to
// use concurrently, which is how this package expects to be used under load:
// one connection per worker.
type Conn struct {
	db     uintptr
	path   string
	closed bool
	stmts  map[*Stmt]struct{}
}

// Open opens (creating if necessary) the database at path for reading and
// writing, with extended result codes enabled.
//
// The special path ":memory:" gives a private in-memory database. Note that
// SQLite's URI forms are not interpreted unless [OpenURI] is passed to
// [OpenFlags].
func Open(path string) (*Conn, error) {
	return OpenFlags(path, OpenDefault, "")
}

// OpenFlags opens a database with an explicit SQLITE_OPEN_* flag set and an
// optional VFS name. An empty vfs selects SQLite's default.
func OpenFlags(path string, flags OpenFlag, vfs string) (*Conn, error) {
	if err := ensureLoaded(); err != nil {
		return nil, err
	}

	var vfsPtr uintptr
	var vfsBuf []byte
	if vfs != "" {
		vfsBuf = append([]byte(vfs), 0)
		vfsPtr = uintptr(unsafe.Pointer(&vfsBuf[0]))
	}

	var db uintptr
	rc := Code(lib.openV2(path, &db, int32(flags), vfsPtr))
	runtime.KeepAlive(vfsBuf)

	if rc != OK {
		// sqlite3_open_v2 allocates a handle even when it fails, precisely so
		// the caller can ask it why; leaking it would leak the message too.
		var err error
		if db != 0 {
			err = &Error{Code: rc.Primary(), Extended: cExtendedErrcode(db), Message: lib.errmsg(db), SQL: "open " + path}
			lib.closeV2(db)
		} else {
			err = errCode(rc, "open "+path)
		}
		return nil, err
	}

	return &Conn{db: db, path: path, stmts: make(map[*Stmt]struct{})}, nil
}

// Close finalizes any statements still open on the connection and closes it.
//
// Statements are finalized first on purpose: sqlite3_close_v2 would otherwise
// return success while leaving the connection alive as a zombie holding file
// locks until the last statement is finalized, which reads as "closed" and
// behaves as "still open".
func (c *Conn) Close() error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	for s := range c.stmts {
		if s.stmt != 0 {
			lib.finalize(s.stmt)
			s.stmt = 0
		}
	}
	c.stmts = nil
	rc := Code(lib.closeV2(c.db))
	db := c.db
	c.db = 0
	if rc != OK {
		return &Error{Code: rc.Primary(), Extended: rc, Message: lib.errmsg(db), SQL: "close " + c.path}
	}
	return nil
}

// Closed reports whether Close has been called.
func (c *Conn) Closed() bool { return c.closed }

// Path is the filename the connection was opened with.
func (c *Conn) Path() string { return c.path }

var errClosed = errors.New("sqlite3: connection is closed")

func (c *Conn) check() error {
	if c == nil || c.closed || c.db == 0 {
		return errClosed
	}
	return nil
}

// err turns a result code into a rich error carrying SQLite's own message and
// the extended code. Row and Done are successes, not errors.
func (c *Conn) err(rc Code, sql string) error {
	if rc == OK || rc == Row || rc == Done {
		return nil
	}
	return &Error{
		Code:     rc.Primary(),
		Extended: cExtendedErrcode(c.db),
		Message:  lib.errmsg(c.db),
		SQL:      sql,
	}
}

// Exec runs SQL that returns no rows.
//
// With no arguments the whole string is executed, so it may contain several
// statements separated by semicolons -- the shape a schema migration takes.
// With arguments it must be a single statement, which is prepared, bound, run
// to completion, and finalized.
func (c *Conn) Exec(sql string, args ...any) error {
	if err := c.check(); err != nil {
		return err
	}
	if len(args) == 0 {
		rc := Code(lib.exec(c.db, sql, 0, 0, 0))
		return c.err(rc, sql)
	}

	stmt, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	defer stmt.Close()
	if err := stmt.Bind(args...); err != nil {
		return err
	}
	for {
		row, err := stmt.Step()
		if err != nil {
			return err
		}
		if !row {
			return nil
		}
	}
}

// Prepare compiles a single SQL statement. The caller owns the result and must
// Close it. Text after the first statement is an error, so that a typo'd
// semicolon cannot silently drop half the query.
func (c *Conn) Prepare(sql string) (*Stmt, error) {
	return c.PrepareFlags(sql, 0)
}

// PrepareFlags is Prepare with SQLITE_PREPARE_* flags. Pass
// [PreparePersistent] for a statement that will be reused for the life of the
// connection, which is the normal case for a polling reader.
func (c *Conn) PrepareFlags(sql string, flags PrepareFlag) (*Stmt, error) {
	stmt, tail, err := c.prepare(sql, flags)
	if err != nil {
		return nil, err
	}
	if stmt == nil {
		return nil, &Error{Code: ErrorCode, Extended: ErrorCode, Message: "no SQL statement in text", SQL: sql}
	}
	if trimSQL(tail) != "" {
		stmt.Close()
		return nil, &Error{Code: ErrorCode, Extended: ErrorCode, Message: "multiple statements; use Exec or PrepareTail", SQL: sql}
	}
	return stmt, nil
}

// PrepareTail compiles the first statement in sql and returns the unconsumed
// remainder, so a caller can walk a multi-statement script one statement at a
// time. A nil Stmt with a nil error means the text held only whitespace or
// comments.
func (c *Conn) PrepareTail(sql string) (*Stmt, string, error) {
	return c.prepare(sql, 0)
}

func (c *Conn) prepare(sql string, flags PrepareFlag) (*Stmt, string, error) {
	if err := c.check(); err != nil {
		return nil, "", err
	}

	// NUL-terminate and pass the length including the terminator: SQLite
	// documents this as the form that avoids a scan for the end of the string.
	buf := make([]byte, len(sql)+1)
	copy(buf, sql)
	base := &buf[0]

	var stmt, tail uintptr
	rc := Code(lib.prepareV3(c.db, base, int32(len(buf)), uint32(flags), &stmt, &tail))
	if rc != OK {
		runtime.KeepAlive(buf)
		return nil, "", c.err(rc, sql)
	}

	rest := ""
	if tail != 0 {
		if off := int(tail - uintptr(unsafe.Pointer(base))); off > 0 && off <= len(sql) {
			rest = sql[off:]
		}
	}
	runtime.KeepAlive(buf)

	if stmt == 0 {
		return nil, rest, nil
	}
	s := &Stmt{conn: c, stmt: stmt, sql: sql[:len(sql)-len(rest)]}
	c.stmts[s] = struct{}{}
	return s, rest, nil
}

// BusyTimeout makes a blocked connection retry for up to d before returning
// [Busy]. Zero disables the wait and fails immediately.
//
// The shipped library is compiled with HAVE_USLEEP, so sub-second timeouts are
// honoured at their stated resolution rather than rounded up to whole seconds.
func (c *Conn) BusyTimeout(d time.Duration) error {
	if err := c.check(); err != nil {
		return err
	}
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	rc := Code(lib.busyTimeout(c.db, int32(ms)))
	return c.err(rc, "busy_timeout")
}

// LastInsertRowID is the rowid of the most recent successful insert on this
// connection.
func (c *Conn) LastInsertRowID() int64 {
	if c.check() != nil {
		return 0
	}
	return lib.lastInsertRowID(c.db)
}

// Changes counts the rows modified by the most recently completed statement.
func (c *Conn) Changes() int64 {
	if c.check() != nil {
		return 0
	}
	return lib.changes64(c.db)
}

// TotalChanges counts the rows modified since the connection was opened.
func (c *Conn) TotalChanges() int64 {
	if c.check() != nil {
		return 0
	}
	return lib.totalChanges64(c.db)
}

// InTransaction reports whether an explicit transaction is open, which is the
// inverse of SQLite's autocommit flag.
func (c *Conn) InTransaction() bool {
	if c.check() != nil {
		return false
	}
	return lib.getAutocommit(c.db) == 0
}

// Interrupt aborts any query running on this connection. It is the one method
// safe to call from another goroutine while the connection is in use, which is
// what makes it useful for cancellation.
func (c *Conn) Interrupt() {
	if c.check() == nil {
		lib.interrupt(c.db)
	}
}

// Filename is the full path SQLite resolved for the named attached database,
// "main" being the one opened by [Open].
func (c *Conn) Filename(dbName string) string {
	if c.check() != nil {
		return ""
	}
	if dbName == "" {
		dbName = "main"
	}
	return lib.dbFilename(c.db, dbName)
}

// WALCheckpoint runs a write-ahead-log checkpoint on the named database and
// reports how large the WAL was, in frames, and how many of those frames were
// moved into the database file. An empty dbName means "main".
//
// Two behaviours surprise people, both of them SQLite's:
//
//   - [CheckpointRestart] and [CheckpointTruncate] report zero for both
//     counts on success, because SQLite reads the counters out of the WAL
//     header after the reset that those modes perform.
//   - A busy return is not an error in the usual sense: a checkpoint that
//     could not move everything reports [Busy] with the counts still filled
//     in, describing how far it got.
func (c *Conn) WALCheckpoint(dbName string, mode CheckpointMode) (walPages, checkpointed int, err error) {
	if err := c.check(); err != nil {
		return 0, 0, err
	}
	if dbName == "" {
		dbName = "main"
	}
	var nLog, nCkpt int32
	rc := Code(lib.walCheckpointV2(c.db, dbName, int32(mode), &nLog, &nCkpt))
	return int(nLog), int(nCkpt), c.err(rc, "wal_checkpoint")
}

// WALAutoCheckpoint sets how many WAL pages may accumulate before SQLite
// checkpoints on its own. Zero disables automatic checkpointing, leaving it to
// the caller -- which is what a process that wants checkpoints off the write
// path will do.
func (c *Conn) WALAutoCheckpoint(pages int) error {
	if err := c.check(); err != nil {
		return err
	}
	rc := Code(lib.walAutocheckpoint(c.db, int32(pages)))
	return c.err(rc, "wal_autocheckpoint")
}

func trimSQL(s string) string {
	for len(s) > 0 {
		switch s[0] {
		case ' ', '\t', '\n', '\r', '\v', '\f', ';':
			s = s[1:]
		default:
			return s
		}
	}
	return s
}
