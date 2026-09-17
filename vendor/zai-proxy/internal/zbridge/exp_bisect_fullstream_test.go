package zbridge

// Bisect the pivot-case INTERNAL_ERROR with FULL-stream reads. All earlier
// bisect probes used expCaptchaChat, which reads only the first 2500 bytes —
// but the error event arrives at the END of a healthy 200 stream, so every
// earlier "PASS" on body-shape suspects was potentially a false positive.
//
// Probes (each 4 runs, full stream drained, inline error detected):
//   A  user/assistant/user — pure text multi-turn, no tool role
//   B  user → assistant+tool_calls → tool             (spike B shape)
//   C  exact pivot body from fail-body2.json          (known bad, control)
//   D  pivot but tool turn folded into a user message (no tool role)
//   E  pivot with the two head users merged into one
//
// Run: EXP_MULTITURN=1 ZAI_TOKEN=... go test ./internal/zbridge/ -run TestExpBisectFullStream -v -count=1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fullStreamProbe(t *testing.T, name string, body map[string]interface{}, prompt string, runs int) {
	t.Helper()
	fails := 0
	for i := 0; i < runs; i++ {
		b := deepCopyBody(t, body)
		b["captcha_verify_param"] = mustCaptchaParam(t)
		code, errSeen, _, events, answer := expStreamChat(t, expTestClient(), b, prompt)
		emittedToolCall := strings.Contains(answer, "TOOL_CALL")
		t.Logf("%s run %d: HTTP %d | events=%d | errSeen=%v | toolCallEmitted=%v | answer=%q",
			name, i+1, code, events, errSeen, emittedToolCall, truncStr(strings.TrimSpace(answer), 1200))
		if errSeen || code != 200 {
			fails++
		}
	}
	if fails > 0 {
		t.Errorf("%s: FAIL %d/%d runs (inline error or non-200)", name, fails, runs)
	} else {
		t.Logf("%s: PASS %d/%d", name, runs, runs)
	}
}

func TestExpBisectFullStream(t *testing.T) {
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

	// Load the captured failing body once.
	raw, err := os.ReadFile("/tmp/ab-bench/fail-body2.json")
	if err != nil {
		t.Fatalf("read captured body: %v", err)
	}
	var pivot map[string]interface{}
	if err := json.Unmarshal(raw, &pivot); err != nil {
		t.Fatalf("parse pivot: %v", err)
	}

	// C: control — the captured pivot body must reproduce (5/6 in prior runs).
	t.Run("C_pivot_control", func(t *testing.T) {
		fullStreamProbe(t, "C", pivot, mustString(pivot, "signature_prompt"), 4)
	})

	// A: pure text multi-turn, no tool role, no contract head.
	t.Run("A_text_multiturn", func(t *testing.T) {
		body := map[string]interface{}{
			"model": "glm-4.7",
			"messages": []map[string]interface{}{
				{"role": "user", "content": "Say exactly: ALPHA"},
				{"role": "assistant", "content": "ALPHA"},
				{"role": "user", "content": "Now say exactly: BRAVO"},
			},
			"signature_prompt": "Now say exactly: BRAVO",
			"stream":           true,
			"features":         map[string]interface{}{"flags": []interface{}{}, "image_generation": false},
		}
		fullStreamProbe(t, "A", body, "Now say exactly: BRAVO", 4)
	})

	// B: spike-B shape — assistant tool_calls + tool role message.
	t.Run("B_tool_role", func(t *testing.T) {
		body := map[string]interface{}{
			"model": "glm-4.7",
			"messages": []map[string]interface{}{
				{"role": "user", "content": "What is 2+2? Use the calculator tool."},
				{"role": "assistant", "content": "", "tool_calls": []map[string]interface{}{
					{"id": "call_1", "type": "function", "function": map[string]interface{}{
						"name": "calculator", "arguments": "{\"expr\": \"2+2\"}",
					}},
				}},
				{"role": "tool", "content": "4", "tool_call_id": "call_1"},
				{"role": "user", "content": "Thanks. Now say exactly: BRAVO."},
			},
			"signature_prompt": "Thanks. Now say exactly: BRAVO.",
			"stream":           true,
			"features":         map[string]interface{}{"flags": []interface{}{}, "image_generation": false},
		}
		fullStreamProbe(t, "B", body, "Thanks. Now say exactly: BRAVO.", 4)
	})

	// D: pivot messages, but the tool turn is folded into a user message —
	// isolates the tool ROLE from the pivot content.
	t.Run("D_tool_folded", func(t *testing.T) {
		b := deepCopyBody(t, pivot)
		msgs := b["messages"].([]interface{})
		// Replace assistant tool_calls turn and tool turn with one user turn.
		out := []interface{}{msgs[0]} // contract head
		folded := map[string]interface{}{"role": "user", "content": "Read main.go and summarize it.\n\n[tool read_file(main.go) returned]\npackage main\nfunc main() { println('hi') }\n\nActually, forget main.go — what is 999/3? Use calculate."}
		out = append(out, folded)
		b["messages"] = out
		fullStreamProbe(t, "D", b, mustString(pivot, "signature_prompt"), 4)
	})

	// E: pivot with the two head user messages merged into one — isolates
	// the consecutive-user head from the tool-role turns.
	t.Run("E_head_merged", func(t *testing.T) {
		b := deepCopyBody(t, pivot)
		msgs := b["messages"].([]interface{})
		head := mustString(msgs[0].(map[string]interface{}), "content")
		u1 := mustString(msgs[1].(map[string]interface{}), "content")
		merged := map[string]interface{}{"role": "user", "content": head + "\n\n" + u1}
		out := append([]interface{}{merged}, msgs[2:]...)
		b["messages"] = out
		fullStreamProbe(t, "E", b, mustString(pivot, "signature_prompt"), 4)
	})
	// F: contract head + tool roles, but trivial final user (no pivot content) —
	// isolates the head+tool-roles combination from the pivot's specific text.
	t.Run("F_head_toolroles_trivial", func(t *testing.T) {
		pivMsgs, ok := pivot["messages"].([]interface{})
		if !ok || len(pivMsgs) < 5 {
			t.Fatalf("bad pivot shape")
		}
		head := pivMsgs[0].(map[string]interface{})["content"].(string)
		b := map[string]interface{}{
			"model": "glm-4.7",
			"messages": []interface{}{
				pivMsgs[0], // contract head (as captured)
				pivMsgs[1], // user "Read main.go..."
				pivMsgs[2], // assistant + tool_calls
				pivMsgs[3], // tool result
				map[string]interface{}{"role": "user", "content": "Thanks. Now say exactly: BRAVO."},
			},
			"signature_prompt": head,
			"stream":           true,
			"features":         map[string]interface{}{"flags": []interface{}{}, "image_generation": false},
		}
		fullStreamProbe(t, "F", b, head, 4)
	})
	// G: pivot shape but NO chat_id — tests whether Z.AI server-side chat
	// state is required for (or the cause of) the failure.
	t.Run("G_no_chat_id", func(t *testing.T) {
		b := deepCopyBody(t, pivot)
		delete(b, "chat_id")
		fullStreamProbe(t, "G", b, mustString(pivot, "signature_prompt"), 4)
	})

	// H: tool history + a final user that triggers a NEW tool call, but with
	// different text than the pivot — isolates "new tool call after tool
	// history" from the specific pivot wording.
	t.Run("H_toolcall_different_text", func(t *testing.T) {
		pivMsgs, ok := pivot["messages"].([]interface{})
		if !ok || len(pivMsgs) < 5 {
			t.Fatalf("bad pivot shape")
		}
		head := pivMsgs[0].(map[string]interface{})["content"].(string)
		b := map[string]interface{}{
			"model": "glm-4.7",
			"messages": []interface{}{
				pivMsgs[0],
				pivMsgs[1],
				pivMsgs[2],
				pivMsgs[3],
				map[string]interface{}{"role": "user", "content": "Now read the file config.yaml using read_file."},
			},
			"signature_prompt": head,
			"stream":           true,
			"features":         map[string]interface{}{"flags": []interface{}{}, "image_generation": false},
		}
		fullStreamProbe(t, "H", b, head, 4)
	})
}

func mustString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return "x"
}
