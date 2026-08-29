#!/bin/bash
export CC=clang-9
export CXX=clang++-9
export CARGO_BUILD_JOBS=2
export CARGO_INCREMENTAL=1

LOG="/tmp/cognix_build_pull.log"
cd /home/tlqbao/Desktop/cognix

echo "=== Build started at $(date)" > "$LOG"
cargo build --release -p zed 2>&1 | tee -a "$LOG"
EXIT_CODE=${PIPESTATUS[0]}
echo "=== Build finished at $(date) with exit code $EXIT_CODE" >> "$LOG"

if [ -f target/release/zed ]; then
    echo "SUCCESS" >> "$LOG"
    ls -la target/release/zed >> "$LOG"
else
    echo "FAILED - no binary" >> "$LOG"
fi
