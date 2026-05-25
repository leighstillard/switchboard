# Claude Code Backend — Design

**Date:** 2026-05-25
**Status:** Approved (design phase)
**Branch:** `feat/claude-code-backend`

## Problem

Switchboard is hard-wired to the **jcode** agent. The router holds a concrete
`*jcode.Client`, and both `router.consumeEvents` and `coalesce.HandleEvent`
switch directly on `jcodeproto.Event*` (13 event types). To support a second
agent backend — Claude Code — we need a backend-agnostic seam so the rest of
the system speaks one normalized vocabulary regardless of which agent produced
the events.

## Goals

- Add **Claude Code** as a selectable agent backend, at **full parity** with
  jcode for every event Claude Code can express (graceful degradation where it
  cannot).
- Keep jcode the default; introduce Claude behind config, staged rollout — same
  shape as the existing `per_thread_workdir` global-default + per-channel
  pattern.
- No behavior change for existing jcode deployments after the refactor.

## Non-Goals

- Replacing jcode. Both backends coexist.
- A Go SDK for Claude Code (none exists). We drive the `claude` CLI.
- Mid-turn in-band cancellation for Claude (the CLI has none; we SIGINT +
  resume).
- Rebuilding the agent loop against the raw Anthropic API.

## Mechanism: `claude` CLI subprocess

Claude Code has no Go SDK. The integration spawns the `claude` CLI in streaming
headless mode — one **persistent subprocess per session**, mirroring how jcode
uses one Unix socket per session.

**Invocation** (process `cwd` set to the session workdir — there is no `--cwd`
flag):

```
claude -p '' \
  --input-format stream-json \
  --output-format stream-json \
  --include-partial-messages \
  --permission-mode bypassPermissions \
  --model <model> \
  --session-id <uuid>
```

- The process **stays alive** reading newline-delimited user messages on stdin;
  it does not exit after a turn.
- **Send a turn:** write `{"type":"user","content":[{"type":"text","text":"…"}]}\n`
  to stdin. Images use an `image` content block (base64 source).
- **Resume:** respawn with `--resume <session_id>`. Force an id at creation with
  `--session-id <uuid>`.
- **Permissions:** `--permission-mode bypassPermissions` — the agent acts fully
  autonomously in the session workdir, matching jcode's current posture. This is
  a deliberate trust decision: there is no human to approve tool use mid-turn in
  a Slack bot.
- **Cancel:** no in-band cancel exists. SIGINT the process, emit `Interrupted`,
  respawn with `--resume` on the next message.

## Architecture

```
router ──> agent.Backend (interface)
              ├── jcode adapter   (wraps existing *jcode.Client; jcodeproto → agent.Event)
              └── claude backend  (spawns `claude`; stream-json → agent.Event)
coalesce.HandleEvent(agent.Event)   ← single normalized vocabulary
```

New package `internal/agent` owns the normalized `Event` type and the `Backend`
interface. `coalesce` and `router` consume only `agent.Event`. jcode and Claude
are both adapters that translate their native event streams into `agent.Event`.

## Normalized event vocabulary (`internal/agent/event.go`)

| `agent.Event`                          | jcode source            | claude source                              |
|----------------------------------------|-------------------------|--------------------------------------------|
| `SessionReady{ID, Model}`              | `swarm_status`/`session`| `system/init`                              |
| `TextDelta{Text}`                      | `text_delta`            | `stream_event` → `content_block_delta`/`text_delta` |
| `TextReplace{Text}`                    | `text_replace`          | *(never emitted)*                          |
| `ToolStart{ID, Name, Input}`           | `tool_start`            | `stream_event` → `content_block_start` (tool_use) |
| `ToolInputDelta{ID, PartialJSON}`      | `tool_input`            | `stream_event` → `content_block_delta`/`input_json_delta` |
| `ToolExec{ID}`                         | `tool_exec`             | synthesized at tool_use `content_block_stop` |
| `ToolDone{ID, IsError}`                | `tool_done`             | `user` → `tool_result` (`is_error`)        |
| `MessageEnd`                           | `message_end`           | `stream_event` → `message_stop`            |
| `TurnDone`                             | `done`                  | `result` (subtype `success`)               |
| `TurnError{Message}`                   | `error`                 | `result` (subtype `error_*`) / terminal `system/api_retry` |
| `Interrupted`                          | `interrupted`           | synthesized on cancel/kill                 |
| `ImageGenerated{Path, Caption}`        | `generated_image`       | *(n/a — degrade)*                          |
| `Notification{Kind, From, Message}`    | `notification`          | *(n/a — degrade)*                          |
| `Provider{Name}`                       | `upstream_provider`     | `system/init.model`                        |

"Full parity" means every event a backend **can** express is mapped; the rest
degrade silently. `coalesce` already tolerates the absence of images and
notifications, so a Claude session that never emits them renders correctly.

### Notes on the claude translation

- **ToolStart / ToolInputDelta / ToolExec** are reconstructed from the partial
  stream: `content_block_start` for a `tool_use` block → `ToolStart` (id, name,
  initial input); successive `input_json_delta` → `ToolInputDelta`; the block's
  `content_block_stop` → `ToolExec`. This matches jcode's
  start→input→exec→done lifecycle that the inline-tool-summary feature relies on.
- **ToolDone** comes from the subsequent `user`/`tool_result` event, matched by
  `tool_use_id`, carrying `is_error`.
- **TurnError** distinguishes a failed turn via `result.subtype != "success"`
  (e.g. `error_max_turns`, `error_context_length`); a terminal `api_retry` that
  exhausts retries also maps here.
- The full-message `assistant` and `user` events are used as authoritative
  fallbacks/reconciliation if a partial stream is incomplete.

## Backend interface (`internal/agent/backend.go`)

```go
type Backend interface {
    Subscribe(ctx context.Context, workdir string) (sessionID string, events <-chan Event, err error)
    SubscribeExisting(ctx context.Context, sessionID, workdir string) (<-chan Event, error)
    SendMessage(ctx context.Context, sessionID, content string, images []Image) error
    Cancel(ctx context.Context, sessionID string) error
    Close() error
}
```

These are exactly the methods the router calls today (`router.go` lines
~418/470/529/633) plus `Close`. `Image` is a small backend-neutral struct
(media type + bytes/path) translated per backend.

## Claude backend (`internal/agent/claude/`)

- **`process.go`** — one persistent subprocess per session, mirroring jcode's
  `sessionConn`: spawn the `claude` command with `cmd.Dir = workdir`; a reader
  goroutine scans stdout NDJSON; writes serialize user turns to stdin. The spawn
  is behind an **exec seam** (an interface returning stdin/stdout pipes) so tests
  feed canned stream-json without invoking a real binary.
- **`translate.go`** — pure function `line []byte → []agent.Event` (table-tested),
  holding the per-session partial-stream accumulator state for tool blocks.
- **Resume / crash recovery** — on unexpected subprocess exit, respawn with
  `--resume <session_id>` using the same exponential-backoff strategy jcode uses
  in `handleDisconnect`.
- **Cancel** — SIGINT the process, emit `Interrupted`, respawn with `--resume`
  on the next message.

## jcode adapter

The existing `internal/jcode` client keeps its socket logic. A thin adapter makes
it satisfy `agent.Backend`: its event channel is wrapped so `jcodeproto.ServerEvent`
is translated to `agent.Event` (`translateJcode(ev) []agent.Event`). This is a
pure refactor — existing jcode behavior and tests are preserved.

## Config

```toml
[routing.backend]
default = "jcode"            # global default: "jcode" | "claude"

[claude]                     # claude-backend settings
binary = "claude"
permission_mode = "bypassPermissions"
model = "claude-opus-4-7"
append_system_prompt = ""    # optional
extra_args = []              # optional escape hatch

[[channels]]
id = "C123"
backend = "claude"           # per-channel override (unset → inherit default)
model = "claude-sonnet-4-6"  # optional per-channel model override
```

- `RoutingConfig2` gains `Backend BackendRoutingConfig` (`Default string`).
- A new `ClaudeConfig` struct holds the `[claude]` section.
- `ChannelConfig` gains `Backend string` and `Model string`.
- Selection logic mirrors `usePerThreadWorkdir`: per-channel value overrides the
  global default; unset inherits.

## Startup validation

The `claude` binary is validated at **every** startup (`claude --version`),
regardless of config. If a channel (or the global default) selects the claude
backend and the binary is absent or non-functional, startup **fails fast** with
a clear error. If no channel uses claude, a missing binary is logged as a warning
and startup proceeds (jcode-only hosts are not blocked).

> Open question for spec review: confirm whether absence should be a hard boot
> failure even when no channel uses claude. Current design: warn-only unless
> claude is actually selected.

## Store

Add **`migrateV4`**:

```sql
ALTER TABLE sessions ADD COLUMN backend TEXT NOT NULL DEFAULT 'jcode';
```

(Following the v3 pattern, resilient to SQLite's non-transactional
`PRAGMA user_version`.) On recovery, the router reads `backend` to select the
correct `Backend` for `SubscribeExisting`. The `jcode_session` column keeps its
name — it now generically holds "the backend session id" — to avoid churn across
the codebase and indexes.

## Router wiring

- `New()` constructs the jcode adapter always, and the claude backend when
  configured (lazily).
- A `backendFor(channelID) agent.Backend` selector (same shape as the existing
  `channelConfig` helper) resolves jcode vs claude per channel and is used by
  `handleNewSession` and recovery.
- The `jcode *jcode.Client` field becomes `defaultBackend agent.Backend` plus a
  `claudeBackend agent.Backend` (nil when unconfigured); `consumeEvents` switches
  on `agent.Event`.

## Testing

Strict TDD via the existing `rtk proxy go test -json | tdd-guard-go` pipeline.

- **`internal/agent/.../translate_test.go`** for **both** adapters — table-driven:
  feed raw NDJSON lines, assert the normalized `agent.Event` sequence. This is
  where the translation correctness is earned.
- **`internal/agent/claude/process_test.go`** — fake exec seam streams a recorded
  `claude` session; assert lifecycle: spawn → `SessionReady` → turn events →
  respawn-on-crash → resume.
- **Router selector test** — channel/global backend resolution (mirrors the
  `usePerThreadWorkdir` table tests).
- **Store** — dedicated `migrateV4` test (the v3 lesson: migrations and shell-outs
  are where tests earn their keep).

## Delivery plan (two PRs)

**PR A — Backend abstraction (pure refactor, no behavior change):**
1. `internal/agent` package: `Event`, `Backend`, `Image`.
2. `translateJcode`; jcode adapter satisfies `agent.Backend`.
3. `coalesce.HandleEvent` and `router.consumeEvents` switch to `agent.Event`.
4. Existing jcode tests stay green; no config or schema change.

**PR B — Claude backend (additive, flag-gated):**
1. Config: `[routing.backend]`, `[claude]`, `ChannelConfig.Backend/Model`.
2. `migrateV4` (sessions.backend column).
3. `internal/agent/claude`: process management (exec seam) + stream-json translator.
4. Startup validation of the `claude` binary.
5. Router `backendFor` selector + recovery by stored backend.
6. Default remains jcode; claude opt-in per channel or globally.

## Decisions captured (from brainstorming)

- **Scope:** full parity.
- **Mechanism:** `claude` CLI subprocess (NDJSON over stdin/stdout).
- **Permissions:** `bypassPermissions` (trusted autonomous, matches jcode).
- **Structure:** abstraction-first (`internal/agent`), jcode becomes an adapter.
- **Streaming:** `--include-partial-messages` on (live text + incremental tool input).
- **Binary validation:** validate at every startup.
