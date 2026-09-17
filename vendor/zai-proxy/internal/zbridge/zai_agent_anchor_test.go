package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// mkMsg builds an agentMessage from a plain content string.
func mkMsg(role, content string) agentMessage {
	raw, _ := json.Marshal(content)
	return agentMessage{Role: role, Content: json.RawMessage(raw)}
}

// mkToolMsg builds an assistant message carrying one tool call + its result.
func mkToolMsg(name string) (agentMessage, agentMessage) {
	rawArgs, _ := json.Marshal(map[string]interface{}{"x": 1})
	call := assistantToolCall{ID: "call_1", Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = json.RawMessage(rawArgs)
	assistant := agentMessage{Role: "assistant", ToolCalls: []assistantToolCall{call}}
	result := mkMsg("tool", "output of "+name)
	return assistant, result
}

// TestCurrentTaskAnchorMidTask guards the production-log bug where the last
// user message was re-presented verbatim as <current_task> even when tool
// results followed it, making the model re-run the identical tool call
// forever ("build lại đi" loop).
func TestCurrentTaskAnchorMidTask(t *testing.T) {
	assistant, result := mkToolMsg("build")
	msgs := []agentMessage{
		mkMsg("user", "build it again"),
		assistant,
		result,
	}
	prompt := buildAgentPrompt(msgs, nil)

	if !strings.Contains(prompt, "<current_task>") {
		t.Fatal("prompt missing <current_task> section")
	}
	if strings.Contains(prompt, "User's request: \"build it again\"") == false {
		t.Fatal("mid-task anchor must restate the user request, not drop it")
	}
	if !strings.Contains(prompt, "Continue the user's task") {
		t.Fatal("mid-task anchor must tell the model to continue from tool results")
	}
	if !strings.Contains(prompt, "Do NOT repeat a tool call") {
		t.Fatal("mid-task anchor must forbid repeating completed tool calls")
	}

	// Sanity: when the user message IS the last message (normal turn), the
	// anchor stays verbatim.
	prompt2 := buildAgentPrompt([]agentMessage{mkMsg("user", "hello there")}, nil)
	if !strings.Contains(prompt2, "hello there") {
		t.Fatal("fresh-turn anchor must contain the verbatim user message")
	}
	if strings.Contains(prompt2, "User's request:") {
		t.Fatal("fresh-turn anchor must not use the mid-task wrapper")
	}
}

// TestExtractToolExchangesKeepsInterleavedUsers guards the fix where user
// messages interleaved with summarized tool exchanges were dropped from the
// recent window, losing task instructions mid-conversation.
func TestExtractToolExchangesKeepsInterleavedUsers(t *testing.T) {
	// 8 exchanges (> maxRecentToolExchanges=6) with a user instruction stuck
	// between exchange 0 and exchange 1 — it falls in the summarized region.
	var msgs []agentMessage
	msgs = append(msgs, mkMsg("user", "analyze the project"))
	for i := 0; i < 8; i++ {
		a, r := mkToolMsg("tool")
		msgs = append(msgs, a, r)
		if i == 0 {
			msgs = append(msgs, mkMsg("user", "IMPORTANT: focus on the parser"))
		}
	}

	old, recent := extractToolExchanges(msgs)
	if len(old) == 0 {
		t.Fatal("expected old exchanges to be summarized")
	}

	var combined strings.Builder
	for _, m := range recent {
		b, _ := json.Marshal(m)
		combined.Write(b)
	}
	if !strings.Contains(combined.String(), "IMPORTANT: focus on the parser") {
		t.Fatal("interleaved user instruction was dropped from the recent window")
	}
}
