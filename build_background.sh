#!/bin/bash
set -x

export CC=clang-9
export CXX=clang++-9
export PATH="$HOME/.cargo/bin:$HOME/.local/bin:$PATH"

LOG="/tmp/zagent_build_$(date +%Y%m%d_%H%M%S).log"

cd /home/tlqbao/Desktop/zagent

echo "=== Build started at $(date) ===" | tee "$LOG"
echo "PID: $$" | tee -a "$LOG"

cargo build --release -p zed 2>&1 | tee -a "$LOG"

EXIT_CODE=$?
echo "=== Build finished at $(date) with exit code $EXIT_CODE ===" >> "$LOG"

# Check if binary exists
if [ -f target/release/zagent ]; then
    echo "✅ SUCCESS: target/release/zagent created" >> "$LOG"
    ls -la target/release/zagent >> "$LOG"
else
    echo "❌ FAILED: target/release/zagent not found" >> "$LOG"
fi

echo "Log file: $LOG"
