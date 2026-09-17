package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// leakedProductionBlock is the exact tool-call payload observed leaking to
// the fclaude UI on 2026-08-28: the model wrote a multi-line bash command
// with LITERAL newlines inside the JSON string and verbatim grep regex
// escapes (\|) — both invalid JSON, so the strict parse failed and the whole
// block was shown to the user as text instead of executing.
const leakedProductionBlock = `{"name":"bash","arguments":{"command":"echo '=== FAILING TEST DETAILS ==='; python3 -m pytest tests/test_auth.py::test_is_logged_in_false
tests/test_auth.py::test_get_token_returns_value -q --no-header 2>&1 | head -40; echo '=== AUTH MOCK/ENV CAUSE ==='; grep -n
'HOME\|Waguri\|monkeypatch\|token_file\|CONFIG' tests/test_auth.py | head -20"}}`

func TestAgentRepairJSONControlCharsAndBadEscapes(t *testing.T) {
	repaired := agentRepairJSON(leakedProductionBlock)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(repaired), &obj); err != nil {
		t.Fatalf("repaired JSON still invalid: %v\n%s", err, repaired)
	}
	// The decoded command must keep its newlines and regex backslashes.
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(obj["arguments"], &args); err != nil {
		t.Fatalf("arguments decode: %v", err)
	}
	if !strings.Contains(args.Command, "test_is_logged_in_false\ntests/test_auth.py") {
		t.Errorf("raw newline not preserved as \\n escape: %q", args.Command)
	}
	if !strings.Contains(args.Command, `HOME\|Waguri`) {
		t.Errorf("grep regex backslashes not preserved: %q", args.Command)
	}
}

func TestAgentRepairJSONLeavesValidJSONUntouched(t *testing.T) {
	cases := []string{
		`{"name":"bash","arguments":{"command":"ls -la"}}`,
		`{"name":"edit","arguments":{"path":"a/b.go","old":"say \"hi\"","new":"c:\\path\\file"}}`,
		"{\n  \"name\": \"bash\",\n  \"arguments\": {\"command\": \"pwd\"}\n}", // pretty-printed
		`{"name":"x","arguments":{"s":"unicode \u00e9 ok"}}`,
	}
	for _, valid := range cases {
		if got := agentRepairJSON(valid); got != valid {
			t.Errorf("valid JSON was modified:\n in: %s\nout: %s", valid, got)
		}
	}
}

func TestAgentStripTrailingCommas(t *testing.T) {
	in := `{"name":"bash","arguments":{"command":"ls",},}`
	want := `{"name":"bash","arguments":{"command":"ls"}}`
	if got := agentStripTrailingCommas(in); got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	arr := `{"a":[1,2,],}`
	if got := agentStripTrailingCommas(arr); got != `{"a":[1,2]}` {
		t.Errorf("array trailing comma: got %s", got)
	}

	// Commas inside string literals must survive.
	inStr := `{"command":"a,b,}"}`
	if got := agentStripTrailingCommas(inStr); got != inStr {
		t.Errorf("string content modified: %s", got)
	}
}

func TestAgentLooseParseRepairsLeakedProductionBlock(t *testing.T) {
	name, args, ok := agentLooseParse(leakedProductionBlock, nil)
	if !ok || name != "bash" {
		t.Fatalf("expected bash tool call, got ok=%v name=%q", ok, name)
	}
	var m map[string]string
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("arguments not valid JSON after repair: %v", err)
	}
	if !strings.Contains(m["command"], "FAILING TEST DETAILS") {
		t.Errorf("command lost: %q", m["command"])
	}
}

func TestAgentLooseParseTrailingCommas(t *testing.T) {
	name, _, ok := agentLooseParse(`{"name":"bash","arguments":{"command":"ls",},}`, nil)
	if !ok || name != "bash" {
		t.Fatalf("trailing-comma payload not repaired: ok=%v name=%q", ok, name)
	}
}

// TestAgentStreamInterceptorLeakedBlock feeds the production block through
// the streaming interceptor in small chunks: nothing may leak as content and
// exactly one tool call must emerge.
func TestAgentStreamInterceptorLeakedBlock(t *testing.T) {
	var in AgentStreamInterceptor
	var content string
	var calls []map[string]interface{}
	block := "\n<<<TOOL_CALL>>>\n" + leakedProductionBlock + "\n<<<END_TOOL_CALL>>>\n"
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		content += p.Content
		calls = append(calls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)
	calls = reassembleStreamDeltas(calls)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected tool bash, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "test_auth.py") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

// interiorQuoteBlock is the second production leak (2026-08-28): the model
// wrote a bash command containing grep -rn "TOKENROUTER_API_URL" ... with BARE
// double quotes inside the JSON string value, on top of literal newlines. The
// unescaped interior quotes closed the string early, the strict parse failed,
// and the whole block leaked to the UI as text. This payload exercises
// malformation class 1 (raw control chars) and class 4 (unescaped interior
// quotes) at the same time.
const interiorQuoteBlock = `{"name":"bash","arguments":{"command":"cd /home/tlqbao/Desktop/cognix && echo '=== Find tokenrouter crate ==='; find crates -type d -name
'tokenrouter*' -o -type d -name 'tokenrouter' | head; echo; grep -rn "TOKENROUTER_API_URL" crates --include='.rs' | head -10; echo; echo '===
tokenrouter lib files ==='; find crates/tokenrouter -name '.rs' 2>/dev/null | head"}}`

func TestAgentRepairJSONInteriorQuotes(t *testing.T) {
	repaired := agentRepairJSON(interiorQuoteBlock)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(repaired), &obj); err != nil {
		t.Fatalf("repaired JSON still invalid: %v\n%s", err, repaired)
	}
}

func TestAgentLooseParseInteriorQuotes(t *testing.T) {
	name, args, ok := agentLooseParse(interiorQuoteBlock, nil)
	if !ok || name != "bash" {
		t.Fatalf("expected bash tool call, got ok=%v name=%q", ok, name)
	}
	var m map[string]string
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("arguments not valid JSON after repair: %v", err)
	}
	want := `grep -rn "TOKENROUTER_API_URL" crates --include='.rs'`
	if !strings.Contains(m["command"], want) {
		t.Errorf("interior quotes not preserved:\nwant substring: %s\ngot command: %q", want, m["command"])
	}
	if !strings.Contains(m["command"], "tokenrouter lib files") {
		t.Errorf("command tail lost: %q", m["command"])
	}
}

// TestAgentStreamInterceptorInteriorQuoteBlock feeds the interior-quote block
// through the streaming interceptor in 7-byte chunks: nothing may leak as
// content and exactly one tool call must emerge.
func TestAgentStreamInterceptorInteriorQuoteBlock(t *testing.T) {
	var in AgentStreamInterceptor
	var content string
	var calls []map[string]interface{}
	block := "\n<<<TOOL_CALL>>>\n" + interiorQuoteBlock + "\n<<<END_TOOL_CALL>>>\n"
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		content += p.Content
		calls = append(calls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)
	calls = reassembleStreamDeltas(calls)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected tool bash, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "TOKENROUTER_API_URL") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

// ========================================================================
// GLM 5.2/5.3 tool-call format tests
// ========================================================================

// TestAgentSingleBracketMarker tests that <TOOL_CALL> (single bracket) is accepted.
func TestAgentSingleBracketMarker(t *testing.T) {
	var in AgentStreamInterceptor
	var content string
	var calls []map[string]interface{}
	block := "some text <TOOL_CALL>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"README.md\"}}\n<END_TOOL_CALL>\nmore text"
	for i := 0; i < len(block); i += 5 {
		end := i + 5
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		content += p.Content
		calls = append(calls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)
	calls = reassembleStreamDeltas(calls)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Errorf("expected read_file, got %v", fn["name"])
	}
}

// TestAgentXMLCloseTag tests that </tool_call> is accepted as end marker.
func TestAgentXMLCloseTag(t *testing.T) {
	var in AgentStreamInterceptor
	block := "some text <<<TOOL_CALL>>>\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n</tool_call>\nmore text"
	var allCalls []map[string]interface{}
	var allContent string
	for i := 0; i < len(block); i += 5 {
		end := i + 5
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		allContent += p.Content
		allCalls = append(allCalls, p.ToolCalls...)
	}
	p := in.Finish()
	allContent += p.Content
	allCalls = append(allCalls, p.ToolCalls...)
	allCalls = reassembleStreamDeltas(allCalls)

	if strings.Contains(allContent, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", allContent)
	}
	if len(allCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(allCalls), allContent)
	}
	fn, _ := allCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected bash, got %v", fn["name"])
	}
}

// TestAgentXMLCloseTagCaseInsensitive tests that </Tool_Call> is also accepted.
func TestAgentXMLCloseTagCaseInsensitive(t *testing.T) {
	var in AgentStreamInterceptor
	block := "text <<<TOOL_CALL>>>\n{\"name\":\"bash\",\"arguments\":{\"command\":\"pwd\"}}\n</Tool_Call>\nend"
	var allCalls []map[string]interface{}
	for i := 0; i < len(block); i += 5 {
		end := i + 5
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		allCalls = append(allCalls, p.ToolCalls...)
	}
	p := in.Finish()
	allCalls = append(allCalls, p.ToolCalls...)
	allCalls = reassembleStreamDeltas(allCalls)

	if len(allCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(allCalls), p.Content)
	}
	fn, _ := allCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected bash, got %v", fn["name"])
	}
}

// TestAgentGLM52KeyValueFormat tests the non-JSON key=value format that GLM 5.2
// sometimes emits inside tool-call markers.
func TestAgentGLM52KeyValueFormat(t *testing.T) {
	name, args, ok := agentLooseParse(`read_file path="README.md" start_line="1" end_line="200"`, nil)
	if !ok || name != "read_file" {
		t.Fatalf("expected read_file, got ok=%v name=%q", ok, name)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if m["path"] != "README.md" {
		t.Errorf("path wrong: %v", m["path"])
	}
	// start_line and end_line should be parsed as integers.
	if m["start_line"] != float64(1) {
		t.Errorf("start_line wrong: %v", m["start_line"])
	}
	if m["end_line"] != float64(200) {
		t.Errorf("end_line wrong: %v", m["end_line"])
	}
}

// TestAgentGLM52CompleteFlow tests the full streaming flow with GLM 5.2's
// non-JSON format inside tool-call markers.
func TestAgentGLM52CompleteFlow(t *testing.T) {
	var in AgentStreamInterceptor
	var content string
	var calls []map[string]interface{}
	block := "text <<<TOOL_CALL>>>\nread_file path=\"README.md\" start_line=\"1\" end_line=\"200\"\n<<<END_TOOL_CALL>>>\nend"
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		content += p.Content
		calls = append(calls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Errorf("expected read_file, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "README.md") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

// TestAgentUnclosedMarkerFallback tests that when the stream ends with an
// unclosed <<<TOOL_CALL>>> block, the interceptor tries to parse it as a tool call.
func TestAgentUnclosedMarkerFallback(t *testing.T) {
	var in AgentStreamInterceptor
	block := "text <<<TOOL_CALL>>>\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n"
	var allCalls []map[string]interface{}
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		allCalls = append(allCalls, p.ToolCalls...)
	}
	p := in.Finish()
	allCalls = append(allCalls, p.ToolCalls...)
	allCalls = reassembleStreamDeltas(allCalls)

	// Should parse the unclosed tool call.
	if len(allCalls) != 1 {
		t.Fatalf("expected 1 tool call from unclosed block, got %d (content=%q)", len(allCalls), p.Content)
	}
	fn, _ := allCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected bash, got %v", fn["name"])
	}
}

// TestAgentGLM52KeyValueWithXMLClose tests the exact pattern from the screenshot:
// <tool_call> with non-JSON key=value args.
func TestAgentGLM52KeyValueWithXMLClose(t *testing.T) {
	var in AgentStreamInterceptor
	block := `Tôi sẽ tiếp tục phân tích cấu trúc dự án.<<<TOOL_CALL>>>read_file path="qwen-api/README.md" start_line="637" end_line="702"
</tool_call>`
	var allCalls []map[string]interface{}
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		allCalls = append(allCalls, p.ToolCalls...)
	}
	p := in.Finish()
	allCalls = append(allCalls, p.ToolCalls...)
	allCalls = reassembleStreamDeltas(allCalls)

	if len(allCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(allCalls), p.Content)
	}
	fn, _ := allCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Errorf("expected read_file, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "qwen-api/README.md") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

// TestAgentNonJSONParse tests the agentParseKeyValueArgs function directly.
func TestAgentNonJSONParse(t *testing.T) {
	name, args, ok := agentParseKeyValueArgs(`read_file path="README.md"`)
	if !ok || name != "read_file" {
		t.Fatalf("expected read_file, got ok=%v name=%q", ok, name)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("args not JSON: %v", err)
	}
	if m["path"] != "README.md" {
		t.Errorf("path wrong: %v", m["path"])
	}
}

// TestAgentSplitQuotedTokens tests the quoted-token splitter.
func TestAgentSplitQuotedTokens(t *testing.T) {
	tokens := agentSplitQuotedTokens(`read_file path="hello world" start_line="1"`)
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d: %v", len(tokens), tokens)
	}
	if tokens[0] != "read_file" {
		t.Errorf("token 0 wrong: %q", tokens[0])
	}
	// The tokenizer preserves quotes around the value, so the token includes the quotes.
	if tokens[1] != `path="hello world"` {
		t.Errorf("token 1 wrong: %q", tokens[1])
	}
	if tokens[2] != `start_line="1"` {
		t.Errorf("token 2 wrong: %q", tokens[2])
	}
}

// TestGLM52StreamingRealWorld simulates the exact streaming pattern from the
// GLM 5.2 model: content arrives in small chunks across multiple Feed() calls.
func TestGLM52StreamingRealWorld(t *testing.T) {
	var in AgentStreamInterceptor
	var content string
	var calls []map[string]interface{}

	// Exact chunks from the live model output (decoded from JSON)
	chunks := []string{
		"\n",
		"<<<TOOL_CALL>>>",
		"\n{\"name\": \"run_terminal_command\", \"arguments\": {\"command\": \"ls -la\"}}",
		"\n<<<END_TOOL_CALL>>>",
		"\n\nMore text after.",
	}

	for _, chunk := range chunks {
		p := in.Feed(chunk)
		content += p.Content
		calls = append(calls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "run_terminal_command" {
		t.Errorf("expected run_terminal_command, got %v", fn["name"])
	}
}

// TestGLM52StreamingSingleBracket simulates GLM 5.2 using single-bracket
// <tool_call> tags with the </tool_call> close tag.
func TestGLM52StreamingSingleBracket(t *testing.T) {
	var in AgentStreamInterceptor
	var content string
	var calls []map[string]interface{}

	chunks := []string{
		"text ",
		"<TOOL_CALL>",
		"\nread_file path=\"README.md\" start_line=\"1\" end_line=\"200\"",
		"\n</TOOL_CALL>",
		"\nend",
	}

	for _, chunk := range chunks {
		p := in.Feed(chunk)
		content += p.Content
		calls = append(calls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	calls = append(calls, p.ToolCalls...)

	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("markers leaked as content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Errorf("expected read_file, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "README.md") {
		t.Errorf("arguments lost: %q", argsStr)
	}
}

// TestAgentExtractFromFullContent tests the safety-net extraction on the
// exact fullContent accumulated during a live GLM 5.2 stream.
func TestAgentExtractFromFullContent(t *testing.T) {
	// Build the content with actual newlines (not literal \n chars).
	var content strings.Builder
	content.WriteString("I will list the files in the current directory for you. ")
	content.WriteString("\n\n")
	content.WriteString("<<<TOOL_CALL>>>\n")
	content.WriteString(`{"name": "run_terminal_command", "arguments": {"command": "ls"}}`)
	content.WriteString("\n<<<END_TOOL_CALL>>>")
	t.Logf("fullContent: %q", content.String())

	calls := agentExtractToolCalls(content.String(), nil)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content.String())
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "run_terminal_command" {
		t.Errorf("expected run_terminal_command, got %v", fn["name"])
	}
}

// TestAgentExtractXMLCloseFromFullContent tests the safety-net with XML-style end tag.
func TestAgentExtractXMLCloseFromFullContent(t *testing.T) {
	var content strings.Builder
	content.WriteString("some text \n\n<<<TOOL_CALL>>>\n")
	content.WriteString(`{"name": "bash", "arguments": {"command": "ls"}}`)
	content.WriteString("\n</tool_call>\nmore text")
	// Use ParseAgentToolCalls directly — the legacy parser doesn't support
	// XML-style </tool_call> end markers.
	calls := ParseAgentToolCalls(content.String())
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(calls), content.String())
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected bash, got %v", fn["name"])
	}
}

// TestDebugXMLCloseStreaming removed (see fixed tests below)

// TestAgentColonKeyValueFormat tests the GLM 5.2 broken JSON format
// where the model outputs edit_file path": "kimi_chat.py edits": [{...}]
func TestAgentColonKeyValueFormat(t *testing.T) {
	// Build body with actual newlines (not literal backslash-n).
	body := "edit_file\npath\": \"kimi_chat.py\nedits\": [{\"old_text\": \"import argparse\", \"new_text\": \"import argparse\\nimport os\"}]"
	text := "<<<TOOL_CALL>>>" + body + "<<<END_TOOL_CALL>>>"
	t.Logf("test body: %q", body)

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (body=%q, text=%q)", len(calls), body, text)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "edit_file" {
		t.Errorf("expected edit_file, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "path") {
		t.Errorf("arguments missing path: %q", argsStr)
	}
}

// TestAgentFenceBeforeSingleBracketMarker tests that ``` fences before <TOOL_CALL> are stripped.
func TestAgentFenceBeforeSingleBracketMarker(t *testing.T) {
	var in AgentStreamInterceptor
	block := "```json\n<TOOL_CALL>\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n</TOOL_CALL>\n```\n"
	var allCalls []map[string]interface{}
	for i := 0; i < len(block); i += 5 {
		end := i + 5
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		allCalls = append(allCalls, p.ToolCalls...)
	}
	p := in.Finish()
	allCalls = append(allCalls, p.ToolCalls...)
	allCalls = reassembleStreamDeltas(allCalls)

	if len(allCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (content=%q)", len(allCalls), p.Content)
	}
	fn, _ := allCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected bash, got %v", fn["name"])
	}
}

// TestAgentTruncatedBracesRepair pins the truncated-JSON class observed in
// production: the model drops the OUTER closing brace(s) before the end
// marker (e.g. ..."timeout_ms":120000} + <<<END_TOOL_CALL>>> where only the
// arguments object closed). Previously the whole block leaked as visible
// text with no execution; agentCloseUnbalancedBraces now closes the still-
// open object(s) so the call parses.
func TestAgentTruncatedBracesRepair(t *testing.T) {
	// Exact shapes from the production failure report.
	bodies := []string{
		`{"name":"terminal","arguments":{"command":"cat go.mod; echo '---BUILD---'; go build ./... 2>&1; echo '---VET---'; go vet ./... 2>&1","cd":"qwen-proxy","timeout_ms":120000}`,
		`{"name":"terminal","arguments":{"command":"wc -l internal/zbridge/*.go cmd/q-bless/*.go main.go 2>/dev/null; echo '---'; ls tests/ testdata/ docs/ scripts/ 2>/dev/null"}`,
	}
	for i, body := range bodies {
		name, args, ok := agentLooseParse(body, nil)
		if !ok || name != "terminal" {
			t.Fatalf("body %d: expected terminal call, got ok=%v name=%q", i, ok, name)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(args, &m); err != nil {
			t.Fatalf("body %d: repaired arguments not valid JSON: %v (%s)", i, err, args)
		}
		if _, lost := m["command"]; !lost {
			t.Errorf("body %d: command argument lost: %v", i, m)
		}
		if _, lost := m["timeout_ms"]; i == 0 && !lost {
			t.Errorf("body 0: timeout_ms argument lost: %v", m)
		}
		if _, lost := m["cd"]; i == 0 && !lost {
			t.Errorf("body 0: cd argument lost: %v", m)
		}
	}
}

// TestAgentTruncatedBracesStreaming runs the truncated-brace block through
// the streaming interceptor: the streamed call must still complete (the
// marker closes a structurally-complete-but-unbalanced body) and nothing
// may leak as content.
func TestAgentTruncatedBracesStreaming(t *testing.T) {
	var in AgentStreamInterceptor
	block := "Mình sẽ kiểm tra dependencies và chạy build/test.<<<TOOL_CALL>>>{\"name\":\"terminal\",\"arguments\":{\"command\":\"go vet ./...\",\"cd\":\"qwen-proxy\",\"timeout_ms\":120000}" + "\n<<<END_TOOL_CALL>>>"
	var allCalls []map[string]interface{}
	var content string
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		p := in.Feed(block[i:end])
		content += p.Content
		allCalls = append(allCalls, p.ToolCalls...)
	}
	p := in.Finish()
	content += p.Content
	allCalls = append(allCalls, p.ToolCalls...)
	allCalls = reassembleStreamDeltas(allCalls)

	if len(allCalls) != 1 {
		t.Fatalf("expected 1 streamed call, got %d (content=%q)", len(allCalls), content)
	}
	fn, _ := allCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "terminal" {
		t.Errorf("expected terminal, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "go vet") {
		t.Errorf("streamed arguments lost command: %q", argsStr)
	}
	if strings.Contains(content, "TOOL_CALL") || strings.Contains(content, "timeout_ms") {
		t.Errorf("truncated block leaked as content: %q", content)
	}
}

// TestParseXMLTagToolCalls_NoNameInJSON locks in recovery of the XML-tag
// open form from bridge.log 2026-09-05 18:10 (req=a1464731): the model
// opened the block with <terminal>, dropped the JSON "name" key, and
// still closed with the standard END marker. The canonical span scan
// found nothing and the whole block leaked as content — the agent
// stalled. The fallback pass must pair the tag with the END marker and
// carry the tool name from the tag.
func TestParseXMLTagToolCalls_NoNameInJSON(t *testing.T) {
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	_ = open

	text := "<terminal>\n" +
		`{"arguments":{"cd":"/home/tlqbao/Desktop/qwen-proxy","command":"echo '=== CRATE COUNT ==='; ls crates | wc -l","timeout_ms":30000}}` +
		"\n" + closeM

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d (text=%q)", len(calls), text)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "terminal" {
		t.Errorf("expected name from tag, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "CRATE COUNT") || !strings.Contains(argsStr, "timeout_ms") {
		t.Errorf("arguments lost payload: %q", argsStr)
	}
}

// TestParseXMLTagToolCalls_JSONNameWins: when the JSON body carries a name
// (canonical or alternate spelling) it outranks the tag, matching the
// canonical pass's preference.
func TestParseXMLTagToolCalls_JSONNameWins(t *testing.T) {
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	text := "<terminal>\n" +
		`{"name":"bash","arguments":{"command":"ls"}}` +
		"\n" + closeM

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("JSON name must win over tag, got %v", fn["name"])
	}
}

// TestParseXMLTagToolCalls_NoEndMarkerNoCall: a tag over JSON WITHOUT the
// standard END marker right after is not a tool call (false-positive
// guard: <details> and other prose tags may precede JSON).
func TestParseXMLTagToolCalls_NoEndMarkerNoCall(t *testing.T) {
	text := "<terminal>\n" + `{"arguments":{"command":"ls"}}` + "\nplain trailing prose"
	if calls := ParseAgentToolCalls(text); len(calls) != 0 {
		t.Fatalf("no END marker must yield no calls, got %d", len(calls))
	}
}

// TestParseXMLTagToolCalls_CanonicalNotDuplicated: the fallback pass must
// stay dormant when the canonical pass already found calls, so a block
// that is canonical in one place and tag-style elsewhere never yields
// duplicates (the observed "double attempt" shape).
func TestParseXMLTagToolCalls_CanonicalNotDuplicated(t *testing.T) {
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	text := open + "\n" + `{"name":"bash","arguments":{"command":"ls"}}` + "\n" + closeM +
		"\n<terminal>\n" + `{"arguments":{"command":"pwd"}}` + "\n" + closeM

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("canonical call must win alone, got %d", len(calls))
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected canonical call, got %v", fn["name"])
	}
}

// TestParseXMLTagToolCalls_TruncatedJSON: the production block arrived
// with the outer object never closed (brace repair must close it before
// the args are forwarded).
func TestParseXMLTagToolCalls_TruncatedJSON(t *testing.T) {
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	// Inner object closed, outer never closed — exactly the production shape.
	jsonBody := `{"arguments":{"cd":"/home/tlqbao/Desktop/qwen-proxy","command":"du -sh ."}}`[:len(`{"arguments":{"cd":"/home/tlqbao/Desktop/qwen-proxy","command":"du -sh ."}}`)-1]
	text := "<terminal>\n" + jsonBody + "\n" + closeM

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d (text=%q)", len(calls), text)
	}
	argsStr, _ := calls[0]["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(argsStr, "du -sh") {
		t.Errorf("repaired arguments lost command: %q", argsStr)
	}
}

// TestParseBareEndToolCalls_MissingOpenMarker: the model emitted prose
// ending in the tool name, a JSON payload, and <<<END_TOOL_CALL>>> with no
// opening marker anywhere (the observed malformation). The recovery pass
// must still yield exactly one call, named from the trailing prose word.
func TestParseBareEndToolCalls_MissingOpenMarker(t *testing.T) {
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	text := "I'll check the README first.read_file" + `{"path":"qwen-proxy/README.md","start_line":148,"end_line":260}` + "\n" + closeM

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d (text=%q)", len(calls), text)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "read_file" {
		t.Errorf("expected name from trailing prose word, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "README.md") {
		t.Errorf("recovered arguments lost path: %q", argsStr)
	}
}

// TestParseBareEndToolCalls_NameInsideJSON: when the JSON body carries the
// tool name itself, it must win over any trailing prose word.
func TestParseBareEndToolCalls_NameInsideJSON(t *testing.T) {
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	text := "Let me look at the config." + `{"name":"bash","arguments":{"command":"ls"}}` + "\n" + closeM

	calls := ParseAgentToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d", len(calls))
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Errorf("expected name from JSON body, got %v", fn["name"])
	}
	argsStr, _ := fn["arguments"].(string)
	if !strings.Contains(argsStr, "ls") {
		t.Errorf("recovered arguments lost command: %q", argsStr)
	}
}

// TestParseBareEndToolCalls_NoJSONNoCall: prose + END marker without any
// JSON object before it must not fabricate a call (false-positive guard).
func TestParseBareEndToolCalls_NoJSONNoCall(t *testing.T) {
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	text := "Actually the marker here is stray prose." + closeM
	if calls := ParseAgentToolCalls(text); len(calls) != 0 {
		t.Fatalf("no JSON before END marker must yield no calls, got %d", len(calls))
	}
}
