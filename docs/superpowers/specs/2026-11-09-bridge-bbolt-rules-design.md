# Spec: Bộ nhớ bền (bbolt), sửa bug restart, tối ưu hiệu năng, và bộ quy tắc tự tổng hợp `rules.md`

Ngày: 2026-11-09 • Trạng thái: thiết kế đã duyệt ở mức khái quát, chờ duyệt spec chi tiết.
Sau khi duyệt: viết implementation plan (skill writing-plans) rồi mới code.

## 1. Bối cảnh & mục tiêu

`tg-bridge` hiện là bridge trả lời tin nhắn Telegram Business bằng LLM, 0 dependency
ngoài stdlib, binary arm64 ~6.0 MB, RSS ~9 MB. Nhược điểm thực tế:

1. Restart = mất toàn bộ ngữ cảnh chat (LRU chỉ ở RAM).
2. Restart = mất update offset → bot trả lời lại tin nhắn cũ (Telegram giữ update
   chưa-ack tới 24 h); `DailyReplyCap` cũng bị reset.
3. Trần RAM của lịch sử chat chưa bị chặn theo tổng ký tự (tệ nhất 500 chat × 20 tin
   × 8 KB ≈ 40 MB lý thuyết).

Mục tiêu đợt này: giữ triết lý "nhẹ" (không SQLite) nhưng bot **nhớ được sau
restart**, **không trả lời tin cũ**, RAM/disk bị **chặn trần + tự dọn dẹp**, và thêm
**bộ quy tắc chung AI tự tổng hợp** kiểu AGENTS.md nhưng không lưu dữ liệu nhạy cảm.

### Quyết định đã chốt với chủ sở hữu

- Bộ nhớ bền dùng **bbolt** (pure-Go, single-file KV, atomic commit, tự tái dùng
  page → không cần VACUUM như SQLite). **Không dùng SQLite** (biến thể pure-Go làm
  binary +11–15 MB và tăng RAM; biến thể gốc cần CGO, phá build `CGO_ENABLED=0`).
- Thiết kế được duyệt ở mức: P0 (bug) + P1 (bbolt + rules.md + tối ưu). P2/P3
  (fallback model, `/stats` mở rộng, media, metrics) nằm ngoài đợt này.

## 2. Phạm vi

### Làm

- Thay `state.json` bằng bbolt `bridge.db` (một nguồn chân lý, kèm migration nhẹ).
- Persist: `paused`, `offset`, `day`, `day_count`, `conn_ok`, `rules_on`, lịch sử chat.
- Bộ lọc backlog theo tuổi tin nhắn khi restart.
- Chặn trần lịch sử theo ký tự (`history_max_chars`).
- Tập tối ưu hiệu năng/RAM (Section 6).
- Tính năng `rules.md`: AI tự tổng hợp, sanitizer chặn dữ liệu nhạy cảm, lệnh owner.
- Migration một chiều từ `state.json` cũ.
- Test (unit + integration), cập nhật README, bump version `1.1.0`.

### Không làm (non-goals)

- SQLite bất kỳ biến thể nào; lưu lịch sử RAW của tất cả tin nhắn ra đĩa.
- Fallback model, media/vision, metrics endpoint, webhook mode, mã hoá DB.
- Đổi triết lý log "không bao giờ log nội dung tin nhắn/token/key".

## 3. Lưu trữ bbolt

### File & dependency

- `go.etcd.io/bbolt/v3` — dependency duy nhất mới. Binary dự kiến 6.0 → ~7.6 MB.
- File DB: `db_file` (mặc định `bridge.db` cạnh `config.json`), mode `0600`.
- Mở bằng `bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})`;
  nếu file đã bị tiến trình khác mở → thoát với lỗi rõ (khớp ràng buộc
  "chỉ 1 poller" của Telegram).

### Schema

Bucket `meta` (value là ASCII/JSON, dễ debug):

| key | value |
|---|---|
| `schema` | `1` |
| `paused` | `0` hoặc `1` |
| `offset` | int64 |
| `day` | `2006-01-02` |
| `day_count` | int |
| `conn_ok` | JSON `map[string]bool` |
| `rules_on` | `0` hoặc `1` |

Bucket `chats`: key = chat key hiện có (`b:<conn>:<chatID>` / `d:<chatID>`), value JSON:

```json
{"introduced": true, "updated": 1731123456, "msgs": [{"role": "user", "content": "..."}]}
```

Ghi chú: `msgs` giữ cấu trúc `ChatMsg` hiện tại (merge tin cùng role đã có sẵn).

### Mô hình ghi (chống phá đĩa / SD card)

- **Chỉ một goroutine duy nhất được ghi DB**: flush worker. Goroutine khác chỉ đánh
  dirty-flag dưới `b.mu` — loại tranh chấp writer của bbolt ngay từ thiết kế.
- Trạng thái dirty: `dirtyMeta bool` + `dirtyChats map[string]struct{}` (đặt dưới `b.mu`).
- Nhịp flush: `flush_sec` (mặc định 30, clamp ≥5). Worker chờ timer OR signal
  (context); mỗi vòng: nếu có dirty → 1 tx `Update`:
  1. ghi meta nếu `dirtyMeta`;
  2. ghi từng chat trong `dirtyChats`;
  3. chạy **prune** trong cùng tx (mục "Tự dọn dẹp" dưới);
  4. xoá dirty (nếu trong lúc tx có biến đổi mới xảy ra, giữ dirty cho vòng sau).
- Flush bắt buộc khi SIGTERM/ngừng chạy (path `Run` trả về đã có saveState — thay
  bằng flush DB).
- `offset` chỉ đánh dirty khi tăng — crash mất tối đa offset của ~30 s update →
  replay nhỏ, đã được bộ lọc backlog che (Section 4).

### Tự dọn dẹp (ngăn DB phình)

- `memory_days` (mặc định 7, clamp ≥1): trong tx prune, xoá chat có `updated` cũ hơn
  hạn — ngữ cảnh trên đĩa tự hết hạn đúng tinh thần "dữ liệu ngắn hạn, không lưu trữ
  dư". Chat **bị LRU đuổi vì RAM** vẫn giữ ở đĩa → restart sau vẫn nhớ lại.
- `max_stored_chats` (mặc định 1000, clamp ≥ `max_chats`): sau khi xoá hết hạn, nếu
  vẫn còn nhiều hơn giới hạn, sort theo `updated` giảm dần và xoá các chat cũ nhất
  đến khi đủ. Trần tệ của DB ≈ 1000 × 8 KB ≈ 8 MB (thực tế ~1–3 MB).
- Không `Compact()` định kỳ (bbolt tự quản free-list); chỉ compact khi boot nếu DB
  lớn hơn `db_compact_mb` (mặc định 20) — I/O tạm khi boot.

### Restore khi boot

- Mở DB → iterate `chats`: decode; chat quá `memory_days` → xoá tại chỗ (scan đầu
  vừa đóng vai prune đầu tiên); còn lại sắp theo `updated` giảm dần → đẩy vào LRU
  map theo thứ tự (thừa `max_chats` bị LRU đuổi như cũ, vẫn giữ trên đĩa).
- Đọc meta: `paused`, `offset`, `day`/`day_count`, `conn_ok`, `rules_on`.
- Migration: nếu DB chưa có bucket và `state_file` cũ tồn tại → nhập `paused`,
  `conn_ok` vào DB (ghi 1 lần), log INFO; **không xoá state.json** (chủ tự dọn;
  README ghi chú). Còn `state_file` sau mốc này bị bỏ qua hoàn toàn.

## 4. Sửa bug restart (P0)

- Thêm trường `Date int64` vào `Message` (Telegram `date`, có ở cả message thường
  lẫn business_message).
- `max_backlog_min` (mặc định 10, 0 = tắt): khi nhận tin nhắn, nếu
  `m.Date > 0 && now − m.Date > hạn` → `return "stale"` (không trả lời, không đếm
  quota, vẫn advance offset). Run đầu tiên chưa có offset cũng không gây "bot hồi
  sinh và trả lời 24 h tin cũ".
- `day`/`day_count` bền trong meta (cắm dirty flag vào `rateOK` nơi cuối đổi ngày).

## 5. Tính năng `rules.md` (bộ quy tắc chung AI tự tổng hợp)

### Hình thức & nơi lưu

- Một file markdown duy nhất: `rules_file` (mặc định `rules.md` cạnh config), mode 0600.
- Hình thức chọn: "file nguyên khối, AI viết lại hoàn toàn mỗi lần tổng hợp" (thay
  cho danh sách fact có cấu trúc trong DB) vì con người đọc/sửa trực tiếp được —
  đúng tinh thần AGENTS.md.
- Trần: `rules_max_chars` (mặc định 4000, clamp ≥200), dùng chặn cả file chủ tay
  sửa quá dài.

### Đưa vào prompt

- `Bridge` giữ `rulesText` trong RAM. `replyJob` dựng system:
  `sysPrompt + "\n\n[Long-term rules]\n" + rulesText` (đã chặn trần khi nạp/ghi).
  Áp cho **mọi** chat (quy tắc chung). File chưa có → không chèn gì.
- `/reload` nạp lại cả system prompt lẫn rules file.

### Vòng tự tổng hợp

- Sau mỗi reply thành công → `rulesPending++` (dưới `b.mu`). Đạt `rules_update_every`
  (mặc định 10, clamp ≥2) → đặt 0 và đẩy job distiller (goroutine **dùng chung
  `llmSem`** — không tạo concurrency mới, không tăng RAM).
- Input job: rules hiện tại + tối đa 12 tin cuối của tối đa 3 chat vừa hoạt động
  nhất (lấy từ LRU, cap tổng ~6000 ký tự; role-tag người dùng/trợ lý; không ghi
  ra đĩa — chỉ gửi tới LLM như mọi lần gọi LLM khác).
- Prompt contract key cốt lõi (EN): hai khối `=== CURRENT RULES ===` và
  `=== RECENT CHATS ===`; output chỉ được là `UNCHANGED` hoặc full markdown mới
  (thêm/sửa/xoá rule: giữ rule còn đúng, bỏ rule sai/hết hạn, thêm rule mới).
  Cấm: tên thật, SĐT, email, handle Telegram khác tag chủ, địa chỉ, giá tiền cụ
  thể, trích lời nguyên bản; chỉ quy tắc hành vi khái quát.
- Parse: strip code fence; `UNCHANGED` → bỏ qua. Khác → **sanitizer** (dưới) →
  chặn trần ký tự → so hash với bản cũ (chỉ ghi khi khác) → ghi atomic
  (tmp+rename, 0600) → cập nhật `rulesText` trong RAM. **DB không chứa rules.**
- Lỗi LLM của distiller: log WARN, giữ file cũ, chờ đợt sau (không retry riêng).

### Sanitizer chống dữ liệu nhạy cảm (2 lớp: prompt + regex)

Chạy trên TỪNG DÒNG output; regex cấm (biên dịch dạng Go, không phụ thuộc locale):

| Loại | Pattern |
|---|---|
| Email | `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}` |
| URL (mặc định chặn; bật qua `rules_allow_url`) | `https?://`, `www\.`, `\.(com|net|vn|org|io|biz)\b` |
| Chuỗi ≥6 chữ số (SĐT/CCCD/ID) | `[0-9]{6,}` |
| Tiền tệ + con số | `(?i)\d+[,.]?\d*\s*(vnd|đ|usd|\$|k\b|tr\b|triệu|tỷ)` |
| Handle Telegram (chỉ chủ được phép) | `@[A-Za-z0-9_]{3,}` — chỉ pass khi đúng `@owner_username` |
| Nhắc ID tường minh | `(?i)\bid\b[^0-9]{0,3}\d{3,}` |

- 1 dòng vi phạm → vứt dòng; **hơn 50% dòng bị vứt** → từ chối TOÀN BỘ bản ghi
  (giữ file cũ, log WARN — model đang cố nhồi dữ liệu).
- Chống trích nguyên văn: của từng tin trong batch, lấy 3 khối 40 ký tự cách đều;
  khối nào xuất hiện trong output → từ chối toàn bộ.
- Kết quả rỗng (mọi dòng bị vứt) → coi như không có gì mới, giữ file cũ.

### Lệnh owner (mở rộng)

- `/rules` — gửi nội dung hiện có (hoặc "(trống)").
- `/rules reset` — xoá file + bản RAM.
- `/rules off` / `on` — cờ `rules_on` (bền trong meta): off = bỏ distill + bỏ inject.
- Cho phép chạy ở chat riêng của bot **và trong business chat** khi chủ gõ: nhánh
  owner-takeover thêm `strings.HasPrefix(text, "/")` → dispatch
  `ownerCommand(chatID, conn, cmd, args)`.
- `/forget` trong business chat: xoá chat state hiện tại (RAM + record DB) — tiện
  xoá sạch ngữ cảnh nhạy cảm của 1 chat.

## 6. Tối ưu hiệu năng / RAM

1. **Một `http.Transport` dùng chung** cho Telegram + LLM: `MaxIdleConnsPerHost=4`
   (bỏ 2 pool × 16 idle conns hiện tại; HTTP/2 thực tế chỉ cần ~1 conn/host).
2. `telegram.go`/`llm.go`: dùng `json.NewDecoder` trên `LimitReader(4<<20)`, bỏ
   `io.ReadAll` (double-buffer); envelope `result` vẫn là `RawMessage` cho `out`.
3. `Run`: hoist map params `getUpdates` ra khỏi vòng for; chỉ đổi `offset` key.
4. `history_max_chars` (mặc định 8000, clamp ≥200): `rememberLocked` theo dõi tổng
   `cs.bytes`; vượt trần → drop tin cũ nhất đến vừa; giữ ngữ nghĩa merge cùng role.
5. GC giữ nguyên (`GOGC=50` + memory limit 64 MiB). Không thêm sync.Pool cho body nhỏ.

## 7. Config keys (mới)

Config JSON giữ tương thích (các key cũ nguyên vị trí; `state_file` chỉ được đọc
1 lần khi migrate rồi bị bỏ qua).

| key | mặc định | clamp |
|---|---|---|
| `db_file` | `bridge.db` cạnh config | — |
| `flush_sec` | 30 | ≥5 |
| `memory_days` | 7 | ≥1 |
| `max_stored_chats` | 1000 | ≥ `max_chats` |
| `max_backlog_min` | 10 | ≥0 |
| `history_max_chars` | 8000 | ≥200 |
| `rules_file` | `rules.md` cạnh config | — |
| `rules_enabled` | true | explicit `false` = tắt tính hoàn toàn |
| `rules_max_chars` | 4000 | ≥200 |
| `rules_update_every` | 10 | ≥2 |
| `rules_allow_url` | false | — |
| `db_compact_mb` | 20 | ≥1 (0 = không compact) |

## 8. Cấu trúc mã

- **Mới**: `storage.go` (bbolt open/close, helpers meta/chats, flush worker,
  restore, prune), `rules.go` (distiller input builder, sanitizeRules, ghi file
  atomic).
- **Sửa**: `bridge.go` (dirty flags, restore/hook, backlog filter, history cap,
  lệnh mới, counter distiller), `config.go` (keys mới + clamp), `telegram.go`
  (thêm `date`), `main.go` (mở DB, 1 transport, start/stop flush worker),
  `README.md`, `go.mod`.
- **Test mới**: `storage_test.go`, `rules_test.go`; phần còn bổ sung trong
  `bridge_test.go` / `integration_test.go`.

## 9. Test & kiểm chứng

- Unit: sanitizeRules (bảng: SĐT VN, email, URL, giá "500k", @handle lạ, trích
  nguyên văn, dòng sạch được chấp nhận), roundtrip bbolt meta/chats, prune
  `memory_days` + `max_stored_chats` (fake clock sẵn có), bộ lọc backlog,
  history char cap + merge, counter distiller, `/rules` reset/off/on,
  `/forget` trên business path.
- Integration: giả lập restart — đóng Bridge (flush), mở lại → ngữ cảnh + offset +
  day_count + rules giữ nguyên; test LOAD_N vẫn đạt tương đương.
- Lệnh kiểm tra: `go vet ./...`, `go test -race -count=1 .`, build arm64 ≤ 8.0 MB.
- Chuẩn log giữ nguyên: distiller chỉ log sự kiện/lỗi đếm được, không log nội dung
  tin nhắn hay nội dung rules.

## 10. Tiêu chí nghiệm thu

1. Restart: bot KHÔNG trả lời tin cũ hơn `max_backlog_min`, ngữ cảnh chat hợp hạn
   đầy đủ, quota/đếm trong ngày giữ nguyên.
2. DB không vượt trần: hết hạn `memory_days`, cap `max_stored_chats` hoạt động đúng.
3. rules.md chỉ chứa quy tắc khái quát; mọi dạng nhạy cảm ở Section 5 bị chặn
   (unit test chứng minh); chủ có thể sửa tay và `/reload` thấy ngay.
4. Binary arm64 ≤ 8.0 MB; RSS điển hình tăng khiêm tốn (≤ ~2 MB).
5. `go test -race -count=1 .` và `go vet` sạch; README đúng với config mới.

## 11. Rủi ro & biện pháp

| Rủi ro | Biện pháp |
|---|---|
| bbolt bị 2 tiến trình mở chung | `Timeout: 5s` + lỗi rõ (khớp ràng buộc 1-poller) |
| Crash mất ≤ 30 s offset | chấp nhận; bộ lọc backlog + Telegram replay an toàn |
| AI churn viết lại file mỗi lần tổng hợp | ghi chỉ khi hash thay đổi + sentinel `UNCHANGED` |
| Chi phí LLM của distiller | cadence 10 reply; nâng `rules_update_every` dễ dàng |
| rules.md phình khi AI nhồi | trần `rules_max_chars` hard cap + kiểm soát nội dung |
| bbolt mmap chiếm RSS tạm thời | file nhỏ, file-backed pages tự thu hồi; đo qua test LOAD_N |
