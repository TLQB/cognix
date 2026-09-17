#!/usr/bin/env python3
"""Live E2E: native agent variant tool-loop through the running proxy.

Sends a 2-turn conversation ending with a tool result — the shape that
stalled most with the modern fold — and asserts the model answers with
either a new TOOL_CALL block or a final answer (never silence).
"""
import json, sys, urllib.request

PROXY = "http://localhost:3009/v1/chat/completions"
TOOLS = [{
    "type": "function",
    "function": {
        "name": "calculate",
        "description": "Compute an arithmetic expression",
        "parameters": {"type": "object", "properties": {"expr": {"type": "string"}}},
    },
}]

def send(messages):
    body = {"model": "glm-4.7", "messages": messages, "tools": TOOLS, "stream": False}
    req = urllib.request.Request(PROXY, json.dumps(body).encode(),
                                 {"Content-Type": "application/json", "Authorization": "Bearer Waguri"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.load(r)

# Turn 1: opening request (last message = user → full thinking).
r1 = send([{"role": "user", "content": "What is 128 * 46? Use the calculate tool."}])
m1 = r1["choices"][0]["message"]
print("TURN1 content:", (m1.get("content") or "")[:150])
calls1 = m1.get("tool_calls") or []
print("TURN1 tool_calls:", json.dumps(calls1)[:200])
assert calls1, "TURN1: expected a tool call"

# Turn 2: feed the tool result back — last message = tool result
# (the mid-loop turn that used to stall).
r2 = send([
    {"role": "user", "content": "What is 128 * 46? Use the calculate tool."},
    {"role": "assistant", "content": m1.get("content") or "", "tool_calls": calls1},
    {"role": "tool", "tool_call_id": calls1[0]["id"], "content": "5888"},
])
m2 = r2["choices"][0]["message"]
content2 = (m2.get("content") or "").strip()
print("TURN2 content:", content2[:200])
print("TURN2 tool_calls:", json.dumps(m2.get("tool_calls") or [])[:200])
assert "5888" in content2, f"TURN2: expected the final answer containing 5888, got: {content2!r}"
print("\nE2E PASS: native multi-turn tool loop works end-to-end.")
