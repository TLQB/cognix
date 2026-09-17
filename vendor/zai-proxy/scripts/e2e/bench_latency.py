#!/usr/bin/env python3
"""Latency benchmark for the zai-proxy agent path.

Measures, across scenarios, against a LIVE proxy (default: the production
instance style config, but point PROXY at any port):
  - TTFT (time to first content/tool_call delta) on streaming requests
  - total request latency
  - HTTP status / error rate
  - concurrency behavior (N parallel requests)

Scenarios:
  1. plain          — single user message, no tools (baseline raw LLM cost)
  2. tool_one       — one tool round-trip (call -> result -> final)
  3. depth          — 8-turn tool history (deep loop; history cost)
  4. long_ctx       — ~8KB tool result in history (context bloat)
  5. burst          — N parallel simple requests (pool + captcha contention)

Usage:
  python3 bench_latency.py <port> [--model glm-4.7] [--runs 3] [--json out.json]

Numbers land on stdout as a table; --json also dumps raw samples.
"""
import json, os, socket, sys, time, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor

PORT = sys.argv[1] if len(sys.argv) > 1 else "3002"
BASE = f"http://localhost:{PORT}/v1/chat/completions"
TOKEN = os.environ.get("BENCH_TOKEN", "Waguri")
MODEL = "glm-4.7"
RUNS = 3

args = sys.argv[2:]
if "--model" in args:
    MODEL = args[args.index("--model") + 1]
if "--runs" in args:
    RUNS = int(args[args.index("--runs") + 1])
OUT_JSON = args[args.index("--json") + 1] if "--json" in args else None

TOOLS = [
    {"type": "function", "function": {"name": "read_file", "description": "Read a file",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}},
    {"type": "function", "function": {"name": "calculate", "description": "Evaluate arithmetic",
     "parameters": {"type": "object", "properties": {"expr": {"type": "string"}}, "required": ["expr"]}}},
]

def tool_history(depth):
    """Build a deep multi-turn tool history (the native defuse shape)."""
    msgs = [{"role": "user", "content": "Read report.txt and compute stats. Use tools."}]
    for i in range(depth):
        msgs.append({"role": "assistant", "content": "", "tool_calls": [
            {"id": f"c{i}", "type": "function",
             "function": {"name": "read_file", "arguments": json.dumps({"path": f"file{i}.txt"})}}]})
        msgs.append({"role": "tool", "tool_call_id": f"c{i}",
                     "content": f"line {i}: some data {i*7} more filler text " * 20})
    msgs.append({"role": "user", "content": "Now compute 999/3 with calculate."})
    return msgs

def long_ctx_history():
    msgs = [{"role": "user", "content": "Read big.log and summarize."}]
    msgs.append({"role": "assistant", "content": "", "tool_calls": [
        {"id": "c0", "type": "function",
         "function": {"name": "read_file", "arguments": json.dumps({"path": "big.log"})}}]})
    msgs.append({"role": "tool", "tool_call_id": "c0",
                 "content": "ERROR some component failed with details\n" * 160})
    msgs.append({"role": "user", "content": "What is 999/3? Use calculate."})
    return msgs

def stream_once(body, timeout=180):
    """Stream a request; return (ttft_s, total_s, status, first_kind).

    TTFT = first delta carrying real CONTENT or a tool_call fragment (the
    role-only and empty-delta chunks the bridge emits at stream start do
    not count).
    """
    req = urllib.request.Request(BASE, json.dumps(body).encode(),
        {"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
    t0 = time.time()
    ttft = None
    first_kind = None
    status = 200
    buf = b""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            while True:
                chunk = r.read(1)
                if not chunk:
                    break
                buf += chunk
                if ttft is None and b"\n\n" in buf:
                    kind = _first_meaningful(buf)[0]
                    if kind is not None:
                        ttft = time.time() - t0
                        first_kind = kind
                        break
            if ttft is None:  # stream ended without content
                kind = _first_meaningful(buf)[0]
                if kind is not None:
                    ttft = time.time() - t0
                    first_kind = kind
        return ttft, time.time() - t0, status, first_kind
    except urllib.error.HTTPError as e:
        return ttft, time.time() - t0, e.code, "http_error"
    except Exception as e:
        return ttft, time.time() - t0, -1, str(e)[:40]

def _first_meaningful(buf):
    """Scan completed SSE events in buf; return (ttft_info, kind) for the
    first delta with content or a tool_call fragment, else (None, None).

    reasoning_content counts as meaningful: glm-4.7 streams its thinking
    body there first, so a probe that only looks at `content` reports
    "no content" for thinking turns (observed 2026-09-10: bench plain
    showed errs=1 while the stream was actually fine)."""
    for block in buf.decode(errors="replace").split("\n\n"):
        for line in block.splitlines():
            if not line.startswith("data:"):
                continue
            try:
                ev = json.loads(line[5:])
            except json.JSONDecodeError:
                continue
            for ch in ev.get("choices", []):
                d = ch.get("delta", {})
                if d.get("tool_calls"):
                    return "tool_call", "tool_call"
                if d.get("content"):
                    return "content", "content"
                if d.get("reasoning_content"):
                    return "reasoning", "reasoning"
    return None, None

def bench(name, body, runs=RUNS):
    rows = []
    for _ in range(runs):
        rows.append(stream_once(body))
    ttfts = [r[0] for r in rows if isinstance(r[0], float)]
    tots = [r[1] for r in rows]
    errs = sum(1 for r in rows if r[2] != 200 or r[0] is None)
    med = lambda v: sorted(v)[len(v)//2] if v else -1
    print(f"{name:<12} ttft_med={med(ttfts):6.2f}s total_med={med(tots):6.2f}s errs={errs}/{len(rows)}")
    return rows

def burst(n):
    body = {"model": MODEL, "stream": True,
            "messages": [{"role": "user", "content": "Say exactly: OK"}]}
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=n) as ex:
        rows = list(ex.map(lambda _: stream_once(body), range(n)))
    tots = [r[1] for r in rows]
    errs = sum(1 for r in rows if r[2] != 200)
    print(f"burst{n:<8} wall={time.time()-t0:6.2f}s max={max(tots):6.2f}s "
          f"p50={sorted(tots)[len(tots)//2]:6.2f}s errs={errs}/{n}")
    return rows

all_rows = {}
print(f"=== bench vs {BASE} model={MODEL} runs={RUNS} ===")
all_rows["plain"] = bench("plain", {"model": MODEL, "stream": True,
    "messages": [{"role": "user", "content": "Say exactly: OK"}]})
all_rows["tool_one"] = bench("tool_one", {"model": MODEL, "stream": True,
    "messages": [{"role": "user", "content": "Use calculate to compute 999/3."},
                 {"role": "assistant", "content": "", "tool_calls": [
                     {"id": "m1", "type": "function", "function": {"name": "read_file",
                      "arguments": json.dumps({"path": "x"})}}]},
                 {"role": "tool", "tool_call_id": "m1", "content": "dummy"},
                 {"role": "user", "content": "Now actually compute 999/3 with calculate."}],
    "tools": TOOLS})
all_rows["depth8"] = bench("depth8", {"model": MODEL, "stream": True,
    "messages": tool_history(8), "tools": TOOLS})
all_rows["long_ctx"] = bench("long_ctx", {"model": MODEL, "stream": True,
    "messages": long_ctx_history(), "tools": TOOLS})
all_rows["burst6"] = burst(6)
all_rows["burst12"] = burst(12)

if OUT_JSON:
    json.dump(all_rows, open(OUT_JSON, "w"), indent=1)
    print("json ->", OUT_JSON)
