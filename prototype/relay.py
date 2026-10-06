#!/usr/bin/env python3
"""muse-relay.py v2 -- group task relay for the user's Muse assistants.

Both assistants are Tailscale client-only (outbound connections only), so
this relay runs on the user's always-on VPS (on the tailnet, accepts inbound)
and every assistant polls it over the tailnet. The relay never calls out; it
only queues tasks and results.

Identities are arbitrary ids (muse-a, muse-b, ...). Each assistant polls with
its own identity; no per-assistant registration needed.

Protocol: JSON over HTTP. Every request needs:
    Authorization: Bearer <MUSE_RELAY_TOKEN>

  POST /task     {"to":"muse-b"|"*","from":"muse-a","id":"t1",
                 "instruction":"..."}   -> queue a task. "to":"*" broadcasts
                                          to every assistant except the sender.
  GET  /tasks?for=muse-b                -> unacked tasks for that identity
                                          (direct + broadcasts not from self)
  POST /ack      {"kind":"task"|"result","id":"t1","by":"muse-b"}
                                        -> mark delivered FOR THAT identity.
                                          Broadcasts stay visible to others
                                          until each acks.
  POST /result   {"to":"muse-a","id":"t1","result":"..."}
                                        -> store a result for `to`
  GET  /results?for=muse-a              -> unacked results for that identity
  POST /heartbeat {"id":"muse-b"}       -> mark identity alive (assistants
                                          call this on every poll)
  GET  /peers                           -> [{"id","last_seen","online"}]
  GET  /health                          -> {"ok": true}

Stdlib only. State lives in JSON files under the data dir.
v1 -> v2 migration: {"acked": bool} becomes {"acked_by": [ids]} on load.
"""

import hmac
import json
import os
import re
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

TOKEN = os.environ.get("MUSE_RELAY_TOKEN", "")
PORT = int(os.environ.get("MUSE_RELAY_PORT", "18789"))
DATA_DIR = os.environ.get("MUSE_RELAY_DATA", "/var/lib/muse-relay")
MAX_BODY = 1 << 20  # 1 MiB
ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")
PEER_ONLINE_SECS = 300
PEER_PRUNE_SECS = 7 * 86400

if not TOKEN:
    sys.exit("MUSE_RELAY_TOKEN is not set")

os.makedirs(DATA_DIR, exist_ok=True)
TASKS_FILE = os.path.join(DATA_DIR, "tasks.json")
RESULTS_FILE = os.path.join(DATA_DIR, "results.json")
PEERS_FILE = os.path.join(DATA_DIR, "peers.json")
_lock = threading.Lock()


def _load(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
            return data if isinstance(data, dict) else {}
    except (FileNotFoundError, ValueError):
        return {}


def _save(path, data):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False)
    os.replace(tmp, path)


def _migrate_acks(items):
    """v1 {"acked": bool} -> v2 {"acked_by": [identity, ...]}."""
    for v in items.values():
        if "acked_by" not in v:
            if v.get("acked"):
                v["acked_by"] = [v.get("to")]
            else:
                v["acked_by"] = []
            v.pop("acked", None)
    return items


def _valid_id(s):
    return isinstance(s, str) and bool(ID_RE.match(s))


def _valid_target(s):
    return s == "*" or _valid_id(s)


def _tailscale_ip():
    bind = os.environ.get("MUSE_RELAY_BIND")  # override, e.g. for testing
    if bind:
        return bind
    try:
        out = subprocess.run(["tailscale", "ip", "-4"], capture_output=True,
                             text=True, timeout=10)
    except (FileNotFoundError, subprocess.SubprocessError):
        out = None
    ip = ""
    if out is not None and out.returncode == 0:
        parts = (out.stdout or "").strip().split()
        ip = parts[0] if parts else ""
    if not ip or not ip.startswith("100."):
        sys.exit("no Tailscale IPv4 found; is tailscale up on this machine?")
    return ip


class Handler(BaseHTTPRequestHandler):
    server_version = "muse-relay/2.0"

    def log_message(self, fmt, *args):  # one line per request on stderr
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

    def _send(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authed(self):
        auth = self.headers.get("Authorization", "")
        return auth.startswith("Bearer ") and hmac.compare_digest(
            auth[len("Bearer "):], TOKEN)

    def _read_json(self):
        try:
            n = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return None
        if n <= 0 or n > MAX_BODY:
            return None
        try:
            return json.loads(self.rfile.read(n).decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            return None

    def _guard(self):
        if not self._authed():
            self._send(401, {"ok": False, "error": "unauthorized"})
            return False
        return True

    def do_GET(self):
        if not self._guard():
            return
        u = urlparse(self.path)
        q = parse_qs(u.query)
        who = q.get("for", [""])[0]
        if u.path == "/health":
            return self._send(200, {"ok": True, "version": 2})
        if u.path == "/peers":
            return self._send(200, {"ok": True, "peers": self._peers()})
        if u.path not in ("/tasks", "/results"):
            return self._send(404, {"ok": False, "error": "not found"})
        if not _valid_id(who):
            return self._send(400, {"ok": False, "error": "bad 'for' param"})
        path = TASKS_FILE if u.path == "/tasks" else RESULTS_FILE
        is_tasks = u.path == "/tasks"
        with _lock:
            items = _migrate_acks(_load(path))
            out = []
            for v in items.values():
                if who in v.get("acked_by", []):
                    continue
                to = v.get("to")
                if is_tasks:
                    if to != who and to != "*":
                        continue
                    if to == "*" and v.get("from") == who:
                        continue  # don't receive your own broadcast
                elif to != who:
                    continue
                out.append(v)
        out.sort(key=lambda v: v.get("created", 0))
        self._send(200, {"ok": True, "items": out})

    def do_POST(self):
        if not self._guard():
            return
        u = urlparse(self.path)
        body = self._read_json()
        if body is None:
            return self._send(400, {"ok": False, "error": "bad json body"})
        if u.path == "/task":
            return self._handle_task(body)
        if u.path == "/result":
            ok, info = self._store_result(body)
            if not ok:
                return self._send(400, {"ok": False, "error": info})
            return self._send(200, {"ok": True, "id": info})
        if u.path == "/ack":
            return self._handle_ack(body)
        if u.path == "/heartbeat":
            return self._handle_heartbeat(body)
        return self._send(404, {"ok": False, "error": "not found"})

    def _handle_task(self, b):
        tid, to, frm, ins = (b.get("id"), b.get("to"), b.get("from"),
                             b.get("instruction"))
        if not (_valid_id(tid) and _valid_target(to)
                and isinstance(ins, str) and ins.strip()
                and len(ins) <= 200_000):
            return self._send(400, {"ok": False, "error": "bad task"})
        with _lock:
            tasks = _migrate_acks(_load(TASKS_FILE))
            if tid in tasks:
                return self._send(409, {"ok": False, "error": "duplicate id"})
            tasks[tid] = {"id": tid, "to": to, "from": frm,
                          "instruction": ins, "created": time.time(),
                          "acked_by": []}
            _save(TASKS_FILE, tasks)
        self._send(200, {"ok": True, "id": tid})

    def _store_result(self, b):
        """Returns (True, tid) on success, (False, error) on bad input."""
        tid, to, res = b.get("id"), b.get("to"), b.get("result")
        if not (_valid_id(tid) and _valid_id(to)
                and isinstance(res, str) and len(res) <= 500_000):
            return False, "bad result"
        with _lock:
            results = _migrate_acks(_load(RESULTS_FILE))
            results[tid] = {"id": tid, "to": to, "result": res,
                            "created": time.time(), "acked_by": []}
            _save(RESULTS_FILE, results)
        return True, tid

    def _handle_ack(self, b):
        kind, tid, by = b.get("kind"), b.get("id"), b.get("by")
        if kind not in ("task", "result") or not _valid_id(tid) \
                or not _valid_id(by):
            return self._send(400, {"ok": False, "error": "bad ack"})
        path = TASKS_FILE if kind == "task" else RESULTS_FILE
        with _lock:
            items = _migrate_acks(_load(path))
            if tid in items and by not in items[tid]["acked_by"]:
                items[tid]["acked_by"].append(by)
                _save(path, items)
        self._send(200, {"ok": True})

    def _handle_heartbeat(self, b):
        ident = b.get("id")
        if not _valid_id(ident):
            return self._send(400, {"ok": False, "error": "bad id"})
        now = time.time()
        with _lock:
            peers = _load(PEERS_FILE)
            peers[ident] = now
            peers = {k: v for k, v in peers.items()
                     if now - v < PEER_PRUNE_SECS}
            _save(PEERS_FILE, peers)
        self._send(200, {"ok": True})

    def _peers(self):
        now = time.time()
        with _lock:
            peers = _load(PEERS_FILE)
        return [{"id": k, "last_seen": v,
                 "online": now - v < PEER_ONLINE_SECS}
                for k, v in sorted(peers.items())]


def main():
    bind = _tailscale_ip()
    srv = ThreadingHTTPServer((bind, PORT), Handler)
    print("muse-relay v2 listening on %s:%d" % (bind, PORT), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
