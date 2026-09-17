import socket, time, json, sys

HOST, PORT = "127.0.0.1", 3001

# TRUE-stall probe: tools are listed but the task genuinely needs none.
# If the model still announces intent without calling a tool, the proxy's
# stall-retry kicks in. Verifies: announcement appears EXACTLY once even
# with retries (SSE cannot un-send), final answer eventually arrives.
PROMPT = sys.argv[1] if len(sys.argv) > 1 else "Chào bạn, bạn là AI gì vậy?"

body = json.dumps({
    "model": "glm-4.7",
    "stream": True,
    "messages": [{"role": "user", "content": PROMPT}],
    "tools": [{
        "type": "function",
        "function": {
            "name": "list_directory",
            "description": "List files in a directory",
            "parameters": {
                "type": "object",
                "properties": {"path": {"type": "string"}},
                "required": ["path"],
            },
        },
    }],
})

req = (
    "POST /v1/chat/completions HTTP/1.1\r\n"
    f"Host: {HOST}:{PORT}\r\n"
    "Authorization: Bearer Waguri\r\n"
    "Content-Type: application/json\r\n"
    f"Content-Length: {len(body.encode())}\r\n"
    "Connection: close\r\n\r\n"
    + body
)

s = socket.create_connection((HOST, PORT))
s.sendall(req.encode())

t0 = time.time()
first_reasoning = first_content = None
content_events = []
content_text = ""
reasoning_text = ""
finish_reason = None
buf = b""

while True:
    chunk = s.recv(4096)
    if not chunk:
        break
    now = time.time() - t0
    buf += chunk
    while b"\n\n" in buf:
        raw, buf = buf.split(b"\n\n", 1)
        for line in raw.split(b"\n"):
            if not line.startswith(b"data: "):
                continue
            data = line[6:]
            if data == b"[DONE]":
                continue
            try:
                obj = json.loads(data)
            except Exception:
                continue
            ch = obj.get("choices", [{}])[0]
            delta = ch.get("delta", {})
            r = delta.get("reasoning_content") or delta.get("reasoning")
            c = delta.get("content")
            if ch.get("finish_reason"):
                finish_reason = ch["finish_reason"]
            if r:
                if first_reasoning is None:
                    first_reasoning = now
                reasoning_text += r
            if c:
                if first_content is None:
                    first_content = now
                content_events.append((now, len(c)))
                content_text += c

s.close()
total = time.time() - t0

print(f"total={total:.2f}s first_reasoning={first_reasoning and f'{first_reasoning:.2f}s'} first_content={first_content and f'{first_content:.2f}s'}")
print(f"finish_reason={finish_reason} reasoning={len(reasoning_text)}B content={len(content_text)}B chunks={len(content_events)}")
low = content_text.lower()
for p in ("chào bạn", "hello", "chào"):
    n = low.count(p)
    if n > 1:
        print(f"DUPLICATE DETECTED: '{p}' x{n}")
print(f"content: {content_text[:200]!r}")
if content_events and len(content_events) >= 4:
    span = content_events[-1][0] - content_events[0][0]
    print(f"avg inter-chunk gap: {span/(len(content_events)-1)*1000:.0f}ms largest={max(b for _,b in content_events)}B")
