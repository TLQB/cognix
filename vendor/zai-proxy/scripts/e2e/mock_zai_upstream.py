#!/usr/bin/env python3
"""
Mock Z.AI upstream for CI e2e: mimics the chat.z.ai surface the bridge uses.

Serves (all on 127.0.0.1:$PORT):
  GET  /                       -> 200 (cookie warmup)
  GET  /api/v1/auths/          -> guest identity JSON
  GET  /api/models             -> static GLM model list
  POST /api/v2/chat/completions -> SSE stream shaped like Z.AI
  POST /api/v1/chats           -> create chat id (session pool warmup)
  DELETE /api/v1/chats/<id>     -> {"success": true}

AGENT PROMPT SHAPE (see internal/zbridge/agent.go buildAgentPrompt): the
bridge folds the WHOLE conversation into ONE user message with XML-ish
sections, so the mock can never see role="tool" messages. State must be
derived from the prompt itself each turn:

  <current_task>   — the task (or a "Continue the user's task" wrapper)
  <recent>         — recent turns; tool turns are grouped as
                     <tool_exchange> ... <tool_result ...>res</tool_result> ...
  <already_called> — "- name {args}" per call ever made (deduped)

The mock is a deterministic scripted agent over that state:

  turn 1 (no calls yet):
    - task mentions fib          -> write_file fib.py (hardcoded source)
    - task mentions summary.txt -> run_bash "cat *.txt"
    - else                       -> read_file the first *.py/*.txt mentioned
  later turns, keyed on the LAST call made:
    read_file  -> edit_file (flip the buggy line: "- b"->"+ b", or
                  "+ b"->"* b" when the task says mul), or final echo
                  for plain reads (note.txt)
    edit_file  -> run_bash verifying the fixed function
    write_file -> run_bash fib.py | final summary word count
    run_bash   -> final answer derived from the output (55/DONE, fixed
                  line echoed from the edit args, or summary.txt words
                  written from the cat output)

This exercises the full bridge tool loop: OpenAI tools in -> text protocol
out, tool_calls deltas back, multi-turn folding, already_called anti-loop
rendering, and the non-stream + Anthropic surfaces (same upstream shape).
A >=6-call safety valve always ends with a final answer so a bridge bug
surfaces as a FAILED assertion, never a 24-turn hang.
"""
import json, re, sys, uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 39998

MODELS = {"data": [{"id": "glm-4.7", "name": "GLM-4.7",
  "info": {"name": "GLM-4.7", "meta": {"description": "mock",
  "capabilities": {"enable_thinking": True}}}}]}

FIB_SRC = ("def fib(n):\n"
           "    a, b = 0, 1\n"
           "    for _ in range(n):\n"
           "        a, b = b, a + b\n"
           "    return a\n"
           "\n"
           "print(fib(10))\n")

# ------------------------------------------------------------------ state ---

def parse_state(prompt):
    """Extract (task, calls, results) from the prompt.

    Supports BOTH agent-variant wire shapes:
      - modern fold: one user message with <current_task>/<already_called>/
        <tool_result> XML sections
      - native multi-turn: real user/assistant/tool messages, tool exchanges
        defused to text (assistant turn carries <<<TOOL_CALL>>> blocks, tool
        results arrive as user messages framed by <tool_result ...>)
    """
    # Native multi-turn: reconstruct from the message list. The bridge's
    # native transform keeps separate messages, so parse them directly.
    def _parse_messages(prompt):
        task = ""
        calls = []
        results = []
        for m in prompt if isinstance(prompt, list) else []:
            role = m.get("role", "")
            content = m.get("content")
            content = content if isinstance(content, str) else ""
            if role == "user":
                if "<<<TOOL_CALL>>>" in content or "<tool_result" in content:
                    # Defused assistant call block or tool result frame.
                    for cm in re.finditer(
                            r"<<<TOOL_CALL>>>\s*(\{.*?\})\s*<<<END_TOOL_CALL>>>",
                            content, re.S):
                        try:
                            p = json.loads(cm.group(1))
                            calls.append((p["name"], p.get("arguments") or {}))
                        except (json.JSONDecodeError, KeyError):
                            pass
                    for rm in re.finditer(
                            r"<tool_result[^>]*>\n?(.*?)\n?</tool_result>",
                            content, re.S):
                        results.append(rm.group(1))
                elif content.strip():
                    # Plain user text: the latest one is the current task,
                    # earlier ones fold into it (same task semantics).
                    task = content if not task else task + "\n" + content
            elif role == "assistant":
                if "<<<TOOL_CALL>>>" in content:
                    for cm in re.finditer(
                            r"<<<TOOL_CALL>>>\s*(\{.*?\})\s*<<<END_TOOL_CALL>>>",
                            content, re.S):
                        try:
                            p = json.loads(cm.group(1))
                            calls.append((p["name"], p.get("arguments") or {}))
                        except (json.JSONDecodeError, KeyError):
                            pass
        return task, calls, results

    if isinstance(prompt, list):
        return _parse_messages(prompt)

    # Modern fold: single text prompt with XML sections.
    task = prompt
    calls = []  # [(name, args_dict)] in already_called order
    m = re.search(r"<current_task>\n(.*?)\n</current_task>", prompt, re.S)
    task = m.group(1) if m else prompt
    m = re.search(r"<already_called>\n(.*?)</already_called>", prompt, re.S)
    if m:
        for line in m.group(1).splitlines():
            cm = re.match(r"- (\w+) (\{.*\})", line.strip())
            if cm:
                try:
                    calls.append((cm.group(1), json.loads(cm.group(2))))
                except json.JSONDecodeError:
                    pass
    results = re.findall(r"<tool_result[^>]*>\n(.*?)\n</tool_result>", prompt, re.S)
    return task, calls, results

def first_file_mentioned(task):
    m = re.search(r"\b([\w./-]+\.(?:py|txt))\b", task)
    return m.group(1) if m else None

def decide(prompt, msgs=None):
    """Return ("tool", name, args) or ("final", text).

    `prompt` is the messages list (native) or the joined text (modern fold,
    when called with a string). Normalize first, then decide.
    """
    # Normalize: whatever shape arrives, get (task, calls, results) plus a
    # plain-text view of the whole conversation for keyword probes.
    if isinstance(prompt, list):
        task, calls, results = parse_state(prompt)
        full_text = "\n".join(
            m.get("content") if isinstance(m.get("content"), str) else ""
            for m in prompt)
    else:
        task, calls, results = parse_state(prompt)
        full_text = prompt

    # Smoke path: plain echo, no tools.
    if "SMOKE_OK" in full_text:
        m = re.search(r"SMOKE_OK_?\w*", full_text)
        return "final", (m.group(0) if m else "SMOKE_OK")

    # Manual debug path: force one read_file note.txt round-trip.
    if "TOOL_MARK" in task:
        if results:
            return "final", "TOOL_RESULT: " + results[-1][:400]
        return "tool", "read_file", {"path": "note.txt"}

    # Safety valve: never let a bridge bug hang the harness for 24 turns.
    if len(calls) >= 6:
        return "final", "task complete"

    if not calls:
        if "fib" in task:
            return "tool", "write_file", {"path": "fib.py", "content": FIB_SRC}
        if "summary.txt" in task:
            return "tool", "run_bash", {"command": "cat *.txt"}
        f = first_file_mentioned(task)
        if f:
            return "tool", "read_file", {"path": f}
        return "final", "done"

    name, args = calls[-1]
    last = results[-1] if results else ""

    if name == "read_file":
        if "return a - b" in last:
            return "tool", "edit_file", {"path": args.get("path"),
                "old_text": "return a - b", "new_text": "return a + b"}
        if "return a + b" in last and "mul" in task:
            return "tool", "edit_file", {"path": args.get("path"),
                "old_text": "return a + b", "new_text": "return a * b"}
        # plain read (e.g. note.txt): answer with the content
        return "final", "The file contains: " + last.strip()[:200]

    if name == "edit_file":
        path = args.get("path", "app.py")
        mod = path.rsplit(".", 1)[0]
        fm = re.search(r"(\w+)\((\d+)\s*,\s*(\d+)\)", task)  # add(2,3)/mul(3,4)
        if fm:
            fn, x, y = fm.groups()
            cmd = "python3 -c 'from %s import %s; print(%s(%s,%s))'" % (mod, fn, fn, x, y)
        else:
            cmd = "cat " + path
        return "tool", "run_bash", {"command": cmd}

    if name == "write_file":
        if args.get("path") == "fib.py":
            return "tool", "run_bash", {"command": "python3 fib.py"}
        if args.get("path") == "summary.txt":
            n = len(args.get("content", "").split())
            return "final", "Created summary.txt with %d words." % n
        return "final", "done"

    if name == "run_bash":
        out = last.strip()
        if "55" in out:
            return "final", "Output: 55. DONE"
        if "5" in out or "12" in out:
            new_text = None
            for nm, a in calls:
                if nm == "edit_file":
                    new_text = a.get("new_text")
            return "final", "Verified — the function now prints the right " \
                "value. The fixed line is: %s" % (new_text or "fixed")
        if "alpha" in out or "beta" in out:  # cat *.txt result
            words = sorted(set(w for w in out.split() if w.isalpha()))
            return "tool", "write_file", {"path": "summary.txt",
                "content": "\n".join(words) + "\n"}
        return "final", "done: " + out[:100]

    return "final", "done"

# ----------------------------------------------------------------- server ---

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):  # quiet
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/":
            self._json({})
        elif self.path.startswith("/api/v1/auths"):
            self._json({"data": {"id": "mock-user", "email": "mock@local"}})
        elif self.path.startswith("/api/models"):
            self._json(MODELS)
        else:
            self._json({}, 404)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        if self.path.startswith("/api/v1/chats"):
            self._json({"data": str(uuid.uuid4())})
            return
        if not self.path.startswith("/api/v2/chat/completions"):
            self._json({}, 404)
            return
        self._stream(body)

    def do_DELETE(self):
        self._json({"success": True})

    def _sse(self, obj):
        self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")

    def _stream(self, body):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        msgs = body.get("messages") or []
        # Pass the message list through: parse_state handles both the modern
        # fold (single joined text) and the native multi-turn wire shape.
        prompt = msgs

        r = decide(prompt, msgs)
        if r[0] == "tool":
            _, name, tool_args = r
            text_out = ('<<<TOOL_CALL>>>{"name":"%s","arguments":%s}<<<END_TOOL_CALL>>>'
                        % (name, json.dumps(tool_args)))
        else:
            text_out = r[1]

        # Emit as edit_index + edit_content chunks, mirroring the real
        # upstream: content = content[:edit_index] + edit_content.
        pos = 0
        step = max(1, len(text_out) // 6)
        i = 0
        while pos < len(text_out):
            chunk = text_out[pos:pos+step]
            self._sse({"type": "chat:completion", "data": {
                "phase": "other", "edit_index": i, "edit_content": chunk}})
            pos += step
            i += step
        if not text_out:
            self._sse({"type": "chat:completion", "data": {
                "phase": "other", "edit_index": 0, "edit_content": ""}})
        self._sse({"type": "chat:completion", "data": {"phase": "done", "done": True}})

if __name__ == "__main__":
    HTTPServer(("127.0.0.1", PORT), H).serve_forever()
