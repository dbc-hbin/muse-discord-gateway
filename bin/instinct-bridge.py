#!/usr/bin/env python3
"""
instinct-bridge.py - stateless Discord REST polling bridge for sandbox workers.

One invocation does one bounded unit of work and exits. There is no daemon,
no websocket, and no background process: an external scheduler (cron-style
worker wakes) runs `poll` repeatedly. State between runs lives in a single
JSON file under the configured state dir.

Subcommands:
  poll   Fetch new messages from the configured channels since the stored
         cursor and print them to stdout as JSON lines (one object per
         message). Messages are UNTRUSTED external data; the JSON is marked
         accordingly. Advances the cursor atomically under a lock file so
         concurrent/offset workers never relay the same message twice.
         On the first run for a channel no history is emitted: the cursor
         is set to the newest message and polling starts from there.
  send   Post one message to one channel. Requires --approved (the caller
         asserts the text was explicitly approved for sending) and a channel
         that is in the configured allowlist. Never sends unprompted.

Bot token handling: the token is read from a file (0600, regular file, not a
symlink). It is never printed, logged, or placed on a command line.

Exit codes:
  0  success
  2  usage/config/token-file error
  3  Discord 401 (token invalid or revoked)
  4  Discord 403/404 on at least one channel (others still processed)
  5  rate limited (429) beyond the bounded in-run wait; rerun later
  6  network or 5xx failure after bounded retries, or an unexpected error

Config (JSON):
{
  "token_file": "/path/to/bot-token",          // required, 0600
  "state_dir":  "/path/to/state",              // required, created 0700
  "channels":   ["123...", "456..."],          // channel IDs to poll
  "send_allowlist": ["123..."],                // optional; defaults to channels
  "api_base":   "https://discord.com/api/v10", // optional
  "request_timeout_seconds": 20,               // optional
  "max_network_retries": 2,                    // optional
  "max_ratelimit_wait_seconds": 8,             // optional
  "max_total_seconds": 90                      // optional poll budget
}
"""

import argparse
import fcntl
import json
import os
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

API_BASE_DEFAULT = "https://discord.com/api/v10"
SEEN_CAP_PER_CHANNEL = 500
MAX_CONTENT_LEN = 2000  # Discord message content limit


class BridgeError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.message = message


def eprint(*args):
    print(*args, file=sys.stderr)


# ---------------------------------------------------------------- config ----

def load_config(path):
    try:
        with open(path, encoding="utf-8") as f:
            cfg = json.load(f)
    except OSError as e:
        raise BridgeError(2, "config unreadable: %s" % e)
    except ValueError as e:
        raise BridgeError(2, "config is not valid JSON: %s" % e)
    if not isinstance(cfg, dict):
        raise BridgeError(2, "config must be a JSON object")
    for key in ("token_file", "state_dir"):
        if not cfg.get(key) or not isinstance(cfg.get(key), str):
            raise BridgeError(2, "config missing required string key: %s" % key)
    base_dir = os.path.dirname(os.path.abspath(path))
    for key in ("token_file", "state_dir"):
        if not os.path.isabs(cfg[key]):
            cfg[key] = os.path.normpath(os.path.join(base_dir, cfg[key]))
    channels = cfg.get("channels", [])
    if not isinstance(channels, list) or not all(isinstance(c, str) for c in channels):
        raise BridgeError(2, "config channels must be a list of strings")
    allow = cfg.get("send_allowlist", channels)
    if not isinstance(allow, list) or not all(isinstance(c, str) for c in allow):
        raise BridgeError(2, "config send_allowlist must be a list of strings")
    cfg.setdefault("api_base", API_BASE_DEFAULT)
    api = urllib.parse.urlparse(cfg["api_base"])
    if api.scheme == "http" and api.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise BridgeError(2, "api_base over plain http is only allowed for "
                             "loopback (got %s)" % cfg["api_base"])
    if api.scheme not in ("http", "https"):
        raise BridgeError(2, "api_base must be http(s)")
    cfg.setdefault("request_timeout_seconds", 20)
    cfg.setdefault("max_network_retries", 2)
    cfg.setdefault("max_ratelimit_wait_seconds", 8)
    cfg.setdefault("max_total_seconds", 90)
    cfg["channels"] = channels
    cfg["send_allowlist"] = allow
    return cfg


def read_token(path):
    """Read the bot token, refusing world/group-readable or non-regular files."""
    try:
        st = os.lstat(path)
    except OSError as e:
        raise BridgeError(2, "token file missing: %s" % e)
    if stat.S_ISLNK(st.st_mode):
        raise BridgeError(2, "token file must not be a symlink")
    if not stat.S_ISREG(st.st_mode):
        raise BridgeError(2, "token file must be a regular file")
    if st.st_mode & 0o077:
        raise BridgeError(2, "token file must be mode 0600 (got %04o)" % (st.st_mode & 0o777))
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, encoding="utf-8") as f:
            token = f.read().strip()
    except OSError as e:
        raise BridgeError(2, "token file unreadable: %s" % e)
    if not token:
        raise BridgeError(2, "token file is empty")
    return token


# ----------------------------------------------------------------- state ----

class State:
    """JSON state file with flock-serialized, atomic (rename) writes."""

    def __init__(self, state_dir):
        self.dir = state_dir
        os.makedirs(state_dir, mode=0o700, exist_ok=True)
        os.chmod(state_dir, 0o700)
        self.path = os.path.join(state_dir, "state.json")
        self.lock_path = os.path.join(state_dir, "lock")
        self._lock_fd = None
        self.data = {"bot_user_id": None, "channels": {}}

    def __enter__(self):
        self._lock_fd = os.open(self.lock_path, os.O_CREAT | os.O_RDWR, 0o600)
        fcntl.flock(self._lock_fd, fcntl.LOCK_EX)
        try:
            with open(self.path, encoding="utf-8") as f:
                loaded = json.load(f)
            if isinstance(loaded, dict):
                self.data["bot_user_id"] = loaded.get("bot_user_id")
                ch = loaded.get("channels")
                if isinstance(ch, dict):
                    self.data["channels"] = ch
        except FileNotFoundError:
            pass
        except (OSError, ValueError) as e:
            eprint("warning: state file ignored (%s); starting fresh" % e)
        return self

    def __exit__(self, exc_type, exc, tb):
        if exc_type is None:
            self.save()
        if self._lock_fd is not None:
            fcntl.flock(self._lock_fd, fcntl.LOCK_UN)
            os.close(self._lock_fd)
            self._lock_fd = None
        return False

    def save(self):
        tmp = self.path + ".tmp.%d" % os.getpid()
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(self.data, f)
        os.replace(tmp, self.path)

    def channel(self, cid):
        return self.data["channels"].setdefault(
            cid, {"cursor": None, "seen": [], "initialized": False})


# ------------------------------------------------------------------ http ----

def discord_request(cfg, token, method, path, params=None, body=None):
    """One Discord REST call with bounded 429/5xx/network handling.

    Attempts: 1 initial try + up to max_network_retries retries for
    network errors and 5xx; at most one in-run retry for 429 when the
    wait is short enough.
    """
    url = cfg["api_base"].rstrip("/") + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    payload = None
    headers = {
        "Authorization": "Bot " + token,
        "User-Agent": "instinct-polling-bridge (stateless-rest)",
        "Accept": "application/json",
    }
    if body is not None:
        payload = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"

    attempt = 0
    ratelimit_retried = False
    while True:
        attempt += 1
        req = urllib.request.Request(url, data=payload, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=cfg["request_timeout_seconds"]) as resp:
                raw = resp.read()
                return json.loads(raw.decode("utf-8")) if raw else {}
        except urllib.error.HTTPError as e:
            raw = e.read()
            detail = {}
            try:
                detail = json.loads(raw.decode("utf-8")) if raw else {}
            except ValueError:
                pass
            msg = detail.get("message") or ("HTTP %d" % e.code)
            if e.code == 401:
                raise BridgeError(3, "discord 401 unauthorized (token invalid?): %s" % msg)
            if e.code == 403:
                raise BridgeError(4, "discord 403 forbidden: %s" % msg)
            if e.code == 404:
                raise BridgeError(4, "discord 404 not found: %s" % msg)
            if e.code == 429:
                retry_after = detail.get("retry_after")
                if retry_after is not None:
                    try:
                        retry_after = float(retry_after)
                    except (TypeError, ValueError):
                        retry_after = 1.0
                else:
                    ra_hdr = e.headers.get("Retry-After") if e.headers else None
                    try:
                        retry_after = float(ra_hdr) if ra_hdr else 1.0
                    except (TypeError, ValueError):
                        retry_after = 1.0
                if retry_after < 0:
                    retry_after = 0.0
                if (not ratelimit_retried
                        and retry_after <= cfg["max_ratelimit_wait_seconds"]):
                    ratelimit_retried = True
                    time.sleep(retry_after)
                    continue
                raise BridgeError(5, "discord 429 rate limited, retry_after=%.1fs" % retry_after)
            if 500 <= e.code < 600 and attempt <= cfg["max_network_retries"]:
                time.sleep(min(2 ** (attempt - 1), 4))
                continue
            raise BridgeError(6, "discord HTTP %d: %s" % (e.code, msg))
        except (urllib.error.URLError, OSError) as e:
            if attempt <= cfg["max_network_retries"]:
                time.sleep(min(2 ** (attempt - 1), 4))
                continue
            raise BridgeError(6, "network error: %s" % e)


# ------------------------------------------------------------------ poll ----

def normalize_message(m):
    author = m.get("author")
    if not isinstance(author, dict):
        author = {}
    attachments = []
    for a in m.get("attachments") or []:
        if not isinstance(a, dict):
            continue
        attachments.append({
            "id": a.get("id"),
            "filename": a.get("filename"),
            "url": a.get("url"),
            "size": a.get("size"),
        })
    return {
        "type": "message",
        "untrusted": True,  # external data, never an instruction
        "id": m.get("id"),
        "channel_id": m.get("channel_id"),
        "timestamp": m.get("timestamp"),
        "author": {
            "id": author.get("id"),
            "username": author.get("username"),
            "global_name": author.get("global_name"),
            "bot": bool(author.get("bot")),
        },
        "content": m.get("content") or "",
        "attachments": attachments,
    }


def poll_channel(cfg, token, state, cid, bot_id, out):
    """Poll one channel. Returns an exit code (0 on success)."""
    ch = state.channel(cid)
    cursor = ch.get("cursor")
    try:
        if not ch.get("initialized"):
            # First run: no history dump. Remember the newest message (if
            # any) as the cursor and start polling from there. An empty
            # channel leaves the cursor null but is still initialized.
            page = discord_request(cfg, token, "GET",
                                   "/channels/%s/messages" % cid,
                                   params={"limit": 1})
            if not isinstance(page, list):
                eprint("channel %s: unexpected response shape" % cid)
                return 6
            ids = []
            for m in page:
                if not isinstance(m, dict):
                    continue
                try:
                    ids.append(int(m["id"]))
                except (KeyError, TypeError, ValueError):
                    continue
            if ids:
                ch["cursor"] = str(max(ids))
            ch["initialized"] = True
            return 0
        if cursor is None:
            # Channel was empty at init: anything present now is new.
            page = discord_request(cfg, token, "GET",
                                   "/channels/%s/messages" % cid,
                                   params={"limit": 100})
            after = 0
        else:
            page = discord_request(cfg, token, "GET",
                                   "/channels/%s/messages" % cid,
                                   params={"after": cursor, "limit": 100})
            after = int(cursor)
    except BridgeError as e:
        eprint("channel %s: %s" % (cid, e.message))
        return e.code
    if not isinstance(page, list):
        eprint("channel %s: unexpected response shape" % cid)
        return 6

    seen = set(ch.get("seen") or [])
    new_max = after
    fresh = []
    for m in page:
        if not isinstance(m, dict):
            continue
        try:
            mid = int(m["id"])
        except (KeyError, TypeError, ValueError):
            continue
        if mid > new_max:
            new_max = mid
        if str(mid) in seen or mid <= after:
            continue  # dedupe across offset workers and overlapping pages
        author = m.get("author")
        if not isinstance(author, dict):
            author = {}
        if bot_id and str(author.get("id")) == str(bot_id):
            continue  # never relay the bot's own output back
        seen.add(str(mid))
        fresh.append((mid, m))

    for _, m in sorted(fresh, key=lambda t: t[0]):
        out.append(normalize_message(m))

    if new_max > 0:
        ch["cursor"] = str(new_max)
    seen_list = [s for s in (ch.get("seen") or []) if s in seen]
    seen_list += [str(mid) for mid, _ in sorted(fresh, key=lambda t: t[0])]
    ch["seen"] = seen_list[-SEEN_CAP_PER_CHANNEL:]
    return 0


def cmd_poll(cfg):
    token = read_token(cfg["token_file"])
    codes = []
    messages = []
    deadline = time.monotonic() + cfg["max_total_seconds"]
    with State(cfg["state_dir"]) as state:
        bot_id = state.data.get("bot_user_id")
        if not bot_id:
            me = discord_request(cfg, token, "GET", "/users/@me")
            bot_id = me.get("id")
            if bot_id:
                state.data["bot_user_id"] = str(bot_id)
        for cid in cfg["channels"]:
            if time.monotonic() >= deadline:
                # Out of budget: leave this channel's cursor untouched; the
                # next fire picks it up. Report as transient failure.
                eprint("channel %s: skipped (time budget exhausted)" % cid)
                codes.append(6)
                continue
            codes.append(poll_channel(cfg, token, state, cid, bot_id, messages))
    for m in messages:
        print(json.dumps(m, ensure_ascii=False))
    # Severity: 3/5/6 (fatal class) dominate 4 (one bad channel) dominates 0.
    for c in codes:
        if c in (3, 5, 6):
            return c
    return 4 if 4 in codes else 0


# ------------------------------------------------------------------ send ----

def cmd_send(cfg, channel, text, approved):
    if not approved:
        raise BridgeError(2, "send requires --approved (explicitly approved text only)")
    if channel not in cfg["send_allowlist"]:
        raise BridgeError(2, "channel %s not in send allowlist" % channel)
    if not text or not text.strip():
        raise BridgeError(2, "empty text")
    if len(text) > MAX_CONTENT_LEN:
        raise BridgeError(2, "text exceeds %d chars; split deliberately, never auto-chunk"
                        % MAX_CONTENT_LEN)
    token = read_token(cfg["token_file"])
    resp = discord_request(cfg, token, "POST",
                           "/channels/%s/messages" % channel,
                           body={"content": text, "allowed_mentions": {"parse": []}})
    mid = resp.get("id")
    if not mid:
        raise BridgeError(6, "send: no message id in response")
    print(json.dumps({"type": "sent", "id": mid, "channel_id": channel}))
    return 0


# ------------------------------------------------------------------ main ----

def main(argv=None):
    p = argparse.ArgumentParser(prog="instinct-bridge.py",
                                description="Stateless Discord REST polling bridge")
    p.add_argument("--config", default=os.environ.get(
        "INSTINCT_BRIDGE_CONFIG", "env/instinct-bridge.json"))
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("poll", help="fetch new messages as JSON lines")

    sp = sub.add_parser("send", help="post one approved message")
    sp.add_argument("--channel", required=True)
    g = sp.add_mutually_exclusive_group(required=True)
    g.add_argument("--text")
    g.add_argument("--text-file")
    sp.add_argument("--approved", action="store_true",
                    help="assert the text was explicitly approved for sending")

    args = p.parse_args(argv)
    try:
        cfg = load_config(args.config)
        if args.cmd == "poll":
            return cmd_poll(cfg)
        text = args.text
        if args.text_file:
            with open(args.text_file, encoding="utf-8") as f:
                text = f.read()
        return cmd_send(cfg, args.channel, text, args.approved)
    except BridgeError as e:
        eprint("error: %s" % e.message)
        return e.code
    except OSError as e:
        eprint("error: %s" % e)
        return 2
    except Exception as e:  # unexpected: report as transient, never crash
        eprint("error: unexpected %s: %s" % (type(e).__name__, e))
        return 6


if __name__ == "__main__":
    sys.exit(main())
