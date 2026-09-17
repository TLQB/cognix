#!/usr/bin/env bash
# Builds the vendored zai-proxy sidecar binary for embedding into cognix.
# Usage: scripts/build-zai-proxy-sidecar.sh [target-triple ...]
#        (default: host target; outputs crates/zai_proxy_sidecar/binaries/)
set -euo pipefail
cd "$(dirname "$0")/.."

TARGETS=("$@")
if [ ${#TARGETS[@]} -eq 0 ]; then
  TARGETS=("$(rustc -vV | awk '/host:/ {print $2}')")
fi

for triple in "${TARGETS[@]}"; do
  case "$triple" in
    *linux*)  goos=linux ;;
    *darwin*) goos=darwin ;;
    *windows*) goos=windows ;;
    *) echo "unsupported triple: $triple" >&2; exit 1 ;;
  esac
  case "$triple" in
    *aarch64*|*arm64*) goarch=arm64 ;;
    *)                 goarch=amd64 ;;
  esac
  out="crates/zai_proxy_sidecar/binaries/zai-proxy-${goos}-${goarch}"
  echo "[*] building $out (GOOS=$goos GOARCH=$goarch)"
  (cd vendor/zai-proxy && GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "../../$out" .)
  qout="crates/zai_proxy_sidecar/binaries/q-bless-${goos}-${goarch}"
  echo "[*] building $qout"
  (cd vendor/zai-proxy && GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "../../$qout" ./cmd/q-bless)
 done
echo "[✓] sidecar binaries ready"
