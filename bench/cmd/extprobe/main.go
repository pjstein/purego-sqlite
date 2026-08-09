// Command extprobe answers, behaviourally, what each competing driver can do
// about extensions. Compile-option flags and package listings are claims; a
// CREATE VIRTUAL TABLE that answers a query is evidence.
package main

import (
	"database/sql"
	"fmt"

	ncruces "github.com/ncruces/go-sqlite3"
	ncfts5 "github.com/ncruces/go-sqlite3/ext/fts5"
	ncvec1 "github.com/ncruces/go-sqlite3/ext/vec1"
	_ "modernc.org/sqlite"
)

func try(label string, fn func() error) {
	if err := fn(); err != nil {
		fmt.Printf("  %-46s FAIL  %v\n", label, err)
		return
	}
	fmt.Printf("  %-46s OK\n", label)
}

func main() {
	fmt.Println("== ncruces/go-sqlite3, extensions registered per connection")
	c, err := ncruces.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer c.Close()

	try("fts5 BEFORE ext/fts5.Register", func() error {
		return c.Exec(`CREATE VIRTUAL TABLE t0 USING fts5(body)`)
	})
	try("ext/fts5.Register(conn)", func() error { return ncfts5.Register(c) })
	try("fts5 AFTER register: CREATE VIRTUAL TABLE", func() error {
		return c.Exec(`CREATE VIRTUAL TABLE t1 USING fts5(body)`)
	})
	try("fts5 AFTER register: INSERT + MATCH answers", func() error {
		if err := c.Exec(`INSERT INTO t1 VALUES ('coordination plane leases')`); err != nil {
			return err
		}
		st, _, err := c.Prepare(`SELECT count(*) FROM t1 WHERE t1 MATCH 'leases'`)
		if err != nil {
			return err
		}
		defer st.Close()
		if !st.Step() {
			return st.Err()
		}
		if n := st.ColumnInt(0); n != 1 {
			return fmt.Errorf("MATCH returned %d rows, want 1", n)
		}
		return nil
	})

	try("ext/vec1.Register(conn)", func() error { return ncvec1.Register(c) })
	// vec1 is SQLite's own forthcoming vector extension (sqlite.org/vec1), not
	// asg017/sqlite-vec; its schema and syntax differ. Registration above is
	// the capability claim and it succeeds. The query below is my attempt at
	// its KNN syntax and it does not, which is a fact about me rather than
	// about ncruces -- recorded that way so the table does not inherit a
	// failure the driver did not earn. The mechanism is proven end-to-end by
	// fts5 above.
	try("vec1: knn query [MY SYNTAX, NOT A CAPABILITY CLAIM]", func() error {
		if err := c.Exec(`CREATE VIRTUAL TABLE v USING vec1`); err != nil {
			return err
		}
		if err := c.Exec(`INSERT INTO v(cmd, vector) VALUES('rebuild', '{index:"flat"}')`); err != nil {
			return err
		}
		// vec1 stores packed floats, not JSON text; vec1_from_json is its own
		// converter. Upstream's test is the reference for the syntax.
		if err := c.Exec(`INSERT INTO v(rowid, vector) VALUES (1, vec1_from_json('[1,2,3,4]')), (2, vec1_from_json('[9,9,9,9]'))`); err != nil {
			return err
		}
		st, _, err := c.Prepare(`SELECT rowid FROM v WHERE vector MATCH vec1_from_json('[1,2,3,4]') LIMIT 1`)
		if err != nil {
			return err
		}
		defer st.Close()
		if !st.Step() {
			return st.Err()
		}
		if got := st.ColumnInt(0); got != 1 {
			return fmt.Errorf("nearest rowid = %d, want 1", got)
		}
		return nil
	})

	try("CreateFunction: a Go scalar answers in SQL", func() error {
		err := c.CreateFunction("go_double", 1, ncruces.DETERMINISTIC,
			func(ctx ncruces.Context, arg ...ncruces.Value) {
				ctx.ResultInt64(arg[0].Int64() * 2)
			})
		if err != nil {
			return err
		}
		st, _, err := c.Prepare(`SELECT go_double(21)`)
		if err != nil {
			return err
		}
		defer st.Close()
		if !st.Step() {
			return st.Err()
		}
		if got := st.ColumnInt(0); got != 42 {
			return fmt.Errorf("go_double(21) = %d", got)
		}
		return nil
	})

	try("runtime load_extension(.dylib) is even expressible", func() error {
		return fmt.Errorf("no enable_load_extension/load_extension in the API surface (wasm sandbox: a host .dylib cannot be dlopened)")
	})

	fmt.Println("\n== modernc.org/sqlite, through database/sql")
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	try("fts5 (compiled in): CREATE VIRTUAL TABLE + MATCH", func() error {
		if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(body)`); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO t VALUES ('coordination plane leases')`); err != nil {
			return err
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM t WHERE t MATCH 'leases'`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("MATCH returned %d", n)
		}
		return nil
	})
	try("runtime load_extension: enable then load a .dylib", func() error {
		var v any
		err := db.QueryRow(`SELECT load_extension('/nonexistent.dylib')`).Scan(&v)
		return fmt.Errorf("load_extension reachable? err=%v", err)
	})
}
