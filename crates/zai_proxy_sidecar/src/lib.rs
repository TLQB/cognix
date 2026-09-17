//! Embedded zai-proxy sidecar (model proxy on 127.0.0.1:3001).
//!
//! Release builds embed the vendored Go binary (see build.rs and
//! scripts/build-zai-proxy-sidecar.sh). `init()` is fire-and-forget: it never
//! delays app startup. On a background thread it
//!   1. reuses an already-running proxy when /api/healthz answers on the port,
//!   2. otherwise extracts the embedded binary to the user state dir, spawns
//!      it and waits (bounded) for the health endpoint.
//!
//! The Z.AI token is a PERSONAL credential and is never baked into the
//! binary. It reaches the proxy through, in priority order:
//!   1. ZAI_TOKEN environment variable,
//!   2. COGNIX_GLM_API_KEY environment variable (provider hand-off),
//!   3. the token file written by [`set_token`] when the user signs in via
//!      the cognix.glm provider UI (stored in the OS keychain by zed, mirrored
//!      here so the Go proxy can read it on restart).

use anyhow::{anyhow, Context, Result};
use std::io::{Read, Write};
use std::net::TcpStream;
use std::path::PathBuf;
use std::process::{Child, Command};
use std::sync::{Mutex, Once};
use std::time::Duration;

pub const PROXY_PORT: u16 = 3001;
pub const PROXY_HOST: &str = "127.0.0.1";
pub const PROXY_BASE_URL: &str = "http://127.0.0.1:3001";

static START: Once = Once::new();
static CHILD: Mutex<Option<Child>> = Mutex::new(None);
static TOKEN_HANDOFF: Once = Once::new();

#[cfg(zai_proxy_embedded)]
const PROXY_BYTES: &[u8] = include_bytes!(concat!(env!("ZAI_PROXY_SIDECAR_PATH")));
#[cfg(not(zai_proxy_embedded))]
const PROXY_BYTES: &[u8] = &[];
#[cfg(zai_proxy_qbless_embedded)]
const QBLESS_BYTES: &[u8] = include_bytes!(concat!(env!("ZAI_QBLESS_PATH")));
#[cfg(not(zai_proxy_qbless_embedded))]
const QBLESS_BYTES: &[u8] = &[];

/// Whether this build carries an embedded sidecar binary.
pub fn embedded() -> bool {
    !PROXY_BYTES.is_empty()
}

/// Start (or reuse) the embedded proxy. Idempotent; never blocks startup.
pub fn init() {
    // SAFETY: init() runs on the main thread from zed::main() before any
    // other thread reads the environment (gpui threads spawn later), so env
    // mutation here is single-threaded.
    // Bundled-release defaults — both overridable by the user's environment:
    //   COGNIX_ENABLED_PROVIDERS : model-selector allowlist (default: glm only)
    //   COGNIX_GLM_API_KEY       : personal token handed to the glm provider
    unsafe {
        if std::env::var_os("COGNIX_ENABLED_PROVIDERS").is_none() {
            std::env::set_var("COGNIX_ENABLED_PROVIDERS", "cognix.glm");
        }
        if std::env::var_os("COGNIX_GLM_API_KEY").is_none() {
            if let Some(token) = resolve_token() {
                std::env::set_var("COGNIX_GLM_API_KEY", token);
            }
        }
    }
    START.call_once(|| {
        std::thread::spawn(|| {
            if let Err(e) = ensure_running() {
                eprintln!("[zai_proxy_sidecar] proxy not started: {e:#}");
            }
        });
    });
}

/// Test/CLI-friendly variant of [`init`]: runs the ensure path synchronously.
pub fn init_checked() -> Result<()> {
    START.call_once(|| {});
    ensure_running()
}

/// First-run hand-off from the glm provider: persist the user's personal
/// token and make sure the proxy runs with it. Acts at most once per
/// process (model refetches call this repeatedly with the same key).
pub fn handoff_token_once(token: &str) {
    if token.is_empty() {
        return;
    }
    TOKEN_HANDOFF.call_once(|| {
        let already = token_file_token().as_deref() == Some(token) && healthz_ok();
        if already {
            return;
        }
        if let Err(e) = set_token(token) {
            eprintln!("[zai_proxy_sidecar] token hand-off failed: {e:#}");
        }
    });
}

/// Persist the user's personal token and restart the proxy with it. Called
/// when the user signs in / changes the key in the provider UI.
pub fn set_token(token: &str) -> Result<()> {
    let path = token_path();
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).context("create zai-proxy config dir")?;
    }
    std::fs::write(&path, token).context("write zai-proxy token file")?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
    }

    stop_child();
    // Wait for the port to be released before respawning.
    for _ in 0..25 {
        if !healthz_ok() {
            break;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    ensure_running()
}

fn stop_child() {
    if let Some(mut child) = CHILD.lock().unwrap().take() {
        let _ = child.kill();
        let _ = child.wait();
    }
}

fn ensure_running() -> Result<()> {
    if healthz_ok() {
        return Ok(()); // already serving (previous launch, systemd unit, …)
    }
    if !embedded() {
        anyhow::bail!(
            "sidecar binary not embedded — build it with scripts/build-zai-proxy-sidecar.sh"
        );
    }
    let path = extracted_path();
    write_if_stale(&path, PROXY_BYTES)?;

    // q-bless (auto-rebless helper) lands NEXT TO the session file so the
    // vendored autorebless fallback ("binary next to qbless.json") finds it
    // without any PATH setup.
    let mut cmd_envs: Vec<(String, std::ffi::OsString)> = Vec::new();
    if !QBLESS_BYTES.is_empty() {
        if let Some(parent) = path.parent() {
            let q = parent.join(exec_name("q-bless"));
            write_if_stale(&q, QBLESS_BYTES)?;
            if std::env::var_os("QBLESS_BINARY").is_none() {
                cmd_envs.push(("QBLESS_BINARY".into(), q.into_os_string()));
            }
        }
    }
    let session = path.parent().unwrap().join("qbless.json");
    if std::env::var_os("QBLESS_FILE").is_none() {
        cmd_envs.push(("QBLESS_FILE".into(), session.into_os_string()));
    }

    let mut cmd = Command::new(&path);
    cmd.arg("--agent-mode").env("PORT", PROXY_PORT.to_string());
    if let Some(token) = resolve_token() {
        cmd.env("ZAI_TOKEN", token);
    }
    for (k, v) in &cmd_envs {
        cmd.env(k, v);
    }
    let child = cmd.spawn().context("spawn zai-proxy sidecar")?;
    *CHILD.lock().unwrap() = Some(child);

    for _ in 0..30 {
        if healthz_ok() {
            return Ok(());
        }
        std::thread::sleep(Duration::from_millis(300));
    }
    Err(anyhow!("sidecar not healthy after 9s"))
}

/// Personal-token resolution order (NEVER embedded in the binary):
/// env ZAI_TOKEN → env COGNIX_GLM_API_KEY → token file (written by set_token).
fn resolve_token() -> Option<String> {
    for var in ["ZAI_TOKEN", "COGNIX_GLM_API_KEY"] {
        if let Ok(t) = std::env::var(var) {
            if !t.is_empty() {
                return Some(t);
            }
        }
    }
    token_file_token()
}

fn token_file_token() -> Option<String> {
    std::fs::read_to_string(token_path())
        .ok()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
}

fn token_path() -> PathBuf {
    config_dir().join("zai-proxy").join("token")
}

fn config_dir() -> PathBuf {
    std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| home().join(".config"))
}

/// Executable file name with platform suffix (".exe" on Windows, where
/// spawning an extension-less image fails).
fn exec_name(base: &str) -> String {
    if cfg!(windows) {
        format!("{base}.exe")
    } else {
        base.to_string()
    }
}

fn extracted_path() -> PathBuf {
    let state = std::env::var_os("XDG_STATE_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| home().join(".local/state"));
    state.join("zai-proxy-sidecar").join(exec_name("zai-proxy"))
}

fn home() -> PathBuf {
    std::env::var_os("HOME")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .map(PathBuf::from)
        .unwrap_or_default()
}

fn write_if_stale(path: &PathBuf, bytes: &[u8]) -> Result<()> {
    if std::fs::metadata(path)
        .map(|m| m.len() as usize != bytes.len())
        .unwrap_or(true)
    {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).context("create sidecar state dir")?;
        }
        std::fs::write(path, bytes).context("extract sidecar binary")?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755))?;
        }
    }
    Ok(())
}

/// Minimal HTTP/1.0 GET — no client dependency; returns true on HTTP 200.
fn healthz_ok() -> bool {
    let Ok(mut s) = TcpStream::connect((PROXY_HOST, PROXY_PORT)) else {
        return false;
    };
    s.set_read_timeout(Some(Duration::from_secs(2))).ok();
    let req = format!(
        "GET /api/healthz HTTP/1.0\r\nHost: {}:{}\r\n\r\n",
        PROXY_HOST, PROXY_PORT
    );
    if s.write_all(req.as_bytes()).is_err() {
        return false;
    }
    let mut buf = [0u8; 128];
    let mut head = String::new();
    while head.len() < 32 {
        match s.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => head.push_str(&String::from_utf8_lossy(&buf[..n])),
            Err(_) => break,
        }
    }
    head.starts_with("HTTP/") && head.contains(" 200")
}
