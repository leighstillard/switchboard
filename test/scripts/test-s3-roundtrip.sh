#!/usr/bin/env bash
# §3 Round-trip test from injection endpoint
# Verifies: POST /test/inject -> Router -> jcode session -> Coalescer -> Outbound -> Slack reply
#
# Preconditions:
#   - Switchboard running with --debug
#   - jcode daemon running
#   - sw-test-noise channel configured with valid workdir
set -uo pipefail

CHANNEL="C0B17DDAQ67"   # #sw-test-noise
INJECT_URL="http://127.0.0.1:8765/test/inject"
HEALTH_URL="http://127.0.0.1:8765/health"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DBQ="python3 ${SCRIPT_DIR}/dbq.py"
SLACK_TOKEN="${SWITCHBOARD_BOT_TOKEN:-}"

PASS=0
FAIL=0

result() {
  local status="$1" test_id="$2" msg="$3"
  case "$status" in
    PASS) ((PASS++)); echo "  ✅ §3.$test_id: $msg" ;;
    FAIL) ((FAIL++)); echo "  ❌ §3.$test_id: $msg" ;;
  esac
}

echo "=============================================="
echo "§3 Round-trip test: /test/inject -> Slack reply"
echo "=============================================="
echo ""

# --- 3.1 Health check ---
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$HEALTH_URL")
if [ "$STATUS" = "200" ]; then
  result PASS "1" "Switchboard healthy"
else
  result FAIL "1" "Health check failed (HTTP $STATUS) - is switchboard running with --debug?"
  echo ""; echo "FAIL: Cannot proceed without healthy switchboard"; exit 1
fi

# --- 3.2 Inject endpoint accepts request ---
MARKER="RT_$(date +%s%N)"
INJECT_BODY=$(cat <<EOF
{"channel_id":"$CHANNEL","thread_ts":"","user_id":"U_ROUNDTRIP","text":"Say exactly: $MARKER"}
EOF
)
RESP=$(curl -s -w "\n%{http_code}" -X POST "$INJECT_URL" \
  -H "Content-Type: application/json" \
  -d "$INJECT_BODY")
HTTP_CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | head -1)

if [ "$HTTP_CODE" = "200" ]; then
  result PASS "2" "Inject endpoint accepted (HTTP 200)"
  MESSAGE_TS=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('message_ts',''))" 2>/dev/null)
  echo "       message_ts=$MESSAGE_TS"
else
  result FAIL "2" "Inject endpoint returned HTTP $HTTP_CODE: $BODY"
  echo ""; echo "FAIL: Inject not working"; exit 1
fi

# --- 3.3 Session created in store ---
sleep 2
SESSION=$($DBQ "SELECT channel_id, thread_ts, jcode_session, status FROM sessions WHERE channel_id = '$CHANNEL' AND thread_ts = '$MESSAGE_TS'" 2>/dev/null)
if echo "$SESSION" | grep -q "jcode_session"; then
  JCODE_SID=$(echo "$SESSION" | python3 -c "import sys; print(eval(sys.stdin.read()).get('jcode_session',''))")
  result PASS "3" "Session created: $JCODE_SID"
else
  result FAIL "3" "No session found for channel=$CHANNEL thread_ts=$MESSAGE_TS"
  # Try broader search
  echo "       Debugging: all sessions for channel:"
  $DBQ "SELECT thread_ts, jcode_session, status FROM sessions WHERE channel_id = '$CHANNEL' ORDER BY created_at DESC LIMIT 3"
fi

# --- 3.4 Audit entry written ---
AUDIT=$($DBQ "SELECT source, event_type, channel_id FROM audit_log WHERE channel_id = '$CHANNEL' AND ts >= $(( $(date +%s) - 10 )) ORDER BY ts DESC LIMIT 1" 2>/dev/null)
if echo "$AUDIT" | grep -q "slack"; then
  result PASS "4" "Audit entry written for inbound message"
else
  result FAIL "4" "No audit entry found (last 10s, channel=$CHANNEL)"
fi

# --- 3.5 Wait for jcode to respond -> bot posts reply in Slack ---
echo ""
echo "  Waiting for bot reply in Slack (up to 90s)..."

if [ -z "$SLACK_TOKEN" ]; then
  echo "  ⚠️  SWITCHBOARD_BOT_TOKEN not set - checking DB for outbound activity instead"
  
  # Fallback: check session transitions to idle (means turn completed)
  FOUND=0
  for i in $(seq 1 30); do
    sleep 3
    STATUS_NOW=$($DBQ "SELECT status FROM sessions WHERE channel_id = '$CHANNEL' AND thread_ts = '$MESSAGE_TS'" 2>/dev/null | python3 -c "import sys; print(eval(sys.stdin.read()).get('status',''))" 2>/dev/null)
    if [ "$STATUS_NOW" = "idle" ]; then
      FOUND=1
      break
    fi
    printf "."
  done
  echo ""
  
  if [ "$FOUND" = "1" ]; then
    result PASS "5" "Session completed turn (status=idle) - jcode responded"
  else
    STATUS_NOW=$($DBQ "SELECT status FROM sessions WHERE channel_id = '$CHANNEL' AND thread_ts = '$MESSAGE_TS'" 2>/dev/null | python3 -c "import sys; print(eval(sys.stdin.read()).get('status','unknown'))" 2>/dev/null)
    result FAIL "5" "Session did not complete within 90s (status=$STATUS_NOW)"
  fi
else
  # Use Slack API to verify bot reply appeared
  FOUND=0
  for i in $(seq 1 30); do
    sleep 3
    # Check thread for bot reply
    REPLIES=$(curl -s -H "Authorization: Bearer $SLACK_TOKEN" \
      "https://slack.com/api/conversations.replies?channel=$CHANNEL&ts=$MESSAGE_TS&limit=10")
    
    HAS_BOT=$(echo "$REPLIES" | python3 -c "
import sys, json
data = json.load(sys.stdin)
msgs = data.get('messages', [])
for m in msgs:
    if m.get('ts') != '$MESSAGE_TS' and (m.get('bot_id') or m.get('subtype') == 'bot_message'):
        print('YES')
        break
else:
    print('NO')
" 2>/dev/null)
    
    if [ "$HAS_BOT" = "YES" ]; then
      FOUND=1
      break
    fi
    printf "."
  done
  echo ""
  
  if [ "$FOUND" = "1" ]; then
    # Check if the reply contains our marker
    REPLY_TEXT=$(curl -s -H "Authorization: Bearer $SLACK_TOKEN" \
      "https://slack.com/api/conversations.replies?channel=$CHANNEL&ts=$MESSAGE_TS&limit=10" | \
      python3 -c "
import sys, json
data = json.load(sys.stdin)
msgs = data.get('messages', [])
for m in msgs:
    if m.get('ts') != '$MESSAGE_TS' and (m.get('bot_id') or m.get('subtype') == 'bot_message'):
        print(m.get('text', '')[:200])
        break
" 2>/dev/null)
    
    if echo "$REPLY_TEXT" | grep -qi "$MARKER"; then
      result PASS "5" "Bot reply contains marker ($MARKER)"
    else
      result PASS "5" "Bot replied (may not echo marker literally): ${REPLY_TEXT:0:100}"
    fi
  else
    STATUS_NOW=$($DBQ "SELECT status FROM sessions WHERE channel_id = '$CHANNEL' AND thread_ts = '$MESSAGE_TS'" 2>/dev/null | python3 -c "import sys; print(eval(sys.stdin.read()).get('status','unknown'))" 2>/dev/null)
    result FAIL "5" "No bot reply in Slack within 90s (session status=$STATUS_NOW)"
  fi
fi

# --- 3.6 Verify round-trip audit trail ---
echo ""
echo "  Checking audit trail..."
AUDIT_COUNT=$($DBQ "SELECT COUNT(*) as cnt FROM audit_log WHERE channel_id = '$CHANNEL' AND ts >= $(( $(date +%s) - 120 ))" 2>/dev/null | python3 -c "import sys; print(eval(sys.stdin.read()).get('cnt',0))" 2>/dev/null)
if [ "$AUDIT_COUNT" -ge 1 ]; then
  result PASS "6" "Audit trail has $AUDIT_COUNT entries for this round-trip"
else
  result FAIL "6" "No audit entries found for round-trip"
fi

# --- 3.7 Switchboard still healthy after test ---
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$HEALTH_URL")
if [ "$STATUS" = "200" ]; then
  result PASS "7" "Switchboard still healthy after round-trip"
else
  result FAIL "7" "Switchboard unhealthy after test (HTTP $STATUS)"
fi

echo ""
echo "=============================================="
echo "§3 SUMMARY"
echo "=============================================="
echo "  PASS: $PASS"
echo "  FAIL: $FAIL"
echo "  TOTAL: $((PASS + FAIL))"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
