#!/usr/bin/env bash
# Freeclaude all-in-one launcher: starts zai-proxy, then runs the TUI.
#
# Usage:
#   ./start.sh [workdir]                # TUI opens in workdir (default: $PWD)
#   ZAI_TOKEN=<token> ./start.sh        # token via env
#   PORT=3100 ./start.sh                # proxy port (must match PROXY_URL the
#                                       #  TUI was baked with: default 3002)
#
# First run: put your Z.AI token in ~/.config/zai-proxy/token
#   mkdir -p ~/.config/zai-proxy && echo <token> > ~/.config/zai-proxy/token
set -euo pipefail
cd "$(dirname "$0")"

PORT="${PORT:-3002}"
AUTH_TOKEN="${AUTH_TOKEN:-Waguri}"
WORKDIR="${1:-$HOME}"
LOG_LEVEL="${LOG_LEVEL:-info}"

CONFIG_DIR="$HOME/.config/zai-proxy"
TOKEN_FILE="$CONFIG_DIR/token"
TOKEN="${ZAI_TOKEN:-$(cat "$TOKEN_FILE" 2>/dev/null || echo '')}"

if [ -z "$TOKEN" ]; then
  echo "Freeclaude needs a Z.AI token (first run only)."
  echo "Get it from https://chat.z.ai (see README), then:"
  echo "  mkdir -p $CONFIG_DIR && echo <token> > $TOKEN_FILE"
  printf "Or paste it here (input hidden): "
  read -rs TOKEN
  echo
  if [ -z "$TOKEN" ]; then
    echo "Error: no token provided."
    exit 1
  fi
  mkdir -p "$CONFIG_DIR"
  chmod 700 "$CONFIG_DIR"
  printf '%s' "$TOKEN" > "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
fi

# Stop a stale proxy from a previous run of this bundle on the same port.
pkill -f "zai-proxy-bundle.*$PORT" 2>/dev/null || true
pkill -f "$PWD/zai-proxy.*" 2>/dev/null || true
sleep 0.5

# Resolve device-token sources. The proxy reads qbless.json/tokens.sqlite
# relative to ITS cwd by default, so pass explicit paths. Search order:
#   1. bundle dir (user may ship a pre-blessed session alongside)
#   2. ~/.config/zai-proxy/ (standard config location, survives upgrades)
PROXY_ARGS=()
for f in qbless.json tokens.sqlite; do
  if   [ -f "$PWD/$f" ];          then FOUND="$PWD/$f"
  elif [ -f "$CONFIG_DIR/$f" ];  then FOUND="$CONFIG_DIR/$f"
  else FOUND=""; fi
  if [ -n "$FOUND" ]; then
    case "$f" in
      qbless.json)    export QBLESS_FILE="$FOUND" ;;
      tokens.sqlite)  PROXY_ARGS+=("--db-path" "$FOUND") ;;
    esac
    echo "    Using $f: $FOUND"
  fi
done
if [ -z "${QBLESS_FILE:-}" ] && [ -x "$PWD/q-bless" ]; then
  # No blessed session yet — bless one now (~30s headless Chromium). This is
  # the Linux first-run flow; requires Chrome/Chromium installed.
  echo "[*] qbless.json not found — running q-bless to bless a fresh Q (~30s)..."
  "$PWD/q-bless" -out "$CONFIG_DIR/qbless.json" && export QBLESS_FILE="$CONFIG_DIR/qbless.json"
fi
if [ -z "${QBLESS_FILE:-}" ]; then
  echo "[!] Warning: no qbless.json found (bundle dir or $CONFIG_DIR)."
  echo "    Without it the proxy cannot mint device tokens — requests will"
  echo "    fail with 'captcha generation returned empty payload'."
  echo "    Run q-bless to create one, then copy it next to start.sh or into $CONFIG_DIR."
fi

# Auto-rebless: the proxy probes the blessed Q every 10 minutes and re-blesses
# by itself when the Q dies (needs the shipped q-bless binary). Without this,
# a dead Q bricks the proxy until the user manually re-runs q-bless.
export QBLESS_AUTO_REBLESS="${QBLESS_AUTO_REBLESS:-1}"
if [ -x "$PWD/q-bless" ]; then
  export QBLESS_BINARY="$PWD/q-bless"
fi

# Start the proxy (info-level logs; LOG_LEVEL=debug to investigate).
# Qwen gateway: when qwen-proxy ships with the bundle, start it on QWEN_PORT
# (3003) and point zai-proxy at it — the model picker then shows Qwen models
# next to the GLM ones. Requires ~/.config/qwen-proxy/token (first run
# prompt below). Set QWEN_NO_BUNDLE=1 to skip it entirely.
QWEN_PORT="${QWEN_PORT:-3003}"
QWEN_PID=""
QWEN_TOKEN_FILE="$HOME/.config/qwen-proxy/token"
if [ -x "$PWD/qwen-proxy" ] && [ "${QWEN_NO_BUNDLE:-0}" != "1" ]; then
  QWEN_TOKEN="${QWEN_TOKEN:-$(cat "$QWEN_TOKEN_FILE" 2>/dev/null || echo '')}"
  if [ -z "$QWEN_TOKEN" ]; then
    echo "[*] qwen-proxy found but no Qwen token at $QWEN_TOKEN_FILE."
    echo "    Get one from https://chat.qwen.ai (see README), then:"
    echo "      mkdir -p ~/.config/qwen-proxy && echo <token> > $QWEN_TOKEN_FILE"
    echo "    Starting WITHOUT Qwen models (GLM only)."
  else
    QWEN_TRANSPORT="${QWEN_TRANSPORT:-apk}" \
    QWEN_TOKEN="$QWEN_TOKEN" AUTH_TOKEN="$AUTH_TOKEN" LOG_LEVEL="$LOG_LEVEL" \
      nohup ./qwen-proxy --agent-mode --port "$QWEN_PORT" > "/tmp/freeclaude-qwen-$QWEN_PORT.log" 2>&1 &
    QWEN_PID=$!
    export QWEN_PROXY_URL="http://localhost:$QWEN_PORT"
    echo "[*] qwen-proxy starting on http://localhost:$QWEN_PORT (PID $QWEN_PID)"
  fi
fi

ZAI_TOKEN="$TOKEN" PORT="$PORT" LOG_LEVEL="$LOG_LEVEL" \
  nohup ./zai-proxy --agent-mode "${PROXY_ARGS[@]}" > "/tmp/freeclaude-proxy-$PORT.log" 2>&1 &
PROXY_PID=$!

cleanup() {
  # SIGTERM triggers the proxy's graceful drain: in-flight responses
  # finish (10s deadline) and all pooled Z.AI chat sessions are deleted.
  [ -n "$QWEN_PID" ] && kill -TERM "$QWEN_PID" 2>/dev/null || true
  kill -TERM "$PROXY_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "[*] Starting zai-proxy on http://localhost:$PORT (PID $PROXY_PID)"
if [ -n "$QWEN_PID" ]; then
  # qwen-proxy has no healthz route; its listener answering a TCP dial is
  # enough — zai-proxy already skips Qwen models gracefully when it is slow.
  for _ in $(seq 1 20); do
    if curl -sf "http://localhost:$QWEN_PORT/v1/models" -H "Authorization: Bearer $AUTH_TOKEN" >/dev/null 2>&1; then
      echo "[✓] qwen-proxy ready"
      break
    fi
    sleep 0.5
  done
fi
READY=0
for _ in $(seq 1 30); do
  if curl -sf "http://localhost:$PORT/api/healthz" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 0.5
done
if [ "$READY" != "1" ]; then
  echo "[!] Proxy not ready after 15s — check /tmp/freeclaude-proxy-$PORT.log"
  exit 1
fi
echo "[✓] Proxy ready"

# Clear any stale freebuff session so the TUI lands on the model picker
# instead of a stale takeover prompt.
curl -sf -X DELETE "http://localhost:$PORT/api/v1/freebuff/session" \
  -H "Authorization: Bearer $AUTH_TOKEN" >/dev/null 2>&1 || true

echo "[*] Starting Freeclaude TUI (workdir: $WORKDIR)"
echo "    Logs:  tail -f /tmp/freeclaude-proxy-$PORT.log"
echo "    Quit the TUI with Ctrl+C / Ctrl+Q (the proxy stops automatically)."
export CODEBUFF_API_KEY="$AUTH_TOKEN"
# NOT exec: the TUI replaces nothing — when it exits we fall through to the
# cleanup trap which SIGTERMs the proxy (graceful drain + session cleanup
# on Z.AI, see internal/zbridge/run.go).
./freeclaude --cwd "$WORKDIR"
