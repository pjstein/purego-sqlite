package sqlite3_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjstein/purego-sqlite/sqlite3"
)

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func open(t *testing.T) *sqlite3.Conn {
	t.Helper()
	c, err := sqlite3.Open(tempDB(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func mustExec(t *testing.T, c *sqlite3.Conn, sql string, args ...any) {
	t.Helper()
	if err := c.Exec(sql, args...); err != nil {
		t.Fatalf("Exec(%q): %v", sql, err)
	}
}

// TestShippedLibraryIsThePinnedUpstreamOne is the load-bearing check that this
// package binds the SQLite this repository compiled, and not whatever
// libsqlite3 the operating system happens to offer. The expected version is
// read from sqlite-version.lock so the pin and the binary cannot drift apart
// without a test failing.
func TestShippedLibraryIsThePinnedUpstreamOne(t *testing.T) {
	// The lock lives at the repository root, one level above this package — the build
	// scripts and the Makefile read it from there, and it must stay in exactly one place.
	lock, err := os.ReadFile(filepath.Join("..", "sqlite-version.lock"))
	if err != nil {
		t.Fatalf("reading sqlite-version.lock: %v", err)
	}
	var want string
	for _, line := range strings.Split(string(lock), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SQLITE_VERSION="); ok {
			want = v
			break
		}
	}
	if want == "" {
		t.Fatal("sqlite-version.lock has no SQLITE_VERSION line")
	}

	got, err := sqlite3.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != want {
		t.Errorf("loaded SQLite %s, but sqlite-version.lock pins %s", got, want)
	}

	src, err := sqlite3.SourceID()
	if err != nil {
		t.Fatalf("SourceID: %v", err)
	}
	if src == "" {
		t.Error("SourceID is empty; the library did not report an upstream check-in")
	}

	mode, err := sqlite3.ThreadSafeMode()
	if err != nil {
		t.Fatalf("ThreadSafeMode: %v", err)
	}
	if mode != 2 {
		t.Errorf("SQLITE_THREADSAFE=%d, want 2 (multi-thread); build/flags.sh and the shipped library disagree", mode)
	}

	t.Logf("SQLite %s (%s), threadsafe=%d, from %s", got, src, mode, sqlite3.LoadedLibraryPath())
}

// TestCompiledFeatures exercises each compile-time flag through behaviour
// rather than through sqlite_compileoption_used, so a flag that is present but
// non-functional still fails.
func TestCompiledFeatures(t *testing.T) {
	c := open(t)

	t.Run("FTS5", func(t *testing.T) {
		mustExec(t, c, `CREATE VIRTUAL TABLE docs USING fts5(body)`)
		mustExec(t, c, `INSERT INTO docs VALUES ('the coordination plane holds leases')`)
		st, err := c.Prepare(`SELECT count(*) FROM docs WHERE docs MATCH 'leases'`)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.Step(); err != nil {
			t.Fatal(err)
		}
		if n := st.ColumnInt(0); n != 1 {
			t.Errorf("FTS5 match returned %d rows, want 1", n)
		}
	})

	t.Run("JSON", func(t *testing.T) {
		var got string
		if err := queryOne(c, `SELECT json_extract('{"a":{"b":7}}', '$.a.b')`, &got); err != nil {
			t.Fatal(err)
		}
		if got != "7" {
			t.Errorf("json_extract = %q, want \"7\"", got)
		}
	})

	t.Run("MathFunctions", func(t *testing.T) {
		var got string
		if err := queryOne(c, `SELECT CAST(pow(2,10) AS INTEGER)`, &got); err != nil {
			t.Fatal(err)
		}
		if got != "1024" {
			t.Errorf("pow(2,10) = %q, want \"1024\"", got)
		}
	})

	// SQLITE_DQS=0: a double-quoted token is an identifier, always. Without it
	// SQLite falls back to treating an unresolvable one as a string literal,
	// which turns a misspelled column into a silent constant.
	t.Run("DoubleQuotedStringsRejected", func(t *testing.T) {
		mustExec(t, c, `CREATE TABLE dqs (a TEXT)`)
		err := c.Exec(`INSERT INTO dqs VALUES ("this is not a column")`)
		if err == nil {
			t.Fatal("double-quoted string literal was accepted; SQLITE_DQS=0 is not in effect")
		}
		if !strings.Contains(err.Error(), "no such column") {
			t.Errorf("unexpected error for double-quoted literal: %v", err)
		}
	})

	// STAT4 is a planner input, not a visible feature; the compile option list
	// is the only honest way to assert it.
	t.Run("STAT4", func(t *testing.T) {
		var got string
		if err := queryOne(c, `SELECT sqlite_compileoption_used('SQLITE_ENABLE_STAT4')`, &got); err != nil {
			t.Fatal(err)
		}
		if got != "1" {
			t.Error("SQLITE_ENABLE_STAT4 is not compiled in")
		}
	})

	// SQLITE_DEFAULT_WAL_SYNCHRONOUS applies when a connection opens a
	// database that is *already* in WAL mode: SQLite reads it off page 1 of
	// the file header. The connection that converts a rollback-journal
	// database to WAL keeps the synchronous level it started with, so the
	// check has to close and reopen or it measures nothing.
	t.Run("WALSynchronousDefault", func(t *testing.T) {
		path := tempDB(t)
		w, err := sqlite3.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, w, `PRAGMA journal_mode=WAL`)
		mustExec(t, w, `CREATE TABLE anything (x)`) // force the header to disk
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		r, err := sqlite3.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var mode, got string
		if err := queryOne(r, `PRAGMA journal_mode`, &mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Fatalf("reopened database is in %s mode, not wal", mode)
		}
		if err := queryOne(r, `SELECT * FROM pragma_synchronous`, &got); err != nil {
			t.Fatal(err)
		}
		if got != "1" {
			t.Errorf("synchronous = %s on a connection opening a WAL database, want 1 (NORMAL) from SQLITE_DEFAULT_WAL_SYNCHRONOUS", got)
		}
	})
}

// queryOne runs a single-column single-row query and writes the result as text.
func queryOne(c *sqlite3.Conn, sql string, out *string) error {
	st, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	defer st.Close()
	row, err := st.Step()
	if err != nil {
		return err
	}
	if !row {
		return errors.New("query returned no rows")
	}
	*out = st.ColumnText(0)
	return nil
}

func TestInsertQueryRoundTrip(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, score REAL, data BLOB, missing TEXT)`)

	ins, err := c.Prepare(`INSERT INTO t (name, score, data, missing) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()

	if n := ins.BindCount(); n != 4 {
		t.Fatalf("BindCount = %d, want 4", n)
	}

	rows := []struct {
		name  string
		score float64
		data  []byte
	}{
		{"alpha", 1.5, []byte{0x00, 0xff, 0x10}},
		{"beta", -0.25, []byte{}},
		{"gamma", 0, nil},
	}
	for _, r := range rows {
		if err := ins.Bind(r.name, r.score, r.data, nil); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Changes(); got != 1 {
		t.Errorf("Changes after last insert = %d, want 1", got)
	}
	if got := c.LastInsertRowID(); got != 3 {
		t.Errorf("LastInsertRowID = %d, want 3", got)
	}
	if got := c.TotalChanges(); got != 3 {
		t.Errorf("TotalChanges = %d, want 3", got)
	}

	sel, err := c.Prepare(`SELECT id, name, score, data, missing FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer sel.Close()

	if n := sel.ColumnCount(); n != 5 {
		t.Fatalf("ColumnCount = %d, want 5", n)
	}

	i := 0
	for {
		row, err := sel.Step()
		if err != nil {
			t.Fatal(err)
		}
		if !row {
			break
		}
		if i >= len(rows) {
			t.Fatalf("more rows than inserted")
		}
		want := rows[i]
		if got := sel.ColumnInt64(0); got != int64(i+1) {
			t.Errorf("row %d id = %d, want %d", i, got, i+1)
		}
		if got := sel.ColumnText(1); got != want.name {
			t.Errorf("row %d name = %q, want %q", i, got, want.name)
		}
		if got := sel.ColumnFloat(2); got != want.score {
			t.Errorf("row %d score = %v, want %v", i, got, want.score)
		}
		got := sel.ColumnBlob(3)
		switch {
		case want.data == nil:
			if got != nil {
				t.Errorf("row %d data = %v, want nil for a NULL blob", i, got)
			}
			if !sel.ColumnIsNull(3) {
				t.Errorf("row %d data should read as NULL", i)
			}
		default:
			if string(got) != string(want.data) {
				t.Errorf("row %d data = %v, want %v", i, got, want.data)
			}
			if got == nil {
				t.Errorf("row %d: a zero-length blob must not read back as nil", i)
			}
		}
		if !sel.ColumnIsNull(4) {
			t.Errorf("row %d missing should be NULL", i)
		}
		i++
	}
	if i != len(rows) {
		t.Errorf("read %d rows, inserted %d", i, len(rows))
	}
}

func TestColumnMetadataAndTypes(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE m (i INTEGER, f REAL, s TEXT, b BLOB, n TEXT)`)
	mustExec(t, c, `INSERT INTO m VALUES (?, ?, ?, ?, ?)`, int64(-9223372036854775808), 3.5, "text", []byte{1, 2}, nil)

	st, err := c.Prepare(`SELECT i, f, s, b, n AS renamed FROM m`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.ColumnName(4); got != "renamed" {
		t.Errorf("ColumnName(4) = %q, want %q", got, "renamed")
	}
	if got := st.ColumnDeclType(0); got != "INTEGER" {
		t.Errorf("ColumnDeclType(0) = %q, want INTEGER", got)
	}
	if !st.ReadOnly() {
		t.Error("SELECT should report ReadOnly")
	}

	row, err := st.Step()
	if err != nil || !row {
		t.Fatalf("Step: row=%v err=%v", row, err)
	}
	if got := st.DataCount(); got != 5 {
		t.Errorf("DataCount = %d, want 5", got)
	}
	wantTypes := []sqlite3.ColumnType{sqlite3.TypeInteger, sqlite3.TypeFloat, sqlite3.TypeText, sqlite3.TypeBlob, sqlite3.TypeNull}
	for i, want := range wantTypes {
		if got := st.ColumnType(i); got != want {
			t.Errorf("ColumnType(%d) = %v, want %v", i, got, want)
		}
	}
	if got := st.ColumnInt64(0); got != -9223372036854775808 {
		t.Errorf("int64 min did not survive: %d", got)
	}

	// Out-of-range access is undefined in the C API; this package defines it.
	if got := st.ColumnText(99); got != "" {
		t.Errorf("ColumnText(99) = %q, want \"\"", got)
	}
	if got := st.ColumnInt64(-1); got != 0 {
		t.Errorf("ColumnInt64(-1) = %d, want 0", got)
	}
}

func TestNamedParameters(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE p (a TEXT, b TEXT)`)

	st, err := c.Prepare(`INSERT INTO p VALUES (:first, @second)`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := st.BindIndex(":first")
	second := st.BindIndex("@second")
	if first != 1 || second != 2 {
		t.Fatalf("BindIndex(:first)=%d BindIndex(@second)=%d, want 1 and 2", first, second)
	}
	if got := st.BindIndex(":nope"); got != 0 {
		t.Errorf("BindIndex of an absent parameter = %d, want 0", got)
	}
	if got := st.BindName(1); got != ":first" {
		t.Errorf("BindName(1) = %q, want \":first\"", got)
	}

	if err := st.BindText(first, "one"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindText(second, "two"); err != nil {
		t.Fatal(err)
	}
	if err := st.Run(); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := queryOne(c, `SELECT a || '/' || b FROM p`, &got); err != nil {
		t.Fatal(err)
	}
	if got != "one/two" {
		t.Errorf("round trip = %q, want \"one/two\"", got)
	}
}

func TestClearBindingsAndReset(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE cb (v TEXT)`)

	st, err := c.Prepare(`INSERT INTO cb VALUES (?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.BindText(1, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := st.Run(); err != nil {
		t.Fatal(err)
	}
	// Bindings survive Reset, so this inserts "kept" a second time.
	if err := st.Run(); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearBindings(); err != nil {
		t.Fatal(err)
	}
	if err := st.Run(); err != nil {
		t.Fatal(err)
	}

	var kept, nulls string
	if err := queryOne(c, `SELECT count(*) FROM cb WHERE v = 'kept'`, &kept); err != nil {
		t.Fatal(err)
	}
	if err := queryOne(c, `SELECT count(*) FROM cb WHERE v IS NULL`, &nulls); err != nil {
		t.Fatal(err)
	}
	if kept != "2" || nulls != "1" {
		t.Errorf("after two Runs, a ClearBindings and a third Run: %s kept rows and %s NULL rows, want 2 and 1", kept, nulls)
	}
}

func TestPrepareTailWalksAScript(t *testing.T) {
	c := open(t)
	script := `CREATE TABLE s (n INTEGER); INSERT INTO s VALUES (1);
	           -- a comment between statements
	           INSERT INTO s VALUES (2);`

	rest := script
	count := 0
	for {
		st, tail, err := c.PrepareTail(rest)
		if err != nil {
			t.Fatalf("PrepareTail: %v", err)
		}
		if st == nil {
			break
		}
		count++
		if err := st.Run(); err != nil {
			t.Fatal(err)
		}
		st.Close()
		rest = tail
		if strings.TrimSpace(rest) == "" {
			break
		}
	}
	if count != 3 {
		t.Errorf("walked %d statements, want 3", count)
	}

	var sum string
	if err := queryOne(c, `SELECT sum(n) FROM s`, &sum); err != nil {
		t.Fatal(err)
	}
	if sum != "3" {
		t.Errorf("sum = %s, want 3", sum)
	}
}

func TestPrepareRejectsMultipleStatements(t *testing.T) {
	c := open(t)
	if _, err := c.Prepare(`SELECT 1; SELECT 2`); err == nil {
		t.Fatal("Prepare accepted two statements; the second would have been silently dropped")
	}
	// Exec, by contrast, is the multi-statement door.
	mustExec(t, c, `CREATE TABLE a (x); CREATE TABLE b (y);`)
	var n string
	if err := queryOne(c, `SELECT count(*) FROM sqlite_master WHERE type='table'`, &n); err != nil {
		t.Fatal(err)
	}
	if n != "2" {
		t.Errorf("Exec created %s tables, want 2", n)
	}
}

func TestExpandedSQL(t *testing.T) {
	c := open(t)
	st, err := c.Prepare(`SELECT ?, ?`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Bind(42, "hi"); err != nil {
		t.Fatal(err)
	}
	got := st.ExpandedSQL()
	if !strings.Contains(got, "42") || !strings.Contains(got, "'hi'") {
		t.Errorf("ExpandedSQL = %q, want the bound values substituted", got)
	}
}

func TestTransactionState(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE tx (n INTEGER)`)
	if c.InTransaction() {
		t.Error("InTransaction before BEGIN")
	}
	mustExec(t, c, `BEGIN`)
	if !c.InTransaction() {
		t.Error("not InTransaction after BEGIN")
	}
	mustExec(t, c, `INSERT INTO tx VALUES (1)`)
	mustExec(t, c, `ROLLBACK`)
	if c.InTransaction() {
		t.Error("InTransaction after ROLLBACK")
	}
	var n string
	if err := queryOne(c, `SELECT count(*) FROM tx`, &n); err != nil {
		t.Fatal(err)
	}
	if n != "0" {
		t.Errorf("rolled-back insert left %s rows", n)
	}
}
