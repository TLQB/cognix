# Vendored: zai-proxy

Cloned from: /home/tlqbao/Desktop/zai-proxy (origin: https://github.com/TLQB/zai-proxy.git)
Upstream commit: 2a17aa3 Add slide export passthrough (/v1/slides/export/*)

This copy is vendored into cognix so the release build can embed the zai-proxy
sidecar (model proxy on 127.0.0.1:3001). Build the sidecar binary with:

    scripts/build-zai-proxy-sidecar.sh

The produced binary is NOT committed (see .gitignore); CI and local release
builds generate it before `cargo build --release`.
