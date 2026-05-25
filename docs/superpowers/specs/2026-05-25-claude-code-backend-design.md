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
  autonomously in the session workdir. **Precondition (explicit trust decision):**
  the workdir is a normal checkout on the host, **not** a sandboxed/isolated
  environment. `bypassPermissions` grants the agent unrestricted filesystem and
  shell access within that host context. We accept this to match jcode's existing
  autonomous posture and because there is no human to approve tool use mid-turn in
  a Slack bot. Per-thread git worktrees (the separate `per_thread_workdir` feature)
  narrow the blast radius but do not sandbox it. If stricter isolation is wanted
  later, `dontAsk` + `--allowedTools` is the migration path.
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
| `ToolDone{ID, Name, IsError}`          | `tool_done`             | `user` → `tool_result` (`is_error`); Name threaded from the tool_use block |
| `MessageEnd`                           | `message_end`           | `stream_event` → `message_stop`            |
| `TurnDone`                             | `done`                  | `result` (subtype `success`)               |
| `TurnError{Message}`                   | `error`                 | `result` (subtype != `success`) / terminal `system/api_retry` |
| `Interrupted`                          | `interrupted`           | synthesized on cancel/kill/crash           |
| `ImageGenerated{Path, Caption}`        | `generated_image`       | *(n/a — degrade)*                          |
| `Notification{Kind, From, Message}`    | `notification`          | *(n/a — degrade)*                          |
| `Provider{Name}`                       | `upstream_provider`     | *(n/a — degrade; model carried on `SessionReady`)* |

"Full parity" means every event a backend **can** express is mapped; the rest
degrade silently. `coalesce` already tolerates the absence of images,
notifications, and provider events, so a Claude session that never emits them
renders correctly. (Provider is intentionally **not** mapped from `init.model`:
that would conflate "provider" with "model", and the model is already carried on
`SessionReady`.)

### Notes on the claude translation

- **ToolStart / ToolInputDelta / ToolExec** are reconstructed from the partial
  stream: `content_block_start` for a `tool_use` block → `ToolStart` (id, name,
  initial input); successive `input_json_delta` → `ToolInputDelta`; the block's
  `content_block_stop` → `ToolExec`. This matches jcode's
  start→input→exec→done lifecycle that the inline-tool-summary feature relies on.

- **LOAD-BEARING ORDERING INVARIANT** (`coalesce.go` `EventToolInput` handler):
  `coalesce` routes tool-input deltas to *"the most recently started non-exec
  tool"* — it does **not** use the tool ID for routing. Therefore the claude
  translator MUST NOT emit `ToolStart` for block N+1 before emitting `ToolExec`
  (= `content_block_stop`) for block N. Anthropic's SSE emits tool_use blocks
  strictly sequentially (start→delta→stop per block index, one at a time), so this
  invariant holds naturally — but it is load-bearing and must be **explicitly
  tested** in the claude translator (a test that asserts no overlapping open
  tool blocks). See "ToolInputDelta routing" under the jcode adapter for why the
  normalized event carries an ID even though jcode's coalesce path ignores it.

- **ToolDone** comes from the subsequent `user`/`tool_result` event, matched by
  `tool_use_id`. claude's `tool_result` does **not** carry the tool name; the
  translator threads `Name` through from the earlier `tool_use` block. (coalesce
  resolves the display description by ID regardless, so an empty Name is
  tolerated — but threading it keeps the two backends symmetric.)

- **TurnError** distinguishes a failed turn via `result.subtype != "success"` —
  implemented as an **inequality, not an enumerated allow-list**, so a future
  error subtype (`error_max_budget_usd`, etc.) is never silently treated as
  success. A terminal `api_retry` that exhausts retries also maps here.

- **MessageEnd is non-finalizing.** In `coalesce`, `MessageEnd` triggers
  `flushLocked(false)` — a progress flush with **no** reset. Only `TurnDone` /
  `TurnError` / `Interrupted` call `flushLocked(true)` + `resetForNextTurn()`.
  This matters because, with `--include-partial-messages`, claude emits a
  `message_stop` **per Anthropic message**, and a single claude *turn* can contain
  multiple assistant messages (assistant → tool_result → assistant, all inside one
  `result`). Mapping every `message_stop` → `MessageEnd` is therefore **safe and
  correct**: it produces multiple mid-turn progress flushes (good UX), and the
  turn only finalizes/resets on the terminal `result` → `TurnDone`. The translator
  must emit `TurnDone` **only** from `result`, never from `message_stop`.

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

These are exactly the methods the router calls today. The full set of call sites
to migrate in PR A: `router.go` **418, 470, 525, 529, 548, 633, 681, 860, 1129**
(Subscribe / SubscribeExisting / SendMessage / Cancel). `Image` is a small
backend-neutral struct.

**`Image` struct:** carries `MediaType string` + `Data []byte` (decoded bytes,
not a path). The router already passes concrete image data today; PR A's jcode
adapter must produce `agent.Image` losslessly from the current `[]jcodeproto.ImagePair`
the router constructs (verify the existing field shapes during PR A — if jcode's
images are path-based, the adapter reads+holds bytes so the contract is uniform).
The claude adapter base64-encodes `Data` into a `{type:image, source:{type:base64,
media_type, data}}` block.

**Event-channel lifecycle (contract both backends must honor — copy jcode's
existing `closeEvents`/`closedOnce` pattern):**
- The channel is closed **exactly once**, when the session terminates permanently
  (subprocess/socket dies and is not recoverable) or `Close()` is called.
- It is **not** closed on a recoverable disconnect/crash — those respawn/reconnect
  transparently (see crash recovery below).
- `router.consumeEvents` ranges over the channel; a never-closed channel leaks the
  consumer goroutine and a double-close panics, so the once-semantics are required.

**`SendMessage` ↔ subprocess correlation (claude backend):** a
`map[sessionID]*session` guarded by a mutex resolves the target subprocess.
Writes to a subprocess's stdin are serialized by a per-session write mutex
(mirrors jcode's `writeMu`). If `SendMessage` is called while a prior turn is
still streaming, the user line is written to stdin and **queued by claude** (it
processes turns sequentially) — switchboard does not reject or block it. The
router's existing turn-queue/coalescer machinery already governs when a new
message is sent, so this matches current jcode behavior.

**`SubscribeExisting` for claude has no live process to reattach to** (unlike
jcode, which reconnects to a daemon-side session). After a switchboard restart the
subprocess is gone, so `SubscribeExisting(sessionID, workdir)` **spawns a fresh**
`claude --resume <session_id>` process (cwd = workdir) and returns its event
channel. That process **replays nothing and emits no events until the next
`SendMessage`** — it simply waits on stdin. This is compatible with the router's
recovery path (`router.go:1129`), which re-attaches a consumer and lets the user's
next message drive the first turn; recovery does not expect events before then.

## Claude backend (`internal/agent/claude/`)

- **`process.go`** — one persistent subprocess per session, mirroring jcode's
  `sessionConn`: spawn the `claude` command with `cmd.Dir = workdir`; a reader
  goroutine scans stdout NDJSON; writes serialize user turns to stdin. The spawn
  is behind an **exec seam** (an interface returning stdin/stdout pipes) so tests
  feed canned stream-json without invoking a real binary.
- **`translate.go`** — pure function `line []byte → []agent.Event` (table-tested),
  holding the per-session partial-stream accumulator state for tool blocks.
- **Resume / crash recovery** — on unexpected subprocess exit, **first emit
  `Interrupted`** (so `coalesce` flushes its dirty buffer and resets — otherwise a
  mid-turn crash leaves accumulated text/pending tools with no `ToolDone`/`TurnDone`
  and the coalescer hangs), **then** respawn with `--resume <session_id>` using the
  same exponential-backoff strategy jcode uses in `handleDisconnect`. The
  incomplete turn's output is lost (the CLI does not persist it); the user
  re-triggers with their next message. If respawn fails permanently, close the
  event channel (see lifecycle contract above).
- **Cancel** — SIGINT the process, emit `Interrupted`, respawn with `--resume`
  on the next message. (Same Interrupted-then-respawn path as crash recovery; the
  only difference is the trigger.)

## jcode adapter

The existing `internal/jcode` client keeps its socket logic. A thin adapter makes
it satisfy `agent.Backend`: its event channel is wrapped so `jcodeproto.ServerEvent`
is translated to `agent.Event` (`translateJcode(ev) []agent.Event`).

**ToolInputDelta routing — the behavior-preservation crux of PR A.** jcode's
`tool_input` events carry **no tool ID**; coalesce today routes them to the most
recently started non-exec tool. The normalized `ToolInputDelta` carries an `ID`
(claude has one). To preserve jcode behavior *exactly*, the jcode adapter emits
`ToolInputDelta` with an **empty `ID`**, and coalesce's `ToolInputDelta` handler
keeps both paths:
- `ID == ""` → fall back to the existing "most-recently-started non-exec tool"
  heuristic (jcode's current, unchanged behavior).
- `ID != ""` → route precisely by ID (claude).

This is the single quirk most likely to break PR A's "no behavior change" claim,
so it gets a dedicated coalesce test asserting the empty-ID path is byte-identical
to today's output. See risk note in the delivery plan.

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
regardless of config. "claude is selected" is determined by scanning **both** the
global `routing.backend.default` **and every** `ChannelConfig.Backend` override
(an idle channel with `backend = "claude"` still counts). If claude is selected
anywhere and the binary is absent or non-functional, startup **fails fast** with a
clear error. If no channel and no default uses claude, a missing binary is logged
as a warning and startup proceeds (jcode-only hosts are not blocked).

## Store

Add **`migrateV4`**:

```sql
ALTER TABLE sessions ADD COLUMN backend TEXT NOT NULL DEFAULT 'jcode';
```

(Following the v3 pattern, resilient to SQLite's non-transactional
`PRAGMA user_version`.) On recovery, the router reads `backend` to select the
correct `Backend` for `SubscribeExisting`. The `jcode_session` column **and the
`idx_sessions_jcode` index keep their names** — the column now generically holds
"the backend session id" — to avoid churn across the codebase. A dedicated
`migrateV4` test is required (the v3 lesson: migrations are where tests earn their
keep).

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
  where the translation correctness is earned. Must include:
  - **claude tool-block ordering invariant**: assert the translator never emits
    `ToolStart` for a block before `ToolExec` of the prior tool block (the
    load-bearing invariant coalesce's no-ID input routing depends on).
  - **claude `message_stop` multiplicity**: a turn with multiple assistant
    messages emits multiple `MessageEnd` and exactly one `TurnDone` (from
    `result`), never `TurnDone` from `message_stop`.
  - **claude `TurnError` inequality**: an unknown/future `result.subtype` maps to
    `TurnError`, not `TurnDone`.
- **`internal/coalesce/coalesce_test.go`** — **empty-ID `ToolInputDelta` path is
  byte-identical to today's output** (PR A behavior-preservation guard), plus the
  ID-routed path for claude.
- **`internal/agent/claude/process_test.go`** — fake exec seam streams a recorded
  `claude` session; assert lifecycle: spawn → `SessionReady` → turn events →
  **crash mid-turn emits `Interrupted` before respawn** → respawn-with-`--resume` →
  `SubscribeExisting` spawns a fresh process that emits nothing until the next
  `SendMessage`.
- **Router selector test** — channel/global backend resolution (mirrors the
  `usePerThreadWorkdir` table tests).
- **Store** — dedicated `migrateV4` test (the v3 lesson: migrations and shell-outs
  are where tests earn their keep).

## Delivery plan (two PRs)

**PR A — Backend abstraction (refactor, behavior-preserving but NOT trivial):**
1. `internal/agent` package: `Event`, `Backend`, `Image`.
2. `translateJcode`; jcode adapter satisfies `agent.Backend`.
3. `coalesce.HandleEvent` and `router.consumeEvents` switch to `agent.Event`.
4. Existing jcode tests stay green; no config or schema change.

> **Risk note — PR A is a full event-vocabulary swap, not a small refactor.**
> It rewrites coalesce's 13-case `switch ev.Type` and all 9 router call sites,
> behind a `translateJcode` layer that must be *exactly* behavior-preserving. The
> highest-risk corner is the `ToolInputDelta` empty-ID quirk (see jcode adapter):
> the normalized event gained an ID that jcode never had, and coalesce must behave
> byte-identically on the empty-ID path. The team's strict TDD + existing jcode
> tests are the safety net; PR A must add the empty-ID coalesce test before the
> switch. Treat "no behavior change" as the *verification target*, not an excuse
> to skip tests.

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
