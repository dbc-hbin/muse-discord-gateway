#!/usr/bin/env python3
"""
Offline tests for bin/instinct-bridge.py against a mock Discord HTTP server.
No real Discord calls, no real tokens.
"""

import json
import os
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

HERE = os.path.dirname(os.path.abspath(__file__))
BRIDGE = os.path.join(HERE, "instinct-bridge.py")
BOT_ID = "900000000000000001"


class MockDiscord:
    """Scriptable in-process mock of the Discord REST API."""

    def __init__(self):
        self.messages = {}       # channel_id -> list of message dicts (ascending ids)
        self.get_scripts = {}    # (method, path-prefix) -> list of (status, body) to play first
        self.posts = []          # recorded POST bodies
        self.counts = {}         # (method, path) -> request count

    def add_message(self, channel_id, mid, content, author_id="1234", bot=False):
        msg = {
            "id": str(mid),
            "channel_id": str(channel_id),
            "timestamp": "2026-11-24T01:00:00.000000+00:00",
            "content": content,
            "author": {"id": str(author_id), "username": "user%s" % author_id,
                       "global_name": None, "bot": bot},
            "attachments": [],
        }
        self.messages.setdefault(str(channel_id), []).append(msg)
        self.messages[str(channel_id)].sort(key=lambda m: int(m["id"]))
        return msg

    def script(self, method, path_prefix, status, body):
        self.get_scripts.setdefault((method, path_prefix), []).append((status, body))

    # -- request handling ------------------------------------------------

    def handle(self, handler, method):
        parsed = urlparse(handler.path)
        path = parsed.path
        qs = parse_qs(parsed.query)
        self.counts[(method, path)] = self.counts.get((method, path), 0) + 1

        for (m, prefix), scripts in list(self.get_scripts.items()):
            if m == method and path.startswith(prefix) and scripts:
                status, body = scripts.pop(0)
                return status, body

        if method == "GET" and path == "/api/v10/users/@me":
            if handler.headers.get("Authorization") != "Bot test-token":
                return 401, {"message": "401: Unauthorized", "code": 0}
            return 200, {"id": BOT_ID, "username": "bridgebot", "bot": True}

        if path.startswith("/api/v10/channels/") and path.endswith("/messages"):
            cid = path.split("/")[4]
            if handler.headers.get("Authorization") != "Bot test-token":
                return 401, {"message": "401: Unauthorized", "code": 0}
            if cid == "forbidden":
                return 403, {"message": "Missing Access", "code": 50001}
            if method == "GET":
                msgs = self.messages.get(cid, [])
                after = qs.get("after", [None])[0]
                limit = int(qs.get("limit", ["50"])[0])
                if after is not None:
                    msgs = [m for m in msgs if int(m["id"]) > int(after)]
                else:
                    msgs = msgs[-limit:]
                return 200, msgs[:limit] if after is not None else msgs
            if method == "POST":
                length = int(handler.headers.get("Content-Length", 0))
                body = json.loads(handler.rfile.read(length).decode("utf-8"))
                self.posts.append((cid, body))
                new_id = str(max([int(m["id"]) for m in self.messages.get(cid, [])] + [1000]) + 1)
                msg = self.add_message(cid, new_id, body.get("content", ""),
                                       author_id=BOT_ID, bot=True)
                return 200, msg
        return 404, {"message": "404: Not Found", "code": 0}


def make_server(mock):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            self._reply("GET")

        def do_POST(self):
            self._reply("POST")

        def _reply(self, method):
            try:
                status, body = mock.handle(self, method)
            except Exception as e:  # pragma: no cover - defensive
                status, body = 500, {"message": str(e)}
            raw = json.dumps(body).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *args):
            pass

    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv


class BridgeTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = self.tmp.name
        self.token_path = os.path.join(self.dir, "bot-token")
        with open(self.token_path, "w") as f:
            f.write("test-token\n")
        os.chmod(self.token_path, 0o600)
        self.mock = MockDiscord()
        self.srv = make_server(self.mock)
        self.config_path = os.path.join(self.dir, "config.json")

    def tearDown(self):
        self.srv.shutdown()
        self.srv.server_close()
        self.tmp.cleanup()

    def write_config(self, channels, **extra):
        cfg = {
            "token_file": self.token_path,
            "state_dir": os.path.join(self.dir, "state"),
            "channels": channels,
            "api_base": "http://127.0.0.1:%d/api/v10" % self.srv.server_address[1],
            "max_ratelimit_wait_seconds": 2,
            "request_timeout_seconds": 5,
        }
        cfg.update(extra)
        with open(self.config_path, "w") as f:
            json.dump(cfg, f)

    def run_bridge(self, *args):
        return subprocess.run(
            [sys.executable, BRIDGE, "--config", self.config_path] + list(args),
            capture_output=True, text=True, timeout=60)

    def poll_lines(self, proc):
        return [json.loads(l) for l in proc.stdout.splitlines() if l.strip()]

    # ------------------------------------------------------------- tests

    def test_first_poll_initializes_cursor_without_backfill(self):
        self.mock.add_message("c1", 100, "old message")
        self.write_config(["c1"])
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.poll_lines(p), [])
        # second poll with no new messages: still nothing
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.poll_lines(p), [])

    def test_poll_emits_new_messages_ascending_and_advances(self):
        self.mock.add_message("c1", 100, "old")
        self.write_config(["c1"])
        self.run_bridge("poll")  # init cursor
        self.mock.add_message("c1", 101, "first new")
        self.mock.add_message("c1", 102, "second new")
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        lines = self.poll_lines(p)
        self.assertEqual([m["content"] for m in lines], ["first new", "second new"])
        self.assertTrue(all(m["untrusted"] for m in lines))
        self.assertEqual(lines[0]["author"]["username"], "user1234")
        # already-relayed messages are not repeated
        p = self.run_bridge("poll")
        self.assertEqual(self.poll_lines(p), [])

    def test_poll_skips_own_bot_messages(self):
        self.write_config(["c1"])
        self.run_bridge("poll")
        self.mock.add_message("c1", 200, "own reply", author_id=BOT_ID, bot=True)
        self.mock.add_message("c1", 201, "human reply")
        p = self.run_bridge("poll")
        lines = self.poll_lines(p)
        self.assertEqual([m["content"] for m in lines], ["human reply"])

    def test_poll_dedupes_overlapping_pages(self):
        self.write_config(["c1"])
        self.run_bridge("poll")
        self.mock.add_message("c1", 300, "hello")
        p = self.run_bridge("poll")
        self.assertEqual(len(self.poll_lines(p)), 1)
        # simulate a worker whose cursor raced: replay the same page by hand
        state_path = os.path.join(self.dir, "state", "state.json")
        with open(state_path) as f:
            st = json.load(f)
        st["channels"]["c1"]["cursor"] = "299"  # rewind cursor only, seen-set intact
        with open(state_path, "w") as f:
            json.dump(st, f)
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.poll_lines(p), [])  # seen-set dedupes id 300

    def test_401_exits_3(self):
        os.chmod(self.token_path, 0o600)
        with open(self.token_path, "w") as f:
            f.write("wrong-token")
        self.write_config(["c1"])
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 3, p.stderr)

    def test_403_one_channel_others_continue(self):
        self.mock.add_message("ok", 500, "fine")
        self.write_config(["forbidden", "ok"])
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 4, p.stderr)
        self.assertIn("forbidden", p.stderr)
        # healthy channel still initialized and pollable
        self.mock.add_message("ok", 501, "later")
        self.write_config(["ok"])
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual([m["content"] for m in self.poll_lines(p)], ["later"])

    def test_429_short_wait_retries_then_succeeds(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/users/@me", 429,
                         {"message": "rate limited", "retry_after": 0.1})
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_429_long_wait_exits_5(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/users/@me", 429,
                         {"message": "rate limited", "retry_after": 30})
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 5, p.stderr)

    def test_5xx_retries_then_succeeds(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/users/@me", 500, {"message": "boom"})
        self.mock.script("GET", "/api/v10/users/@me", 502, {"message": "boom"})
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_token_file_permissions_enforced(self):
        os.chmod(self.token_path, 0o644)
        self.write_config(["c1"])
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertIn("0600", p.stderr)

    def test_time_budget_skips_channels_without_advancing_cursor(self):
        self.mock.add_message("c1", 700, "later")
        self.write_config(["c1"], max_total_seconds=0)
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 6, p.stderr)
        self.assertIn("time budget", p.stderr)
        # nothing was consumed: a normal poll still treats it as first run
        self.write_config(["c1"])
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.poll_lines(p), [])
        self.mock.add_message("c1", 701, "next")
        p = self.run_bridge("poll")
        self.assertEqual([m["content"] for m in self.poll_lines(p)], ["next"])

    def test_429_negative_retry_after_clamped_and_retried(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/users/@me", 429,
                         {"message": "rate limited", "retry_after": -5})
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_429_unparseable_retry_after_defaults_and_retries(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/users/@me", 429,
                         {"message": "rate limited", "retry_after": "soon"})
        p = self.run_bridge("poll")  # falls back to 1.0s wait, then retries
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_malformed_page_entries_skipped(self):
        self.write_config(["c1"])
        self.run_bridge("poll")  # init on empty channel
        good = self.mock.add_message("c1", 400, "valid")
        page = ["not-a-dict", {"no_id": True}, {"id": "not-a-number"},
                {"id": None}, good]
        self.mock.script("GET", "/api/v10/channels/c1/messages", 200, page)
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        lines = self.poll_lines(p)
        self.assertEqual([m["content"] for m in lines], ["valid"])

    def test_malformed_author_and_attachments_normalized(self):
        self.write_config(["c1"])
        self.run_bridge("poll")
        msg = self.mock.add_message("c1", 410, "odd")
        msg["author"] = "not-a-dict"
        msg["attachments"] = ["bad", {"id": "a1", "filename": "f",
                                      "url": "u", "size": 3}]
        self.mock.script("GET", "/api/v10/channels/c1/messages", 200, [msg])
        p = self.run_bridge("poll")
        lines = self.poll_lines(p)
        self.assertEqual(len(lines), 1)
        self.assertEqual(lines[0]["author"]["id"], None)
        self.assertEqual(len(lines[0]["attachments"]), 1)

    def test_first_run_init_tolerates_malformed_page(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/channels/c1/messages", 200,
                         [{"no_id": True}, "junk"])
        p = self.run_bridge("poll")  # init run
        self.assertEqual(p.returncode, 0, p.stderr)
        # initialized despite no usable id; next poll works normally
        self.mock.add_message("c1", 420, "after init")
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual([m["content"] for m in self.poll_lines(p)], ["after init"])

    def test_first_run_init_rejects_nonlist_page(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/channels/c1/messages", 200,
                         {"not": "a list"})
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 6, p.stderr)

    def test_unexpected_response_type_exits_6(self):
        self.write_config(["c1"])
        self.mock.script("GET", "/api/v10/users/@me", 200, "garbage-string")
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 6, p.stderr)

    def test_network_retry_count_is_bounded(self):
        self.write_config(["c1"])
        for _ in range(3):
            self.mock.script("GET", "/api/v10/users/@me", 500, {"message": "boom"})
        p = self.run_bridge("poll")  # 1 initial + 2 retries = 3 attempts, then fail
        self.assertEqual(p.returncode, 6, p.stderr)
        self.assertEqual(self.mock.counts[("GET", "/api/v10/users/@me")], 3)
        # 2 failures then success: third attempt succeeds
        self.mock.script("GET", "/api/v10/users/@me", 500, {"message": "boom"})
        self.mock.script("GET", "/api/v10/users/@me", 500, {"message": "boom"})
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_plain_http_api_base_refused_off_loopback(self):
        self.write_config(["c1"], api_base="http://192.0.2.1/api/v10")
        p = self.run_bridge("poll")
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertIn("loopback", p.stderr)

    def test_send_success(self):
        self.write_config(["c1"])
        p = self.run_bridge("send", "--channel", "c1", "--text", "approved hello",
                            "--approved")
        self.assertEqual(p.returncode, 0, p.stderr)
        out = self.poll_lines(p)
        self.assertEqual(out[0]["type"], "sent")
        self.assertEqual(self.mock.posts[0][1]["content"], "approved hello")
        self.assertEqual(self.mock.posts[0][1]["allowed_mentions"], {"parse": []})

    def test_send_requires_approved_flag(self):
        self.write_config(["c1"])
        p = self.run_bridge("send", "--channel", "c1", "--text", "hi")
        self.assertEqual(p.returncode, 2)
        self.assertEqual(self.mock.posts, [])

    def test_send_refuses_channel_outside_allowlist(self):
        self.write_config(["c1"], send_allowlist=["c1"])
        p = self.run_bridge("send", "--channel", "c2", "--text", "hi", "--approved")
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertEqual(self.mock.posts, [])

    def test_send_refuses_overlong_text(self):
        self.write_config(["c1"])
        p = self.run_bridge("send", "--channel", "c1", "--text", "x" * 2001,
                            "--approved")
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertEqual(self.mock.posts, [])


if __name__ == "__main__":
    unittest.main()
