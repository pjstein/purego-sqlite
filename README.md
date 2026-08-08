# purego-sqlite

Real, upstream SQLite from Go, with no cgo anywhere — not in this package, and not in anything that imports it.

The library is SQLite's own amalgamation, pinned to a version and checksum in this repository, compiled here with a configuration stated in one place, and embedded in the Go package as a platform shared library. Calls cross into it through [purego](https://github.com/ebitengine/purego). Consumers get `CGO_ENABLED=0` builds, working cross-compilation, and C SQLite's actual performance and locking semantics.

> **Working name.** The module path `github.com/pjstein/purego-sqlite` is a placeholder. The repository name, module path, and package name are all renameable before first publish; nothing outside `go.mod` and the import lines depends on them. The MIT license file is likewise a default, not a decision.

## Why this exists

The pure-Go SQLite implementations (`modernc.org/sqlite`, `ncruces/go-sqlite3`) are real engineering and they solve the cgo problem. They also mean you are running a translation of SQLite rather than SQLite. This package takes the other route, the one [yzma and kronk](https://github.com/hybridgroup) take for llama.cpp: ship the upstream C library per platform, bind it with purego, and let consumers stay cgo-free.

What that buys:

- **It is upstream SQLite.** Same query planner, same WAL implementation, same locking, same bugs and same fixes, on a version you pinned.
- **No C toolchain downstream.** A consumer cross-compiles for five targets without thinking about it. Only re-pinning SQLite needs a compiler, and only on this repository's own machine.
- **Not the system library.** macOS ships Apple's fork of SQLite, on a version that changes with OS updates, compiled with settings you did not choose. This package never touches it.

What it costs, stated plainly:

- **A per-call foreign-function overhead that cgo does not pay.** A call through purego costs about 65ns against cgo's 20ns on this machine, and — the part that matters more under concurrency — one 208-byte heap allocation that cgo does not make. How much that costs you depends entirely on how many calls per unit of work you make; see "Row batching" below and the benchmarks in `bench/`.
- **A bounded binding surface.** 55 SQLite entry points, listed in `library.go`, plus two for the optional row-batching shim. Anything outside that surface does not exist until someone adds it.
- **A platform matrix this repository maintains by hand.** Today: darwin/arm64.

## Using it

```go
import sqlite3 "github.com/pjstein/purego-sqlite"

conn, err := sqlite3.Open("app.db")
if err != nil {
    return err
}
defer conn.Close()

if err := conn.Exec(`PRAGMA journal_mode=WAL`); err != nil {
    return err
}
if err := conn.Exec(`CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
    return err
}

ins, err := conn.PrepareFlags(`INSERT INTO events (payload) VALUES (?)`, sqlite3.PreparePersistent)
if err != nil {
    return err
}
defer ins.Close()

for _, payload := range payloads {
    if err := ins.Bind(payload); err != nil {
        return err
    }
    if err := ins.Run(); err != nil { // step to completion, then reset
        return err
    }
}
```

Reading:

```go
q, err := conn.Prepare(`SELECT seq, payload FROM events WHERE seq > ? ORDER BY seq`)
if err != nil {
    return err
}
defer q.Close()

if err := q.Bind(cursor); err != nil {
    return err
}
for {
    row, err := q.Step()
    if err != nil {
        return err
    }
    if !row {
        break
    }
    seq, payload := q.ColumnInt64(0), q.ColumnBlob(1)
    // ...
}
```

Errors carry SQLite's own codes, and `errors.Is` works against them:

```go
if errors.Is(err, sqlite3.Busy) { /* retry */ }
if errors.Is(err, sqlite3.ConstraintUnique) { /* already have it */ }
```

Parameter indexes are 1-based and column indexes are 0-based, because that is what SQLite's documentation says. Translating one of them here would only mean translating it back to read the docs.

## Row batching

Reading a four-column row through SQLite's C API takes seven calls: one step, one accessor per column, and a second call per text or blob column to learn its length. That is free through cgo and expensive through purego.

So the library this repository compiles also carries a small shim (`build/shim.c`) that does a whole row inside C and returns once. It adds no SQLite behaviour — it calls the same public API in the same order — and it collapses seven crossings per row into one. Measured on the read path here, that is 2.9× faster and cuts allocations per 64-row page from 1158 to 263, which brings the binding within about 3% of a cgo binding making the same single call per row.

Two properties make it safe to rely on:

- **It is optional.** The binding looks the symbol up at load and falls back to per-call accessors when it is absent, so pointing the package at a stock system libsqlite3 still works correctly — only slower. `RowBatchingActive` reports which path is in use.
- **It never converts.** The shim returns each value in the representation SQLite stored it as. Ask for a column as some other type and the binding goes back to SQLite for the answer, because SQLite's conversion rules are its own and reimplementing them in Go would be a source of quiet disagreement.

The suite runs on both paths, and an internal test reads the same rows both ways and requires every reading to be identical:

```
go test ./...
PUREGO_SQLITE_BATCH=0 go test ./...
```

## Concurrency

The shipped library is compiled `SQLITE_THREADSAFE=2` (multi-thread), and this package inherits exactly that contract:

- A `Conn`, and every `Stmt` prepared on it, belongs to one goroutine at a time. Serialize it yourself, or give each goroutine its own connection.
- Separate `Conn` values are safe to use concurrently against the same database file. In WAL mode, readers do not block the writer and the writer does not block readers.

This is deliberately not hidden behind an internal mutex. A connection pool is a policy — how many, when to open, what to do under contention — and it belongs to the application, not to a binding. `Conn.Interrupt` is the one method that is safe to call from another goroutine, which is what makes cancellation possible.

## What is not here

Named so nobody has to discover it by reading source:

- **No `database/sql` driver.** v1 is the direct API. The driver interfaces would add row-scanning conversions, a connection pool, and `context` plumbing on top of what is here — worth doing, but as an additive subpackage, not as the thing you have to go through.
- **No callbacks or hooks.** No `update_hook`, `commit_hook`, authorizer, custom SQL functions, or custom collations. purego can express them (`purego.NewCallback`), but a Go function called from C on a SQLite-owned thread is a different set of rules, and the surface is bounded until something needs them.
- **No `time.Time` binding.** SQLite stores time as whatever the application decides — Unix seconds, Julian day, ISO-8601 text. A binding that picked one would be picking your schema.
- **No incremental blob I/O**, and no `sqlite3_serialize`/`deserialize`. Backup is here; those are not.

## Platforms

| Target | Status |
| --- | --- |
| darwin/arm64 | shipped, tested |
| darwin/amd64 | not built |
| linux/amd64 | not built |
| linux/arm64 | not built |

The layout is already per-platform: a target needs `build/build-<goos>-<goarch>.sh`, a `lib/<goos>_<goarch>/` directory, and a build-tagged embed file. Nothing in the binding is platform-specific.

On a platform with no shipped library, the package still builds and fails on first use with `ErrUnsupportedPlatform`. Point it at a system library with `SetLibraryPath` or the `PUREGO_SQLITE_LIBRARY` environment variable if you would rather manage the shared library yourself.

## Building and re-pinning SQLite

```
make build   # fetch (checksum-verified) and compile for the host platform
make test    # go test ./...
make bench   # the three-way driver bake-off under bench/
```

The pin lives in `sqlite-version.lock`: version, URL, SHA256, and the SHA3-256 that sqlite.org publishes in its own download manifest. `build/fetch.sh` verifies both before unpacking. The compile flags live in `build/flags.sh`, each with the reason it is there — that file is the only place they are stated.

To move to a new SQLite: edit the four values in `sqlite-version.lock`, run `make build test`, commit the new dylib. `TestShippedLibraryIsThePinnedUpstreamOne` fails if the lock file and the shipped binary ever disagree.

## Tests

`go test ./...` covers the binding surface against the real library: round-trips of every scalar type, WAL mode and checkpointing, concurrent readers under a sustained writer, busy-timeout behaviour at sub-second resolution, the online backup API, and the error paths including extended result codes.

The suite must be run both ways — plain, and with `PUREGO_SQLITE_BATCH=0` — because the batched and per-call read paths are different code. `TestRowBatchingIsActive` fails rather than skips if the shipped library has lost its shim, so the fast path cannot quietly go untested.

`conformance/` is a shared corpus with no dependency on the binding: values that must round-trip byte-for-byte (embedded NUL, real newlines, astral-plane characters, invalid UTF-8, all 256 byte values) and SQL expressions whose results every implementation must agree on. The bake-off under `bench/` runs the same corpus through competing drivers, which is what makes "they agree" a checkable claim rather than an assumption.

## License

MIT — see LICENSE. Placeholder pending a decision before publication.
