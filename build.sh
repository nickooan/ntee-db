#!/usr/bin/env bash
# Build the nteedb-server binary into bin/ for each target platform.
#
# Usage: ./build.sh [target ...]
#   targets: macos | linux-amd64 | linux-arm64   (default: all three)
#
# The server is pure Go (CGO_ENABLED=0), so every target cross-compiles from
# any host — no Docker or C toolchain needed (unlike nteedb-js/capi/build.sh,
# whose c-shared library must be compiled per platform). Linux binaries are
# fully static.
set -euo pipefail

cd "$(dirname "$0")"                 # repo root
OUT=bin
mkdir -p "$OUT"

# -s -w strips the symbol table and DWARF debug info; -trimpath removes local
# filesystem paths from the binary.
GO_BUILD_FLAGS=(-trimpath -ldflags="-s -w")

build() { # <goos> <goarch>
  local goos=$1 goarch=$2 out="$OUT/nteedb-server-$1-$2"
  echo "Building $out ..."
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build "${GO_BUILD_FLAGS[@]}" -o "$out" ./nteedb-server
  echo "→ $out"
}

targets=("$@")
[[ ${#targets[@]} -eq 0 ]] && targets=(macos linux-amd64 linux-arm64)

for target in "${targets[@]}"; do
  case "$target" in
    macos)       build darwin "$(go env GOARCH)" ;; # native arch of this Mac
    linux-amd64) build linux amd64 ;;
    linux-arm64) build linux arm64 ;;
    *) echo "unknown target: $target (expected macos, linux-amd64, or linux-arm64)" >&2; exit 1 ;;
  esac
done
