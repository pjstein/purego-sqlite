package sqlite3

import (
	"github.com/ebitengine/purego"
)

// Extension names an optional SQLite extension this repository can compile
// into the shipped library.
type Extension string

const (
	// ExtSQLiteVec is asg017/sqlite-vec: vector storage and KNN search through
	// vec0 virtual tables. Pinned in build/extensions.lock, built by
	// `WITH_SQLITE_VEC=1 make build`.
	ExtSQLiteVec Extension = "sqlite-vec"
)

// extBit maps each extension to its bit in the mask build/extensions.c
// returns. The mask exists so that capability can be reported without running
// a probe query per extension.
var extBit = map[Extension]int32{
	ExtSQLiteVec: 1 << 0,
}

var (
	extAvailableMask  int32
	extRegisteredMask int32
	extSupported      bool
)

// bindExtensions registers whatever extensions the loaded library carries.
//
// Its absence is not an error, exactly as with the row-batching shim: a
// library built without extensions has no such symbol, and one Go package has
// to keep working against several builds of the library. Registration happens
// at load, before any connection exists, because sqlite3_auto_extension only
// affects connections opened after it.
func bindExtensions(h uintptr) {
	avail, err := purego.Dlsym(h, "sqlite_ext_available")
	if err != nil || avail == 0 {
		return
	}
	reg, err := purego.Dlsym(h, "sqlite_ext_autoregister")
	if err != nil || reg == 0 {
		return
	}
	extSupported = true

	a, _, _ := purego.SyscallN(avail)
	extAvailableMask = int32(a)
	r, _, _ := purego.SyscallN(reg)
	extRegisteredMask = int32(r)
}

// CompiledExtensions reports the extensions built into the loaded library and
// successfully registered for every new connection.
//
// An empty result is the normal case: extensions are opt-in at build time.
func CompiledExtensions() ([]Extension, error) {
	if err := ensureLoaded(); err != nil {
		return nil, err
	}
	var out []Extension
	for ext, bit := range extBit {
		if extRegisteredMask&bit != 0 {
			out = append(out, ext)
		}
	}
	return out, nil
}

// HasExtension reports whether the named extension is compiled in and
// registered.
func HasExtension(e Extension) (bool, error) {
	if err := ensureLoaded(); err != nil {
		return false, err
	}
	bit, known := extBit[e]
	return known && extRegisteredMask&bit != 0, nil
}

// extensionRegistrationFailed reports extensions the library was built with
// that nonetheless failed to register. It exists so that "not built in" and
// "built in but broken" cannot be confused, which is the failure a single
// boolean would hide.
func extensionRegistrationFailed() []Extension {
	var out []Extension
	for ext, bit := range extBit {
		if extAvailableMask&bit != 0 && extRegisteredMask&bit == 0 {
			out = append(out, ext)
		}
	}
	return out
}

// EnableLoadExtension turns SQLite's runtime extension loading on or off for
// this connection.
//
// It is off by default in SQLite and this package does not change that: a
// connection that can load a shared library from a path is a connection that
// can execute arbitrary code, and whether that is acceptable is the
// application's call, not a binding's.
//
// Enabling affects the C API only. SQL's load_extension() stays disabled
// unless the application also turns on SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION's
// SQL half, which this package deliberately does not expose -- a SQL string
// that can load a dylib is a much larger blast radius than a Go call that can.
func (c *Conn) EnableLoadExtension(on bool) error {
	if err := c.check(); err != nil {
		return err
	}
	if lib.enableLoadExtension == 0 {
		return errNoLoadExtension
	}
	var v uintptr
	if on {
		v = 1
	}
	rc, _, _ := purego.SyscallN(lib.enableLoadExtension, c.db, v)
	return c.err(Code(int32(rc)), "enable_load_extension")
}

// LoadExtension loads a SQLite extension from a shared library at runtime.
//
// entry may be empty, in which case SQLite derives the entry point from the
// filename. [Conn.EnableLoadExtension] must have been called first.
//
// This is the escape hatch for extensions this repository has not compiled in:
// it costs a file on disk that has to match the platform and the SQLite ABI,
// which is exactly the deployment problem the embedded library exists to
// avoid. Prefer compiling an extension in when it is one you always want.
func (c *Conn) LoadExtension(path, entry string) error {
	if err := c.check(); err != nil {
		return err
	}
	if lib.loadExtension == nil {
		return errNoLoadExtension
	}
	var errMsg uintptr
	rc := Code(lib.loadExtension(c.db, path, entry, &errMsg))
	if rc != OK {
		msg := ""
		if errMsg != 0 {
			msg = cString(errMsg)
			lib.free(errMsg)
		}
		return &Error{Code: rc.Primary(), Extended: rc, Message: msg, SQL: "load_extension " + path}
	}
	if errMsg != 0 {
		lib.free(errMsg)
	}
	return nil
}
