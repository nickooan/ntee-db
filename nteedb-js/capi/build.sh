#!/usr/bin/env bash
# Build the nteedb c-shared library into the Node package's prebuilds/<os>-<arch>/
# directory.
#
# Usage: ./build.sh [target ...]
#   targets: macos | linux-amd64 | linux-arm64   (default: all three)
#
# macOS builds run on the host (must be run on a Mac); Linux builds run inside
# the official golang Docker image with --platform, so each arch compiles
# natively with the image's own gcc — no cross C toolchain required. The
# non-native arch runs under Rosetta/QEMU emulation.
set -euo pipefail

cd "$(dirname "$0")"                 # ntee-db/nteedb-js/capi
REPO_ROOT="$(cd ../.. && pwd)"
OUT_ROOT="../prebuilds"
GO_IMAGE="${GO_IMAGE:-golang:1.25-bookworm}"

# -s -w strips the symbol table and DWARF debug info (~30-40% smaller); the
# library is only ever called through its C ABI, so they are dead weight.
GO_BUILD_FLAGS=(-trimpath -ldflags="-s -w" -buildmode=c-shared)

build_macos() {
  if [[ "$(uname -s)" != "Darwin" ]]; then
    echo "skipping macos build: not running on a Mac" >&2
    return 0
  fi
  local goarch dir
  goarch="$(go env GOARCH)"
  dir="$OUT_ROOT/darwin-${goarch}"
  mkdir -p "$dir"
  echo "Building libnteedb.dylib for darwin-${goarch} (host) ..."
  CGO_ENABLED=1 go build "${GO_BUILD_FLAGS[@]}" -o "$dir/libnteedb.dylib" .
  echo "→ $dir/libnteedb.dylib"
}

build_linux() {
  local arch="$1" dir
  dir="$OUT_ROOT/linux-${arch}"
  mkdir -p "$dir"
  echo "Building libnteedb.so for linux-${arch} (docker, $GO_IMAGE) ..."
  docker run --rm \
    --platform "linux/${arch}" \
    --user "$(id -u):$(id -g)" \
    -v "$REPO_ROOT":/src \
    -w /src/nteedb-js/capi \
    -e CGO_ENABLED=1 \
    -e GOCACHE=/tmp/gocache \
    -e GOMODCACHE=/tmp/gomodcache \
    "$GO_IMAGE" \
    go build "${GO_BUILD_FLAGS[@]}" -o "../prebuilds/linux-${arch}/libnteedb.so" .
  echo "→ $dir/libnteedb.so"
}

targets=("$@")
[[ ${#targets[@]} -eq 0 ]] && targets=(macos linux-amd64 linux-arm64)

for target in "${targets[@]}"; do
  case "$target" in
    macos)       build_macos ;;
    linux-amd64) build_linux amd64 ;;
    linux-arm64) build_linux arm64 ;;
    *) echo "unknown target: $target (expected macos, linux-amd64, or linux-arm64)" >&2; exit 1 ;;
  esac
done
