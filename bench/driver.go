// Package bakeoff runs the same coordination-plane workload and the same
// conformance corpus through several SQLite drivers so their numbers can be
// compared rather than guessed at.
//
// It is a separate Go module on purpose: the drivers under test are heavy
// dependencies, and the package being measured must not acquire them.
//
// Each driver implements the workload idiomatically rather than through a
// lowest-common-denominator wrapper. Forcing a database/sql driver through a
// prepare/step/column shape, or a direct driver through a rows-scanning shape,
// would measure the wrapper. What is held identical is the thing that matters:
// the schema, the SQL, the pragmas, and the sequence of operations.
package bakeoff

import (
	"fmt"
	"strings"
	"time"
)

// Schema is the coordination-plane table every driver runs against: an
// append-only event log with a monotonic sequence, which is the shape a
// coordination daemon's readers subscribe to.
const Schema = `
CREATE TABLE IF NOT EXISTS events (
  seq     INTEGER PRIMARY KEY AUTOINCREMENT,
  stream  TEXT NOT NULL,
  kind    TEXT NOT NULL,
  ts      INTEGER NOT NULL,
  payload BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS events_by_stream ON events(stream, seq);
`

// AppendSQL is the writer's statement: one event, one implicit transaction,
// which is what an event append is.
const AppendSQL = `INSERT INTO events (stream, kind, ts, payload) VALUES (?, ?, ?, ?)`

// TailSQL is the subscriber's statement: everything after my cursor, in order,
// bounded. Every column is read, so column extraction counts toward the cost.
const TailSQL = `SELECT seq, kind, ts, payload FROM events WHERE seq > ? ORDER BY seq LIMIT 64`

// TailLimit matches the LIMIT in TailSQL.
const TailLimit = 64

// Pragmas are applied to every connection of every driver.
//
// Without this the comparison would be partly a comparison of compile-time
// defaults: the three drivers ship different journal modes, synchronous
// levels, and page cache sizes. Normalizing them is what makes the remaining
// difference attributable to the driver.
// busy_timeout comes first, and that ordering is load-bearing rather than
// cosmetic: switching a database into WAL mode takes a lock, so a connection
// opening against a database already under load fails immediately with
// SQLITE_BUSY unless it is already willing to wait. Measured, not theorized --
// with the timeout set last, one reader in eight failed to start under
// contention and its zero throughput read as starvation.
var Pragmas = []string{
	`PRAGMA busy_timeout=10000`,
	`PRAGMA journal_mode=WAL`,
	`PRAGMA synchronous=NORMAL`,
	`PRAGMA cache_size=-16000`, // 16 MiB per connection
	`PRAGMA foreign_keys=OFF`,
}

// Driver is one implementation under test.
type Driver interface {
	// Name identifies the driver in results. It includes the access shape
	// where a driver is measured through more than one.
	Name() string
	// Open returns a connection with Pragmas already applied.
	Open(path string) (DB, error)
}

// DB is a single connection. Connections are never shared between goroutines:
// the workload gives every worker its own, which is both what the direct
// drivers require and what a coordination daemon would do.
type DB interface {
	// SQLiteVersion reports the version of SQLite behind this driver.
	SQLiteVersion() (string, error)
	// Exec runs a statement that returns no rows.
	Exec(sql string, args ...any) error
	// QueryText runs a single-column query and returns the results rendered
	// as text, which is how the conformance corpus compares implementations.
	QueryText(sql string, args ...any) ([]string, error)
	// NewAppender prepares the writer statement.
	NewAppender() (Appender, error)
	// NewTailer prepares the subscriber statement.
	NewTailer() (Tailer, error)
	// RoundTrip stores a text and a blob value and reads both back, which is
	// the conformance check that no encoding step corrupted them.
	RoundTrip(key, text string, blob []byte) (gotText string, gotBlob []byte, err error)
	Close() error
}

// Appender appends one event.
type Appender interface {
	Append(stream, kind string, ts int64, payload []byte) error
	Close() error
}

// Tailer reads the events after a cursor, returning the new cursor and how
// many rows it saw.
type Tailer interface {
	Poll(after int64) (last int64, rows int, err error)
	Close() error
}

// verifyPragmas reads back the settings that decide what the benchmark is
// even measuring, and refuses a connection that did not take them.
//
// This is not defensiveness for its own sake. Journal mode governs whether
// readers and writers block each other, which is the entire subject of the
// contended workload; a connection quietly left in rollback-journal mode
// produces plausible numbers about a different question.
func verifyPragmas(db DB) error {
	checks := []struct {
		query string
		want  string
		why   string
	}{
		{`PRAGMA journal_mode`, "wal", "readers and writers only run concurrently in WAL mode"},
		{`PRAGMA synchronous`, "1", "durability level changes append cost by an order of magnitude"},
		{`PRAGMA busy_timeout`, "10000", "without a timeout, contention shows up as errors instead of latency"},
	}
	for _, c := range checks {
		got, err := db.QueryText(c.query)
		if err != nil {
			return fmt.Errorf("verifying %q: %w", c.query, err)
		}
		if len(got) == 0 {
			return fmt.Errorf("verifying %q: no result", c.query)
		}
		if !strings.EqualFold(got[0], c.want) {
			return fmt.Errorf("%s reports %q, want %q -- %s", c.query, got[0], c.want, c.why)
		}
	}
	return nil
}

// Drivers is every configuration under test, in a fixed order so results
// tables are stable.
func Drivers() []Driver {
	return []Driver{
		PuregoDriver{},
		NcrucesDriver{},
		SQLDriver{driverName: "sqlite3", label: "ncruces/go-sqlite3 (database/sql)"},
		SQLDriver{driverName: "sqlite", label: "modernc.org/sqlite (database/sql)"},
	}
}

// DriverByName finds a driver by its reported name.
func DriverByName(name string) (Driver, error) {
	for _, d := range Drivers() {
		if d.Name() == name {
			return d, nil
		}
	}
	return nil, fmt.Errorf("no driver named %q", name)
}

// RoundTripTable is the table RoundTrip uses. It is separate from events so a
// conformance run and a load run can share a database without interfering.
const RoundTripTable = `CREATE TABLE IF NOT EXISTS roundtrip (k TEXT PRIMARY KEY, t TEXT, b BLOB)`

const (
	roundTripInsert = `INSERT OR REPLACE INTO roundtrip (k, t, b) VALUES (?, ?, ?)`
	roundTripSelect = `SELECT t, b FROM roundtrip WHERE k = ?`
)

// SeedPayload builds the event payload used throughout: a fixed-size blob,
// large enough that row width matters and small enough that the benchmark is
// not measuring memcpy.
func SeedPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte('a' + i%26)
	}
	return p
}

// DefaultPayloadSize is the event payload size for all runs.
const DefaultPayloadSize = 256

// Now is the timestamp source for appends.
func Now() int64 { return time.Now().UnixMilli() }
