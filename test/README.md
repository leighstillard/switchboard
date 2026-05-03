# Testing

## Unit Tests

Run the standard unit tests (no external dependencies):

```bash
go test ./...
```

## Integration Tests

Test the jcode client against a **live jcode daemon**. Requires the jcode daemon to be running.

```bash
go test -tags integration ./test/integration/ -v -timeout 120s
```

Tests:
- `Subscribe_NewSession` - Create new session, verify session_id returned
- `SubscribeExisting_Resume` - Resume an existing session by ID
- `SendMessage_ReceiveEvents` - Send message, verify text_delta/done events
- `Cancel` - Cancel in-flight generation, verify interrupted event
- `Keepalive_Connection` - Verify connection survives idle period

## E2E Tests

Test the full Slack -> Switchboard -> jcode -> Slack pipeline.

### Without user token (basic connectivity only):

```bash
go test -tags e2e ./test/e2e/ -v -timeout 60s
```

This runs `TestSlackAPIConnectivity` (posts and reads a message via bot token) and skips the full-flow tests.

### With user token (full flow):

```bash
SWITCHBOARD_USER_TOKEN=xoxp-... go test -tags e2e ./test/e2e/ -v -timeout 180s
```

The `SWITCHBOARD_USER_TOKEN` must be a Slack **user OAuth token** (`xoxp-` prefix) that has `chat:write` and `channels:history` scopes. This is required because the Switchboard edge correctly filters self-messages from the bot, so triggering the full flow requires posting as a real user.

Full-flow tests:
- `MentionTriggersResponse` - @mention bot -> bot replies in thread
- `ThreadContinuation` - Reply in bot's thread -> bot responds again
- `StopCommand` - `!stop` in thread -> cancels processing
- `MrkdwnFormatting` - Verify Slack mrkdwn format (no `**bold**`)

### Environment Variables

| Variable | Description | Default |
|---|---|---|
| `JCODE_SOCKET` | Path to jcode daemon socket | `/run/user/$UID/jcode.sock` |
| `SWITCHBOARD_BOT_TOKEN` | Slack bot token (for reading) | from config |
| `SWITCHBOARD_USER_TOKEN` | Slack user token (for posting as user) | none (tests skip) |
| `SWITCHBOARD_TEST_CHANNEL` | Slack channel ID for e2e | `C0AL12WCNBG` |
| `SWITCHBOARD_BOT_USER_ID` | Bot's Slack user ID | `U0B1FNCSWP6` |
