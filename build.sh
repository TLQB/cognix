#!/bin/bash
export PATH="$HOME/.local/bin:$PATH"
export CC=clang-9
export CXX=clang++-9
cd /home/tlqbao/Desktop/cognix
echo "Starting build at $(date)" >> /tmp/cognix_build.log
cargo build --release -p zed 2>&1 | tee -a /tmp/cognix_build.log
echo "Build finished at $(date) with exit code $?" >> /tmp/cognix_build.log
