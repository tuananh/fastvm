#!/usr/bin/env bash
# Fetch Firecracker-compatible kernel and an Alpine rootfs for e2e tests.
# Cached under test/e2e/assets (gitignored). Safe to re-run.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)/assets"
mkdir -p "$DIR"

if [[ ! -f "$DIR/vmlinux.bin" ]]; then
  echo "downloading Firecracker quickstart kernel..."
  curl -fsSL -o "$DIR/vmlinux.bin.tmp" \
    https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin
  mv "$DIR/vmlinux.bin.tmp" "$DIR/vmlinux.bin"
fi

if [[ ! -d "$DIR/rootfs" ]]; then
  echo "downloading Alpine minirootfs..."
  curl -fsSL -o "$DIR/alpine.tgz.tmp" \
    https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.3-x86_64.tar.gz
  mkdir -p "$DIR/rootfs"
  tar -xzf "$DIR/alpine.tgz.tmp" -C "$DIR/rootfs"
  rm -f "$DIR/alpine.tgz.tmp"
fi

echo "e2e assets ready:"
echo "  FASTVM_KERNEL=$DIR/vmlinux.bin"
echo "  FASTVM_ROOTFS=$DIR/rootfs"
