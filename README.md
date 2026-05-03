# Switchboard

Slack-side router that connects [jcode](https://github.com/1jehuang/jcode) coding-agent sessions to Slack threads, and unifies inbound engineering signals (GitHub, Temporal, cron, monitoring) into the same channels.

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
Slack (Socket Mode) ──> Edge ──> Router ──> jcode Client ──> jcode daemon
                                   ^
Webhooks (HTTP) ──> Ingest ────────┘
                                   │
                                   v
                              SQLite (state)
                                   │
                                   v
                        Coalescer ──> Outbound Queue ──> Slack API
```

One Slack thread = one jcode session. Replies in a thread continue the session.

## Key Features

- **Bidirectional Slack <-> jcode** with thread-per-session granularity
- **Durable webhook ingest** with HMAC verification and at-least-once delivery
- **Per-channel rate limiting** with round-robin fairness
- **Lazy message coalescing** (at most 1 update/sec per session)
- **Multiple bot identities** via `chat:write.customize`
- **Auto-spawn** jcode daemon if not running
- **Bridge restart recovery** from SQLite state

## Configuration

See `docs/config.example.toml` for the full configuration reference. Key sections:

- `[slack]` - Slack app/bot tokens
- `[[channels]]` - Channel ID to workdir mapping
- `[ingest]` - Webhook server settings
- `[[routes]]` - Deterministic event routing rules
- `[identities.*]` - Bot display personas

## Commands

In any active agent thread:
- `!stop` / `!cancel` - Cancel the current turn
- `!purge` - Clear queued messages

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

## Gotchas

- **bufio.Reader not Scanner**: jcode can emit lines up to 32 MB. We use `ReadBytes('\n')` with a 32 MB buffer, NOT `bufio.Scanner` (which has a 64 KB default limit).
- **One connection per session**: The jcode protocol doesn't tag events with session_id; each session needs its own Unix socket connection.
- **chat.update is workspace-scoped**: 50/min across all channels. The outbound queue uses per-channel sub-queues with round-robin to ensure fairness.

## License

MIT
