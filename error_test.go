package sqlite3_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite3 "github.com/pjstein/purego-sqlite"
)

func TestSyntaxErrorCarriesSQLiteMessage(t *testing.T) {
	c := open(t)
	_, err := c.Prepare(`SELCT 1`)
	if err == nil {
		t.Fatal("Prepare accepted invalid SQL")
	}
	var e *sqlite3.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T, want *sqlite3.Error", err)
	}
	if e.Code != sqlite3.ErrorCode {
		t.Errorf("Code = %v, want ErrorCode", e.Code)
	}
	if !strings.Contains(e.Message, "syntax error") {
		t.Errorf("Message = %q, want SQLite's own syntax error text", e.Message)
	}
	if !strings.Contains(e.SQL, "SELCT") {
		t.Errorf("SQL = %q, want the offending statement", e.SQL)
	}
}

// TestConstraintErrorsCarryExtendedCodes is why OpenExResCode is on by
// default: "constraint failed" is not actionable, "UNIQUE constraint failed"
// is.
func TestConstraintErrorsCarryExtendedCodes(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE u (a TEXT UNIQUE, b TEXT NOT NULL, c INTEGER CHECK (c > 0))`)
	mustExec(t, c, `INSERT INTO u VALUES ('one', 'b', 1)`)

	cases := []struct {
		name     string
		sql      string
		args     []any
		extended sqlite3.Code
	}{
		{"unique", `INSERT INTO u VALUES (?, ?, ?)`, []any{"one", "b", 1}, sqlite3.ConstraintUnique},
		{"not-null", `INSERT INTO u VALUES (?, ?, ?)`, []any{"two", nil, 1}, sqlite3.ConstraintNotNull},
		{"check", `INSERT INTO u VALUES (?, ?, ?)`, []any{"three", "b", 0}, sqlite3.ConstraintCheck},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Exec(tc.sql, tc.args...)
			if err == nil {
				t.Fatal("the constraint did not fire")
			}
			if !errors.Is(err, sqlite3.Constraint) {
				t.Errorf("errors.Is(err, Constraint) is false for %v", err)
			}
			if !errors.Is(err, tc.extended) {
				t.Errorf("errors.Is(err, %v) is false for %v", tc.extended, err)
			}
			// An unrelated code must not match, or matching means nothing.
			if errors.Is(err, sqlite3.Busy) {
				t.Errorf("a constraint error matched Busy: %v", err)
			}
			if errors.Is(err, sqlite3.ConstraintForeignKey) && tc.extended != sqlite3.ConstraintForeignKey {
				t.Errorf("a %s error matched ConstraintForeignKey: %v", tc.name, err)
			}
		})
	}
}

func TestOpenErrors(t *testing.T) {
	t.Run("read-only-missing-file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist.db")
		c, err := sqlite3.OpenFlags(path, sqlite3.OpenReadOnly|sqlite3.OpenExResCode, "")
		if err == nil {
			c.Close()
			t.Fatal("opening a missing file read-only succeeded")
		}
		if !errors.Is(err, sqlite3.CantOpen) {
			t.Errorf("got %v, want CantOpen", err)
		}
	})

	t.Run("unknown-vfs", func(t *testing.T) {
		c, err := sqlite3.OpenFlags(tempDB(t), sqlite3.OpenDefault, "no-such-vfs")
		if err == nil {
			c.Close()
			t.Fatal("opening with an unknown VFS succeeded")
		}
	})

	t.Run("not-a-database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage.db")
		if err := writeFile(path, strings.Repeat("this is not a database\n", 100)); err != nil {
			t.Fatal(err)
		}
		c, err := sqlite3.Open(path)
		if err != nil {
			t.Skipf("open deferred the error, which is also valid: %v", err)
		}
		defer c.Close()
		err = c.Exec(`SELECT count(*) FROM sqlite_master`)
		if err == nil {
			t.Fatal("querying a non-database succeeded")
		}
		if !errors.Is(err, sqlite3.NotADB) {
			t.Errorf("got %v, want NotADB", err)
		}
	})
}

func TestWriteToReadOnlyConnection(t *testing.T) {
	path := tempDB(t)
	w := open(t)
	_ = w
	setup, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, setup, `CREATE TABLE ro (n INTEGER)`)
	setup.Close()

	c, err := sqlite3.OpenFlags(path, sqlite3.OpenReadOnly|sqlite3.OpenExResCode, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Exec(`INSERT INTO ro VALUES (1)`)
	if err == nil {
		t.Fatal("wrote through a read-only connection")
	}
	if !errors.Is(err, sqlite3.ReadOnly) {
		t.Errorf("got %v, want ReadOnly", err)
	}
}

func TestUseAfterClose(t *testing.T) {
	c, err := sqlite3.Open(tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, `CREATE TABLE t (n INTEGER)`)
	st, err := c.Prepare(`SELECT n FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.Closed() {
		t.Error("Closed() is false after Close()")
	}

	if err := c.Exec(`SELECT 1`); err == nil {
		t.Error("Exec on a closed connection succeeded")
	}
	if _, err := c.Prepare(`SELECT 1`); err == nil {
		t.Error("Prepare on a closed connection succeeded")
	}
	if _, err := st.Step(); err == nil {
		t.Error("Step on a statement of a closed connection succeeded")
	}
	// Both must be idempotent rather than crash.
	if err := st.Close(); err != nil {
		t.Errorf("second Stmt.Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Conn.Close: %v", err)
	}
}

// TestCloseFinalizesOutstandingStatements checks the reason Conn.Close does
// not simply call sqlite3_close_v2: with statements outstanding that call
// reports success while leaving the connection alive, holding its file locks.
// A connection reported closed must have released the database.
func TestCloseFinalizesOutstandingStatements(t *testing.T) {
	path := tempDB(t)
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, `CREATE TABLE t (n INTEGER)`)
	mustExec(t, c, `INSERT INTO t VALUES (1)`)

	// Leave a statement mid-execution: it holds a read transaction open.
	st, err := c.Prepare(`SELECT n FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	if row, err := st.Step(); err != nil || !row {
		t.Fatalf("Step: row=%v err=%v", row, err)
	}
	if !st.Busy() {
		t.Error("statement should report Busy mid-execution")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// If the connection were a zombie it would still hold locks, and an
	// exclusive-locking connection could not take the database.
	other, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if err := other.Exec(`INSERT INTO t VALUES (2)`); err != nil {
		t.Fatalf("the closed connection was still holding the database: %v", err)
	}
}

func TestErrorCodeFormatting(t *testing.T) {
	if got := sqlite3.Busy.Error(); got != "database is locked" {
		t.Errorf("Busy.Error() = %q", got)
	}
	if !sqlite3.ConstraintUnique.IsExtended() {
		t.Error("ConstraintUnique should report IsExtended")
	}
	if sqlite3.Constraint.IsExtended() {
		t.Error("Constraint is a primary code and should not report IsExtended")
	}
	if got := sqlite3.ConstraintUnique.Primary(); got != sqlite3.Constraint {
		t.Errorf("ConstraintUnique.Primary() = %v, want Constraint", got)
	}
	if got := sqlite3.Row.Primary(); got != sqlite3.Row {
		t.Errorf("Row.Primary() = %v, want Row", got)
	}
	e := &sqlite3.Error{Code: sqlite3.Constraint, Extended: sqlite3.ConstraintUnique, Message: "UNIQUE constraint failed: u.a", SQL: "INSERT INTO u VALUES (?)"}
	s := e.Error()
	for _, want := range []string{"constraint failed", "UNIQUE constraint failed", "INSERT INTO u"} {
		if !strings.Contains(s, want) {
			t.Errorf("Error() = %q, missing %q", s, want)
		}
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
