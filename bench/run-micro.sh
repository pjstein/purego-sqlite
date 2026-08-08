#!/usr/bin/env bash
# Run the micro-benchmarks and write their raw output under results/.
#
# Same rule as the comparative run: every number reported must be derivable
# from a file this script wrote.
#
# Usage:  bench/run-micro.sh [repetitions] [benchtime]
# Default: 5 repetitions per benchmark, 3s each.
#
# Repetitions are separate `go test` invocations with -count=1, not one
# invocation with -count=5. That is not a stylistic choice: `go test -bench`
# with -count=N runs each benchmark N times *consecutively* before moving to
# the next one, which is exactly the all-of-A-then-all-of-B ordering the
# comparative run was rebuilt to avoid. One invocation per repetition puts
# every benchmark once into each pass, so drift lands on all of them.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

REPS="${1:-5}"
BENCHTIME="${2:-3s}"
results="$here/results"
mkdir -p "$results"

# Reps interleave the benchmarks; each rep is one full pass over the set.
reps() {
  local label="$1"; shift
  local out="$results/$label.txt"
  echo
  echo "running $label x${REPS} -> $out"
  {
    echo "# raw output of ${REPS} separate invocations of:"
    echo "#   $*"
    echo "# one invocation per repetition, so every benchmark runs once per pass"
    echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
  } > "$out"
  for i in $(seq 1 "$REPS"); do
    {
      echo "### repetition $i/$REPS  load $(sysctl -n vm.loadavg)  $(date -u +%H:%M:%SZ)"
    } >> "$out"
    "$@" 2>&1 | tee -a "$out"
    echo >> "$out"
  done
  {
    echo "# machine at end: load $(sysctl -n vm.loadavg)"
  } >> "$out"
}

# The FFI tax: the same library through three calling mechanisms.
reps "micro-nullcall" \
  go test -run '^$' -bench 'BenchmarkNullCall' -benchtime "$BENCHTIME" -count 1 -benchmem .

# The read path: one 64-row poll per iteration, through every driver plus the
# cgo reference at both call granularities.
reps "micro-poll" \
  go test -run '^$' -bench 'BenchmarkPoll' -benchtime "$BENCHTIME" -count 1 -benchmem .

# The write path.
reps "micro-append" \
  go test -run '^$' -bench 'BenchmarkAppend' -benchtime "$BENCHTIME" -count 1 -benchmem .

# The read path again with our row-batching shim disabled, so the shim's effect
# is measured rather than asserted.
out="$results/micro-poll-nobatch.txt"
echo
echo "running micro-poll-nobatch x${REPS} -> $out"
{
  echo "# raw output of ${REPS} separate invocations of:"
  echo "#   PUREGO_SQLITE_BATCH=0 go test -run '^\$' -bench BenchmarkPoll/purego -benchtime $BENCHTIME -count 1 -benchmem ."
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
} > "$out"
for i in $(seq 1 "$REPS"); do
  echo "### repetition $i/$REPS  load $(sysctl -n vm.loadavg)" >> "$out"
  PUREGO_SQLITE_BATCH=0 go test -run '^$' -bench 'BenchmarkPoll/purego' \
    -benchtime "$BENCHTIME" -count 1 -benchmem . 2>&1 | tee -a "$out"
  echo >> "$out"
done

# The conformance suite, verbose, so the subject counts are on the record
# rather than hidden behind "ok".
out="$results/conformance.txt"
echo
echo "running conformance -> $out"
{
  echo "# raw output of: go test -v -count=1 -run 'TestDriverConformance|TestWorkloadShapeIsIdentical|TestFFISetupReportsWhatItMeasured' ."
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
} > "$out"
go test -v -count=1 -run 'TestDriverConformance|TestWorkloadShapeIsIdentical|TestFFISetupReportsWhatItMeasured' . 2>&1 | tee -a "$out"

# The binding's own suite, on both read paths, with -v so that every test that
# reports a subject count says it out loud.
out="$results/suite-binding.txt"
echo
echo "running the binding suite on both paths -> $out"
{
  echo "# raw output of, in order:"
  echo "#   go test -v -count=1 ./...                        (row batching active)"
  echo "#   PUREGO_SQLITE_BATCH=0 go test -v -count=1 ./...   (per-call fallback)"
  echo "#   go test -count=1 -race ./...                      (race detector)"
  echo "# run from the repository root, not bench/"
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
} > "$out"
(
  cd ..
  echo "########## batched path"
  go test -v -count=1 ./... 2>&1
  echo
  echo "########## per-call fallback path (PUREGO_SQLITE_BATCH=0)"
  PUREGO_SQLITE_BATCH=0 go test -v -count=1 ./... 2>&1
  echo
  echo "########## race detector"
  go test -count=1 -race ./... 2>&1
) | tee -a "$out" | rg '^(ok|FAIL|---|=== RUN)' | tail -5 || true

echo
echo "raw outputs under $results"
ls -la "$results"
