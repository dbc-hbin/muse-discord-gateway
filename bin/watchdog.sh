#!/bin/bash
# Watchdog for the Muse isolated Discord gateway.
# - (Re)installs the systemd unit from the persistent copy (survives VM replacement).
# - Starts the gateway only when the owner-provided token file exists.
# - Never prints secrets.
set -u
GW=/home/hatch/workspace/discord-gateway
UNIT=discord-gateway.service
SRC="$GW/systemd/$UNIT"
DST="/etc/systemd/system/$UNIT"
TOKEN_FILE="$GW/env/bot-token"

if [ ! -f "$DST" ] || ! cmp -s "$SRC" "$DST"; then
  cp "$SRC" "$DST"
  systemctl daemon-reload
  systemctl enable "$UNIT" >/dev/null 2>&1
  echo "unit (re)installed"
fi

if [ ! -f "$TOKEN_FILE" ]; then
  echo "waiting_for_token"
  exit 0
fi

if ! systemctl is-active --quiet "$UNIT"; then
  if systemctl start "$UNIT"; then
    echo "service started"
  else
    echo "service start FAILED"
    exit 1
  fi
else
  echo "service active"
fi
