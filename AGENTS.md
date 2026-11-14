# Repository Agent Instructions

## Required validation workflow

- If work spans multiple independent areas, spawn multiple agents and assign non-overlapping files. Explicitly tell each agent which files are theirs; avoid agents editing the same file. The parent agent integrates shared changes, resolves conflicts, and validates the combined result.
- After finishing **every feature or code change**, run the relevant tests, verify the app in Docker, and immediately commit that feature/change before moving to the next item or reporting completion.
- For web UI or localization changes, rebuild and restart the Docker app with `docker compose up -d --build artex`, then verify the changed behavior in the running app using the browser at `http://localhost:8787` (or the configured Docker port). Do not treat `tsc`, lint, or a local build as a substitute for Docker/browser verification.
- For browser and E2E verification, execute the test suite in `tests/` (e.g. `./tests/run_tests.sh` or `NODE_PATH=web/node_modules node tests/verify_all_pages_browser.js`). Ensure all test scripts cleanly close contexts and explicitly call `process.exit(0)` upon completion.
- Also run relevant static checks:
  - Web UI: `cd web && npx tsc --noEmit && git diff --check`
  - Backend: `go build ./cmd/artex` and relevant unit tests (e.g., `go test ./server/...`).
- If Docker/browser verification cannot be completed, state the exact blocker and do not claim the task is fully verified.
- Preserve existing Docker data unless the user explicitly authorizes resetting it. Never run destructive volume/data reset commands without explicit approval.

## Localization requirements

### Frontend (Web UI)
- Supported locale identifiers are `zh-CN`, `en-US`, and `vi-VN`; English is the default.
- Continue accepting legacy locale cookies `en` and `vi`, normalize them to `en-US` and `vi-VN`, and persist the canonical identifiers.
- Translate Vietnamese from the Simplified Chinese source as faithfully as possible.
- Keep technical terms in English in Vietnamese; do not append Vietnamese glosses to technical terms. Translate ordinary prose and general vocabulary normally.
- The product term “Skills” and all skill names/content should remain in English in every locale; do not translate “Skills” as a generic Vietnamese word.
- Keep catalog key parity across all supported languages (`zh-CN`, `en-US`, `vi-VN`).

### Backend (Go Services)
- The backend operates directly and strictly in English (API error messages, logs, trigger headers, validation errors, and rule mode command responses). Do not add multilingual catalog layers to the Go backend.
- **LLM System Prompt Exemption**: Do NOT translate or modify LLM system prompts in `agent/promptcatalog.go`, `agent/planner.go`, `agent/worker.go`, `agent/goals.go`, `agent/compaction.go`, `agent/wrapup.go`, `agent/retester.go`, `sidequestion/request.go`, `sidequestion/context.go`, and `intercept/prompt.go` until explicitly instructed by the user ("riêng phần prompt giữ nguyên để sau").
- Preserve all existing code comments, docstrings, and license headers.

## Test directory and local tooling
- Local browser E2E test scripts are located in `tests/` (`verify_all_pages_browser.js`, `test_browser_smoke.js`, `check_chinese_dom.js`, `run_tests.sh`).
- Keep `/tests/` in `.gitignore` to prevent committing local test credentials, run logs, or ad-hoc scripts.

## Git and communication
- Test and commit each feature or fix immediately upon completion (one clean, verified commit per feature/task; do not batch multiple features into a single commit or leave completed features uncommitted).
- Commit messages must be written in English and include a clear subject plus a concise body describing the change and relevant behavior/validation; do not use a subject-only commit.
- Target remote is `konanguyen` on branch `main`.
- Do not push (`git push`) unless the user explicitly requests it.
- Report validation truthfully, distinguishing completed checks from blocked or skipped checks.
