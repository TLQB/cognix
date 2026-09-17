import socket, time, json, sys

HOST, PORT = "127.0.0.1", 3001
PROMPT = sys.argv[1] if len(sys.argv) > 1 else "Viết 1 đoạn ngắn giới thiệu về cây cầu Long Biên ở Hà Nội."

body = json.dumps({
    "model": "glm-4.7",
    "stream": True,
    "messages": [{"role": "user", "content": PROMPT}],
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
first_reasoning = None
first_content = None
content_events = []  # (t, bytes)
buf = b""
reasoning_bytes = 0
content_bytes = 0

while True:
    chunk = s.recv(4096)
    if not chunk:
        break
    now = time.time() - t0
    buf += chunk
    # parse complete SSE lines
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
            delta = obj.get("choices", [{}])[0].get("delta", {})
            r = delta.get("reasoning_content") or delta.get("reasoning")
            c = delta.get("content")
            if r:
                if first_reasoning is None:
                    first_reasoning = now
                reasoning_bytes += len(r)
            if c:
                if first_content is None:
                    first_content = now
                content_events.append((now, len(c)))
                content_bytes += len(c)

s.close()
total = time.time() - t0

print(f"total={total:.2f}s  first_reasoning={first_reasoning and f'{first_reasoning:.2f}s'}  first_content={first_content and f'{first_content:.2f}s'}")
print(f"reasoning={reasoning_bytes}B  content={content_bytes}B  content_chunks={len(content_events)}")
if content_events:
    span = content_events[-1][0] - content_events[0][0]
    print(f"content span: first→last = {span:.2f}s")
    if content_events:
        biggest = max(b for _, b in content_events)
        print(f"largest single content chunk: {biggest}B")
        if len(content_events) >= 4 and span > 0:
            print(f"avg inter-chunk gap: {span/(len(content_events)-1)*1000:.0f}ms")
    # chunk timeline (t, size) — first 12
    print("timeline:", " ".join(f"{t:.2f}s:{b}B" for t, b in content_events[:12]))
