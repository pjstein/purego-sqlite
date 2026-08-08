package sqlite3_test

import (
	"bytes"
	"os"
	"testing"

	sqlite3 "github.com/pjstein/purego-sqlite"
	"github.com/pjstein/purego-sqlite/conformance"
)

// TestRowBatchingIsActive states which path the rest of the suite just
// exercised. Without it, a shim that failed to load would leave every other
// test passing on the fallback path and the fast path would ship untested.
//
// The suite is meant to be run both ways:
//
//	go test ./...
//	PUREGO_SQLITE_BATCH=0 go test ./...
func TestRowBatchingIsActive(t *testing.T) {
	active, err := sqlite3.RowBatchingActive()
	if err != nil {
		t.Fatal(err)
	}
	disabled := os.Getenv(sqlite3.BatchEnv) == "0"

	switch {
	case disabled && active:
		t.Fatalf("%s=0 but row batching is still active", sqlite3.BatchEnv)
	case disabled:
		t.Log("row batching disabled by environment; this run covered the per-call fallback path")
	case !active:
		t.Fatalf("the shipped library has no rowbatch_step, so every other test in this package just ran the fallback path; rebuild with `make build`")
	default:
		t.Log("row batching active; this run covered the batched path")
	}
}

// TestBatchedBlobAndTextAreExact checks the fast path against the corpus
// directly: whatever the batching shim writes into the row buffer, the values
// that come back must still be byte-identical to what went in.
func TestBatchedBlobAndTextAreExact(t *testing.T) {
	c := open(t)
	mustExec(t, c, `CREATE TABLE b (k TEXT, t TEXT, b BLOB)`)
	ins, err := c.Prepare(`INSERT INTO b VALUES (?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()
	for _, v := range conformance.Values() {
		if err := ins.Bind(v.Name, v.Text, v.Blob); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}

	st, err := c.Prepare(`SELECT k, t, b FROM b`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	want := map[string]conformance.Value{}
	for _, v := range conformance.Values() {
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
			t.Errorf("%s: text %q, want %q", v.Name, got, v.Text)
		}
		if got := st.ColumnBlob(2); !bytes.Equal(got, v.Blob) {
			t.Errorf("%s: blob %x, want %x", v.Name, got, v.Blob)
		}
		if got := st.ColumnBytes(2); got != len(v.Blob) {
			t.Errorf("%s: blob length %d, want %d", v.Name, got, len(v.Blob))
		}
		if got := string(st.ColumnRawText(1)); got != v.Text {
			t.Errorf("%s: raw text %q, want %q", v.Name, got, v.Text)
		}
		if got := st.ColumnRawBlob(2); !bytes.Equal(got, v.Blob) {
			t.Errorf("%s: raw blob %x, want %x", v.Name, got, v.Blob)
		}
		seen++
	}
	if seen != len(want) {
		t.Errorf("read %d rows, wrote %d", seen, len(want))
	}
}
