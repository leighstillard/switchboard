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

## §2-§13 Test Results

_To be filled during test execution._
