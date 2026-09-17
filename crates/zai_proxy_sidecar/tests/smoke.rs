//! Runtime smoke test: stop any proxy on 3001 first if you want the full
//! spawn path; with a proxy running this validates the reuse path.
#[test]
fn healthz_and_embed() {
    // Embedded in release/test builds only when binaries/ artifacts exist.
    println!("embedded = {}", zai_proxy_sidecar::embedded());
    // Reuse path: whatever is on 3001 (or nothing) must not panic.
    let ok = zai_proxy_sidecar::init_checked();
    println!("ensure_running result = {ok:?}");
}
