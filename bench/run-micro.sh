#!/usr/bin/env bash
# Run the micro-benchmarks and write their raw output under results/.
#
# Same rule as the comparative run: every number reported must be derivable
# from a file this script wrote.
#
# Usage:  bench/run-micro.sh [count] [benchtime]
# Default: 5 repetitions per benchmark, 3s each.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

COUNT="${1:-5}"
BENCHTIME="${2:-3s}"
results="$here/results"
mkdir -p "$results"

# `go test -bench` interleaves repetitions itself when -count is used: it runs
# every benchmark once, then repeats the whole set. That is the ordering the
# comparative run has to be told to do explicitly.
run() {
  local label="$1"; shift
  local out="$results/$label.txt"
  echo
  echo "running $label -> $out"
  {
    echo "# raw output of: $*"
    echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "# machine at start: load $(sysctl -n vm.loadavg)"
    echo
  } > "$out"
  "$@" 2>&1 | tee -a "$out"
  {
    echo
    echo "# machine at end: load $(sysctl -n vm.loadavg)"
  } >> "$out"
}

# The FFI tax: the same library through three calling mechanisms.
run "micro-nullcall" \
  go test -run '^$' -bench 'BenchmarkNullCall' -benchtime "$BENCHTIME" -count "$COUNT" -benchmem .

# The read path: one 64-row poll per iteration, through every driver plus the
# cgo reference at both call granularities.
run "micro-poll" \
  go test -run '^$' -bench 'BenchmarkPoll' -benchtime "$BENCHTIME" -count "$COUNT" -benchmem .

# The read path again with our row-batching shim disabled, so the shim's effect
# is measured rather than asserted.
echo
echo "running micro-poll-nobatch -> $results/micro-poll-nobatch.txt"
{
  echo "# raw output of: PUREGO_SQLITE_BATCH=0 go test -run ^\$ -bench BenchmarkPoll/purego -benchtime $BENCHTIME -count $COUNT -benchmem ."
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# machine at start: load $(sysctl -n vm.loadavg)"
  echo
} > "$results/micro-poll-nobatch.txt"
PUREGO_SQLITE_BATCH=0 go test -run '^$' -bench 'BenchmarkPoll/purego' \
  -benchtime "$BENCHTIME" -count "$COUNT" -benchmem . 2>&1 | tee -a "$results/micro-poll-nobatch.txt"

# The write path.
run "micro-append" \
  go test -run '^$' -bench 'BenchmarkAppend' -benchtime "$BENCHTIME" -count "$COUNT" -benchmem .

# The conformance suite, verbose, so the subject counts are on the record
# rather than hidden behind "ok".
run "conformance" \
  go test -v -count=1 -run 'TestDriverConformance|TestWorkloadShapeIsIdentical|TestFFISetupReportsWhatItMeasured' .

echo
echo "raw outputs under $results"
