"""Slow-stream mock upstream: emits N SSE delta_content chunks at a fixed
interval (steady stream, total duration >> stall window). Used to verify the
idle-aware stall guard never kills a live long stream."""
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 39878
CHUNKS = int(sys.argv[2]) if len(sys.argv) > 2 else 20
INTERVAL = float(sys.argv[3]) if len(sys.argv) > 3 else 0.5

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path.startswith("/api/models"):
            self._json({"data": [{"id": "glm-4.7", "name": "GLM-4.7",
                "info": {"name": "GLM-4.7", "meta": {"description": "mock",
                "capabilities": {"enable_thinking": True}}}}]})
        else:
            # base-url scrape: any JSON keeps the feVersion fallback quiet
            self._json({"ok": True})

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        self.rfile.read(n)
        if not self.path.endswith("/chat/completions"):
            self._json({}, 404)
            return
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        for i in range(CHUNKS):
            time.sleep(INTERVAL)
            data = json.dumps({"data": {"delta_content": f"chunk{i} "}})
            self.wfile.write(f"data: {data}\n\n".encode())
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def do_DELETE(self):
        # chat-delete after a used session — the real upstream returns 200.
        self._json({"code": 0, "msg": "ok"})

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
