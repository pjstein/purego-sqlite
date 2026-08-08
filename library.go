package sqlite3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ebitengine/purego"
)

// ErrUnsupportedPlatform is returned when the package is built for a platform
// this repository does not yet ship a compiled SQLite for. Adding one means
// adding build/build-<goos>-<goarch>.sh, lib/<goos>_<goarch>/, and the
// matching embed file -- nothing in this file needs to change.
var ErrUnsupportedPlatform = errors.New("purego-sqlite: no SQLite library is shipped for " + runtimePlatform)

// LibraryPathEnv names an environment variable that, when set, overrides the
// embedded library with a path on disk. Intended for testing against a
// differently-configured SQLite, and for distributions that would rather
// manage the shared library themselves.
const LibraryPathEnv = "PUREGO_SQLITE_LIBRARY"

var (
	loadOnce     sync.Once
	loadErr      error
	libHandle    uintptr
	libPath      string
	overridePath string
)

// SetLibraryPath directs the package at a SQLite shared library on disk
// instead of the embedded one. It must be called before any other function in
// this package; once the library is loaded the choice is fixed for the
// lifetime of the process.
func SetLibraryPath(path string) error {
	if libHandle != 0 {
		return errors.New("purego-sqlite: library already loaded from " + libPath)
	}
	overridePath = path
	return nil
}

// LoadedLibraryPath reports the file the SQLite library was loaded from, which
// is a cache directory when the embedded library is in use. It returns "" if
// the library has not been loaded yet.
func LoadedLibraryPath() string { return libPath }

func ensureLoaded() error {
	loadOnce.Do(func() { loadErr = loadLibrary() })
	return loadErr
}

func loadLibrary() error {
	path := overridePath
	if path == "" {
		path = os.Getenv(LibraryPathEnv)
	}
	if path == "" {
		if len(embeddedLibrary) == 0 {
			return ErrUnsupportedPlatform
		}
		p, err := materializeEmbedded()
		if err != nil {
			return fmt.Errorf("purego-sqlite: unpacking embedded library: %w", err)
		}
		path = p
	}

	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("purego-sqlite: dlopen %s: %w", path, err)
	}
	if err := bindSymbols(h); err != nil {
		return err
	}
	libHandle, libPath = h, path

	if rc := Code(lib.initialize()); rc != OK {
		return errCode(rc, "sqlite3_initialize")
	}
	// A library compiled single-threaded would silently corrupt under the
	// concurrency this package documents as supported. Refuse it at load.
	if lib.threadsafe() == 0 {
		return errors.New("purego-sqlite: library at " + path + " was compiled SQLITE_THREADSAFE=0 and cannot be used safely from multiple goroutines")
	}
	return nil
}

// materializeEmbedded writes the embedded library into a content-addressed
// directory under the user cache and returns its path. Content addressing
// means a re-pinned SQLite lands beside the old one rather than racing it, and
// concurrent processes converge on the same file: the write is to a temporary
// name in the same directory followed by an atomic rename.
func materializeEmbedded() (string, error) {
	sum := sha256.Sum256(embeddedLibrary)
	tag := hex.EncodeToString(sum[:])[:16]

	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "purego-sqlite", tag)
	path := filepath.Join(dir, embeddedLibraryName)

	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(embeddedLibrary)) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".unpack-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(embeddedLibrary); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		// Another process may have won the race; its file is byte-identical
		// because the directory name is the content hash.
		if fi, statErr := os.Stat(path); statErr == nil && fi.Size() == int64(len(embeddedLibrary)) {
			return path, nil
		}
		return "", err
	}
	return path, nil
}

// libFuncs is the entire foreign surface of this package. Two call styles
// share it:
//
// Typed fields go through purego.RegisterLibFunc, which uses reflection to
// place arguments. That is the readable choice, and it is the only correct
// choice for functions carrying float arguments or returns, because
// purego.SyscallN cannot tell an integer slot from a float one.
//
// Raw address fields (a-prefixed) are called with purego.SyscallN, which skips
// the reflection. They are the functions a query loop touches per row.
type libFuncs struct {
	initialize       func() int32
	libversion       func() string
	libversionNumber func() int32
	sourceID         func() string
	threadsafe       func() int32
	free             func(p uintptr)

	openV2              func(filename string, ppDb *uintptr, flags int32, zVfs uintptr) int32
	closeV2             func(db uintptr) int32
	errmsg              func(db uintptr) string
	extendedResultCodes func(db uintptr, on int32) int32
	busyTimeout         func(db uintptr, ms int32) int32
	lastInsertRowID     func(db uintptr) int64
	changes64           func(db uintptr) int64
	totalChanges64      func(db uintptr) int64
	getAutocommit       func(db uintptr) int32
	interrupt           func(db uintptr)
	exec                func(db uintptr, sql string, cb, arg, errmsg uintptr) int32
	dbFilename          func(db uintptr, dbName string) string
	walCheckpointV2     func(db uintptr, zDb string, mode int32, pnLog, pnCkpt *int32) int32
	walAutocheckpoint   func(db uintptr, n int32) int32

	prepareV3          func(db uintptr, sql *byte, n int32, prepFlags uint32, ppStmt *uintptr, pzTail *uintptr) int32
	finalize           func(stmt uintptr) int32
	stmtSQL            func(stmt uintptr) string
	expandedSQL        func(stmt uintptr) uintptr
	stmtReadonly       func(stmt uintptr) int32
	stmtBusy           func(stmt uintptr) int32
	columnName         func(stmt uintptr, i int32) string
	columnDecltype     func(stmt uintptr, i int32) string
	bindParameterCount func(stmt uintptr) int32
	bindParameterIndex func(stmt uintptr, name string) int32
	bindParameterName  func(stmt uintptr, i int32) string

	// Float-carrying: must stay on the reflection path.
	bindDouble   func(stmt uintptr, i int32, v float64) int32
	columnDouble func(stmt uintptr, i int32) float64

	backupInit      func(dst uintptr, dstName string, src uintptr, srcName string) uintptr
	backupStep      func(b uintptr, nPage int32) int32
	backupFinish    func(b uintptr) int32
	backupRemaining func(b uintptr) int32
	backupPagecount func(b uintptr) int32

	aStep, aReset, aClearBindings                              uintptr
	aBindInt64, aBindText, aBindBlob, aBindNull, aBindZeroblob uintptr
	aColumnCount, aDataCount, aColumnType, aColumnInt64        uintptr
	aColumnText, aColumnBlob, aColumnBytes                     uintptr
	aErrcode, aExtendedErrcode                                 uintptr
}

var lib libFuncs

// bindSymbols resolves every symbol the package will ever call, at load, so a
// library missing one fails immediately and by name rather than at the moment
// some rarely-taken branch reaches for it.
func bindSymbols(h uintptr) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("purego-sqlite: binding SQLite symbols: %v", r)
		}
	}()

	reg := func(fptr any, name string) { purego.RegisterLibFunc(fptr, h, name) }
	addr := func(name string) uintptr {
		p, e := purego.Dlsym(h, name)
		if e != nil || p == 0 {
			panic("missing symbol " + name)
		}
		return p
	}

	reg(&lib.initialize, "sqlite3_initialize")
	reg(&lib.libversion, "sqlite3_libversion")
	reg(&lib.libversionNumber, "sqlite3_libversion_number")
	reg(&lib.sourceID, "sqlite3_sourceid")
	reg(&lib.threadsafe, "sqlite3_threadsafe")
	reg(&lib.free, "sqlite3_free")

	reg(&lib.openV2, "sqlite3_open_v2")
	reg(&lib.closeV2, "sqlite3_close_v2")
	reg(&lib.errmsg, "sqlite3_errmsg")
	reg(&lib.extendedResultCodes, "sqlite3_extended_result_codes")
	reg(&lib.busyTimeout, "sqlite3_busy_timeout")
	reg(&lib.lastInsertRowID, "sqlite3_last_insert_rowid")
	reg(&lib.changes64, "sqlite3_changes64")
	reg(&lib.totalChanges64, "sqlite3_total_changes64")
	reg(&lib.getAutocommit, "sqlite3_get_autocommit")
	reg(&lib.interrupt, "sqlite3_interrupt")
	reg(&lib.exec, "sqlite3_exec")
	reg(&lib.dbFilename, "sqlite3_db_filename")
	reg(&lib.walCheckpointV2, "sqlite3_wal_checkpoint_v2")
	reg(&lib.walAutocheckpoint, "sqlite3_wal_autocheckpoint")

	reg(&lib.prepareV3, "sqlite3_prepare_v3")
	reg(&lib.finalize, "sqlite3_finalize")
	reg(&lib.stmtSQL, "sqlite3_sql")
	reg(&lib.expandedSQL, "sqlite3_expanded_sql")
	reg(&lib.stmtReadonly, "sqlite3_stmt_readonly")
	reg(&lib.stmtBusy, "sqlite3_stmt_busy")
	reg(&lib.columnName, "sqlite3_column_name")
	reg(&lib.columnDecltype, "sqlite3_column_decltype")
	reg(&lib.bindParameterCount, "sqlite3_bind_parameter_count")
	reg(&lib.bindParameterIndex, "sqlite3_bind_parameter_index")
	reg(&lib.bindParameterName, "sqlite3_bind_parameter_name")
	reg(&lib.bindDouble, "sqlite3_bind_double")
	reg(&lib.columnDouble, "sqlite3_column_double")

	reg(&lib.backupInit, "sqlite3_backup_init")
	reg(&lib.backupStep, "sqlite3_backup_step")
	reg(&lib.backupFinish, "sqlite3_backup_finish")
	reg(&lib.backupRemaining, "sqlite3_backup_remaining")
	reg(&lib.backupPagecount, "sqlite3_backup_pagecount")

	lib.aStep = addr("sqlite3_step")
	lib.aReset = addr("sqlite3_reset")
	lib.aClearBindings = addr("sqlite3_clear_bindings")
	lib.aBindInt64 = addr("sqlite3_bind_int64")
	lib.aBindText = addr("sqlite3_bind_text")
	lib.aBindBlob = addr("sqlite3_bind_blob")
	lib.aBindNull = addr("sqlite3_bind_null")
	lib.aBindZeroblob = addr("sqlite3_bind_zeroblob")
	lib.aColumnCount = addr("sqlite3_column_count")
	lib.aDataCount = addr("sqlite3_data_count")
	lib.aColumnType = addr("sqlite3_column_type")
	lib.aColumnInt64 = addr("sqlite3_column_int64")
	lib.aColumnText = addr("sqlite3_column_text")
	lib.aColumnBlob = addr("sqlite3_column_blob")
	lib.aColumnBytes = addr("sqlite3_column_bytes")
	lib.aErrcode = addr("sqlite3_errcode")
	lib.aExtendedErrcode = addr("sqlite3_extended_errcode")

	return nil
}

// Version reports the SQLite version string of the loaded library, for example
// "3.53.4". It is the version this repository pins, unless the library was
// overridden with [SetLibraryPath] or LibraryPathEnv.
func Version() (string, error) {
	if err := ensureLoaded(); err != nil {
		return "", err
	}
	return lib.libversion(), nil
}

// VersionNumber reports the loaded library's version as the integer
// (major*1000000 + minor*1000 + patch) that SQLite itself uses.
func VersionNumber() (int, error) {
	if err := ensureLoaded(); err != nil {
		return 0, err
	}
	return int(lib.libversionNumber()), nil
}

// SourceID reports the loaded library's upstream check-in identifier: the date
// and hash of the exact SQLite source it was compiled from.
func SourceID() (string, error) {
	if err := ensureLoaded(); err != nil {
		return "", err
	}
	return lib.sourceID(), nil
}

// ThreadSafeMode reports the SQLITE_THREADSAFE mode the loaded library was
// compiled with: 0 single-thread, 1 serialized, 2 multi-thread. This
// repository ships 2.
func ThreadSafeMode() (int, error) {
	if err := ensureLoaded(); err != nil {
		return 0, err
	}
	return int(lib.threadsafe()), nil
}
