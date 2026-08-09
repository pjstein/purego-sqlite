# purego-sqlite

Real, upstream SQLite from Go, with no cgo anywhere — not in this package, and not in anything that imports it.

```go
import sqlite3 "github.com/pjstein/purego-sqlite"

conn, err := sqlite3.Open("app.db")
```

That is the whole install. No C toolchain, no `CGO_ENABLED=1`, no build tags, no system libsqlite3. The library it drives is SQLite's own amalgamation — pinned to a version and two checksums in this repository, compiled here with a configuration stated in exactly one file, and shipped inside the Go package as a platform shared library. Calls cross into it through [purego](https://github.com/ebitengine/purego).

```
go get github.com/pjstein/purego-sqlite
```

Requires Go 1.26.5 or newer — that is the version in `go.mod`, and with the default `GOTOOLCHAIN=auto` an older Go will fetch it rather than fail. Nothing else: no build tags, no `go generate`, no install step.

**Status: v0.1.0.** Proven on darwin/arm64 and linux/amd64, measured, and used in anger by exactly one project. The API is not frozen; see [API stability](#api-stability) before you depend on it.

## Why this exists

The pure-Go SQLite implementations — [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite), [`ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3) — are real engineering and they solve the cgo problem well. They also mean you are running a *translation* of SQLite rather than SQLite. This package takes the other route: ship the upstream C library per platform, bind it with purego, and let consumers stay cgo-free.

The thing you get that no pure-Go driver can give you is **the compile is yours**:

- **It is upstream SQLite.** The same query planner, the same WAL implementation, the same locking, the same bugs and the same fixes, on a version you chose and pinned.
- **You pick the compile-time flags.** FTS5, STAT4, `SQLITE_DQS=0`, math functions — set once in [`build/flags.sh`](build/flags.sh), each with the reason it is there. A pure-Go driver hands you whatever configuration its author transpiled.
- **You can compile extensions in.** Because this repository builds its own SQLite, it can build things alongside it. sqlite-vec is wired up as the worked example and ships in the prebuilt libraries. The session extension, or anything else with a C source file, is a pin and a `-D` away.
- **You can load extensions at runtime**, through SQLite's own dlopen path.
- **It is not the system library.** macOS ships Apple's fork of SQLite, on a version that changes with OS updates, compiled with settings you did not choose. This package never touches it.
- **No C toolchain downstream.** A consumer cross-compiles for five targets without thinking about it. Only *re-pinning* SQLite needs a compiler, and only on this repository's own machines.

What it costs, stated plainly, with numbers in [Measurements](#measurements):

- **A per-call foreign-function overhead cgo does not pay** — about 65ns against cgo's 20ns, plus one 208-byte heap allocation per call that cgo does not make. How much that costs depends entirely on calls per unit of work, which is why the row-batching shim below exists.
- **A bounded binding surface.** 71 SQLite entry points — 69 required, plus `sqlite3_enable_load_extension` and `sqlite3_load_extension` looked up optionally — all listed in one function in [`library.go`](library.go). Anything outside that surface does not exist until someone adds it.
- **A platform matrix maintained by hand.** Two targets today; see [Platforms](#platforms).
- **No `database/sql` driver yet.** See [What is not here](#what-is-not-here).

## Using it

```go
package main

import (
	"errors"
	"fmt"
	"log"

	sqlite3 "github.com/pjstein/purego-sqlite"
)

func main() {
	conn, err := sqlite3.Open("app.db")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if err := conn.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		log.Fatal(err)
	}
	if err := conn.Exec(`CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		log.Fatal(err)
	}

	// Prepare once, bind and run many times. PreparePersistent tells SQLite
	// the statement is worth keeping in its cache.
	ins, err := conn.PrepareFlags(`INSERT INTO events (payload) VALUES (?)`, sqlite3.PreparePersistent)
	if err != nil {
		log.Fatal(err)
	}
	defer ins.Close()

	for _, payload := range [][]byte{[]byte("alpha"), []byte("beta")} {
		if err := ins.Bind(payload); err != nil {
			log.Fatal(err)
		}
		if err := ins.Run(); err != nil { // step to completion, then reset
			log.Fatal(err)
		}
	}

	q, err := conn.Prepare(`SELECT seq, payload FROM events ORDER BY seq`)
	if err != nil {
		log.Fatal(err)
	}
	defer q.Close()

	for {
		row, err := q.Step()
		if err != nil {
			log.Fatal(err)
		}
		if !row {
			break
		}
		fmt.Printf("%d %s\n", q.ColumnInt64(0), q.ColumnBlob(1))
	}

	// Errors carry SQLite's own codes, primary and extended.
	err = conn.Exec(`INSERT INTO events (seq) VALUES (1), (1)`)
	fmt.Println(errors.Is(err, sqlite3.ConstraintPrimaryKey)) // true
}
```

Parameter indexes are 1-based and column indexes are 0-based, because that is what SQLite's documentation says. Translating one of them here would only mean translating it back to read the docs.

Full API documentation: [pkg.go.dev](https://pkg.go.dev/github.com/pjstein/purego-sqlite).

## Platforms

| Target | Prebuilt library | Tested in CI |
| --- | --- | --- |
| darwin/arm64 | `lib/darwin_arm64/libsqlite3.dylib` | yes |
| linux/amd64 | `lib/linux_amd64/libsqlite3.so` | yes |
| darwin/amd64 | not built | — |
| linux/arm64 | not built | — |
| windows/* | not built | — |

**On a platform with no prebuilt library, the package still compiles.** That is deliberate: a `go build` for an unsupported target must not fail, or the package would poison cross-compilation for programs that never reach a SQLite path on that target. It fails instead at the first call that needs the library, with `ErrUnsupportedPlatform`.

Two ways out, both without a rebuild of this repository:

```go
sqlite3.SetLibraryPath("/usr/lib/libsqlite3.so")   // or
// PUREGO_SQLITE_LIBRARY=/usr/lib/libsqlite3.so
```

A stock system libsqlite3 works correctly this way — the row-batching shim is absent, so reads take the slower per-call path, and `RowBatchingActive` reports `false`. Adding a real target is small: a `build/build-<goos>-<goarch>.sh`, a `lib/<goos>_<goarch>/` directory, and a build-tagged embed file. Nothing in the binding itself is platform-specific.

## Shipping binaries in the repository

`lib/darwin_arm64/libsqlite3.dylib` and `lib/linux_amd64/libsqlite3.so` are committed, compiled artifacts. That is unusual enough to justify.

**Why.** The entire value proposition is that a consumer needs no C toolchain. Any alternative — building on install, downloading at first run, a `go generate` step — hands the toolchain problem back to the consumer, or introduces a network dependency into their build. The library is embedded with `go:embed`, so `go get` and `go build` are the only steps there are, and the binary a consumer ships already contains the exact SQLite this repository pinned.

**What it costs, measured.** A full clone with all history is **5.9 MB working tree, 1.95 MiB packed** — shared libraries delta- and zlib-compress well, and five rebuilt versions of the dylib are already in that history. The module payload `go get` fetches is about **3.2 MB**, of which 3.0 MB is the two libraries (`bench/` is a separate module and is excluded from the zip). Your compiled binary grows by roughly **1.5 MB**, not 3 MB: `go:embed` is behind a build tag, so only your target's library is embedded.

This is small enough to ship as-is, and the recommendation is to ship it. The number worth watching is the *history*: every re-pin of SQLite adds another compressed copy of every platform's library, permanently. At two platforms and a few pins a year that is noise. If the matrix grows to five or six platforms and the pin moves monthly, move the libraries to release assets with a download-and-verify step — but do that when the number says to, not before.

**How they were built.** By `make ship-lib` on a machine of the target platform, which is `WITH_SQLITE_VEC=1 make build` plus a manifest regeneration. That fetches the pinned amalgamation from sqlite.org, verifies both its SHA256 and the SHA3-256 that sqlite.org's own download manifest publishes, fetches and verifies sqlite-vec against its pin, and compiles them with `build/shim.c` under the flags in `build/flags.sh`. Every input is committed; only the compiler is not.

**How to verify what you got.**

```
shasum -a 256 lib/darwin_arm64/libsqlite3.dylib   # compare against lib/MANIFEST.txt
go test -run TestManifestMatchesTheCommittedLibraries ./...
```

[`lib/MANIFEST.txt`](lib/MANIFEST.txt) records the size and SHA256 of each committed library alongside the pins it was built from, and a test in the suite fails if the two ever drift. At runtime, `TestShippedLibraryIsThePinnedUpstreamOne` fails if the loaded library does not report the version in `sqlite-version.lock`, and `sqlite3.Version()`, `SourceID()`, and `CompiledExtensions()` let you ask a running program the same questions.

What the manifest does *not* claim is bit-for-bit reproducibility from a different toolchain. Two clang versions do not emit an identical dylib from identical input, and asserting they would produces a failure that means nothing. The reproducible artifacts are the *inputs*: the amalgamation and its two upstream hashes, the extension pin and its hash, and the complete flag list.

**Building from source instead.**

```
make build      # fetch (checksum-verified) and compile for the host, no extensions
make ship-lib   # the same, in the configuration committed under lib/
make check      # gofmt, go vet, and the suite on both read paths
```

## Row batching

Reading a four-column row through SQLite's C API takes seven calls: one step, one accessor per column, and a second call per text or blob column to learn its length. That is free through cgo and expensive through purego.

So the library this repository compiles also carries a small shim ([`build/shim.c`](build/shim.c)) that does a whole row inside C and returns once. It adds no SQLite behaviour — it calls the same public API in the same order — and it collapses seven crossings per row into one. Measured on the read path here, that is **3× faster** and cuts allocations per 64-row page from **1158 to 263**, which brings the binding within about **3.5%** of a cgo binding making the same single call per row. See [Measurements](#measurements).

Two properties make it safe to rely on:

- **It is optional.** The binding looks the symbol up at load and falls back to per-call accessors when it is absent, so pointing the package at a stock system libsqlite3 still works correctly — only slower. `RowBatchingActive` reports which path is in use.
- **It never converts.** The shim returns each value in the representation SQLite stored it as. Ask for a column as some other type and the binding goes back to SQLite for the answer, because SQLite's conversion rules are its own and reimplementing them in Go would be a source of quiet disagreement.

The suite runs on both paths, and an internal test reads the same rows both ways and requires every reading to be identical.

## Extensions

Three ways to add SQL that SQLite does not ship with, in descending order of how much they cost you.

**Compile one in.** sqlite-vec is the worked example: pinned by checksum in [`build/extensions.lock`](build/extensions.lock) and **compiled into the prebuilt libraries this repository ships**, so vector search works out of the box.

```go
exts, _ := sqlite3.CompiledExtensions()   // [sqlite-vec]
conn.Exec(`CREATE VIRTUAL TABLE v USING vec0(embedding float[4])`)
```

Note the asymmetry, because it surprises people reading the build scripts: `make build` compiles **without** extensions — that is the source-build default — while the libraries **committed** under `lib/` were built with `make ship-lib`, which turns sqlite-vec on. The package feature-detects which shape of library it opened, so one Go package works against both; `CompiledExtensions()` is the authoritative answer for a given process. Registration runs through `sqlite3_auto_extension`, so a compiled-in extension is present on every connection rather than just the first.

Adding another extension means pinning it in `build/extensions.lock`, adding a case to `build/fetch-extensions.sh`, adding its init call to `build/extensions.c`, and adding a test that proves it answers a query rather than merely links.

**Load one at runtime.** `Conn.EnableLoadExtension` then `Conn.LoadExtension` bind SQLite's own dlopen path. Loading is refused until you ask for it, and enabling the C API deliberately does *not* enable SQL's `load_extension()` — a SQL string that can load a shared library is a much larger blast radius than a Go call that can. Note that a loadable extension must route every call through the `sqlite3_api` routines table; one that references libsqlite3's symbols directly will link against nothing and jump to a null pointer on first use.

**Write one in Go.** `Conn.CreateFunction` registers a Go closure as a SQL scalar function:

```go
err := conn.CreateFunction("go_abs", 1, sqlite3.Deterministic,
    func(ctx *sqlite3.FuncContext, args []sqlite3.Value) {
        v := args[0].Int64()
        if v < 0 { v = -v }
        ctx.ResultInt64(v)
    })
```

A panic inside the function becomes a SQL error rather than unwinding into C and killing the process, and every registered function shares one purego callback — purego can mint only about 2000 C function pointers per process and never reclaims one.

**Know what it costs, and prefer an aggregate.** Answering a SQL *scalar* in Go costs **245 ± 7 ns** per invocation against **26 ns** for SQLite's own C equivalent. Most of that is a floor rather than slack: a bare purego callback from C costs **175 ns** before your function does anything, because purego's callback path takes a process-wide mutex and dispatches through reflection. There is no faster door.

So for anything per-row, use `CreateAggregate` instead. It accumulates in C and crosses to Go once per group:

```go
err := conn.CreateAggregate("go_total", 1, 0,
    func(ctx *sqlite3.FuncContext, acc sqlite3.Accumulation) {
        if acc.AllInt {
            ctx.ResultInt64(acc.IntSum)
            return
        }
        ctx.ResultFloat(acc.Sum)
    })
```

That measures **23.7 ± 0.3 ns per row** — faster than SQLite's own `abs()`, because 999 crossings in every 1000 stop happening. What C can summarise is deliberately narrow (count, sum, min, max, an exact integer sum while inputs stay integral); an aggregate needing every individual row is an ordinary query with a loop around it, and the loop is cheaper.

## Measurements

**Every number here is darwin/arm64 on one machine** — an Apple M2 Max, 12 cores, Go 1.26.5, SQLite 3.53.4. They are not portable claims, and linux/amd64 has not been measured. Treat them as evidence that the approach is sound and the shim earns its keep, not as a performance guarantee for your workload.

Contended read throughput — 8 concurrent readers against a writer held at 2000 appends/s, 8 interleaved rounds inside a quiet window the harness verified, mean ± sample stddev ([raw](bench/results/contended-fixed-2000-quiet.txt)):

| Driver | median polls/s | p99 latency |
| --- | --- | --- |
| purego-sqlite (this package) | 14327 ± 1325 | 332 ± 37 µs |
| ncruces/go-sqlite3 (native API) | 14515 ± 3517 | 504 ± 442 µs |
| ncruces/go-sqlite3 (database/sql) | 10787 ± 1325 | 452 ± 79 µs |
| modernc.org/sqlite (database/sql) | 3816 ± 586 | 2622 ± 649 µs |

Read the spread before the ranking. The harness marks any figure whose stddev exceeds 10% of its mean and says so in the transcript, and several of these are marked — ncruces' native row in particular varies by ±24%. The defensible conclusion is that this package and the fastest pure-Go driver are **indistinguishable at this sample size**, and that both are roughly 3.8× `modernc.org/sqlite` through `database/sql`. Anything finer than that is not in the data.

Single-reader row scan, median of 5 interleaved repetitions, including a cgo binding driving *the same shipped library through the same shim* as the control ([raw](bench/results/micro-poll.txt)):

| Driver | ns/row | allocs/op |
| --- | --- | --- |
| cgo, same library, batched | 273 | 127 |
| **purego-sqlite, batched** | **283** | **263** |
| cgo, same library, per-call | 338 | 127 |
| ncruces/go-sqlite3 (native) | 339 | 63 |
| ncruces/go-sqlite3 (database/sql) | 616 | 469 |
| modernc.org/sqlite (database/sql) | 634 | 471 |
| purego-sqlite, batching disabled | 853 | 1158 |

The comparison that matters is the first two rows: against cgo over the identical library, through the identical shim, batched reads land about **3.5%** behind. That isolates the calling convention from the SQLite implementation, which comparing against a pure-Go driver cannot do. The last row is what the same code costs without the shim — **3× slower and 4.4× the allocations**, which is the whole argument for the shim existing.

Component costs: a purego call is **65 ns** and one 208-byte allocation against cgo's **20 ns** and none. A Go scalar SQL function is **245 ± 7 ns** per call on a **175 ns** callback floor; a batched aggregate is **23.7 ± 0.3 ns** per row.

**Raw results, and how they were produced**, are committed under [`bench/`](bench/) — every JSON and every console transcript, not a summary. [`bench/results/environment.txt`](bench/results/environment.txt) records the host, the toolchain, the pins, and the machine's load at the start and end of the run. The harness rotates driver order between rounds, refuses to start into a populated results directory, gates on a quiet machine, and fails the run rather than publishing if row batching was not actually on. See [`bench/README.md`](bench/README.md) for how to re-run it. Paths in those transcripts are the measurement host's absolute paths, kept verbatim rather than tidied, because a transcript you have edited is not a transcript.

## Replication

Litestream works on the database *files*, so it is driver-agnostic in principle and verified in practice: `bench/run-litestream-smoke.sh` replicates a database being written through this binding to a local file replica, restores it, and checks the restored copy — integrity clean, identical row and byte counts, and the whole conformance corpus intact.

Litestream v0.5 as a *library* is cgo-free in the paths that compile (its `go.mod` mentions `mattn/go-sqlite3`, but only a transitive dependency's test binary reaches it), so it composes with this package without giving up `CGO_ENABLED=0`. It does embed `modernc.org/sqlite`, so a process using both carries two SQLite implementations; running the litestream binary as a sidecar avoids that.

## Concurrency

The shipped library is compiled `SQLITE_THREADSAFE=2` (multi-thread), and this package inherits exactly that contract:

- A `Conn`, and every `Stmt` prepared on it, belongs to one goroutine at a time. Serialize it yourself, or give each goroutine its own connection.
- Separate `Conn` values are safe to use concurrently against the same database file. In WAL mode, readers do not block the writer and the writer does not block readers.

This is deliberately not hidden behind an internal mutex. A connection pool is a policy — how many, when to open, what to do under contention — and it belongs to the application, not to a binding. `Conn.Interrupt` is the one method that is safe to call from another goroutine, which is what makes cancellation possible.

## What is not here

Named so nobody has to discover it by reading source:

- **No `database/sql` driver.** The direct API is the interface. A driver would add row-scanning conversions, a connection pool, and `context` plumbing on top of what is here — worth doing, and intended as an additive subpackage rather than as the thing you have to go through. Until it exists, code written against this package cannot be swapped for another driver by changing an import.
- **No hooks.** No `update_hook`, `commit_hook`, or authorizer. Scalar functions and batched aggregates *are* here (see [Extensions](#extensions)); window functions and custom collations are not.
- **No `time.Time` binding.** SQLite stores time as whatever the application decides — Unix seconds, Julian day, ISO-8601 text. A binding that picked one would be picking your schema.
- **No incremental blob I/O**, and no `sqlite3_serialize`/`deserialize`. Online backup is here; those are not.
- **No session extension yet.** The compile-your-own path makes it reachable — that is much of why this repository exists — but it is not wired up.

## Tests

```
go test ./...                                  # batched read path
PUREGO_SQLITE_BATCH=0 go test ./...            # per-call read path
PUREGO_SQLITE_FUNCTIONS=portable go test ./... # portable function dispatch
```

The suite covers the binding surface against the real library: round-trips of every scalar type, WAL mode and checkpointing, concurrent readers under a sustained writer, busy-timeout behaviour at sub-second resolution, the online backup API, and the error paths including extended result codes.

It must be run all three ways, because those are genuinely different code paths. `TestRowBatchingIsActive` fails rather than skips if the shipped library has lost its shim, so the fast path cannot quietly go untested.

`conformance/` is a shared corpus with no dependency on the binding: values that must round-trip byte-for-byte (embedded NUL, real newlines, astral-plane characters, invalid UTF-8, all 256 byte values) and SQL expressions whose results every implementation must agree on. The bake-off under `bench/` runs the same corpus through competing drivers, which is what makes "they agree" a checkable claim rather than an assumption.

CI runs the suite on darwin/arm64 and linux/amd64 against the committed libraries, and separately rebuilds the library from pinned source on both platforms so that a build script which works only on its author's machine fails visibly.

## API stability

Pre-1.0, and honest about it:

- **Within v0.x, minor versions may break the API.** Pin an exact version; `go.mod` does this by default.
- **What is least likely to move**: `Open`/`Close`, `Prepare`/`Bind`/`Step`/`Run`, the column accessors, and the error codes. These are shaped by SQLite's own API and there is not much freedom to redesign them.
- **What is most likely to move**: the extension and user-function surface (`CreateFunction`, `CreateAggregate`, `Accumulation`), which is the newest code here and the least exercised by real use.
- **What will be additive**: a `database/sql` driver, if it lands, arrives as a subpackage. It will not change this API.
- **The shipped SQLite version can change in a patch release.** The pin is recorded in `sqlite-version.lock` and in the changelog; a re-pin is a behaviour change to your program even when no Go signature moves.

v1.0 waits on a second real consumer and on linux/amd64 measurements. See [CHANGELOG.md](CHANGELOG.md).

## License and attribution

This repository's own code is **MIT** — see [LICENSE](LICENSE). MIT because it is the Go ecosystem's default, it imposes nothing on consumers of a database binding, and it is compatible with everything bundled here.

The compiled libraries under `lib/` contain third-party code, and [**NOTICE**](NOTICE) states component by component what it is, where it came from, what license it carries, and whether it was modified. In short:

| Component | License | Shipped in `lib/`? |
| --- | --- | --- |
| SQLite 3.53.4 amalgamation | Public domain | yes, unmodified |
| [sqlite-vec](https://github.com/asg017/sqlite-vec) 0.1.9 (Alex Garcia) | Apache-2.0 OR MIT; MIT elected here | yes, unmodified |
| [ebitengine/purego](https://github.com/ebitengine/purego) v0.10.2 | Apache-2.0 | no — an ordinary Go module dependency |
| `build/shim.c`, `build/extensions.c` | MIT (this repository) | yes |

License texts for the bundled components are under [`third_party/`](third_party/).

The shim is worth stating precisely, since "derived from SQLite" would be wrong: `build/shim.c` contains no SQLite source. It declares the prototypes of the eight public `sqlite3_*` entry points it calls and calls them in the order any caller would. It is a consumer of SQLite's published C API that happens to be compiled into the same shared library, so that it can make those calls without a boundary crossing of its own.
