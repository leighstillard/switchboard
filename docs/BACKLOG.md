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

## Handover / agent switching — PROMOTED to the spec (2026-06-07)

In-thread handover to another agent via natural-language instruction ("build
this feature, then use codex to review it") is **no longer backlog** — it is now
designed in `docs/superpowers/specs/2026-05-25-claude-code-backend-design.md`
§"Agent switching": reversible switching, one session id per backend per thread
(persisted in `thread_backend_sessions`), switch-at-turn-boundary timing,
dormant-session draining, **process ownership inside the backend** (not a
router-side map), and idle eviction (all specified, not deferred). Delivery is
PR 2 in that spec's delivery plan. Kept here only as a pointer. Note: uses
`--resume <session_id>`, not `--continue` (which is workdir-relative and would
cross-wire concurrent threads sharing a workdir).
