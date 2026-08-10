//go:build !((darwin || freebsd || linux || netbsd) && !android && !faketime)

package sqlite3

import "errors"

// This platform has no dlfcn, so there is no way to open a shared library or
// resolve a symbol in one. The package still compiles — a consumer can
// cross-compile a binary whose SQLite paths are unreachable, which is the same
// promise library_unsupported.go makes about the missing shipped library — and
// every entry point fails with a sentence that says why rather than a link
// error that says where.
var errNoDynamicLoading = errors.New(
	"purego-sqlite: this platform has no dynamic loading (dlopen/dlsym), so the SQLite " +
		"library cannot be opened; the package compiles here so that cross-compilation works, " +
		"but no database can be opened on it")

func dlopen(path string, mode int) (uintptr, error) { return 0, errNoDynamicLoading }

func dlsym(handle uintptr, name string) (uintptr, error) { return 0, errNoDynamicLoading }

const (
	rtldNow    = 0
	rtldGlobal = 0
)
