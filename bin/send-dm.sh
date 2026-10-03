#!/bin/bash
# Proactive outbound DM via Discord REST API (the gateway itself is inbound-only).
# Usage: send-dm.sh --text "message" | --text-file /path/to/file
# Reads the bot token from the 0600 token file; never prints it.
set -u
GW=/home/hatch/workspace/discord-gateway
TOKEN_FILE="$GW/env/bot-token"
CHANNEL_CACHE="$GW/state/dm-channel-id"

set -a
# shellcheck disable=SC1091
. "$GW/env/gateway.env"
set +a
OWNER="${DISCORD_OWNER_ID:?owner id not configured}"

TEXT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --text) TEXT="$2"; shift 2 ;;
    --text-file) TEXT="$(cat "$2")"; shift 2 ;;
    *) echo "usage: send-dm.sh --text TEXT | --text-file FILE" >&2; exit 2 ;;
  esac
done
[ -n "$TEXT" ] || { echo "empty text" >&2; exit 2; }
[ -f "$TOKEN_FILE" ] || { echo "token file missing" >&2; exit 1; }

TOKEN="$(cat "$TOKEN_FILE")"
AUTH="Authorization: Bot $TOKEN"

channel_id() {
  if [ -f "$CHANNEL_CACHE" ]; then
    cat "$CHANNEL_CACHE"
    return
  fi
  local id
  id=$(curl -s --max-time 20 -X POST "https://discord.com/api/v10/users/@me/channels" \
    -H "$AUTH" -H "Content-Type: application/json" \
    -d "{\"recipient_id\":\"$OWNER\"}" | jq -r '.id // empty')
  [ -n "$id" ] || { echo "dm channel creation failed" >&2; exit 1; }
  printf '%s' "$id" > "$CHANNEL_CACHE"
  chmod 600 "$CHANNEL_CACHE"
  printf '%s' "$id"
}

CH="$(channel_id)"

# Split into 2000-char chunks (Discord limit).
i=0
while [ $i -lt ${#TEXT} ]; do
  chunk="${TEXT:$i:2000}"
  resp=$(curl -s --max-time 20 -X POST "https://discord.com/api/v10/channels/$CH/messages" \
    -H "$AUTH" -H "Content-Type: application/json" \
    -d "$(jq -n --arg t "$chunk" '{content:$t}')")
  mid=$(echo "$resp" | jq -r '.id // empty')
  if [ -z "$mid" ]; then
    echo "send failed: $(echo "$resp" | jq -c '{code,message}' 2>/dev/null || echo "$resp" | head -c 200)" >&2
    exit 1
  fi
  [ $i -eq 0 ] && echo "sent message_id=$mid"
  i=$((i + 2000))
  [ $i -lt ${#TEXT} ] && sleep 1
done
exit 0
