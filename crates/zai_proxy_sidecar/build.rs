//! Embeds the vendored zai-proxy artifacts when they have been built
//! (scripts/build-zai-proxy-sidecar.sh). Without them the crate still
//! compiles and `init()` becomes a no-op, so plain dev builds keep working.
use std::env;

fn artifact(os_arch: &str) -> String {
    let manifest = env::var("CARGO_MANIFEST_DIR").unwrap();
    format!("{manifest}/binaries/{os_arch}")
}

fn main() {
    let proxy = artifact("zai-proxy-linux-amd64");
    let qbless = artifact("q-bless-linux-amd64");
    // NOTE: per-target names once cross-compile targets are wired in CI;
    // CARGO_CFG_TARGET_OS/ARCH switch them when needed.
    let os = env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let arch = env::var("CARGO_CFG_TARGET_ARCH").unwrap_or_default();
    let goarch = match arch.as_str() {
        "aarch64" => "arm64",
        _ => "amd64",
    };
    let proxy_t = artifact(&format!("zai-proxy-{os}-{goarch}"));
    let qbless_t = artifact(&format!("q-bless-{os}-{goarch}"));
    let proxy = if std::path::Path::new(&proxy_t).exists() { proxy_t } else { proxy };
    let qbless = if std::path::Path::new(&qbless_t).exists() { qbless_t } else { qbless };

    if std::path::Path::new(&proxy).exists() {
        println!("cargo:rustc-env=ZAI_PROXY_SIDECAR_PATH={proxy}");
        println!("cargo:rustc-cfg=zai_proxy_embedded");
    }
    if std::path::Path::new(&qbless).exists() {
        println!("cargo:rustc-env=ZAI_QBLESS_PATH={qbless}");
        println!("cargo:rustc-cfg=zai_proxy_qbless_embedded");
    }
    println!("cargo:rerun-if-changed=binaries");
}

