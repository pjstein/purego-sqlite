#!/usr/bin/env bash
# One Config A run into a distinct file, for use when the machine is genuinely
# quiet. Kept separate from run-bakeoff.sh so it cannot clobber the committed
# dataset, and so the comparison between a contended and an uncontended
# measurement of the same configuration stays available.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"
ROUNDS="${1:-8}"
out="$here/results/contended-fixed-2000-quiet.txt"
js="$here/results/contended-fixed-2000-quiet.json"
cmd=("$here/bin/bakeoff" -duration 30s -warmup 3s -rounds "$ROUNDS" -readers 8 -seed 200000 -writerate 2000 -quietwait 3m)
{
  echo "# raw output of: ${cmd[*]} -json $js"
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# context: run during a deliberately quiet window -- the machine's backup"
  echo "#          daemon was paused and no other builds were running. This is the"
  echo "#          only measurement of the contended workload not taken through"
  echo "#          background load, and it exists to settle the one comparison the"
  echo "#          contended runs could not resolve."
  echo "# machine at start: load $(sysctl -n vm.loadavg)"
  { ps -Aro '%cpu,comm' | head -8; } || true
  echo
} > "$out"
"${cmd[@]}" -json "$js" 2>&1 | tee -a "$out"
{ echo; echo "# machine at end: load $(sysctl -n vm.loadavg)"; } >> "$out"
echo QUIET_WINDOW_COMPLETE
