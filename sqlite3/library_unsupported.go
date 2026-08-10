//go:build !(darwin && arm64) && !(linux && amd64)

package sqlite3

// No compiled SQLite is shipped for this platform yet. The package still
// builds -- so consumers can cross-compile a binary whose SQLite paths are
// unreachable on that target -- and fails with [ErrUnsupportedPlatform] on
// first use. Setting LibraryPathEnv or calling [SetLibraryPath] makes it work
// against a system-provided library.
var embeddedLibrary []byte

const embeddedLibraryName = ""
