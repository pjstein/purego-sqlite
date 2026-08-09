package sqlite3_test

import (
	"strings"
	"testing"

	sqlite3 "github.com/pjstein/purego-sqlite"
)

// TestCreateAggregate checks the batched aggregate against SQLite's own
// aggregates over the same data. The Go function runs once per group; the
// answers must still match what C would have produced.
func TestCreateAggregate(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE n (g TEXT, v)`)
	mustExec(t, c, `BEGIN`)
	ins, err := c.Prepare(`INSERT INTO n VALUES (?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	groups := []string{"a", "b"}
	for i := 0; i < 1000; i++ {
		if err := ins.Bind(groups[i%2], int64(i-500)); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}
	// NULLs must be ignored, as SQL aggregates do.
	for i := 0; i < 10; i++ {
		if err := ins.Bind("a", nil); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}
	ins.Close()
	mustExec(t, c, `COMMIT`)

	reg := func(name string, fn sqlite3.AggregateFunc) {
		t.Helper()
		if err := c.CreateAggregate(name, 1, 0, fn); err != nil {
			t.Fatal(err)
		}
	}
	reg("go_sum", func(ctx *sqlite3.FuncContext, a sqlite3.Accumulation) {
		if a.Count == 0 {
			ctx.ResultNull()
			return
		}
		if a.AllInt {
			ctx.ResultInt64(a.IntSum)
			return
		}
		ctx.ResultFloat(a.Sum)
	})
	reg("go_count", func(ctx *sqlite3.FuncContext, a sqlite3.Accumulation) { ctx.ResultInt64(a.Count) })
	reg("go_min", func(ctx *sqlite3.FuncContext, a sqlite3.Accumulation) { ctx.ResultFloat(a.Min) })
	reg("go_max", func(ctx *sqlite3.FuncContext, a sqlite3.Accumulation) { ctx.ResultFloat(a.Max) })
	reg("go_boom", func(ctx *sqlite3.FuncContext, a sqlite3.Accumulation) { panic("deliberate") })

	// Each Go aggregate must agree with SQLite's own over the same data.
	pairs := []struct{ mine, theirs string }{
		{`SELECT go_sum(v) FROM n`, `SELECT sum(v) FROM n`},
		{`SELECT go_count(v) FROM n`, `SELECT count(v) FROM n`},
		{`SELECT CAST(go_min(v) AS INTEGER) FROM n`, `SELECT min(v) FROM n`},
		{`SELECT CAST(go_max(v) AS INTEGER) FROM n`, `SELECT max(v) FROM n`},
	}
	for _, p := range pairs {
		var mine, theirs string
		if err := queryOne(c, p.mine, &mine); err != nil {
			t.Errorf("%s: %v", p.mine, err)
			continue
		}
		if err := queryOne(c, p.theirs, &theirs); err != nil {
			t.Fatal(err)
		}
		if mine != theirs {
			t.Errorf("%s = %q but %s = %q", p.mine, mine, p.theirs, theirs)
		}
	}

	// GROUP BY must call the Go function once per group with that group's data.
	st, err := c.Prepare(`SELECT g, go_sum(v), go_count(v) FROM n GROUP BY g ORDER BY g`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seen := 0
	for {
		row, err := st.Step()
		if err != nil {
			t.Fatal(err)
		}
		if !row {
			break
		}
		g := st.ColumnText(0)
		var wantSum, wantCount string
		q, err := c.Prepare(`SELECT sum(v), count(v) FROM n WHERE g = ?`)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Bind(g); err != nil {
			t.Fatal(err)
		}
		if ok, _ := q.Step(); ok {
			wantSum, wantCount = q.ColumnText(0), q.ColumnText(1)
		}
		q.Close()
		if got := st.ColumnText(1); got != wantSum {
			t.Errorf("group %s: go_sum = %s, sum = %s", g, got, wantSum)
		}
		if got := st.ColumnText(2); got != wantCount {
			t.Errorf("group %s: go_count = %s, count = %s", g, got, wantCount)
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("GROUP BY produced %d groups, want 2", seen)
	}

	// An empty input must reach the Go function with Count == 0 rather than
	// not reaching it at all.
	var empty string
	if err := queryOne(c, `SELECT quote(go_sum(v)) FROM n WHERE 0`, &empty); err != nil {
		t.Fatal(err)
	}
	if empty != "NULL" {
		t.Errorf("aggregate over no rows = %s, want NULL", empty)
	}

	// A panic in xFinal must become a SQL error, not a dead process.
	if err := c.Exec(`SELECT go_boom(v) FROM n`); err == nil {
		t.Error("a panicking aggregate did not fail the query")
	} else if !strings.Contains(err.Error(), "deliberate") {
		t.Errorf("panic value lost: %v", err)
	}
	var still string
	if err := queryOne(c, `SELECT 1+1`, &still); err != nil || still != "2" {
		t.Errorf("connection unusable after an aggregate panic: %v", err)
	}
}
