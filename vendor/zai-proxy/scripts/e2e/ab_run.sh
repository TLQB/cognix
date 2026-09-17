#!/usr/bin/env bash
# A/B wrapper: runs ab_bench.py against both agent variants, one at a time,
# on the same live token, then reports stall counts per variant.
#
# Usage: ./scripts/e2e/ab_run.sh [workdir]
set -u
cd "$(dirname "$0")/../.."

REPO="$PWD"
LOG_DIR="/tmp/ab-bench"
mkdir -p "$LOG_DIR"
ZAI_TOKEN="$(cat ~/.config/zai-proxy/token)"
QBLESS="$HOME/.config/zai-proxy/qbless.json"

run_variant() {
  local variant="$1"
  local port="$2"
  local log="$LOG_DIR/proxy-$variant.log"
  echo "=== starting proxy variant=$variant port=$port ==="
  PORT=$port AUTH_TOKEN=Waguri ZAI_TOKEN="$ZAI_TOKEN" QBLESS_FILE="$QBLESS" \
    "$REPO/bin/zai-proxy-ab" --agent-mode --agent-mode-variant "$variant" \
    > "$log" 2>&1 &
  local pid=$!
  # Wait for readiness.
  for i in $(seq 1 30); do
    curl -sf -H "Authorization: Bearer Waguri" "http://localhost:$port/v1/models" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -sf -H "Authorization: Bearer Waguri" "http://localhost:$port/v1/models" >/dev/null \
    || { echo "proxy $variant did not start"; tail -20 "$log"; kill $pid 2>/dev/null; exit 1; }

  python3 "$REPO/scripts/e2e/ab_bench.py" "$port" "$variant"
  local stalls
  stalls=$(grep -ac "stall detected" "$log" || true)
  local errors
  errors=$(grep -acE "\[Stream\] Error" "$log" || true)
  echo "STALLCOUNT $variant stalls=$stalls errors=$errors"
  kill $pid 2>/dev/null
  wait $pid 2>/dev/null
  sleep 1
}

# Build the binary once (both variants come from the same build).
echo "=== building ==="
go build -o "$REPO/bin/zai-proxy-ab" . || exit 1

run_variant modern 3011
run_variant native 3012

echo
echo "=== done; logs in $LOG_DIR ==="
