package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// Native variant: text turns keep REAL multi-turn roles upstream, but tool
// exchanges are DEFUSED into text. On the GLM upstream (chat.z.ai,
// live-probed 2026-09-10) the wire tool_calls / role:"tool" format gets
// re-framed into <glm_block> blocks in the model context; the model then
// emits its new tool calls in that poisoned format, which the backend
// rejects with INTERNAL_ERROR at end-of-stream (pivot: 4/4 crash with
// glm_block emission vs 4/4 pass text-folded). Unverified on chat.qwen.ai —
// the defuse is kept defensively. These tests pin the transform shape
// without network access.

func withAgentNative(t *testing.T) {
	t.Helper()
	oldMode, oldVariant := config.AgentMode, config.AgentModeVariant
	config.AgentMode = true
	config.AgentModeVariant = "native"
	t.Cleanup(func() {
		config.AgentMode, config.AgentModeVariant = oldMode, oldVariant
	})
}

func TestTransformMessagesForAgentNative(t *testing.T) {
	withAgentNative(t)

	raw := json.RawMessage(`[
		{"role":"system","content":"You are terse."},
		{"role":"user","content":"List the files."},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"list_directory","arguments":"{\"path\":\".\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"file_a\nfile_b"},
		{"role":"assistant","content":"2 files found."},
		{"role":"user","content":"Now count them."}
	]`)
	tools := json.RawMessage(`[{"type":"function","function":{"name":"list_directory","parameters":{"type":"object"}}}]`)

	out, err := transformMessagesForAgentNative(raw, tools)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var msgs []agentMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	// 1 contract + system-fold + user + defused-assistant + defused-tool +
	// assistant + user = 7 messages.
	if len(msgs) != 7 {
		t.Fatalf("expected 7 messages, got %d: %s", len(msgs), out)
	}

	// First message = contract: rules + tool list.
	if msgs[0].Role != "user" {
		t.Fatalf("contract message role = %s, want user", msgs[0].Role)
	}
	contract := contentToText(msgs[0].Content)
	if !strings.Contains(contract, "<<<TOOL_CALL>>>") {
		t.Error("contract missing TOOL_CALL marker rules")
	}
	if !strings.Contains(contract, "### Tool 1: list_directory") {
		t.Error("contract missing rendered tool schema")
	}

	// System turn folded as user, content preserved.
	if msgs[1].Role != "user" || contentToText(msgs[1].Content) != "You are terse." {
		t.Errorf("system fold wrong: role=%s content=%q", msgs[1].Role, contentToText(msgs[1].Content))
	}

	// Text user turn passes through with role intact.
	if msgs[2].Role != "user" || contentToText(msgs[2].Content) != "List the files." {
		t.Errorf("user turn wrong: %+v", msgs[2])
	}

	// Assistant tool_calls turn DEFUSED: role stays assistant, but the wire
	// tool_calls field is gone and the call is rendered as a text block.
	if msgs[3].Role != "assistant" || len(msgs[3].ToolCalls) != 0 {
		t.Errorf("assistant tool_calls must be defused: %+v", msgs[3])
	}
	defusedCall := contentToText(msgs[3].Content)
	if !strings.Contains(defusedCall, "<<<TOOL_CALL>>>") || !strings.Contains(defusedCall, `"name":"list_directory"`) {
		t.Errorf("defused assistant turn missing rendered call block: %q", defusedCall)
	}

	// Tool result DEFUSED: role becomes user (never "tool" upstream), framed
	// as <tool_result> with the call_id, content preserved.
	if msgs[4].Role != "user" || msgs[4].ToolCallID != "" {
		t.Errorf("tool message must be defused to a user message: %+v", msgs[4])
	}
	defusedResult := contentToText(msgs[4].Content)
	if !strings.Contains(defusedResult, `<tool_result call_id="c1">`) {
		t.Errorf("defused tool result missing call_id framing: %q", defusedResult)
	}
	if !strings.Contains(defusedResult, "file_a\nfile_b") {
		t.Errorf("defused tool result content wrong: %q", defusedResult)
	}

	// Later text turns untouched.
	if msgs[5].Role != "assistant" || contentToText(msgs[5].Content) != "2 files found." {
		t.Errorf("assistant turn wrong: %+v", msgs[5])
	}
	if msgs[6].Role != "user" || contentToText(msgs[6].Content) != "Now count them." {
		t.Errorf("final user turn wrong: %+v", msgs[6])
	}
}

func TestTransformMessagesForAgentNativeDispatch(t *testing.T) {
	withAgentNative(t)
	// agentTransformMessages must route to the native transform.
	out, err := agentTransformMessages(
		json.RawMessage(`[{"role":"user","content":"hi"}]`),
		json.RawMessage("null"),
	)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var msgs []agentMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Native always prepends the contract: 1 + 1 = 2 messages.
	if len(msgs) != 2 {
		t.Fatalf("native dispatch produced %d messages, want 2 (contract + turn): %s", len(msgs), out)
	}
}

func TestTransformMessagesForAgentNativeUnknownRole(t *testing.T) {
	withAgentNative(t)
	out, err := transformMessagesForAgentNative(
		json.RawMessage(`[{"role":"weird","content":"x"}]`),
		json.RawMessage("null"),
	)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var msgs []agentMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msgs[1].Role != "user" {
		t.Errorf("unknown role should default to user, got %s", msgs[1].Role)
	}
}

// The defused assistant turn must round-trip: the rendered <<<TOOL_CALL>>>
// block is exactly the format ParseAgentToolCalls understands, so a later
// fold of the conversation (e.g. modern shim on retry) sees the same calls.
func TestAgentNativeDefusedRoundTrip(t *testing.T) {
	withAgentNative(t)
	raw := json.RawMessage(`[
		{"role":"assistant","content":"","tool_calls":[{"id":"c9","type":"function","function":{"name":"calculate","arguments":"{\"expr\":\"1+1\"}"}}]}
	]`)
	out, err := transformMessagesForAgentNative(raw, json.RawMessage("null"))
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var msgs []agentMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	calls := ParseAgentToolCalls(contentToText(msgs[1].Content))
	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d from %q", len(calls), contentToText(msgs[1].Content))
	}
	fn := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "calculate" {
		t.Errorf("recovered name = %v, want calculate", fn["name"])
	}
}

// agentNative() must take precedence in agentTransformMessages even though
// agentModern() is also true for variant=native (it only excludes legacy).
func TestAgentNativePrecedenceOverModern(t *testing.T) {
	withAgentNative(t)
	if !config.agentModern() {
		t.Fatal("sanity: native must still count as modern-family for output parsing")
	}
	if !config.agentNative() {
		t.Fatal("agentNative() must be true for variant=native")
	}
	// And a plain modern config must not claim native.
	config.AgentModeVariant = ""
	if config.agentNative() {
		t.Fatal("variant='' must not be native")
	}
}
