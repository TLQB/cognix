package zbridge

// Tests for the args-only payload recovery (production shape from
// error_toolcall_log.md 2026-09-06): the model emits an opener, an
// arguments-only JSON body with no name key (outer brace truncated), then a
// mirror closing tag built from the argument key. The bridge must recover the
// tool call via schema fingerprinting instead of leaking the block as text.

import (
	"encoding/json"
	"strings"
	"testing"
)

// e2eStyleTools mirrors a realistic client tool set: run_bash/command,
// read_file/path + line range, write_file/path + content.
const e2eStyleTools = `[
  {"type":"function","function":{"name":"run_bash","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"},"cd":{"type":"string"},"timeout_ms":{"type":"number"}},"required":["command"]}}},
  {"type":"function","function":{"name":"read_file","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"number"},"end_line":{"type":"number"}},"required":["path"]}}},
  {"type":"function","function":{"name":"write_file","description":"write","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path"]}}}
]`

func argOnlyCalls(t *testing.T) []map[string]interface{} {
	t.Helper()
	// Exact production shape: prose, single-bracket lowercase opener,
	// args-only body ({"arguments":{...}} with the OUTER brace never closed
	// -- only the inner arguments object closed), mirror closer.
	text := "Tôi sẽ kiểm tra mức độ sẵn sàng release thực tế. <tool_call>\n" +
		`{"arguments":{"command":"echo '=== GIT STATUS ==='; git --no-optional-locks status --short","cd":"/home/tlqbao/Desktop/zai-proxy","timeout_ms":120000,"head_lines":60}` +
		"\n</arguments>\n"
	return ParseAgentToolCallsWithTools(text, json.RawMessage(e2eStyleTools))
}

func argOnlyCallName(call map[string]interface{}) string {
	fn, _ := call["function"].(map[string]interface{})
	if fn == nil {
		return ""
	}
	n, _ := fn["name"].(string)
	return n
}

func argOnlyCallArgs(call map[string]interface{}) string {
	fn, _ := call["function"].(map[string]interface{})
	if fn == nil {
		return ""
	}
	a, _ := fn["arguments"].(string)
	return a
}

func TestArgOnly_MirrorCloser_RecoversCall(t *testing.T) {
	calls := argOnlyCalls(t)
	if len(calls) != 1 {
		t.Fatalf("expected 1 recovered call, got %d", len(calls))
	}
	if n := argOnlyCallName(calls[0]); n != "run_bash" {
		t.Fatalf("inferred name = %q, want run_bash", n)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(argOnlyCallArgs(calls[0])), &m); err != nil {
		t.Fatalf("arguments not valid JSON: %v (%q)", err, argOnlyCallArgs(calls[0]))
	}
	if c, _ := m["command"].(string); !strings.Contains(c, "GIT STATUS") {
		t.Errorf("command lost: %v", m["command"])
	}
	if cd, _ := m["cd"].(string); cd != "/home/tlqbao/Desktop/zai-proxy" {
		t.Errorf("cd lost: %v", m["cd"])
	}
}

func TestArgOnly_MirrorCloser_NoSchemas_NoFalseGuess(t *testing.T) {
	// Without schemas the args-only body cannot be attributed to a tool:
	// the bridge must NOT fabricate a call (a wrong guess executes the wrong
	// tool) and must leave the block out of the calls list.
	text := "<tool_call>\n" +
		`{"arguments":{"command":"git status --short","cd":"/tmp"}}` +
		"\n</arguments>\n"
	calls := ParseAgentToolCallsWithTools(text, nil)
	if len(calls) != 0 {
		t.Fatalf("no-schemas args-only must yield no calls, got %d (%v)", len(calls), calls)
	}
}

func TestArgOnly_MirrorCloser_StripsBlock(t *testing.T) {
	text := "before <tool_call>\n" +
		`{"arguments":{"command":"ls"}}` + "\n</arguments> after"
	stripped := StripAgentToolCalls(text)
	if strings.Contains(stripped, "arguments") || strings.Contains(stripped, "command") {
		t.Fatalf("block leaked into content: %q", stripped)
	}
	if !strings.Contains(stripped, "before") || !strings.Contains(stripped, "after") {
		t.Fatalf("surrounding prose damaged: %q", stripped)
	}
}

func TestArgOnly_StreamPath_RecoversCall(t *testing.T) {
	// Feed the production shape through the streaming interceptor in small
	// chunks so marker bytes arrive split across Feed calls, exactly like an
	// upstream SSE stream. The fallback parser must resolve the block as
	// soon as the mirror closer completes -- not only at Finish.
	text := "Tôi sẽ kiểm tra. <tool_call>\n" +
		`{"arguments":{"command":"go test ./... -count=1","cd":"/home/tlqbao/Desktop/zai-proxy","timeout_ms":120000}` +
		"\n</arguments>\nHoàn tất kiểm tra."

	var in AgentStreamInterceptor
	in.toolSchemas = agentCollectToolSchemas(json.RawMessage(e2eStyleTools))

	var allCalls []map[string]interface{}
	var content strings.Builder
	rest := text
	for len(rest) > 0 {
		n := 7
		if n > len(rest) {
			n = len(rest)
		}
		p := in.Feed(rest[:n])
		allCalls = append(allCalls, p.ToolCalls...)
		content.WriteString(p.Content)
		rest = rest[n:]
	}
	p := in.Finish()
	allCalls = append(allCalls, p.ToolCalls...)
	content.WriteString(p.Content)
	allCalls = reassembleStreamDeltas(allCalls)

	if len(allCalls) != 1 {
		t.Fatalf("stream: expected 1 call, got %d (content=%q)", len(allCalls), content.String())
	}
	if n := argOnlyCallName(allCalls[0]); n != "run_bash" {
		t.Fatalf("stream: inferred name = %q, want run_bash", n)
	}
	args := argOnlyCallArgs(allCalls[0])
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("stream: arguments not valid JSON: %v (%q)", err, args)
	}
	if strings.Contains(content.String(), "arguments") || strings.Contains(content.String(), "command") {
		t.Errorf("stream: block leaked as content: %q", content.String())
	}
	if !strings.Contains(content.String(), "Hoàn tất kiểm tra") {
		t.Errorf("stream: trailing prose lost: %q", content.String())
	}
}

func TestArgOnly_InferenceRules(t *testing.T) {
	schemas := []agentToolSchema{
		{name: "run_bash", props: map[string]bool{"command": true, "cd": true, "timeout_ms": true}},
		{name: "read_file", props: map[string]bool{"path": true, "start_line": true, "end_line": true}},
	}
	// Unique fingerprint.
	if n := agentInferToolName(json.RawMessage(`{"command":"ls","cd":"/x"}`), schemas); n != "run_bash" {
		t.Errorf("unique match: got %q, want run_bash", n)
	}
	// Majority rule: half the keys matching still identifies the tool.
	if n := agentInferToolName(json.RawMessage(`{"command":"ls","weird_key":1}`), schemas); n != "run_bash" {
		t.Errorf("majority match: got %q, want run_bash", n)
	}
	// No schemas at all.
	if n := agentInferToolName(json.RawMessage(`{"command":"ls"}`), nil); n != "" {
		t.Errorf("no schemas: got %q, want empty", n)
	}
	// Unknown keys only.
	if n := agentInferToolName(json.RawMessage(`{"foo":"bar","baz":1}`), schemas); n != "" {
		t.Errorf("unknown keys: got %q, want empty (no tool covers half)", n)
	}
	// Ambiguity: two tools sharing the same key must not guess.
	tie := []agentToolSchema{
		{name: "tool_a", props: map[string]bool{"path": true}},
		{name: "tool_b", props: map[string]bool{"path": true}},
	}
	if n := agentInferToolName(json.RawMessage(`{"path":"x"}`), tie); n != "" {
		t.Errorf("ambiguous: got %q, want empty", n)
	}
}

func TestArgOnly_NamedWinsOverInferred(t *testing.T) {
	// Double attempt inside ONE body: a leading args-only fragment followed
	// by the real named payload. The explicit name must win over inference
	// on the first fragment.
	body := `{"arguments":{"command":"ls"}} noise {"name":"read_file","arguments":{"path":"README.md"}}`
	name, args, ok := agentLooseParse(body, agentCollectToolSchemas(json.RawMessage(e2eStyleTools)))
	if !ok || name != "read_file" {
		t.Fatalf("named payload must win, got name=%q ok=%v", name, ok)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil || m["path"] != "README.md" {
		t.Fatalf("wrong arguments recovered: %v %q", err, string(args))
	}
}

func TestArgOnly_XMLTag_WithMirrorCloser(t *testing.T) {
	// XML-style opener + args-only body + mirror closer: the tag itself
	// carries the tool name, so no inference is needed.
	text := "<run_bash>\n" +
		`{"arguments":{"command":"go vet ./..."}` +
		"\n</arguments>\n"
	calls := ParseAgentToolCallsWithTools(text, json.RawMessage(e2eStyleTools))
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if n := argOnlyCallName(calls[0]); n != "run_bash" {
		t.Fatalf("name = %q, want run_bash", n)
	}
}

func TestArgOnly_CanonicalStillWins(t *testing.T) {
	// A canonical named call in the same text with a mirror-closed block:
	// both calls recover, canonical one keeps its explicit name.
	text := "<tool_call>\n" + `{"arguments":{"command":"ls"}}` + "\n</arguments>\n" +
		"<<<TOOL_CALL>>>\n" + `{"name":"read_file","arguments":{"path":"main.go"}}` + "\n<<<END_TOOL_CALL>>>"
	calls := ParseAgentToolCallsWithTools(text, json.RawMessage(e2eStyleTools))
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if argOnlyCallName(calls[0]) != "run_bash" || argOnlyCallName(calls[1]) != "read_file" {
		t.Fatalf("names = %q, %q", argOnlyCallName(calls[0]), argOnlyCallName(calls[1]))
	}
}
