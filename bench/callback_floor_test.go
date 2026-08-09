package bakeoff_test

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/pjstein/purego-sqlite/bench/cgoref"
)

// BenchmarkCallbackFloor measures what a C-to-Go crossing costs through purego
// with nothing at all behind it: C calls a Go function that does one store and
// returns.
//
// This is the floor under every Go-implemented SQL function. No amount of
// marshaling on the C side gets below it, so it is what says whether the
// remaining gap to a native Go driver is ours to close or purego's.
//
// purego's callback path is why it is not small: callbackWrap takes a process
// -wide mutex on every invocation, allocates a []reflect.Value, builds a
// reflect.Value per argument, and dispatches through reflect.Value.Call.
const floorBatch = 10000

var (
	floorOnce   sync.Once
	floorNoArg  uintptr
	floorOneArg uintptr
	floorSink   int64
)

func setupFloor() {
	floorOnce.Do(func() {
		floorNoArg = purego.NewCallback(func() uintptr {
			floorSink++
			return 0
		})
		floorOneArg = purego.NewCallback(func(p uintptr) uintptr {
			floorSink += int64(p)
			return 0
		})
	})
}

func BenchmarkCallbackFloor(b *testing.B) {
	if !cgoref.Available {
		b.Skip("cgo reference not built")
	}
	setupFloor()

	b.Run("purego callback, no arguments", func(b *testing.B) {
		calls := 0
		for b.Loop() {
			cgoref.CallN(floorNoArg, floorBatch)
			calls += floorBatch
		}
		if calls == 0 {
			b.Fatal("measured nothing")
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(calls), "ns/call")
	})

	b.Run("purego callback, one pointer argument", func(b *testing.B) {
		var arg int64
		calls := 0
		for b.Loop() {
			cgoref.CallNArg(floorOneArg, unsafe.Pointer(&arg), floorBatch)
			calls += floorBatch
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(calls), "ns/call")
	})

	// For scale: the same crossing in the other direction, which the bake-off
	// measured at roughly 67ns.
	b.Run("Go to C for comparison (cgo null call)", func(b *testing.B) {
		calls := 0
		for b.Loop() {
			cgoref.NullCall()
			calls++
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(calls), "ns/call")
	})
}
