#!/usr/bin/env bash
# Run the full comparative bake-off and write every raw output under results/.
#
# This script is the invocation of record. Anything reported from the bake-off
# must be derivable from the files it writes; a number that exists only in a
# terminal that has since scrolled away is not a measurement, it is a memory.
#
# Usage:  bench/run-bakeoff.sh [rounds] [duration] [seed-rows]
# Default: 5 rounds, 30s measured per run, 200000 seeded events.
#
# Every driver runs once per round, in round-robin order, so thermal drift and
# whatever else the machine is doing land on all of them rather than on
# whichever went first.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

ROUNDS="${1:-5}"
DURATION="${2:-30s}"
SEED="${3:-200000}"
WARMUP=3s
READERS=8

results="$here/results"
mkdir -p "$results"

# Build the binary first and run that, rather than `go run`. `go run` leaves a
# `go` process alive for the whole benchmark, which both consumes CPU and trips
# the harness's own "is a build running" check.
bin="$here/bin/bakeoff"
mkdir -p "$here/bin"
echo "building $bin"
go build -o "$bin" ./cmd/bakeoff

stamp() { date -u +%Y%m%dT%H%M%SZ; }

# --- wait for the machine to be quiet ----------------------------------------
# This machine hosts other agent lanes and a backup daemon. Measuring through a
# disk-scanning backup is not a benchmark of SQLite, it is a benchmark of what
# is left of the disk, and interleaving does not save you from a contaminant
# that lasts longer than a whole round. So: wait, up to a bound, then proceed
# and say loudly that it did not settle.
# The thresholds are empirical, not aspirational. This machine's idle baseline
# with the other agent lanes present is a load average around 6-8, and rounds
# measured at that baseline reproduced to within 2% of each other. What ruins a
# round is a compiler storm (load above 20) or a disk-scanning backup, so those
# are what the gate waits out. A stricter bar would wait forever and measure
# nothing.
QUIET_LOAD="${QUIET_LOAD:-9.0}"
QUIET_HOG="${QUIET_HOG:-50}"
QUIET_WAIT_SECS="${QUIET_WAIT_SECS:-3600}"

machine_offenders() {
  # Build tools matched on process NAME, not command line: Chrome and VS Code
  # both mention "clang" and "link" in their flags, which made the first
  # version of this check fire constantly and mean nothing.
  ps -Aro '%cpu,comm' | awk -v hog="$QUIET_HOG" '
    { n=$2; sub(/.*\//,"",n)
      if (n ~ /^(go|compile|link|asm|cgo|clang|clang\+\+|cc1|gcc|ld|swift-frontend|swiftc|swift|xcodebuild|cargo|rustc|tsc|ninja|make)$/ && $1>1.0)
        printf "build:%s(%.0f%%) ", n, $1
      else if ($1 >= hog)
        printf "hog:%s(%.0f%%) ", n, $1
    }'
}

wait_for_quiet() {
  local waited=0 offenders load1
  while :; do
    offenders="$(machine_offenders)"
    load1="$(sysctl -n vm.loadavg | awk '{print $2}')"
    if [ -z "$offenders" ] && [ "$(echo "$load1 < $QUIET_LOAD" | bc -l)" = "1" ]; then
      echo "machine quiet after ${waited}s (load1 $load1)"
      QUIET_RESULT="quiet after ${waited}s, load1 $load1"
      return 0
    fi
    if [ "$waited" -ge "$QUIET_WAIT_SECS" ]; then
      echo "WARNING: machine did not settle within ${QUIET_WAIT_SECS}s -- proceeding CONTAMINATED"
      echo "WARNING: load1 $load1, offenders: $offenders"
      QUIET_RESULT="NOT QUIET after ${waited}s -- load1 $load1, offenders: $offenders"
      return 0
    fi
    if [ $((waited % 60)) -eq 0 ]; then
      echo "waiting for quiet (${waited}s): load1 $load1, offenders: ${offenders:-none}"
    fi
    sleep 15
    waited=$((waited + 15))
  done
}

echo "pre-flight: waiting for a quiet machine before measuring"
wait_for_quiet

# --- environment record, written once per invocation -------------------------
env_file="$results/environment.txt"
{
  echo "# Environment for the bake-off run recorded at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "## script invocation"
  echo "bench/run-bakeoff.sh $ROUNDS $DURATION $SEED"
  echo
  echo "## host"
  sw_vers
  sysctl -n machdep.cpu.brand_string
  echo "cores: $(sysctl -n hw.ncpu)"
  echo "memory: $(sysctl -n hw.memsize) bytes"
  uname -a
  echo
  echo "## power and thermal"
  pmset -g ps 2>/dev/null | head -2 || echo "pmset unavailable"
  pmset -g therm 2>/dev/null | head -4 || true
  echo
  echo "## toolchain"
  go version
  echo
  echo "## pinned SQLite (this repository compiles and ships it)"
  cat ../sqlite-version.lock | grep -E '^SQLITE_(VERSION|SHA256|SHA3_256|URL)='
  echo "shipped dylib sha256: $(shasum -a 256 ../lib/darwin_arm64/libsqlite3.dylib | awk '{print $1}')"
  echo "shipped dylib size:   $(wc -c < ../lib/darwin_arm64/libsqlite3.dylib) bytes"
  echo
  echo "## competitor versions (from bench/go.mod resolution)"
  go list -m github.com/ncruces/go-sqlite3 modernc.org/sqlite github.com/ebitengine/purego
  echo
  echo "## quiet gate"
  echo "$QUIET_RESULT"
  echo
  echo "## machine state at the start of this invocation"
  echo "load average: $(sysctl -n vm.loadavg)"
  echo "top CPU consumers:"
  ps -Aro '%cpu,comm' | head -12
} > "$env_file"
echo "wrote $env_file"

run() {
  local label="$1"; shift
  local out="$results/$label.txt"
  local js="$results/$label.json"
  echo
  echo "=============================================================="
  echo "running $label -> $out"
  echo "=============================================================="
  {
    echo "# raw output of: $*"
    echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
  } > "$out"
  "$@" -json "$js" 2>&1 | tee -a "$out"
}

# --- Config A: readers compared at an identical, fixed write rate ------------
# This is the configuration that answers the question, because writer and
# reader throughput trade against each other.
run "contended-fixed-2000" \
  "$bin" -duration "$DURATION" -warmup "$WARMUP" -rounds "$ROUNDS" \
         -readers "$READERS" -seed "$SEED" -writerate 2000

# --- Config B: writer unthrottled -------------------------------------------
# Kept because it shows the trade-off, not because its cross-driver reader
# numbers are comparable. They are not.
run "contended-unthrottled" \
  "$bin" -duration "$DURATION" -warmup "$WARMUP" -rounds "$ROUNDS" \
         -readers "$READERS" -seed "$SEED" -writerate 0

# --- Config C: our binding with row batching disabled ------------------------
# Same fixed write rate as Config A, so the two are directly comparable and the
# difference is the shim and nothing else.
echo
echo "=============================================================="
echo "running contended-fixed-2000-nobatch (PUREGO_SQLITE_BATCH=0)"
echo "=============================================================="
{
  echo "# raw output of: PUREGO_SQLITE_BATCH=0 $bin -duration $DURATION -warmup $WARMUP -rounds $ROUNDS -readers $READERS -seed $SEED -writerate 2000 -only purego"
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
} > "$results/contended-fixed-2000-nobatch.txt"
PUREGO_SQLITE_BATCH=0 "$bin" -duration "$DURATION" -warmup "$WARMUP" -rounds "$ROUNDS" \
  -readers "$READERS" -seed "$SEED" -writerate 2000 -only purego \
  -json "$results/contended-fixed-2000-nobatch.json" 2>&1 | tee -a "$results/contended-fixed-2000-nobatch.txt"

echo
echo "machine state after the final round:"
{
  echo
  echo "## machine state after the final round ($(date -u +%Y-%m-%dT%H:%M:%SZ))"
  echo "load average: $(sysctl -n vm.loadavg)"
  ps -Aro '%cpu,comm' | head -8
} >> "$env_file"

echo "all raw outputs under $results"
ls -la "$results"
