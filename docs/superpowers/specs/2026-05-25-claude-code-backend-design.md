# Claude Code Backend — Design

**Date:** 2026-05-25 (revised 2026-06-06)
**Status:** Revised (pending review)
**Branch:** `feat/claude-code-backend`

## Revision history

- **2026-06-06 — Mechanism rewrite.** The original design spawned `claude -p ''`
  per turn with `--permission-mode bypassPermissions` and `--session-id <uuid>`.
  Production testing on the merged PR (`d28a85d`) showed empty responses across
  channels: each turn cold-started a fresh `claude` process which inherited the
  host's interactive Claude Code SessionStart hooks (superpowers, brainspike,
  etc.) via `~/.claude/settings.json`. The hook output bloated the system prompt
  with tens of KB of "must use skills / brainstorm first" guidance, Sonnet 4
  spent the turn in `thinking` content blocks deliberating about which skill
  to use, never producing a `text` block — translator dropped thinking deltas,
  surfaced empty output. Revised mechanism mirrors the shape of the **cc-connect**
  wrapper in the sibling repo `~/workspace/cc-connect/agent/claudecode/`: one
  **long-running** `claude` process per session, stdin held open, turns written
  as stream-json lines into the live process, `--permission-prompt-tool stdio`
  with in-process auto-approve, setpgid + process-group kill, `CLAUDECODE` env
  stripped. The **hook root cause is addressed separately by `--bare`** (skips
  hooks, LSP, plugins — see §Invocation). Long-running is still the right shape
  because (a) state stays warm across turns, (b) it composes cleanly with the
  upcoming agent-handover feature.
- **2026-06-06 — Restart resume strategy.** Original plan persisted the claude
  session UUID and used `--resume <uuid>` for `SubscribeExisting`. An interim
  revision tried `claude --continue`, but `--continue` is documented as "the
  most recent conversation in [cwd]" — workdir-relative, so two concurrent
  switchboard sessions sharing a workdir (the case when `per_thread_workdir` is
  off) would cross-wire. Reverted to **`--resume <session_id>`**. The UUID is
  captured from `system/init` on first spawn and written to the existing
  `sessions.jcode_session` column (already present via `migrateV4`); no new
  schema is required.
- **2026-06-06 — Handover-readiness.** The redesign is intentionally compatible
  with the upcoming in-thread agent-handover feature (see `docs/BACKLOG.md`):
  warm claude process held in a router-side per-thread map, not torn down on
  handover. See §Handover compatibility for how this composes with the
  close-once channel contract.

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
- One long-running `claude` subprocess per session, stable across turns, so
  state is warm and no per-turn cold-start cost is paid.
- Clean teardown of the entire descendant tree (claude → MCP bridges) when a
  session ends.
- Keep jcode the default; introduce Claude behind config — same shape as the
  existing `per_thread_workdir` global-default + per-channel pattern.
- No behavior change for existing jcode deployments after the refactor.

## Non-Goals

- Replacing jcode. Both backends coexist.
- A Go SDK for Claude Code (none exists). We drive the `claude` CLI.
- Mid-turn in-band cancellation for Claude (the CLI has none; we cancel by
  killing the process group and respawning).
- Rebuilding the agent loop against the raw Anthropic API.
- Importing or vendoring `cc-connect`. The cc-connect pattern is the
  inspiration; the implementation is switchboard-native against
  `internal/agent.Backend` and `agent.Event`.

## Mechanism: long-running `claude` CLI subprocess

Claude Code has no Go SDK. The integration spawns the `claude` CLI in
streaming, interactive (not `-p`) mode — one **persistent subprocess per
session**, mirroring how jcode uses one Unix socket per session. The process is
spawned once when the session starts and stays alive across all turns until
the session is closed or handed over.

### Invocation

Process `cwd` is set to the session workdir (there is no `--cwd` flag):

```
claude \
  --bare \
  --input-format stream-json \
  --output-format stream-json \
  --verbose \
  --permission-prompt-tool stdio \
  --replay-user-messages \
  --append-system-prompt "<switchboard system prompt>" \
  --model <model> \
  [--resume <session_id>]
```

Notes on each flag:

- **`--bare`.** "Minimal mode: skip hooks, LSP, plugins" (per `claude --help`).
  Load-bearing for switchboard: the host's `~/.claude/settings.json` defines
  SessionStart hooks (superpowers, brainspike, claude-mem) that fire in every
  spawned `claude` and bloat the system prompt with tens of KB of guidance.
  Bloated prompts caused the empty-response bug observed on the original
  mechanism. Stripping `CLAUDECODE` from env is not enough — hooks come from
  settings, not env. `--bare` is what closes that gap. **Without `--bare` the
  rewrite would reproduce the same empty-response symptom.**
- **No `-p '' `.** Print mode forces a single-turn run that exits when stdin
  closes; we want a session-lifetime process that reads many turns from stdin.
- **`--input-format stream-json` + `--output-format stream-json`.** Bidirectional
  stream-json over stdio. Turns in, events out, all NDJSON. In this mode the
  CLI emits `content_block_delta` events natively for text and tool input
  streaming — no additional flag required.
- **`--verbose`.** Required when `--output-format stream-json` is combined with
  the interactive (non-`-p`) run; the CLI rejects the combination without it.
- **No `--include-partial-messages`.** The CLI documents this flag as
  *"only works with --print"*; we are not in print mode. We rely on the base
  stream-json's `content_block_delta` granularity for live text and tool-input
  streaming. The cc-connect reference also omits this flag, confirming the
  base stream is sufficient. (A smoke test in the first implementation PR will
  verify text/tool deltas actually arrive at expected cadence; if they don't,
  fall back to the multi-message-per-turn pattern and accept coarser UX.)
- **`--permission-prompt-tool stdio`.** Permission requests come back as
  `control_request` lines on stdout; we respond with `control_response` lines
  on stdin. **Not** `--permission-mode bypassPermissions` — the bypass path
  fails when run as root and is incompatible with the sandboxing options we
  may add later. Auto-approve policy lives in switchboard (see below).
- **`--replay-user-messages`.** Echoes user messages back in the stream so the
  wrapper sees them in the same event channel as assistant content. Matches
  jcode's behavior where user input is observable on the event stream.
- **`--append-system-prompt`.** Adds a small switchboard-specific system prompt
  (e.g. "you are running inside a Slack-bridged session, format responses for
  Slack mrkdwn") without replacing Claude Code's built-in system prompt.
  Confirmed safe to combine with `--bare`: `claude --help` lists
  `--append-system-prompt` among the explicit context-injection flags
  permitted in bare mode (alongside `--settings`, `--agents`, `--plugin-dir`).
- **`--model`.** Per-config / per-channel override.
- **`--resume <session_id>`.** Used **only** for recovery (after switchboard
  restart, after a crash mid-turn, after a Cancel). The UUID comes from the
  stored `sessions.jcode_session` column, captured originally from
  `system/init` on the first spawn. Not used for fresh sessions. We do not use
  `--continue` because it's workdir-relative and would cross-wire concurrent
  sessions sharing a workdir.

### Sending a turn

A user message is sent by writing one NDJSON line to the live process's stdin:

```json
{"type":"user","message":{"role":"user","content":"hello"}}
```

Multimodal input uses an array of content blocks:

```json
{"type":"user","message":{"role":"user","content":[
  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}},
  {"type":"text","text":"caption"}
]}}
```

Writes are serialized by a per-session `stdinMu` (mirrors jcode's `writeMu`).
If `SendMessage` is called while a prior turn is still streaming, the line is
written and **queued by claude** — it processes turns sequentially. Switchboard
does not reject or block it; the router's existing turn-queue/coalescer
machinery already governs when a new message is sent.

### Permission protocol (`--permission-prompt-tool stdio`)

When the model wants to use a tool, the CLI emits a control_request line:

```json
{"type":"control_request","request_id":"<id>","request":{
  "subtype":"can_use_tool",
  "tool_name":"Bash",
  "input":{ ... }
}}
```

Switchboard responds with a control_response line:

```json
{"type":"control_response","response":{
  "subtype":"success",
  "request_id":"<id>",
  "response":{"behavior":"allow","updatedInput":{ ... }}
}}
```

**Deny shape:**

```json
{"type":"control_response","response":{
  "subtype":"success",
  "request_id":"<id>",
  "response":{"behavior":"deny","message":"<reason>"}
}}
```

If a policy returns deny with an empty message, the wrapper substitutes
`"The user denied this tool use. Stop and wait for the user's instructions."`
(mirrors cc-connect's `session.go:631-633`) so the model gets actionable
feedback rather than a silent reject.

**Auto-approve policy.** Default is `allow_all` (matches jcode's existing
autonomous posture; the workdir is a normal checkout on the host, not a
sandbox; there is no human in the loop mid-turn in a Slack bot). The policy is
a switchboard-side struct so the same surface supports future modes:
`allow_all`, `deny_all`, `accept_edits_only` (allow Edit/Write/NotebookEdit/
MultiEdit, deny others), `prompt_in_slack` (post a prompt to the thread and
wait — out of scope for this PR). For now: a single global `allow_all` default
with a config knob to set it per-backend.

### Process group + cleanup

On Unix, spawn with `setpgid` so the claude CLI and every grandchild process
(MCP server bridges, the `bun` Telegram bridge, jdocmunch, etc.) live in one
process group. On Close/Cancel, send SIGTERM to the group, wait up to
`graceful_stop_timeout` (default 30s; cc-connect uses 120s for claude-mem
hooks — we'll start at 30s), then SIGKILL the group.

Without this, killing only the direct child leaves MCP grandchildren spinning
at 100% CPU after their parent's stdio pipe closes — the setpgid approach is
adopted preemptively from the cc-connect reference (`session.go:149-154`).

### Environment hygiene

Before spawning, the parent environment is filtered:

- **Strip `CLAUDECODE`** — its presence triggers "nested session" detection in
  the CLI which changes behavior; switchboard is a bridge, not a nested
  Claude Code instance.
- Pass through `ANTHROPIC_*`, `CLAUDE_*`, `AWS_*` (for Bedrock), `NO_PROXY`,
  and switchboard-relevant vars.
- Inject any `extra_env` from `[claude]` config last (overrides above).

### Resume / crash recovery

On unexpected subprocess exit during a turn, first emit `Interrupted` (so
`coalesce` flushes its dirty buffer and resets — otherwise a mid-turn crash
leaves accumulated text/pending tools with no `ToolDone`/`TurnDone` and the
coalescer hangs). Then respawn with `--resume <session_id>` using the UUID
stored in `sessions.jcode_session`, with the same exponential-backoff strategy
jcode uses in `handleDisconnect`. The incomplete turn's output is lost — the
CLI does not persist it — and the user re-triggers with their next message.
If respawn fails permanently, close the event channel (see lifecycle contract
below).

### Cancel

In-band cancel does not exist for the claude CLI. To cancel a turn:
SIGTERM the process group, emit `Interrupted`, respawn with
`--resume <session_id>`. The next `SendMessage` drives the next turn cleanly.
(Same Interrupted-then-respawn path as crash recovery; the only difference is
the trigger.)

## Architecture

```
router ──> agent.Backend (interface)
              ├── jcode adapter   (wraps existing *jcode.Client; jcodeproto → agent.Event)
              └── claude backend  (spawns `claude`; stream-json → agent.Event;
                                  one process per session, warm in steady-state,
                                  respawned on crash/cancel)
coalesce.HandleEvent(agent.Event)   ← single normalized vocabulary
```

The `internal/agent` package owns the normalized `Event` type and the `Backend`
interface (already merged in `d28a85d`). `coalesce` and `router` consume only
`agent.Event`. jcode and Claude are both adapters that translate their native
event streams into `agent.Event`.

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

- **Thinking blocks are silently dropped.** `content_block_start` of type
  `thinking` and its `thinking_delta` / `signature_delta` updates produce no
  `agent.Event`. Already the case in the merged translator (`translate.go:175`,
  `:227`).

- **`control_request` lines are handled by the process layer, not the
  translator.** They do not flow into `agent.Event`. The process layer auto-
  approves (per policy) and writes the `control_response` to stdin. If a future
  policy needs UI surfacing (e.g. `prompt_in_slack`), introduce a new
  `agent.EventPermissionRequest` then — not now.

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
  This matters because a single claude *turn* can contain multiple assistant
  messages (assistant → tool_result → assistant, all inside one `result`), each
  bracketed by `message_start` / `message_stop`. Mapping every `message_stop` →
  `MessageEnd` is therefore **safe and correct**: it produces multiple mid-turn
  progress flushes (good UX), and the turn only finalizes/resets on the terminal
  `result` → `TurnDone`. The translator must emit `TurnDone` **only** from
  `result`, never from `message_stop`.

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

**`SendMessage` for the claude backend** writes one stream-json `user` line to
the held-open stdin of the warm subprocess — it does **not** spawn a new
process. A `map[sessionID]*session` guarded by a mutex resolves the target
subprocess; per-session `stdinMu` serializes writes.

**`Subscribe`** spawns a fresh claude process (no `--continue`), waits for the
`system/init` event to extract the session UUID, and returns. The returned
session ID is the value from `system/init.session_id`.

**`SubscribeExisting`** is the recovery path used after switchboard restart.
For claude, it **spawns a fresh process with `--resume <sessionID>`** (cwd =
workdir), where `sessionID` is the value the caller (router) loaded from
`sessions.jcode_session`. The fresh process emits nothing until the next
`SendMessage`. This matches the router's recovery path (`router.go:1129`):
re-attach a consumer, wait for the next user message.

**Early-exit detection.** If `--resume` fails (the CLI has no record of that
session UUID — e.g. claude pruned the conversation), the subprocess exits
non-zero before any `system/init` arrives. The reader goroutine sees EOF
first; the backend caches the failure as a session-fatal flag. The next
`SendMessage` synthesizes `TurnError` (with the captured stderr) and the
event channel is closed per the once-semantics contract. The router then
treats this thread as needing a fresh session.

**`Image` struct:** carries `MediaType string` + `Data []byte` (decoded bytes,
not a path). The router already passes concrete image data today; PR A's jcode
adapter must produce `agent.Image` losslessly from the current
`[]jcodeproto.ImagePair` the router constructs. The claude adapter base64-encodes
`Data` into a `{type:image, source:{type:base64, media_type, data}}` block.

**Event-channel lifecycle (contract both backends must honor — copy jcode's
existing `closeEvents`/`closedOnce` pattern):**
- The channel is closed **exactly once**, when the session terminates permanently
  (subprocess/socket dies and is not recoverable) or `Close()` is called.
- It is **not** closed on a recoverable disconnect/crash — those respawn/reconnect
  transparently (see crash recovery above).
- `router.consumeEvents` ranges over the channel; a never-closed channel leaks the
  consumer goroutine and a double-close panics, so the once-semantics are required.

## Claude backend (`internal/agent/claude/`)

The package already exists in `d28a85d` with `backend.go` + `translate.go`.
This revision is a **rewrite of `backend.go`** (process management) and a
**small expansion of `translate.go`** (control_request handling moves to
`backend.go`, but the translator's other behavior is preserved). The
translator file itself stays close to its current shape — its existing tests
remain a useful regression guard.

- **`backend.go`** — one persistent subprocess per session.
  - Spawn `claude` with `cmd.Dir = workdir`, the flag set above, setpgid on,
    `CLAUDECODE` stripped from env.
  - Reader goroutine scans stdout NDJSON, runs each line through the
    translator, fans out `agent.Event` on the session's channel. Lines of
    `type:control_request` are intercepted before the translator and handled
    by the permission policy (writes `control_response` to stdin via the
    serialized `writeJSON`).
  - `SendMessage` writes one `user` line to stdin via `writeJSON`.
  - `Cancel` sends SIGTERM to the process group, waits up to
    `graceful_stop_timeout`, then SIGKILL.
  - `Close` closes stdin (signals clean EOF; lets claude run its Stop hooks),
    waits up to `graceful_stop_timeout` for natural exit, then escalates to
    SIGTERM-group → SIGKILL-group.
  - Spawn is behind an **exec seam** (an interface returning stdin/stdout
    pipes + a cleanup func) so tests feed canned stream-json without invoking
    a real binary.

- **`translate.go`** — unchanged except for the load-bearing invariants
  already captured in tests on the merged code. Stays a pure
  `line []byte → []agent.Event` (table-tested), holding per-session
  partial-stream accumulator state for tool blocks.

- **`permission.go`** (new, small) — the policy struct:
  ```go
  type PermissionPolicy interface {
      Decide(toolName string, input map[string]any) PermissionResult
  }
  ```
  Default impl: `AllowAll{}`. Future impls (`AcceptEditsOnly`, etc.) plug in
  here. Tests can substitute a recording policy.

## jcode adapter

The existing `internal/jcode` client keeps its socket logic. A thin adapter makes
it satisfy `agent.Backend`: its event channel is wrapped so `jcodeproto.ServerEvent`
is translated to `agent.Event` (`translateJcode(ev) []agent.Event`).
(Already merged in `1f18926` / `4526aa0`.)

**ToolInputDelta routing — the behavior-preservation crux of PR A.** jcode's
`tool_input` events carry **no tool ID**; coalesce today routes them to the most
recently started non-exec tool. The normalized `ToolInputDelta` carries an `ID`
(claude has one). To preserve jcode behavior *exactly*, the jcode adapter emits
`ToolInputDelta` with an **empty `ID`**, and coalesce's `ToolInputDelta` handler
keeps both paths:
- `ID == ""` → fall back to the existing "most-recently-started non-exec tool"
  heuristic (jcode's current, unchanged behavior).
- `ID != ""` → route precisely by ID (claude).

## Config

```toml
[routing.backend]
default = "jcode"            # global default: "jcode" | "claude"

[claude]                     # claude-backend settings
binary = "claude"
model = "claude-sonnet-4-6"
permission_policy = "allow_all"  # allow_all | deny_all | accept_edits_only
append_system_prompt = ""    # optional, appended to claude's built-in prompt
graceful_stop_timeout = "30s"
extra_args = []              # optional escape hatch (appended after our flags)

[claude.extra_env]           # optional, matches switchboard's existing
ANTHROPIC_API_KEY = "..."    # map[string]string convention (see routes.match)

[[channels]]
id = "C123"
backend = "claude"           # per-channel override (unset → inherit default)
model = "claude-sonnet-4-6"  # optional per-channel model override
```

- `RoutingConfig2` already has `Backend BackendRoutingConfig` (`Default string`)
  from `d28a85d`.
- `ClaudeConfig` already exists; this revision **drops `permission_mode`** and
  adds `permission_policy`, `append_system_prompt`, `graceful_stop_timeout`,
  `extra_env` (as `map[string]string`, matching the convention at
  `config.go:48` `Repos` and `config.go:164` `Match`).
- `ChannelConfig.Backend` and `ChannelConfig.Model` already exist
  (`config.go:158-160`).
- Selection logic mirrors `usePerThreadWorkdir`: per-channel value overrides
  the global default; unset inherits.

### Backwards-compat for `permission_mode`

The previous merged config used `permission_mode = "bypassPermissions"`. Existing
config files in the wild will still have it. On load:

- `permission_mode = "bypassPermissions"` (or absent) → silently treat as
  `permission_policy = "allow_all"` (the equivalent behavior).
- `permission_mode = "default"` / `"acceptEdits"` / `"plan"` / `"dontAsk"` → log
  a single startup warning naming the key and the mapped policy (`default` →
  `allow_all`; `acceptEdits` → `accept_edits_only`; `dontAsk` → `deny_all`;
  `plan` → `allow_all` with a note that plan-mode isn't preserved across the
  rewrite), and continue.
- Both `permission_mode` and `permission_policy` set → fail loud at startup
  with a clear error telling the operator to remove the legacy key.

Removing the legacy `permission_mode` field entirely is deferred to the release
after this one.

## Startup validation

The `claude` binary is validated at **every** startup (`claude --version`),
regardless of config. "claude is selected" is determined by scanning **both**
the global `routing.backend.default` **and every** `ChannelConfig.Backend`
override (an idle channel with `backend = "claude"` still counts). If claude
is selected anywhere and the binary is absent or non-functional, startup
**fails fast** with a clear error. If no channel and no default uses claude, a
missing binary is logged as a warning and startup proceeds (jcode-only hosts
are not blocked).

## Store

`migrateV4` (already merged in `d28a85d` + self-healed in `8940919`) adds
`backend TEXT NOT NULL DEFAULT 'jcode'` to `sessions`. On recovery the router
reads `backend` to select the correct `Backend` for `SubscribeExisting`.

The `jcode_session` column **and the `idx_sessions_jcode` index keep their
names** — the column generically holds "the backend session id". For claude
sessions, this column stores the UUID captured from `system/init` and is the
value passed to `--resume` on respawn / restart-recovery.

**No additional schema changes are needed for this revision.** The UUID is
captured at first spawn and written to the existing column. Future in-thread
handover may want a JSON map column for warm prior-backend session IDs (see
`docs/BACKLOG.md`) — that's deferred until the handover feature lands.

## Handover compatibility

A planned follow-up feature (see `docs/BACKLOG.md`) lets the user hand off a
thread to another agent mid-conversation — e.g. "build this feature, then use
codex to do a code review of it". This spec is intentionally compatible with
that future feature; the design rules below resolve the tension between
holding a warm process across handover and the close-once channel contract.

- **Handover does not call `Close()`** on the warm claude session. The session
  stays in a router-side `map[(channel, thread)]*ClaudeSession` and its
  subprocess remains running.
- **While handed off**, the router stops fanning the session's event channel to
  `coalesce`. Between turns, a claude subprocess emits nothing on stdout (it
  blocks on stdin waiting for the next message), so the channel sits empty —
  no buffering pressure, no goroutine leak.
- **Returning control** to claude is just a `SendMessage` to the warm session.
  No respawn, no `--resume`, full context still warm.
- **`Close()` is called only on permanent termination**: thread archived,
  switchboard shutdown, or an explicit eviction policy (idle timeout — out of
  scope for this PR; logged in BACKLOG). Close-once contract is preserved
  because handover is not termination.
- **If the warm subprocess dies while held in the map** (OOM, crash), the
  router treats it like any other crash — emit `Interrupted`, respawn with
  `--resume <stored uuid>` on the next `SendMessage`. The warm-process
  invariant is steady-state, not absolute.

**Operational note.** Until idle-eviction lands (see `docs/BACKLOG.md`), a
long-running switchboard accumulates one warm `claude` subprocess (plus its
MCP grandchild tree) per `(channel, thread)` ever conversed with. Hosts with
many active threads should expect commensurate RAM/FD pressure. This is
acceptable for the initial handover feature because the active-thread set is
small on dev/single-user deployments; on a multi-tenant host the eviction
policy becomes load-bearing.

The router-side map is out of scope for this PR — it lands with the handover
feature. This PR delivers the backend in a shape that **does not preclude** it:
`Subscribe`/`SubscribeExisting`/`SendMessage`/`Close` are pure-per-session and
do not assume the lifetime is bounded by a single turn.

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
  feed raw NDJSON lines, assert the normalized `agent.Event` sequence. Must
  include the tests already on the merged code (thinking-block drop,
  message_stop multiplicity, TurnError inequality, tool-block ordering
  invariant) — they all remain valid under the new mechanism.
- **`internal/coalesce/coalesce_test.go`** — **empty-ID `ToolInputDelta` path is
  byte-identical to today's output** (PR A behavior-preservation guard), plus the
  ID-routed path for claude.
- **`internal/agent/claude/backend_test.go`** (rewritten) — fake exec seam
  streams a recorded `claude` session; assert lifecycle:
  - spawn → `system/init` → `SessionReady` emitted with init's session UUID.
  - **`--bare` is on the argv** of every spawn (load-bearing: this is what
    prevents the host SessionStart hooks from firing and reproducing the
    empty-response bug).
  - **`--include-partial-messages` is NOT on the argv** (would be a no-op at
    best; documented as `--print`-only).
  - **`CLAUDECODE` is stripped** from the spawned process env.
  - `SendMessage` writes the expected `{"type":"user","message":…}` line to
    stdin (capture via the fake stdin pipe).
  - `control_request` (`subtype:can_use_tool`) → policy invoked → expected
    `control_response` written to stdin. Cases: `AllowAll` allow shape with
    `updatedInput`; `DenyAll` deny shape with non-empty `message`; deny with
    empty policy message gets the default-deny string.
  - **Crash mid-turn emits `Interrupted` before respawn**, then respawn uses
    `--resume <stored_uuid>` (assert the args of the second spawn include
    `--resume` and the captured UUID from the first `system/init`).
  - `Cancel` SIGTERMs the process group (verify via a fake `cmd` that records
    `pgid` and signal); after grace period escalates to SIGKILL.
  - `Close` closes stdin first; if the process exits within grace, no signals
    are sent.
  - `SubscribeExisting(uuid, workdir)` spawns with `--resume <uuid>` (cwd =
    workdir) and emits nothing until `SendMessage`. If `--resume` fails
    (subprocess exits with non-zero before any event), the next `SendMessage`
    surfaces `TurnError`.
  - **Process-group cleanup integration test** (`proc_unix_test.go`-style):
    spawn a fake child that itself spawns a grandchild that sleeps 60s;
    `Close` must terminate both. Skipped on non-Unix.
- **`internal/agent/claude/streaming_smoke_test.go`** (new, build-tag-gated;
  not part of the default test pass) — spawns a real `claude --bare` with our
  flag set (minus `--include-partial-messages`). Two assertions, both before
  the terminal `result`:
  1. A short text-only prompt produces at least one `content_block_delta`
     with `text_delta`.
  2. A prompt that provokes a tool call (e.g. "list files in `/tmp`")
     produces at least one `content_block_delta` with `input_json_delta`.
  Together these validate the BL-2 assumption that base stream-json gives us
  live deltas for **both** text and tool input without
  `--include-partial-messages`. Gated behind a build tag because it needs a
  real binary and a real model; run manually before tagging the release that
  ships this PR.
- **`internal/agent/claude/permission_test.go`** — `AllowAll.Decide` returns
  `{allow, input}`; `DenyAll.Decide` returns `{deny, "…"}`;
  `AcceptEditsOnly.Decide` returns allow for Edit/Write/NotebookEdit/MultiEdit
  and deny for Bash.
- **Router selector test** — channel/global backend resolution (mirrors the
  `usePerThreadWorkdir` table tests). Already exists; no changes needed for
  the revised recovery path (`--resume <uuid>` uses the existing
  `sessions.jcode_session` column the selector already reads).
- **Store** — `migrateV4` test already exists (merged in `d28a85d`); no
  additional schema change here.

## Delivery plan (one PR on top of current trunk)

PR A (backend abstraction) and the initial PR B (subprocess-per-turn claude
backend) **are already merged** as `4526aa0` and `d28a85d`. This revision is
delivered as a single follow-up PR on `feat/claude-code-backend`:

**PR — Claude backend rewrite (subprocess-per-turn → one persistent process):**
1. Config: add `permission_policy`, `append_system_prompt`,
   `graceful_stop_timeout`, `extra_env` (as `map[string]string`). Backwards-compat
   translation of legacy `permission_mode` per §Backwards-compat — translate +
   warn, or fail-loud when both are set.
2. `internal/agent/claude/backend.go` — rewrite for long-running process:
   `--bare`, spawn-once, stdin-held-open, setpgid + group-kill, `CLAUDECODE`
   env strip, `--resume <uuid>` recovery using the UUID stored in
   `sessions.jcode_session`.
3. `internal/agent/claude/permission.go` — new file; `AllowAll` default,
   `control_request` handler wired in `backend.go`. Includes the deny-shape
   default-message fallback.
4. `internal/agent/claude/proc_unix.go` (+ `proc_windows.go` stub) — setpgid
   spawn + group-kill helpers (Windows TBD; build-tag scoped).
5. `internal/agent/claude/backend_test.go` — rewrite; cover the new lifecycle
   including `--bare`-on-argv, `CLAUDECODE`-stripped, `--resume`-on-respawn,
   and the permission allow/deny/default-message paths.
6. `internal/agent/claude/streaming_smoke_test.go` — build-tag-gated; verifies
   base stream-json gives us live text deltas without
   `--include-partial-messages`. Manual gate before release.
7. No schema change. No router-interface change. `Subscribe` /
   `SubscribeExisting` / `SendMessage` / `Cancel` / `Close` signatures stay.

> **Risk note — the rewrite is invisible to the router but very visible to
> claude's stdio contract.** The translator already passes its tests; the
> rewrite's risk is concentrated in (a) the control_request stdio protocol,
> (b) process-group cleanup, and (c) the `--resume <stored_uuid>` recovery
> path correctly re-attaching and emitting a usable `system/init`. Each gets
> a dedicated test. The fake exec seam is the safety net.

## Decisions captured (from brainstorming and revision)

- **Scope:** full parity.
- **Mechanism:** `claude` CLI subprocess, **one long-running process per
  session**, stdin held open, stream-json on both sides. `--bare` to skip
  host hooks/LSP/plugins.
- **Permissions:** `--permission-prompt-tool stdio` with in-process
  auto-approve policy (default `allow_all` — matches jcode's autonomous
  posture). **Not** `bypassPermissions`. Legacy `permission_mode` config
  translated on read with a warning.
- **Structure:** abstraction-first (`internal/agent`), jcode is an adapter,
  claude is a sibling adapter — switchboard-native, no `cc-connect` import.
- **Streaming:** base stream-json (no `--include-partial-messages`, which is
  `--print`-only). `--verbose` required when combining stream-json with
  interactive (non-`-p`) mode. Streaming-granularity smoke test before
  release.
- **Restart resume:** `--resume <session_id>` using the UUID from
  `sessions.jcode_session` (captured originally from `system/init`). Not
  `--continue` — workdir-relative semantics cross-wire concurrent sessions.
- **Process tree:** setpgid + SIGTERM-group → SIGKILL-group; strips
  `CLAUDECODE` from env to prevent nested-session detection.
- **Binary validation:** validate at every startup.
- **Handover-readiness:** warm process held in router-side per-thread map,
  not torn down on handover. Close-once contract preserved because handover
  is not termination. Router-side map lands with the handover feature. See
  `docs/BACKLOG.md` and §Handover compatibility.
