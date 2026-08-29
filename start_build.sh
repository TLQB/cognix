#!/bin/bash
exec > /tmp/cognix_build.log 2>&1
export PATH="$HOME/.local/bin:$PATH"
export CC=clang-9
export CXX=clang++-9
cd /home/tlqbao/Desktop/cognix
echo "Build started at $(date)"
cargo build --release -p zed
echo "Build finished at $(date), exit=$?"
