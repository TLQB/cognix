# Development Guide — zai-proxy

Workflow hàng ngày cho dev: build → test → chạy e2e thật → đọc log → debug.
README.md là tài liệu product (API, config, Q-Bless). File này là tài liệu
**cách làm việc trên codebase**.

---

## 1. Yêu cầu môi trường

| Tool | Version | Dùng để |
|---|---|---|
| Go | 1.25+ (`go 1.25.0` trong `go.mod`) | build, unit test |
| Python | 3.8+ (chỉ dùng stdlib) | chạy e2e harness |
| Google Chrome / Chromium | bất kỳ | `q-bless` bless Q (~30s, một lần) |
| curl, dig, sqlite3 | — | debug/probe khi cần |

Không cần CGO (`modernc.org/sqlite`), không cần Playwright lúc runtime.

## 2. Câu lệnh nhanh

```bash
# Build production (nhanh hơn go run nhiều)
go build -o zai-proxy -trimpath -gcflags="all=-l=4" -ldflags="-s -w" .

# Toàn bộ unit test + vet
go vet ./... && go test ./... -count=1

# Chạy 1 package cụ thể khi iterate
go test ./internal/zbridge/ -run TestWAF -v -count=1

# Bless Q lần đầu (hoặc khi Q chết)
go build -trimpath -ldflags="-s -w" -o q-bless ./cmd/q-bless
./q-bless -out qbless.json
```

## 3. Khởi động dev server

```bash
ZAI_TOKEN='<jwt-từ-localStorage-của-chat.z.ai>' \
PORT=3002 \
AUTH_TOKEN=e2e-token \
AGENT_MODE=1 \
DB_PATH=/tmp/e2e/tokens.sqlite \
LOG_LEVEL=debug \
./zai-proxy
```

- `ZAI_TOKEN` lấy từ DevTools → Application → Local Storage → `token`
  (không bắt buộc — guest chỉ cho `glm-4.7`).
- `AGENT_MODE=1` bắt buộc nếu test tool-calling.
- `LOG_LEVEL=debug` dump toàn bộ request/response Z.AI — rất ồn, chỉ dùng
  khi debug; tắt đi khi chạy e2e ổn định.
- Kiểm chứng server sống:

  ```bash
  curl -s http://localhost:3002/health      # {"healthy":true,"mode":"direct"}
  curl -s http://localhost:3002/v1/models -H "Authorization: Bearer e2e-token"
  ```

> **Chú ý XFF:** đừng set `ZAI_XFF_IP` / `ZAI_XFF_IPS` khi test — nó pin
> egress identity và làm một số unit test skip. Chiền lược mặc định là
> RANDOM-BANK (XFF ngẫu nhiên mỗi request, xem §6).

## 4. Unit test

```bash
go test ./... -count=1
```

 convention test trong repo:
- Test gọi trực tiếp `sendToZAIStream(...)` phải tự `close(ch)` sau khi gọi
  (chỉ wrapper `sendToZAI` mới close trong goroutine).
- Captcha param single-use, TTL 75s: mỗi upstream attempt cần 1 param.
  Unit test seed bằng `SeedCaptchaParam("test-captcha-param")` lặp đủ số
  attempt (blocked rotations + success + slack).
- Test đụng XFF internals bắt đầu bằng `resetXffStateForTest(t)` (force
  `ZAI_XFF_STRATEGY=pool` + clear state) để không leak state sang test khác.
- Không test nào gọi mạng thật, trừ các probe `EXP_CAPTCHA=1` (skip mặc định).

## 5. E2E — vòng lặp kiểm tra ổn định

E2E là **agent harness thật**: gửi task coding + tool definitions, tự thực thi
tool (list/read/write/edit/bash trên workspace `/tmp/e2e/workspace`), feed
kết quả về multi-turn cho đến khi model ra final answer.

### 5.1 Smoke (2 request, ~10s)

```bash
python3 scripts/e2e/smoke_test.py
# [models] 6 models: [...]
# [nonstream] status=200 ok=True content='\nSMOKE_OK_1'
# [stream] chunks=20 done=True ok=True content='\nSMOKE_OK_2'
# SMOKE: PASS
```

### 5.2 Agent e2e đầy đủ (5 task, ~60–130s)

```bash
E2E_MODEL=glm-5.3 python3 scripts/e2e/agent_e2e.py all
E2E_MODEL=glm-5.2 python3 scripts/e2e/agent_e2e.py all
E2E_MODEL=glm-4.7 python3 scripts/e2e/agent_e2e.py all   # nhanh, ổn định nhất
python3 scripts/e2e/agent_e2e.py 3                        # chỉ task 3
```

| Task | Kiểm tra gì |
|---|---|
| 1 | Fix bug trong `app.py` (list/read/edit/bash multi-turn), final answer nêu fix |
| 2 | Viết `fib.py` từ đầu + verify bằng bash in 55 |
| 3 | Đọc 3 file .txt, tổng hợp `summary.txt` sort alphabet |
| 4 | Non-stream path (code path khác stream) |
| 5 | Anthropic `/v1/messages` tool_use round-trip |

Exit code 0 = pass hết; từng check in `[PASS]/[FAIL]`. **Chạy 2–3 round liên
tiếp** cho mỗi model trước khi kết luận ổn định — lỗi chỉ lộ ở round thứ 2+.

### 5.3 Đọc log server khi e2e fail

```bash
# Lỗi client-visible (surface tới harness)
grep -aE "\[API\] Error|\[Stream\] Error" /tmp/e2e/server.log

# Retry WAF/capacity đã tự phục hồi (không fail — chỉ để đếm tần suất)
grep -a "\[Retry\]" /tmp/e2e/server.log

# Q-bless chết (fallback reserve + nag re-bless)
grep -a "QBless" /tmp/e2e/server.log
```

> Log 405 chứa HTML khổng lồ — luôn dùng `cut -c1-200` khi in ra.
> Log server là binary-safe (lẫn HTML) — thêm `-a` cho grep.

## 6. WAF 405 — đã bypass hoàn toàn, cần biết gì

Chi tiết đầy đủ: `ZAI_RESEARCH_405.md`. Tóm tắt cho dev:

- 405 do **2 rule độc lập**: (1) TLS fingerprint phải giống Chrome —
  bridge dùng uTLS `HelloChrome_120`, đừng bỏ; (2) rate-limit
  ~12–16 request **mỗi identity** (XFF nếu có, không thì IP thật).
- Chiến lược mặc định **RANDOM-BANK**: mỗi request sinh 1 XFF ngẫu nhiên từ
  7 dải CIDR Cloudflare (>1 triệu host) → không bao giờ chạm budget.
  Banner startup: `[Startup] XFF strategy: RANDOM-BANK (...)`.
- Xem thử request đã đi với XFF nào: grep `"X-Forwarded-For"` trong
  LOG_LEVEL=debug log.
- Muốn reproduce/audit 1 IP cố định: `ZAI_XFF_IP=104.16.132.229 ./zai-proxy`
  — nhưng budget ~15 request rồi 405 vĩnh viễn, chỉ dùng để probe.
- Quay lại pool rotation legacy nếu cần: `ZAI_XFF_STRATEGY=pool`.

## 7. Vòng đời Q (blessed session) — sự cố thường gặp nhất

Symptom: mọi request fail `captcha generation returned empty payload`,
log có `[QBless] Q marked DEAD after N consecutive verify FAILs`.

Fix (30s, không cần restart server — bridge theo dõi mtime của
`qbless.json` và tự nhận session mới):

```bash
./q-bless -out qbless.json
# ✅ canary PASS — Q proven blessed → bridge tự pickup
```

Khi e2e stress kéo dài (>30–60 phút liên tục), Q có thể chết vì tải —
re-bless rồi chạy tiếp round. Dài hạn: đặt cron audit theo README §Q-Bless.

Reserve `tokens.sqlite` chỉ là fallback ~100 token single-use; nếu thấy log
rơi vào reserve thường xuyên thì Q đã chết từ lâu.

## 8. Checklist trước khi conclude "ổn định"

1. `go vet ./... && go test ./... -count=1` — xanh toàn bộ.
2. E2e 2–3 round liên tiếp cho model đang thay đổi: 10/10 mỗi round.
3. `grep -aE "\[API\] Error|\[Stream\] Error" server.log` — chỉ chứa lỗi
   đã hiểu (ví dụ capacity burst đã retry xong thì không có dòng nào).
4. Không có `[Retry]` nào lặp đến mức drain (7/7) rồi surface error.
5. `[Startup] XFF strategy: RANDOM-BANK` trong banner + 0 dòng
   `response status: 405` trong log.

## 9. Cấu trúc code liên quan khi debug

| File | Nội dung |
|---|---|
| `internal/zbridge/zai.go` | `sendToZAIStream` — retry loop: WAF rotation, capacity retry, captcha regen; `streamSSEResponse` — SSE parse + typed errors |
| `internal/zbridge/util.go` | XFF strategy (random-bank + pool), uTLS dialer, HTTP client, ALB bypass |
| `internal/zbridge/agent.go` | Agent shim modern (XML-sectioned prompt, tool-call interception) |
| `internal/zbridge/handlers.go` | OpenAI endpoint, stall-retry, salvage tool calls từ stream lỗi |
| `internal/zbridge/qmint.go` | Mint device token trên blessed Q, health tracking, reserve fallback |
| `scripts/e2e/*.py` | Harness smoke + agent e2e (Xem §5) |

Typed errors đáng nhớ khi đọc log:
- `captchaFlowError` (FRONTEND_CAPTCHA_REQUIRED / F018/F019) — regen param + retry 1 lần.
- `modelCapacityError` (MODEL_CONCURRENCY_LIMIT) — backoff 2s tăng dần, tối đa 7 lần (~56s).
- WAF 405 — không phải typed error; xoay identity trong `sendToZAIStream`.
