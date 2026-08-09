#!/usr/bin/env bash
# Interleaved A/B of the row-batching shim: batching on against batching off,
# alternating round by round in a single sitting.
#
# Why this exists separately. Batching is decided when the library loads, so
# one process cannot measure both; the two configurations have to be two
# processes. Running them as two separate multi-round invocations -- config A
# now, config C twenty minutes later -- makes the comparison a
# cross-invocation one, and this machine's load moves enough over twenty
# minutes to account for a difference all by itself.
#
# So the rounds alternate: on, off, on, off. Each pair sits seconds apart
# rather than tens of minutes, and whatever the machine is doing lands on both
# halves of the comparison.
#
# Usage:  bench/run-batching-compare.sh [rounds] [duration] [seed-rows]
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
bin="$here/bin/bakeoff"
mkdir -p "$here/bin"
go build -o "$bin" ./cmd/bakeoff

out="$results/batching-ab.txt"
{
  echo "# Interleaved A/B of the row-batching shim"
  echo "# rounds alternate: batching ON, batching OFF, ON, OFF, ..."
  echo "# each round is a separate one-round invocation of:"
  echo "#   $bin -duration $DURATION -warmup $WARMUP -rounds 1 -readers $READERS -seed $SEED -writerate 2000 -only purego"
  echo "# with PUREGO_SQLITE_BATCH unset (on) or =0 (off)"
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
} > "$out"

for i in $(seq 1 "$ROUNDS"); do
  for mode in on off; do
    js="$results/batching-ab-${mode}-${i}.json"
    {
      echo "########## pair $i/$ROUNDS  batching=$mode  load $(sysctl -n vm.loadavg)"
    } >> "$out"
    if [ "$mode" = off ]; then
      PUREGO_SQLITE_BATCH=0 "$bin" -duration "$DURATION" -warmup "$WARMUP" -rounds 1 \
        -readers "$READERS" -seed "$SEED" -writerate 2000 -only purego -json "$js" 2>&1 | tee -a "$out"
    else
      "$bin" -duration "$DURATION" -warmup "$WARMUP" -rounds 1 \
        -readers "$READERS" -seed "$SEED" -writerate 2000 -only purego -json "$js" 2>&1 | tee -a "$out"
    fi
    echo >> "$out"
  done
done

echo "wrote $out and per-pair JSON alongside it"
