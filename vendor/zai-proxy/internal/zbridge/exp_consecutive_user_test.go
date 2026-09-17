package zbridge

// Direct spike: is CONSECUTIVE user messages the INTERNAL_ERROR trigger?
// The native variant's upstream array starts [contract(user), first-user
// turn] — two user messages in a row. Z.AI's own web client never sends
// that. This probe sends the exact pivot case in two shapes:
//   1. two consecutive user messages at the head (native shape)
//   2. same content merged into ONE user message
// both against tools [read_file, calculate] — the combo that failed 5/6.
//
// Run: EXP_MULTITURN=1 ZAI_TOKEN=... go test ./internal/zbridge/ -run TestExpConsecutiveUser -v -count=1

import (
	"os"
	"testing"
)

func TestExpConsecutiveUser(t *testing.T) {
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

	contract := "You are a helpful assistant with tools. Reply with <<<TOOL_CALL>>>{\"name\":...,\"arguments\":{...}}<<<END_TOOL_CALL>>> blocks only."
	pivot := "Actually, forget main.go \u2014 what is 999/3? Use calculate."

	// Shape 1: native — two consecutive user messages at the head.
	t.Run("consecutive_user_head", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			body := expMultiturnBody([]map[string]interface{}{
				{"role": "user", "content": contract},
				{"role": "user", "content": "Read main.go and summarize it."},
				{"role": "assistant", "content": "", "tool_calls": []map[string]interface{}{
					{"id": "m1", "type": "function", "function": map[string]interface{}{
						"name": "read_file", "arguments": `{"path": "main.go"}`}},
				}},
				{"role": "tool", "content": "package main\nfunc main() { println('hi') }", "tool_call_id": "m1"},
				{"role": "user", "content": pivot},
			})
			body["captcha_verify_param"] = mustCaptchaParam(t)
			code, out := expCaptchaChat(t, expTestClient(), body, pivot)
			t.Logf("run %d: HTTP %d | %s", i+1, code, firstLine(out))
			if code != 200 {
				t.Errorf("run %d: consecutive-user head got %d", i+1, code)
			}
		}
	})

	// Shape 2: merged — contract + first turn in ONE user message.
	t.Run("merged_user_head", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			body := expMultiturnBody([]map[string]interface{}{
				{"role": "user", "content": contract + "\n\n---\n\nRead main.go and summarize it."},
				{"role": "assistant", "content": "", "tool_calls": []map[string]interface{}{
					{"id": "m1", "type": "function", "function": map[string]interface{}{
						"name": "read_file", "arguments": `{"path": "main.go"}`}},
				}},
				{"role": "tool", "content": "package main\nfunc main() { println('hi') }", "tool_call_id": "m1"},
				{"role": "user", "content": pivot},
			})
			body["captcha_verify_param"] = mustCaptchaParam(t)
			code, out := expCaptchaChat(t, expTestClient(), body, pivot)
			t.Logf("run %d: HTTP %d | %s", i+1, code, firstLine(out))
			if code != 200 {
				t.Errorf("run %d: merged head got %d", i+1, code)
			}
		}
	})
}

func mustCaptchaParam(t *testing.T) string {
	t.Helper()
	p, err := getCaptchaVerifyParam()
	if err != nil {
		t.Fatalf("captcha param: %v", err)
	}
	return p
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '|' {
			return s[:i]
		}
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
