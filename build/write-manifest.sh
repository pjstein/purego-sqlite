#!/usr/bin/env bash
# Regenerate sqlite3/lib/MANIFEST.txt from the libraries actually present on disk.
#
# The manifest is what lets a consumer answer one question without a compiler:
# "is the binary in my module cache the binary this repository says it is?"
# Hashing needs no toolchain, so this runs correctly on any host regardless of
# which platform's library was last rebuilt -- every committed library is
# hashed, not only the one just built.
#
# What the manifest deliberately does NOT claim is bit-for-bit reproducibility.
# Two different clang versions will not emit an identical dylib from identical
# input, and pretending otherwise would be a promise this repository cannot
# keep. The reproducible facts are the inputs: the pinned amalgamation and its
# two upstream hashes, the extension pin, and the flag list -- all committed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(dirname "$here")"
# shellcheck disable=SC1091
source "$root/sqlite-version.lock"
# shellcheck disable=SC1091
source "$root/build/extensions.lock"

out="$root/sqlite3/lib/MANIFEST.txt"

sha256() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

size() { wc -c < "$1" | tr -d ' '; }

{
  echo "# Shipped SQLite libraries -- what they are and how to check them."
  echo "#"
  echo "# Regenerate with \`make manifest\`. Rebuild with \`make ship-lib\`, which"
  echo "# builds the configuration recorded below and then rewrites this file."
  echo "#"
  echo "# To verify a library yourself:"
  echo "#     shasum -a 256 sqlite3/lib/darwin_arm64/libsqlite3.dylib"
  echo "# and compare against the sha256 line for that platform."
  echo
  echo "sqlite-version    $SQLITE_VERSION"
  echo "sqlite-sha256     $SQLITE_SHA256"
  echo "sqlite-sha3-256   $SQLITE_SHA3_256"
  echo "sqlite-vec        $SQLITE_VEC_VERSION"
  echo "sqlite-vec-sha256 $SQLITE_VEC_SHA256"
  echo "build-recipe      WITH_SQLITE_VEC=1 make build   (i.e. \`make ship-lib\`)"
  echo "compile-flags     build/flags.sh, plus build/shim.c and build/extensions.c"
  echo

  found=0
  for lib in "$root"/sqlite3/lib/*/libsqlite3.dylib "$root"/sqlite3/lib/*/libsqlite3.so; do
    [ -e "$lib" ] || continue
    found=1
    rel="${lib#"$root"/}"
    platform="$(basename "$(dirname "$lib")")"
    echo "[$platform]"
    echo "  path    $rel"
    echo "  size    $(size "$lib") bytes"
    echo "  sha256  $(sha256 "$lib")"
    echo
  done

  if [ "$found" -eq 0 ]; then
    echo "# No libraries present. Run \`make ship-lib\`." >&2
    exit 1
  fi
} > "$out"

echo "manifest: wrote ${out#"$root"/}"
