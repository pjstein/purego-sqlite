#!/usr/bin/env bash
# Litestream compatibility smoke: replicate a database this binding is writing,
# restore it, and check the restored copy through this binding.
#
# All local, no cloud: a file replica is enough to exercise the same LTX
# pipeline a cloud replica uses.
#
# The question is not whether litestream works. It is whether a database
# written through purego -- our WAL frames, our checkpoint behaviour -- is one
# litestream can follow and reconstruct with the convicted corpus intact.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(dirname "$here")"
cd "$root"

LITESTREAM_VERSION="${LITESTREAM_VERSION:-v0.5.16}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
out="$here/results/litestream-smoke.txt"
mkdir -p "$here/results"

bin="$here/bin/litestream"
if [ ! -x "$bin" ]; then
  echo "installing litestream $LITESTREAM_VERSION"
  GOBIN="$here/bin" go install "github.com/benbjohnson/litestream/cmd/litestream@$LITESTREAM_VERSION"
fi

go build -o "$work/smoke" ./cmd/litestream-smoke

{
  echo "# litestream compatibility smoke"
  echo "# written $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# litestream: $("$bin" version)"
  echo "# sqlite:     $(go run ./cmd/sqliteversion | head -1)"
  echo
} > "$out"

cat > "$work/litestream.yml" <<EOF
dbs:
  - path: $work/app.db
    replicas:
      - type: file
        path: $work/replica
EOF

{
  echo "== seed the database through this binding"
  "$work/smoke" write "$work/app.db" 500

  echo
  echo "== start litestream replicating"
  "$bin" replicate -config "$work/litestream.yml" > "$work/litestream.log" 2>&1 &
  ls_pid=$!
  sleep 4

  echo "== append 2000 more rows through this binding WHILE litestream follows"
  "$work/smoke" write "$work/app.db" 2000
  sleep 6

  echo
  echo "== replica contents"
  find "$work/replica" -type f -name '*.ltx' | wc -l | sed 's/^/  ltx files: /'
  du -sh "$work/replica" | sed 's/^/  replica size: /'

  kill "$ls_pid" 2>/dev/null || true
  wait "$ls_pid" 2>/dev/null || true

  echo
  echo "== restore from the replica into a new file"
  "$bin" restore -config "$work/litestream.yml" -o "$work/restored.db" "$work/app.db" 2>&1 | tail -3 || true

  echo
  echo "== original, verified through this binding"
  "$work/smoke" verify "$work/app.db"

  echo
  echo "== restored, verified through this binding"
  "$work/smoke" verify "$work/restored.db"

  echo
  echo "== litestream log (last lines)"
  tail -5 "$work/litestream.log"
} 2>&1 | tee -a "$out"

echo
echo "wrote $out"
