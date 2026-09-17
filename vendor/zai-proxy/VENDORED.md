# Vendored: zai-proxy

Cloned from: /home/tlqbao/Desktop/zai-proxy (origin: https://github.com/TLQB/zai-proxy.git)
Upstream commit: da89ebbe37e82f8559b586227eb2e0d082fab8ab feat(qfarm): enable AutoRebless by default; add probe jitter, rebless backoff, reserve refresh on Q swap

This copy is vendored into cognix so the release build can embed the zai-proxy
sidecar (model proxy on 127.0.0.1:3001). Build the sidecar binary with:

    scripts/build-zai-proxy-sidecar.sh

The produced binary is NOT committed (see .gitignore); CI and local release
builds generate it before `cargo build --release`.
