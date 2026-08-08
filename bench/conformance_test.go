package bakeoff_test

import (
	"bytes"
	"path/filepath"
	"testing"

	bakeoff "github.com/pjstein/purego-sqlite/bench"
	"github.com/pjstein/purego-sqlite/conformance"
)

// TestDriverConformance runs the identical corpus through every driver under
// test. It is the check that makes the performance comparison meaningful: a
// driver that is fast because it corrupts a value has not won anything.
//
// Failures here are reported per driver rather than aborting, because the
// interesting result is the pattern -- which drivers disagree, about what.
func TestDriverConformance(t *testing.T) {
	for _, d := range bakeoff.Drivers() {
		t.Run(d.Name(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conformance.db")
			db, err := d.Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer db.Close()

			version, err := db.SQLiteVersion()
			if err != nil {
				t.Fatalf("SQLiteVersion: %v", err)
			}
			t.Logf("SQLite %s", version)

			if err := db.Exec(bakeoff.RoundTripTable); err != nil {
				t.Fatalf("creating round-trip table: %v", err)
			}

			values := conformance.Values()
			if len(values) == 0 {
				t.Fatal("the value corpus is empty; this test would pass without checking anything")
			}
			for _, v := range values {
				gotText, gotBlob, err := db.RoundTrip(v.Name, v.Text, v.Blob)
				if err != nil {
					t.Errorf("value %s: %v", v.Name, err)
					continue
				}
				if gotText != v.Text {
					t.Errorf("value %s: TEXT changed\n  in  (%d bytes) %q\n  out (%d bytes) %q",
						v.Name, len(v.Text), v.Text, len(gotText), gotText)
				}
				if !bytes.Equal(gotBlob, v.Blob) {
					t.Errorf("value %s: BLOB changed\n  in  (%d bytes) %x\n  out (%d bytes) %x",
						v.Name, len(v.Blob), v.Blob, len(gotBlob), gotBlob)
				}
			}

			exprs := conformance.Exprs()
			if len(exprs) == 0 {
				t.Fatal("the expression corpus is empty")
			}
			for _, e := range exprs {
				got, err := db.QueryText(e.SQL)
				if err != nil {
					t.Errorf("expr %s (%s): %v", e.Name, e.SQL, err)
					continue
				}
				if len(got) != 1 {
					t.Errorf("expr %s: returned %d rows, want 1", e.Name, len(got))
					continue
				}
				if got[0] != e.Want {
					t.Errorf("expr %s: %s\n  got  %q\n  want %q", e.Name, e.SQL, got[0], e.Want)
				}
			}
			t.Logf("checked %d round-trip values and %d expressions", len(values), len(exprs))
		})
	}
}

// TestWorkloadShapeIsIdentical checks that every driver, given the same
// appends, produces the same rows through the same tail query. Without this
// the throughput numbers could be comparing different amounts of work.
func TestWorkloadShapeIsIdentical(t *testing.T) {
	const appends = 500

	type result struct {
		rows int
		last int64
	}
	results := map[string]result{}

	for _, d := range bakeoff.Drivers() {
		path := filepath.Join(t.TempDir(), "shape.db")
		db, err := d.Open(path)
		if err != nil {
			t.Fatalf("%s: Open: %v", d.Name(), err)
		}
		if err := db.Exec(bakeoff.Schema); err != nil {
			t.Fatalf("%s: schema: %v", d.Name(), err)
		}

		app, err := db.NewAppender()
		if err != nil {
			t.Fatalf("%s: NewAppender: %v", d.Name(), err)
		}
		payload := bakeoff.SeedPayload(bakeoff.DefaultPayloadSize)
		for i := 0; i < appends; i++ {
			if err := app.Append("stream-a", "event.kind", int64(i), payload); err != nil {
				t.Fatalf("%s: Append: %v", d.Name(), err)
			}
		}
		app.Close()

		tail, err := db.NewTailer()
		if err != nil {
			t.Fatalf("%s: NewTailer: %v", d.Name(), err)
		}
		var cursor int64
		total := 0
		for {
			last, n, err := tail.Poll(cursor)
			if err != nil {
				t.Fatalf("%s: Poll: %v", d.Name(), err)
			}
			total += n
			if n == 0 {
				break
			}
			cursor = last
		}
		tail.Close()
		db.Close()

		results[d.Name()] = result{rows: total, last: cursor}
		if total != appends {
			t.Errorf("%s: tailed %d rows after %d appends", d.Name(), total, appends)
		}
	}

	var first string
	for name, r := range results {
		if first == "" {
			first = name
			continue
		}
		if r != results[first] {
			t.Errorf("drivers disagree on the workload result: %s=%+v %s=%+v", first, results[first], name, r)
		}
	}
}
