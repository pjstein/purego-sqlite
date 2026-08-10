package sqlite3

import (
	"path/filepath"
	"strconv"
	"testing"
)

// TestBatchedPathAgreesWithDirectPath is the load-bearing test for the row
// batching shim, and it is an internal test because that is the only way to
// read the same row both ways in one process: it flips the package's own
// batching switch between passes.
//
// Every column is read as every type on both paths. That is deliberate -- the
// interesting cases are the ones where a value is read as a type it is not
// stored as, because those are where the fast path must decline to answer and
// hand the question back to SQLite, whose conversion rules are its own.
func TestBatchedPathAgreesWithDirectPath(t *testing.T) {
	if err := ensureLoaded(); err != nil {
		t.Fatal(err)
	}
	if !batchEnabled {
		t.Skip("row batching is not active; there is no second path to compare against")
	}

	c, err := Open(filepath.Join(t.TempDir(), "agree.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Exec(`CREATE TABLE mixed (k TEXT, s TEXT, b BLOB, i INTEGER, f REAL, n TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		k, s string
		b    []byte
		i    int64
		f    float64
	}{
		{"plain", "hello", []byte{1, 2, 3}, 42, 3.5},
		{"nul", "a\x00b", []byte{0, 0xff, 0}, -1, -0.125},
		{"empty", "", []byte{}, 0, 0},
		{"numeric-text", "1234", []byte("5678"), 9223372036854775807, 1e308},
		{"unicode", "日本語\U0001F680", []byte("日本語"), -9223372036854775808, -1e-308},
		{"float-text", "2.5e3", []byte("-0.5"), 7, 0.1},
		{"invalid-utf8", "\x80\x81", []byte{0x80, 0x81}, 1 << 40, 1.0 / 3.0},
	}
	ins, err := c.Prepare(`INSERT INTO mixed VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := ins.Bind(r.k, r.s, r.b, r.i, r.f, nil); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}
	ins.Close()

	readAll := func() [][]string {
		st, err := c.Prepare(`SELECT k, s, b, i, f, n FROM mixed ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var out [][]string
		for {
			row, err := st.Step()
			if err != nil {
				t.Fatal(err)
			}
			if !row {
				return out
			}
			var readings []string
			for col := 0; col < 6; col++ {
				readings = append(readings,
					st.ColumnType(col).String(),
					"text:"+st.ColumnText(col),
					"blob:"+string(st.ColumnBlob(col)),
					"rawtext:"+string(st.ColumnRawText(col)),
					"rawblob:"+string(st.ColumnRawBlob(col)),
					"int:"+itoa(st.ColumnInt64(col)),
					"float:"+ftoa(st.ColumnFloat(col)),
					"bytes:"+itoa(int64(st.ColumnBytes(col))),
					"isnull:"+btoa(st.ColumnIsNull(col)),
				)
			}
			out = append(out, readings)
		}
	}

	batched := readAll()

	batchEnabled = false
	direct := readAll()
	batchEnabled = true

	if len(batched) != len(rows) {
		t.Fatalf("batched read produced %d rows, want %d", len(batched), len(rows))
	}
	if len(direct) != len(batched) {
		t.Fatalf("direct read produced %d rows, batched %d", len(direct), len(batched))
	}

	compared := 0
	for r := range batched {
		for i := range batched[r] {
			compared++
			if batched[r][i] != direct[r][i] {
				t.Errorf("row %d (%s) reading %d: batched %q, direct %q",
					r, rows[r].k, i, batched[r][i], direct[r][i])
			}
		}
	}
	if compared == 0 {
		t.Fatal("compared nothing")
	}
	t.Logf("compared %d readings across %d rows on both paths", compared, len(batched))
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ftoa renders at full precision so that a difference between the two paths
// cannot hide inside rounding.
func ftoa(v float64) string { return strconv.FormatFloat(v, 'x', -1, 64) }

func btoa(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
