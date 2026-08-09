#!/usr/bin/env bash
# The callback optimization ladder, measured with the same discipline as
# everything else: five passes, interleaved by invocation rather than by
# -count, each pass touching every configuration once.
#
# The portable and marshaled paths are chosen at library load, so they cannot
# both live in one process. They alternate pass by pass instead, seconds apart,
# so machine drift lands on both.
#
# Within the marshaled process the rungs separate further: C marshaling with
# the original map-under-a-lock dispatch, against C marshaling with slice-index
# dispatch. The aggregate is measured in BOTH processes, because whether the
# scalar path underneath it matters is a question to answer rather than assume.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"
PASSES="${1:-5}"
BENCHTIME="${2:-2s}"
out="$here/results/micro-callback-ladder.txt"
mkdir -p "$here/results"
{
  echo "# callback optimization ladder"
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# ${PASSES} passes; each pass runs marshaled, then portable, then the floor"
  echo "# marshaled/portable alternate by invocation because the path is fixed at library load"
  echo
} > "$out"
for i in $(seq 1 "$PASSES"); do
  {
    echo "### pass $i/$PASSES  load $(sysctl -n vm.loadavg)"
    # `|| true` on every grep: an empty match must not kill the run under
    # `set -e`. It already did once, when the benchmarks were renamed and a
    # stale filter matched nothing -- a silent-looking failure of exactly the
    # kind this whole harness exists to prevent.
    { go test -run '^$' -bench BenchmarkSQLFunction -benchtime "$BENCHTIME" -count 1 -benchmem . 2>&1 | grep Benchmark; } || true
    { PUREGO_SQLITE_FUNCTIONS=portable go test -run '^$' -bench BenchmarkSQLFunction -benchtime "$BENCHTIME" -count 1 -benchmem . 2>&1 | grep Benchmark; } || true
    { go test -run '^$' -bench BenchmarkCallbackFloor -benchtime "$BENCHTIME" -count 1 . 2>&1 | grep Benchmark; } || true
    echo
  } >> "$out"
done
echo "# machine at end: load $(sysctl -n vm.loadavg)" >> "$out"
echo "wrote $out"
