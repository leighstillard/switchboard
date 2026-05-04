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

## §1-§13 Test Results

_To be filled during test execution._
