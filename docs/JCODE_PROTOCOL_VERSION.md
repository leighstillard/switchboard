# jcode Protocol Version

**Pinned at:** jcode v0.11.x (commit `f071bc2a`)

This file records the version of the jcode Unix-socket NDJSON protocol that
Switchboard is built against. The protocol is unstable and undocumented upstream;
we pin to a specific commit and maintain hand-rolled Go types in
`internal/jcodeproto/types.go`.

## Wire Format

- NDJSON (newline-delimited JSON) over Unix socket
- Both requests and events use `"type"` as the discriminant tag
- All field names are `snake_case`
- Maximum line size: 32 MB (History events with full conversation context)

## Events Handled (v1)

| Event | Wire type | Action |
|-------|-----------|--------|
| `ack` | `{"type":"ack","id":N}` | Match to outstanding request |
| `done` | `{"type":"done","id":N}` | Trigger final flush |
| `error` | `{"type":"error","id":N,"message":"..."}` | Surface in thread |
| `pong` | `{"type":"pong","id":N}` | Heartbeat ack |
| `session` | `{"type":"session","session_id":"..."}` | Record session ID |
| `text_delta` | `{"type":"text_delta","text":"..."}` | Append to buffer |
| `text_replace` | `{"type":"text_replace","text":"..."}` | Replace buffer |
| `message_end` | `{"type":"message_end"}` | Finalize message |
| `interrupted` | `{"type":"interrupted"}` | Show interrupted notice |
| `tool_start` | `{"type":"tool_start","id":"...","name":"..."}` | Track pending tool |
| `tool_exec` | `{"type":"tool_exec","id":"...","name":"..."}` | Mark executing |
| `tool_done` | `{"type":"tool_done","id":"...","name":"...","output":"..."}` | Complete tool |
| `generated_image` | `{"type":"generated_image","path":"..."}` | Upload to thread |
| `notification` | `{"type":"notification",...}` | Render in thread |
| `upstream_provider` | `{"type":"upstream_provider","provider":"..."}` | Footer note |
| `reloading` | `{"type":"reloading"}` | Trigger reconnect |
| `history` | `{"type":"history","was_interrupted":...}` | Extract flag, discard |

## Requests Sent (v1)

| Request | Wire type |
|---------|-----------|
| Subscribe (new) | `{"type":"subscribe","id":N,"working_dir":"...","client_has_local_history":false}` |
| Subscribe (resume) | `{"type":"subscribe","id":N,"target_session_id":"...","client_has_local_history":true}` |
| Message | `{"type":"message","id":N,"content":"...","images":[]}` |
| Cancel | `{"type":"cancel","id":N}` |
| Ping | `{"type":"ping","id":N}` |

## Drift Detection

CI should run an integration test against a real `jcode serve` to detect
protocol drift. If the test fails, update the types and bump this version.

## Known Gotchas

1. **Use `bufio.Reader.ReadBytes('\n')` not `bufio.Scanner`**: Scanner has a
   64KB default line cap that silently truncates large History events.
2. **One socket per session**: Events are not tagged with session_id on the wire.
   Each Subscribe creates a session-scoped stream on that connection.
3. **History events can be huge**: 50MB+ for long sessions. We drop messages
   and only extract `was_interrupted`.
