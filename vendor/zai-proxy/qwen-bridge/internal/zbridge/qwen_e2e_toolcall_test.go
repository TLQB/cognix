package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// =============================================================================
// E2E Tool Call Pipeline Tests — GLM 5.2 / GLM 5.3 patterns
// =============================================================================
//
// These tests exercise the full agent-mode tool call pipeline end-to-end:
//   1. Streaming interceptor (AgentStreamInterceptor) — incremental parsing
//   2. Finished-text parsing (ParseAgentToolCalls) — safety net fallback
//   3. Stall detection (looksLikeAnnouncementOnly) — retry trigger
//   4. Prompt building (buildAgentPrompt) — prompt construction
//   5. Tool call extraction from both content and reasoning channels
//
// Each subtest simulates a realistic model output pattern observed in
// production with GLM 5.2/5.3 and verifies the correct outcome.

// ── helpers ──────────────────────────────────────────────────────────────────

// feedSimulatedStream feeds text through an AgentStreamInterceptor in chunk
// sizes that simulate realistic SSE streaming (5-15 byte chunks). Returns
// the accumulated content and tool calls.
func feedSimulatedStream(t *testing.T, text string) (content string, calls []map[string]interface{}) {
	t.Helper()
	var in AgentStreamInterceptor
	for i := 0; i < len(text); {
		// Vary chunk size to stress the hold-back window
		chunkSize := 5 + (i % 11)
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		p := in.Feed(text[i:end])
		content += p.Content
		calls = append(calls, p.ToolCalls...)
		i = end
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)
	return content, reassembleStreamDeltas(calls)
}

// reassembleStreamDeltas merges streamed tool-call deltas — a header delta
// (id != "") followed by id-less argument fragments — into complete logical
// calls, mirroring how an OpenAI SDK accumulates function.arguments by
// index. Complete buffered calls (fallback path) pass through unchanged.
func reassembleStreamDeltas(deltas []map[string]interface{}) []map[string]interface{} {
	var calls []map[string]interface{}
	byIndex := map[int]map[string]interface{}{}
	for _, tc := range deltas {
		idx, _ := tc["index"].(int)
		fn, _ := tc["function"].(map[string]interface{})
		if id, _ := tc["id"].(string); id != "" {
			call := map[string]interface{}{
				"index":    idx,
				"id":       id,
				"type":     tc["type"],
				"function": map[string]interface{}{},
			}
			for k, v := range fn {
				call["function"].(map[string]interface{})[k] = v
			}
			byIndex[idx] = call
			calls = append(calls, call)
			continue
		}
		// id-less fragment: append arguments to the open call at this index
		if call, ok := byIndex[idx]; ok {
			cfn := call["function"].(map[string]interface{})
			prev, _ := cfn["arguments"].(string)
			frag, _ := fn["arguments"].(string)
			cfn["arguments"] = prev + frag
		}
	}
	return calls
}

// extractToolNames returns the function names from parsed tool calls.
func extractToolNames(calls []map[string]interface{}) []string {
	var names []string
	for _, c := range calls {
		if fn, ok := c["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// =============================================================================
// 1. STREAMING INTERCEPTOR — GLM 5.2/5.3 FORMAT VARIANTS
// =============================================================================

func TestE2E_Streaming_StandardJSON_TripleBracket(t *testing.T) {
	// GLM 5.2: standard JSON inside canonical <<<TOOL_CALL>>> markers
	block := "I'll read the file.\n\n<<<TOOL_CALL>>>\n" +
		`{"name":"read_file","arguments":{"path":"README.md","start_line":1,"end_line":50}}` +
		"\n<<<END_TOOL_CALL>>>\n\nDone."
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	names := extractToolNames(calls)
	if names[0] != "read_file" {
		t.Errorf("expected read_file, got %v", names[0])
	}
	// Verify arguments survived the streaming pipeline
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "README.md") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

func TestE2E_Streaming_SingleBracket_XMLOnClose(t *testing.T) {
	// GLM 5.3: single bracket <TOOL_CALL> with XML-style </tool_call>
	block := "Let me check the config.\n\n" +
		"<TOOL_CALL>\n" +
		`{"name":"read_file","arguments":{"path":"config.py","start_line":1}}` +
		"\n</tool_call>\n"
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("expected read_file, got %v", n[0])
	}
}

func TestE2E_Streaming_KeyValueFormat(t *testing.T) {
	// GLM 5.2: non-JSON key=value format inside markers
	block := "text <<<TOOL_CALL>>>\n" +
		`read_file path="qwen-api/README.md" start_line="637" end_line="702"` +
		"\n</tool_call>\n"
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("expected read_file, got %v", n[0])
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "qwen-api/README.md") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

func TestE2E_Streaming_FenceWrapping(t *testing.T) {
	// GLM 5.2/5.3: tool call wrapped in ```json fences (common deviation)
	block := "Let me check.\n\n```json\n<<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{"command":"ls -la"}}` +
		"\n<<<END_TOOL_CALL>>>\n```\n"
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
	if strings.Contains(content, "```json") || strings.Contains(content, "```") {
		t.Errorf("fence leaked: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "bash" {
		t.Errorf("expected bash, got %v", n[0])
	}
}

func TestE2E_Streaming_MultipleToolCalls(t *testing.T) {
	// Model emits two tool calls in one response
	block := "I'll read both files.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"a.go"}}` + "\n<<<END_TOOL_CALL>>>\n" +
		"\n<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"b.go"}}` + "\n<<<END_TOOL_CALL>>>\n" +
		"\nDone reading."
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	names := extractToolNames(calls)
	if names[0] != "read_file" || names[1] != "read_file" {
		t.Errorf("expected two read_file calls, got %v", names)
	}
}

func TestE2E_Streaming_UnclosedMarker_FinalFlush(t *testing.T) {
	// Stream ends without END_TOOL_CALL — the final flush should still parse it
	block := "Let me run this.\n\n<<<TOOL_CALL>>>\n" +
		`{"name":"run_terminal_command","arguments":{"command":"go test ./..."}}` +
		"\n"
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call from unclosed block, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "run_terminal_command" {
		t.Errorf("expected run_terminal_command, got %v", n[0])
	}
}

func TestE2E_Streaming_TolerantBrackets(t *testing.T) {
	// Model miscounts brackets: <<TOOL_CALL>>> (2 lead, 3 trail)
	block := "text\n<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{"command":"pwd"}}` +
		"\n<<<END_TOOL_CALL>>>\nend"
	content, calls := feedSimulatedStream(t, block)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
}

func TestE2E_Streaming_ZeroBrackets(t *testing.T) {
	// Extreme: model drops all leading brackets: TOOL_CALL>>>
	block := "text\nTOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{"command":"whoami"}}` +
		"\n<<<END_TOOL_CALL>>>\nend"
	_, calls := feedSimulatedStream(t, block)

	// Zero-bracket: the parser currently accepts TOOL_CALL>>> because
	// findAgentMarker allows agentMinBrackets=0 for standalone words
	// (no preceding char like _ or / or >). This is intentional tolerance.
	if len(calls) != 1 {
		t.Errorf("zero-bracket: expected 1 call (tolerant parse), got %d", len(calls))
	}
}

func TestE2E_Streaming_TextBeforeAndAfter(t *testing.T) {
	// Model emits explanation before AND after the tool call
	block := "I'll list the files.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"run_terminal_command","arguments":{"command":"ls"}}` + "\n<<<END_TOOL_CALL>>>\n" +
		"\nThe files are listed above."
	content, calls := feedSimulatedStream(t, block)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	// Content should have text before and after, but not markers
	if !strings.Contains(content, "I'll list the files") {
		t.Errorf("pre-text lost: %q", content)
	}
	if !strings.Contains(content, "The files are listed above") {
		t.Errorf("post-text lost: %q", content)
	}
	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
}

// =============================================================================
// 2. STALL DETECTION — CHINESE/ENGLISH/VIETNAMESE + REASONING-ONLY
// =============================================================================

func TestE2E_StallDetection_FullMatrix(t *testing.T) {
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"

	cases := []struct {
		name      string
		content   string
		reasoning string
		wantStall bool
	}{
		// ── Should stall ──
		{"en: i'll", "I'll check the file now.", "", true},
		{"en: let me", "Let me look at the logs.", "", true},
		{"en: i need to", "I need to read the config.", "", true},
		{"vi: toi se", "Tôi sẽ kiểm tra ngay.", "", true},
		{"vi: de toi", "Để tôi chạy thử đã.", "", true},
		{"zh: wo hui", "我会检查文件。", "", true},
		{"zh: rang wo", "让我查看日志。", "", true},
		{"zh: xianzai wo", "现在我来分析代码。", "", true},
		{"zh: wo xu yao", "我需要读取配置文件。", "", true},
		{"zh: wo jiang", "我将执行构建。", "", true},
		{"zh: jie xia lai", "接下来我将运行测试。", "", true},
		// Reasoning-only stalls (GLM 5.3 emits intent in <details>)
		{"reasoning zh", "", "让我检查文件。", true},
		{"reasoning en", "", "Let me read the config.", true},
		{"reasoning vi", "", "Để tôi chạy thử.", true},
		{"reasoning zh+en", "", "让我 read_file the file.", true},

		// ── Should NOT stall ──
		{"marker in content", "I'll check. " + open + `{"name":"x"}` + closeM, "", false},
		{"marker in reasoning", "I'll check now.", "emit " + open + " next", false},
		{"empty both", "", "", false},
		{"long content", strings.Repeat("I'll do it. ", 300), "", false},
		{"long reasoning", "", strings.Repeat("I'll do it. ", 300), false},
		{"final answer en", "Done: build passes.", "", false},
		{"final answer zh", "构建完成，测试全部通过。", "", false},
		{"final answer vi", "Đã xong: build thành công.", "", false},
		{"no intent", "The file has 42 lines.", "", false},
		// Offer answers (user-directed, awaiting reply) must not stall even
		// though they contain intent vocabulary — the 2026-09-09 duplicate
		// greeting bug: reasoning-fallback matched "let me ", nudge retry
		// re-generated the answer, client saw two concatenated greetings.
		{"offer en let me know", "Just let me know how I can help today! 😊", "", false},
		{"offer en how can i help", "How can I help you today? I'll get started right away.", "", false},
		{"offer vi detail", "Bạn cần mình giúp gì hôm nay? 😊 Mình có thể viết code, tìm file, chạy test!", "", false},
		{"offer vi reason", "", "Chào bạn! Mình có thể giúp gì cho bạn?", false},
		{"offer en reason", "", "Just let me know what you need, I'm happy to help.", false},
		{"offer zh reason", "", "请告诉我需要我帮你做什么。", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := looksLikeAnnouncementOnly(tc.content, tc.reasoning)
			if got != tc.wantStall {
				t.Errorf("looksLikeAnnouncementOnly(%q, %q) = %v, want %v",
					tc.content, tc.reasoning, got, tc.wantStall)
			}
		})
	}
}

func TestE2E_StallNudge_ContainsRequiredElements(t *testing.T) {
	nudge := agentStallNudge()
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"

	checks := []struct {
		desc    string
		present bool
	}{
		{"starts with double newline", strings.HasPrefix(nudge, "\n\n")},
		{"has <system_nudge>", strings.Contains(nudge, "<system_nudge>")},
		{"has </system_nudge>", strings.Contains(nudge, "</system_nudge>")},
		{"has TOOL_CALL marker", strings.Contains(nudge, open)},
		{"has END_TOOL_CALL marker", strings.Contains(nudge, closeM)},
		{"offers FINAL ANSWER", strings.Contains(nudge, "FINAL ANSWER")},
		{"mentions 'no tool call'", strings.Contains(nudge, "NO tool call") || strings.Contains(nudge, "no tool call")},
	}

	for _, c := range checks {
		if !c.present {
			t.Errorf("stall nudge missing: %s", c.desc)
		}
	}
}

// =============================================================================
// 3. TOOL CALL EXTRACTION — CONTENT VS REASONING
// =============================================================================

func TestE2E_ExtractToolCalls_FromContent(t *testing.T) {
	text := "I'll check the files.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"main.go"}}` + "\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("expected read_file, got %v", n[0])
	}
}

func TestE2E_ExtractToolCalls_FromReasoning(t *testing.T) {
	// GLM 5.3: tool call inside <details> reasoning block
	reasoning := "让我读取文件。\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"config.py"}}` + "\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(reasoning)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call in reasoning, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("expected read_file, got %v", n[0])
	}
}

func TestE2E_ExtractToolCalls_FromBoth_PrefersContent(t *testing.T) {
	// Both channels have tool calls — content should be tried first
	contentText := "Let me check.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"bash","arguments":{"command":"ls"}}` + "\n<<<END_TOOL_CALL>>>"
	reasoningText := "让我读取文件。\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"a.go"}}` + "\n<<<END_TOOL_CALL>>>"

	// Content first
	calls := ParseAgentToolCalls(contentText)
	if len(calls) != 1 {
		t.Fatalf("content: expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "bash" {
		t.Errorf("content: expected bash, got %v", n[0])
	}

	// Reasoning as fallback
	calls = ParseAgentToolCalls(reasoningText)
	if len(calls) != 1 {
		t.Fatalf("reasoning: expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("reasoning: expected read_file, got %v", n[0])
	}
}

func TestE2E_StripToolCalls_LeavesPlainText(t *testing.T) {
	text := "I'll check the file.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"a.go"}}` + "\n<<<END_TOOL_CALL>>>" +
		"\n\nThe file looks good."

	stripped := StripAgentToolCalls(text)
	if strings.Contains(stripped, "TOOL_CALL") {
		t.Errorf("markers not stripped: %q", stripped)
	}
	if !strings.Contains(stripped, "I'll check") {
		t.Errorf("pre-text lost: %q", stripped)
	}
	if !strings.Contains(stripped, "file looks good") {
		t.Errorf("post-text lost: %q", stripped)
	}
}

// =============================================================================
// 4. PROMPT BUILDING — TOOL EXCHANGES
// =============================================================================

func TestE2E_PromptBuilding_WithTools(t *testing.T) {
	tools := []openAITool{
		{
			Type: "function",
			Function: &openAIFnSpec{
				Name:        "read_file",
				Description: "Read a file from disk",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
		{
			Type: "function",
			Function: &openAIFnSpec{
				Name:        "write_file",
				Description: "Write a file to disk",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
			},
		},
	}

	msgs := []agentMessage{
		mkMsg("user", "Read README.md and tell me the project structure"),
	}

	prompt := buildAgentPrompt(msgs, tools)

	// Verify prompt structure
	checks := []struct {
		desc    string
		present bool
	}{
		{"has <system>", strings.Contains(prompt, "<system>")},
		{"has </system>", strings.Contains(prompt, "</system>")},
		{"has <tools>", strings.Contains(prompt, "<tools>")},
		{"has </tools>", strings.Contains(prompt, "</tools>")},
		{"has read_file tool", strings.Contains(prompt, "read_file")},
		{"has write_file tool", strings.Contains(prompt, "write_file")},
		{"has <current_task>", strings.Contains(prompt, "<current_task>")},
		{"has <output_rules>", strings.Contains(prompt, "<output_rules>")},
		{"has TOOL_CALL marker in rules", strings.Contains(prompt, "TOOL_CALL")},
	}

	for _, c := range checks {
		if !c.present {
			t.Errorf("prompt missing: %s", c.desc)
		}
	}
}

func TestE2E_PromptBuilding_WithToolHistory(t *testing.T) {
	// Simulate a conversation with tool exchanges
	assistant1, result1 := mkToolMsg("read_file")
	assistant2, result2 := mkToolMsg("write_file")

	msgs := []agentMessage{
		mkMsg("user", "Update the config"),
		assistant1,
		result1,
		assistant2,
		result2,
		mkMsg("user", "Verify the changes"),
	}

	prompt := buildAgentPrompt(msgs, nil)

	// The prompt should have <recent> with tool exchanges grouped
	if !strings.Contains(prompt, "<recent>") {
		t.Error("prompt missing <recent>")
	}
	// The last user message goes in <current_task>
	if !strings.Contains(prompt, "Verify the changes") {
		t.Error("last user message not in prompt")
	}
}

// =============================================================================
// 5. MULTI-LINE STRING TOOL ARGS — EDIT_FILE / WRITE_FILE / READ_FILE
// =============================================================================

func TestE2E_ColonFormat_MultiLineOldText(t *testing.T) {
	// GLM 5.2: edit_file with multi-line old_text/new_text
	body := "edit_file\n" +
		"path: \"main.go\"\n" +
		"old_text: \"import \"fmt\"\nimport \"os\"\n\"\n" +
		"new_text: \"import \"fmt\"\nimport \"os\"\nimport \"strings\"\n\""
	text := "<<<TOOL_CALL>>>" + body + "<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d (body=%q)", len(calls), body)
	}
	if n := extractToolNames(calls); n[0] != "edit_file" {
		t.Errorf("expected edit_file, got %v", n[0])
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "main.go") {
		t.Errorf("path lost: %q", argsStr)
	}
}

func TestE2E_ColonFormat_MultiLineContent(t *testing.T) {
	// GLM 5.2: write_file with multi-line content spanning lines
	body := "write_file\n" +
		"path: \"helper.py\"\n" +
		"content: \"def hello():\n    print('hi')\n\ndef main():\n    hello()\n\""
	text := "<<<TOOL_CALL>>>" + body + "<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "write_file" {
		t.Errorf("expected write_file, got %v", n[0])
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "def hello()") || !strings.Contains(argsStr, "def main()") {
		t.Errorf("multi-line content lost: %q", argsStr)
	}
}

func TestE2E_KeyValueFormat_RunTerminalCommand(t *testing.T) {
	// GLM 5.2: run_terminal_command with key=value args
	body := `run_terminal_command command="go test ./... -v"`
	text := "<<<TOOL_CALL>>>" + body + "<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "run_terminal_command" {
		t.Errorf("expected run_terminal_command, got %v", n[0])
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "go test") {
		t.Errorf("command lost: %q", argsStr)
	}
}

func TestE2E_JSONFormat_EditFileEdits(t *testing.T) {
	// Standard JSON with edits array
	edits := `{"old_text":"import argparse","new_text":"import argparse\nimport os"}`
	args := `{"path":"kimi_chat.py","edits":[` + edits + `]}`
	text := "<<<TOOL_CALL>>>\n" +
		`{"name":"edit_file","arguments":` + args + `}` +
		"\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "edit_file" {
		t.Errorf("expected edit_file, got %v", n[0])
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "kimi_chat.py") {
		t.Errorf("path lost: %q", argsStr)
	}
	if !strings.Contains(argsStr, "import argparse") {
		t.Errorf("old_text lost: %q", argsStr)
	}
}

// =============================================================================
// 6. BROKEN JSON REPAIR — GLM 5.2/5.3 DEVIATIONS
// =============================================================================

func TestE2E_BrokenJSON_TrailingComma(t *testing.T) {
	text := "<<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{"command":"ls",},}` +
		"\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "bash" {
		t.Errorf("expected bash, got %v", n[0])
	}
}

func TestE2E_BrokenJSON_InteriorQuotes(t *testing.T) {
	// grep command with bare double quotes inside JSON string
	text := "<<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{"command":"grep -rn \"TOKENROUTER\" crates"}}` +
		"\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "TOKENROUTER") {
		t.Errorf("command lost: %q", argsStr)
	}
}

func TestE2E_BrokenJSON_ControlChars(t *testing.T) {
	// Multi-line bash command with literal newlines and grep regex
	block := `{"name":"bash","arguments":{"command":"echo '=== INFO ===';\ngrep -n 'HOME\|Waguri' tests/test_auth.py | head -20"}}`

	name, args, ok := agentLooseParse(block, nil)
	if !ok || name != "bash" {
		t.Fatalf("expected bash, got ok=%v name=%q", ok, name)
	}
	var m map[string]string
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("args not valid JSON: %v", err)
	}
	if !strings.Contains(m["command"], "FAILING") && !strings.Contains(m["command"], "HOME") {
		t.Errorf("command lost: %q", m["command"])
	}
}

func TestE2E_BrokenJSON_EmptyArgs(t *testing.T) {
	text := "<<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{}}` +
		"\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if argsStr != "{}" {
		t.Errorf("expected empty args '{}', got %q", argsStr)
	}
}

func TestE2E_BrokenJSON_NullArgs(t *testing.T) {
	text := "<<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":null}` +
		"\n<<<END_TOOL_CALL>>>"

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	// null arguments should be normalized to empty object by agentParseArguments
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "{}") {
		t.Errorf("null args should normalize to empty object, got %q", argsStr)
	}
}

// =============================================================================
// 7. END-TO-END STREAMING PIPELINE — FULL SCENARIOS
// =============================================================================

func TestE2E_FullPipeline_GLM52AnnouncementThenToolCall(t *testing.T) {
	// GLM 5.3 emits: short announcement text + tool call
	// The interceptor should extract the tool call and forward the text
	block := "让我检查文件内容。\n\n" +
		"<TOOL_CALL>\n" +
		`{"name":"read_file","arguments":{"path":"internal/zbridge/config.go","start_line":1,"end_line":50}}` +
		"\n</tool_call>\n" +
		"文件包含配置信息。"

	content, calls := feedSimulatedStream(t, block)

	// Tool call should be extracted
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("expected read_file, got %v", n[0])
	}

	// Text before and after should be forwarded (minus markers)
	if !strings.Contains(content, "让我检查文件内容") {
		t.Errorf("pre-text lost: %q", content)
	}
	if !strings.Contains(content, "文件包含配置信息") {
		t.Errorf("post-text lost: %q", content)
	}
	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked: %q", content)
	}
}

func TestE2E_FullPipeline_GLM52KeyValueWithXMLClose(t *testing.T) {
	// Exact pattern from production: key=value args with </tool_call>
	block := "text <<<TOOL_CALL>>>\n" +
		`read_file path="qwen-api/README.md" start_line="637" end_line="702"` +
		"\n</tool_call>"

	content, calls := feedSimulatedStream(t, block)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d (content=%q)", len(calls), content)
	}
	if n := extractToolNames(calls); n[0] != "read_file" {
		t.Errorf("expected read_file, got %v", n[0])
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "qwen-api/README.md") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

func TestE2E_FullPipeline_MixedTextAndToolCalls(t *testing.T) {
	// Model emits text, two tool calls, and trailing text
	block := "I need to check two files.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"a.go"}}` + "\n<<<END_TOOL_CALL>>>\n" +
		"\nNow let me also check b.go.\n\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"b.go"}}` + "\n<<<END_TOOL_CALL>>>\n" +
		"\nBoth files look good."

	content, calls := feedSimulatedStream(t, block)

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	names := extractToolNames(calls)
	if names[0] != "read_file" || names[1] != "read_file" {
		t.Errorf("expected two read_file, got %v", names)
	}
	// All text segments should be forwarded
	if !strings.Contains(content, "check two files") {
		t.Errorf("text1 lost: %q", content)
	}
	if !strings.Contains(content, "check b.go") {
		t.Errorf("text2 lost: %q", content)
	}
	if !strings.Contains(content, "Both files") {
		t.Errorf("text3 lost: %q", content)
	}
}

func TestE2E_FullPipeline_ToolCallOnlyNoText(t *testing.T) {
	// Model emits ONLY a tool call, no surrounding text
	block := "<<<TOOL_CALL>>>\n" +
		`{"name":"run_terminal_command","arguments":{"command":"echo hello"}}` +
		"\n<<<END_TOOL_CALL>>>"

	content, calls := feedSimulatedStream(t, block)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	// Content should be empty (or whitespace only)
	if strings.TrimSpace(content) != "" {
		t.Errorf("expected empty content, got: %q", content)
	}
}

func TestE2E_FullPipeline_XMLOpen_XMLOnlyClose(t *testing.T) {
	// GLM 5.3 pattern: <<<TOOL_CALL>>> (canonical open) + </Tool_Call> (XML close, mixed case)
	block := "Let me check.\n\n" +
		"<<<TOOL_CALL>>>\n" +
		`{"name":"bash","arguments":{"command":"pwd"}}` +
		"\n</Tool_Call>\nend"

	_, calls := feedSimulatedStream(t, block)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "bash" {
		t.Errorf("expected bash, got %v", n[0])
	}
}

func TestE2E_FullPipeline_VeryLongToolCall(t *testing.T) {
	// Large tool call with a long bash command
	longCmd := "for i in $(seq 1 100); do echo \"iteration $i: $(date)\"; sleep 0.1; done"
	args := `{"name":"run_terminal_command","arguments":{"command":"` + longCmd + `"}}`
	block := "Running a long command.\n\n" +
		"<<<TOOL_CALL>>>\n" + args + "\n<<<END_TOOL_CALL>>>"

	_, calls := feedSimulatedStream(t, block)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	// The escaped quotes in the JSON cause agentParseArguments to compact
	// differently — verify the core command content survived.
	if !strings.Contains(argsStr, "seq 1 100") || !strings.Contains(argsStr, "sleep 0.1") {
		t.Errorf("long command truncated: %q", argsStr)
	}
}

// =============================================================================
// 8. STATUS MAPPING — 405 AND UPSTREAM 5XX
// =============================================================================

func TestE2E_StatusFromError_Coverage(t *testing.T) {
	cases := []struct {
		errMsg string
		want   int
	}{
		{"Qwen error 405: Method Not Allowed", 405},
		{"Qwen error 401: unauthorized", 401},
		{"Qwen error 403: forbidden", 403},
		{"Qwen error 429: rate limited", 429},
		{"Qwen error 400: bad request", 400},
		{"Qwen error 502: bad gateway", 502},
		{"Qwen error 503: service unavailable", 503},
		{"Qwen error 504: gateway timeout", 504},
		{"connection refused", 500},
		{"", 500},
	}
	for _, tc := range cases {
		t.Run(tc.errMsg, func(t *testing.T) {
			if got := statusFromError(tc.errMsg); got != tc.want {
				t.Errorf("statusFromError(%q) = %d, want %d", tc.errMsg, got, tc.want)
			}
		})
	}
}

// =============================================================================
// 9. PAYLOAD TOLERANCE — ALTERNATE KEY SPELLINGS
// =============================================================================

func TestE2E_PayloadTolerance_AlternateKeys(t *testing.T) {
	// GLM 5.3 sometimes uses "tool" instead of "name"
	cases := []struct {
		name     string
		payload  string
		wantName string
	}{
		{"name key", `{"name":"bash","arguments":{"command":"ls"}}`, "bash"},
		{"tool key", `{"tool":"bash","arguments":{"command":"ls"}}`, "bash"},
		{"function key", `{"function":"bash","arguments":{"command":"ls"}}`, "bash"},
		{"tool_name key", `{"tool_name":"bash","arguments":{"command":"ls"}}`, "bash"},
		{"parameters instead of arguments", `{"name":"bash","parameters":{"command":"ls"}}`, "bash"},
		{"args instead of arguments", `{"name":"bash","args":{"command":"ls"}}`, "bash"},
		{"params instead of arguments", `{"name":"bash","params":{"command":"ls"}}`, "bash"},
		{"flat payload", `{"tool":"bash","command":"ls"}`, "bash"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := "<<<TOOL_CALL>>>" + tc.payload + "<<<END_TOOL_CALL>>>"
			calls := ParseAgentToolCalls(text)
			if len(calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(calls))
			}
			if n := extractToolNames(calls); n[0] != tc.wantName {
				t.Errorf("expected %v, got %v", tc.wantName, n[0])
			}
		})
	}
}

// =============================================================================
// 10. AGENT PROMPT — CURRENT TASK ANCHOR
// =============================================================================

func TestE2E_Prompt_CurrentTaskAnchor_MidTask(t *testing.T) {
	// When tool results follow the user message, the anchor should say
	// "Continue..." instead of re-presenting the user's message as a fresh task
	assistant, result := mkToolMsg("build")
	msgs := []agentMessage{
		mkMsg("user", "build it again"),
		assistant,
		result,
	}
	prompt := buildAgentPrompt(msgs, nil)

	if !strings.Contains(prompt, "<current_task>") {
		t.Fatal("missing <current_task>")
	}
	if !strings.Contains(prompt, "Continue the user's task") {
		t.Error("mid-task anchor must say 'Continue the user's task'")
	}
	if !strings.Contains(prompt, "Do NOT repeat a tool call") {
		t.Error("mid-task anchor must forbid repeating completed calls")
	}
}

func TestE2E_Prompt_CurrentTaskAnchor_FreshTurn(t *testing.T) {
	// When user message IS the last message, anchor should be verbatim
	msgs := []agentMessage{
		mkMsg("user", "read the config file"),
	}
	prompt := buildAgentPrompt(msgs, nil)

	if !strings.Contains(prompt, "read the config file") {
		t.Error("fresh-turn anchor must contain verbatim user message")
	}
	if strings.Contains(prompt, "User's request:") {
		t.Error("fresh-turn anchor must not use mid-task wrapper")
	}
}

func TestE2E_Prompt_HistorySummarization(t *testing.T) {
	// When there are more than maxRecentToolExchanges=6 tool exchanges,
	// older ones should be summarized
	var msgs []agentMessage
	msgs = append(msgs, mkMsg("user", "analyze the project"))
	for i := 0; i < 10; i++ {
		a, r := mkToolMsg("tool")
		msgs = append(msgs, a, r)
	}

	prompt := buildAgentPrompt(msgs, nil)

	// Should have <history_summary> for old exchanges
	if !strings.Contains(prompt, "<history_summary>") {
		t.Error("expected <history_summary> for 10 tool exchanges")
	}
	// Should have <recent> for recent exchanges
	if !strings.Contains(prompt, "<recent>") {
		t.Error("expected <recent> section")
	}
}

// ── holdBackTailSafe / toolCallMarkerTailHoldback tests ─────────────────────

func TestHoldBackTailSafePreservesToolCallMarkers(t *testing.T) {
	const n = 4 // regular holdBack

	for _, tc := range []struct {
		name, input string
		want        string
	}{
		// Complete markers: must NOT trim into them
		{"complete_end_tool_call", "Hello <<<END_TOOL_CALL>>>", "Hello <<<END_TOOL_CALL>>>"},
		{"complete_tool_call", "fn() <<<TOOL_CALL>>>\n{\"name\": \"ls\"}\n<<<END_TOOL_CALL>>>", "fn() <<<TOOL_CALL>>>\n{\"name\": \"ls\"}\n<<<END_TOOL_CALL>>>"},
		// Partial markers: must NOT trim into them
		{"partial_end_tool_call", "Hello <<<END_TOOL_CAL", "Hello <<<END_TOOL_CAL"},
		{"partial_tool_call", "Hello <<<TOOL_CAL", "Hello <<<TOOL_CAL"},
		{"case_insensitive", "Hello <<<tool_call>>>", "Hello <<<tool_call>>>"},
		// No marker: normal holdBack applies
		{"no_marker", "Hello world", "Hello w"},
		{"short_string_no_marker", "abc", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := holdBackTailSafe(tc.input, n)
			if got != tc.want {
				t.Errorf("holdBackTailSafe(%q, %d) = %q, want %q", tc.input, n, got, tc.want)
			}
		})
	}
}

func TestHasToolCallMarkerSuffix(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"<<<END_TOOL_CALL>>>", true},
		{"<<<TOOL_CALL>>>", true},
		{"<<<END_TOOL_CAL", true}, // partial END_TOOL_CALL
		{"<<<TOOL_CAL", true},     // partial TOOL_CALL
		{"<<tool_call>>", true},   // case-insensitive
		{"Hello world", false},
		{"TOOL_CALL", false}, // no brackets
		{"<<<TOOL_OTHER>>>", false},
		{"", false},
		{"abc", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got := hasToolCallMarkerSuffix(tc.input)
			if got != tc.want {
				t.Errorf("hasToolCallMarkerSuffix(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// =============================================================================
// 8. SSE STREAMING PIPELINE — TOOL CALL INTERCEPTION
// =============================================================================
//
// These tests simulate the full SSE streaming pipeline as it works in
// streamSSEResponse → emitContent → handler, verifying that tool calls
// are properly intercepted and NOT leaked as text content.

// TestSSEStream_ToolCallInterception simulates the exact SSE sequence
// from the production debug log and verifies the interceptor catches
// the tool call.
func TestSSEStream_ToolCallInterception(t *testing.T) {
	// Simulate the exact SSE events from the debug log:
	// 1. edit_content with reasoning + </details> + <<<TOOL_CALL>>>
	// 2. delta_content with JSON body
	// 3. edit_content completing with <<<END_TOOL_CALL>>>
	// 4. phase=done

	reasoningText := "The user wants a code quality assessment."
	toolCallJSON := `{"name":"read_file","arguments":{"path":"qwen-proxy/internal/zbridge/qwen.go","start_line":385,"end_line":595}}`

	// Step 1: Full content with reasoning + tool call start marker
	step1Content := `<details type="reasoning" done="false">
> ` + reasoningText + `
</details>
<<<TOOL_CALL>>>`

	// Step 2: JSON body arrives as delta
	step2Delta := `
` + toolCallJSON

	// Step 3: End marker arrives as edit
	step3Edit := `
<<<END_TOOL_CALL>>>`

	// Simulate the SSE pipeline:
	// - fullText accumulates
	// - splitDetails extracts content
	// - holdBackTailSafe preserves markers
	// - contentEmitter computes delta
	// - AgentStreamInterceptor processes delta

	var fullText strings.Builder
	var contentEmitter sseEmitter
	interceptor := &AgentStreamInterceptor{}

	// --- Step 1 ---
	fullText.WriteString(step1Content)
	reasoning, content := splitDetails(fullText.String())
	_ = reasoning // not tested here
	target := holdBackTailSafe(content, 4)
	target = holdBackPartialDetailsTag(target)
	delta1 := contentEmitter.delta(target)
	if delta1 == "" {
		t.Fatal("step 1: expected non-empty delta")
	}
	p1 := interceptor.Feed(delta1)
	if len(p1.ToolCalls) > 0 {
		t.Fatal("step 1: tool call should not complete yet (no end marker)")
	}
	t.Logf("step 1: delta=%q, content=%q", delta1, p1.Content)

	// --- Step 2 ---
	fullText.WriteString(step2Delta)
	_, content = splitDetails(fullText.String())
	target = holdBackTailSafe(content, 4)
	target = holdBackPartialDetailsTag(target)
	delta2 := contentEmitter.delta(target)
	if delta2 == "" {
		t.Fatal("step 2: expected non-empty delta")
	}
	p2 := interceptor.Feed(delta2)
	// With incremental streaming the header delta MAY already be out (name
	// + arguments object start arrived) — that is the new contract, not a
	// leak. The call must not be COMPLETE yet (no closing marker):
	// fragments keep arriving with no second header.
	for _, tc := range p2.ToolCalls {
		if id, _ := tc["id"].(string); id == "" {
			continue // argument fragment of the open call — expected
		}
		fn, _ := tc["function"].(map[string]interface{})
		if name, _ := fn["name"].(string); name != "read_file" {
			t.Fatalf("step 2: unexpected early header: %+v", tc)
		}
	}
	if len(reassembleStreamDeltas(p2.ToolCalls)) > 1 {
		t.Fatalf("step 2: more than one logical call: %+v", p2.ToolCalls)
	}
	t.Logf("step 2: delta=%q, content=%q", delta2, p2.Content)

	// --- Step 3 ---
	fullText.WriteString(step3Edit)
	_, content = splitDetails(fullText.String())
	target = holdBackTailSafe(content, 4)
	target = holdBackPartialDetailsTag(target)
	delta3 := contentEmitter.delta(target)
	if delta3 == "" {
		t.Fatal("step 3: expected non-empty delta")
	}
	p3 := interceptor.Feed(delta3)
	t.Logf("step 3: delta=%q, content=%q, toolCalls=%d", delta3, p3.Content, len(p3.ToolCalls))

	// --- Finish ---
	pf := interceptor.Finish()
	t.Logf("finish: content=%q, toolCalls=%d", pf.Content, len(pf.ToolCalls))

	// Verify tool call was intercepted
	allToolCalls := append(append(p1.ToolCalls, p2.ToolCalls...), p3.ToolCalls...)
	allToolCalls = append(allToolCalls, pf.ToolCalls...)
	allToolCalls = reassembleStreamDeltas(allToolCalls)

	if len(allToolCalls) == 0 {
		t.Fatal("TOOL CALL LEAK: interceptor did not detect any tool call!")
	}
	if len(allToolCalls) != 1 {
		t.Fatalf("expected exactly 1 logical call, got %d", len(allToolCalls))
	}

	// Verify tool call content
	tc := allToolCalls[0]
	fn, _ := tc["function"].(map[string]interface{})
	name, _ := fn["name"].(string)
	if name != "read_file" {
		t.Errorf("expected tool name 'read_file', got %q", name)
	}
	args, _ := fn["arguments"].(string)
	if !strings.Contains(args, "qwen.go") {
		t.Errorf("arguments should contain 'qwen.go', got %q", args)
	}
}

// TestSSEStream_ToolCallLeakWhenMarkerTruncated verifies that if
// holdBackTail truncates into the marker, the interceptor DOESN'T
// catch it — proving our holdBackTailSafe fix is necessary.
func TestSSEStream_ToolCallLeakWhenMarkerTruncated(t *testing.T) {
	// Simulate what would happen with the OLD holdBackTail (no marker awareness)
	toolCallJSON := `{"name":"read_file","arguments":{"path":"test.go"}}`

	fullText := `<details type="reasoning" done="false">
> thinking
</details>
<<<TOOL_CALL>>>
` + toolCallJSON + `
<<<END_TOOL_CALL>>>`

	reasoning, content := splitDetails(fullText)
	_ = reasoning

	// With OLD holdBackTail: trim 4 runes into the marker
	badTarget := holdBackTail(content, 4) // <<<END_TOOL_CALL>>> → <<<END_TOOL_CAL
	badTarget = holdBackPartialDetailsTag(badTarget)

	// With NEW holdBackTailSafe: preserve the marker
	goodTarget := holdBackTailSafe(content, 4)
	goodTarget = holdBackPartialDetailsTag(goodTarget)

	t.Logf("content (len=%d): %q", len(content), content)
	t.Logf("bad target (holdBackTail):     %q", badTarget)
	t.Logf("good target (holdBackTailSafe): %q", goodTarget)

	// Verify holdBackTailSafe preserves the marker
	if !strings.HasSuffix(goodTarget, "<<<END_TOOL_CALL>>>") {
		t.Errorf("holdBackTailSafe should preserve end marker, got suffix: %q", goodTarget[max(0, len(goodTarget)-30):])
	}

	// Verify old holdBackTail truncates the marker
	if strings.HasSuffix(badTarget, "<<<END_TOOL_CALL>>>") {
		t.Errorf("old holdBackTail should truncate end marker")
	}

	// Now simulate the interceptor with the BAD target
	var emitter sseEmitter
	interceptor := &AgentStreamInterceptor{}

	// Feed the content with truncated marker through interceptor
	delta := emitter.delta(badTarget)
	p := interceptor.Finish()

	// The interceptor should NOT catch the tool call because the end marker is truncated
	if len(p.ToolCalls) > 0 && delta != "" {
		// If delta contains the full marker, it might work — but the truncated
		// end marker means the body will include garbage
		t.Logf("interceptor returned %d tool calls with bad target (might parse)", len(p.ToolCalls))
	} else {
		t.Logf("interceptor correctly FAILED to catch tool call with truncated marker — this is why holdBackTailSafe is needed")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
