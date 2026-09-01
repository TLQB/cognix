> [!IMPORTANT]
> Remove this line to confirm you've reviewed this PR before submitting.

# Cognix

A fork of [Zed](https://github.com/zed-industries/zed) focused on multi-provider AI
routing: a high-performance, multiplayer code editor with first-class support for a
wide range of language model backends.

[![Build Linux](https://github.com/TLQB/cognix/actions/workflows/build-linux.yml/badge.svg)](https://github.com/TLQB/cognix/actions/workflows/build-linux.yml)

---

### What this fork adds

Beyond upstream Zed, Cognix ships additional language model providers wired into the
agent panel, each with its own model registry and streaming adapter:

| Provider | Crate |
| --- | --- |
| GLM | `crates/language_models/src/provider/glm.rs` |
| JustWoker | `crates/language_models/src/provider/justwoker.rs` |
| Kilo | `crates/language_models/src/provider/kilo.rs` |
| NextRouter | `crates/language_models/src/provider/nextrouter.rs` |
| NVIDIA NIM | `crates/language_models/src/provider/nim.rs` |
| OpenCode | `crates/language_models/src/provider/opencode.rs` |
| TokenRouter | `crates/language_models/src/provider/tokenrouter.rs` |
| Zen | `crates/language_models/src/provider/zen.rs` |

Plus a thread sidebar (`crates/sidebar`) for switching between agent threads.

### Installation

Linux x86_64 builds are produced by the
[Build Linux](https://github.com/TLQB/cognix/actions/workflows/build-linux.yml)
workflow. Download the `cognix-linux-x86_64` artifact from a successful run, then:

```sh
unzip cognix-linux-x86_64.zip
install -m 755 zed ~/.local/bin/cognix
```

Artifacts expire 14 days after the build.

### Building from source

Cognix uses the same build system as upstream Zed:

- [Building for macOS](./docs/src/development/macos.md)
- [Building for Linux](./docs/src/development/linux.md)
- [Building for Windows](./docs/src/development/windows.md)

To build the editor only, skipping the collab server and other workspace members:

```sh
cargo build --release -p zed
```

### Vendored dependencies

`vendor/xim-ctext` is a patched copy of the upstream crate. It fixes a
COMPOUND_TEXT decoding bug where the `ESC % @` ("UTF-8 End") escape sequence was
treated as a terminator, discarding the remaining bytes in the chunk. This broke
X11 input methods that interleave UTF-8 and Latin-1 segments — notably Vietnamese
Telex via `ibus-unikey`, where pressing Space dropped the last character of a
syllable. The patch is applied through `[patch]` in the workspace `Cargo.toml`.

### Licensing

Cognix source code is licensed primarily under GPL-3.0-or-later, with Apache-2.0
components where marked, following upstream Zed.

License information for third party dependencies must be correctly provided for CI
to pass. We use [`cargo-about`](https://github.com/EmbarkStudios/cargo-about) to
automatically comply with open source licenses. If CI is failing, check the
following:

- Is it showing a `no license specified` error for a crate you've created? If so, add `publish = false` under `[package]` in your crate's Cargo.toml.
- Is the error `failed to satisfy license requirements` for a dependency? If so, first determine what license the project has and whether this system is sufficient to comply with this license's requirements. If you're unsure, ask a lawyer. Once you've verified that this system is acceptable add the license's SPDX identifier to the `accepted` array in `script/licenses/zed-licenses.toml`.
- Is `cargo-about` unable to find the license for a dependency? If so, add a clarification field at the end of `script/licenses/zed-licenses.toml`, as specified in the [cargo-about book](https://embarkstudios.github.io/cargo-about/cli/generate/config.html#crate-configuration).

### Upstream

For documentation, contribution guidelines, and general editor features, see the
[upstream Zed repository](https://github.com/zed-industries/zed). Cognix tracks
upstream `main` and merges periodically.
