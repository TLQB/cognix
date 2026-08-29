#!/bin/bash
exec > /tmp/cognix_build_v2.log 2>&1
echo "=== Build v2 started at $(date) ==="
export CC=clang-9
export CXX=clang++-9
export CARGO_BUILD_JOBS=1
export CARGO_INCREMENTAL=1
cd /home/tlqbao/Desktop/cognix
cargo build --release -p zed
EXIT_CODE=$?
echo "=== Build v2 finished at $(date) with exit code $EXIT_CODE ==="
if [ -f target/release/zed ]; then
    echo "SUCCESS" && ls -la target/release/zed
else
    echo "FAILED - no binary"
fi
