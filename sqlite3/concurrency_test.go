package sqlite3_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjstein/purego-sqlite/sqlite3"
)

// openWAL creates a WAL-mode database at path and returns a connection to it.
func openWAL(t *testing.T, path string) *sqlite3.Conn {
	t.Helper()
	c, err := sqlite3.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enabling WAL: %v", err)
	}
	var mode string
	if err := queryOne(c, `PRAGMA journal_mode`, &mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode is %q, not wal; every concurrency claim below would be measuring the wrong thing", mode)
	}
	return c
}

func TestWALModeAndCheckpoint(t *testing.T) {
	path := tempDB(t)
	c := openWAL(t, path)

	mustExec(t, c, `CREATE TABLE w (n INTEGER)`)
	mustExec(t, c, `PRAGMA wal_autocheckpoint=0`) // keep checkpoints off the write path
	if err := c.WALAutoCheckpoint(0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		mustExec(t, c, `INSERT INTO w VALUES (?)`, i)
	}

	walPath := path + "-wal"
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("no WAL file at %s: %v", walPath, err)
	}
	if fi.Size() == 0 {
		t.Fatal("WAL file is empty after 500 inserts with autocheckpoint disabled")
	}
	t.Logf("WAL grew to %d bytes before checkpointing", fi.Size())

	// A passive checkpoint reports the WAL it found and how much of it it
	// moved into the database. With no other connection open, it moves all
	// of it.
	walPages, moved, err := c.WALCheckpoint("main", sqlite3.CheckpointPassive)
	if err != nil {
		t.Fatalf("passive WALCheckpoint: %v", err)
	}
	if walPages == 0 || moved == 0 {
		t.Errorf("passive checkpoint reported walPages=%d checkpointed=%d, want both non-zero", walPages, moved)
	}
	if walPages != moved {
		t.Errorf("passive checkpoint left %d of %d frames behind with no other connection open", walPages-moved, walPages)
	}
	t.Logf("passive checkpoint moved %d of %d WAL frames", moved, walPages)

	// A truncating checkpoint reports zeroes, and that is correct rather than
	// a failure: SQLite reads both counters out of the WAL header after the
	// truncation has already reset it.
	walPages, moved, err = c.WALCheckpoint("main", sqlite3.CheckpointTruncate)
	if err != nil {
		t.Fatalf("truncating WALCheckpoint: %v", err)
	}
	if walPages != 0 || moved != 0 {
		t.Errorf("truncating checkpoint reported walPages=%d checkpointed=%d, want 0 and 0 (counters are read after the reset)", walPages, moved)
	}

	fi, err = os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("WAL is %d bytes after a truncating checkpoint, want 0", fi.Size())
	}

	var n string
	if err := queryOne(c, `SELECT count(*) FROM w`, &n); err != nil {
		t.Fatal(err)
	}
	if n != "500" {
		t.Errorf("after checkpoint the table holds %s rows, want 500", n)
	}
}

// TestConcurrentReadersDuringWrite is the shape this package exists to serve:
// one connection appending events while several others poll for them, each on
// its own connection. Every reader must make progress -- a reader that returns
// zero results for the whole run is starved, which is the failure mode that
// matters when readers are subscribers.
func TestConcurrentReadersDuringWrite(t *testing.T) {
	path := tempDB(t)
	setup := openWAL(t, path)
	mustExec(t, setup, `CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, payload BLOB)`)

	const (
		readers  = 8
		duration = 2 * time.Second
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes atomic.Int64
	readCounts := make([]int64, readers)
	errCh := make(chan error, readers+1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		w, err := sqlite3.Open(path)
		if err != nil {
			errCh <- err
			return
		}
		defer w.Close()
		if err := w.BusyTimeout(5 * time.Second); err != nil {
			errCh <- err
			return
		}
		ins, err := w.PrepareFlags(`INSERT INTO events (payload) VALUES (?)`, sqlite3.PreparePersistent)
		if err != nil {
			errCh <- err
			return
		}
		defer ins.Close()
		payload := make([]byte, 256)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := ins.Bind(payload); err != nil {
				errCh <- err
				return
			}
			if err := ins.Run(); err != nil {
				errCh <- err
				return
			}
			writes.Add(1)
		}
	}()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := sqlite3.Open(path)
			if err != nil {
				errCh <- err
				return
			}
			defer r.Close()
			if err := r.BusyTimeout(5 * time.Second); err != nil {
				errCh <- err
				return
			}
			q, err := r.PrepareFlags(`SELECT seq FROM events WHERE seq > ? ORDER BY seq LIMIT 32`, sqlite3.PreparePersistent)
			if err != nil {
				errCh <- err
				return
			}
			defer q.Close()

			var cursor int64
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := q.Bind(cursor); err != nil {
					errCh <- err
					return
				}
				for {
					row, err := q.Step()
					if err != nil {
						errCh <- err
						return
					}
					if !row {
						break
					}
					cursor = q.ColumnInt64(0)
				}
				if err := q.Reset(); err != nil {
					errCh <- err
					return
				}
				readCounts[i]++
			}
		}(i)
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("worker error: %v", err)
	}

	minReads, maxReads := readCounts[0], readCounts[0]
	var total int64
	for _, n := range readCounts {
		total += n
		if n < minReads {
			minReads = n
		}
		if n > maxReads {
			maxReads = n
		}
	}
	t.Logf("%d writes; reader query rounds min=%d max=%d total=%d over %s", writes.Load(), minReads, maxReads, total, duration)

	if writes.Load() == 0 {
		t.Error("the writer never committed a row")
	}
	if minReads == 0 {
		t.Errorf("a reader was starved: per-reader counts %v", readCounts)
	}
	// A spread this wide would mean readers are queueing behind each other
	// rather than reading concurrently from their own WAL snapshots.
	if minReads > 0 && maxReads/minReads > 20 {
		t.Errorf("reader throughput is wildly unfair: min=%d max=%d (%v)", minReads, maxReads, readCounts)
	}
}

func TestBusyTimeoutWaitsThenReportsBusy(t *testing.T) {
	path := tempDB(t)
	a := openWAL(t, path)
	mustExec(t, a, `CREATE TABLE b (n INTEGER)`)

	blocker, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	mustExec(t, blocker, `BEGIN IMMEDIATE`)
	defer blocker.Exec(`ROLLBACK`)

	const timeout = 300 * time.Millisecond
	if err := a.BusyTimeout(timeout); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = a.Exec(`INSERT INTO b VALUES (1)`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the blocked write succeeded while another connection held the write lock")
	}
	if !errors.Is(err, sqlite3.Busy) {
		t.Fatalf("blocked write failed with %v, want a Busy error", err)
	}
	// The shipped library is compiled with HAVE_USLEEP; without it SQLite
	// sleeps in whole seconds and a 300ms timeout becomes a 1s stall.
	if elapsed < timeout*3/4 {
		t.Errorf("gave up after %v, well under the %v busy timeout", elapsed, timeout)
	}
	if elapsed > timeout*4 {
		t.Errorf("waited %v for a %v busy timeout; sub-second sleeps are not working (HAVE_USLEEP)", elapsed, timeout)
	}
	t.Logf("busy timeout of %v was honoured after %v", timeout, elapsed)
}

func TestBusyTimeoutZeroFailsImmediately(t *testing.T) {
	path := tempDB(t)
	a := openWAL(t, path)
	mustExec(t, a, `CREATE TABLE b (n INTEGER)`)

	blocker, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	mustExec(t, blocker, `BEGIN IMMEDIATE`)
	defer blocker.Exec(`ROLLBACK`)

	if err := a.BusyTimeout(0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = a.Exec(`INSERT INTO b VALUES (1)`)
	elapsed := time.Since(start)

	if !errors.Is(err, sqlite3.Busy) {
		t.Fatalf("got %v, want Busy", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("a zero busy timeout still waited %v", elapsed)
	}
}

func TestInterruptCancelsARunningQuery(t *testing.T) {
	c := open(t)

	done := make(chan error, 1)
	go func() {
		// A recursive CTE with no termination, so the only way out is the
		// interrupt.
		done <- c.Exec(`WITH RECURSIVE forever(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM forever) SELECT count(*) FROM forever`)
	}()

	time.Sleep(100 * time.Millisecond)
	c.Interrupt()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the unbounded query returned without error")
		}
		if !errors.Is(err, sqlite3.Interrupt) {
			t.Errorf("query ended with %v, want an Interrupt error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Interrupt did not stop the query")
	}
}

func TestBackupCopiesAnOpenDatabase(t *testing.T) {
	src := open(t)
	mustExec(t, src, `CREATE TABLE things (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, src, `BEGIN`)
	ins, err := src.Prepare(`INSERT INTO things (v) VALUES (?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := ins.Bind("value with a \x00 nul and a \n newline"); err != nil {
			t.Fatal(err)
		}
		if err := ins.Run(); err != nil {
			t.Fatal(err)
		}
	}
	ins.Close()
	mustExec(t, src, `COMMIT`)

	dstPath := filepath.Join(t.TempDir(), "backup.db")
	dst, err := sqlite3.Open(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	b, err := dst.BackupFrom("main", src, "main")
	if err != nil {
		t.Fatal(err)
	}
	steps := 0
	for {
		done, err := b.Step(8) // a few pages at a time, the incremental shape
		if err != nil {
			t.Fatal(err)
		}
		steps++
		if done {
			break
		}
		if b.PageCount() == 0 {
			t.Fatal("PageCount is zero mid-backup")
		}
	}
	if steps < 2 {
		t.Errorf("backup finished in %d step(s); it was not exercised incrementally", steps)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	var n, sample string
	if err := queryOne(dst, `SELECT count(*) FROM things`, &n); err != nil {
		t.Fatal(err)
	}
	if n != "2000" {
		t.Errorf("backup holds %s rows, want 2000", n)
	}
	if err := queryOne(dst, `SELECT v FROM things LIMIT 1`, &sample); err != nil {
		t.Fatal(err)
	}
	if sample != "value with a \x00 nul and a \n newline" {
		t.Errorf("backup mangled a value: %q", sample)
	}
}

func TestBackupToConvenience(t *testing.T) {
	src := open(t)
	mustExec(t, src, `CREATE TABLE q (n INTEGER)`)
	mustExec(t, src, `INSERT INTO q VALUES (7)`)

	dst, err := sqlite3.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	if err := src.BackupTo(dst); err != nil {
		t.Fatal(err)
	}
	var n string
	if err := queryOne(dst, `SELECT n FROM q`, &n); err != nil {
		t.Fatal(err)
	}
	if n != "7" {
		t.Errorf("copied value = %s, want 7", n)
	}

	if _, err := src.BackupFrom("main", src, "main"); err == nil {
		t.Error("backing a connection up to itself was allowed")
	}
}
