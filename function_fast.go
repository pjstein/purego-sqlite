package sqlite3

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The marshaled function path: C flattens the arguments, Go is called once,
// and C issues the result.
//
// The portable path costs four boundary crossings to answer a one-argument
// function -- one C-to-Go for the call itself, then Go-to-C for the user data,
// for the argument, and for the result. This path costs one. It is the
// row-batching lesson pointed in the other direction, and like row batching it
// needs the shim, so it is feature-detected: a stock libsqlite3 gets the
// portable path and identical answers, only slower.

const (
	fnMaxArgs      = 16  // mirrors FN_MAX_ARGS in build/shim.c
	fnResultInline = 512 // mirrors FN_RESULT_INLINE

	fnResultNull    = 0
	fnResultInt     = 1
	fnResultFloat   = 2
	fnResultText    = 3
	fnResultBlob    = 4
	fnResultError   = 5
	fnResultHandled = 6 // Go already called sqlite3_result_*; C does nothing
)

// fnCall mirrors fn_call in build/shim.c. The args array is [fnMaxArgs]Value
// rather than a private type because the fast path hands the Go function a
// slice aliasing it directly, with no copy and no allocation -- which is only
// sound if Value has fn_value's layout, checked against the library's own
// sizeof at load.
type fnCall struct {
	ctx    uintptr
	handle int64
	argc   int32
	rtype  int32
	rlen   int32
	pad    int32
	rival  int64
	rfval  float64
	args   [fnMaxArgs]Value
	rbuf   [fnResultInline]byte
}

var (
	fnFastAvailable bool
	fnScalarPtr     uintptr // the C trampoline SQLite is given as xFunc
	fnSetDispatch   func(uintptr)
	fnDispatchOnce  sync.Once

	// forcePortableFunctions makes CreateFunction take the portable path even
	// when the shim is present. It exists so the benchmarks can measure both
	// against one library, and so a consumer who distrusts the fast path has a
	// switch rather than a fork.
	forcePortableFunctions bool
)

// FunctionPathEnv names an environment variable that, when set to "portable",
// forces application-defined functions onto the per-call path.
const FunctionPathEnv = "PUREGO_SQLITE_FUNCTIONS"

// UseMarshaledFunctions reports whether application-defined functions take the
// C-marshaling fast path. False means the loaded library has no marshaling
// trampoline, or it was disabled by FunctionPathEnv.
func UseMarshaledFunctions() (bool, error) {
	if err := ensureLoaded(); err != nil {
		return false, err
	}
	return fnFastAvailable && !forcePortableFunctions, nil
}

// ForcePortableFunctionsForTest switches subsequent CreateFunction calls onto
// the portable path. It exists so a test can compare the two implementations
// in one process, which is the only way to compare them at all: the
// environment variable is read once, at library load.
func ForcePortableFunctionsForTest(on bool) { forcePortableFunctions = on }

// bindFunctionFast wires the C marshaling trampoline if the library has it.
func bindFunctionFast(h uintptr) {
	sym := func(name string) uintptr {
		p, err := purego.Dlsym(h, name)
		if err != nil {
			return 0
		}
		return p
	}
	scalarPtrFn, setDispatch := sym("fn_scalar_ptr"), sym("fn_set_dispatch")
	sizeFn, maxArgsFn := sym("fn_call_size"), sym("fn_max_args")
	if scalarPtrFn == 0 || setDispatch == 0 || sizeFn == 0 || maxArgsFn == 0 {
		return
	}

	// Refuse rather than misread. If the two structs have drifted apart, every
	// argument past the first would be read from the wrong offset and the
	// answers would be wrong rather than missing -- the worst failure mode
	// available here.
	if gotSize, _, _ := purego.SyscallN(sizeFn); int(int32(gotSize)) != int(unsafe.Sizeof(fnCall{})) {
		return
	}
	if gotMax, _, _ := purego.SyscallN(maxArgsFn); int(int32(gotMax)) != fnMaxArgs {
		return
	}

	ptr, _, _ := purego.SyscallN(scalarPtrFn)
	if ptr == 0 {
		return
	}
	fnScalarPtr = ptr
	purego.RegisterFunc(&fnSetDispatch, setDispatch)
	fnFastAvailable = true
}

// fnTable is the dispatch table. The handle C hands back is an index, so
// finding the Go function is a bounds check and a load rather than a map
// lookup under a read lock.
//
// It is stored as an immutable slice swapped atomically: registration builds a
// new slice and publishes it, invocation loads the pointer once. Registration
// is rare, invocation is per row, and the per-row path takes no lock at all.
var fnTable atomic.Pointer[[]ScalarFunc]

var fnRegisterMu sync.Mutex

// registerFast adds fn to the dispatch table and returns its index.
func registerFast(fn ScalarFunc) int64 {
	fnRegisterMu.Lock()
	defer fnRegisterMu.Unlock()

	var cur []ScalarFunc
	if p := fnTable.Load(); p != nil {
		cur = *p
	}
	next := make([]ScalarFunc, len(cur), len(cur)+1)
	copy(next, cur)
	next = append(next, fn)
	fnTable.Store(&next)
	return int64(len(next) - 1)
}

// installDispatch hands C the single Go callback that answers every function.
//
// One argument, and it is a pointer, deliberately: purego decodes callback
// arguments with reflection, building a reflect.Value per argument on every
// invocation, so the cheapest signature available is the one that carries
// everything behind a single word.
func installDispatch() {
	fnDispatchOnce.Do(func() {
		fnSetDispatch(purego.NewCallback(func(callPtr uintptr) uintptr {
			fnDispatch((*fnCall)(cptr(callPtr)))
			return 0
		}))
	})
}

// fnDispatch is the whole Go side of a function call.
//
// It must not panic into C: a Go panic unwinding through SQLite's frames kills
// the process. The recovery below is not an optimization to be traded away --
// a variant of this path without it would not be a faster variant, it would be
// a bug.
func fnDispatch(call *fnCall) {
	defer func() {
		if r := recover(); r != nil {
			msg := "sqlite3: panic in application-defined function: " + fmt.Sprint(r)
			call.rtype = fnResultError
			call.rlen = int32(copy(call.rbuf[:], msg))
		}
	}()

	p := fnTable.Load()
	if p == nil || call.handle < 0 || call.handle >= int64(len(*p)) {
		msg := "sqlite3: no Go implementation registered for this function"
		call.rtype = fnResultError
		call.rlen = int32(copy(call.rbuf[:], msg))
		return
	}
	fn := (*p)[call.handle]

	ctx := FuncContext{ptr: call.ctx, call: call}
	// The argument slice aliases C's stack frame, which is alive for exactly
	// the duration of this call: no allocation, no copy.
	args := unsafe.Slice(&call.args[0], int(call.argc))
	fn(&ctx, args)
}
