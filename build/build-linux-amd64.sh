#!/usr/bin/env bash
# Compile the pinned SQLite amalgamation into sqlite3/lib/linux_amd64/libsqlite3.so.
#
# The linux sibling of build-darwin-arm64.sh. The bake-off shipped darwin/arm64
# first and named the missing linux/amd64 build as the largest gap between this
# binding and a general-purpose one; this is it. Same flags.sh, same
# amalgamation, same shim and optional extensions --
# only the four things that are genuinely platform-specific differ, and each is
# marked below so the two scripts can be read against each other:
#
#   1. cc emits an ELF shared object (-shared) rather than a Mach-O dylib.
#   2. the soname is set with -Wl,-soname rather than -install_name.
#   3. there is no codesign step -- ELF has no ad-hoc signature to add.
#   4. there is no deployment-target flag -- the glibc it links against is the
#      build host's, which is the readiness caveat recorded in the README.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(dirname "$here")"
# shellcheck disable=SC1091
source "$root/sqlite-version.lock"
# shellcheck disable=SC1091
source "$here/flags.sh"

goos=linux
goarch=amd64
srcdir="$root/build/upstream/sqlite-amalgamation-${SQLITE_VERSION}"
outdir="$root/sqlite3/lib/${goos}_${goarch}"
out="$outdir/libsqlite3.so"

test -f "$srcdir/sqlite3.c" || { echo "build: run build/fetch.sh first" >&2; exit 1; }
mkdir -p "$outdir"

# Optional extensions, off unless asked for -- the same contract the darwin
# script documents: each contributes a source file and a -D that switches on its
# init call in build/extensions.c, and the Go binding feature-detects at load
# which of them the library it opened actually carries.
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

# build/shim.c is compiled in beside the amalgamation, exactly as on darwin: it
# adds no SQLite behaviour and exists only so the Go binding can fetch a whole
# row in one foreign call. The binding treats it as optional and works without it.
echo "build: SQLite $SQLITE_VERSION + row-batching shim${ext_sources[0]:+ + extensions} -> $out"
# -I"$srcdir" puts the amalgamation's sqlite3.h on the include path: sqlite-vec.c
# does #include "sqlite3.h", and a quote-include searches the including file's
# own directory (the sqlite-vec tree) before the search path -- the header it
# needs lives one directory over, with sqlite3.c.
cc \
  "${SQLITE_CFLAGS[@]}" \
  "${SQLITE_DEFINES[@]}" \
  -I"$srcdir" \
  -shared \
  -Wl,-soname,libsqlite3.so \
  -o "$out" \
  "$srcdir/sqlite3.c" \
  "$root/build/shim.c" \
  "${ext_defines[@]+"${ext_defines[@]}"}" \
  "${ext_sources[@]+"${ext_sources[@]}"}" \
  -lm

echo "build: $(ls -lh "$out" | awk '{print $5}')  $(sha256sum "$out" | awk '{print $1}')"
