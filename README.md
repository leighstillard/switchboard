# Switchboard

Bridges Slack channels to jcode agent sessions with webhook ingestion, message
coalescing, and intelligent routing.

## Quick Start

```bash
go build ./cmd/switchboard
./switchboard --config ~/.config/switchboard/config.toml
```

## Configuration

See [docs/design.md](docs/design.md) for full specification reference.

## Architecture

```
Slack <-> Edge <-> Router <-> Jcode Sessions
                     ^
                     |
              Ingest Server (webhooks)
```

## Development

```bash
go test ./...
go vet ./...
```
