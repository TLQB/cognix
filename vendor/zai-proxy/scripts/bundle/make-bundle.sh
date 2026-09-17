#!/usr/bin/env bash
# Builds the Freeclaude all-in-one release bundle.
#
# Output: dist/freeclaude-<platform>-x64/
#   freeclaude[.exe]     TUI binary (Freebuff fork, proxy URL baked at build)
#   tree-sitter.wasm    sibling asset required by the TUI binary
#   zai-proxy[.exe]     Go proxy binary
#   start.sh / start.bat  single-command launcher
#
# Usage:
#   ./make-bundle.sh [version] [platform]    # platform: linux (default) | windows
#   VERSION=1.2.3 PLATFORM=windows ./make-bundle.sh
#
# Env:
#   FREEBUFF_SRC     freebuff-src checkout (default ~/Desktop/freebuff-src)
#   PROXY_URL        URL baked into the TUI binary (default http://localhost:3002)
#   SKIP_TUI=1       skip TUI build, reuse existing cli/bin binary
#   SKIP_PROXY=1     skip Go build, reuse existing /tmp/zai-proxy-bundle[.exe]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEFAULT_FREEBUFF_SRC="$HOME/Desktop/freebuff-src"
FREEBUFF_SRC="${FREEBUFF_SRC:-$DEFAULT_FREEBUFF_SRC}"
if [ ! -d "$FREEBUFF_SRC" ] && [ -d "$SCRIPT_DIR/../../../freebuff-src" ]; then
  FREEBUFF_SRC="$SCRIPT_DIR/../../../freebuff-src"
fi
VERSION="${1:-${VERSION:-1.0.0}}"
PLATFORM="${2:-${PLATFORM:-linux}}"
PROXY_URL="${PROXY_URL:-http://localhost:3002}"
BUN="${BUN:-$HOME/.local/share/reflex/bun/bin/bun}"
OUT_REL="freeclaude-$PLATFORM-x64"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

ZAI_PROXY_DIR="$SCRIPT_DIR/../.."
[ -f "$ZAI_PROXY_DIR/go.mod" ] || ZAI_PROXY_DIR="$SCRIPT_DIR/../../.."
[ -f "$ZAI_PROXY_DIR/go.mod" ] || { echo "Error: go.mod not found near $SCRIPT_DIR"; exit 1; }

[ -x "$BUN" ] || BUN="$(command -v bun)"
[ -x "$BUN" ] || { echo "Error: bun >= 1.3 not found (set BUN=...)"; exit 1; }

case "$PLATFORM" in
  linux)
    GOOS=linux; GOARCH=amd64
    BUN_TARGET=bun-linux-x64; NF_PLATFORM=linux; NF_ARCH=x64
    PROXY_OUT=/tmp/zai-proxy-bundle; TUI_NAME=freeclaude
    LAUNCHER=start.sh; PROXY_BIN_NAME=zai-proxy
    ARCHIVE_EXT=tar.gz
    ;;
  windows)
    GOOS=windows; GOARCH=amd64
    BUN_TARGET=bun-windows-x64; NF_PLATFORM=win32; NF_ARCH=x64
    PROXY_OUT=/tmp/zai-proxy-bundle.exe; TUI_NAME=freeclaude
    LAUNCHER=start.bat; PROXY_BIN_NAME=zai-proxy.exe
    ARCHIVE_EXT=zip
    ;;
  *)
    echo "Error: unsupported platform '$PLATFORM' (use linux|windows)"; exit 1 ;;
esac

echo "[1/4] Building zai-proxy ($GOOS $GOARCH)..."
if [ "${SKIP_PROXY:-0}" = "1" ] && [ -x "$PROXY_OUT" ]; then
  echo "      (skipped, reusing $PROXY_OUT)"
else
  ( cd "$ZAI_PROXY_DIR" && CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
      go build -trimpath -ldflags="-s -w" -o "$PROXY_OUT" . )
fi
[ -x "$PROXY_OUT" ] || { echo "Error: proxy binary missing"; exit 1; }

# qwen-proxy (Qwen bridge) — bundled alongside zai-proxy. The bundle runs it
# on :3003 and zai-proxy merges its models into /v1/models via QWEN_PROXY_URL.
# Optional: set QWEN_NO_BUNDLE=1 to skip (pure Z.AI bundle).
QBLESS_EXT="$([ "$PLATFORM" = windows ] && echo '.exe' || true)"
QWEN_OUT="/tmp/qwen-proxy-bundle$QBLESS_EXT"
if [ "${QWEN_NO_BUNDLE:-0}" != "1" ]; then
  echo "[1.4/4] Building qwen-proxy ($GOOS $GOARCH)..."
  if [ -d "$ZAI_PROXY_DIR/qwen-bridge" ]; then
    ( cd "$ZAI_PROXY_DIR/qwen-bridge" && CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
        go build -trimpath -ldflags="-s -w" -o "$QWEN_OUT" . )
    [ -x "$QWEN_OUT" ] || { echo "Error: qwen-proxy build failed"; exit 1; }
  else
    echo "      (qwen-bridge/ absent — building a pure Z.AI bundle)"
  fi
fi

# Build q-bless helper for both bundles. start.sh/start.bat use it for
# first-run bless (Windows) and the proxy's auto-rebless goroutine needs
# the binary at runtime to recover from Q death without user
# intervention (QBLESS_AUTO_REBLESS=1) — so Linux needs it shipped too.
# NOTE the `|| true`: on linux the test exits 1 and set -e would otherwise
# kill the script silently right after the proxy build.
QBLESS_OUT="/tmp/q-bless-bundle$QBLESS_EXT"
echo "[1.5/4] Building q-bless ($PLATFORM helper, cross-compiled)..."
(
  cd "$ZAI_PROXY_DIR"
  CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
    go build -trimpath -ldflags="-s -w" -o "$QBLESS_OUT" ./cmd/q-bless
)
[ -f "$QBLESS_OUT" ] || { echo "Error: q-bless build failed"; exit 1; }

TUI_BIN="$FREEBUFF_SRC/cli/bin/$TUI_NAME$([ "$PLATFORM" = windows ] && echo '.exe' || true)"
WASM="$FREEBUFF_SRC/cli/bin/tree-sitter.wasm"
echo "[2/4] Building Freeclaude TUI binary (this takes ~40-90s)..."
if [ "${SKIP_TUI:-0}" = "1" ] && [ -f "$TUI_BIN" ]; then
  echo "      (skipped, reusing $TUI_BIN)"
else
  (
    cd "$FREEBUFF_SRC"
    FREEBUFF_MODE=true \
    OVERRIDE_TARGET=$BUN_TARGET OVERRIDE_PLATFORM=$NF_PLATFORM OVERRIDE_ARCH=$NF_ARCH \
    NEXT_PUBLIC_CB_ENVIRONMENT=prod \
    NEXT_PUBLIC_CODEBUFF_APP_URL="$PROXY_URL" \
    NEXT_PUBLIC_SUPPORT_EMAIL=support@example.com \
    NEXT_PUBLIC_POSTHOG_API_KEY=phc_local_stub \
    NEXT_PUBLIC_POSTHOG_HOST_URL="$PROXY_URL" \
    NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY=pk_test_stub \
    NEXT_PUBLIC_STRIPE_CUSTOMER_PORTAL="$PROXY_URL" \
    NEXT_PUBLIC_WEB_PORT=3000 \
    "$BUN" cli/scripts/build-binary.ts freeclaude "$VERSION"
    # build-binary writes cli/bin/freeclaude[.exe] directly — no rename needed.
  )
fi
[ -f "$TUI_BIN" ] || { echo "Error: TUI binary missing at $TUI_BIN"; exit 1; }
[ -f "$WASM" ] || { echo "Error: tree-sitter.wasm missing at $WASM"; exit 1; }

echo "[3/4] Staging bundle..."
mkdir -p "$STAGE/$OUT_REL"
cp "$PROXY_OUT"               "$STAGE/$OUT_REL/$PROXY_BIN_NAME"
cp "$TUI_BIN"                 "$STAGE/$OUT_REL/freeclaude$([ "$PLATFORM" = windows ] && echo '.exe' || true)"
# Pack q-bless into the bundle: proxy auto-rebless uses it at runtime.
cp "$QBLESS_OUT"              "$STAGE/$OUT_REL/q-bless$QBLESS_EXT"
if [ -x "$QWEN_OUT" ]; then
  # Qwen bridge binary (optional — built when qwen-bridge/ exists).
  cp "$QWEN_OUT"               "$STAGE/$OUT_REL/qwen-proxy$QBLESS_EXT"
fi
cp "$WASM"                    "$STAGE/$OUT_REL/tree-sitter.wasm"
cp "$SCRIPT_DIR/$LAUNCHER"    "$STAGE/$OUT_REL/$LAUNCHER"
if [ "$PLATFORM" = linux ]; then
  cp "$SCRIPT_DIR/README.md"  "$STAGE/$OUT_REL/README.md"
  chmod +x "$STAGE/$OUT_REL/start.sh" "$STAGE/$OUT_REL/freeclaude" "$STAGE/$OUT_REL/zai-proxy"
  [ -f "$STAGE/$OUT_REL/qwen-proxy" ] && chmod +x "$STAGE/$OUT_REL/qwen-proxy"
fi
echo "$VERSION" > "$STAGE/$OUT_REL/VERSION"

echo "[4/4] Packing archive..."
OUT_DIR="$ZAI_PROXY_DIR/dist"
rm -rf "$OUT_DIR/$OUT_REL"
mkdir -p "$OUT_DIR"
mv "$STAGE/$OUT_REL" "$OUT_DIR/$OUT_REL"
if [ "$PLATFORM" = windows ]; then
  ( cd "$OUT_DIR" && zip -qr "$OUT_REL-$VERSION.zip" "$OUT_REL" )
  ARCHIVE="$OUT_DIR/$OUT_REL-$VERSION.zip"
else
  tar -C "$OUT_DIR" -czf "$OUT_DIR/$OUT_REL-$VERSION.tar.gz" "$OUT_REL"
  ARCHIVE="$OUT_DIR/$OUT_REL-$VERSION.tar.gz"
fi

echo
echo "✅ Bundle ready: $OUT_DIR/$OUT_REL/"
ls -lh "$OUT_DIR/$OUT_REL" | sed 1d
echo "   Archive: $ARCHIVE ($(du -h "$ARCHIVE" | cut -f1))"
echo "   Next:   cd $OUT_DIR/$OUT_REL && ./start.sh (linux) or start.bat (windows)"
