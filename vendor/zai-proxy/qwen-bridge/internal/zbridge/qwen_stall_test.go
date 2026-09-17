package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// Built by concatenation so this source never contains a literal shim
// marker that downstream tooling could misparse (same trick as
// TestLooksLikeAnnouncementOnly below).
const (
	openMarker  = "<<<" + "TOOL_CALL" + ">>>"
	closeMarker = "<<<" + "END_TOOL_CALL" + ">>>"
)

// TestLooksLikeAnnouncementOnly locks in the stall detector used by the
// streaming handler: intent language ("I'll ...", "Let me ...", "tôi sẽ ...")
// with a short, marker-free content must be flagged, while real tool-call
// blocks, marker-tainted reasoning, empty content, and long output must not.
func TestLooksLikeAnnouncementOnly(t *testing.T) {
	// Built by concatenation so this source never contains a literal
	// shim marker that downstream tooling could misparse.
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"
	toolTag := "<" + "tool_call"

	cases := []struct {
		name      string
		content   string
		reasoning string
		want      bool
	}{
		// English intent phrases
		{"english i'll", "I'll check the file now.", "", true},
		{"english let me", "Let me look at the logs.", "", true},
		{"english i need to", "I need to read the config.", "", true},
		// Vietnamese intent phrases
		{"vietnamese toi se", "Tôi sẽ kiểm tra ngay.", "", true},
		{"vietnamese de toi", "Để tôi chạy thử đã.", "", true},
		// New Vietnamese intent shapes (production stalls 2026-09-06:
		// “Mình cần xem cách server xử lý…”, “Giờ mình viết pass recovery…”
		// ended the turn text-only because none matched the old list)
		{"vietnamese minh can", "Mình cần xem cấu hình server trước.", "", true},
		{"vietnamese toi can", "Tôi cần đọc phần còn lại của file.", "", true},
		{"vietnamese gio minh", "Giờ mình viết pass recovery cho block này.", "", true},
		{"vietnamese bay gio", "Bây giờ mình đọc README để trả lời.", "", true},
		{"vietnamese minh phai", "Mình phải đọc file config trước đã.", "", true},
		// Chinese intent phrases (GLM 5.3)
		{"chinese wo hui", "我会检查文件。", "", true},
		{"chinese rang wo", "让我查看日志。", "", true},
		{"chinese xianzai wo", "现在我来分析代码。", "", true},
		{"chinese wo xu yao", "我需要读取配置文件。", "", true},
		{"chinese wo jiang", "我将执行构建。", "", true},
		{"chinese wo lai", "我来检查错误。", "", true},
		{"chinese jie xia lai", "接下来我将运行测试。", "", true},
		// Reasoning-only stalls (GLM 5.3 emits intent in reasoning, content empty)
		{"reasoning only chinese", "", "让我检查文件。", true},
		{"reasoning only english", "", "Let me read the config.", true},
		{"reasoning only vietnamese", "", "Để tôi chạy thử.", true},
		// Junk-content stalls (bridge.log 2026-09-05 17:15 req=1d9d9bdf:
		// contentOut=1 byte of junk + 494b planning-only reasoning — the
		// intent lives entirely in reasoning but content is not literally
		// empty, so the old empty-check missed it)
		{"junk single char content", "<", "Let me verify the API contract first.", true},
		{"junk newline content", "\n", "Tôi sẽ kiểm tra hợp đồng API trước.", true},
		{"junk punctuation content", "…", "I should run the grep now.", true},
		{"junk content but clean reasoning", "—", "The config file has three sections.", false},
		// Vietnamese preamble stall (bridge.log 2026-09-05 17:42
		// req=19a275e7: "Trước khi khuyến nghị…, tôi xác minh nhanh…" —
		// intent phrase appears past the 32-byte streaming guard)
		{"vietnamese preamble truoc khi", "Trước khi khuyến nghị phương án ghép, tôi xác minh nhanh hợp đồng API của proxy.", "", true},
		// Edge cases: should NOT be flagged as stall
		{"empty content and reasoning", "", "", false},
		{"marker in content", "I'll check. " + open + `{"name":"x"}` + closeM, "", false},
		{"marker in reasoning", "I'll check now.", "emit " + open + " next", false},
		{"function_call tag", "I'll use a function_call now.", "", false},
		{"tool_call tag", "Let me send a " + toolTag + " now.", "", false},
		{"too long content", strings.Repeat("I'll do it. ", 300), "", false},
		{"too long reasoning", "", strings.Repeat("I'll do it. ", 300), false},
		{"plain final answer", "Done: build passes and tests are green.", "", false},
		{"chinese final answer", "构建完成，测试全部通过。", "", false},
		// A short but REAL answer must never consult reasoning (greeting
		// regression: the E2E triple-greeting bug)
		{"short real answer with planning reasoning", "Chào bạn! Mình có thể giúp gì?", "I should respond with a friendly greeting. Let me think about what to say.", false},
		{"single word answer", "OK", "Let me consider the options carefully first.", false},
	}
	for _, tc := range cases {
		if got := looksLikeAnnouncementOnly(tc.content, tc.reasoning); got != tc.want {
			t.Errorf("%s: got %v, want %v (content=%q reasoning=%q)", tc.name, got, tc.want, tc.content, tc.reasoning)
		}
	}
}

// TestAgentToolResultBlankTurn locks in the post-tool-result stall detector:
// after a tool RESULT the model's reply must be the next tool call or the
// final answer, so a turn ending with empty or junk-only content (and no
// tool call) is a stall even without any intent phrase. A real short answer
// ("done") has letters/digits and must never be flagged.
func TestAgentToolResultBlankTurn(t *testing.T) {
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"

	cases := []struct {
		name      string
		content   string
		reasoning string
		want      bool
	}{
		// The reproduced GLM 5.3 stall (bridge.log done-stop contentOut≈1
		// right after a tool result): empty/junk content, planning-only
		// reasoning, no phrase necessarily present.
		{"empty content clean reasoning", "", "The file contains the bug. I now have everything needed to answer.", true},
		{"empty content english thinking", "", "Let me now write the file with the findings.", true},
		{"empty content nothing", "", "", true},
		{"junk newline only", "\n", "I should produce the answer file now.", true},
		{"junk single byte", "<", "Let me verify the API contract first.", true},
		{"junk punctuation", "…", "Ready to write ANSWER.md.", true},
		// Real short answers have letters/digits — never a stall.
		{"done answer", "done", "", false},
		{"real short vi answer", "Xong!", "", false},
		{"real answer with reasoning", "Bug là Len() luôn trả 0, fix bằng len(s.m).", "Let me double check the store file.", false},
		{"junk but real answer after", "…\nĐã xong: bug nằm ở Len().", "", false},
		// Even a very long thinking block that ends blank after a tool result
		// is a stall: the retry budget (StallMaxRetries) bounds the cost.
		{"huge reasoning blank content", "", strings.Repeat("planning ", 400), true},
		// A leaked tool-call block counts as attempted (safety net handles it).
		{"leaked call block", "I'll write it now. " + open + `{"name":"write_file"}` + closeM, "", false},
	}
	for _, tc := range cases {
		if got := agentToolResultBlankTurn(tc.content, tc.reasoning); got != tc.want {
			t.Errorf("%s: got %v, want %v (content=%q reasoning=%q)", tc.name, got, tc.want, tc.content, tc.reasoning)
		}
	}
}

// TestAgentStallNudge verifies the retry nudge carries the shim's tool-call
// format and the system_nudge framing so the retried turn can actually emit
// a tool call instead of announcing intent again.
func TestAgentStallNudge(t *testing.T) {
	nudge := agentStallNudge()
	open := "<<<" + "TOOL_CALL" + ">>>"
	closeM := "<<<" + "END_TOOL_CALL" + ">>>"

	if !strings.HasPrefix(nudge, "\n\n") {
		t.Errorf("nudge must start with a blank-line separator, got prefix %q", nudge[:8])
	}
	if !strings.Contains(nudge, "<system_nudge>") || !strings.Contains(nudge, "</system_nudge>") {
		t.Errorf("nudge missing <system_nudge> framing: %q", nudge)
	}
	if !strings.Contains(nudge, open) || !strings.Contains(nudge, closeM) {
		t.Errorf("nudge missing shim tool-call marker: %q", nudge)
	}
	if !strings.Contains(nudge, "FINAL ANSWER") {
		t.Errorf("nudge must offer the final-answer escape hatch: %q", nudge)
	}
}

// TestStatusFromError verifies that statusFromError maps error strings to
// the correct HTTP status codes, including the WAF 405 and upstream 5xx codes.
// TestStallEvidentShapes locks in the GLM 5.3 failure shapes recorded in
// docs/stall_evident.md (2026-09-07):
//
//  1. "**Tool Call: **" — an unnamed tool call reached the client, which
//     rejected the body with "tool input was not fully received".
//  2. Pseudo-XML transcript leak ("<tool_result>\n<name>ls</name>…") — the
//     model echoed the prompt's XML framing instead of emitting a real call.
//  3. Half-open marker echo (""<<<TOOL_CALL\n{"name":"read_file", …") that
//     parsed into nothing, waved through as an "attempted call", so the
//     turn ended without a retry.
func TestStallEvidentShapes(t *testing.T) {
	t.Run("blank tool name never resolves in stream extraction", func(t *testing.T) {
		for _, body := range []string{
			`{"name":"","arguments":{"path":"x"}}`,
			`{"name": "", "arguments": {}}`,
			`{"name":""}`, // name-only block
		} {
			if name, ok := agentStreamExtractName(body); ok {
				t.Errorf("agentStreamExtractName(%q) = (%q, true); blank names must not resolve", body, name)
			}
		}
	})

	t.Run("blank-name tool-call body is detected as emptyNameBlock", func(t *testing.T) {
		if !emptyNameBlock(`{"name":"","arguments":{"path":"x"}}`) {
			t.Error("emptyNameBlock must flag a blank-name payload")
		}
		if !emptyNameBlock(`{"name": "", "arguments": null}`) {
			t.Error("emptyNameBlock must flag blank name with null arguments")
		}
		if !emptyNameBlock(`{"name":""}`) {
			t.Error("emptyNameBlock must flag name-only blank payload")
		}
		// Real names are NOT empty-name blocks.
		if emptyNameBlock(`{"name":"read_file","arguments":{"path":"x"}}`) {
			t.Error("emptyNameBlock must not flag a real name")
		}
		if emptyNameBlock(`{"arguments":{"command":"ls"}}`) {
			t.Error("args-only payload is not an empty-name block (inference owns it)")
		}
	})

	t.Run("blank-name block still infers a real tool from schema", func(t *testing.T) {
		tools := []openAITool{{Function: &openAIFnSpec{Name: "terminal", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{},"cd":{}}}`)}}}
		raw, _ := json.Marshal(tools)
		calls := ParseAgentToolCallsWithTools(
			openMarker+`{"name":"","arguments":{"command":"ls","cd":"/tmp"}}`+closeMarker,
			raw,
		)
		if len(calls) != 1 {
			t.Fatalf("expected 1 recovered call, got %d", len(calls))
		}
		if fn := calls[0]["function"].(map[string]interface{}); fn["name"] != "terminal" {
			t.Errorf("inferred name = %v, want terminal", fn["name"])
		}
	})

	t.Run("dangling marker transcript after tool result is a stall", func(t *testing.T) {
		// Exactly the stall_evident.md leak-2 transcript.
		content := "<tool_result>\n<name>ls</name>\n<output>file1\nfile2</output>\n</tool_result>\n<tool_call>"
		if !agentDanglingXMLCallMarker(content) {
			t.Error("dangling <tool_call> at end of transcript must be flagged")
		}
		if !agentToolResultBlankTurn(content, "planning only") {
			t.Error("post-tool-result turn ending in a dangling marker must be a stall")
		}
		// A WELL-FORMED pseudo-block is actually recoverable (single-bracket
		// opener + mirror closer are accepted): the parser must extract the
		// call, so it is NOT dangling — the safety net handles it.
		pseudo := "<assistant>\n<tool_call>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/a\"}}\n</arguments>"
		if n := len(ParseAgentToolCalls(pseudo)); n != 1 {
			t.Errorf("pseudo-block with valid payload must be recovered, got %d calls", n)
		}
		if agentDanglingXMLCallMarker(pseudo) {
			t.Error("a recovered pseudo-block must not be flagged dangling")
		}
		// An UNRECOVERABLE transcript (name blank — the doc's leak-1 shape)
		// is dangling.
		blank := "<assistant>\n<tool_call>\n{\"name\":\"\",\"arguments\":{\"path\":\"/a\"}}"
		if !agentDanglingXMLCallMarker(blank) {
			t.Error("transcript whose only payload has a blank name must be flagged dangling")
		}
	})

	t.Run("dangling marker is NOT flagged when the parser recovered the call", func(t *testing.T) {
		// If a real parse exists, the marker is not "dangling" — the
		// early-return "attempted call" verdict stands.
		ok := openMarker + `{"name":"read_file","arguments":{"path":"/a"}}` + closeMarker
		if agentDanglingXMLCallMarker("I'll check it.\n" + ok) {
			t.Error("a parsed canonical block must not be flagged dangling")
		}
		// Prose mentions of the vocabulary with a sentence after — ordinary text.
		for _, prose := range []string{
			"Let me send a <tool_call> now.",
			"The <<<TOOL_CALL>>> marker delimits calls.",
			"I'll emit the block, close it with </tool_call>, then explain.",
		} {
			if agentDanglingXMLCallMarker(prose) {
				t.Errorf("ordinary prose must not be flagged dangling: %q", prose)
			}
		}
	})

	t.Run("half-open marker echo with intent phrase stays detectable", func(t *testing.T) {
		// The doc's "<<<TOOL_CALL\n{\"name\":\"read_file\",\"arguments\":…" fragment
		// parses into nothing; the announcement in front of it must still
		// trip the stall detector instead of being waved through.
		content := "Tôi sẽ đọc cấu hình ngay. <<<TOOL_CALL\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/x\"}}"
		if !looksLikeAnnouncementOnly(content, "") {
			t.Error("half-open marker echo with an intent phrase must be detected as a stall")
		}
	})

	t.Run("recovered canonical block still suppresses the stall retry", func(t *testing.T) {
		// The original escape-hatch contract: a real parsed call is not a
		// stall even though marker text is present.
		content := "I'll check. " + openMarker + `{"name":"x"}` + closeMarker
		if looksLikeAnnouncementOnly(content, "") {
			t.Error("a parsed tool call must suppress the stall detection")
		}
	})

	t.Run("stall nudge warns against transcript echo and blank names", func(t *testing.T) {
		n := agentStallNudge()
		if !strings.Contains(n, "Never echo, quote, or role-play") {
			t.Error("nudge must warn against echoing the marker vocabulary")
		}
		if !strings.Contains(n, "NON-EMPTY tool name") {
			t.Error("nudge must require a non-empty tool name")
		}
	})
}

// TestToolLoopThinkingThrottle locks in the TOOL_LOOP_THINKING latency
// policy: a mid-loop agent turn (last message = tool RESULT) gets
// enable_thinking=false upstream (GLM 5.3 burns 5k-16k reasoning chars on
// mechanical decisions — docs/stall_evident.md), while opening turns (user
// message last) and explicit client thinking requests keep their config.
func TestToolLoopThinkingThrottle(t *testing.T) {
	restore := func(t *testing.T, mode string) {
		t.Helper()
		old := config.ToolLoopThinking
		config.ToolLoopThinking = mode
		t.Cleanup(func() { config.ToolLoopThinking = old })
	}
	t.Run("tool-result turn is throttled under default off", func(t *testing.T) {
		restore(t, "off")
		opts := QwenSendOptions{}
		config.throttleThinkingForTurn(&opts, true)
		if opts.Thinking == nil || *opts.Thinking {
			t.Fatalf("tool-result turn must get Thinking=false, got %v", opts.Thinking)
		}
		// The upstream features mapping turns this into enable_thinking:false.
	})
	t.Run("opening turn keeps thinking", func(t *testing.T) {
		restore(t, "off")
		opts := QwenSendOptions{}
		config.throttleThinkingForTurn(&opts, false)
		if opts.Thinking != nil {
			t.Fatalf("opening turn must keep Thinking nil, got %v", *opts.Thinking)
		}
	})
	t.Run("explicit client request wins over policy", func(t *testing.T) {
		restore(t, "off")
		want := true
		opts := QwenSendOptions{Thinking: &want}
		config.throttleThinkingForTurn(&opts, true)
		if opts.Thinking == nil || !*opts.Thinking {
			t.Fatal("explicit Thinking=true must survive the policy")
		}
	})
	t.Run("policy disabled with TOOL_LOOP_THINKING=on", func(t *testing.T) {
		restore(t, "on")
		opts := QwenSendOptions{}
		config.throttleThinkingForTurn(&opts, true)
		if opts.Thinking != nil {
			t.Fatal("mode=on must never touch Thinking")
		}
	})
	t.Run("lastMessageIsToolResult detects the tool tail case-insensitively", func(t *testing.T) {
		mk := func(role string) []Message {
			return []Message{{Role: "user", Content: json.RawMessage(`"x"`)}, {Role: role, Content: json.RawMessage(`"y"`)}}
		}
		if !lastMessageIsToolResult(mk("tool")) || !lastMessageIsToolResult(mk("Tool")) {
			t.Fatal("role=tool tail must be detected (case-insensitive)")
		}
		for _, role := range []string{"user", "assistant", "system", ""} {
			if lastMessageIsToolResult(mk(role)) {
				t.Errorf("role=%q tail must NOT count as a tool result", role)
			}
		}
		if lastMessageIsToolResult(nil) {
			t.Fatal("empty messages must not count as tool-result tail")
		}
	})
}

func TestStatusFromError(t *testing.T) {
	cases := []struct {
		errMsg string
		want   int
	}{
		{"Qwen error 401: unauthorized", 401},
		{"Qwen error 403: forbidden", 403},
		{"Qwen error 405: Method Not Allowed", 405},
		{"Qwen error 429: rate limited", 429},
		{"Qwen error 400: bad request", 400},
		{"Qwen error 502: bad gateway", 502},
		{"Qwen error 503: service unavailable", 503},
		{"Qwen error 504: gateway timeout", 504},
		{"something else entirely", 500},
		{"", 500},
	}
	for _, tc := range cases {
		if got := statusFromError(tc.errMsg); got != tc.want {
			t.Errorf("statusFromError(%q) = %d, want %d", tc.errMsg, got, tc.want)
		}
	}
}

// TestAgentHasClosingQuote verifies the closing-quote detector used by the
// colon-format multi-line string collector.
func TestAgentHasClosingQuote(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{`"hello"`, true},
		{`"hello`, false},
		{`"he\"llo"`, true},
		{`"he\"llo`, false},
		{`"he\"llo\" world"`, true},
		{"", false},
		{"no quotes here", false},
		{`"just an opening`, false},
		{`at end"`, true},
	}
	for _, tc := range cases {
		if got := agentHasClosingQuote(tc.s); got != tc.want {
			t.Errorf("agentHasClosingQuote(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// TestColonFormatMultiLineString verifies that the colon-format parser
// collects multi-line string values (e.g. old_text/new_text spanning lines)
// that GLM 5.2/5.3 emit inside broken JSON.
func TestColonFormatMultiLineString(t *testing.T) {
	body := "edit_file\npath: \"file.py\"\nold_text: \"import argparse\nimport sys\"\nnew_text: \"import argparse\nimport os\nimport sys\""
	name, args, ok := agentParseColonKeyValueArgs("edit_file", body)
	if !ok || name != "edit_file" {
		t.Fatalf("expected edit_file, got ok=%v name=%q", ok, name)
	}
	var m map[string]string
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("arguments not valid JSON: %v\n%s", err, string(args))
	}
	if !strings.Contains(m["old_text"], "import argparse\nimport sys") {
		t.Errorf("old_text not collected across lines: %q", m["old_text"])
	}
	if !strings.Contains(m["new_text"], "import argparse\nimport os\nimport sys") {
		t.Errorf("new_text not collected across lines: %q", m["new_text"])
	}
}

// TestColonFormatMultiLineStringStreaming tests the multi-line string
// collection through the full agentParseKeyValueArgs entry point.
func TestColonFormatMultiLineStringStreaming(t *testing.T) {
	body := "write_file\npath: \"config.py\"\ncontent: \"import os\nimport sys\n\ndef main():\n    pass\""
	name, args, ok := agentParseKeyValueArgs(body)
	if !ok || name != "write_file" {
		t.Fatalf("expected write_file, got ok=%v name=%q", ok, name)
	}
	var m map[string]string
	if err := json.Unmarshal(args, &m); err != nil {
		t.Fatalf("arguments not valid JSON: %v\n%s", err, string(args))
	}
	if !strings.Contains(m["content"], "def main():") {
		t.Errorf("content not collected across lines: %q", m["content"])
	}
}
