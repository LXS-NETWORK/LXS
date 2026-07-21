#!/usr/bin/env bash
# build.sh — cross-compile the LXS node as a static linux/amd64 binary for a VPS.
#
# The node uses NO cgo (pebble and libp2p are pure Go), so CGO_ENABLED=0 yields a
# single statically-linked ELF that runs on any modern x86-64 Linux with zero
# shared-library dependencies — no libc-version surprises on the server. That is
# the whole reason deployment is "copy one file", not "match the toolchain".
set -euo pipefail

cd "$(dirname "$0")/.."
out=${1:-deploy/lxs-linux-amd64}

# -trimpath strips local filesystem paths from the binary; -s -w drop the symbol
# and DWARF tables to shrink it. Neither changes behaviour — they make the
# artifact smaller and less machine-specific.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -tags "pebble,libp2p" -trimpath -ldflags "-s -w" -o "$out" ./cmd/lxs

echo "built $out"
file "$out" 2>/dev/null || true
if command -v sha256sum >/dev/null; then sha256sum "$out"; else shasum -a 256 "$out"; fi
