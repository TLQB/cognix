#!/usr/bin/env python3
"""Reproduce the native-variant INTERNAL_ERROR on pivot_mid_loop in
isolation, dumping the exact messages array that triggered it."""
import json, sys, urllib.request, urllib.error

BASE = "http://localhost:3013/v1/chat/completions"
TOOLS = [
    {"type": "function", "function": {
        "name": "read_file", "description": "Read a file's content",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}},
                       "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "calculate", "description": "Evaluate an arithmetic expression",
        "parameters": {"type": "object", "properties": {"expr": {"type": "string"}},
                       "required": ["expr"]}}},
]

messages = [
    {"role": "user", "content": "Read main.go and summarize it."},
    {"role": "assistant", "content": "", "tool_calls": [
        {"id": "m1", "type": "function",
         "function": {"name": "read_file", "arguments": "{\"path\": \"main.go\"}"}}]},
    {"role": "tool", "tool_call_id": "m1", "content": "package main\nfunc main() { println('hi') }"},
    {"role": "user", "content": "Actually, forget main.go — what is 999/3? Use calculate."},
]

body = {"model": "glm-4.7", "messages": messages, "tools": TOOLS, "stream": False}
print("=== messages array ===")
print(json.dumps(messages, indent=1)[:800])
req = urllib.request.Request(BASE, json.dumps(body).encode(),
                             {"Content-Type": "application/json",
                              "Authorization": "Bearer Waguri"})
try:
    with urllib.request.urlopen(req, timeout=120) as r:
        data = json.load(r)
    m = data["choices"][0]["message"]
    print("OK content:", (m.get("content") or "")[:150])
    print("OK tool_calls:", json.dumps(m.get("tool_calls") or [])[:150])
except urllib.error.HTTPError as e:
    print("HTTP", e.code, e.read()[:300])
