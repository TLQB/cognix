#!/usr/bin/env python3
"""Smoke test: basic chat (non-stream + stream) against the e2e server."""
import json, sys, urllib.request

BASE = "http://localhost:3002"
TOKEN = "e2e-token"

def post(path, body, timeout=120):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json",
                                          "Authorization": "Bearer " + TOKEN})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status, r.read()

def smoke_nonstream():
    st, body = post("/v1/chat/completions", {
        "model": "glm-4.7",
        "stream": False,
        "messages": [{"role": "user", "content": "Reply with exactly: SMOKE_OK_1"}],
    })
    data = json.loads(body)
    content = data["choices"][0]["message"]["content"]
    ok = "SMOKE_OK_1" in content
    print(f"[nonstream] status={st} ok={ok} content={content[:120]!r}")
    return ok

def smoke_stream():
    req = urllib.request.Request(BASE + "/v1/chat/completions",
        data=json.dumps({"model": "glm-4.7", "stream": True,
                         "messages": [{"role": "user", "content": "Reply with exactly: SMOKE_OK_2"}]}).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
    chunks, content, done = 0, "", False
    with urllib.request.urlopen(req, timeout=120) as r:
        for line in r:
            line = line.decode().strip()
            if not line.startswith("data: "):
                continue
            payload = line[6:]
            if payload == "[DONE]":
                done = True
                break
            chunks += 1
            try:
                d = json.loads(payload)
                delta = d["choices"][0]["delta"].get("content") or ""
                content += delta
            except Exception:
                pass
    ok = "SMOKE_OK_2" in content and done and chunks > 0
    print(f"[stream] chunks={chunks} done={done} ok={ok} content={content[:120]!r}")
    return ok

def smoke_models():
    req = urllib.request.Request(BASE + "/v1/models", headers={"Authorization": "Bearer " + TOKEN})
    with urllib.request.urlopen(req, timeout=30) as r:
        data = json.loads(r.read())
    ids = [m["id"] for m in data["data"]]
    print(f"[models] {len(ids)} models: {ids[:8]}")
    return len(ids) > 0

if __name__ == "__main__":
    r1 = smoke_models()
    r2 = smoke_nonstream()
    r3 = smoke_stream()
    print("SMOKE:", "PASS" if (r1 and r2 and r3) else "FAIL")
    sys.exit(0 if (r1 and r2 and r3) else 1)
