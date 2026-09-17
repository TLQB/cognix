package zbridge

// Replay the EXACT failing proxy body directly against Z.AI (bypassing
// the proxy HTTP layer): isolates whether the INTERNAL_ERROR comes from
// the body content itself or from proxy plumbing (chat_id, session,
// headers). The body is captured from LOG_LEVEL=debug output of the
// native proxy on the pivot case that fails 5/6.
//
// Variants replayed:
//   1. as-captured (chat_id intact, fresh captcha)
//   2. chat_id removed (stateless-style, like the original spike)
//
// Run: EXP_MULTITURN=1 ZAI_TOKEN=... go test ./internal/zbridge/ -run TestExpReplayPivotBody -v -count=1

import (
	"encoding/json"
	"os"
	"testing"
)

const expReplayBody = `{"model":"glm-4.7","chat_id":"replay-chat","messages":[
{"role":"user","content":"<system>\nYou are a helpful assistant with access to tools. Follow these rules strictly:\n\nREPLY FORMAT — exactly ONE of:\n(A) TOOL CALL: <<<TOOL_CALL>>>{\"name\":\"<tool_name>\",\"arguments\":{<parameter JSON>}}<<<END_TOOL_CALL>>> — nothing before or after.\n    The JSON object has EXACTLY two keys: \"name\" (the tool to call, spelled exactly as in <tools>) and \"arguments\" (an object with ONLY that tool's parameters).\n(B) FINAL ANSWER: plain text, only when no tool applies.\n\n<tools>\n### Tool 1: read_file — Read a file's content\nParameters (JSON object):\n{\"properties\":{\"path\":{\"type\":\"string\"}},\"required\":[\"path\"],\"type\":\"object\"}\n\n### Tool 2: calculate — Evaluate an arithmetic expression\nParameters (JSON object):\n{\"properties\":{\"expr\":{\"type\":\"string\"}},\"required\":[\"expr\"],\"type\":\"object\"}\n\n</tools>\n<output_rules>\nRESPOND WITH EXACTLY ONE OF:\n1. <<<TOOL_CALL>>>{\"name\":\"<tool_name>\",\"arguments\":{...}}<<<END_TOOL_CALL>>> (no fences, no other text)\n2. Plain text final answer (only if NO tool applies to this step — announcing a plan is NOT a final answer)\n</output_rules>"},
{"role":"user","content":"Read main.go and summarize it."},
{"role":"assistant","content":"","tool_calls":[{"id":"m1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"main.go\"}"}}]},
{"role":"tool","content":"package main\nfunc main() { println('hi') }","tool_call_id":"m1"},
{"role":"user","content":"Actually, forget main.go — what is 999/3? Use calculate."}
],"signature_prompt":"x","stream":true,"features":{"flags":[],"image_generation":false}}`

func TestExpReplayPivotBody(t *testing.T) {
	if os.Getenv("EXP_MULTITURN") == "" {
		t.Skip("set EXP_MULTITURN=1 to run this experimental live probe")
	}
	verbose = true
	dbPath = "../../tokens.sqlite"
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	if config.ZaiToken == "" {
		t.Fatal("ZAI_TOKEN not set")
	}

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(expReplayBody), &body); err != nil {
		t.Fatalf("parse captured body: %v", err)
	}
	pivot := "Actually, forget main.go — what is 999/3? Use calculate."

	// Variant 1: as-captured (chat_id present), fresh captcha, stream:true.
	t.Run("with_chat_id", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			b := deepCopyBody(t, body)
			b["captcha_verify_param"] = mustCaptchaParam(t)
			code, out := expCaptchaChat(t, expTestClient(), b, pivot)
			t.Logf("run %d: HTTP %d | %s", i+1, code, firstLine(out))
			if code != 200 {
				t.Errorf("run %d: with chat_id got %d", i+1, code)
			}
		}
	})

	// Variant 2: chat_id removed.
	t.Run("no_chat_id", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			b := deepCopyBody(t, body)
			delete(b, "chat_id")
			b["captcha_verify_param"] = mustCaptchaParam(t)
			code, out := expCaptchaChat(t, expTestClient(), b, pivot)
			t.Logf("run %d: HTTP %d | %s", i+1, code, firstLine(out))
			if code != 200 {
				t.Errorf("run %d: no chat_id got %d", i+1, code)
			}
		}
	})
}

func deepCopyBody(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}
