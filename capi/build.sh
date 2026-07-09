#!/usr/bin/env bash
# Build the nteedb c-shared library for the host platform into the Node package's
# prebuilds/<os>-<arch>/ directory. Run this on each target OS (no cross-compile).
set -euo pipefail

cd "$(dirname "$0")"                 # ntee-db/capi
OUT_ROOT="../ntee-db-js/prebuilds"

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"

case "$goos" in
  darwin)  ext="dylib" ;;
  linux)   ext="so" ;;
  windows) ext="dll" ;;
  *) echo "unsupported GOOS: $goos" >&2; exit 1 ;;
esac

dir="$OUT_ROOT/${goos}-${goarch}"
mkdir -p "$dir"

echo "Building libnteedb.$ext for ${goos}-${goarch} ..."
# -s -w strips the symbol table and DWARF debug info (~30-40% smaller); the
# library is only ever called through its C ABI, so they are dead weight.
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o "$dir/libnteedb.$ext" .
echo "→ $dir/libnteedb.$ext"
