# QBless — Kiến trúc token tối ưu (Q-Bless + Pure-Go Mint)

**Trạng thái:** Đã triển khai & chứng minh live (2026-09-03) — chi tiết bằng chứng trong `EXP_CAPTCHA_RESULTS.md` §10–11
**Code đi kèm:** `cmd/q-bless/main.go` · `internal/zbridge/qmint.go` · `internal/zbridge/captcha.go` (getNextToken)

---

## 1. QBless là gì

**QBless** là kiến trúc token thứ 3 của bridge, thay thế hai phương án trước:

| # | Kiến trúc | Ra đời | Trạng thái |
|---|---|---|---|
| 1 | **Harvest batch** (`cmd/token-collector` → `tokens.sqlite`) | 2026-09-01 | Legacy — chỉ còn là dự trữ khẩn cấp |
| 2 | **Daemon mint** (`cmd/token-minter`, Chrome sống 24/7) | 2026-09-02 | Legacy — RAM nặng |
| 3 | **QBless** (`cmd/q-bless` + `internal/zbridge/qmint.go`) | 2026-09-03 | **Hiện tại — tối ưu nhất** |

Nguyên lý trong một câu:

> Server Aliyun gắn **điểm tin cậy (trust-score)** vào `Q` ngay tại lần gọi `Log1` đầu tiên, dựa trên fingerprint của kết nối (Chrome TLS + HTTP/2 frame ordering). Một `Q` đăng ký từ Chrome thật → Go có thể mint **vô hạn token** trên Q đó trong nhiều giờ. Một Q từ Go/curl/Node → sinh ra đã bị "nhiễm" (F001 vĩnh viễn).

Nên thay vì giữ Chrome sống để mint (phương án 2), ta chỉ mượn Chrome **~29 giây, MỘT LẦN DUY NHẤT** để "bless" Q (Q không có TTL theo thời gian — đo PASS ở tuổi 18h39m, xem `EXP_CAPTCHA_RESULTS.md` §10.4), rồi mọi việc khác làm bằng Go thuần:

```
┌────────── BLESS 1 LẦN (re-bless chỉ khi Q chết sự kiện) ──┐
│                                                │
│  cmd/q-bless  (Chromium, ~29s, rồi TẮT)        │
│  1. full Chrome load chat.z.ai                 │
│  2. capture Log1 DeviceConfig (fingerprint ✅) │
│  3. decrypt → {Q, sessionKey, ip, qts}         │
│  4. mint 1 token canary → VerifyCaptchaV3      │
│     PASS (T001) mới ghi file; FAIL thì thoát   │
│     (KHÔNG ghi đè session cũ)                  │
│  5. qbless.json (2KB) — Chrome tắt             │
└────────────────────┬───────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────┐
│  BRIDGE (Go thuần, 0 browser 0 daemon 0 DB)    │
│  internal/zbridge/qmint.go                      │
│  • đọc qbless.json (cache theo mtime)           │
│  • mint Token-A: ~0ms/token, vô hạn            │
│  • health tracker: 3 verify FAIL liên tiếp     │
│    → Q DEAD → fallback DB reserve + nag log    │
│    (Q không có TTL theo tuổi — không còn      │
│    max-age gate; session mới → tracker reset)  │
└─────────────────────────────────────────────┘
```

## 2. Hướng dẫn chạy

### 2.1. Cài đặt (lần đầu)

```sh
# Dependencies hệ thống của Chromium (chỉ 1 lần)
npx playwright install-deps
# hoặc: sudo apt install -y libnss3 libatk1.0-0 libatk-bridge2.0-0 \
#        libcups2 libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 \
#        libxrandr2 libgbm1 libasound2 libpangocairo-1.0-0

# Build
go build -trimpath -ldflags="-s -w" -o q-bless ./cmd/q-bless
go build -trimpath -ldflags="-s -w" -o zai-proxy .
```

Lần chạy đầu, nếu chưa có Chromium build, `q-bless` tự `playwright.Install` (in `⏳ …installing`). Sau đó mọi lần chạy không cần cài thêm gì.

### 2.2. Bless Q (thủ công — ~30 giây)

```sh
./q-bless -out qbless.json
```

Output chuẩn:

```
🪄 q-bless: launching Chromium to bless a fresh Q…
✅ Log1 captured in 27.6s: Q=3795d282…-h-17884… sk=08870e5a… ip=2405:4803:… qts=1788405598930
canary verify resp: { … "VerifyCode":"T001","VerifyResult":true … }
✅ canary PASS — Q proven blessed (T001)
✅ session written: qbless.json (total 28.4s, Chromium now dead)
```

**Exit codes:** `0` = PASS & đã ghi · `2` = canary FAIL (không ghi đè session cũ) · `1` = lỗi khác (mạng, page, decrypt).

### 2.3. Chạy bridge

```sh
./zai-proxy          # tự tìm qbless.json — không cần flag/env gì thêm
```

Thứ tự nguồn token khi captcha cần token:
**`qbless.json`** → `tokens.sqlite (DB dự trữ)`. (token-minter daemon đã bị xóa khỏi branch này — chỉ còn trong lịch sử nghiên cứu.)

Ép chạy thuần qbless (khi test, hoặc khi muốn tắt hẳn daemon):

```sh
TOKEN_MINTER_URL=off ./zai-proxy
```

### 2.4. Chính sách re-bless — theo sự kiện, KHÔNG theo lịch (cập nhật 2026-09-05)

**Q KHÔNG có TTL theo thời gian.** Bằng chứng tích lũy (chi tiết `EXP_CAPTCHA_RESULTS.md` §10.4): PASS ở tuổi 18h39m (live-verify 2026-09-05), probe nền 38/38 round tới 14h01m không FAIL lẻ, mint vô hạn, **khác IP bless vẫn PASS**. SDK AliyunCaptcha lưu Q vào localStorage mãi mãi và không có cơ chế refresh — server được thiết kế để chấp nhận Q vô thời hạn. Q chết theo **sự kiện** (schema/appKey rotation, trust revoke, device purge) — không dự đoán được bằng tuổi.

Do đó **không cần cron re-bless định kỳ**. Bridge tự theo dõi sức khoẻ Q từ verify thật:

- Mọi token qbless-mint đi qua VerifyCaptchaV3 đều feed health tracker (`qmint.go`).
- **3 FAIL liên tiếp** → Q đánh dấu DEAD → mint dừng, tự rơi về `tokens.sqlite` reserve, nag log 1 lần/h cho operator chạy lại `q-bless`.
- Session file mới (re-bless tay) → tracker tự reset.

Chạy bless **1 lần duy nhất**; chỉ re-bless khi bridge nag. Audit độc lập (tùy chọn, không bắt buộc):

```sh
# crontab — canary audit mỗi giờ; FAIL thì tự re-bless (30s)
0 * * * * cd /opt/zai-proxy && ./q-bless -probe -once -quiet || ./q-bless -out qbless.json >> q-bless.log 2>&1
```

Hoặc systemd timer (2 file) nếu muốn audit bằng systemd:

```ini
# /etc/systemd/system/qbless.service
[Unit]
Description=Z.AI bridge — Q bless refresh

[Service]
Type=oneshot
WorkingDirectory=/opt/zai-proxy
ExecStart=/opt/zai-proxy/q-bless -probe -once -quiet
```

```ini
# /etc/systemd/system/qbless.timer
[Unit]
Description=Hourly Q canary audit (self-heals via OnFailure)

[Timer]
OnCalendar=*-*-* *:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

```sh
sudo systemctl daemon-reload && sudo systemctl enable --now qbless.timer
```

### 2.5. Flags & env (tham chiếu)

| Flag / Env | Mặc định | Ý nghĩa |
|---|---|---|
| `-out` | `qbless.json` | file session sẽ ghi (atomic: tmp+rename) |
| `-verify` | `true` | canary trước khi ghi — **đừng tắt trừ khi debug** |
| `-timeout` | `60`s | chờ Log1 response |
| `QBLESS_CHROME` | auto | override đường dẫn **full Chrome binary** (phải là `chrome` đầy đủ, không phải headless-shell) |
| `QBLESS_LINGER` | `15000`ms | giữ Chrome sống sau Log1 để SDK chạy nốt handshake (Init/dynamicJS/UploadLog) |
| `QBLESS_STEALTH` | (tắt) | bật lại stealth init-script — **biết rõ làm hỏng fingerprint**, chỉ để nghiên cứu |
| `QBLESS_FILE` | auto | bridge: đường dẫn session (tìm `./qbless.json` → cạnh `tokens.sqlite` → `/var/lib/zai/qbless.json`) |
| `QBLESS_MAX_AGE` | *(không đặt)* | bridge: override operator — session già hơn bị bỏ qua, rơi về DB reserve. Mặc định KHÔNG giới hạn tuổi (Q không có TTL theo thời gian; sống chết theo verify result) |

## 3. Vì sao QBless hơn hai phương án cũ

### 3.1. Bảng so sánh trực tiếp (số đo thực)

| Chỉ số | (1) Harvest batch + DB | (2) Daemon mint 24/7 | **(3) QBless** |
|---|---|---|---|
| RAM steady-state | ~0 (nhưng collector định kỳ cần ~700MB khi chạy) | **694MB PSS** (Chrome tree sống vĩnh viễn, đo `smaps_rollup`) | **~0** (Chrome chỉ 29s mỗi lần bless — 1 lần duy nhất cho tới khi Q chết sự kiện) |
| CPU | collector ~150s/batch | Chrome render liên tục | ~0 sau lần bless đầu |
| Chrome uptime | phút (batch) | vĩnh viễn → leak RAM, footprint lớn | **29s, 1 lần (re-bless chỉ khi Q DEAD)** |
| Latency mint token | 0 (đọc DB) nhưng kho cạn dần | ~10-13ms (HTTP + page) | **~0ms** (thuần Go, in-process) |
| Giới hạn token | theo số thu được | vô hạn | **vô hạn** |
| TTL nguồn | token ≥2h32m sau harvest | Q đóng băng theo page-load, restart mỗi 2h | **Q không TTL theo thời gian** (PASS 18h39m, 38/38 probe tới 14h01m) — chết theo sự kiện, bridge tự phát hiện |
| SPOF | kho DB cạn | page chết phải self-heal | file 2KB; health tracker tự fallback DB reserve khi Q DEAD |
| Playwright trong runtime | không | **có** (daemon) | **không** |
| DB bắt buộc? | **có** (cốt lõi) | không (dự trữ) | **không** (chỉ dự trữ khẩn cấp) |
| Cần tiến trình ngoài? | collector định kỳ | daemon phải sống | **không** (bless 1 lần; audit cron tùy chọn) |
| Footprint trước Aliyun | batch lớn dễ thấy | page sống lâu, lặp `z_um.getToken()` vô hạn | 1 Chrome "người dùng" vào 30s rồi rời đi — gần như không footnote |

### 3.2. Ưu điểm riêng của QBless

1. **RAM/CPU gần bằng 0** — Chrome tắt giữa 2 lần bless. VPS 1GB vẫn dư sức chạy bridge đầy tải.
2. **Mint 0ms in-process** — không round-trip tới daemon; `getNextToken()` chỉ là đọc JSON + AES + MD5.
3. **Self-proving (canary)** — Q không bao giờ được "tin mù": mỗi bless verify T001 trước khi ghi. Session FAIL không đụng tới session đang chạy.
4. **Stateless & restart-an toàn** — bridge restart chỉ đọc lại file; bless fail giữa chừng không phá session cũ (atomic write).
5. **Bỏ daemon, bỏ collector, bỏ DB** khỏi luồng vận hành — 3 moving parts thành 1 file tĩnh.
6. **Footprint như người dùng thật** — định kỳ một Chrome vào trang 30s rồi rời đi; so với daemon lặp `getToken()` hàng nghìn lần trên cùng page, khó bị anomaly-detection bắt hơn.

### 3.3. Nhược điểm & rủi ro của QBless

| Nhược điểm | Mức độ | Giảm thiểu |
|---|---|---|
| **Q chết theo sự kiện, không theo tuổi** (schema/appKey rotation, trust revoke) — không dự đoán được trước | Trung bình | Bridge tự theo dõi từ verify thật: streak ≥3 FAIL → Q DEAD → tự rơi về `tokens.sqlite` reserve + nag 1 lần/h. DB reserve là cầu chì trong lúc operator re-bless tay (30s). |
| **Cửa sổ phát hiện chết** — nếu Q chết, 1-2 request đầu có thể tốn verify FAIL trước khi tracker kịp đánh dấu | Thấp | Breaker + reserve chặn burn; tracker chỉ cần 3 FAIL (≈ vài giây) để chuyển nguồn. |
| **Aliyun đổi cách chấm trust** (vd gắn theo IP egress) | Trung bình | Verify thật chính là "địa chấn kế": đổi gì → FAIL hàng loạt → tracker tự đánh dấu DEAD → fallback, không chết im lặng. |
| **Chrome vẫn phải có trên host** (~390MB disk) | Nhẹ | Chỉ cần cho lần bless (1 lần duy nhất + mỗi lần re-bless). Không giữ trong RAM. |
| **1 Q dùng chung cho toàn bridge** | Nhẹ | Không quan sát thấy rate-limit theo Q (18h39m mint thoải mái). Nếu xuất hiện → bless đa session + round-robin. |

### 3.4. Khi nào nên dùng phương án nào

- **VPS/máy chủ dài hạn, ít RAM, muốn ổn định tối đa** → **QBless** (mặc định).
- **Host không cài được Chromium** (container khóa, no-display) → daemon mint ở **máy khác** trỏ `TOKEN_MINTER_URL` sang; hoặc DB dự trữ lớn.
- **Traffic cực cao + Aliyun rate-limit theo Q** → QBless **đa session** (bless N file, round-robin trong qmint).
- **Canary FAIL kéo dài** (Aliyun đổi giao thức) → DB dự trữ là chỗ trú cuối: nạp lại bằng `scripts/refill_reserve.go` (mint thuần Go từ session qbless sống) hoặc bless session mới.

## 4. Runbook vận hành

### 4.1. Khởi động lần đầu trên máy mới

```sh
git clone … && cd zai-proxy
go build -trimpath -ldflags="-s -w" -o q-bless ./cmd/q-bless
go build -trimpath -ldflags="-s -w" -o zai-proxy .
./q-bless -out qbless.json       # lần đầu tự cài Chromium build nếu thiếu
./zai-proxy
```

### 4.2. Kiểm tra sức khoẻ hằng ngày

```sh
# Session bao nhiêu tuổi? (Q không có TTL theo thời gian — tuổi chỉ là thông tin)
python3 -c "import json,time; s=json.load(open('qbless.json')); print('age: %.1fh, verified: %s' % ((time.time()*1000-s['blessedAt'])/3.6e6, s['verified']))"

# Bridge health — tìm nag DEAD (nếu có) trong log:
grep -E "\[QBless\].*(DEAD|FAIL)" zai-proxy.log | tail -5

# Audit độc lập (không cần, bridge đã tự theo dõi):
./q-bless -probe -once -quiet && echo "Q ALIVE" || echo "Q DEAD — re-bless"
```

### 4.3. Sự cố thường gặp

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| `❌ canary verify FAILED` khi bless tay | Q lần đó không được bless (mạng, Aliyun đổi) | Session cũ vẫn chạy (không ghi đè). Thử lại; FAIL ≥3 lần liên tiếp → xem 4.4. |
| Log bridge `[QBless] Q marked DEAD after N FAILs` / `[QBless] Q DEAD …` | Q chết sự kiện (rotation/revoke) — tracker đã chuyển sang DB reserve | Chạy `./q-bless -out qbless.json` (30s) — session mới tự reset tracker về alive. |
| `timeout waiting for Log1 DeviceConfig` | chat.z.ai chậm, selector đổi | tăng `-timeout 90`; kiểm tra `textarea` selector |
| `chromium launch: … no such file` | Chromium build chưa có / `QBLESS_CHROME` sai | chạy 1 lần trên máy có mạng để tự cài; hoặc set `QBLESS_CHROME` |
| Log `[Minter] daemon unreachable` | env vẫn trỏ daemon đã bỏ | vô hại (breaker 60s); hoặc `TOKEN_MINTER_URL=off` |
| Verify F001 **hàng loạt** từ token qbless | session bị từ chối đột ngột | tracker tự đánh dấu DEAD sau 3 FAIL → bridge đã tự dùng DB reserve; chỉ cần re-bless tay |

### 4.4. Rollback / rời qbless

```sh
rm qbless.json                   # bridge tự rơi về DB reserve (tokens.sqlite)
# nạp lại reserve bằng q-mint thuần Go (không cần browser):
./q-bless -probe                 # kiểm tra session hiện có còn sống không
# mint N token từ session qbless hiện có vào DB:
#   scripts/refill_reserve.go — go run scripts/refill_reserve.go -n 100
```

Không cần đổi code — nguồn sống theo thứ tự ưu tiên qbless.json → tokens.sqlite, xóa file là rollback.

## 5. Cơ sở khoa học (tóm tắt — bằng chứng đầy đủ trong `EXP_CAPTCHA_RESULTS.md`)

- **F001 root-cause:** trust-score theo Q tại Log1, phụ thuộc fingerprint kết nối (Go/uTLS/curl/Node Log1 → F001; Chrome thật Log1 → PASS; token gomint verify bằng Chrome thật → T001).
- **Q browser-born tái sử dụng được:** BURNIN 3/3 token khác nhau cùng Q PASS; Q 11h34m tuổi vẫn 30/30 PASS (probe nền, probe đang chạy tiếp).
- **Token formula byte-exact:** `base64("SG_WEB#Q#w#tC#" + MD5("SG_WEB#Q#w#tC#daye,raolewoba!"))`.
- **w-blob 111 field** (Token-A shape, w[77]=""): pass verify với certifyId mới — bisect 10/10.
- **Anti-replay chỉ là hash (Q,w):** đổi 1 field w → pass trở lại.
- **Flow tối thiểu:** Log1 → Init → dynamicJS (CDN công khai) → UploadLog → Verify. **Không Log2/Log3** (gửi kèm thậm chí poison Q tươi).

## 6. FAQ ngắn

**Q: Sao không bỏ hẳn Chrome?**
A: Transport Go/curl/Node chưa bắt chước được HTTP/2 frame ordering của Chrome 151 — Log1 qua chúng luôn F001. Hướng dài hạn: `bogdanfinn/tls-client` (xem §11 EXP_CAPTCHA_RESULTS.md).

**Q: Một session mint được bao nhiêu token?**
A: Vô hạn trong TTL (anti-replay chỉ hash (Q,w) — w luôn fresh). Giới hạn là thời gian, không phải số lượng.

**Q: Session file chứa gì nhạy cảm?**
A: Q, sessionKey (AES key w-blob), IP — không có credential Z.AI/Aliyun. File chmod 0600 + `.gitignore` đã chặn `qbless.json`.

**Q: Cần nhiều session không?**
A: 1 session đủ cho traffic bình thường. Rate-limit theo Q nếu xuất hiện → bless N session + round-robin (mở rộng nhỏ vì file đã tách sẵn).

**Q: q-bless có headless được không?**
A: ĐANG headless — điểm mấu chốt là dùng binary **full Chrome** (không phải `chromium_headless_shell`) và **không** stealth patches (chúng làm hỏng fingerprint).

**Q: Debug canary với token ngoài?**
A: `VERIFY_TOKEN=… VERIFY_CID=… ./q-bless` chạy đúng verify path trên token bất kỳ (dùng để kiểm chứng verify pipeline độc lập với bless).
