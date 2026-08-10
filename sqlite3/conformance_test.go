package sqlite3_test

import (
	"bytes"
	"testing"

	"github.com/pjstein/purego-sqlite/sqlite3"
	"github.com/pjstein/purego-sqlite/conformance"
)

// TestValueRoundTrip stores every corpus value as both TEXT and BLOB and reads
// it back. A value that goes in and comes out different is data loss, and the
// corpus is built out of the classes that lose data quietly: embedded NUL,
// real newlines, astral-plane characters, and bytes that are not valid UTF-8.
func TestValueRoundTrip(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE v (name TEXT PRIMARY KEY, t TEXT, b BLOB)`)

	ins, err := c.Prepare(`INSERT INTO v (name, t, b) VALUES (?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()

	sel, err := c.Prepare(`SELECT t, b, length(CAST(b AS BLOB)) FROM v WHERE name = ?`)
	if err != nil {
		t.Fatal(err)
	}
	defer sel.Close()

	for _, v := range conformance.Values() {
		t.Run(v.Name, func(t *testing.T) {
			if err := ins.Bind(v.Name, v.Text, v.Blob); err != nil {
				t.Fatal(err)
			}
			if err := ins.Run(); err != nil {
				t.Fatal(err)
			}

			if err := sel.Bind(v.Name); err != nil {
				t.Fatal(err)
			}
			row, err := sel.Step()
			if err != nil {
				t.Fatal(err)
			}
			if !row {
				t.Fatal("value did not come back")
			}

			if got := sel.ColumnText(0); got != v.Text {
				t.Errorf("TEXT round trip lost data\n  in  (%d bytes): %q\n  out (%d bytes): %q", len(v.Text), v.Text, len(got), got)
			}
			gotBlob := sel.ColumnBlob(1)
			if !bytes.Equal(gotBlob, v.Blob) {
				t.Errorf("BLOB round trip lost data\n  in  (%d bytes): %x\n  out (%d bytes): %x", len(v.Blob), v.Blob, len(gotBlob), gotBlob)
			}
			if got := sel.ColumnInt(2); got != len(v.Blob) {
				t.Errorf("SQLite reports the stored blob as %d bytes, %d went in", got, len(v.Blob))
			}
			// The zero-copy accessors must see exactly what the copying ones do.
			if got := sel.ColumnRawBlob(1); !bytes.Equal(got, v.Blob) {
				t.Errorf("ColumnRawBlob disagrees with ColumnBlob: %x", got)
			}
			if got := string(sel.ColumnRawText(0)); got != v.Text {
				t.Errorf("ColumnRawText disagrees with ColumnText: %q", got)
			}
			if err := sel.Reset(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestExprConformance evaluates the shared expression corpus. These are pure
// SQLite semantics -- integer range, float rendering, affinity, NULL, the
// compiled-in function set -- so a disagreement means the library is not the
// SQLite this repository thinks it is.
func TestExprConformance(t *testing.T) {
	c := open(t)
	for _, e := range conformance.Exprs() {
		t.Run(e.Name, func(t *testing.T) {
			st, err := c.Prepare(e.SQL)
			if err != nil {
				t.Fatalf("%s: %v", e.SQL, err)
			}
			defer st.Close()
			row, err := st.Step()
			if err != nil {
				t.Fatalf("%s: %v", e.SQL, err)
			}
			if !row {
				t.Fatalf("%s: no rows", e.SQL)
			}
			if got := st.ColumnText(0); got != e.Want {
				t.Errorf("%s\n  got  %q\n  want %q", e.SQL, got, e.Want)
			}
		})
	}
}

// TestNullVersusEmpty pins the distinction that a binding is most likely to
// erase: SQL NULL, the empty string, and the zero-length blob are three
// different values.
func TestNullVersusEmpty(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE ne (k TEXT, v)`)

	st, err := c.Prepare(`INSERT INTO ne VALUES (?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cases := []struct {
		key      string
		bind     func() error
		wantType string
	}{
		{"null", func() error { return st.BindNull(2) }, "null"},
		{"empty-text", func() error { return st.BindText(2, "") }, "text"},
		{"empty-blob", func() error { return st.BindBlob(2, []byte{}) }, "blob"},
		{"nil-blob", func() error { return st.BindBlob(2, nil) }, "null"},
	}
	for _, tc := range cases {
		if err := st.BindText(1, tc.key); err != nil {
			t.Fatal(err)
		}
		if err := tc.bind(); err != nil {
			t.Fatal(err)
		}
		if err := st.Run(); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range cases {
		q, err := c.Prepare(`SELECT typeof(v) FROM ne WHERE k = ?`)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Bind(tc.key); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Step(); err != nil {
			t.Fatal(err)
		}
		if got := q.ColumnText(0); got != tc.wantType {
			t.Errorf("%s stored as typeof=%q, want %q", tc.key, got, tc.wantType)
		}
		q.Close()
	}
}

// TestBindRejectsUnrepresentable checks the two refusals: a uint64 too large
// for SQLite's signed integers, and a type with no SQLite representation. Both
// must be errors rather than silent coercions.
func TestBindRejectsUnrepresentable(t *testing.T) {
	c := open(t)
	st, err := c.Prepare(`SELECT ?`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Bind(uint64(1) << 63); err == nil {
		t.Error("binding a uint64 above MaxInt64 succeeded; it would have wrapped to a negative integer")
	}
	if err := st.Bind(struct{ A int }{1}); err == nil {
		t.Error("binding an arbitrary struct succeeded")
	}
	// The largest representable unsigned value must still work.
	if err := st.Bind(uint64(1<<63 - 1)); err != nil {
		t.Errorf("binding MaxInt64 as uint64 failed: %v", err)
	}
	if _, err := st.Step(); err != nil {
		t.Fatal(err)
	}
	if got := st.ColumnInt64(0); got != 1<<63-1 {
		t.Errorf("MaxInt64 round trip = %d", got)
	}
	_ = sqlite3.Busy
}
