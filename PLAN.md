# Kế hoạch Port Tính năng vào ARTEX (`konanguyen/ARTEX`)

Tài liệu này theo dõi lộ trình port từng tính năng từ hai nhánh fork đang phát triển mạnh:
- **`baarai/main`** (`BaaRai/ARTEX` — commit `b5c0bb9`, bản `v0.6.0` + các PR `#194`–`#222`)
- **`cskwork/main`** (`cskwork/scopeweaver` — commit `ea57586`, bản `v0.9.1`)

*(Lưu ý: `origin` / `mhtsec/ARTEX` và `cmc/ARTEX` có 0 commit mới hơn `HEAD` nên không còn tính năng nào cần port từ hai nhánh đó).*

---

## Nguyên tắc bắt buộc khi Port (theo `AGENTS.md`)

1. **Ngôn ngữ Frontend (`web/`)**:
   - Mọi tính năng giao diện port sang đều phải hỗ trợ đủ 3 ngôn ngữ chuẩn: **`en-US` (mặc định), `vi-VN`, `zh-CN`** và giữ đồng bộ số lượng key trong `web/src/i18n/locales/{en,vi,zh-CN}.ts`.
   - Không port hệ thống 6 ngôn ngữ (`ko`, `ja`, `es`, `zh-Hant`) của `cskwork` trừ khi có yêu cầu riêng.
   - Bản dịch tiếng Việt dịch sát từ nguồn tiếng Trung giản thể, giữ nguyên thuật ngữ kỹ thuật và từ **"Skills"** bằng tiếng Anh.
2. **Ngôn ngữ Backend (Go)**:
   - Backend Go vận hành **trực tiếp bằng tiếng Anh**. Không port tầng đa ngôn ngữ `locale/` của `cskwork` vào Go backend; chuyển các thông báo API/log của tính năng mới sang tiếng Anh chuẩn.
3. **Quy tắc miễn trừ System Prompt (`LLM System Prompt Exemption`)**:
   - Không tự ý sửa đổi hoặc dịch các LLM system prompt trong `agent/promptcatalog.go`, `agent/planner.go`, `agent/worker.go`, `agent/goals.go`, `agent/compaction.go`, `agent/wrapup.go`, `agent/retester.go`, `sidequestion/request.go`, `sidequestion/context.go`, `intercept/prompt.go` nếu chưa được xác nhận rõ ở bước port đó. Các mục có chạm vào danh sách file này được đánh dấu ⚠️ **`[Chạm Prompt]`** bên dưới.
4. **Quy trình kiểm thử sau mỗi mục port**:
   - Backend: `go build ./cmd/artex` và `go test` cho các package liên quan.
   - Web UI: `cd web && npx tsc --noEmit && git diff --check`.
   - Nếu có thay đổi Web UI hoặc hành vi ứng dụng: `docker compose up -d --build artex`, kiểm tra trên trình duyệt tại `http://localhost:8787` và chạy `./tests/run_tests.sh` (hoặc `NODE_PATH=web/node_modules node tests/verify_all_pages_browser.js`).

---

## Đã hoàn thành (Baseline `c9629af`)

- [x] Đa ngôn ngữ giao diện `en-US`, `vi-VN`, `zh-CN` (2.851 keys) + địa phương hóa Go backend sang tiếng Anh (`cded6f2`..`56bf0dd`).
- [x] Fix ổn định & bảo mật cơ bản (`df8d8b7`, `f3334ab`, `2320361`, `1c189c4`, `4cb687f`):
  - Xóa biến môi trường chứa secret trước khi spawn tiến trình con (`BaaRai #136, #146`).
  - Match `ARTEX_LLM_PROVIDER` không phân biệt hoa/thường (`BaaRai #140`).
  - Điền đủ task metrics ở detail endpoint & đóng băng deadline khi pause (`BaaRai #92, #110`).
  - Báo đúng số lượng asset bị ẩn ở cảnh báo malformed IP (`BaaRai #171`).
  - `insert_assets` fail-closed khi lỗi đọc rule & `exact_domain` khớp đúng host (`cskwork #63, #64`).
  - Chỉ gửi header `MCP-Protocol-Version` sau khi `initialize` xong (`mcphttp/client.go`).
  - Chấp nhận tiền tố `[model]`/`[Model]` ở intercept (`BaaRai #159`).
  - Sửa biểu đồ token hàng ngày tính trùng cache read (`cskwork #16`).
- [x] Tích hợp provider `opencode-go` + preset cấu hình UI + mặc định `User-Agent: artex` (`c9629af`, từ `cskwork #47`).

---

## Giai đoạn 1: Các tính năng độc lập, giá trị cao, ít rủi ro (Quick Wins)

Ưu tiên các tính năng nhỏ gọn, ít thay đổi schema phức tạp và không chạm vào LLM system prompt:

- [x] **1.1. Xuất danh sách Assets ra CSV / JSON (`cskwork #17` — `753fc5c`, `23a0293`, `912c17c`)**
  - **Mô tả**: Thêm API export CSV/JSON và nút xuất trên trang Assets (xuất các dòng đã chọn hoặc toàn bộ theo bộ lọc hiện tại, không bị giới hạn phân trang 50 dòng, giữ nguyên full URL).
  - **File chính**: `server/assets.go` (hoặc handler export), `db/assets.go`, `web/src/app/(main)/function/assets/page.tsx`, `web/src/lib/api.ts`, `web/src/i18n/locales/{en,vi,zh-CN}.ts`.

- [x] **1.2. Đàm phán phiên bản MCP (`MCP-Protocol-Version` fallback) & Sửa proxy scheme cho Custom Tool (`BaaRai #132, #187` — `44d755f`, `31e56a1`)**
  - **Mô tả**: Streamable HTTP MCP client tự đàm phán version (`2025-11-25` → fallback `2025-06-18`); sửa lỗi nhân đôi `http://http://...` khi bật `use_recording_proxy` ở custom HTTP tool.
  - **File chính**: `mcphttp/client.go`, `mcphttp/client_test.go`, `server/customtool.go`.

- [x] **1.3. Tuỳ chỉnh `User-Agent` theo LLM Profile & Giới hạn đồng thời `llm_max_concurrent` (`BaaRai #155` `9bf5d81` + `cskwork #14` `8c67daf`)**
  - **Mô tả**: Cho phép cấu hình `user_agent` riêng trên từng LLM profile (tối đa 256 bytes, để trống dùng mặc định `artex`); thêm cấu hình `llm_max_concurrent` giới hạn số request LLM đồng thời trên mỗi profile.
  - **File chính**: `db/config.go`, `db/settings.go`, `agent/provider.go`, `llmpool/`, `server/server.go`, `web/src/app/(main)/system/llm/page.tsx`, `web/src/app/(main)/system/settings/`.

- [x] **1.4. Tuỳ chỉnh giới hạn thông báo & Chống tràn gói Feishu Digest (`BaaRai #170, #172, #173, #174` — `cf0b0c1`, `3b6eb2f`, `9b7276d`, `b847867`)**
  - **Mô tả**: Thêm cài đặt `notify_summary_max_runes` (0–4000, mặc định 120) và `notify_assets_max_shown` (0–50, mặc định 3) trong `/api/settings` và trang System → Notifications; tách gói digest Feishu theo kích thước payload thực tế (< 16 KB) để không bị từ chối ở mốc 20 KB; sửa đếm số asset ẩn trong thông báo.
  - **File chính**: `notify/`, `db/settings.go`, `server/notify_api.go`, `web/src/app/(main)/system/notify/page.tsx`.

- [x] **1.5. Mở rộng Scope `host:<name>`, Ranh giới `fuzzy_domain` & Transaction cho `prove_goal` (`cskwork #42, #50, #65` — `77842b4`, `82c3c8a`, `8990040`)**
  - **Mô tả**: Hỗ trợ company scope loại `host:<name>` (khớp chính xác 1 hostname) bên cạnh `domain` (khớp cả subdomain); sửa `fuzzy_domain` khớp theo ranh giới nhãn tên miền đầy đủ (`.gov` không chặn nhầm `a.government.com`); gom ghi nhận edge bằng chứng và trạng thái của `prove_goal` vào một transaction nguyên tử.
  - **File chính**: `db/company_scope.go`, `db/asset_intercept_match.go`, `db/task_scope.go`, `agent/tools.go`, `web/src/app/(main)/function/companies/`.

- [x] **1.6. Chrome/Edge Extension `ARTEX Traffic Capture` + API `POST /api/traffic/ingest` (`BaaRai #33` — `d8f47d4`)**
  - **Mô tả**: Thêm endpoint `POST /api/traffic/ingest` (tối đa 50 mục/lô, mỗi body tối đa 256 KiB) và thư mục `extension/` (Manifest V3 Chrome/Edge extension dùng `chrome.debugger` Network domain) để đẩy lưu lượng duyệt web đã đăng nhập của operator (sau CAPTCHA/OTP/2FA/SSO) vào kho traffic của ARTEX.
  - **File chính**: `extension/{manifest.json,background.js,popup.html,popup.js}`, `traffic/external.go`, `server/traffic_ingest.go`, `server/server.go`.

---

## Giai đoạn 2: Bảo mật Hệ thống, Mã hóa Secrets & Kiểm soát Lệnh (Interception)

- [x] **2.1. Hardening Xác thực, Setup Token, Rate Limit Login & Tách Volume `jwt.key` (`BaaRai v0.5.0, v0.5.1` — `#137, #149, #150` + `cskwork v0.2.0`)**
  - **Mô tả**:
    - Sinh one-time `setup_token` (128-bit) in ra log khi chưa đặt mật khẩu admin; yêu cầu nhập trên `/setup` và `POST /api/auth/init` (`#137`).
    - Rate limit `POST /api/auth/login` theo IP (10 lần/phút, khóa lũy tiến tối đa 15 phút) hỗ trợ `ARTEX_TRUSTED_PROXIES` (`#150`).
    - Yêu cầu mật khẩu mới tối thiểu 12 ký tự; fail-closed trả HTTP 503 nếu lỗi đọc DB khi kiểm tra auth (`cskwork v0.2.0`).
    - Ẩn thông tin `version` ở `GET /api/health` khi gọi không có token.
    - Hỗ trợ `ARTEX_JWT_KEY_DIR` để lưu `jwt.key` ở volume riêng (`/app/keys`), không bị đăng xuất khi rebuild container (`#149`).
  - **File chính**: `server/auth.go`, `server/rate_limit.go`, `server/server.go`, `cmd/artex/main.go`, `docker-compose.yml`, `Dockerfile`, `web/src/app/setup/page.tsx`.

- [x] **2.2. Mã hóa Secrets At-Rest trong PostgreSQL (`BaaRai #213, #216, #217, #218` — `adf4b6d`, `5aab3d3`, `b74ff21`, `5122baa`)**
  - **Mô tả**: Mã hóa toàn bộ dữ liệu nhạy cảm lưu trong PostgreSQL: LLM API keys & profile proxy, MCP headers/env, thông tin kênh thông báo, search keys, global proxy, custom tool `exec` spec và evidence snapshot heads; chặn lợi dụng giá trị `__masked__` khi đổi URL/đích đến của MCP, notify hoặc custom tool.
  - **File chính**: `db/secrets.go`, `db/config.go`, `db/settings.go`, `db/tools.go`, `db/finding_traffic.go`, `server/server_mgmt.go`, `server/customtool.go`.

- [x] **2.3. Nâng cấp Command Interception: `decided_by`, Quy tắc `argv` Prefix & Closed-Question Judge (`BaaRai #159, #165, #208, #210, #211`)** ⚠️ **`[Chạm Prompt ở #208]`**
  - **Mô tả**:
    - Lưu và lọc người ra quyết định `decided_by` (`human`, `rule`, `model`, `system`) trên bảng lịch sử phê duyệt (`#165`) + backfill dữ liệu cũ (`#159`).
    - Hỗ trợ loại quy tắc `match_type=argv` khớp theo mảng đối số lệnh và nút *"Always allow pending Bash prefix"* kèm denylist cứng bảo vệ các trình thông dịch nguy hiểm (`#210`, `#211`).
    - Chế độ câu hỏi đóng (closed-question mode, fail-closed về `ask`) cho LLM Judge (`#208` — kiểm tra kỹ phần `intercept/prompt.go`).
  - **File chính**: `db/intercept.go`, `db/schema.sql`, `intercept/intercept.go`, `intercept/argv.go`, `server/intercept_api.go`, `web/src/components/approval-records.tsx`, `web/src/app/(main)/system/intercept/page.tsx`.

---

## Giai đoạn 3: Quản lý Task, Kho Tài khoản Test, Xác minh Lỗ hổng & Thu hồi Worker Output

- [x] **3.1. Kho tài khoản Test theo Task (`Per-Task Credentials Vault`, `BaaRai #3, #130` — `c177ab9`, `00f5d8c`)** ⚠️ **`[Chạm nhẹ Overview/Prompt]`**
  - **Mô tả**: Bảng `task_credentials`, API `GET/POST/DELETE /api/tasks/{id}/credentials`, mục khai báo tài khoản khi tạo Task, tab `Credentials` ở chi tiết Task (ẩn/hiện/copy mật khẩu), và 2 tool `save_task_credential` & `list_task_credentials` cho worker/pentest (hỗ trợ chế độ để trống mật khẩu cho agent tự đăng ký rồi lưu lại).
  - **File chính**: `db/task_credentials.go`, `db/schema.sql`, `agent/tools_creds.go`, `agent/toolcatalog.go`, `server/task_credentials.go`, `web/src/app/(main)/function/tasks/detail/_tabs/credentials-tab.tsx`, `web/src/app/(main)/function/tasks/page.tsx`.

- [x] **3.2. Thu hồi / Xóa kết quả của Worker đã hoàn tất (`Delete Finished Worker Output`, `cskwork #62, #73` — `v0.9.0`)** ⚠️ **`[Chạm nhẹ Planner Context]`**
  - **Mô tả**: Cho phép xóa cả các intent đã kết thúc (`done`, `blocked`, `exhausted`, `stopped`) với lý do bắt buộc:
    - **Retract (Soft delete)**: giữ lại lịch sử nhưng đánh dấu `[withdrawn]` xuyên suốt các vòng planner, `graph_overview`, `list_facts`, `list_findings`, `node_detail`, `get_worker_output`...
    - **Permanent delete (Hard delete)**: xóa intent cùng các node chỉ do nó chứng minh, có khóa row chống race condition với worker đang chạy, compaction và rerun.
  - **File chính**: `db/exploration.go`, `agent/tools.go`, `agent/planner.go`, `server/orchestration.go`, `web/src/app/(main)/function/tasks/detail/_tabs/sessions-tab.tsx`.

- [x] **3.3. Hệ thống Xác minh Lỗ hổng tự động (`Verified Findings — S1`, `BaaRai #162, #163, #164, #169, #193` — `v0.6.0`)** ⚠️ **`[Chạm Prompt ở #193]`**
  - **Mô tả**:
    - Thêm `poe_level` (0–4), `verification_status` (`verified`, `unverified`, `blocked`, `not_assessed`, `false_positive`), `verification_note`.
    - Tự động đối chiếu trích dẫn bằng chứng (≥ 8 ký tự) nguyên văn trong HTTP response body đã gắn (hỗ trợ giải nén `gzip`/`deflate` tối đa 4 MiB).
    - Tự động nâng lên `verified` khi retest trả kết quả `reproduced` hoặc `fixed`.
    - Thêm trường tùy chọn `counterevidence` và `confidence_rationale` vào `report_finding` và trang chi tiết lỗ hổng.
    - Hiển thị badge xác minh, bộ lọc và ghi chú trên UI Findings.
  - **File chính**: `db/findings.go`, `db/schema.sql`, `agent/finding_recorder.go`, `agent/finding_workflow.go`, `agent/retester.go`, `server/findings.go`, `web/src/app/(main)/function/findings/`.

- [x] **3.4. Intent Claim Lease & Fencing (`BaaRai S3c #176` — `7ccae15`)**
  - **Mô tả**: Thêm cơ chế thuê bao intent có heartbeat 15s, TTL 90s và `owner_token` để ngăn worker bị treo ghi đè kết quả sau khi intent đã được cấp lại cho worker khác.
  - **File chính**: `db/exploration.go`, `db/schema.sql`, `server/engine.go`.

---

## Giai đoạn 4: Trải nghiệm Chat Nâng cao, Dán Tài liệu & Đa dạng LLM Provider

- [x] **4.1. Điều khiển Chat nâng cao, Dán tài liệu & Lưu Finding độc lập (`cskwork #29, #30, #32, #80` — `v0.6.0`)**
  - **Mô tả**:
    - Hàng đợi tin nhắn (`/queue`: thêm, sửa, đổi thứ tự, tạm dừng/tiếp tục, hủy), điều hướng khi tool đang chạy (`/steer`), thử lại (`/retry`), sửa câu hỏi cũ & rẽ nhánh hội thoại (`/branch`).
    - Xem và nén context thủ công (`/context`, `/usage`, `/compress`) giữ lại các lượt trao đổi gần nhất; lọc ký tự `\u0000` NUL khi lưu snapshot (`#80`).
    - Dán file từ clipboard và tự động chuyển văn bản > 3.000 ký tự thành file đính kèm (Document Paste).
    - Cho phép hội thoại Pentest độc lập lưu lỗ hổng trực tiếp vào trang Findings (nhóm chưa gán task) và đọc lại qua các lượt (`#30`).
  - **File chính**: `chatstate/`, `db/chat_*.go`, `db/schema.sql`, `agent/chat_*.go`, `server/chat_*.go`, `web/src/app/(main)/chat/`.

- [x] **4.2. Hỗ trợ Ollama Native, Chế độ Zero-Egress `local_llm_only` & Cloud Economy Templates (`cskwork #22, #25, #27` + `BaaRai #128`)**
  - **Mô tả**:
    - Provider `ollama` native qua `/api/chat` (streaming, tool calls, thinking, `num_ctx`, `keep_alive`, tự liệt kê model đã cài, hiển thị % GPU memory, ưu tiên slot cho approval judge / main agent / side question, tự động nén sớm cho context ≤ 64K) kèm hỗ trợ `ollama` OpenAI-compatible (`BaaRai #128`).
    - Chính sách tùy chọn `local_llm_only` (`llmnet/`): khóa cứng kết nối LLM chỉ cho phép loopback và các CIDR tin cậy (`ARTEX_LOCAL_LLM_ALLOWED_CIDRS`), chặn redirect, proxy, web search và auto-update.
    - Thêm bộ preset Cloud Economy (DeepSeek, Claude Haiku, OpenAI Responses, Qwen, Kimi, OpenRouter, Z.ai GLM-5.3) và hạch toán chi tiết token cache read/write + reasoning (`#27`).
  - **File chính**: `ollama/`, `llmnet/`, `agent/provider.go`, `llmpool/`, `server/llm_*.go`, `web/src/app/(main)/system/llm/page.tsx`.

- [x] **4.3. Sổ cái Chi phí EUR (`Cost Ledger`) & Xoay vòng LLM theo Intent (`BaaRai #71, #74, #111, #114`)**
  - **Mô tả**: Cấu hình đơn giá token theo từng LLM profile để tính chi phí thực tế (`cost_eur`) cho mỗi task/lần gọi; hỗ trợ xoay vòng profile theo từng intent (`llm_rotate`) và tự động leo thang sang profile kế tiếp khi lỗi liên tiếp; thêm thẻ tổng kết Task Outcome Card (`#102`).
  - **File chính**: `db/llm_usage.go`, `llmpool/`, `server/cost_ledger_test.go`, `server/engine.go`, `web/src/app/(main)/function/tasks/detail/`.

---

## Giai đoạn 5: Công cụ Recon, Nạp Kết quả Quét, Ngân sách & Kiểm soát Agentic Nâng cao

- [ ] **5.1. Công cụ nạp kết quả quét có cấu trúc: `ingest_scan_output` & `ingest_bbot` (`BaaRai #199, #200, #214, #215, #222`)**
  - **Mô tả**: Thêm 2 công cụ cho worker:
    - `ingest_scan_output`: đọc trực tiếp file kết quả của `nmap` (XML), `nuclei`, `httpx`, `ffuf` (JSONL) và nạp thành assets/facts vào đồ thị mà không làm tràn context LLM.
    - `ingest_bbot`: đọc file JSONL của `bbot`, tự động tách sự kiện trong scope (`scope_distance == 0`) để chèn asset và sự kiện ngoài scope thành fact tình báo; tự lọc bỏ thông tin nhạy cảm `userinfo` trong URL (`#222`).
  - **File chính**: `agent/scan_ingest.go`, `agent/scan_ingest_tool.go`, `agent/bbot_ingest.go`, `agent/bbot_ingest_tool.go`, `agent/tools_insert.go`.

- [ ] **5.2. Sổ cái Độ phủ (`Coverage Ledger` — `record_coverage`) & Ngân sách chạy phân cấp (`Hierarchical Budgets`) (`BaaRai #206, #207`)**
  - **Mô tả**:
    - Công cụ `record_coverage` và bảng `coverage_ledger` ghi lại các bề mặt & nhóm rủi ro đã kiểm tra (`reported`, `no_issue_found`, `ruled_out`, `not_applicable`, `needs_follow_up`).
    - Ngân sách phân cấp `task -> intent -> planner round` (`agent/budget.go`, tắt mặc định) tự động dừng mở rộng và chuyển sang vòng `wrap-up` khi cạn ngân sách.
  - **File chính**: `db/coverage_ledger.go`, `agent/coverage_ledger.go`, `agent/budget.go`, `server/engine_budget.go`.

- [ ] **5.3. Bộ điều tiết lưu lượng `Egress Governor`, `Noise Profiles` & `Lexical Tool RAG` (`BaaRai #204, #205, #212`)** ⚠️ **`[Chạm Prompt Tail ở #212]`**
  - **Mô tả**:
    - Package `egress/` điều tiết tốc độ gửi request theo từng host (token bucket, concurrency cap, jitter, backoff `429`/`503`, gom các request `WebFetch GET` trùng lặp) theo cấu hình `ARTEX_NOISE_PROFILE`.
    - `ARTEX_TOOL_RAG` (`agent/toolrag.go`): lọc từ vựng danh sách công cụ gắn cho agent, chỉ giữ lại các công cụ cốt lõi và top-K công cụ phù hợp nhất với nhiệm vụ để tiết kiệm token.
  - **File chính**: `egress/egress.go`, `agent/webfetch.go`, `agent/noise_profile.go`, `agent/toolrag.go`.

- [ ] **5.4. Target Gate (`S3b`) & Chống Prompt Injection / Agentic Guards (`BaaRai #166, #177–#186, #189, #190, #194–#198, #203`)** ⚠️ **`[Chạm Prompt]`**
  - **Mô tả**:
    - Bọc đầu ra của công cụ bằng ranh giới dữ liệu không tin cậy (`ARTEX_UNTRUSTED_BOUNDARY`) và đóng khung các trường do agent sinh ra bằng nonce envelope (`agent/data_envelope.go`).
    - Gắn nguồn gốc dữ kiện `record_fact` (`origin` do server xác định), buộc `prove_goal` phải dùng fact có độ tin cậy `observed`, chặn `goal_met` khi còn goal mở, áp dụng rule `[built-in]` deny cho cả `planner` và `main agent`.
    - Hệ thống `target_pins` & pinned dialer kiểm soát đích kết nối thực tế và từng bước chuyển hướng HTTP (`WebFetch`, custom `http`, `enrich`).
  - **File chính**: `agent/untrusted_boundary.go`, `agent/untrusted_prompt.go`, `agent/data_envelope.go`, `intercept/target_*.go`, `db/target_pins.go`, `server/target_gate_*.go`.

---

## Giai đoạn 6: Mở rộng Hệ sinh thái, Docker Recon Toolbox, Skills & Adapters

- [ ] **6.1. Bổ sung bộ Skills mới & Đóng gói Recon Toolbox vào Docker (`BaaRai #55, #77, #112, #113, #118, #188`)**
  - **Mô tả**:
    - Port các skill mới vào `skills/`: `bbot-recon`, `php2shell`, `codeaudit`, `api-security`, `edr-bypass-re`, `patch-diff`, `rea`.
    - Cập nhật `Dockerfile` cài đặt sẵn và kiểm tra SHA256 checksum cho bộ công cụ recon (`nuclei`, `httpx`, `subfinder`, `katana`, `ffuf`, `dalfox`, `sqlmap` cố định commit) và nâng `Norma` lên `v0.4.8` (watchdog bộ nhớ tiến trình con, cắt chuỗi chuẩn UTF-8).
  - **File chính**: `skills/`, `Dockerfile`, `norma/`, `scripts/backup.sh`.

- [ ] **6.2. Adapter điều khiển ngoài cho Claude Code / Codex / Pi & Cầu nối `opencode-sdk` (`cskwork #1, #57, #60, #66`)**
  - **Mô tả**:
    - Thêm `adapters/agent/` cung cấp stdio MCP server cho Claude Code / Codex và extension cho Pi để đọc trạng thái task, coverage, findings và điều khiển task từ bên ngoài.
    - *(Tùy chọn)* Thêm `opencodesdk/` và `adapters/opencode-sdk/` sau khi đánh giá xử lý cảnh báo phụ thuộc `zod` (`cskwork #67`).
  - **File chính**: `adapters/agent/`, `opencodesdk/`, `adapters/opencode-sdk/`.

---

## Thứ tự thực hiện đề xuất

1. **Bắt đầu từ Giai đoạn 1** (1.1 → 1.6): Mỗi mục hoàn toàn độc lập, có thể port, kiểm thử và xác nhận ngay trên Docker + trình duyệt trong từng bước ngắn.
2. **Tiếp theo là Giai đoạn 2 & 3**: Nâng cấp bảo mật lưu trữ (mã hóa secrets, rate limit login, setup token) và quản lý kết quả (Per-task Credentials, Xóa kết quả Worker, Verified Findings).
3. **Sau đó là Giai đoạn 4, 5, 6**: Chọn lọc các hệ thống lớn (Chat Controls, Ollama Native, Scan Ingestion, Egress Governor, Recon Toolbox & Skills) theo nhu cầu sử dụng thực tế.
