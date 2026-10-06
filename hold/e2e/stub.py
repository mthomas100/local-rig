#!/usr/bin/env python3
"""A stand-in llama-swap for the hold gate's pi test: logs every request; /v1/chat/completions streams a reply
(a bash tool call when the user says USE_TOOL, plain text otherwise); a request marks qwen38 loaded."""
import json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
LOG = sys.argv[2]
state = {"loaded": [], "lock": threading.Lock()}
def log(line):
    with open(LOG, "a") as f: f.write(f"{time.time():.3f} {line}\n")
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def send_json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        log(f"GET {self.path}")
        if self.path.startswith("/running"):
            self.send_json({"running": [{"model": m, "state": "ready"} for m in state["loaded"]]})
        elif self.path.startswith("/unload"):
            time.sleep(0.3); state["loaded"] = []; log("UNLOADED"); self.send_json({"ok": True})
        elif self.path.startswith("/v1/models"):
            self.send_json({"data": [{"id": "qwen38", "object": "model"}]})
        else:
            self.send_json({"error": "no"}, 404)
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0)); body = json.loads(self.rfile.read(n) or b"{}")
        if not self.path.startswith("/v1/chat/completions"):
            log(f"POST {self.path}"); return self.send_json({"error": "no"}, 404)
        if "qwen38" not in state["loaded"]:
            state["loaded"].append("qwen38"); log("LOADED qwen38")
        msgs = body.get("messages", [])
        last = msgs[-1] if msgs else {}
        text = json.dumps(last.get("content", ""))
        log(f"CHAT role={last.get('role')} tools={len(body.get('tools') or [])} last={text[:60]}")
        self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.send_header("Transfer-Encoding", "chunked"); self.end_headers()
        def chunk(obj):
            data = ("data: " + (obj if isinstance(obj, str) else json.dumps(obj)) + "\n\n").encode()
            self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n"); self.wfile.flush()
        base = {"id": "c1", "object": "chat.completion.chunk", "created": int(time.time()), "model": "qwen38"}
        slow = float(self.headers.get("X-Slow", "0") or 0) or (2.0 if "SLOW" in text else 0)
        if last.get("role") == "user" and "USE_TOOL" in text:
            args = json.dumps({"command": "sleep 4; echo tool-ran"})
            chunk({**base, "choices": [{"index": 0, "delta": {"role": "assistant", "tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "bash", "arguments": args}}]}, "finish_reason": None}]})
            chunk({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}], "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
        else:
            reply = "DONE-AFTER-TOOL" if last.get("role") == "tool" else "PONG"
            for i, piece in enumerate([reply[: len(reply) // 2], reply[len(reply) // 2 :]]):
                chunk({**base, "choices": [{"index": 0, "delta": ({"role": "assistant"} if i == 0 else {}) | {"content": piece}, "finish_reason": None}]})
                if slow: time.sleep(slow)
            chunk({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}})
        chunk("[DONE]")
        self.wfile.write(b"0\r\n\r\n"); self.wfile.flush()
        log("CHAT done")
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
