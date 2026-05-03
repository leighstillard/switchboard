# Switchboard — Reader's Toolkit

Start here if you're onboarding to the Switchboard codebase.

## Key Documents

- **[`docs/design.md`](docs/design.md)** — Full v1 design document. Start here for architecture, goals, and trade-offs.
- **[`docs/JCODE_PROTOCOL_VERSION.md`](docs/JCODE_PROTOCOL_VERSION.md)** — Pinned jcode protocol version and wire format reference.
- **[`docs/config.example.toml`](docs/config.example.toml)** — Annotated configuration example.
- **[`docs/slack-app-manifest.json`](docs/slack-app-manifest.json)** — Slack app manifest for workspace setup.
- **[`docs/adr/`](docs/adr/)** — Architecture Decision Records (created as decisions are made).

## Code Layout

```
cmd/switchboard/main.go     — Entry point, signal handling, component wiring
internal/
  config/                   — TOML config loader with ${VAR} substitution
  jcodeproto/               — Hand-rolled jcode protocol types (v1 subset)
  jcode/                    — Socket client, session management, auto-spawn
  slack/                    — Socket Mode events, outbound API calls
  coalesce/                 — Per-session lazy message buffer
  outbound/                 — Rate-limited send queue with per-channel fairness
  ingest/                   — Webhook HTTP server with HMAC + durable inbox
  router/                   — Rule engine, session lifecycle, event dispatch
  store/                    — SQLite layer (sessions, turns, correlations, audit)
```

## Testing

```bash
go test ./...              # All tests
go test ./internal/store/  # Just the store
go test -race ./...        # With race detector
```

## Running

```bash
go build ./cmd/switchboard
./switchboard --config ~/.config/switchboard/config.toml
```

Debug mode: `SWITCHBOARD_LOG_LEVEL=debug` or `--debug` flag.
Hot-reload: send SIGHUP to reload routing rules without restart.
