#!/usr/bin/env python3
"""Stub order-gateway for the HAProxy validation drill (Task 5.3.29).

Usage: stub_server.py <COLOR> <PORT>

Behaviour:
  GET /health/ready   -> 200 "<COLOR>\n"   (what `option httpchk` gates on)
  GET /health/live    -> 200 "<COLOR>\n"
  GET/POST any path   -> 200 "<COLOR>\n"   (X-Stub-Color header echoed too)
  Upgrade: websocket  -> honest 101 Switching Protocols with a real
                         Sec-WebSocket-Accept, then EOF (no frame codec —
                         HAProxy only needs the 101 to enter tunnel mode)

Bind is 0.0.0.0 so the haproxy container reaches it via
host.docker.internal -> host-gateway.
"""
import base64
import hashlib
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

COLOR = sys.argv[1] if len(sys.argv) > 1 else "UNKNOWN"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 18081
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s:%d %s\n" % (COLOR, PORT, fmt % args))

    def _respond_body(self):
        body = (COLOR + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("X-Stub-Color", COLOR)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _respond_ws(self):
        key = self.headers.get("Sec-WebSocket-Key", "")
        accept = base64.b64encode(
            hashlib.sha1((key + WS_GUID).encode()).digest()
        ).decode()
        self.send_response(101, "Switching Protocols")
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", accept)
        self.send_header("X-Stub-Color", COLOR)
        self.end_headers()
        # Tunnel established; stub has no frame codec — close honestly.
        self.close_connection = True

    def _dispatch(self):
        if self.headers.get("Upgrade", "").lower() == "websocket":
            self._respond_ws()
        else:
            self._respond_body()

    do_GET = _dispatch
    do_POST = _dispatch
    do_PUT = _dispatch
    do_DELETE = _dispatch
    do_OPTIONS = _dispatch

    def do_HEAD(self):
        self._respond_body()


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    sys.stderr.write("stub %s listening on 0.0.0.0:%d\n" % (COLOR, PORT))
    srv.serve_forever()
