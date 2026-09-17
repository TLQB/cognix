# Freeclaude — Claude-style coding agent chạy trên GLM (Z.AI), miễn phí

Freeclaude = **Freebuff TUI + zai-proxy** đóng gói thành một thư mục, chạy bằng **một lệnh**. Bạn mở TUI, gõ task, agent đọc/sửa code, chạy lệnh — tất cả trên GLM models của chat.z.ai. **Proxy tự bật khi TUI mở và tự tắt khi TUI thoát** — không cần cấu hình gì thêm.

```
freeclaude (TUI) ──► zai-proxy (auto-start) ──► chat.z.ai (GLM models)
```

## Chạy Freeclaude

### Cách 1: Bundle release (khuyên dùng)

Tải bundle từ [Releases](https://github.com/TLQB/zai-proxy/releases) (job build tự động trên CI):

**Linux x64 / WSL:**
```bash
tar xzf freeclaude-linux-x64-<version>.tar.gz
cd freeclaude-linux-x64
./start.sh                     # TUI mở ở $HOME
./start.sh /path/to/project    # hoặc mở thẳng trong project của bạn
```

**Windows x64:** giải nén zip, nhấp đúp `start.bat` (cần `curl.exe` trong PATH — có sẵn từ Win10 1803+). WSL không bắt buộc.

**Lần đầu chạy**, `start.sh` hỏi Z.AI token và tự lưu vào `~/.config/zai-proxy/token` — các lần sau dùng lại luôn. Lấy token: đăng nhập [chat.z.ai](https://chat.z.ai) → DevTools (F12) → Application → Local Storage → key `token`.

Chỉ có vậy. Proxy được start trước TUI (poll health tối đa 15s), khi bạn thoát TUI (Ctrl+C) proxy tự stop và dọn session trên Z.AI.

### Cách 2: Từ source (cho dev)

```bash
git clone https://github.com/TLQB/zai-proxy.git && cd zai-proxy
go build -o zai-proxy -trimpath -ldflags="-s -w" .

# Một lần duy nhất: bless Q để proxy mint device token (~30s Chromium)
go build -o q-bless ./cmd/q-bless && ./q-bless -out qbless.json

# Terminal 1: proxy
ZAI_TOKEN=$(cat ~/.config/zai-proxy/token) ./zai-proxy --agent-mode
# Terminal 2: TUI (repo freebuff-src, bun >= 1.3)
cd freebuff-src && ./run-freebuff.sh /path/to/project
```

Hoặc build lại bundle: `./scripts/bundle/make-bundle.sh [version] [linux|windows]` (cần Go 1.25+, bun 1.3+, checkout freebuff-src). Chi tiết: [scripts/bundle/README.md](scripts/bundle/README.md).

### Yêu cầu bắt buộc: `qbless.json`

Proxy cần một Q được "bless" (bắt buộc cho captcha chống bot):

```bash
go build -o q-bless ./cmd/q-bless
./q-bless -out qbless.json                 # ~30s Chromium, MỘT LẦN duy nhất
mkdir -p ~/.config/zai-proxy && cp qbless.json ~/.config/zai-proxy/
```

Q không có TTL theo thời gian — chỉ re-bless khi proxy log "re-bless nag". Không có file này: lỗi `captcha generation returned empty payload`. Chi tiết: [docs/QBLESS.md](docs/QBLESS.md).

## Models

Lấy live từ Z.AI: `glm-5.3-flash` (nhẹ, nhanh), `glm-5.3` (flagship coding), `glm-5.2`, `GLM-5-Turbo`, `glm-4.7`... Không set `ZAI_TOKEN` → guest session chỉ dùng được vài model; có token → mở tất cả. Chọn model trong TUI ngay khi mở.

## Dùng proxy như API server (tùy chọn)

Proxy là server **OpenAI + Anthropic compatible** — dùng độc lập với bất kỳ client nào:

```bash
curl -s localhost:3001/v1/chat/completions \
  -H "Authorization: Bearer Waguri" -H "Content-Type: application/json" \
  -d '{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | OpenAI chat; `stream`, `tools`, `reasoning_effort` |
| `POST /v1/messages` | Anthropic Messages; `x-api-key` auth, tool use + thinking |
| `GET /v1/models` | OpenAI-style model list |
| `GET /health`, `/status`, `GET/POST /features` | health / session / per-model features |

Tool calling cần `--agent-mode` (bundle tự bật).

## Development

```bash
go test ./...                       # full suite

# E2E với mock upstream — không cần account Z.AI
go build -o /tmp/zai-proxy-mocktest .
python3 scripts/e2e/mock_zai_upstream.py 39877 &
PORT=3002 AUTH_TOKEN=e2e-token ZAI_TOKEN=mock-stub-token \
  ZAI_BASE_URL=http://127.0.0.1:39877 /tmp/zai-proxy-mocktest --agent-mode &
python3 scripts/e2e/agent_e2e.py
```

Core nằm trong `internal/zbridge` (`agent.go` shim + stall retry, `zai.go` streaming, `session_pool.go`, `anthropic.go`, `qmint.go`); `cmd/q-bless` là tool bless.

**Debug agent dừng giữa task:** chạy với `TRACE=1 LOG_LEVEL=info LOG_FILE=bridge.log`, rồi grep `'[trace '` — mỗi request có dòng START/END ghi bytes forwarded, tool calls, bytes dropped và exit path (`done-stop`, `upstream-stall-kill`, `client-disconnect`...).

## CI / Release

`.github/workflows/bundle-release.yml` chạy trên mọi push/PR:

1. **Bundle linux + Bundle windows** — build cả 2 bundle qua `make-bundle.sh`
2. **E2E (mock upstream)** — chạy agent harness đầy đủ
3. **Create release** — khi push tag `v*`: tạo GitHub Release kèm 2 archive

Set secrets `FREEBUFF_PAT` + `FREEBUFF_REPO_NAME` để CI build từ private Freebuff fork (có patch rename); không set thì dùng public source (thiếu patch).

```bash
git tag v1.0.0 && git push origin v1.0.0   # trigger release
```

## Cấu hình

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `3001` (bundle: `3002`) | port proxy |
| `AUTH_TOKEN` | `Waguri` | auth TUI ↔ proxy / client auth |
| `ZAI_TOKEN` | — | Z.AI JWT; mở khóa mọi model |
| `SESSION_POOL_SIZE` | `5` | pool chat session pre-warmed |
| `TOOL_LOOP_THINKING` | `off` | tắt thinking ở tool-loop turn (giảm latency mạnh trên GLM 5.3). Flips được runtime qua `/thinking` trong TUI (endpoint `POST /api/v1/freebuff/config`) |
| `STALL_TIMEOUT` | `120` | giây im lặng của upstream trước khi kill stream |
| `LOG_LEVEL` | `info` | `debug` để dump mọi SSE line (rất nhiều) |
| `QBLESS_FILE` | auto | đường dẫn `qbless.json` |

## License

MIT. Use responsibly and in accordance with Z.AI's terms of service.
