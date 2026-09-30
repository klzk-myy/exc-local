#!/usr/bin/env python3
"""Mock order gateway for the blue-green synthetic-gate tests.

Modes (argv[2]):
  ok         — POST /api/v1/orders -> 201 {"order_id": "syn-test-1"};
               DELETE /api/v1/orders/syn-test-1 -> 200; /health/ready -> 200
  fail       — POST -> 500 (dead order path); DELETE -> 500
  reject     — POST -> 422 (risk rejection — path alive, probe must PASS);
               DELETE -> 404
  cancelfail — POST -> 201 with id; DELETE -> 500 (probe must FAIL)

Writes the bound port to argv[3] (port file) once listening so the test
can use --port 0 and read the real port back.
"""
import json
import sys
import http.server


def main():
    port = int(sys.argv[1])
    mode = sys.argv[2] if len(sys.argv) > 2 else "ok"
    port_file = sys.argv[3] if len(sys.argv) > 3 else None

    class H(http.server.BaseHTTPRequestHandler):
        def _send(self, code, obj=None):
            body = json.dumps(obj or {}).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a):  # quiet
            pass

        def do_GET(self):
            if self.path == "/health/ready":
                self._send(200, {"status": "ready"})
            else:
                self._send(404, {})

        def do_POST(self):
            if self.path == "/api/v1/orders":
                if mode == "fail":
                    self._send(500, {"error": "boom"})
                elif mode == "reject":
                    self._send(422, {"code": "RISK_REJECTED"})
                else:
                    self._send(201, {"order_id": "syn-test-1"})
            else:
                self._send(404, {})

        def do_DELETE(self):
            if mode in ("fail", "cancelfail"):
                self._send(500, {"error": "boom"})
            elif mode == "reject":
                self._send(404, {})
            elif self.path == "/api/v1/orders/syn-test-1":
                self._send(200, {"status": "cancelled"})
            else:
                self._send(404, {})

    srv = http.server.HTTPServer(("127.0.0.1", port), H)
    if port_file:
        with open(port_file, "w") as f:
            f.write(str(srv.server_address[1]))
    srv.serve_forever()


if __name__ == "__main__":
    main()
