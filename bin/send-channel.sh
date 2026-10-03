#!/bin/bash
# Post a message to a Discord guild text channel via REST API.
# Usage: send-channel.sh --channel <channel-id> --text "message" | --text-file /path/to/file
# Reads the bot token from the 0600 token file; never prints it.
# The channel must be listed in DISCORD_SEND_ALLOWLIST (gateway.env); anything
# else is refused. The token is passed to curl via a 0600 config file, never argv.
# Long text is split into <=1900-char chunks at newline boundaries.
set -u
GW=/home/hatch/workspace/discord-gateway
TOKEN_FILE="$GW/env/bot-token"

set -a
# shellcheck disable=SC1091
. "$GW/env/gateway.env"
set +a

CHANNEL=""
TEXT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --channel) CHANNEL="$2"; shift 2 ;;
    --text) TEXT="$2"; shift 2 ;;
    --text-file) TEXT="$(cat "$2")"; shift 2 ;;
    *) echo "usage: send-channel.sh --channel ID (--text TEXT | --text-file FILE)" >&2; exit 2 ;;
  esac
done
[ -n "$CHANNEL" ] || { echo "channel id required" >&2; exit 2; }
[ -n "$TEXT" ] || { echo "empty text" >&2; exit 2; }
[ -f "$TOKEN_FILE" ] || { echo "token file missing" >&2; exit 1; }

# Destination allowlist: refuse any channel not explicitly approved, so this
# script can never be pointed at an arbitrary channel the bot can access.
allowed=0
for a in ${DISCORD_SEND_ALLOWLIST:-}; do
  [ "$a" = "$CHANNEL" ] && allowed=1
done
[ $allowed -eq 1 ] || { echo "channel $CHANNEL not in DISCORD_SEND_ALLOWLIST" >&2; exit 2; }

TOKEN=$(cat "$TOKEN_FILE")
PROXY="${BRIDGE_HTTPS_PROXY:-http://hatch-egress-proxy:3128}"

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

# Token goes into a 0600 curl config file, never into argv (visible via ps).
CURL_CONF="$TMPD/curl.conf"
printf 'header = "Authorization: Bot %s"\nheader = "Content-Type: application/json"\n' "$TOKEN" > "$CURL_CONF"
chmod 600 "$CURL_CONF"
unset TOKEN

# Split text into chunks of at most 1900 chars, preferring newline boundaries.
python3 - "$TEXT" "$TMPD" <<'EOF'
import sys, os
text, tmpd = sys.argv[1], sys.argv[2]
chunks, cur = [], ""
for line in text.split("\n"):
    if cur and len(cur) + len(line) + 1 > 1900:
        chunks.append(cur); cur = ""
    cur += ("" if not cur else "\n") + line
if cur.strip():
    chunks.append(cur)
for i, c in enumerate(chunks):
    with open(os.path.join(tmpd, f"chunk_{i:02d}.txt"), "w") as f:
        f.write(c)
print(len(chunks))
EOF

n=0
for f in "$TMPD"/chunk_*.txt; do
  [ -f "$f" ] || { echo "no chunks produced" >&2; exit 1; }
  nonce=$(python3 -c "import secrets; print(secrets.token_hex(12))")
  resp=$(python3 - "$f" "$nonce" <<'EOF'
import json, sys
body = open(sys.argv[1]).read()
print(json.dumps({"content": body, "enforce_nonce": True, "nonce": sys.argv[2],
                  "allowed_mentions": {"parse": []}}))
EOF
)
  out=$(curl -s --max-time 30 -x "$PROXY" -X POST -K "$CURL_CONF" \
    "https://discord.com/api/v10/channels/$CHANNEL/messages" \
    -d "$resp")
  mid=$(printf '%s' "$out" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null)
  if [ -z "$mid" ]; then
    echo "post failed: $(printf '%s' "$out" | head -c 300)" >&2
    exit 1
  fi
  echo "sent chunk $n message_id=$mid"
  n=$((n+1))
done
