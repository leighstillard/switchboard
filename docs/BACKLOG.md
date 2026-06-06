# Backlog

Out-of-scope ideas captured during in-flight work. Not promises — entries here
may be reworked, deferred indefinitely, or dropped.

## Agent identity display

### 1. Context size in the customizable agent name

Today the customizable per-channel agent name is used to show what repo
switchboard is operating in (e.g. "Switchboard Dev"). We want to also surface
the current turn's **context size** in that display string so users can see
how much headroom they have before compaction.

- Needs: a clean way to read context size from the active backend mid-turn
  (claude exposes usage in `message_delta` / `result`; jcode exposes it in
  `usage`-style events).
- Open question: is context size shown as raw tokens, percentage of model
  limit, or a coloured indicator? Probably percentage with a band.
- Touch points: `internal/agent/*` (surface usage on events), Slack post
  rendering (identity string composition).

### 2. Show active agent in the customizable name + per-agent icon

When a thread is handed over to a different agent (claude → codex → claude),
the displayed identity should reflect that — both the name suffix and ideally
the avatar icon should change so the user can tell at a glance who they're
talking to.

- Name: append/swap agent tag in the displayed identity.
- Icon: investigate whether Slack's `icon_url` per-message override is
  sufficient, or if we need to maintain a small icon set per backend.
- Touch points: identity rendering, message post path, possibly config to
  let users supply their own per-agent icons.

## Handover (separate feature, captured here for reference)

In-thread handover to another agent via natural-language instruction in Slack
(not a slash command). Example: "build this feature, then use codex to do a
code review of it."

- Lives one layer above `agent.Backend`. Router decides which backend takes
  the next turn for a given thread.
- Warm claude process stays held in a router-side per-thread map across
  handover. On switchboard restart we use `claude --resume <session_id>`
  with the UUID stored in `sessions.jcode_session` (captured originally from
  `system/init`). Not `--continue` — that's workdir-relative and would
  cross-wire concurrent threads sharing a workdir.
- Idle-timeout eviction of warm sessions (kill the subprocess after N
  minutes of inactivity, rehydrate via `--resume` on next message) is a
  future polish — not needed for the initial handover feature.
- See `docs/superpowers/specs/2026-05-25-claude-code-backend-design.md`
  §"Handover compatibility" for the close-semantics + warm-session
  decisions that are load-bearing for this feature.
