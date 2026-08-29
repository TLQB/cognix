#!/bin/bash
export CC=clang-9
export CXX=clang++-9
export CARGO_BUILD_JOBS=2
export CARGO_INCREMENTAL=1
cd /home/tlqbao/Desktop/cognix
echo "Build started at $(date)" > /tmp/cognix_build_bg.log
cargo build --release -p zed 2>&1 | tee -a /tmp/cognix_build_bg.log
EXIT_CODE=${PIPESTATUS[0]}
echo "Build finished at $(date) with exit code $EXIT_CODE" >> /tmp/cognix_build_bg.log
if [ -f target/release/zed ]; then
    echo "SUCCESS: target/release/zed exists" >> /tmp/cognix_build_bg.log
    ls -la target/release/zed >> /tmp/cognix_build_bg.log
else
    echo "FAILED: no binary" >> /tmp/cognix_build_bg.log
fi
