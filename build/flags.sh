#!/usr/bin/env bash
# Compile-time configuration for the SQLite we ship.
#
# Every flag here is a promise to consumers of the shipped library: it changes
# what SQL works, what durability means, and how the library behaves under
# concurrency. Each one carries its reason. Removing a flag is a breaking
# change to the library's behaviour even when the Go API is untouched.
#
# Sourced by build-<platform>.sh. Do not duplicate this list anywhere else.

SQLITE_DEFINES=(
  # Multi-thread mode: SQLite's own mutexes protect global and per-cache
  # structures, but individual connection objects are left unguarded, so two
  # threads must never touch one connection at once. That is exactly the
  # contract this package enforces in Go with a per-Conn mutex. Mode 1
  # (serialized) would buy nothing and add a mutex acquire to every API call;
  # mode 0 would make the library unusable from more than one goroutine.
  -DSQLITE_THREADSAFE=2

  # Full-text search. A coordination daemon accumulates an event log and notes
  # that people and agents want to search by content, not only by key.
  -DSQLITE_ENABLE_FTS5

  # JSON functions. This is a no-op on 3.38.0 and later, where JSON is part of
  # the core build; it is kept as an explicit statement of intent so that the
  # requirement survives a re-pin onto an older or differently-configured
  # amalgamation, where its absence would be silent.
  -DSQLITE_ENABLE_JSON1

  # Histogram statistics for the query planner. Coordination queries filter on
  # skewed columns -- status, kind, owner -- where the default single-sample
  # stats mislead the planner into the wrong index.
  -DSQLITE_ENABLE_STAT4

  # synchronous=NORMAL by default in WAL mode: fsync at checkpoint rather than
  # at every commit. In WAL mode this can only lose the most recent
  # transactions on power loss, never corrupt the database. Appropriate where
  # the database is a coordination cache and durable truth lives elsewhere;
  # consumers who need FULL can still set it per connection with a pragma.
  #
  # Worth knowing, because it surprises: this applies to a connection that
  # OPENS a database already in WAL mode, which SQLite reads from the file
  # header. The connection that converts a database to WAL keeps whatever
  # synchronous level it opened with. Both cases are covered by the suite.
  -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1

  # Without this SQLite's busy handler sleeps in whole seconds, which turns a
  # 50ms busy_timeout into a 1s stall and makes contention benchmarks lie.
  # Not a default: the amalgamation cannot detect usleep() on its own.
  -DHAVE_USLEEP=1

  # Reject double-quoted string literals in both DDL and DML. SQLite's own
  # documentation calls this misfeature a bug-compatibility shim; leaving it on
  # means a typo'd column name silently becomes a string constant.
  -DSQLITE_DQS=0

  # ceil/floor/pow/log and friends as SQL functions. Cheap, and their absence
  # is the kind of thing found at the worst moment.
  -DSQLITE_ENABLE_MATH_FUNCTIONS

  # Do not track allocation statistics. They cost an atomic on every malloc and
  # free, and nothing in this package reads them.
  -DSQLITE_DEFAULT_MEMSTATUS=0

  # Use alloca() for small scratch allocations rather than the general
  # allocator. Recommended upstream for performance; safe on the platforms we
  # target, all of which have a real alloca.
  -DSQLITE_USE_ALLOCA

  # LIKE never matches a blob operand. Upstream-recommended: it lets the
  # optimizer use an index for LIKE, and blob-vs-LIKE is not meaningful.
  -DSQLITE_LIKE_DOESNT_MATCH_BLOBS

  # Remove the recursive expression-depth check. Upstream-recommended for
  # performance; the parser's other limits still bound memory.
  -DSQLITE_MAX_EXPR_DEPTH=0

  # Deliberately NOT set, recorded so the decision is visible:
  #   SQLITE_OMIT_LOAD_EXTENSION -- extension loading stays compiled in
  #     (still disabled for SQL by default) so consumers can opt in.
  #   SQLITE_ENABLE_RTREE / GEOPOLY -- no spatial workload; pure size.
  #   SQLITE_OMIT_DEPRECATED -- would shrink the library, but consumers of a
  #     shared dylib may reasonably reach for older entry points.
)

SQLITE_CFLAGS=(
  -O2
  -fPIC
  -DNDEBUG
)
