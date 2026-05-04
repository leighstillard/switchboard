# Switchboard v1 Test Results

**Date:** 2026-05-04
**Tester:** Leigh
**Switchboard version:** TBD (run `switchboard -version`)

---

## §0. Prerequisites

### 0.1 Hosts and processes
- [x] clawasaki running, ~/workspace/ has 51+ repos
- [x] `jcode serve` works (v0.11.9)
- [x] `switchboard -validate-config` exits 0
- [x] `switchboard -version` prints version info

### 0.2 Slack workspace
- [x] Test channels created: `#sw-test-alpha`, `#sw-test-beta`, `#sw-test-noise`, `#sw-test-fallback`
- [x] Channel IDs recorded below
- [x] Bot invited to all channels
- [ ] DM conversation with bot exists
- [ ] Second user available

| Channel | ID |
|---------|-----|
| #sw-test-alpha | C0B17DC35QT |
| #sw-test-beta | C0B283F8VR6 |
| #sw-test-noise | C0B17DDAQ67 |
| #sw-test-fallback | C0B283J5SHW |

### 0.3 Workdirs
- [x] ~/workspace/sw-test-alpha/ - small Go repo with README.md, main.go, util.go
- [x] ~/workspace/sw-test-beta/ - empty git init
- [x] ~/workspace/sw-test-noise/ - Go repo with pkg/server, pkg/client, pkg/util

### 0.4 Cloudflare Tunnel + webhook sources
- [ ] Tunnel health check passes
- [ ] GitHub webhook configured
- [ ] Temporal test workflow available
- [x] Cron test script: `test/scripts/send-cron-event.sh`

### 0.5 Observability
- [ ] journalctl watching
- [ ] sqlite3 open
- [x] test-results.md created (this file)

---

## §1. Bootstrap and connection

### 1.1 Cold start with no jcode running
- **DEFERRED** - Cannot safely kill jcode with active user sessions (crocodile, snake). Need isolated test window.

### 1.2 Cold start with jcode already running
- [x] **PASS** - Stopped switchboard, started it with jcode already running (PID 324998).
- Logs show `jcode: subscribed to existing session`, `jcode: reusing existing connection for session`.
- jcode PID unchanged after switchboard start.
- Service active within 3 seconds.

### 1.3 Daemon disappears mid-run
- **DEFERRED** - Same reason as 1.1. Code review confirms: exponential backoff (1s, 2s, 4s), re-spawn logic exists in `ensureDaemon()`.

### 1.4 Config validation rejects bad input
- [x] **PASS** - Missing `bot_token`: exits 1 with `config: slack.bot_token is required`
- [x] **PASS** - Bad channel ID format: exits 1 with `config: channel "bad" has invalid Slack ID "not-a-slack-id"`
- [x] **PASS** - Duplicate channel IDs: exits 1 with `config: duplicate channel id "C0123ABCDEF"`
- [x] **PASS** - Short HMAC secret: exits 1 with `config: ingest source "github" secret is too short (5 chars, minimum 16)`
- **NOTE**: Route destination referencing unknown channel - not yet validated (soft warning). Logged as deviation.

### 1.5 SIGHUP hot-reload
- [x] **PARTIAL** - SIGHUP triggers reload, logs show `config reloaded successfully`.
- **DEVIATION**: Only routes are reloaded via `rt.Reload(newCfg.Routes)`. New channels added to config are NOT picked up without full restart. Need to extend reload to include channels, GitHub config, and ingest fallback.

---

## §2. Unit tests

### 2.1 All packages pass
- [x] **PASS** - `go test ./...` all green (coalesce, config, ingest, jcodeproto, outbound, router, store).

### 2.2 Race detector
- [x] **PASS** - `go test -race ./...` all green, no races detected.

---

## §3. Interactive Slack message flow

### 3.1 Full round trip
- [x] **PASS** - Messages sent in `#sw-test-alpha`, `#sw-test-beta`, `#sw-test-noise`. Sessions created (crow, panda, ox), jcode responded, replies delivered back to Slack threads. Full Slack -> switchboard -> jcode -> switchboard -> Slack round trip confirmed.

---

## §4. GitHub webhook routing

### 4.1 Known repo routes to correct channel
- [x] **PASS** - `format5/switchboard` routed to `C0B0Y0WQYQP` (#switchboard-test), `routed_by=github_config`.

### 4.2 Second known repo
- [x] **PASS** - `format5/partseeker-infra` routed to `C0AR5HNQQ68` (#partseeker-infra), `routed_by=github_config`.

### 4.3 Unknown repo falls back
- [x] **PASS** - `unknown/mystery` routed to `C0AL12WCNBG` (#data-worklog fallback), `routed_by=github_config`.

---

## §5. GitHub event formatting

### 5.1 Push event
- [x] **PASS** - Push to `format5/switchboard` accepted (202), formatted with commit list.

### 5.2 Issue comment
- [x] **PASS** - `issue_comment` (created) accepted (202).

### 5.3 PR review
- [x] **PASS** - `pull_request_review` (submitted) accepted (202).

### 5.4 Force push
- [x] **PASS** - Force push accepted (202), formatted with warning emoji.

### 5.5 Unknown event type (generic fallback)
- [x] **PASS** - `deployment_status` accepted (202), falls back to generic formatter.

---

## §6. Session management

### 6.1 Active sessions
- [x] **PASS** - 29 active sessions tracked. 1 processing (current scorpion session on #switchboard-test), rest idle.

### 6.2 Session status tracking
- [x] **PASS** - Status correctly reflects `idle` vs `processing`. Channels: data-worklog (25 idle), switchboard-test (3 idle + 1 processing).

### 6.3 Turn queue
- [x] **PASS** - Turn queue depth = 1 (normal for active session).

### 6.4 All webhooks processed
- [x] **PASS** - 0 pending webhooks. All 500+ test webhooks processed to `done`.

---

## §7. Audit log integrity

### 7.1 Required fields populated
- [x] **PASS** - 0 audit entries with null `source` or `event_type`.

### 7.2 Coverage
- [x] **PASS** - 578 GitHub audit entries, 7 Slack audit entries covering all test events.

### 7.3 Payload hash
- [x] **PASS** - 0 audit entries missing `payload_hash`.

---

## §8a. Cron webhook source

### 8a.1 Cron webhook accepted and processed
- [x] **PASS** - Cron webhook with idempotency key accepted (202), status=done after processing.

---

## §8. Webhook ingest - durability and dedup

### 8.2 Persistence before ack
- [x] **PASS** - POST returns 202, row immediately visible in `webhook_inbox` with status `pending`.

### 8.3 Idempotency dedup
- [x] **PASS** - 3 identical requests with same delivery ID produce exactly 1 row.

### 8.5 HMAC failure
- [x] **PASS** - Bad HMAC signature returns 401.

### 8.7 Body too large
- [x] **PASS** - 2MB+ body returns 413.

### 8.8 Missing idempotency key
- [x] **PASS** - Missing `X-Switchboard-Idempotency-Key` on cron source returns 400.

---

## §9. Notification routing and correlations

### 9.6 No matching route -> fallback channel
- [x] **PASS** - Webhook for `unknown/unrouted-repo` accepted (202), routes to fallback.

---

## §10. Bridge restart guarantees

### 10.5 Pending webhooks survive restart
- **DEFERRED** - Cannot restart switchboard while it's carrying the active Slack test session (crocodile). Restarting kills the bridge and interrupts the session. Need isolated test window.

---

## §11. Audit, logging, and data handling

### 11.4 Secrets scrubbing on inbox
- [x] **PASS** - No sensitive headers (xoxb-, Bearer, authorization) found in `webhook_inbox`. HMAC signature stored as `[REDACTED]`.

### 11.5 slog never logs secrets
- [x] **PASS** - 0 secret leaks found in journalctl logs from last hour.

---

## §13. Adversarial / robustness

### 13.6 Webhook flood
- [x] **PASS** - 100 webhooks sent in rapid succession (10 concurrent batches). All persisted. Bridge remained healthy (200 health check, service active).

### 13.8 Health check after all tests
- [x] **PASS** - `GET /health` returns 200.
