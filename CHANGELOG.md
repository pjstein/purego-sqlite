# Changelog

Notable changes per release. Dates are ISO-8601. This project follows
[semantic versioning](https://semver.org/) with the pre-1.0 caveat that minor
versions may break the API; see [API stability](README.md#api-stability).

## v0.1.0 — 2026-08-10

First public release. Below is a deliberately blunt account of what is proven
and what is not, because a 0.1.0 that oversells itself costs more than one that
under-promises.

### What is here

- A direct binding to upstream SQLite through
  [purego](https://github.com/ebitengine/purego), with no cgo required of
  consumers. 71 SQLite entry points, all listed in one function in `library.go`.
- **SQLite 3.53.4**, pinned by SHA256 and by the SHA3-256 that sqlite.org's own
  download manifest publishes, fetched and verified by `build/fetch.sh`,
  compiled here under the flags in `build/flags.sh`.
- **Prebuilt libraries for darwin/arm64 and linux/amd64**, committed under
  `lib/` and embedded with `go:embed`, with sizes and hashes recorded in
  `sqlite3/lib/MANIFEST.txt` and a test that fails when the two drift.
- **A row-batching shim** (`build/shim.c`) that fetches a whole result row in
  one foreign call instead of seven. Optional and feature-detected, so a stock
  system libsqlite3 still works — only slower.
- **sqlite-vec 0.1.9 compiled into the shipped libraries**, pinned by checksum,
  with `CompiledExtensions()` reporting at runtime what a given library carries.
- **Runtime extension loading** through SQLite's own dlopen path, refused until
  explicitly enabled, and deliberately not enabling SQL's `load_extension()`
  along with the C API.
- **Go-defined SQL functions**: scalar functions via `CreateFunction` and
  batched aggregates via `CreateAggregate`, with panics converted to SQL errors
  rather than unwinding into C.
- Online backup, WAL checkpointing, interrupt, busy-timeout at sub-second
  resolution, and SQLite's primary and extended result codes wired to
  `errors.Is`.
- A conformance corpus (`conformance/`) with no dependency on the binding,
  driven through this package and through competing drivers alike.

### What is proven

- The suite passes on darwin/arm64 and linux/amd64, on the batched read path,
  the per-call read path, and the portable function-dispatch path.
- The shipped library is verified at test time to be the pinned upstream
  version, compiled `SQLITE_THREADSAFE=2`, with the shim present — the last of
  these fails rather than skips, so the fast path cannot quietly go untested.
- Litestream replicates and restores a database written through this binding,
  with the restored copy verified byte-for-byte against the corpus.
- The performance claims in the README are derivable from raw files committed
  under `bench/results/`, produced by a harness that rotates driver order,
  gates on a quiet machine, and refuses to publish a run whose configuration
  was not what it claimed.

### What is not proven

- **All measurements are darwin/arm64 on one machine.** linux/amd64 is built
  and tested but has never been benchmarked.
- **One real consumer.** The API has been shaped by a single project's needs.
  Expect the parts it did not exercise — particularly the user-function and
  extension surface — to have rough edges.
- **The library binaries are not bit-for-bit reproducible** across toolchains,
  and the manifest does not claim they are. The reproducible artifacts are the
  inputs: the amalgamation and its two hashes, the extension pin, the flags.
- **No `database/sql` driver**, so code written against this package cannot be
  swapped for another driver by changing an import.
- **No hooks, no window functions, no custom collations, no incremental blob
  I/O, no `serialize`/`deserialize`, no session extension.**

### Known limits

- A purego call costs about 65ns and one 208-byte allocation against cgo's
  20ns and none. The row-batching shim exists to make that irrelevant on the
  read path; it is still there on every other path.
- A Go scalar SQL function costs 245 ± 7 ns per invocation on a 175 ns purego
  callback floor. Use `CreateAggregate` for anything per-row.
- purego mints roughly 2000 C function pointers per process and never reclaims
  one. All registered SQL functions share a single callback, so this is a
  ceiling on the process rather than on the number of functions, but it is a
  real ceiling.
- The linux/amd64 library links the build host's glibc.
- `OpenNoMutex` and `OpenFullMutex` are accepted and inert: the shipped library
  is compiled `SQLITE_THREADSAFE=2`, in which SQLite honours neither.

### Licensing

MIT for this repository's own code. The shipped libraries contain SQLite
(public domain) and sqlite-vec (Apache-2.0 OR MIT, MIT elected here); purego is
an Apache-2.0 Go dependency. See [NOTICE](NOTICE) for the component-by-component
statement and `third_party/` for the license texts.
