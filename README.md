# Muse isolated Discord gateway — ops notes

Built 2026-10-03 from dbc-hbin/dot-gateway @ 6906628 (pristine clone in `src/`).
The Go component is a Discord WSS/REST transport + durable SQLite queue; it
contains no model client. This deployment is fully isolated from the existing
dot deployment: separate DB, state dir, consumer-id, no shared files.

## Layout

- `bin/dot-gateway` — built binary (CGO_ENABLED=0, go1.27.1 in ~/.local/go)
- `env/gateway.env` — non-secret env (0600). **PLACEHOLDER owner/bot IDs.**
- `env/bot-token` — ABSENT. Owner provides the token via a separately approved
  secure setup; file must be 0600, regular file, owned by root (no symlink).
- `state/` — 0700. bridge.sqlite3 + .sock live here. Same UID (root) for daemon & CLI.
- `systemd/discord-gateway.service` — persistent unit copy; installed to
  /etc/systemd/system (ephemeral) by bin/watchdog.sh.
- `logs/gateway.log` — daemon stdout/stderr.
- `CONTROLLER.md` — worker protocol for the inbox hook.

## Runtime pieces

- systemd unit `discord-gateway.service` (WantedBy=default.target, Restart=always).
  Installed + enabled, currently INACTIVE (no token).
- cron `discord-gateway-watchdog` every 10m → runs bin/watchdog.sh:
  reinstalls the unit if /etc lost it, starts the service once the token file exists.
- hook `discord-inbox` every 90s → `next --consumer-id muse-discord-v1 --wait 0`;
  wakes a worker on inbound messages (inflight dedup, 10-min expiry).

## Verified

- `check` → configuration valid, discord_transport proxy, scope owner_only_dm.
- `next --wait 2` → {"message":null} on a fresh DB.
- Egress: discord.com/api/v10 → 200, gateway.discord.gg:443 → TLS OK,
  via userinfo-less proxy http://hatch-egress-proxy:3128 (env proxy has userinfo,
  which the gateway rejects — hence BRIDGE_HTTPS_PROXY override).
- Hook dry-run → silent on empty queue. Watchdog dry-run → waiting_for_token.

## Still needed before go-live (owner)

1. Discord bot application + token → secure setup into `env/bot-token`.
2. Real snowflakes → `env/gateway.env`: DISCORD_OWNER_ID (= DISCORD_ALLOWED_DM_IDS,
   single entry), DISCORD_EXPECTED_BOT_ID. Optional: DISCORD_GUILD_ID +
   DISCORD_GUILD_CHANNEL_ID for a guild channel (mode mention).
3. Then: watchdog starts the daemon automatically; send a test DM to verify
   the full loop (gateway → hook → worker → reply → delivery).

## If the proxy starts requiring auth

BRIDGE_HTTPS_PROXY must stay userinfo-less. Fallback: run a local
CONNECT-chaining proxy on 127.0.0.1 that injects Proxy-Authorization upstream,
and point BRIDGE_HTTPS_PROXY at it.
