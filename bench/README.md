# bench — the measurements, and how to redo them

This is a **separate Go module**. That is deliberate: the drivers under test are
heavy dependencies (`modernc.org/sqlite`, `ncruces/go-sqlite3`, and a cgo
reference binding), and the package being measured must not acquire them. A
consumer of `github.com/pjstein/purego-sqlite` never sees anything in this
directory — Go module zips exclude a subdirectory that has its own `go.mod`.

Everything published in the top-level README is derivable from files committed
here. That is the rule the harness is built around: a number that exists only in
a terminal which has since scrolled away is not a measurement, it is a memory.

## Reading the results

`results/` holds raw output, not summaries — every JSON the harness wrote and
every console transcript alongside it.

| File | What it is |
| --- | --- |
| `results/environment.txt` | Host, toolchain, pins, and machine load at the start and end of the run |
| `results/contended-fixed-2000-quiet.{json,txt}` | The headline four-driver comparison, 8 rounds in a verified quiet window |
| `results/contended-fixed-2000.{json,txt}` | The same configuration on a contended machine |
| `results/contended-unthrottled.{json,txt}` | Writer unthrottled rather than held at 2000 appends/s |
| `results/batching-ab-{on,off}-N.json`, `batching-ab.txt` | The row-batching A/B, five interleaved pairs |
| `results/micro-*.txt` | Go micro-benchmarks: row polling, appends, the raw call cost, the callback ladder |
| `results/conformance.txt` | The shared corpus driven through every driver, plus purego against cgo on the same library |
| `results/litestream-smoke.txt` | Replicate-restore-verify against a database this binding wrote |
| `results/extension-probe.txt` | What the shipped library reports as compiled in |
| `results/fixed-order-evidence/` | The runs that showed fixed driver order contaminating a slot, kept as the reason rotation exists |

To regenerate the published tables from the raw JSON rather than transcribing
them:

```
bench/derive-tables.py results/
```

**Absolute paths in these files are the measurement host's, kept verbatim.** The
transcripts record the exact invocation so a run can be reproduced, and a
transcript that has been tidied afterwards is not a transcript. For the same
reason, `environment.txt` records the machine's real load and the processes
competing with the benchmark, including the ones that make a round look bad.

One thing to know when reading `environment.txt`: the `shipped dylib sha256` it
records is the library as it stood when that run was made. The library has been
rebuilt since, so it will not match `lib/MANIFEST.txt`. The SQLite version and
source ID, which are what the numbers actually depend on, do match.

## Re-running

Requires a Go toolchain and, for the cgo reference driver, a C compiler. The
harness will not start into a results directory that already has output, so
point it somewhere else or move the committed files aside first.

```
bench/run-bakeoff.sh [rounds] [duration] [seed-rows]   # default 8 30s 200000
bench/run-micro.sh                                     # the Go micro-benchmarks
bench/run-batching-compare.sh                          # row batching on vs off
bench/run-callback-ladder.sh                           # the Go-function optimization ladder
bench/run-quiet-window.sh                              # one Config A run, for a genuinely idle machine
bench/run-litestream-smoke.sh                          # replication compatibility
```

Or, for a quick Go-level pass with no harness:

```
cd bench && go test -run '^$' -bench . -benchtime 10x ./...
```

## What the harness does to keep itself honest

These are the parts worth knowing about before trusting a number:

- **Driver order rotates by one slot per round**, so no driver systematically
  occupies the position a passing contaminant happens to land in. The runs in
  `results/fixed-order-evidence/` are why: with a fixed order, one slot was
  measurably worse regardless of which driver sat in it.
- **It refuses to start into a populated results directory**, so a second run
  cannot silently overwrite the dataset a published number came from.
- **It gates on a quiet machine** and re-checks between rounds, and records the
  load it saw rather than asserting the machine was idle.
- **It records whether row batching was actually on and refuses to publish if
  it was not** — the configuration under test is decided at library load, where
  it is easy to believe you measured something you did not.
- **It counts its own subjects.** A run with zero reader polls or zero rows is
  reported as a failed run, not a fast one.
- **The cgo reference drives the same shipped library through the same shim**,
  which is what isolates the calling convention from the SQLite implementation.
  `TestFFISetupReportsWhatItMeasured` fails if the two are not on the same
  SQLite.

## Scope

All of it is darwin/arm64 on one Apple M2 Max. linux/amd64 is built and tested
but has not been measured. These numbers are evidence that the approach is
sound and that the row-batching shim earns its place; they are not a portable
performance claim.
