#!/usr/bin/env python3
"""Fake audiosocket-bridge /stats server for local controller testing.

Serves the same JSON shape the real bridge exposes:

    GET /stats  -> {"ready": true, "active_call": null | {...}}
    GET /on     -> flip active_call on
    GET /off    -> flip active_call off

Usage:
    python scripts/fake_bridge.py            # listens on 127.0.0.1:8099

Then point the controller at it:

    CF_BRIDGE_STATS_URL=http://127.0.0.1:8099/stats \
        python docker/camoufox/controller.py
"""

from __future__ import annotations

import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {"active": False, "flips": 0}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802 - http.server API
        path = self.path.split("?", 1)[0]
        if path == "/on":
            STATE["active"] = True
            STATE["flips"] += 1
            body = b"on\n"
        elif path == "/off":
            STATE["active"] = False
            STATE["flips"] += 1
            body = b"off\n"
        elif path == "/stats":
            snapshot = {
                "ready": True,
                "mode": "virtual",
                "uptime_seconds": time.time(),
                "counters": {"accepted": STATE["flips"], "rejected": 0,
                             "completed": 0, "failed": 0},
                "active_call": (
                    {"uuid": "probe-call", "remote_addr": "127.0.0.1:0"}
                    if STATE["active"] else None
                ),
            }
            body = json.dumps(snapshot).encode()
        else:
            body = b"{}\n"
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8099), Handler).serve_forever()
