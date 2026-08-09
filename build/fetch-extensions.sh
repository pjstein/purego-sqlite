#!/usr/bin/env bash
# Fetch and checksum-verify the optional extension sources named in
# build/extensions.lock. Only fetches what the caller asked for.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(dirname "$here")"
# shellcheck disable=SC1091
source "$root/build/extensions.lock"

upstream="$root/build/upstream"
mkdir -p "$upstream"

fetch_verify() {
  local name="$1" url="$2" want="$3" file="$upstream/$4"
  if [ ! -f "$file" ]; then
    echo "extensions: downloading $name"
    curl -fsSL --max-time 300 -o "$file" "$url"
  fi
  local got
  got="$(shasum -a 256 "$file" | awk '{print $1}')"
  if [ "$got" != "$want" ]; then
    echo "extensions: SHA256 MISMATCH for $name" >&2
    echo "  expected $want" >&2
    echo "  actual   $got" >&2
    exit 1
  fi
  echo "extensions: $name verified"
}

if [ "${WITH_SQLITE_VEC:-0}" = "1" ]; then
  dir="$upstream/sqlite-vec-${SQLITE_VEC_VERSION}"
  if [ ! -f "$dir/sqlite-vec.c" ]; then
    fetch_verify "sqlite-vec ${SQLITE_VEC_VERSION}" "$SQLITE_VEC_URL" "$SQLITE_VEC_SHA256" "sqlite-vec-${SQLITE_VEC_VERSION}.tar.gz"
    mkdir -p "$dir"
    tar xzf "$upstream/sqlite-vec-${SQLITE_VEC_VERSION}.tar.gz" -C "$dir"
  fi
  test -f "$dir/sqlite-vec.c" || { echo "extensions: sqlite-vec.c missing after unpack" >&2; exit 1; }
  echo "extensions: sqlite-vec ready at $dir"
fi
