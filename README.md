# Switchboard

Slack-side router that connects coding-agent sessions to Slack threads, and unifies inbound engineering signals (GitHub, Temporal, cron, monitoring) into the same channels.

Two agent backends are supported behind a common `agent.Backend` abstraction:

- **[jcode](https://github.com/1jehuang/jcode)** — daemon over a Unix socket (the original backend)
- **Claude Code** — the `claude` CLI, run as one long-running process per session

The backend is selectable globally and per-channel, so different Slack channels can drive different agents from the same bridge.

## Quick Start

```bash
# Build
go build ./cmd/switchboard

# Configure
cp docs/config.example.toml ~/.config/switchboard/config.toml
# Edit config with your Slack tokens, channel IDs, and workdir mappings

# Run
./switchboard --config ~/.config/switchboard/config.toml
```

## Architecture

```
Slack (Socket Mode) ──> Edge ──> Router ──> agent.Backend ──┬─> jcode daemon (Unix socket)
                                   ^                         └─> Claude Code (claude CLI)
Webhooks (HTTP) ──> Ingest ────────┤
                                   │  (unmatched events → LLM router)
                                   v
                              SQLite (state)
                                   │
                                   v
                        Coalescer ──> Outbound Queue ──> Slack API
```

One Slack thread = one agent session. Replies in a thread continue the session.
Both backends translate their native event streams into one normalized event
vocabulary (`internal/agent`), so the router and coalescer are backend-agnostic.

## Key Features

- **Bidirectional Slack <-> agent** with thread-per-session granularity
- **Dual backends** — jcode and Claude Code, selectable globally and per-channel
- **Per-channel model override** — pin a model per channel independent of backend
- **Image support both ways** — Slack image uploads are forwarded to the agent; agent-generated images post back to the thread
- **LLM notification router** — webhook events that match no deterministic rule are routed to a thread by a Claude model (budget-capped, confidence-gated)
- **Cron scheduler** — scheduled prompt dispatches that stream a response into a new thread (dedup state persisted in SQLite)
- **Programmatic dispatch** — `POST /api/correlate` (authenticated, fail-closed) maps external IDs to threads so routed webhooks land in the originating session
- **Durable webhook ingest** with HMAC verification and at-least-once delivery
- **Per-channel rate limiting** with round-robin fairness
- **Lazy message coalescing** (at most 1 update/sec per session)
- **Block Kit rendering** with terse tool descriptions
- **Multiple bot identities** via `chat:write.customize`
- **Auto-spawn** jcode daemon if not running
- **Bridge restart recovery** from SQLite state

### Claude Code backend

The Claude Code backend runs the `claude` CLI as one long-running process per
session (survives idle periods, respawns on demand, evicted after a configurable
idle timeout). It inherits the ambient environment — including subscription
OAuth / keychain credentials — and enforces a configurable permission policy
(`allow_all`, `deny_all`, or `accept_edits_only`). A minimum CLI version is
enforced on startup. See `[claude]` in the configuration reference.

## Configuration

See `docs/config.example.toml` for the full configuration reference. Key sections:

- `[slack]` - Slack app/bot tokens
- `[routing.backend]` - Default agent backend (`jcode` or `claude`)
- `[routing.llm]` - LLM notification router (model, budget, confidence threshold)
- `[jcode]` - jcode socket connection and auto-spawn
- `[claude]` - Claude Code CLI: binary, model, permission policy, idle eviction, min version
- `[[channels]]` - Channel ID to workdir mapping (optional per-channel `backend` and `model`)
- `[ingest]` - Webhook server and per-source HMAC secrets
- `[[routes]]` - Deterministic event routing rules
- `[[cron]]` - Scheduled prompt dispatches
- `[identities.*]` - Bot display personas

## Commands

In any active agent thread:
- `!stop` / `!cancel` - Cancel the current turn
- `!purge` - Clear queued messages
- `!/<cmd>` - Passthrough: sends `/<cmd>` to the agent (Slack eats a leading `/`)

## Documentation

- [`docs/design.md`](docs/design.md) - Full design document
- [`docs/JCODE_PROTOCOL_VERSION.md`](docs/JCODE_PROTOCOL_VERSION.md) - Pinned protocol version
- [`docs/slack-app-manifest.json`](docs/slack-app-manifest.json) - Slack app manifest
- [`docs/adr/`](docs/adr/) - Architecture Decision Records

## Development

```bash
# Run tests
go test ./...

# Build with race detector
go build -race ./cmd/switchboard

# Debug mode
SWITCHBOARD_LOG_LEVEL=debug ./switchboard --config config.toml
```

## ScheduleWakeup Countdown Timer

When an agent calls `ScheduleWakeup` (jcode's `schedule` tool), switchboard displays a live countdown timer in the Slack message:

```
⏱ Timer: 4m 30s remaining
```

The countdown updates every 30 seconds (every 10 seconds when under 30 seconds remain). On expiry it shows:

```
⏱ Timer elapsed — command running
```

The timer is extracted from the `ScheduleWakeup` tool call's `delaySeconds` parameter. It persists across message updates within the same thread but is **in-memory only** -- timers do not survive switchboard restarts.

### jcode ScheduleWakeup parameter mapping

The Anthropic SDK defines `ScheduleWakeup` with fields `delaySeconds`, `prompt`, and `reason`. jcode's internal `schedule` tool uses different field names (`task`, `wake_in_minutes`, `background_context`). jcode maps the tool name automatically but requires serde aliases to map the parameters. If you see `missing field 'task'` errors from `ScheduleWakeup`, ensure your jcode build includes the alias fix (`fix/schedule-anthropic-sdk-params` branch or later).

| Anthropic SDK field | jcode `schedule` field | Notes |
|---|---|---|
| `prompt` | `task` | Required task description |
| `delaySeconds` | `wake_in_minutes` | Seconds converted to minutes (rounded up, min 1) |
| `reason` | `background_context` | Optional context |

## Gotchas

- **bufio.Reader not Scanner**: jcode can emit lines up to 32 MB. We use `ReadBytes('\n')` with a 32 MB buffer, NOT `bufio.Scanner` (which has a 64 KB default limit).
- **One connection per session**: The jcode protocol doesn't tag events with session_id; each session needs its own Unix socket connection.
- **chat.update is workspace-scoped**: 50/min across all channels. The outbound queue uses per-channel sub-queues with round-robin to ensure fairness.

## License

MIT
