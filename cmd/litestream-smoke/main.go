// Command litestream-smoke is the write and verify halves of the litestream
// compatibility check.
//
//	litestream-smoke write <db> <rows>   -- append rows through this binding
//	litestream-smoke verify <db>         -- integrity, counts, corpus fidelity
//
// bench/run-litestream-smoke.sh drives it around a real litestream replicate
// and restore. The point is not that litestream works -- it is that a database
// this binding wrote, with WAL frames this binding produced, replicates and
// restores with the convicted corpus intact.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"

	"github.com/pjstein/purego-sqlite/conformance"
	"github.com/pjstein/purego-sqlite/sqlite3"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: litestream-smoke write <db> <rows> | verify <db>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "write":
		n, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fatal(err)
		}
		write(os.Args[2], n)
	case "verify":
		verify(os.Args[2])
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "litestream-smoke:", err)
	os.Exit(1)
}

func open(path string) *sqlite3.Conn {
	c, err := sqlite3.Open(path)
	if err != nil {
		fatal(err)
	}
	for _, p := range []string{
		`PRAGMA busy_timeout=10000`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
	} {
		if err := c.Exec(p); err != nil {
			fatal(err)
		}
	}
	return c
}

func write(path string, n int) {
	c := open(path)
	defer c.Close()
	if err := c.Exec(`CREATE TABLE IF NOT EXISTS events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, t TEXT, b BLOB)`); err != nil {
		fatal(err)
	}
	ins, err := c.Prepare(`INSERT INTO events (name, t, b) VALUES (?, ?, ?)`)
	if err != nil {
		fatal(err)
	}
	defer ins.Close()

	// The corpus rather than round numbers, so the restored copy is checked on
	// the values that actually break implementations.
	vals := conformance.Values()
	for i := 0; i < n; i++ {
		v := vals[i%len(vals)]
		if err := ins.Bind(v.Name, v.Text, v.Blob); err != nil {
			fatal(err)
		}
		if err := ins.Run(); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("wrote %d rows (corpus cycled %d times)\n", n, n/len(vals))
}

func one(c *sqlite3.Conn, sql string) string {
	st, err := c.Prepare(sql)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer st.Close()
	ok, err := st.Step()
	if err != nil {
		return "ERROR: " + err.Error()
	}
	if !ok {
		return "(no rows)"
	}
	return st.ColumnText(0)
}

func verify(path string) {
	c := open(path)
	defer c.Close()

	integrity := one(c, `PRAGMA integrity_check`)
	rows := one(c, `SELECT count(*) FROM events`)
	blobBytes := one(c, `SELECT coalesce(sum(length(b)),0) FROM events`)
	fmt.Printf("  integrity_check   %s\n", integrity)
	fmt.Printf("  rows              %s\n", rows)
	fmt.Printf("  total blob bytes  %s\n", blobBytes)

	st, err := c.Prepare(`SELECT name, t, b FROM events GROUP BY name`)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	want := map[string]conformance.Value{}
	for _, v := range conformance.Values() {
		want[v.Name] = v
	}
	checked, bad := 0, 0
	for {
		row, err := st.Step()
		if err != nil {
			fatal(err)
		}
		if !row {
			break
		}
		v, known := want[st.ColumnText(0)]
		if !known {
			continue
		}
		if st.ColumnText(1) != v.Text || !bytes.Equal(st.ColumnBlob(2), v.Blob) {
			fmt.Printf("  CORRUPTED %s\n", v.Name)
			bad++
		}
		checked++
	}
	fmt.Printf("  corpus values     %d checked, %d corrupted\n", checked, bad)

	if integrity != "ok" || bad > 0 || checked == 0 {
		fmt.Println("  RESULT            FAIL")
		os.Exit(1)
	}
	fmt.Println("  RESULT            ok")
}
