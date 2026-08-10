package sqlite3

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Accumulation is what C summarised while walking the rows, handed to a Go
// aggregate once at the end.
//
// The narrowness is the point. A scalar function crosses the boundary per row;
// an aggregate that accumulates in C crosses once per query. What C can
// summarise without knowing what the answer means is a small set, and that set
// is this. An aggregate needing every individual row is not this feature --
// that is an ordinary query with a loop around it.
type Accumulation struct {
	// Count is how many non-NULL values were seen. SQL aggregates ignore
	// NULL, and so does this one.
	Count int64
	// Sum is the running total as floating point.
	Sum float64
	// Min and Max are the extremes of the numeric interpretation. They are
	// meaningless when Count is zero.
	Min, Max float64
	// IntSum is the exact integer total, valid only when AllInt is true --
	// float accumulation loses precision that a coordination-plane rollup over
	// counters usually cannot afford to lose.
	IntSum int64
	// AllInt reports that every value seen was an integer.
	AllInt bool
}

// AggregateFunc is a Go implementation of a SQL aggregate's final step. It is
// called once per group, with what C accumulated.
type AggregateFunc func(ctx *FuncContext, acc Accumulation)

// fnAggCall mirrors fn_agg_call in build/shim.c.
type fnAggCall struct {
	ctx    uintptr
	handle int64
	count  int64
	sum    float64
	min    float64
	max    float64
	isum   int64
	allInt int32
	rtype  int32
	rlen   int32
	pad    int32
	rival  int64
	rfval  float64
	rbuf   [fnResultInline]byte
}

var (
	fnAggAvailable   bool
	fnAggStepPtr     uintptr
	fnAggFinalPtr    uintptr
	fnSetAggDispatch func(uintptr)
	fnAggOnce        sync.Once
	fnAggTable       atomic.Pointer[[]AggregateFunc]
	fnAggRegisterMu  sync.Mutex
)

func bindAggregateFast(h uintptr) {
	sym := func(name string) uintptr {
		p, err := dlsym(h, name)
		if err != nil {
			return 0
		}
		return p
	}
	stepFn, finalFn := sym("fn_agg_step_ptr"), sym("fn_agg_final_ptr")
	setFn, sizeFn := sym("fn_set_agg_dispatch"), sym("fn_agg_call_size")
	if stepFn == 0 || finalFn == 0 || setFn == 0 || sizeFn == 0 {
		return
	}
	if got, _, _ := purego.SyscallN(sizeFn); int(int32(got)) != int(unsafe.Sizeof(fnAggCall{})) {
		return
	}
	s, _, _ := purego.SyscallN(stepFn)
	f, _, _ := purego.SyscallN(finalFn)
	if s == 0 || f == 0 {
		return
	}
	fnAggStepPtr, fnAggFinalPtr = s, f
	purego.RegisterFunc(&fnSetAggDispatch, setFn)
	fnAggAvailable = true
}

func installAggDispatch() {
	fnAggOnce.Do(func() {
		fnSetAggDispatch(purego.NewCallback(func(callPtr uintptr) uintptr {
			fnAggDispatch((*fnAggCall)(cptr(callPtr)))
			return 0
		}))
	})
}

func fnAggDispatch(call *fnAggCall) {
	defer func() {
		if r := recover(); r != nil {
			msg := "sqlite3: panic in application-defined aggregate: " + fmt.Sprint(r)
			call.rtype = fnResultError
			call.rlen = int32(copy(call.rbuf[:], msg))
		}
	}()

	p := fnAggTable.Load()
	if p == nil || call.handle < 0 || call.handle >= int64(len(*p)) {
		msg := "sqlite3: no Go implementation registered for this aggregate"
		call.rtype = fnResultError
		call.rlen = int32(copy(call.rbuf[:], msg))
		return
	}
	fn := (*p)[call.handle]

	// The scalar call struct and the aggregate one share their result fields
	// by layout, so one FuncContext serves both; only the result half is used.
	ctx := FuncContext{ptr: call.ctx, agg: call}
	fn(&ctx, Accumulation{
		Count:  call.count,
		Sum:    call.sum,
		Min:    call.min,
		Max:    call.max,
		IntSum: call.isum,
		AllInt: call.allInt != 0,
	})
}

// CreateAggregate registers a Go implementation of a SQL aggregate whose
// per-row accumulation happens in C.
//
// The Go function runs once per group rather than once per row, which is the
// whole reason to prefer an aggregate here: a scalar function over a million
// rows pays a million boundary crossings, this pays one.
//
// It requires the marshaling shim; without it there is no accumulator to batch
// into and the call returns an error rather than silently falling back to
// something with different cost.
func (c *Conn) CreateAggregate(name string, nArg int, flags FunctionFlag, fn AggregateFunc) error {
	if err := c.check(); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("sqlite3: CreateAggregate(%q): nil function", name)
	}
	if !fnAggAvailable {
		return errors.New("sqlite3: the loaded library has no aggregate marshaling trampoline; rebuild with `make build` or use CreateFunction")
	}
	if lib.createFunctionV2 == nil {
		return errors.New("sqlite3: the loaded library has no sqlite3_create_function_v2")
	}
	installAggDispatch()

	fnAggRegisterMu.Lock()
	var cur []AggregateFunc
	if p := fnAggTable.Load(); p != nil {
		cur = *p
	}
	next := make([]AggregateFunc, len(cur), len(cur)+1)
	copy(next, cur)
	next = append(next, fn)
	fnAggTable.Store(&next)
	handle := int64(len(next) - 1)
	fnAggRegisterMu.Unlock()

	rc := Code(lib.createFunctionV2(c.db, name, int32(nArg), int32(flags|utf8),
		uintptr(handle), 0, fnAggStepPtr, fnAggFinalPtr, 0))
	if rc != OK {
		return c.err(rc, "create_aggregate "+name)
	}
	return nil
}
