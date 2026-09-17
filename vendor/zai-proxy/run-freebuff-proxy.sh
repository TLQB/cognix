#!/usr/bin/env bash
# Launches zai-proxy in freebuff-compat mode on port 3002.
#
# Usage:
#   ./run-freebuff-proxy.sh [ZAI_TOKEN]
#   ZAI_TOKEN=<token> ./run-freebuff-proxy.sh
#   echo <token> > ~/.config/zai-proxy/token && ./run-freebuff-proxy.sh
#
# Env:
#   PORT          listen port (default 3002)
#   ZAI_TOKEN     Z.AI auth token (arg / env / config file)
#   LOG_LEVEL     proxy log level (default info; set debug only when
#                 investigating — debug logs every SSE line + full request
#                 bodies, grows to ~100MB+ and slows the hot path)

set -euo pipefail
cd "$(dirname "$0")"

PORT="${PORT:-3002}"
CONFIG_DIR="$HOME/.config/zai-proxy"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export QBLESS_AUTO_REBLESS="${QBLESS_AUTO_REBLESS:-1}"
export QBLESS_BINARY="$SCRIPT_DIR/q-bless"
export QBLESS_FILE="${QBLESS_FILE:-$SCRIPT_DIR/qbless.json}"
TOKEN="${1:-${ZAI_TOKEN:-$(cat "$CONFIG_DIR/token" 2>/dev/null || echo '')}}"

if [ -z "$TOKEN" ]; then
  echo "Error: ZAI_TOKEN required. Provide via one of:"
  echo "  1) ./run-freebuff-proxy.sh <token>"
  echo "  2) ZAI_TOKEN=<token> ./run-freebuff-proxy.sh"
  echo "  3) mkdir -p ~/.config/zai-proxy && echo <token> > ~/.config/zai-proxy/token"
  exit 1
fi

echo "[*] Building zai-proxy..."
go build -o /tmp/zai-proxy-test .

echo "[*] Stopping stale instances..."
pkill -f zai-proxy-test 2>/dev/null || true
sleep 1

echo "[*] Starting proxy on port $PORT..."
# LOG_LEVEL defaults to info in the binary now; keep it explicit here so the
# launcher is self-documenting. Override with LOG_LEVEL=debug to investigate.
LOG_LEVEL="${LOG_LEVEL:-info}"
ZAI_TOKEN="$TOKEN" PORT="$PORT" LOG_LEVEL="$LOG_LEVEL" \
  nohup /tmp/zai-proxy-test --agent-mode --verbose > "/tmp/zai-proxy-$PORT.log" 2>&1 &
PROXY_PID=$!

sleep 1.5
if curl -sf "http://localhost:$PORT/api/healthz" >/dev/null 2>&1; then
  echo "[✓] Proxy ready on http://localhost:$PORT (PID $PROXY_PID)"
  echo "    Logs:   tail -f /tmp/zai-proxy-$PORT.log"
  echo "    Stop:   pkill -f zai-proxy-test"
else
  echo "[!] Proxy may not be ready. Check /tmp/zai-proxy-$PORT.log"
fi
