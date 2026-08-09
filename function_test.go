package sqlite3_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	sqlite3 "github.com/pjstein/purego-sqlite"
	"github.com/pjstein/purego-sqlite/conformance"
)

func TestCreateFunction(t *testing.T) {
	c := open(t)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(c.CreateFunction("go_double", 1, sqlite3.Deterministic, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultInt64(a[0].Int64() * 2)
	}))
	must(c.CreateFunction("go_upper", 1, sqlite3.Deterministic, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultText(strings.ToUpper(a[0].Text()))
	}))
	must(c.CreateFunction("go_argc", -1, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultInt64(int64(len(a)))
	}))
	must(c.CreateFunction("go_typeof", 1, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultText(a[0].Type().String())
	}))
	must(c.CreateFunction("go_echo_blob", 1, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultBlob(a[0].Blob())
	}))
	must(c.CreateFunction("go_halve", 1, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultFloat(a[0].Float() / 2)
	}))
	must(c.CreateFunction("go_nothing", 0, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {}))
	must(c.CreateFunction("go_fail", 0, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultError("deliberate failure")
	}))

	cases := []struct{ sql, want string }{
		{`SELECT go_double(21)`, "42"},
		{`SELECT go_upper('hello')`, "HELLO"},
		{`SELECT go_argc(1,2,3,4,5)`, "5"},
		{`SELECT go_argc()`, "0"},
		{`SELECT go_typeof(1)`, "INTEGER"},
		{`SELECT go_typeof('s')`, "TEXT"},
		{`SELECT go_typeof(x'00')`, "BLOB"},
		{`SELECT go_typeof(NULL)`, "NULL"},
		{`SELECT go_halve(5.0)`, "2.5"},
		{`SELECT quote(go_nothing())`, "NULL"},
		{`SELECT go_double(x) FROM (SELECT 5 AS x)`, "10"},
	}
	for _, tc := range cases {
		var got string
		if err := queryOne(c, tc.sql, &got); err != nil {
			t.Errorf("%s: %v", tc.sql, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.sql, got, tc.want)
		}
	}

	if err := c.Exec(`SELECT go_fail()`); err == nil {
		t.Error("ResultError did not fail the query")
	} else if !strings.Contains(err.Error(), "deliberate failure") {
		t.Errorf("ResultError message lost: %v", err)
	}
}

// TestFunctionValuesSurviveTheCorpus checks that arguments and results cross
// the callback boundary byte-for-byte, using the same corpus the storage path
// is held to. A function boundary that mangles an embedded NUL is no better
// than a column accessor that does.
func TestFunctionValuesSurviveTheCorpus(t *testing.T) {
	c := open(t)

	if err := c.CreateFunction("go_echo_text", 1, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultText(a[0].Text())
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateFunction("go_echo_blob", 1, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		ctx.ResultBlob(a[0].Blob())
	}); err != nil {
		t.Fatal(err)
	}

	mustExec(t, c, `CREATE TABLE f (name TEXT, t TEXT, b BLOB)`)
	ins, err := c.Prepare(`INSERT INTO f VALUES (?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()

	values := conformance.Values()
	if len(values) == 0 {
		t.Fatal("empty corpus")
	}
	for _, v := range values {
		if err := ins.Bind(v.Name, v.Text, v.Blob); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}

	st, err := c.Prepare(`SELECT name, go_echo_text(t), go_echo_blob(b) FROM f`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	want := map[string]conformance.Value{}
	for _, v := range values {
		want[v.Name] = v
	}
	seen := 0
	for {
		row, err := st.Step()
		if err != nil {
			t.Fatal(err)
		}
		if !row {
			break
		}
		v := want[st.ColumnText(0)]
		if got := st.ColumnText(1); got != v.Text {
			t.Errorf("%s: text through a Go function changed\n  in  %q\n  out %q", v.Name, v.Text, got)
		}
		if got := st.ColumnBlob(2); !bytes.Equal(got, v.Blob) {
			t.Errorf("%s: blob through a Go function changed\n  in  %x\n  out %x", v.Name, v.Blob, got)
		}
		seen++
	}
	if seen != len(values) {
		t.Errorf("checked %d values, corpus has %d", seen, len(values))
	}
	t.Logf("round-tripped %d corpus values through a Go SQL function", seen)
}

// TestFunctionPanicBecomesSQLError pins the property that keeps a bug in an
// application function from killing the process: a Go panic must not unwind
// into SQLite's C frames.
func TestFunctionPanicBecomesSQLError(t *testing.T) {
	c := open(t)
	if err := c.CreateFunction("go_boom", 0, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
		panic("deliberate")
	}); err != nil {
		t.Fatal(err)
	}

	err := c.Exec(`SELECT go_boom()`)
	if err == nil {
		t.Fatal("a panicking function did not fail the query")
	}
	if !strings.Contains(err.Error(), "deliberate") {
		t.Errorf("panic value lost: %v", err)
	}

	// The connection must still work afterwards.
	var got string
	if err := queryOne(c, `SELECT 1+1`, &got); err != nil {
		t.Fatalf("connection unusable after a function panic: %v", err)
	}
	if got != "2" {
		t.Errorf("after the panic, 1+1 = %s", got)
	}
}

// TestManyFunctionsShareOneCallback is the check behind the registry design:
// purego can only ever mint about 2000 C function pointers in a process and
// never reclaims one, so a callback per registered function would be a slow
// leak. Registering many functions must consume one.
func TestManyFunctionsShareOneCallback(t *testing.T) {
	c := open(t)
	const n = 500
	for i := 0; i < n; i++ {
		i := i
		name := "go_fn_" + itoaTest(i)
		if err := c.CreateFunction(name, 0, 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
			ctx.ResultInt64(int64(i))
		}); err != nil {
			t.Fatalf("registering function %d: %v", i, err)
		}
	}
	// Spot-check that each resolves to its own closure, which is what proves
	// the shared trampoline dispatches correctly rather than always calling
	// the last-registered function.
	for _, i := range []int{0, 1, n / 2, n - 1} {
		var got string
		if err := queryOne(c, `SELECT go_fn_`+itoaTest(i)+`()`, &got); err != nil {
			t.Fatal(err)
		}
		if got != itoaTest(i) {
			t.Errorf("go_fn_%d() = %s", i, got)
		}
	}
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestFunctionPathIsStated says which of the two implementations the rest of
// this file just exercised. Without it, a marshaling trampoline that failed to
// load would leave every other function test passing on the portable path and
// the fast one shipped untested.
//
// The suite is meant to be run both ways:
//
//	go test ./...
//	PUREGO_SQLITE_FUNCTIONS=portable go test ./...
func TestFunctionPathIsStated(t *testing.T) {
	fast, err := sqlite3.UseMarshaledFunctions()
	if err != nil {
		t.Fatal(err)
	}
	forced := os.Getenv(sqlite3.FunctionPathEnv) == "portable"

	switch {
	case forced && fast:
		t.Fatalf("%s=portable but functions still take the marshaled path", sqlite3.FunctionPathEnv)
	case forced:
		t.Log("functions on the portable path (one crossing per argument and result)")
	case !fast:
		t.Fatalf("the shipped library has no fn_scalar_ptr, so every function test just ran the portable path; rebuild with `make build`")
	default:
		t.Log("functions on the marshaled path (one crossing per invocation)")
	}
}

// TestBothFunctionPathsAgree runs the identical function through both
// implementations in one process and requires the same answers, including the
// cases each path handles differently: values read as a type they are not
// stored as, results too large for the inline buffer, and NULL.
func TestBothFunctionPathsAgree(t *testing.T) {
	fast, err := sqlite3.UseMarshaledFunctions()
	if err != nil {
		t.Fatal(err)
	}
	if !fast {
		t.Skip("marshaled path unavailable; nothing to compare against")
	}

	// Registration picks the path, so two connections with the same functions
	// registered under different forcing give both implementations at once.
	read := func(c *sqlite3.Conn) []string {
		reg := func(name string, n int, fn sqlite3.ScalarFunc) {
			t.Helper()
			if err := c.CreateFunction(name, n, 0, fn); err != nil {
				t.Fatal(err)
			}
		}
		reg("f_int", 1, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) { ctx.ResultInt64(a[0].Int64()) })
		reg("f_float", 1, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) { ctx.ResultFloat(a[0].Float()) })
		reg("f_text", 1, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) { ctx.ResultText(a[0].Text()) })
		reg("f_blob", 1, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) { ctx.ResultBlob(a[0].Blob()) })
		reg("f_type", 1, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) { ctx.ResultText(a[0].Type().String()) })
		reg("f_null", 0, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) { ctx.ResultNull() })
		reg("f_big", 1, func(ctx *sqlite3.FuncContext, a []sqlite3.Value) {
			// Deliberately past the inline result buffer, so the fast path
			// must fall back to calling SQLite itself.
			ctx.ResultText(strings.Repeat("x", int(a[0].Int64())))
		})

		queries := []string{
			`SELECT f_int(42)`,
			`SELECT f_int('17')`,    // text read as integer: SQLite's conversion
			`SELECT f_int(2.9)`,     // float read as integer
			`SELECT f_float('2.5')`, // text read as float
			`SELECT f_float(7)`,     // integer read as float
			`SELECT f_text(42)`,     // integer read as text
			`SELECT f_text(2.5)`,    // float read as text
			`SELECT quote(f_blob(x'00ff'))`,
			`SELECT quote(f_blob(NULL))`,
			`SELECT f_type(1)`, `SELECT f_type('s')`, `SELECT f_type(x'00')`, `SELECT f_type(NULL)`,
			`SELECT quote(f_null())`,
			`SELECT length(f_big(100))`,
			`SELECT length(f_big(511))`,   // just inside the inline buffer
			`SELECT length(f_big(512))`,   // exactly the inline buffer
			`SELECT length(f_big(513))`,   // just past it: the fallback
			`SELECT length(f_big(70000))`, // far past it
		}
		var out []string
		for _, q := range queries {
			var got string
			if err := queryOne(c, q, &got); err != nil {
				out = append(out, q+" => ERROR "+err.Error())
				continue
			}
			out = append(out, q+" => "+got)
		}
		return out
	}

	marshaled := read(open(t))

	if err := os.Setenv(sqlite3.FunctionPathEnv, "portable"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv(sqlite3.FunctionPathEnv) })
	// The environment is read at library load, which already happened, so
	// flipping it here does nothing on its own; the package exposes the switch
	// for exactly this reason.
	sqlite3.ForcePortableFunctionsForTest(true)
	defer sqlite3.ForcePortableFunctionsForTest(false)

	portable := read(open(t))

	if len(marshaled) != len(portable) {
		t.Fatalf("%d results marshaled, %d portable", len(marshaled), len(portable))
	}
	if len(marshaled) == 0 {
		t.Fatal("compared nothing")
	}
	for i := range marshaled {
		if marshaled[i] != portable[i] {
			t.Errorf("paths disagree:\n  marshaled %s\n  portable  %s", marshaled[i], portable[i])
		}
	}
	t.Logf("both paths agree on %d queries", len(marshaled))
}
