package sqlite3

import (
	"os"
	"unsafe"

	"github.com/ebitengine/purego"
)

// BatchEnv names an environment variable that disables the row-batching fast
// path when set to "0". It exists so the benchmarks can measure both paths
// against the same library; there is no reason to set it in production.
const BatchEnv = "PUREGO_SQLITE_BATCH"

// colValue mirrors rowbatch_col in build/shim.c exactly. The size is checked
// against the library's own sizeof at load, so a change to one file without
// the other fails immediately rather than silently misreading rows.
type colValue struct {
	ival int64
	fval float64
	ptr  uintptr
	len  int32
	typ  int32
}

var (
	batchAddr    uintptr // rowbatch_step, 0 when the library does not have it
	batchEnabled bool
)

// bindRowBatch looks for the row-batching shim. Its absence is not an error:
// the package works against a stock libsqlite3, one foreign call per column.
func bindRowBatch(h uintptr) {
	if os.Getenv(BatchEnv) == "0" {
		return
	}
	stepAddr, err := dlsym(h, "rowbatch_step")
	if err != nil || stepAddr == 0 {
		return
	}
	sizeAddr, err := dlsym(h, "rowbatch_col_size")
	if err != nil || sizeAddr == 0 {
		return
	}
	got, _, _ := purego.SyscallN(sizeAddr)
	if int(int32(got)) != int(unsafe.Sizeof(colValue{})) {
		// Refuse rather than misread: the two structs have drifted apart.
		return
	}
	batchAddr, batchEnabled = stepAddr, true
}

// RowBatchingActive reports whether the loaded library provides the
// row-batching shim and it is in use. False means correct results at a higher
// per-row cost, which is what a stock system SQLite gives you.
func RowBatchingActive() (bool, error) {
	if err := ensureLoaded(); err != nil {
		return false, err
	}
	return batchEnabled, nil
}

// cStepRow steps the statement and, on a row, fills buf with the row's values.
func cStepRow(stmt uintptr, buf []colValue) Code {
	r, _, _ := purego.SyscallN(batchAddr, stmt, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	return Code(int32(r))
}

// buffered reports whether column i of the current row was captured by the
// batching shim. Everything outside that -- no shim, no row, a column beyond
// the buffer -- falls back to asking SQLite directly.
func (s *Stmt) buffered(i int) bool {
	return s.rowBuffered && i >= 0 && i < len(s.rowBuf)
}
