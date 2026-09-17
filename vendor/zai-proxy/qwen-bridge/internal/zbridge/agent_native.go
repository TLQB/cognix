package zbridge

// Native agent-mode variant (validated by the EXP_MULTITURN spike,
// 2026-09-10): Qwen's /api/v2/chat/completions accepts a NATIVE
// multi-turn messages array — user/assistant turns alternating with real
// roles, and the model reads the full history (prompt_tokens scale with it).
//
// Unlike the "modern" shim, which folds the whole conversation into ONE
// XML-sectioned user message (buildAgentPrompt + wrapAgentPromptAsMessages),
// the native variant:
//
//   - keeps the client's text turns as real multi-turn messages upstream
//   - prepends ONE user message carrying the system rules + tool contract
//     (Qwen does no system role; the first user message is the convention
//     the modern shim already uses)
//
// BUT tool exchanges must NOT be sent in the OpenAI wire format
// (assistant.tool_calls / role:"tool"):
//
//   - On the GLM upstream (chat.z.ai, live-probed 2026-09-10) wire
//     tool_calls get re-framed by the backend into <glm_block> blocks in
//     the model context, and the model then IMITATES that poisoned framing
//     for its NEW tool calls (pivot: <glm_block tool_call_name=...>
//     emitted, INTERNAL_ERROR at end-of-stream, 4/4 runs; text-folded
//     control passed 4/4). The same wire format is not verified on
//     chat.qwen.ai — but wire tool_calls are unnecessary for the text
//     protocol either way, so the defuse is kept defensively.
//
// So native defuses tool exchanges into text: the assistant turn keeps its
// text plus rendered <<<TOOL_CALL>>> blocks, and the tool result becomes a
// user message in the modern shim's <tool_result> framing. The multi-turn
// benefit (real assistant/user alternation for TEXT) is preserved; only the
// poison wire format is removed.
//
// The OUTPUT side is unchanged: the model still emits <<<TOOL_CALL>>>
// blocks, parsed by the modern interceptor (agentExtractToolCalls /
// newAgentInterceptor route on agentModern() which native extends).

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// transformMessagesForAgentNative builds a native multi-turn messages
// array: [rules+tools user message] + client turns mapped as-is.
func transformMessagesForAgentNative(rawMessages, toolsRaw json.RawMessage) ([]byte, error) {
	var msgs []agentMessage
	if err := json.Unmarshal(rawMessages, &msgs); err != nil {
		return nil, fmt.Errorf("agent transform (native): parse messages: %w", err)
	}
	var tools []openAITool
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}

	out := make([]agentMessage, 0, len(msgs)+1)

	// 1. The contract message: system rules + tool list, as the FIRST user
	//    message (same convention as the modern shim's single message —
	//    Qwen does no system role; it also matches what its own web client
	//    does for "system prompt" features).
	out = append(out, agentMessage{
		Role:    "user",
		Content: json.RawMessage(strconv.Quote(buildNativeContract(tools))),
	})

	// 2. Client turns, native roles preserved — EXCEPT tool exchanges, which
	//    are defused into text (see the header comment: on the GLM upstream,
	//    wire tool_calls get re-framed into <glm_block> blocks the model then
	//    imitates for new calls, which the backend rejects with
	//    INTERNAL_ERROR; the defuse avoids that whole failure class).
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			// Qwen does no system role; fold into the contract message's
			// text (kept verbatim, after the rules) — no info lost.
			if text := contentToText(m.Content); text != "" {
				out = append(out, agentMessage{Role: "user", Content: json.RawMessage(strconv.Quote(text))})
			}
		case "assistant":
			if len(m.ToolCalls) == 0 {
				out = append(out, m)
				continue
			}
			// Defuse: keep the assistant role (multi-turn benefit) but render
			// the calls in the <<<TOOL_CALL>>> text protocol the model is
			// taught, instead of the wire tool_calls field.
			var b strings.Builder
			if text := contentToText(m.Content); text != "" {
				b.WriteString(text)
				b.WriteString("\n")
			}
			for _, call := range m.ToolCalls {
				if block := renderToolCallBlock(call); block != "" {
					b.WriteString(block)
					b.WriteString("\n")
				}
			}
			out = append(out, agentMessage{
				Role:    "assistant",
				Content: json.RawMessage(strconv.Quote(strings.TrimRight(b.String(), "\n"))),
			})
		case "tool":
			// Defuse: tool results never go upstream as role:"tool" — fold
			// into a user message with the modern shim's framing so the
			// association with the preceding call stays unambiguous.
			out = append(out, agentMessage{
				Role:    "user",
				Content: json.RawMessage(strconv.Quote(renderToolResult(m))),
			})
		case "user":
			out = append(out, m)
		default:
			// Unknown role: treat as user (OpenAI default).
			m.Role = "user"
			out = append(out, m)
		}
	}

	return json.Marshal(out)
}

// buildNativeContract renders the rules + tool contract for the leading
// user message. Reuses the modern shim's constants (agentSystemPrefix,
// agentFinalReminder, renderAgentTools) so the output protocol
// (<<<TOOL_CALL>>> blocks) stays identical between variants.
func buildNativeContract(tools []openAITool) string {
	var b strings.Builder
	b.WriteString(agentSystemPrefix)
	b.WriteString("\n\n")
	b.WriteString("<tools>\n")
	b.WriteString(renderAgentTools(tools))
	b.WriteString("\n</tools>\n\n")
	b.WriteString(agentFinalReminder)
	return b.String()
}
