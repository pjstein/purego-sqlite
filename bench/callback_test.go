package bakeoff_test

import (
	"path/filepath"
	"testing"

	ncruces "github.com/ncruces/go-sqlite3"
	sqlite3 "github.com/pjstein/purego-sqlite"
)

// BenchmarkSQLFunction measures what it costs to answer a SQL function call in
// Go rather than in C.
//
// The baseline is SQLite's own abs(), doing comparable work entirely inside
// the library. The difference between it and an identical Go implementation is
// the callback tax: one C-to-Go crossing per invocation, which is the mirror
// image of the Go-to-C tax the FFI benchmarks measure.
//
// Each iteration runs one query over a 1000-row table, so the per-call figure
// is the reported ns/op divided by 1000, reported directly as ns/call.
const fnRows = 1000

func setupFnDB(tb testing.TB) *sqlite3.Conn {
	tb.Helper()
	c, err := sqlite3.Open(filepath.Join(tb.TempDir(), "fn.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { c.Close() })
	if err := c.Exec(`CREATE TABLE n (v INTEGER)`); err != nil {
		tb.Fatal(err)
	}
	if err := c.Exec(`BEGIN`); err != nil {
		tb.Fatal(err)
	}
	ins, err := c.Prepare(`INSERT INTO n VALUES (?)`)
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < fnRows; i++ {
		if err := ins.BindInt64(1, int64(-i)); err != nil {
			tb.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			tb.Fatal(err)
		}
	}
	ins.Close()
	if err := c.Exec(`COMMIT`); err != nil {
		tb.Fatal(err)
	}
	return c
}

func runAgg(tb testing.TB, c *sqlite3.Conn, sql string) {
	st, err := c.Prepare(sql)
	if err != nil {
		tb.Fatal(err)
	}
	defer st.Close()
	b, _ := tb.(*testing.B)
	calls := 0
	for b.Loop() {
		row, err := st.Step()
		if err != nil {
			tb.Fatal(err)
		}
		if !row {
			tb.Fatal("no row")
		}
		if err := st.Reset(); err != nil {
			tb.Fatal(err)
		}
		calls++
	}
	if calls == 0 {
		tb.Fatal("benchmark ran zero iterations")
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(calls*fnRows), "ns/call")
}

func BenchmarkSQLFunction(b *testing.B) {
	// Which implementation this process measures is stated, not assumed: the
	// scalar path is chosen at library load, so a benchmark that stayed silent
	// would be indistinguishable from one measuring the other one.
	fast, err := sqlite3.UseMarshaledFunctions()
	if err != nil {
		b.Fatal(err)
	}

	goAbs := func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		v := a[0].Int64()
		if v < 0 {
			v = -v
		}
		ctx.ResultInt64(v)
	}

	b.Run("C: builtin abs", func(b *testing.B) {
		c := setupFnDB(b)
		runAgg(b, c, `SELECT sum(abs(v)) FROM n`)
	})

	if fast {
		// The default: C marshaling plus slice-index dispatch. The map-under-
		// a-lock variant this replaced was measured in the same harness and
		// then removed; its numbers are preserved in
		// results/micro-callback-ladder-with-map-dispatch.txt.
		b.Run("scalar: marshaled (default)", func(b *testing.B) {
			c := setupFnDB(b)
			if err := c.CreateFunction("go_abs", 1, sqlite3.Deterministic, goAbs); err != nil {
				b.Fatal(err)
			}
			runAgg(b, c, `SELECT sum(go_abs(v)) FROM n`)
		})
	} else {
		// Rung 0: the portable path, one crossing per argument and result.
		b.Run("scalar: portable (per-arg crossings)", func(b *testing.B) {
			c := setupFnDB(b)
			if err := c.CreateFunction("go_abs", 1, sqlite3.Deterministic, goAbs); err != nil {
				b.Fatal(err)
			}
			runAgg(b, c, `SELECT sum(go_abs(v)) FROM n`)
		})
	}

	// Rung 3, and the combination question the owner raised: the aggregate
	// path has its own C trampoline and its own dispatch table, so whether the
	// scalar path underneath it is marshaled or portable should not matter.
	// Measured in both processes rather than reasoned about.
	aggLabel := "aggregate: batched (scalar path portable)"
	if fast {
		aggLabel = "aggregate: batched (scalar path marshaled)"
	}
	b.Run(aggLabel, func(b *testing.B) {
		c := setupFnDB(b)
		if err := c.CreateAggregate("go_sum_abs", 1, sqlite3.Deterministic,
			func(ctx *sqlite3.FuncContext, a sqlite3.Accumulation) {
				ctx.ResultFloat(-a.Sum)
			}); err != nil {
			b.Skipf("aggregate unavailable: %v", err)
		}
		runAgg(b, c, `SELECT go_sum_abs(v) FROM n`)
	})

	// ncruces for scale: its functions are Go calling Go, with no boundary to
	// cross at all, which is the fairest thing to compare a callback tax to.
	b.Run("ncruces: Go function (no boundary)", func(b *testing.B) {
		nc, err := ncruces.Open(filepath.Join(b.TempDir(), "fn.db"))
		if err != nil {
			b.Fatal(err)
		}
		defer nc.Close()
		if err := nc.Exec(`CREATE TABLE n (v INTEGER)`); err != nil {
			b.Fatal(err)
		}
		if err := nc.Exec(`BEGIN`); err != nil {
			b.Fatal(err)
		}
		st, _, err := nc.Prepare(`INSERT INTO n VALUES (?)`)
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < fnRows; i++ {
			if err := st.BindInt64(1, int64(-i)); err != nil {
				b.Fatal(err)
			}
			if err := st.Exec(); err != nil {
				b.Fatal(err)
			}
		}
		st.Close()
		if err := nc.Exec(`COMMIT`); err != nil {
			b.Fatal(err)
		}
		if err := nc.CreateFunction("go_abs", 1, ncruces.DETERMINISTIC, func(ctx ncruces.Context, arg ...ncruces.Value) {
			v := arg[0].Int64()
			if v < 0 {
				v = -v
			}
			ctx.ResultInt64(v)
		}); err != nil {
			b.Fatal(err)
		}
		q, _, err := nc.Prepare(`SELECT sum(go_abs(v)) FROM n`)
		if err != nil {
			b.Fatal(err)
		}
		defer q.Close()
		calls := 0
		for b.Loop() {
			if !q.Step() {
				b.Fatal(q.Err())
			}
			if err := q.Reset(); err != nil {
				b.Fatal(err)
			}
			calls++
		}
		if calls == 0 {
			b.Fatal("measured nothing")
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(calls*fnRows), "ns/call")
	})
}
