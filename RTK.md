# RTK - Release Task Kit

## Switchboard Release Checklist

### Pre-release

- [ ] All tests pass: `go test ./...`
- [ ] Linter clean: `golangci-lint run`
- [ ] Config schema matches spec (§5)
- [ ] Protocol types match target jcode version
- [ ] SQLite migrations are forward-compatible
- [ ] Documentation updated

### Build

```bash
go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/switchboard
```

### Deploy

- [ ] Stop existing switchboard process
- [ ] Backup SQLite database
- [ ] Deploy new binary
- [ ] Verify config: `switchboard --config /path/to/config.toml --dry-run`
- [ ] Start switchboard
- [ ] Verify health endpoint: `curl http://127.0.0.1:8765/health`
- [ ] Check logs for startup errors

### Post-release

- [ ] Monitor Slack connectivity
- [ ] Verify webhook ingestion
- [ ] Check audit log rotation
- [ ] Update CHANGELOG.md
