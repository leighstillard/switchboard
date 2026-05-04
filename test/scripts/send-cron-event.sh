#!/usr/bin/env bash
# send-cron-event.sh - Send a test cron webhook to Switchboard with HMAC signing.
#
# Usage:
#   ./send-cron-event.sh [message]
#
# Examples:
#   ./send-cron-event.sh                        # default test event
#   ./send-cron-event.sh "nightly backup done"  # custom message

set -euo pipefail

SWITCHBOARD_URL="${SWITCHBOARD_URL:-http://127.0.0.1:8765}"
CRON_SECRET="${CRON_WEBHOOK_SECRET:-}"

MESSAGE="${1:-Cron test event triggered at $(date -u +%Y-%m-%dT%H:%M:%SZ)}"
TIMESTAMP=$(date +%s%3N)  # milliseconds
IDEM_KEY="cron-test-$(date +%s)-$$"

BODY=$(cat <<EOF
{
  "event_type": "cron_test",
  "message": "$MESSAGE",
  "source_host": "$(hostname)",
  "triggered_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
)

# Compute HMAC signature if secret is set.
HEADERS=(-H "Content-Type: application/json"
         -H "X-Switchboard-Idempotency-Key: $IDEM_KEY"
         -H "X-Switchboard-Timestamp: $TIMESTAMP")

if [ -n "$CRON_SECRET" ]; then
  SIGNATURE=$(echo -n "${TIMESTAMP}.${BODY}" | openssl dgst -sha256 -hmac "$CRON_SECRET" | awk '{print $NF}')
  HEADERS+=(-H "X-Switchboard-Signature: $SIGNATURE")
  echo "Signing with HMAC-SHA256"
else
  echo "Warning: CRON_WEBHOOK_SECRET not set, sending unsigned"
fi

echo "Sending cron event to $SWITCHBOARD_URL/webhook/cron"
echo "  Idempotency key: $IDEM_KEY"
echo "  Message: $MESSAGE"
echo ""

RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" -X POST \
  "$SWITCHBOARD_URL/webhook/cron" \
  "${HEADERS[@]}" \
  -d "$BODY")

HTTP_STATUS=$(echo "$RESPONSE" | grep "HTTP_STATUS:" | cut -d: -f2)
BODY_RESPONSE=$(echo "$RESPONSE" | grep -v "HTTP_STATUS:")

echo "Response: $HTTP_STATUS"
echo "$BODY_RESPONSE"

if [ "$HTTP_STATUS" = "202" ]; then
  echo "OK: webhook accepted"
else
  echo "FAIL: unexpected status $HTTP_STATUS"
  exit 1
fi
