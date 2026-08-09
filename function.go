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

// Value is one argument to an application-defined function. It is only valid
// for the duration of the call.
type Value struct{ ptr uintptr }

// Type is the storage class of the argument.
func (v Value) Type() ColumnType {
	r, _, _ := purego.SyscallN(lib.aValueType, v.ptr)
	return ColumnType(int32(r))
}

// Int64 reads the argument as an integer, converting per SQLite's rules.
func (v Value) Int64() int64 {
	r, _, _ := purego.SyscallN(lib.aValueInt64, v.ptr)
	return int64(r)
}

// Float reads the argument as a float, converting per SQLite's rules.
func (v Value) Float() float64 { return lib.valueDouble(v.ptr) }

// Text reads the argument as a string, copied into Go memory. Embedded NUL
// bytes survive: the length comes from SQLite.
func (v Value) Text() string {
	p, _, _ := purego.SyscallN(lib.aValueText, v.ptr)
	n, _, _ := purego.SyscallN(lib.aValueBytes, v.ptr)
	return string(cBytes(p, int32(n)))
}

// Blob reads the argument as bytes, copied into Go memory.
func (v Value) Blob() []byte {
	p, _, _ := purego.SyscallN(lib.aValueBlob, v.ptr)
	n, _, _ := purego.SyscallN(lib.aValueBytes, v.ptr)
	out := make([]byte, int32(n))
	copy(out, cBytes(p, int32(n)))
	return out
}

// FuncContext is where an application-defined function writes its answer.
type FuncContext struct{ ptr uintptr }

// ResultInt64 returns an integer.
func (c *FuncContext) ResultInt64(v int64) {
	purego.SyscallN(lib.aResultInt64, c.ptr, uintptr(v))
}

// ResultFloat returns a floating-point value.
func (c *FuncContext) ResultFloat(v float64) { lib.resultDouble(c.ptr, v) }

// ResultText returns a string. It is copied before the call returns.
func (c *FuncContext) ResultText(v string) {
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(unsafe.StringData(v))
	}
	purego.SyscallN(lib.aResultText, c.ptr, uintptr(p), uintptr(int32(len(v))), sqliteTransient)
}

// ResultBlob returns bytes. They are copied before the call returns.
func (c *FuncContext) ResultBlob(v []byte) {
	p := unsafe.Pointer(&emptyByte)
	if len(v) > 0 {
		p = unsafe.Pointer(&v[0])
	}
	purego.SyscallN(lib.aResultBlob, c.ptr, uintptr(p), uintptr(int32(len(v))), sqliteTransient)
}

// ResultNull returns SQL NULL.
func (c *FuncContext) ResultNull() { purego.SyscallN(lib.aResultNull, c.ptr) }

// ResultError aborts the query with a message.
func (c *FuncContext) ResultError(msg string) {
	p := unsafe.Pointer(&emptyByte)
	if len(msg) > 0 {
		p = unsafe.Pointer(unsafe.StringData(msg))
	}
	purego.SyscallN(lib.aResultError, c.ptr, uintptr(p), uintptr(int32(len(msg))))
}

// funcRegistry holds every registered Go function, keyed by a small integer
// handed to SQLite as the function's user-data pointer.
//
// The indirection exists because purego.NewCallback can only ever produce
// about 2000 C function pointers in a process and never reclaims one. A
// callback per registered SQL function would make a long-running daemon that
// creates connections in a loop run out of them. One trampoline serves every
// function instead, and the registry tells it which Go function to call.
var funcRegistry struct {
	mu   sync.RWMutex
	fns  map[uintptr]ScalarFunc
	next uintptr
}

var scalarTrampoline struct {
	once sync.Once
	ptr  uintptr
}

func trampoline() uintptr {
	scalarTrampoline.once.Do(func() {
		scalarTrampoline.ptr = purego.NewCallback(func(ctx uintptr, argc int32, argv uintptr) uintptr {
			handle, _, _ := purego.SyscallN(lib.aUserData, ctx)

			funcRegistry.mu.RLock()
			fn := funcRegistry.fns[handle]
			funcRegistry.mu.RUnlock()
			if fn == nil {
				// Registered and then somehow lost: report it as a SQL error
				// rather than crashing the process from inside C.
				msg := "sqlite3: no Go implementation registered for this function"
				purego.SyscallN(lib.aResultError, ctx, uintptr(unsafe.Pointer(unsafe.StringData(msg))), uintptr(int32(len(msg))))
				return 0
			}

			args := make([]Value, argc)
			for i := int32(0); i < argc; i++ {
				p := *(*uintptr)(unsafe.Add(cptr(argv), uintptr(i)*unsafe.Sizeof(uintptr(0))))
				args[i] = Value{ptr: p}
			}

			// A Go panic must not unwind into C: the C frames have no idea
			// what to do with it and the process dies. Convert it to a SQL
			// error, which is what a failing function is.
			defer func() {
				if r := recover(); r != nil {
					msg := fmt.Sprint("sqlite3: panic in application-defined function: ", r)
					purego.SyscallN(lib.aResultError, ctx, uintptr(unsafe.Pointer(unsafe.StringData(msg))), uintptr(int32(len(msg))))
				}
			}()
			fn(&FuncContext{ptr: ctx}, args)
			return 0
		})
	})
	return scalarTrampoline.ptr
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

	funcRegistry.mu.Lock()
	if funcRegistry.fns == nil {
		funcRegistry.fns = make(map[uintptr]ScalarFunc)
	}
	funcRegistry.next++
	handle := funcRegistry.next
	funcRegistry.fns[handle] = fn
	funcRegistry.mu.Unlock()

	rc := Code(lib.createFunctionV2(c.db, name, int32(nArg), int32(flags|utf8), handle, trampoline(), 0, 0, 0))
	if rc != OK {
		funcRegistry.mu.Lock()
		delete(funcRegistry.fns, handle)
		funcRegistry.mu.Unlock()
		return c.err(rc, "create_function "+name)
	}
	return nil
}
