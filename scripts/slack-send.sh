#!/usr/bin/env bash
# slack-send.sh - Send a message to a Slack channel via Claude.ai Slack MCP.
# Messages are sent as the authenticated user (Leigh).
#
# Usage: ./scripts/slack-send.sh <channel_id> <text>
# Example: ./scripts/slack-send.sh C0B0Y0WQYQP "<@U0B1FNCSWP6> hello"

set -euo pipefail

CHANNEL="${1:?Usage: slack-send.sh <channel_id> <text>}"
TEXT="${2:?Usage: slack-send.sh <channel_id> <text>}"

echo "" | claude \
  -p "Call mcp__claude_ai_Slack__slack_send_message with channel_id ${CHANNEL} and text '${TEXT}'" \
  --allowedTools 'mcp__claude_ai_Slack__slack_send_message' \
  --permission-mode bypassPermissions \
  --max-turns 4 \
  --output-format text \
  --append-system-prompt "You are a test tool. Just call the MCP tool with the exact parameters given. No commentary." \
  2>&1
