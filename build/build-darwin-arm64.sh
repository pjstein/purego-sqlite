#!/usr/bin/env bash
# Compile the pinned SQLite amalgamation into lib/darwin_arm64/libsqlite3.dylib.
#
# One script per platform triple, each writing into lib/<goos>_<goarch>/. Adding
# linux-amd64 or linux-arm64 means adding a sibling script and a sibling
# directory; nothing here needs to become a matrix first.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(dirname "$here")"
# shellcheck disable=SC1091
source "$root/sqlite-version.lock"
# shellcheck disable=SC1091
source "$here/flags.sh"

goos=darwin
goarch=arm64
srcdir="$root/build/upstream/sqlite-amalgamation-${SQLITE_VERSION}"
outdir="$root/lib/${goos}_${goarch}"
out="$outdir/libsqlite3.dylib"

test -f "$srcdir/sqlite3.c" || { echo "build: run build/fetch.sh first" >&2; exit 1; }
mkdir -p "$outdir"

# Optional extensions, off unless asked for. Each contributes a source file and
# a -D that switches on its init call in build/extensions.c; the Go binding
# detects at load which of them the library it opened actually carries, so one
# package works against a plain build and an extended one alike.
ext_sources=()
ext_defines=()
if [ "${WITH_SQLITE_VEC:-0}" = "1" ]; then
  # shellcheck disable=SC1091
  source "$root/build/extensions.lock"
  "$root/build/fetch-extensions.sh"
  ext_sources+=("$root/build/upstream/sqlite-vec-${SQLITE_VEC_VERSION}/sqlite-vec.c")
  ext_defines+=(-DSQLITE_VEC_ENABLED -DSQLITE_VEC_STATIC -DSQLITE_CORE)
fi
if [ ${#ext_sources[@]} -gt 0 ]; then
  ext_sources+=("$root/build/extensions.c")
fi

# Deployment target is stated rather than inherited so the same dylib is
# produced on any macOS host new enough to build it.
target=(-arch arm64 -mmacosx-version-min=13.0)

# build/shim.c is compiled in beside the amalgamation. It adds no SQLite
# behaviour -- it calls the same public API in the same order -- and exists
# only to let the Go binding fetch a whole row in one foreign call instead of
# seven. The binding treats it as optional and works without it.
echo "build: SQLite $SQLITE_VERSION + row-batching shim${ext_sources[0]:+ + extensions} -> $out"
cc "${target[@]}" \
  "${SQLITE_CFLAGS[@]}" \
  "${SQLITE_DEFINES[@]}" \
  -dynamiclib \
  -install_name "@rpath/libsqlite3.dylib" \
  -compatibility_version 1.0.0 \
  -current_version "${SQLITE_VERSION}" \
  -o "$out" \
  "$srcdir/sqlite3.c" \
  "$root/build/shim.c" \
  "${ext_defines[@]+"${ext_defines[@]}"}" \
  "${ext_sources[@]+"${ext_sources[@]}"}"

# An unsigned dylib loads fine when this process wrote it, but ad-hoc signing
# keeps it well-formed for anything that inspects signatures later.
codesign --force --sign - "$out" 2>/dev/null || true

echo "build: $(ls -lh "$out" | awk '{print $5}')  $(shasum -a 256 "$out" | awk '{print $1}')"
