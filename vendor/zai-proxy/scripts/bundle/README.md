# Freeclaude (all-in-one bundle)

Freeclaude = Freebuff TUI (codebuff fork) + zai-proxy, đóng gói chạy 1 lệnh.

## Nội dung

| File | Vai trò |
|---|---|
| `freeclaude` / `freeclaude.exe` | Binary TUI (proxy URL đã bake: `http://localhost:3002`) |
| `zai-proxy` / `zai-proxy.exe` | Binary proxy Go — dịch OpenAI API ↔ chat.z.ai (GLM models) |
| `qwen-proxy` / `qwen-proxy.exe` | *(optional)* proxy Go cho chat.qwen.ai — chạy :3003, models Qwen hiện cùng GLM trong picker |
| `q-bless` / `q-bless.exe` | Helper mint device-token Z.AI (auto-rebless) |
| `tree-sitter.wasm` | Asset bắt buộc nằm cạnh binary TUI |
| `start.sh` / `start.bat` | Launcher: hỏi token lần đầu → start proxy → start TUI |
| `VERSION` | Phiên bản bundle |

## Chạy (Linux x64 / WSL)

```bash
tar xzf freeclaude-linux-x64-<version>.tar.gz
cd freeclaude-linux-x64
./start.sh                    # TUI mở ở $HOME
./start.sh /path/to/project    # TUI mở ở thư mục project
```

## Chạy (Windows x64)

Giải nén `freeclaude-windows-x64-<version>.zip`, rồi nhấp đúp `start.bat`
(hoặc chạy từ `cmd`). Yêu cầu: `curl.exe` có trong PATH (có sẵn từ
Windows 10 1803+). WSL không bắt buộc.

Lần đầu chạy, `start.sh` sẽ hỏi **Z.AI token** (lấy sau khi đăng nhập
https://chat.z.ai — xem README của zai-proxy) và lưu vào
`~/.config/zai-proxy/token` (chmod 600). Các lần sau tự động dùng lại.

**Qwen models (optional):** nếu bundle có `qwen-proxy` và tồn tại
`~/.config/qwen-proxy/token` (token từ https://chat.qwen.ai), launcher tự
start qwen-proxy ở :3003 và model picker sẽ hiển thị cả `Qwen3.8-Max`,
`Qwen3.7-Max`, `Qwen3-Coder-Plus`... bên cạnh GLM. Không có token Qwen →
bundle vẫn chạy bình thường với GLM. Muốn tắt hẳn: `QWEN_NO_BUNDLE=1`.

## Cách hoạt động

```
freeclaude (TUI) ──► http://localhost:3002 ──► zai-proxy ──► chat.z.ai   (GLM)
                      (baked lúc build)        │
                                                 └──► qwen-proxy :3003 ──► chat.qwen.ai  (Qwen, optional)
```

- `start.sh` start proxy trước, poll `/api/healthz` (tối đa 15s), rồi exec TUI.
- Thoát TUI (Ctrl+C) → proxy tự stop (trap cleanup).
- Log proxy: `/tmp/freeclaude-proxy-3002.log` (mặc định level `info`).
  Muốn debug: `LOG_LEVEL=debug ./start.sh`.

## Env hỗ trợ

| Biến | Mặc định | Ý nghĩa |
|---|---|---|
| `ZAI_TOKEN` | hỏi tương tác / `~/.config/zai-proxy/token` | Z.AI token |
| `QWEN_TOKEN` | `~/.config/qwen-proxy/token` | Qwen token (bật models Qwen nếu có) |
| `QWEN_PORT` | `3003` | port qwen-proxy |
| `QWEN_NO_BUNDLE` | `0` | `1` = không start qwen-proxy (chỉ GLM) |
| `PORT` | `3002` | port proxy — **chỉ đổi nếu build TUI với `PROXY_URL` khác** |
| `AUTH_TOKEN` | `Waguri` | token auth giữa TUI ↔ proxy |
| `LOG_LEVEL` | `info` | log level proxy (`debug` để điều tra) |
| `WORKDIR` | tham số `$1` của `start.sh` | thư mục làm việc của TUI |

## Xử lý sự cố

- **Proxy not ready**: `tail -50 /tmp/freeclaude-proxy-3002.log`. Lỗi token
  thường là token hết hạn — mint lại và cập nhật `~/.config/zai-proxy/token`.
- **"captcha generation returned empty payload"**: thiếu `qbless.json`.
  `start.sh` dò nó ở (1) thư mục bundle, (2) `~/.config/zai-proxy/`.
  Run `q-bless` trong repo zai-proxy để tạo, rồi copy tới 1 trong 2 chỗ đó.
  `tokens.sqlite` (reserve) cũng được dò theo cùng thứ tự.
- **Port 3002 bận**: process cũ còn treo — `pkill -f zai-proxy` rồi chạy lại.
- **TUI báo lỗi model/session**: xóa session cũ bằng cách restart bundle
  (`start.sh` đã tự làm đầu mỗi lần chạy).
- **WSL**: chạy trong terminal WSL; bảo đảm `curl` có sẵn (`apt install curl`).

## Build lại bundle

Trong repo zai-proxy:

```bash
./scripts/bundle/make-bundle.sh 1.0.0            # Linux x64
./scripts/bundle/make-bundle.sh 1.0.0 windows    # Windows x64 (zip)
# SKIP_TUI=1 ./scripts/bundle/make-bundle.sh ...  # tái dùng binary TUI vừa build
```

Yêu cầu: Go 1.21+, bun ≥ 1.3, checkout freebuff-src (mặc định
`~/Desktop/freebuff-src`, đổi qua `FREEBUFF_SRC=...`).

## CI (GitHub Actions)

`.github/workflows/bundle-release.yml` tự build cả 2 nền tảng + smoke test
proxy + tạo GitHub Release kèm archive khi push tag `v*`:

```bash
git tag v1.0.0 && git push origin v1.0.0
```

Workflow checkout **freebuff-src riêng** (vì bản public CodebuffAI/freebuff
**không** có các patch Freeclaude). Cấu hình một lần ở repo Settings →
Secrets and variables → Actions:

| Khóa | Loại | Giá trị |
|---|---|---|
| `FREEBUFF_REPO_NAME` | Secret | repo chứa freebuff-src patch, vd `TLQB/freebuff-private` |
| `FREEBUFF_PAT` | Secret | fine-grained PAT với Contents:read trên repo đó |
| `FREEBUFF_REF` | Variable | branch/tag cần build (mặc định `main`) |

Không set `FREEBUFF_REPO_NAME` → workflow dùng public upstream (build vẫn
thành công nhưng thiếu patch rename Freeclaude).
