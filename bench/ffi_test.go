package bakeoff_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ebitengine/purego"
	sqlite3 "github.com/pjstein/purego-sqlite"
	bakeoff "github.com/pjstein/purego-sqlite/bench"
	"github.com/pjstein/purego-sqlite/bench/cgoref"
)

// ---- the null call: what the boundary itself costs ----

var (
	ffiOnce            sync.Once
	ffiHandle          uintptr
	ffiNullAddr        uintptr
	ffiNullRegistered  func() int32
	ffiSetupErr        error
	ffiLibraryLocation string
)

// setupFFI opens the very library the purego binding is already using, so the
// three null-call measurements below differ only in how the call is made.
func setupFFI(tb testing.TB) {
	tb.Helper()
	ffiOnce.Do(func() {
		if _, err := sqlite3.Version(); err != nil {
			ffiSetupErr = err
			return
		}
		ffiLibraryLocation = sqlite3.LoadedLibraryPath()
		h, err := purego.Dlopen(ffiLibraryLocation, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			ffiSetupErr = err
			return
		}
		ffiHandle = h
		addr, err := purego.Dlsym(h, "sqlite3_libversion_number")
		if err != nil {
			ffiSetupErr = err
			return
		}
		ffiNullAddr = addr
		purego.RegisterLibFunc(&ffiNullRegistered, h, "sqlite3_libversion_number")
	})
	if ffiSetupErr != nil {
		tb.Fatalf("setting up the FFI comparison: %v", ffiSetupErr)
	}
}

// BenchmarkNullCall measures a call with no arguments, an integer return, and
// no work behind it: sqlite3_libversion_number. What separates the three
// results is only the mechanism that crosses the boundary.
func BenchmarkNullCall(b *testing.B) {
	setupFFI(b)

	b.Run("cgo", func(b *testing.B) {
		if !cgoref.Available {
			b.Skip("cgo reference not built")
		}
		var sink int
		for b.Loop() {
			sink = cgoref.NullCall()
		}
		_ = sink
	})

	b.Run("purego-SyscallN", func(b *testing.B) {
		var sink uintptr
		for b.Loop() {
			sink, _, _ = purego.SyscallN(ffiNullAddr)
		}
		_ = sink
	})

	// The reflection-based path, which is what purego.RegisterLibFunc gives
	// you and what this binding deliberately keeps off the per-row surface.
	b.Run("purego-RegisterFunc", func(b *testing.B) {
		var sink int32
		for b.Loop() {
			sink = ffiNullRegistered()
		}
		_ = sink
	})
}

// ---- a seeded database every driver reads ----

var (
	seedOnce sync.Once
	seedPath string
	seedErr  error
	seedRows int
)

const seedRowCount = 50000

// seededDB builds one database with the purego driver and lets every driver
// read it. Seeding per driver would risk comparing different files.
func seededDB(tb testing.TB) string {
	tb.Helper()
	seedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bakeoff-seed-")
		if err != nil {
			seedErr = err
			return
		}
		path := filepath.Join(dir, "seed.db")
		db, err := (bakeoff.PuregoDriver{}).Open(path)
		if err != nil {
			seedErr = err
			return
		}
		defer db.Close()
		if err := db.Exec(bakeoff.Schema); err != nil {
			seedErr = err
			return
		}
		app, err := db.NewAppender()
		if err != nil {
			seedErr = err
			return
		}
		defer app.Close()
		payload := bakeoff.SeedPayload(bakeoff.DefaultPayloadSize)
		if err := db.Exec(`BEGIN`); err != nil {
			seedErr = err
			return
		}
		for i := 0; i < seedRowCount; i++ {
			if err := app.Append("stream-a", "event.appended", int64(i), payload); err != nil {
				seedErr = err
				return
			}
		}
		if err := db.Exec(`COMMIT`); err != nil {
			seedErr = err
			return
		}
		// Checkpoint so no driver pays for another driver's leftover WAL.
		if err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			seedErr = err
			return
		}
		seedPath, seedRows = path, seedRowCount
	})
	if seedErr != nil {
		tb.Fatalf("seeding the benchmark database: %v", seedErr)
	}
	return seedPath
}

// BenchmarkPoll runs the subscriber query -- bind a cursor, step up to 64
// rows, read all four columns of each -- once per iteration, through every
// driver plus the cgo reference over the identical library.
func BenchmarkPoll(b *testing.B) {
	path := seededDB(b)

	b.Run("cgo (same library)", func(b *testing.B) {
		if !cgoref.Available {
			b.Skip("cgo reference not built")
		}
		c, err := cgoref.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		for _, p := range bakeoff.Pragmas {
			if err := c.Exec(p); err != nil {
				b.Fatal(err)
			}
		}
		st, err := c.Prepare(bakeoff.TailSQL)
		if err != nil {
			b.Fatal(err)
		}
		defer st.Close()

		var cursor int64
		rows := 0
		for b.Loop() {
			if err := st.BindInt64(1, cursor); err != nil {
				b.Fatal(err)
			}
			last, n := cursor, 0
			for {
				row, err := st.Step()
				if err != nil {
					b.Fatal(err)
				}
				if !row {
					break
				}
				last = st.ColumnInt64(0)
				_ = st.ColumnText(1)
				_ = st.ColumnInt64(2)
				_ = st.ColumnBlob(3)
				n++
			}
			if err := st.Reset(); err != nil {
				b.Fatal(err)
			}
			rows += n
			cursor = last
			if n < bakeoff.TailLimit {
				cursor = 0
			}
		}
		reportPerRow(b, rows)
	})

	// The same cgo reference, given the same row-batching shim the purego
	// binding uses. Without this row the comparison would silently be between
	// two call granularities rather than between two calling conventions.
	b.Run("cgo batched (same shim)", func(b *testing.B) {
		if !cgoref.Available {
			b.Skip("cgo reference not built")
		}
		c, err := cgoref.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		for _, p := range bakeoff.Pragmas {
			if err := c.Exec(p); err != nil {
				b.Fatal(err)
			}
		}
		st, err := c.Prepare(bakeoff.TailSQL)
		if err != nil {
			b.Fatal(err)
		}
		defer st.Close()

		buf := cgoref.NewRowBuffer(4)
		var cursor int64
		rows := 0
		for b.Loop() {
			if err := st.BindInt64(1, cursor); err != nil {
				b.Fatal(err)
			}
			last, n := cursor, 0
			for {
				row, err := st.StepRow(buf)
				if err != nil {
					b.Fatal(err)
				}
				if !row {
					break
				}
				_, seq, _, _ := cgoref.ColValue(buf[0])
				_, _, _, _ = cgoref.ColValue(buf[1])
				_, _, _, _ = cgoref.ColValue(buf[2])
				_, _, _, _ = cgoref.ColValue(buf[3])
				last = seq
				n++
			}
			if err := st.Reset(); err != nil {
				b.Fatal(err)
			}
			rows += n
			cursor = last
			if n < bakeoff.TailLimit {
				cursor = 0
			}
		}
		reportPerRow(b, rows)
	})

	for _, d := range bakeoff.Drivers() {
		b.Run(d.Name(), func(b *testing.B) {
			db, err := d.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			tail, err := db.NewTailer()
			if err != nil {
				b.Fatal(err)
			}
			defer tail.Close()

			var cursor int64
			rows := 0
			for b.Loop() {
				last, n, err := tail.Poll(cursor)
				if err != nil {
					b.Fatal(err)
				}
				rows += n
				cursor = last
				if n < bakeoff.TailLimit {
					cursor = 0
				}
			}
			reportPerRow(b, rows)
		})
	}
}

// BenchmarkAppend runs a single-row insert per iteration, each its own
// implicit transaction, which is what an event append is.
func BenchmarkAppend(b *testing.B) {
	payload := bakeoff.SeedPayload(bakeoff.DefaultPayloadSize)

	b.Run("cgo (same library)", func(b *testing.B) {
		if !cgoref.Available {
			b.Skip("cgo reference not built")
		}
		path := filepath.Join(b.TempDir(), "append.db")
		c, err := cgoref.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		for _, p := range bakeoff.Pragmas {
			if err := c.Exec(p); err != nil {
				b.Fatal(err)
			}
		}
		if err := c.Exec(bakeoff.Schema); err != nil {
			b.Fatal(err)
		}
		st, err := c.Prepare(bakeoff.AppendSQL)
		if err != nil {
			b.Fatal(err)
		}
		defer st.Close()

		for i := 0; b.Loop(); i++ {
			if err := st.BindText(1, "stream-a"); err != nil {
				b.Fatal(err)
			}
			if err := st.BindText(2, "event.appended"); err != nil {
				b.Fatal(err)
			}
			if err := st.BindInt64(3, int64(i)); err != nil {
				b.Fatal(err)
			}
			if err := st.BindBlob(4, payload); err != nil {
				b.Fatal(err)
			}
			for {
				row, err := st.Step()
				if err != nil {
					b.Fatal(err)
				}
				if !row {
					break
				}
			}
			if err := st.Reset(); err != nil {
				b.Fatal(err)
			}
		}
	})

	for _, d := range bakeoff.Drivers() {
		b.Run(d.Name(), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "append.db")
			db, err := d.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := db.Exec(bakeoff.Schema); err != nil {
				b.Fatal(err)
			}
			app, err := db.NewAppender()
			if err != nil {
				b.Fatal(err)
			}
			defer app.Close()

			for i := 0; b.Loop(); i++ {
				if err := app.Append("stream-a", "event.appended", int64(i), payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func reportPerRow(b *testing.B, rows int) {
	if rows == 0 {
		b.Fatal("the poll loop read zero rows; the benchmark measured nothing")
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(rows), "ns/row")
	b.ReportMetric(float64(rows)/b.Elapsed().Seconds(), "rows/s")
}

func TestFFISetupReportsWhatItMeasured(t *testing.T) {
	setupFFI(t)
	if !cgoref.Available {
		t.Fatal("the cgo reference is not built; the purego-tax numbers would be missing their baseline")
	}
	pure, err := sqlite3.Version()
	if err != nil {
		t.Fatal(err)
	}
	if got := cgoref.Version(); got != pure {
		t.Fatalf("the cgo reference is on SQLite %s and the purego binding on %s; they must be the same library for the tax measurement to mean anything", got, pure)
	}
	fmt.Printf("FFI comparison drives %s (SQLite %s) through both purego and cgo\n", ffiLibraryLocation, pure)
}
