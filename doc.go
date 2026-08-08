// Package sqlite3 is a cgo-free binding to real, upstream SQLite.
//
// The library it drives is upstream's own amalgamation, pinned to a version
// and a checksum in this repository, compiled here with a configuration this
// repository states in one place (build/flags.sh), and shipped as a platform
// dylib embedded in the Go package. It is never the operating system's
// libsqlite3: on macOS that is Apple's fork on a version that moves underneath
// you with OS updates.
//
// Calls cross into C through [github.com/ebitengine/purego], so no consumer of
// this package needs a C toolchain, and CGO_ENABLED=0 builds and
// cross-compiles work normally. The cost is a per-call foreign-function
// overhead that cgo does not pay; see the benchmarks under bench/ for what
// that costs at a given call rate.
//
// # Concurrency
//
// The shipped library is compiled SQLITE_THREADSAFE=2 (multi-thread). SQLite's
// own mutexes protect its global structures, but a connection object is not
// guarded, so this package inherits exactly that contract:
//
//   - A [Conn], and every [Stmt] prepared on it, must not be used by more than
//     one goroutine at a time. Serialize them yourself, or give each goroutine
//     its own connection.
//   - Separate [Conn] values may be used concurrently from separate
//     goroutines, including against the same database file. This is the
//     intended shape: one connection per worker, WAL mode, readers that do not
//     block the writer.
//
// # Getting started
//
//	conn, err := sqlite3.Open("app.db")
//	if err != nil {
//		return err
//	}
//	defer conn.Close()
//
//	if err := conn.Exec(`PRAGMA journal_mode=WAL`); err != nil {
//		return err
//	}
//	if err := conn.Exec(`CREATE TABLE IF NOT EXISTS t (k TEXT, v BLOB)`); err != nil {
//		return err
//	}
//	if err := conn.Exec(`INSERT INTO t VALUES (?, ?)`, "key", []byte{0x00, 0x01}); err != nil {
//		return err
//	}
//
// This package deliberately does not implement database/sql's driver
// interfaces in v1; see the README for why and for what that would add.
package sqlite3
