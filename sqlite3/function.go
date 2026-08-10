package sqlite3

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// FunctionFlag controls how SQLite may use an application-defined function,
// mirroring the text-encoding and optimization bits of sqlite3_create_function.
type FunctionFlag int32

const (
	// Deterministic promises the function returns the same answer for the same
	// arguments within a single query, which lets SQLite factor it out of
	// loops and use it in indexes on expressions. Promise it falsely and you
	// get wrong answers, not slow ones.
	Deterministic FunctionFlag = 0x000000800
	// DirectOnly forbids use from triggers, views and schema structures --
	// the right default for anything touching the outside world.
	DirectOnly FunctionFlag = 0x000080000
	// Innocuous declares the function harmless to run on hostile input.
	Innocuous FunctionFlag = 0x000200000

	utf8 FunctionFlag = 1
)

// ScalarFunc is a Go implementation of a SQL scalar function. Write the answer
// through ctx; returning without doing so yields SQL NULL.
type ScalarFunc func(ctx *FuncContext, args []Value)

// typeLazy marks a Value that carries no cached data, only the SQLite value
// handle. The portable path produces these: it has not paid to extract
// anything, so every accessor asks SQLite.
const typeLazy int32 = -1

// Value is one argument to an application-defined function. It is only valid
// for the duration of the call.
//
// The layout mirrors fn_value in build/shim.c exactly, because on the fast
// path the argument slice aliases a buffer C filled in on its own stack. The
// raw handle travels alongside the cached data so that reading an argument as
// a type it is not stored as still goes to SQLite for the conversion -- the
// same refusal to reimplement SQLite's rules that the row batcher makes.
type Value struct {
	ival int64
	fval float64
	ptr  uintptr
	raw  uintptr
	len  int32
	typ  int32
}

// Type is the storage class of the argument.
func (v Value) Type() ColumnType {
	if v.typ != typeLazy {
		return ColumnType(v.typ)
	}
	r, _, _ := purego.SyscallN(lib.aValueType, v.raw)
	return ColumnType(int32(r))
}

// Int64 reads the argument as an integer, converting per SQLite's rules.
func (v Value) Int64() int64 {
	if v.typ == int32(TypeInteger) {
		return v.ival
	}
	r, _, _ := purego.SyscallN(lib.aValueInt64, v.raw)
	return int64(r)
}

// Float reads the argument as a float, converting per SQLite's rules.
func (v Value) Float() float64 {
	if v.typ == int32(TypeFloat) {
		return v.fval
	}
	return lib.valueDouble(v.raw)
}

// Text reads the argument as a string, copied into Go memory. Embedded NUL
// bytes survive: the length comes from SQLite.
func (v Value) Text() string {
	switch v.typ {
	case int32(TypeText):
		return string(cBytes(v.ptr, v.len))
	case int32(TypeNull):
		return ""
	}
	p, _, _ := purego.SyscallN(lib.aValueText, v.raw)
	n, _, _ := purego.SyscallN(lib.aValueBytes, v.raw)
	return string(cBytes(p, int32(n)))
}

// Blob reads the argument as bytes, copied into Go memory. It returns nil for
// SQL NULL.
func (v Value) Blob() []byte {
	switch v.typ {
	case int32(TypeBlob):
		out := make([]byte, v.len)
		copy(out, cBytes(v.ptr, v.len))
		return out
	case int32(TypeNull):
		return nil
	}
	if v.typ == typeLazy {
		r, _, _ := purego.SyscallN(lib.aValueType, v.raw)
		if ColumnType(int32(r)) == TypeNull {
			return nil
		}
	}
	p, _, _ := purego.SyscallN(lib.aValueBlob, v.raw)
	n, _, _ := purego.SyscallN(lib.aValueBytes, v.raw)
	out := make([]byte, int32(n))
	copy(out, cBytes(p, int32(n)))
	return out
}

// RawBlob returns the argument's bytes without copying, aliasing SQLite's own
// memory. Valid only until the function returns. Use [Value.Blob] unless the
// copy is measurably the problem.
func (v Value) RawBlob() []byte {
	if v.typ == int32(TypeBlob) || v.typ == int32(TypeText) {
		return cBytes(v.ptr, v.len)
	}
	p, _, _ := purego.SyscallN(lib.aValueBlob, v.raw)
	n, _, _ := purego.SyscallN(lib.aValueBytes, v.raw)
	return cBytes(p, int32(n))
}

// FuncContext is where an application-defined function writes its answer.
//
// On the marshaled path the answer is written into a buffer that C reads after
// the call returns; on the portable path it goes straight to SQLite. Callers
// see neither difference.
type FuncContext struct {
	ptr  uintptr    // sqlite3_context*
	call *fnCall    // non-nil on the marshaled scalar path
	agg  *fnAggCall // non-nil on the aggregate path
}

// resultInline reports whether an answer of n bytes fits the buffer C reads
// back, and returns the buffer to write it into.
func (c *FuncContext) resultInline(n int) ([]byte, bool) {
	switch {
	case c.call != nil && n <= fnResultInline:
		return c.call.rbuf[:], true
	case c.agg != nil && n <= fnResultInline:
		return c.agg.rbuf[:], true
	}
	return nil, false
}

func (c *FuncContext) setResult(kind int32, ival int64, fval float64, n int32) {
	switch {
	case c.call != nil:
		c.call.rtype, c.call.rival, c.call.rfval, c.call.rlen = kind, ival, fval, n
	case c.agg != nil:
		c.agg.rtype, c.agg.rival, c.agg.rfval, c.agg.rlen = kind, ival, fval, n
	}
}

func (c *FuncContext) marshaled() bool { return c.call != nil || c.agg != nil }

// ResultInt64 returns an integer.
func (c *FuncContext) ResultInt64(v int64) {
	if c.marshaled() {
		c.setResult(fnResultInt, v, 0, 0)
		return
	}
	purego.SyscallN(lib.aResultInt64, c.ptr, uintptr(v))
}

// ResultFloat returns a floating-point value.
func (c *FuncContext) ResultFloat(v float64) {
	if c.marshaled() {
		c.setResult(fnResultFloat, 0, v, 0)
		return
	}
	lib.resultDouble(c.ptr, v)
}

// ResultText returns a string.
func (c *FuncContext) ResultText(v string) {
	if buf, ok := c.resultInline(len(v)); ok {
		c.setResult(fnResultText, 0, 0, int32(copy(buf, v)))
		return
	}
	// Too large for the inline buffer, or no buffer at all: call SQLite
	// directly. It copies before returning, which is what makes it safe to
	// hand it a pointer into Go memory.
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(unsafe.StringData(v))
	}
	purego.SyscallN(lib.aResultText, c.ptr, uintptr(p), uintptr(int32(len(v))), sqliteTransient)
	c.setResult(fnResultHandled, 0, 0, 0)
}

// ResultBlob returns bytes.
func (c *FuncContext) ResultBlob(v []byte) {
	if buf, ok := c.resultInline(len(v)); ok {
		c.setResult(fnResultBlob, 0, 0, int32(copy(buf, v)))
		return
	}
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(&v[0])
	}
	purego.SyscallN(lib.aResultBlob, c.ptr, uintptr(p), uintptr(int32(len(v))), sqliteTransient)
	c.setResult(fnResultHandled, 0, 0, 0)
}

// ResultNull returns SQL NULL.
func (c *FuncContext) ResultNull() {
	if c.marshaled() {
		c.setResult(fnResultNull, 0, 0, 0)
		return
	}
	purego.SyscallN(lib.aResultNull, c.ptr)
}

// ResultError aborts the query with a message.
func (c *FuncContext) ResultError(msg string) {
	if buf, ok := c.resultInline(len(msg)); ok {
		c.setResult(fnResultError, 0, 0, int32(copy(buf, msg)))
		return
	}
	p := unsafe.Pointer(&emptyByte)
	if len(msg) > 0 {
		p = unsafe.Pointer(unsafe.StringData(msg))
	}
	purego.SyscallN(lib.aResultError, c.ptr, uintptr(p), uintptr(int32(len(msg))))
	c.setResult(fnResultHandled, 0, 0, 0)
}

// ---- the portable path ----

// portableRegistry holds functions registered when the marshaling trampoline
// is unavailable. It is a map because this path is not the one that runs per
// row; the fast path uses slice-index dispatch instead.
var portableRegistry struct {
	mu   sync.RWMutex
	fns  map[uintptr]ScalarFunc
	next uintptr
}

var portableTrampoline struct {
	once sync.Once
	ptr  uintptr
}

// portableCallback is the trampoline used against a library without the shim.
// It costs a Go-to-C call for the user data and one per argument and result,
// which is why the marshaled path exists -- but it is correct, and correctness
// is what a fallback is for.
func portableCallback() uintptr {
	portableTrampoline.once.Do(func() {
		portableTrampoline.ptr = purego.NewCallback(func(ctx uintptr, argc int32, argv uintptr) uintptr {
			handle, _, _ := purego.SyscallN(lib.aUserData, ctx)

			portableRegistry.mu.RLock()
			fn := portableRegistry.fns[handle]
			portableRegistry.mu.RUnlock()

			fctx := FuncContext{ptr: ctx}
			if fn == nil {
				fctx.ResultError("sqlite3: no Go implementation registered for this function")
				return 0
			}

			args := make([]Value, argc)
			for i := int32(0); i < argc; i++ {
				p := *(*uintptr)(unsafe.Add(cptr(argv), uintptr(i)*unsafe.Sizeof(uintptr(0))))
				args[i] = Value{raw: p, typ: typeLazy}
			}

			defer func() {
				if r := recover(); r != nil {
					fctx.ResultError("sqlite3: panic in application-defined function: " + fmt.Sprint(r))
				}
			}()
			fn(&fctx, args)
			return 0
		})
	})
	return portableTrampoline.ptr
}

// CreateFunction registers a Go implementation of a SQL scalar function on
// this connection.
//
// nArg is the argument count, or -1 for variadic. The function is visible only
// to this connection, which is SQLite's model: there is no global registry.
//
// The Go function is called on whatever thread SQLite is running the query on,
// which for this package is the goroutine that called Step. It must not use
// the connection it was called from.
//
// Answering a SQL function in Go crosses the C boundary once per invocation.
// See the package benchmarks for what that costs; it is fine for functions
// called thousands of times and wrong for ones called millions.
func (c *Conn) CreateFunction(name string, nArg int, flags FunctionFlag, fn ScalarFunc) error {
	if err := c.check(); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("sqlite3: CreateFunction(%q): nil function", name)
	}
	if lib.createFunctionV2 == nil {
		return errors.New("sqlite3: the loaded library has no sqlite3_create_function_v2")
	}
	if nArg > fnMaxArgs {
		return fmt.Errorf("sqlite3: CreateFunction(%q): %d arguments exceeds the supported maximum of %d", name, nArg, fnMaxArgs)
	}

	if fnFastAvailable && !forcePortableFunctions {
		installDispatch()
		handle := registerFast(fn)
		rc := Code(lib.createFunctionV2(c.db, name, int32(nArg), int32(flags|utf8), uintptr(handle), fnScalarPtr, 0, 0, 0))
		if rc != OK {
			return c.err(rc, "create_function "+name)
		}
		return nil
	}

	portableRegistry.mu.Lock()
	if portableRegistry.fns == nil {
		portableRegistry.fns = make(map[uintptr]ScalarFunc)
	}
	portableRegistry.next++
	handle := portableRegistry.next
	portableRegistry.fns[handle] = fn
	portableRegistry.mu.Unlock()

	rc := Code(lib.createFunctionV2(c.db, name, int32(nArg), int32(flags|utf8), handle, portableCallback(), 0, 0, 0))
	if rc != OK {
		portableRegistry.mu.Lock()
		delete(portableRegistry.fns, handle)
		portableRegistry.mu.Unlock()
		return c.err(rc, "create_function "+name)
	}
	return nil
}
