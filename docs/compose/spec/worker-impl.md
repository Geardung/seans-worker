---
feature: worker-impl
status: designed
updated: 2026-09-20
branch: feat/worker-impl
commits: (pending)
---

# Worker Implementation

## Report

(Empty — not yet delivered)

## [S1] Problem

Seans needs a worker component that pulls tasks from the backend API, downloads torrents via qBittorrent, uploads files to S3 via rclone, and reports progress. The worker must be resilient (graceful shutdown, lease management, retry logic) and testable (mock backend for development without real backend/S3).

## [S2] Design

### Architecture
- Go 1.23+ stdlib only (net/http, encoding/json, log/slog, os/signal, context)
- rclone as subprocess for S3 upload (generates INI config at startup)
- qBittorrent Web API v2 for torrent management
- Pull model: worker polls backend for tasks, sends heartbeats

### API Contract (v1)
Worker calls these backend endpoints:
- `POST /v1/workers/register` — register with shared secret, get worker_id + worker_token
- `POST /v1/workers/{worker_id}/heartbeat` — report all active tasks progress
- `POST /v1/workers/{worker_id}/claim` — claim next task
- `POST /v1/tasks/{task_id}/complete` — report task success with file list
- `POST /v1/tasks/{task_id}/fail` — report task failure (permanent or temporary)

Auth: `Authorization: Bearer {worker_token}` (register uses `WORKER_REGISTER_TOKEN`)

### Task State Machine
`preparing → downloading → uploading → done`

#### Preparing
1. Clean staging dir `/staging/{task_id}/`
2. Remove leftover torrents from qBittorrent (same category+savepath)
3. Add torrent (magnet or base64 .torrent file)
4. Wait for torrent metadata (5 min timeout for magnets)
5. Read file list, check total size vs max_bytes
6. Set file priorities (selected=1, unselected=0)

#### Downloading
1. Poll qBittorrent every 5s for progress
2. Stall detection: no progress for 30 min → fail(permanent)
3. Task timeout: 12 hours → fail(temporary)
4. Complete when progress >= 1.0 and not actively downloading

#### Uploading
1. Compute s3_key = dest_prefix + relative_path
2. rclone move with retry (3 attempts)
3. Clean staging dir
4. Remove torrent from qBittorrent
5. POST complete to backend

### Config
All from environment variables. See .env.example for full list.

### Mock Backend
stdlib HTTP server implementing all v1 endpoints. Serves one fake task from env vars. Prints heartbeats/complete/fail to stdout.

### Mock S3
When MOCK_S3=true, rclone uses local filesystem instead of S3 remote.

## [S3] Out of Scope
- Backend implementation (only mock provided)
- Multiple concurrent task support in v1 (MAX_CONCURRENT_TASKS defaults to 1)
- TLS/certificate management
- Web UI for worker monitoring

## Tasks
- [ ] T1: Go module + config — acceptance: `go build ./cmd/agent` compiles, config parses from env (covers: S2)
- [ ] T2: Backend HTTP client — acceptance: register/claim/heartbeat/complete/fail with auth, 401 re-register, retry logic (covers: S2)
- [ ] T3: qBittorrent client — acceptance: login, add torrent, poll progress, set priorities, delete torrent (covers: S2)
- [ ] T4: rclone uploader — acceptance: generate rclone.conf, execute rclone move with retry, mock S3 support (covers: S2)
- [ ] T5: Task state machine — acceptance: preparing→downloading→uploading→done with all error paths (covers: S2)
- [ ] T6: Engine (claim loop, heartbeat, graceful shutdown) — acceptance: claim loop, heartbeat goroutine, SIGTERM handling (covers: S2)
- [ ] T7: Agent entrypoint — acceptance: `make build` produces working binary, logs version (covers: S2)
- [ ] T8: Mock backend — acceptance: serves one task, prints heartbeats/complete/fail (covers: S2)
- [ ] T9: Docker (Dockerfile + compose) — acceptance: `docker compose up` starts qbit + agent (covers: S2)
- [ ] T10: Tests — acceptance: qbit/client_test, engine/task_test, backend/client_test all pass (covers: S2)
- [ ] T11: .env.example + Makefile + README — acceptance: all env vars documented, README has 3-command deploy guide (covers: S2)