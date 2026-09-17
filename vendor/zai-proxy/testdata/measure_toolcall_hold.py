import socket, time, json, sys

HOST, PORT = "127.0.0.1", 3001

# Tool-call stall-hold probe: the model announces ("Toi se doc...") then
# emits a tool call. Verifies: (1) announcement content reaches client only
# ONCE (no duplicate from stall retry), (2) finish_reason=tool_calls,
# (3) tool call args intact.
PROMPT = "Hãy đọc file README.md ở thư mục hiện tại rồi tóm tắt 3 ý chính."

body = json.dumps({
    "model": "glm-4.7",
    "stream": True,
    "messages": [{"role": "user", "content": PROMPT}],
    "tools": [{
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Read a file from the project",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "File path to read"},
                },
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
first_reasoning = first_content = first_tool = None
content_events = []
content_text = ""
tool_calls = []
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
            tcs = delta.get("tool_calls")
            if ch.get("finish_reason"):
                finish_reason = ch["finish_reason"]
            if r and first_reasoning is None:
                first_reasoning = now
            if c:
                if first_content is None:
                    first_content = now
                content_events.append((now, len(c)))
                content_text += c
            if tcs:
                if first_tool is None:
                    first_tool = now
                for tc in tcs:
                    idx = tc.get("index", len(tool_calls))
                    fn = tc.get("function", {})
                    while idx >= len(tool_calls):
                        tool_calls.append({"name": "", "args": ""})
                    if fn.get("name"):
                        tool_calls[idx]["name"] += fn["name"]
                    if fn.get("arguments"):
                        tool_calls[idx]["args"] += fn["arguments"]

s.close()
total = time.time() - t0

print(f"total={total:.2f}s first_reasoning={first_reasoning and f'{first_reasoning:.2f}s'} first_content={first_content and f'{first_content:.2f}s'} first_tool={first_tool and f'{first_tool:.2f}s'}")
print(f"finish_reason={finish_reason} tool_calls={tool_calls}")
print(f"content_chunks={len(content_events)} content={len(content_text)}B")
print(f"content_text={content_text[:300]!r}")
n_phrase = content_text.lower().count("tôi sẽ")
print(f"'tôi sẽ' occurrences in content: {n_phrase} (must be <=1: no duplicate announcement)")
if content_events and len(content_events) >= 4:
    span = content_events[-1][0] - content_events[0][0]
    print(f"avg inter-chunk gap: {span/(len(content_events)-1)*1000:.0f}ms")
