package zbridge

// Regression tests for the failures observed in the real E2E conversation
// log (log_e2e.md), branch q-bless:
//
//   leak-1  — the model emitted a malformed "double attempt": a tag-style
//             pseudo-call (<list_directory>{args}) followed by the
//             canonical block. agentLooseParse only tried the FIRST JSON
//             object (args-only, no "name") and the WHOLE block leaked as
//             raw text with no execution.
//   leak-2  — same shape at stream end: <grep>{args}</grep> then
//             <Tool_Call>{"name","arguments"}</Tool_Call> — the canonical
//             safety net also uses agentLooseParse and leaked too.
//   greet   — one "chào bạn" turn rendered THREE concatenated greetings:
//             looksLikeAnnouncementOnly matched "I should " inside the
//             thinking block and stall-retried twice; SSE cannot un-send.
//   cut     — every <thinking> block ended mid-word ("...friendly greetin")
//             because flush() advanced the reasoning emitter while
//             discarding the held-back delta.
//
// Marker literals are built by concatenation so this source file never
// contains a shim marker that downstream tooling could misparse.

import (
	"encoding/json"
	"strings"
	"testing"
)

// withAgentModern forces the modern interceptor/extractor dispatch for a
// unit test: agentExtractToolCalls and newAgentInterceptor route by
// config.agentModern(), which reads global config the test never sets.
func withAgentModern(t *testing.T) {
	t.Helper()
	oldMode, oldVariant := config.AgentMode, config.AgentModeVariant
	config.AgentMode = true
	config.AgentModeVariant = ""
	t.Cleanup(func() {
		config.AgentMode, config.AgentModeVariant = oldMode, oldVariant
	})
}

// ── leak-1 / leak-2: double-attempt blocks must parse, not leak ─────────────

func TestRegression_DoubleAttempt_TagPseudoCallThenCanonical(t *testing.T) {
	withAgentModern(t)
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	// Exact shape from log_e2e.md L627-631 (base64 in the analysis): the
	// tag-format pseudo-call body sits INSIDE the canonical block.
	text := "Tôi sẽ xem xét cấu trúc code chính trong `internal/zbridge` để đánh giá kiến trúc chi tiết hơn.\n" +
		"<list_directory>\n" +
		`{"path":"qwen-proxy/internal/zbridge"}` + "\n" +
		open + "\n" +
		`{"name":"list_directory","arguments":{"path":"qwen-proxy/internal/zbridge"}}` + "\n" +
		closeM + "\n"

	// Finished-text path (safety net).
	calls := agentExtractToolCalls(text, nil)
	if len(calls) != 1 {
		t.Fatalf("safety net: expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "list_directory" {
		t.Fatalf("safety net: expected list_directory, got %v", n)
	}

	// Streaming path (interceptor) must neither leak nor lose the call.
	content, scalls := feedSimulatedStream(t, text)
	if len(scalls) != 1 {
		t.Fatalf("streaming: expected 1 call, got %d (content=%q)", len(scalls), content)
	}
	if n := extractToolNames(scalls); n[0] != "list_directory" {
		t.Fatalf("streaming: expected list_directory, got %v", n)
	}
	if strings.Contains(content, "TOOL_CALL") || strings.Contains(content, "list_directory>") {
		t.Errorf("block leaked as content: %q", content)
	}
}

func TestRegression_DoubleAttempt_XMLStyleAtStreamEnd(t *testing.T) {
	withAgentModern(t)
	// Exact shape from log_e2e.md L1920-1922: <grep>{...}</grep> pseudo-call
	// followed by a single-bracket <Tool_Call> with the correct schema.
	text := "Tôi sẽ xem xét code xử lý tool call trong `agent.go` và các file liên quan để đánh giá rủi ro.\n" +
		"<grep>" + `{"regex":"tool_call|tool_use|function_call","include_pattern":"qwen-proxy/internal/zbridge/agent.go","offset":0}` + "</grep>\n" +
		"<Tool_Call>" + `{"name": "grep", "arguments": {"include_pattern": "qwen-proxy/internal/zbridge/agent.go", "offset": 0, "regex": "tool_call|tool_use|function_call"}}` + "</Tool_Call>\n"

	calls := agentExtractToolCalls(text, nil)
	if len(calls) != 1 {
		t.Fatalf("safety net: expected 1 call, got %d", len(calls))
	}
	if n := extractToolNames(calls); n[0] != "grep" {
		t.Fatalf("safety net: expected grep, got %v", n)
	}
	fn := calls[0]["function"].(map[string]interface{})
	if args, ok := fn["arguments"].(string); ok {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(args), &m); err == nil {
			if m["include_pattern"] != "qwen-proxy/internal/zbridge/agent.go" {
				t.Errorf("arguments not preserved: %q", args)
			}
		}
	}

	content, scalls := feedSimulatedStream(t, text)
	if len(scalls) != 1 {
		t.Fatalf("streaming: expected 1 call, got %d (content=%q)", len(scalls), content)
	}
	if strings.Contains(content, "Tool_Call") || strings.Contains(content, "grep>") {
		t.Errorf("block leaked as content: %q", content)
	}
}

func TestRegression_LooseParse_TriesAllJSONObjects(t *testing.T) {
	// Direct unit test of the fix: the first object is args-only, the real
	// call follows.
	name, args, ok := agentLooseParse(`{"path":"x/y"} noise {"name":"read_file","arguments":{"path":"x"}}`, nil)
	if !ok || name != "read_file" {
		t.Fatalf("agentLooseParse: expected read_file, got name=%q ok=%v", name, ok)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil || m["path"] != "x" {
		t.Errorf("arguments not recovered: %v %q", err, string(args))
	}
}

// ── greet: reasoning-only intent language must not trigger stall retry ──────

func TestRegression_Greeting_NotStall(t *testing.T) {
	// Exact content shape from log_e2e.md: a complete greeting answer whose
	// thinking says "I should respond with a friendly greeting".
	content := "Chào bạn! 👋\n\nMình là GLM-5.2, trợ lý AI của Cognix. Mình có thể giúp bạn:\n\n- Phân tích, viết sửa code\n- Tìm kiếm trong codebase\n- Chạy lệnh terminal, build, test\n- Debug vấn đề khó\n- Refactor và tối ưu code\n\nBạn có thể mô tả giúp tôi về dự án `qwen-proxy` không? 😊"
	reasoning := "The user is greeting me in Vietnamese (\"chào bạn\" means \"hello friend\" in Vietnamese). This is a simple greeting, not a coding task that requiresany tool usage. I should respond with a friendly greeting"
	if got := looksLikeAnnouncementOnly(content, reasoning); got {
		t.Error("greeting turn must NOT be flagged as stall (reasoning-only intent is normal planning)")
	}
}

func TestRegression_Greeting_EachAttemptIndependently(t *testing.T) {
	// Every retry attempt (with the nudge appended) still must not stall:
	// a greeting remains a final answer even after nudges.
	content := "Chào bạn! 👋 Mình là trợ lý AI. Bạn cần gì không?"
	reasoning := "The user said \"chào bạn\" which is Vietnamese for \"hello friend\" or \"hello\". This is a simple greet"
	if got := looksLikeAnnouncementOnly(content, reasoning); got {
		t.Error("greeting must not be a stall even on retry attempts")
	}
}

func TestRegression_TrueStalls_StillDetected(t *testing.T) {
	// The fix must not break real stall detection.
	cases := []struct{ content, reasoning string }{
		{"I'll check the file now.", ""},
		{"Tôi sẽ kiểm tra ngay.", ""},
		{"让我检查文件。", ""},
		{"", "Let me read the config."}, // GLM empty-content stall
		{"", "让我检查文件。"},
	}
	for _, tc := range cases {
		if got := looksLikeAnnouncementOnly(tc.content, tc.reasoning); !got {
			t.Errorf("real stall missed: content=%q reasoning=%q", tc.content, tc.reasoning)
		}
	}
}

// ── cut: reasoning tail must complete at the thinking→answer transition ─────

func TestRegression_ReasoningTailCompletes_DeltaContent(t *testing.T) {
	thinking := "I should respond with a friendly greeting"
	answer := "Chào bạn! 👋"

	// Build SSE in the Qwen streaming shape: reasoning rides
	// delta.extra.summary_thought.content (phase "thinking_summary", lines
	// array), then the answer as plain delta.content.
	var b strings.Builder
	b.WriteString("data: {\"choices\":[{\"delta\":{\"phase\":\"thinking_summary\",\"status\":\"typing\",\"extra\":{\"summary_thought\":{\"content\":[" + jsonQuote(thinking) + "]}}},\"index\":0}]}\n\n")
	b.WriteString("data: {\"choices\":[{\"delta\":{\"content\":" + jsonQuote(answer) + "},\"index\":0}]}\n\n")
	b.WriteString("data: [DONE]\n\n")

	ch := make(chan QwenResult, 64)
	go func() {
		if err := streamSSEResponse(strings.NewReader(b.String()), ch, "test-req-id"); err != nil {
			t.Errorf("streamSSEResponse error: %v", err)
		}
		close(ch)
	}()

	var gotReasoning, gotContent string
	for r := range ch {
		if r.Err != nil {
			t.Fatalf("stream error: %v", r.Err)
		}
		gotReasoning += r.Reasoning
		gotContent += r.Chunk
	}

	if gotReasoning != thinking {
		t.Errorf("reasoning truncated: got %q, want %q", gotReasoning, thinking)
	}
	if gotContent != answer {
		t.Errorf("content wrong: got %q, want %q", gotContent, answer)
	}
}

// ── TTFT: phrase-free content must stream live, phrase content stays held ────

func TestRegression_ContentHoldGate(t *testing.T) {
	// A greeting/answer without any intent phrase cannot trip
	// looksLikeAnnouncementOnly no matter how it grows — it must be
	// released to live streaming once past the tail-guard, instead of
	// being held to stream end (TTFT bug: "content bị hold, đợi rồi in
	// 1 loạt").
	if contentNeedsStallHold("Chào bạn! 👋 Mình là trợ lý AI của Cognix, sẵn sàng hỗ trợ bạn.") {
		t.Error("phrase-free content past the guard must not be held (TTFT)")
	}
	// Short prefixes stay held: a phrase can still complete at the tail.
	if !contentNeedsStallHold("Chào bạn") {
		t.Error("short prefix must stay held (phrase can complete at the tail)")
	}
	// Content with an intent phrase is a stall candidate — must stay held
	// so the end-of-stream detector can still retry it.
	if !contentNeedsStallHold("Tôi sẽ xem xét cấu trúc code của dự án này trước.") {
		t.Error("phrase content must stay held (stall candidate)")
	}
	if !contentNeedsStallHold("I'll check the file now, one moment please.") {
		t.Error("English phrase content must stay held (stall candidate)")
	}
	// An empty-content turn relies on the reasoning fallback; the very
	// first content bytes stay held as usual.
	if !contentNeedsStallHold("") {
		t.Error("empty content must stay held (detector fallback pending)")
	}
}

func TestRegression_Gate_MatchesDetector_Exact(t *testing.T) {
	// The hold gate must be exactly as wide as the detector: anything the
	// gate releases must be unable to trip looksLikeAnnouncementOnly on
	// its existing bytes, and everything the detector fires on must have
	// been held.
	for _, tc := range []struct{ content, reasoning string }{
		{"Chào bạn! 👋 Mình là trợ lý AI của Cognix, sẵn sàng hỗ trợ bạn.", ""},
		{"Tôi sẽ xem xét cấu trúc code của dự án này trước.", ""},
		{"I'll check the file now, one moment please.", ""},
		{"", "Let me read the config."},
	} {
		detector := looksLikeAnnouncementOnly(tc.content, tc.reasoning)
		held := contentNeedsStallHold(tc.content)
		if detector && !held {
			t.Errorf("gate narrower than detector: detector fires but content released live (content=%q)", tc.content)
		}
	}
}

func TestRegression_HeldStalls_CanStillBeRetried(t *testing.T) {
	// True stalls (the shapes the retry exists for) must remain held and
	// detectable at stream end — the TTFT release must not break them.
	for _, tc := range []struct{ content, reasoning string }{
		{"Tôi sẽ đọc file này.", ""},
		{"让我检查文件。", ""},
		{"", "I'll grep the codebase for the handler."},
	} {
		if !contentNeedsStallHold(tc.content) {
			t.Errorf("stall candidate released live: content=%q", tc.content)
			continue
		}
		if !looksLikeAnnouncementOnly(tc.content, tc.reasoning) {
			t.Errorf("stall candidate no longer detected at stream end: content=%q", tc.content)
		}
	}
}

// jsonQuote marshals s as a JSON string literal (for building SSE fixtures).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
