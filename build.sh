#!/bin/bash
export PATH="$HOME/.local/bin:$PATH"
export CC=clang-9
export CXX=clang++-9
cd /home/tlqbao/Desktop/zagent
echo "Starting build at $(date)" >> /tmp/zagent_build.log
cargo build --release -p zed 2>&1 | tee -a /tmp/zagent_build.log
echo "Build finished at $(date) with exit code $?" >> /tmp/zagent_build.log
