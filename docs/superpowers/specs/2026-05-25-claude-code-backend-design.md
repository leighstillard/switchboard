# Claude Code Backend — Design

**Date:** 2026-05-25 (revised 2026-06-07)
**Status:** Revised (pending review)
**Branch:** `feat/claude-code-backend`

## Revision history

- **2026-06-07 — Auth, env, and agent-switching revision.** Seven changes:
  1. **`--bare` removed.** `claude --help` documents `--bare` as forcing Anthropic
     auth to be "strictly `ANTHROPIC_API_KEY` or apiKeyHelper via `--settings`
     (OAuth and keychain are **never read**)." switchboard runs on the operator's
     **Claude subscription**, not a metered API key, so `--bare` would either fail
     (no key) or silently bill the wrong account. Replaced with **selective
     settings loading** via `--setting-sources project,local` (excludes the
     `user` layer where the host's noisy SessionStart hooks live) — this fixes
     the empty-response bug **without** disabling OAuth/keychain. See §Invocation.
  2. **Environment is preserved**, not allow-listed. Only `CLAUDECODE` is
     stripped. The previous allow-list risked dropping vars the CLI/keychain
     helper needs for OAuth. See §Environment hygiene.
  3. **Reversible per-thread agent switching** with **one session id per
     backend per thread**, persisted, so claude→codex→claude returns to the
     original warm claude conversation. Promoted from BACKLOG into this spec.
     See §Agent switching.
  4. **Process ownership lives inside each backend**, not in a router-side map.
     The router selects *which* backend takes the next turn; the backend owns
     its subprocess lifecycle. See §Agent switching and §Router wiring.
  5. **Switch timing, inactive-event handling, persistence, and idle eviction**
     are now specified (not deferred). See §Agent switching.
  6. **Subscription-authenticated multi-turn + handover smoke test** added —
     a real `claude` run under OAuth (no API key in env) that does several turns
     and a round-trip handover. See §Testing.
  7. **Claude CLI protocol is pinned/probed** at startup. See §CLI compatibility.

  Retained from the 2026-06-06 rewrite: normalized event translation, the
  persistent (long-running) process, the `--permission-prompt-tool stdio`
  permission protocol, setpgid process-group cleanup, and `--resume` recovery.
- **2026-06-07 — codex review fixes.** (1) `thread_backend_sessions` FK gets
  `ON DELETE CASCADE` so `DeleteSession` doesn't fail the constraint. (2) A
  switch **forwards the residual task** ("…review it") to the new backend as its
  first prompt instead of waiting for a new message. (3) Dormant handling is the
  **router consuming + filtering** every channel (the backend gets no
  activate/deactivate signal, so it can't drain); idleness is inferred from
  `SendMessage` timestamps. (4) Added **`CloseSession(sessionID)`** for
  per-thread teardown (`Close()` tears down the whole backend). (5) The protocol
  probe is **two-stage**: validate `system/init` shape, then validate the first
  `stream_event` envelope under a timeout (init alone can't prove the later
  envelope).

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
  stripped. The **hook root cause is addressed by excluding the `user` settings
  layer** (where the host SessionStart hooks live) — see §Invocation. (This
  entry originally specified `--bare`; superseded by the 2026-06-07 revision
  above, which switches to `--setting-sources` to keep subscription OAuth.)
  Long-running is still the right shape because (a) state stays warm across
  turns, (b) it composes cleanly with the agent-switching feature.
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
  with in-thread agent handover. (This entry originally proposed a router-side
  warm-process map; the 2026-06-07 revision moved process ownership **inside the
  backend** and specified the full switching design — see §Agent switching.)

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
  --setting-sources project,local \
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

- **`--setting-sources project,local`** (replaces the earlier `--bare`).
  `claude --help`: "Comma-separated list of setting sources to load (user,
  project, local)." Omitting a source skips that layer. The empty-response bug
  came from SessionStart hooks (superpowers, brainspike, claude-mem) defined in
  the **`user`** layer (`~/.claude/settings.json`); excluding `user` stops those
  hooks from firing and bloating the system prompt, which was the actual root
  cause. **Why not `--bare`:** `--bare` also documents that "Anthropic auth is
  strictly `ANTHROPIC_API_KEY` or apiKeyHelper via `--settings` (OAuth and
  keychain are **never read**)." switchboard authenticates with the operator's
  **Claude subscription** (OAuth/keychain), not a metered API key, so `--bare`
  would break or mis-bill auth. `--setting-sources` is a *settings*-layer filter
  only — it does **not** touch credentials, so subscription OAuth/keychain are
  read normally. This is the precise, minimal fix.
  - **Project/local hygiene.** The excluded layer is `user`; `project` and
    `local` settings come from the session **workdir**. switchboard controls the
    workdir (a normal checkout), so it does not normally carry SessionStart-hook
    bloat — but if a target repo's `.claude/settings.json` adds noisy hooks, the
    operator can tighten the set (see `setting_sources` config). The
    subscription smoke test (§Testing) asserts the chosen set both authenticates
    via OAuth **and** does not reproduce the empty-response symptom.
  - **Configurable.** `setting_sources` is a `[claude]` config knob defaulting to
    `"project,local"`; set it to `"local"` (or empty) to exclude more, or
    `"user,project,local"` to opt back into the full host config.
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
  Slack mrkdwn") without replacing Claude Code's built-in system prompt. Since
  the `user` settings layer is excluded, this flag (plus `--mcp-config` /
  `--add-dir` when needed) is how switchboard injects the context it actually
  wants, rather than inheriting whatever the host user configured.
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

The child **inherits the parent environment in full**, with exactly one
removal:

- **Strip `CLAUDECODE`** — its presence triggers "nested session" detection in
  the CLI which changes behavior; switchboard is a bridge, not a nested
  Claude Code instance.
- **Everything else passes through unchanged.** An earlier draft used an
  allow-list (`ANTHROPIC_*`, `CLAUDE_*`, `AWS_*`, …), but that is fragile:
  subscription OAuth / the keychain helper and the CLI's own runtime depend on
  ambient vars (`HOME`, `PATH`, `XDG_*`, `SSH_AUTH_SOCK`, proxy vars, locale,
  etc.) that an allow-list inevitably misses, which would manifest as
  intermittent auth failures. Preserve the env; subtract only `CLAUDECODE`.
- Apply `extra_env` from `[claude]` config **last**, so it can override any
  inherited var (including, if an operator deliberately wants it, forcing
  `ANTHROPIC_API_KEY` for a non-subscription deployment).

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
    CloseSession(ctx context.Context, sessionID string) error  // permanent teardown of ONE session
    Close() error                                              // teardown of the whole backend
}
```

**`CloseSession(sessionID)` vs `Close()`** (codex P2). `Close()` tears down the
*entire* backend (all sessions) — used at switchboard shutdown. Per-thread
permanent teardown (a thread is archived, or a logical session is abandoned)
needs to close **one** session without killing the others: that is
`CloseSession`. It terminates that session's process group, closes its event
channel exactly once, and removes its warm handle. (Idle eviction is distinct: it
tears down the *process* but keeps the persisted session id and does **not**
close the logical session — the next activation resumes it. `CloseSession` is
permanent: it would also delete the `thread_backend_sessions` row.) jcode's
adapter implements `CloseSession` by closing that one socket; `Close()` closes
all sockets.

These are the methods the router calls (Close-per-session added this revision). The full set of call sites
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
setting_sources = "project,local"  # settings layers to load; excludes "user"
                                   # (host SessionStart hooks) by default. Does
                                   # NOT affect OAuth/keychain auth.
graceful_stop_timeout = "30s"
idle_eviction_timeout = "30m"  # tear down a dormant warm subprocess after this
                               # idle; "0" = never. Session id is retained for
                               # --resume rehydration.
min_version = "2.1.0"        # fail-fast (if claude selected) below this CLI version
max_version = ""             # optional ceiling; above it warns but proceeds
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
  adds `permission_policy`, `append_system_prompt`, `setting_sources`,
  `graceful_stop_timeout`, `idle_eviction_timeout`, `min_version`/`max_version`,
  and `extra_env` (as `map[string]string`, matching the convention at
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

## CLI compatibility (pin / probe the protocol)

The integration is coupled to the `claude` CLI's stream-json protocol and flag
surface, both of which can change across releases. Two guards:

1. **Version probe at startup.** `claude --version` is parsed (current dev
   baseline: **2.1.168**). switchboard carries a `supported_cli` range in
   `[claude]` config (`min_version`, optional `max_version`; default
   `min_version = "2.1.0"`, no max). If the detected version is below the floor,
   startup behaves like the binary-validation rule above: **fail fast** if claude
   is selected, **warn** otherwise. A version above a configured ceiling logs a
   warning ("untested CLI version") but proceeds, so a routine CLI upgrade is not
   a hard outage — the smoke tests are the real compatibility gate.
2. **Protocol probe, in two stages** (codex P2 — `system/init` alone cannot
   prove the later stream-event envelope, so don't claim it does):
   - *Stage 2a — init shape.* Validate the first `system/init` for the fields the
     backend reads from it directly (`session_id`, model). If it is absent or
     malformed, emit `TurnError` "incompatible claude CLI protocol (init)".
   - *Stage 2b — first stream event.* On the **first real turn**, validate the
     first `stream_event` envelope the translator depends on (the
     `content_block_delta` shape) **within a timeout**. If no well-formed stream
     event arrives before the timeout, emit `TurnError` "incompatible claude CLI
     protocol (stream)". This catches an envelope change that `system/init`
     cannot reveal, and replaces the silent-empty-turn failure mode that
     motivated this revision with an explicit, actionable error.

Pinning the exact CLI version in the deployment (e.g. the install step records a
known-good version) is recommended operationally; the config range is the
in-process backstop. The build-tag smoke tests (§Testing) are run against the
pinned version before each release to catch protocol drift the probe can't.

## Store

`migrateV4` (merged in `d28a85d`, self-healed in `8940919`) adds
`backend TEXT NOT NULL DEFAULT 'jcode'` to `sessions`. `sessions.backend` is the
thread's **active** backend; on recovery the router reads it to pick the
`Backend` for `SubscribeExisting`. The `jcode_session` column (and its
`idx_sessions_jcode` index) keep their names and generically hold the **active**
backend's session id — for claude, the `system/init` UUID passed to `--resume`.

### New: per-backend session ids (`thread_backend_sessions`)

Reversible switching (§Agent switching) needs each backend's session id retained
per thread, not just the active one. A new migration adds:

```sql
CREATE TABLE IF NOT EXISTS thread_backend_sessions (
    channel_id     TEXT NOT NULL,
    thread_ts      TEXT NOT NULL,
    backend        TEXT NOT NULL,         -- "jcode" | "claude" | …
    session_id     TEXT NOT NULL,         -- backend-native id (claude UUID, jcode session_…)
    last_active_at INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (channel_id, thread_ts, backend),
    FOREIGN KEY (channel_id, thread_ts)
        REFERENCES sessions(channel_id, thread_ts) ON DELETE CASCADE
);
```

> **`ON DELETE CASCADE` is required** (codex P1). `foreign_keys=ON` is set, and
> `DeleteSession` deletes the parent `sessions` row directly (it relies on
> `turn_queue` being empty at deletion time, which is not true for
> `thread_backend_sessions` — it holds a row per backend ever used in the
> thread). Without `CASCADE`, stale-session recovery's `DeleteSession` would
> fail the FK constraint. With it, the per-backend rows are removed atomically
> when the session is deleted.

- On first turn under a backend: insert its `(thread, backend, session_id)` row.
- On switch: set `sessions.backend` (+ mirror the new active id into
  `sessions.jcode_session` for the existing recovery path), and upsert the
  incoming backend's row, bumping `last_active_at`.
- On switch-back / recovery: look up `(thread, active_backend)` →
  `SubscribeExisting(session_id)` → `--resume`.
- Idle eviction tears down the *process* but leaves the row intact; the id is
  the resume handle.

**Migration versioning.** This is the next free `user_version` above the
in-flight cron (`V5`) and reaction (`V6`) migrations — i.e. **V7** once those
land (renumber if merge order differs). Per the repo's established convention it
ships with an idempotent self-heal backstop (`ensureThreadBackendSessionsTable`)
that runs unconditionally in `migrate()`, mirroring `ensureBackendColumn` /
`ensureCronLastFiredTable` / `ensureMessageTSColumn`. Coordinate the number with
those PRs before merge.

> Note: the `sessions.jcode_session` mirror of the active id is kept so the
> existing restart-recovery path needs no change; `thread_backend_sessions` is
> additive. A later cleanup could make `thread_backend_sessions` the sole source
> of truth and drop the mirror, but that is out of scope here.

## Agent switching

The user can hand a thread to another agent mid-conversation in natural language
— "build this feature, then use codex to review it" — and later return. This is
now an in-scope design concern (promoted from BACKLOG). The switch must be
**reversible**: returning to a prior backend resumes *that backend's own*
conversation, warm, where it left off.

### Session identity: one session id per backend per thread

A thread is a `(channel_id, thread_ts)`. Each **backend** that has ever taken a
turn in that thread owns its own session id:

- claude's session id is the `system/init` UUID (passed to `--resume`).
- jcode's session id is its `session_<animal>_<digits>` id.
- a future codex backend would have its own.

These are **distinct conversations** that happen to share a Slack thread. The
thread's **active** backend (the one that takes the next turn) is one of them;
the others are dormant but retained, so a switch back is a resume, not a fresh
start. Persistence is described in §Store.

### Process ownership lives inside the backend

The router does **not** hold a map of subprocesses. Each backend owns its
process lifecycle entirely, keyed by its own session id (claude's
`map[sessionID]*session` guarded by a mutex; jcode's existing per-session socket
map). The router's vocabulary is only: *which backend + which session id takes
the next turn.* This keeps process concerns (spawn, warm-hold, setpgid kill,
`--resume` rehydrate, idle eviction) encapsulated where the stdio contract
lives, and keeps the router a pure dispatcher. It also means the close-once
event-channel contract is enforced in one place per backend.

### Switch timing

A switch takes effect **at a turn boundary**, never mid-turn. The router applies
a pending switch only after the current turn reaches a terminal event
(`TurnDone` / `TurnError` / `Interrupted`). Consequences:

- The outgoing session's event channel is **quiescent** at switch time (its last
  turn already finalized), so there is no half-rendered turn to reconcile.

- **The switch carries the task forward** (codex P1). An instruction like "build
  this feature, then **use codex to review it**" has two parts: the work for the
  current agent, and a *residual task for the new agent*. The natural-language
  switch detector extracts both: the pre-switch portion runs under backend A;
  the post-switch portion ("review it") is captured as the **first prompt for
  backend B**. At the turn boundary the router (a) activates B (`Subscribe` if B
  has no session id for this thread yet, else `SubscribeExisting`+`--resume` of
  B's stored id) and (b) **immediately `SendMessage`s the residual task** to B —
  it does **not** sit idle waiting for the user to re-state it. If the
  instruction has no residual task (a bare "switch to codex"), then there is no
  auto-prompt and B simply becomes active for the user's next message.

- A switch instruction arriving mid-turn is queued and applied at the next
  boundary; the user sees the current turn finish under the old agent first,
  then the new agent pick up the forwarded task.

### Inactive (dormant) backend handling

When backend A becomes dormant, its warm subprocess is **not** closed. Between
turns a claude process blocks on stdin and emits nothing, so:

- **The router keeps consuming the channel and filters** (codex P1). The earlier
  draft had the backend "drain dormant events," but the router's consumer stays
  attached to the channel and the backend has no activate/deactivate signal — so
  the backend can't know to drain. Resolution: the **router** keeps ranging over
  every live session's channel (active and dormant) and simply **does not render
  dormant sessions' events to `coalesce`** — it discards them. This needs no new
  interface method and no backpressure risk (the channel is always drained by
  its existing consumer). A dormant claude emits nothing in steady state anyway;
  this just makes the contract implementable as written.
- **Idle tracking needs no signal either.** The backend infers idleness from its
  own `SendMessage` timestamps per session (it sees every send), so the
  idle-eviction timer (below) is backend-internal and does not depend on a
  router-driven activate/deactivate.
- If a dormant subprocess dies (OOM, crash), the backend notes it and clears the
  warm handle but **keeps the persisted session id**. The next time that backend
  is made active, it rehydrates via `SubscribeExisting` → `--resume <session id>`
  exactly like restart recovery.

### Idle eviction

Each backend runs an **idle-eviction timer per session** (config
`idle_eviction_timeout`, default `"30m"`, `0` = never). When a dormant session
exceeds the timeout, the backend tears down the subprocess (SIGTERM-group →
SIGKILL-group) but **retains the persisted session id**. The conversation is not
lost — the next activation rehydrates with `--resume`. This bounds the
steady-state cost: a long-running switchboard holds at most one live subprocess
per *recently active* `(thread, backend)`, not per thread ever conversed with.
Eviction is owned by the backend (process ownership rule), not the router.

### Lifecycle summary

- **Switch away from a backend:** dormant, warm, router-filtered — no teardown.
- **Switch back to a backend:** `SendMessage` to the warm session (no respawn),
  or `SubscribeExisting`+`--resume` if it was idle-evicted or crashed.
- **`CloseSession(id)`** — permanent teardown of **one** session: thread
  archived / logical session abandoned. Closes that channel once, kills that
  process group, deletes its `thread_backend_sessions` row.
- **`Close()`** — teardown of the **whole** backend, at switchboard shutdown.
- **Idle eviction** is a *process* teardown only — **not** `CloseSession`: the
  session id and its row survive for `--resume`. The close-once channel contract
  holds: a session's channel closes exactly once, on `CloseSession` or `Close` —
  switching and idle eviction do not close it.

## Router wiring

- `New()` constructs the jcode adapter always, and the claude backend when
  configured (lazily).
- A `backendFor(channelID) agent.Backend` selector (same shape as the existing
  `channelConfig` helper) resolves the **configured** backend per channel and is
  used by `handleNewSession` and recovery.
- The `jcode *jcode.Client` field becomes `defaultBackend agent.Backend` plus a
  `claudeBackend agent.Backend` (nil when unconfigured); `consumeEvents` switches
  on `agent.Event`.
- **Per-thread active backend.** Beyond the static per-channel default, the
  router tracks each thread's *currently active* backend (in
  `sessions.backend`, see §Store) so a natural-language switch can move a thread
  to a different agent. The router holds **no subprocess handles** — it holds, per
  thread, only `{active backend, the session id to drive next}` and resolves the
  process via the owning backend's `Subscribe`/`SubscribeExisting`/`SendMessage`.
- **Applying a switch** (the agent-switching feature; the parsing of the natural
  -language instruction is its own piece, out of scope here): at the next turn
  boundary, persist the new active backend + session id, then dispatch the next
  user message to the new backend. The previous backend's session is left
  dormant and warm inside that backend per §Agent switching.

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
  - **`--setting-sources project,local` is on the argv** of every spawn (the
    `user` layer is excluded — this is what stops the host SessionStart hooks
    from firing and reproducing the empty-response bug), and the value matches
    the `setting_sources` config knob.
  - **`--bare` is NOT on the argv** (it would disable OAuth/keychain and break
    subscription auth).
  - **`--include-partial-messages` is NOT on the argv** (would be a no-op at
    best; documented as `--print`-only).
  - **`CLAUDECODE` is stripped** from the spawned process env, and **all other
    parent env vars are passed through** (assert a sentinel var survives).
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
  - **`CloseSession(id)` tears down only that session** — its channel closes
    once and its process group is killed, while a second concurrently-open
    session keeps running and its channel stays open. (`Close()` then tears down
    the remaining one.)
  - `SubscribeExisting(uuid, workdir)` spawns with `--resume <uuid>` (cwd =
    workdir) and emits nothing until `SendMessage`. If `--resume` fails
    (subprocess exits with non-zero before any event), the next `SendMessage`
    surfaces `TurnError`.
  - **Process-group cleanup integration test** (`proc_unix_test.go`-style):
    spawn a fake child that itself spawns a grandchild that sleeps 60s;
    `Close` must terminate both. Skipped on non-Unix.
- **`internal/agent/claude/streaming_smoke_test.go`** (new, build-tag-gated;
  not part of the default test pass) — spawns a real `claude` with our flag set
  (`--setting-sources project,local`, minus `--include-partial-messages`). Two
  assertions, both before the terminal `result`:
  1. A short text-only prompt produces at least one `content_block_delta`
     with `text_delta`.
  2. A prompt that provokes a tool call (e.g. "list files in `/tmp`")
     produces at least one `content_block_delta` with `input_json_delta`.
  Together these validate that base stream-json gives us live deltas for
  **both** text and tool input without `--include-partial-messages`. Gated
  behind a build tag because it needs a real binary and a real model.
- **`internal/agent/claude/subscription_smoke_test.go`** (new, build-tag-gated)
  — the load-bearing end-to-end guard for this revision. It must run against the
  operator's **Claude subscription**, so the test:
  1. **Asserts the env has no `ANTHROPIC_API_KEY`** (skips with a clear message
     if one is set, so it can't accidentally pass on API-key billing), then
     spawns claude with the real flag set under OAuth/keychain.
  2. **Multi-turn:** sends turn 1 ("remember the number 42"), waits for
     `TurnDone`; sends turn 2 ("what number did I say?"), asserts the reply
     references 42 — proving the persistent process keeps context across turns
     **and** that OAuth auth actually worked (a non-empty `text` response, i.e.
     the empty-response bug is gone with `--setting-sources` instead of
     `--bare`).
  3. **Handover round-trip:** records claude's session id, simulates a switch
     away (stop driving claude; the session goes dormant, not closed) and back
     via `SubscribeExisting(session_id)` → `--resume`, then sends turn 3 ("what
     number again?") and asserts it still answers 42 — proving the per-backend
     session id resumes the *same* conversation.
  Gated behind a build tag (needs a real subscription-authenticated binary);
  run before tagging the release.
- **`internal/agent/claude/compat_test.go`** — version-range parsing
  (`min_version`/`max_version` accept/reject); and the **two-stage** probe
  (§CLI compatibility, probe #2): malformed/absent `system/init` → `TurnError`
  "(init)"; and a first turn whose stream-event envelope never arrives well-formed
  within the timeout → `TurnError` "(stream)". Ordinary unit tests (fake exec
  seam, no real binary), including a timeout case driven by a fake clock.
- **`internal/agent/claude/permission_test.go`** — `AllowAll.Decide` returns
  `{allow, input}`; `DenyAll.Decide` returns `{deny, "…"}`;
  `AcceptEditsOnly.Decide` returns allow for Edit/Write/NotebookEdit/MultiEdit
  and deny for Bash.
- **Router selector test** — channel/global backend resolution (mirrors the
  `usePerThreadWorkdir` table tests). Already exists; recovery uses the active
  backend + its stored session id.
- **Store** — `migrateV4` test already exists (merged in `d28a85d`). Add a
  `thread_backend_sessions` migration test (table created; upsert/lookup
  round-trips a per-backend session id; self-heal backstop restores the table
  when `user_version` is advanced past it — same shape as the existing
  `ensureBackendColumn` self-heal test).

## Delivery plan

PR A (backend abstraction) and the initial PR B (subprocess-per-turn claude
backend) **are already merged** as `4526aa0` and `d28a85d`. This revision is
delivered as **two** follow-up PRs so the backend rewrite is reviewable
independently of the cross-backend switching feature.

**PR 1 — Claude backend rewrite (subprocess-per-turn → one persistent
process), auth, env, CLI compatibility:**
1. Config: add `permission_policy`, `append_system_prompt`, `setting_sources`
   (default `"project,local"`), `graceful_stop_timeout`, `idle_eviction_timeout`,
   `min_version`/`max_version`, `extra_env` (as `map[string]string`).
   Backwards-compat translation of legacy `permission_mode` per §Backwards-compat.
2. `internal/agent/claude/backend.go` — rewrite for long-running process:
   `--setting-sources project,local` (no `--bare`), spawn-once, stdin-held-open,
   setpgid + group-kill, **env inherited in full minus `CLAUDECODE`**,
   `--resume <uuid>` recovery using the UUID stored in `sessions.jcode_session`,
   and the per-session **idle-eviction timer** (process owned by the backend).
3. `internal/agent/claude/permission.go` — new file; `AllowAll` default,
   `control_request` handler wired in `backend.go`. Deny-shape default-message
   fallback.
4. `internal/agent/claude/proc_unix.go` (+ `proc_windows.go` stub) — setpgid
   spawn + group-kill helpers (Windows TBD; build-tag scoped).
5. `internal/agent/claude/compat.go` — `claude --version` parse + `min/max`
   range check (startup); `system/init` shape probe → `TurnError` on mismatch.
6. `internal/agent/claude/backend_test.go` — rewrite; cover the new lifecycle
   including **`--setting-sources`-on-argv, `--bare`-NOT-on-argv**, env
   pass-through with `CLAUDECODE`-stripped, `--resume`-on-respawn, idle eviction
   (process gone, session id retained), and permission allow/deny/default-message.
7. `streaming_smoke_test.go`, `subscription_smoke_test.go`, `compat_test.go` per
   §Testing. Subscription + streaming smoke tests are build-tag-gated; run
   against the pinned CLI before release.
8. **Backend-interface addition:** `CloseSession(ctx, sessionID)` for per-thread
   permanent teardown (codex P2), implemented in both adapters and called by the
   router where it deletes a session today. `Subscribe` / `SubscribeExisting` /
   `SendMessage` / `Cancel` / `Close` signatures otherwise unchanged.

**PR 2 — Agent switching:**
1. Migration: `thread_backend_sessions` (next free `user_version`, **FK with
   `ON DELETE CASCADE`** per codex P1, self-heal backstop) + store accessors
   (upsert / lookup per `(thread, backend)`).
2. Router: track per-thread active backend; apply a pending switch at the next
   turn boundary; resolve the target session id and dispatch to the owning
   backend (`Subscribe` first time, else `SubscribeExisting`+`--resume`). The
   router **consumes every live channel and filters dormant ones** (codex P1) —
   no backend drain signal.
3. Natural-language switch detection ("…use codex to review it") — its own
   contained piece; emits a switch intent **plus the residual task**, which the
   router applies at the boundary by activating the new backend and **sending the
   residual task as its first prompt** (codex P1), not waiting for a new message.
4. Tests: router-level boundary timing; **task-forwarding** (switch intent with a
   residual task auto-sends it to the new backend; bare switch does not);
   dormant-channel filtering; `DeleteSession` cascades `thread_backend_sessions`.
   Plus the PR 1 subscription smoke test's handover round-trip.

> **Risk note — the rewrite is invisible to the router but very visible to
> claude's stdio contract.** Risk concentrates in (a) the control_request stdio
> protocol, (b) process-group cleanup, (c) `--resume` recovery re-attaching with
> a usable `system/init`, and (d) **auth: confirming subscription OAuth survives
> `--setting-sources` (no `--bare`)** — the subscription smoke test is the gate
> for (d). The fake exec seam is the unit-test safety net.

## Decisions captured (from brainstorming and revision)

- **Scope:** full parity.
- **Mechanism:** `claude` CLI subprocess, **one long-running process per
  session**, stdin held open, stream-json on both sides. `--setting-sources
  project,local` (exclude the `user` layer) to skip the host SessionStart hooks
  that caused the empty-response bug — **not** `--bare`, which would disable
  subscription OAuth.
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
- **Process tree:** setpgid + SIGTERM-group → SIGKILL-group.
- **Environment:** inherit the parent env in full; strip only `CLAUDECODE`
  (prevents nested-session detection). No allow-list (it would starve OAuth).
- **Auth:** the operator's Claude **subscription** (OAuth/keychain), preserved by
  *not* using `--bare`. A subscription-authenticated smoke test guards it.
- **Binary validation + CLI compatibility:** validate `claude --version` at every
  startup; enforce a configurable `min_version` (fail-fast if claude selected),
  warn above `max_version`; probe `system/init` shape on first spawn.
- **Agent switching:** reversible, **one session id per backend per thread**
  (persisted in `thread_backend_sessions`, FK `ON DELETE CASCADE`); switches
  apply at a **turn boundary** and **forward the residual task** to the new
  backend as its first prompt; **process ownership lives inside each backend**
  (not a router map); the **router consumes + filters** dormant channels;
  per-thread teardown via **`CloseSession`**, whole-backend via `Close`; **idle
  eviction** tears down the process but keeps the resume id. See §Agent switching.
