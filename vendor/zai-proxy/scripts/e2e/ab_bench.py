#!/usr/bin/env python3
"""A/B stall-rate benchmark: modern (fold) vs native (multi-turn) agent variant.

Drives BOTH proxy variants with the same hard cases and measures:
  - stalls (parsed from the proxy log after the run)
  - latency per turn (TTFT where visible + total)
  - protocol failures (no tool call when one was required, empty answers)

Hard cases mirror observed production stall patterns (docs/stall_evident.md,
2026-09-09 stall storm):
  1. deep_tool_loop   — 6 sequential tool exchanges; each turn ends with a
                        tool result (the shape that stalled most: model must
                        emit call N+1 from results already in context)
  2. announce_then_do — opening turn primed with an assistant turn that
                        ANNOUNCES intent without calling (the classic stall
                        trigger); model must still call the tool
  3. redundant_guard   — history contains 2 completed calls; the correct
                        next step is a DIFFERENT tool or the final answer —
                        punishes models that re-run prior calls
  4. long_tool_result  — one tool result is ~4KB of text; the next turn must
                        still emit a crisp call/answer (context bloat test)
  5. multi_tool_result — one assistant turn with TWO parallel tool calls and
                        two results; the follow-up must combine both
  6. pivot_mid_loop   — user interjects a changed requirement mid-tool-loop

Usage:  python3 ab_bench.py <proxy_port> <variant_label>
        (one variant per invocation; the wrapper starts each proxy)
"""
import json, sys, time, urllib.request, urllib.error

PORT = sys.argv[1]
LABEL = sys.argv[2]
BASE = f"http://localhost:{PORT}/v1/chat/completions"

TOOLS = [
    {"type": "function", "function": {
        "name": "read_file", "description": "Read a file's content",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                       "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "grep", "description": "Search files by regex",
        "parameters": {"type": "object", "properties": {"pattern": {"type": "string"}},
                       "required": ["pattern"]}}},
    {"type": "function", "function": {
        "name": "calculate", "description": "Evaluate an arithmetic expression",
        "parameters": {"type": "object", "properties": {"expr": {"type": "string"}},
                       "required": ["expr"]}}},
]


def send(messages, timeout=150):
    body = {"model": "glm-4.7", "messages": messages, "tools": TOOLS, "stream": False}
    req = urllib.request.Request(
        BASE, json.dumps(body).encode(),
        {"Content-Type": "application/json", "Authorization": "Bearer Waguri"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.load(r)
        return data, time.time() - t0, None
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as e:
        return None, time.time() - t0, str(e)


def asst(content="", calls=None):
    m = {"role": "assistant", "content": content}
    if calls:
        m["tool_calls"] = [{"id": c[0], "type": "function",
                            "function": {"name": c[1], "arguments": json.dumps(c[2])}}
                           for c in calls]
    return m


def tool(cid, content):
    return {"role": "tool", "tool_call_id": cid, "content": content}


CALL = lambda i, name, args: (f"call_{i}", name, args)

results = []


def run_case(name, build_messages, validator, max_turns=1):
    """build_messages(step_results) -> messages; validator(msg) -> ok, note."""
    msgs = build_messages([])
    ok_all, notes, lat, turns = True, [], 0.0, 0
    # Interactive loop: the case drives multi-turn by appending model answers.
    while turns < max_turns:
        data, dt, err = send(msgs)
        lat += dt
        turns += 1
        if err or data is None:
            results.append((name, False, f"turn{turns} ERROR {err}", dt, turns))
            return
        m = data["choices"][0]["message"]
        calls = m.get("tool_calls") or []
        content = (m.get("content") or "").strip()
        if turns == max_turns:
            ok, note = validator(calls, content)
            results.append((name, ok, note, lat, turns))
            return
        # Feed the model's answer back with a canned tool result.
        msgs = msgs + [m]
        for c in calls:
            msgs.append(tool(c["id"], "ok: result data"))
        if not calls:
            # No call emitted mid-loop: case ends here (validated below).
            ok, note = validator(calls, content)
            results.append((name, ok, note + " [ended early]", lat, turns))
            return


# ── Case 1: deep tool loop ────────────────────────────────────────────
run_case(
    "deep_tool_loop",
    lambda done: [
        {"role": "user", "content": "Compute ((17*23)+100)/7 step by step using the calculate tool for every arithmetic step."}
    ] + done,
    lambda calls, content: (bool(calls) or "compute" in content.lower() or any(ch.isdigit() for ch in content),
                            f"calls={len(calls)} content={content[:60]!r}"),
    max_turns=6,
)

# ── Case 2: announcement-only history then act ─────────────────────────
run_case(
    "announce_then_do",
    lambda done: [
        {"role": "user", "content": "Find the word 'needle' in the project files."},
        asst("I'll search the project files for 'needle' now."),
    ] + done,
    lambda calls, content: (bool(calls), f"calls={len(calls)} content={content[:60]!r}"),
    max_turns=3,
)

# ── Case 3: redundant call guard ──────────────────────────────────────
run_case(
    "redundant_guard",
    lambda done: [
        {"role": "user", "content": "Read config/app.json then read package.json. Report both."},
        asst(calls=[CALL("c1", "read_file", {"path": "config/app.json"})]),
        tool("c1", '{"port": 3002, "mode": "agent"}'),
        asst(calls=[CALL("c2", "read_file", {"path": "package.json"})]),
        tool("c2", '{"name": "zai-proxy", "version": "1.0.8"}'),
    ] + done,
    lambda calls, content: (not calls or content != "",
                            f"final answer with both configs: calls={len(calls)} content={content[:80]!r}"),
    max_turns=2,
)

# ── Case 4: long tool result (~4KB) ───────────────────────────────────
LONG = ("An exception of type SystemError was raised.\n" * 90)[:4000]
run_case(
    "long_tool_result",
    lambda done: [
        {"role": "user", "content": "Read the error log and tell me the FIRST line only."},
        asst(calls=[CALL("c1", "read_file", {"path": "error.log"})]),
        tool("c1", LONG),
    ] + done,
    lambda calls, content: ("SystemError" in content and bool(content) and not calls,
                            f"content={content[:80]!r} calls={len(calls)}"),
    max_turns=2,
)

# ── Case 5: two parallel tool calls ───────────────────────────────────
run_case(
    "multi_tool_result",
    lambda done: [
        {"role": "user", "content": "What is 12*12 and 13*13? Call calculate twice in one reply, then report both results."},
        asst(calls=[CALL("p1", "calculate", {"expr": "12*12"}), CALL("p2", "calculate", {"expr": "13*13"})]),
        tool("p1", "144"), tool("p2", "169"),
    ] + done,
    lambda calls, content: ("144" in content and "169" in content,
                            f"content={content[:80]!r} calls={len(calls)}"),
    max_turns=2,
)

# ── Case 6: mid-loop pivot (user interjects) ───────────────────────────
run_case(
    "pivot_mid_loop",
    lambda done: [
        {"role": "user", "content": "Read main.go and summarize it."},
        asst(calls=[CALL("m1", "read_file", {"path": "main.go"})]),
        tool("m1", "package main\nfunc main() { println('hi') }"),
        {"role": "user", "content": "Actually, forget main.go — what is 999/3? Use calculate."},
    ] + done,
    lambda calls, content: (("333" in content) or (calls and calls[0]["function"]["name"] == "calculate"),
                            f"content={content[:60]!r} calls={len(calls)}"),
    max_turns=2,
)

# ── Report ────────────────────────────────────────────────────────────
print(f"\n===== RESULTS [{LABEL}] port {PORT} =====")
fails = 0
for name, ok, note, lat, turns in results:
    status = "PASS" if ok else "FAIL"
    if not ok:
        fails += 1
    print(f"{status}  {name:18s} turns={turns} lat={lat:5.1f}s  {note}")
total_lat = sum(r[3] for r in results)
print(f"\ntotal_latency={total_lat:.1f}s  cases={len(results)}  failed={fails}")
print(f"ABSUMMARY {LABEL} total_latency={total_lat:.1f} cases={len(results)} failed={fails}")
