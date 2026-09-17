#!/usr/bin/env python3
"""
E2E agent harness: drives the bridge through REAL multi-turn coding tasks.

The harness acts as an agent runtime (like the Cognix editor itself):
  1. Sends a coding task + tool definitions to /v1/chat/completions
  2. Receives tool_calls deltas (streaming) or complete calls (non-stream)
  3. EXECUTES the tools against a scratch workspace (list/read/write/edit/
     grep/bash) and feeds results back
  4. Repeats until the model produces a final answer (no tool calls)
  5. Verifies the workspace end state matches the task requirements

Also covers the Anthropic /v1/messages surface with tool use.
"""
import json, os, shlex, shutil, subprocess, sys, time, urllib.request

BASE = "http://localhost:3002"
TOKEN = "e2e-token"
WS = "/tmp/e2e/workspace"
MAX_TURNS = 24
MODEL = os.environ.get("E2E_MODEL", "glm-4.7")

# ---------------------------------------------------------------- tools ----

TOOLS = [
    {"type": "function", "function": {"name": "list_directory",
        "description": "List files and directories at the given path relative to the workspace root.",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                       "required": ["path"]}}},
    {"type": "function", "function": {"name": "read_file",
        "description": "Read the content of a file at the given path relative to the workspace root.",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                       "required": ["path"]}}},
    {"type": "function", "function": {"name": "write_file",
        "description": "Create or overwrite a file with the given content, path relative to the workspace root.",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                       "required": ["path", "content"]}}},
    {"type": "function", "function": {"name": "edit_file",
        "description": "Replace an exact text snippet in a file. old_text must match exactly.",
        "parameters": {"type": "object",
                       "properties": {"path": {"type": "string"}, "old_text": {"type": "string"}, "new_text": {"type": "string"}},
                       "required": ["path", "old_text", "new_text"]}}},
    {"type": "function", "function": {"name": "run_bash",
        "description": "Run a bash command inside the workspace and return stdout+stderr.",
        "parameters": {"type": "object", "properties": {"command": {"type": "string"}},
                       "required": ["command"]}}},
]

def ws_path(rel):
    p = os.path.normpath(os.path.join(WS, rel))
    if not p.startswith(WS):
        raise ValueError("path escapes workspace: " + rel)
    return p

def execute_tool(name, args):
    try:
        if name == "list_directory":
            p = ws_path(args.get("path", "."))
            entries = sorted(os.listdir(p)) if os.path.isdir(p) else []
            return "\n".join(entries) or "(empty)"
        if name == "read_file":
            with open(ws_path(args["path"]), encoding="utf-8") as f:
                return f.read()
        if name == "write_file":
            p = ws_path(args["path"])
            os.makedirs(os.path.dirname(p), exist_ok=True)
            with open(p, "w", encoding="utf-8") as f:
                f.write(args["content"])
            return "OK: wrote " + args["path"]
        if name == "edit_file":
            p = ws_path(args["path"])
            with open(p, encoding="utf-8") as f:
                text = f.read()
            if args["old_text"] not in text:
                return "ERROR: old_text not found in " + args["path"]
            text = text.replace(args["old_text"], args["new_text"], 1)
            with open(p, "w", encoding="utf-8") as f:
                f.write(text)
            return "OK: edited " + args["path"]
        if name == "run_bash":
            r = subprocess.run(["bash", "-c", args["command"]], cwd=WS,
                              capture_output=True, text=True, timeout=30)
            out = (r.stdout or "") + (("\n[stderr]\n" + r.stderr) if r.stderr else "")
            return out.strip() or "(no output)"
        return "ERROR: unknown tool " + name
    except Exception as e:
        return "ERROR: " + str(e)

# ------------------------------------------------------------- transport ---

def post_stream(body, timeout=300):
    req = urllib.request.Request(BASE + "/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
    events = []  # list of dicts: {"delta": {...}, "finish": str}
    with urllib.request.urlopen(req, timeout=timeout) as r:
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data: "):
                continue
            payload = line[6:]
            if payload == "[DONE]":
                events.append({"done": True})
                break
            try:
                ev = json.loads(payload)
            except json.JSONDecodeError:
                continue
            # Surface upstream/server errors instead of silently returning
            # an empty final answer (observed: MODEL_CONCURRENCY_LIMIT bursts
            # made tasks fail with "" and no clue why).
            if "error" in ev and "choices" not in ev:
                raise RuntimeError("stream error event: " + json.dumps(ev["error"])[:300])
            events.append(ev)
    return events

def post_nonstream(body, timeout=300):
    req = urllib.request.Request(BASE + "/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read())

# ------------------------------------------------------------ agent loop ---

def run_task(task, mode="stream", tools=TOOLS, model=MODEL):
    """Runs one multi-turn agent task; returns (final_text, tool_calls_count)."""
    messages = [{"role": "user", "content": task}]
    total_calls = 0
    for turn in range(MAX_TURNS):
        body = {"model": model, "messages": messages, "tools": tools}
        if mode == "stream":
            body["stream"] = True
            events = post_stream(body)
            content = ""
            calls_by_idx = {}
            finish = None
            for ev in events:
                if ev.get("done"):
                    continue
                for ch in ev.get("choices", []):
                    d = ch.get("delta", {})
                    content += d.get("content") or ""
                    for tc in d.get("tool_calls") or []:
                        i = tc.get("index", 0)
                        c = calls_by_idx.setdefault(i, {"id": "", "name": "", "args": ""})
                        if tc.get("id"):
                            c["id"] = tc["id"]
                        fn = tc.get("function") or {}
                        if fn.get("name"):
                            c["name"] = fn["name"]
                        c["args"] += fn.get("arguments") or ""
                    if ch.get("finish_reason"):
                        finish = ch["finish_reason"]
            calls = [calls_by_idx[i] for i in sorted(calls_by_idx)]
        else:
            body["stream"] = False
            data = post_nonstream(body)
            msg = data["choices"][0]["message"]
            content = msg.get("content") or ""
            calls = [{"id": tc.get("id", ""), "name": tc["function"]["name"],
                      "args": tc["function"].get("arguments") or "{}"}
                     for tc in msg.get("tool_calls") or []]

        if not calls:
            return content, total_calls

        total_calls += len(calls)
        # Append the assistant turn with tool calls, then each tool result.
        if mode == "stream":
            messages.append({"role": "assistant", "content": content or None,
                "tool_calls": [{"id": c["id"], "type": "function",
                                "function": {"name": c["name"], "arguments": c["args"]}}
                               for c in calls]})
        else:
            messages.append(data["choices"][0]["message"])
        for c in calls:
            try:
                args = json.loads(c["args"])
            except json.JSONDecodeError:
                result = "ERROR: arguments are not valid JSON: " + c["args"][:200]
            else:
                result = execute_tool(c["name"], args)
            messages.append({"role": "tool", "tool_call_id": c["id"], "content": result})
    raise RuntimeError("task did not finish in %d turns" % MAX_TURNS)

# ------------------------------------------------------------------ tasks ---

def reset_ws(files=None):
    shutil.rmtree(WS, ignore_errors=True)
    os.makedirs(WS, exist_ok=True)
    for rel, content in (files or {}).items():
        p = ws_path(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w", encoding="utf-8") as f:
            f.write(content)

RESULTS = []

def check(name, cond, detail=""):
    status = "PASS" if cond else "FAIL"
    RESULTS.append((name, status, detail))
    print(f"[{status}] {name}" + (f" — {detail}" if detail and not cond else ""))

def task1():
    """Explore + fix a bug: requires list_directory, read_file, edit_file, verification."""
    reset_ws({"app.py": "def add(a, b):\n    return a - b  # BUG: should be +\n",
              "util.py": "def greet(name):\n    return 'hello ' + name\n"})
    text, calls = run_task(
        "In the workspace there is app.py containing a buggy `add` function. "
        "Find the bug by reading the file, fix it so add(2,3) returns 5, "
        "then verify by running a bash command that prints add(2,3). "
        "Report the fixed line when done.")
    src = open(ws_path("app.py")).read()
    check("task1: bug fixed in app.py", "return a + b" in src and "return a - b" not in src, repr(src))
    check("task1: multi-turn tool usage", calls >= 2, f"calls={calls}")
    check("task1: final answer mentions the fix", "a + b" in text, text[:200])

def task2():
    """Create a small project from scratch: write_file + bash verification."""
    reset_ws()
    text, calls = run_task(
        "Create a file fib.py containing a function fib(n) that returns the "
        "n-th Fibonacci number (fib(0)=0, fib(1)=1). Then run it with bash to "
        "print fib(10) and confirm the output is 55. "
        "Answer DONE when verified.")
    src = open(ws_path("fib.py")).read()
    check("task2: fib.py created", os.path.exists(ws_path("fib.py")) and "fib" in src)
    check("task2: model verified via bash", calls >= 2, f"calls={calls}")
    check("task2: confirmed 55 / DONE", ("55" in text) or ("DONE" in text.upper()), text[:200])

def task3():
    """Refactor task requiring multiple files + grep-like exploration."""
    reset_ws({"a.txt": "alpha beta gamma\n", "b.txt": "beta delta\n",
              "c.txt": "gamma epsilon zeta\n"})
    text, calls = run_task(
        "List the workspace, read every .txt file, then create summary.txt "
        "containing one line per distinct word found across all txt files, "
        "sorted alphabetically. Verify with bash (cat summary.txt). "
        "Then answer with the word count.")
    summary = open(ws_path("summary.txt")).read() if os.path.exists(ws_path("summary.txt")) else ""
    words = [w for w in summary.split() if w.isalpha()]
    check("task3: summary.txt created", len(words) >= 6, repr(summary[:200]))
    check("task3: words sorted alphabetically", words == sorted(words), repr(words))

def task4_nonstream():
    """Same as task1 but through the NON-STREAMING path (different bridge code)."""
    reset_ws({"calc.py": "def mul(a, b):\n    return a + b\n"})
    text, calls = run_task(
        "calc.py has a broken mul function. Read it, fix so mul(3,4)=12, "
        "verify with bash, then answer with the fixed line.",
        mode="nonstream")
    src = open(ws_path("calc.py")).read()
    check("task4(nonstream): fixed", "return a * b" in src, repr(src))

def task5_anthropic():
    """Anthropic /v1/messages with tool use — the second protocol surface."""
    reset_ws({"note.txt": "hello world\n"})
    req = urllib.request.Request(BASE + "/v1/messages",
        data=json.dumps({"model": MODEL, "max_tokens": 1024, "stream": False,
            "messages": [{"role": "user", "content": "Read note.txt and tell me exactly what it contains. Use the read_file tool."}],
            "tools": [{"name": "read_file", "description": "Read a file",
                        "input_schema": {"type": "object", "properties": {"path": {"type": "string"}},
                                         "required": ["path"]}}]}).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN,
                 "anthropic-version": "2023-06-01"})
    with urllib.request.urlopen(req, timeout=180) as r:
        data = json.loads(r.read())
    blocks = data.get("content", [])
    tool_uses = [b for b in blocks if b.get("type") == "tool_use"]
    if not tool_uses:
        check("task5(anthropic): tool_use block", False, json.dumps(data)[:300])
        return
    tu = tool_uses[0]
    result = execute_tool(tu["name"], tu.get("input") or {})
    req2 = urllib.request.Request(BASE + "/v1/messages",
        data=json.dumps({"model": MODEL, "max_tokens": 1024, "stream": False,
            "messages": [
                {"role": "user", "content": "Read note.txt and tell me exactly what it contains. Use the read_file tool."},
                {"role": "assistant", "content": blocks},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tu["id"], "content": result}]}],
            "tools": [{"name": "read_file", "description": "Read a file",
                        "input_schema": {"type": "object", "properties": {"path": {"type": "string"}},
                                         "required": ["path"]}}]}).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN,
                 "anthropic-version": "2023-06-01"})
    with urllib.request.urlopen(req2, timeout=180) as r:
        data2 = json.loads(r.read())
    text2 = "".join(b.get("text", "") for b in data2.get("content", []))
    check("task5(anthropic): tool_use + result round-trip", "hello world" in text2, text2[:200])

if __name__ == "__main__":
    only = sys.argv[1] if len(sys.argv) > 1 else "all"
    t0 = time.time()
    try:
        if only in ("all", "1"): task1()
        if only in ("all", "2"): task2()
        if only in ("all", "3"): task3()
        if only in ("all", "4"): task4_nonstream()
        if only in ("all", "5"): task5_anthropic()
    except Exception as e:
        check("harness exception", False, repr(e))
    fails = [r for r in RESULTS if r[1] == "FAIL"]
    print(f"\n=== E2E {len(RESULTS)-len(fails)}/{len(RESULTS)} passed in {time.time()-t0:.0f}s ===")
    sys.exit(1 if fails else 0)
