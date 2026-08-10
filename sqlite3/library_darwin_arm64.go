//go:build darwin && arm64

package sqlite3

import _ "embed"

// The library is embedded rather than fetched or expected on the system: a
// consumer's `go build` produces a binary that already contains the exact
// SQLite this repository pinned and compiled, with no install step and no
// chance of binding whatever libsqlite3 the host happens to have.
//
//go:embed lib/darwin_arm64/libsqlite3.dylib
var embeddedLibrary []byte

const embeddedLibraryName = "libsqlite3.dylib"
