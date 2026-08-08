#!/usr/bin/env bash
# Fetch and verify the pinned upstream SQLite amalgamation.
#
# The unpacked source lands in build/upstream/ which is gitignored: the
# repository ships the compiled dylib and the pin, not two megabytes of C.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(dirname "$here")"
# shellcheck disable=SC1091
source "$root/sqlite-version.lock"

upstream="$root/build/upstream"
srcdir="$upstream/sqlite-amalgamation-${SQLITE_VERSION}"
mkdir -p "$upstream"

if [ -f "$srcdir/sqlite3.c" ]; then
  echo "fetch: $srcdir already present"
  exit 0
fi

zip="$upstream/$SQLITE_ZIP"
if [ ! -f "$zip" ]; then
  echo "fetch: downloading $SQLITE_URL"
  curl -fsSL --max-time 300 -o "$zip" "$SQLITE_URL"
fi

echo "fetch: verifying SHA256"
actual_sha256="$(shasum -a 256 "$zip" | awk '{print $1}')"
if [ "$actual_sha256" != "$SQLITE_SHA256" ]; then
  echo "fetch: SHA256 MISMATCH" >&2
  echo "  expected $SQLITE_SHA256" >&2
  echo "  actual   $actual_sha256" >&2
  exit 1
fi

# sqlite.org publishes SHA3-256 in its download manifest, so that is the hash
# upstream actually attests to. Check it too when a SHA3 implementation exists.
if command -v openssl >/dev/null 2>&1 && openssl dgst -sha3-256 /dev/null >/dev/null 2>&1; then
  echo "fetch: verifying SHA3-256 (upstream's published hash)"
  actual_sha3="$(openssl dgst -sha3-256 "$zip" | awk '{print $NF}')"
  if [ "$actual_sha3" != "$SQLITE_SHA3_256" ]; then
    echo "fetch: SHA3-256 MISMATCH" >&2
    echo "  expected $SQLITE_SHA3_256" >&2
    echo "  actual   $actual_sha3" >&2
    exit 1
  fi
else
  echo "fetch: no SHA3-256 available, skipped (SHA256 verified)" >&2
fi

echo "fetch: unpacking"
rm -rf "$srcdir"
unzip -q -d "$upstream" "$zip"

# The zip unpacks as sqlite-amalgamation-3530400; normalise to the dotted
# version so the rest of the build does not have to know the encoding.
packed="$upstream/${SQLITE_ZIP%.zip}"
if [ -d "$packed" ] && [ "$packed" != "$srcdir" ]; then
  mv "$packed" "$srcdir"
fi

test -f "$srcdir/sqlite3.c" || { echo "fetch: sqlite3.c missing after unpack" >&2; exit 1; }
echo "fetch: SQLite $SQLITE_VERSION ready at $srcdir"
