//go:build linux && amd64

package sqlite3

import _ "embed"

// The library is embedded rather than fetched or expected on the system, the
// same contract as darwin/arm64: a consumer's `go build` produces a binary that
// already contains the exact SQLite this repository pinned and compiled, with no
// install step and no chance of binding whatever libsqlite3 the host happens to
// have. Built by build/build-linux-amd64.sh; it links the build host's glibc,
// which is the one platform caveat the README records.
//
//go:embed lib/linux_amd64/libsqlite3.so
var embeddedLibrary []byte

const embeddedLibraryName = "libsqlite3.so"
