package zbridge

// Regression tests for the GLM-5.3 stall shapes observed in the 2026-09-06
// production session export (docs/log_stall.md):
//
//   args-truncate — the upstream stream died mid-arguments 10×; the client
//     reassembled fragments into unparseable JSON and rejected every call
//     with "tool input was not fully received" / "Error parsing input JSON",
//     losing the whole turn. The interceptor must now append a minimal JSON
//     closure at end-of-stream so the reassembled arguments stay parseable.
//
//   name-glue — the model glued the tool name to neighbouring tokens
//     ("read_filearguments", "read_fileailing", "grep_arguments_placeholder",
//     "code.read_file"); the client rejected the calls with "No tool named X
//     exists". agentSnapToolName must repair them against the request's
//     declared tool list.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamTruncatedArgumentsGetJSONClosure feeds a stream that dies in the
// middle of the arguments object and asserts Finish() emits a closure that
// makes the concatenated fragments valid JSON again.
func TestStreamTruncatedArgumentsGetJSONClosure(t *testing.T) {
	cases := []struct {
		name  string
		chunk string // everything up to the point the stream dies
	}{
		{"string value cut", "<<<TOOL_CALL>>>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"qwen-proxy/READ"},
		{"key cut", "<<<TOOL_CALL>>>\n{\"name\":\"edit_file\",\"arguments\":{\"path\":\"a.go\",\"old"},
		{"nested object cut", "<<<TOOL_CALL>>>\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\",\"edits\":[{\"old_text\":\"x\""},
		{"number cut", "<<<TOOL_CALL>>>\n{\"name\":\"terminal\",\"arguments\":{\"timeout\":12."},
		{"keyword cut", "<<<TOOL_CALL>>>\n{\"name\":\"grep\",\"arguments\":{\"regex\":\"x\",\"case_sensitive\":tru"},
	}
	for _, tc := range cases {
		in := &AgentStreamInterceptor{}
		var args strings.Builder
		collect := func(parsed AgentParsedChunk) {
			for _, call := range parsed.ToolCalls {
				fn, _ := call["function"].(map[string]interface{})
				frag, _ := fn["arguments"].(string)
				args.WriteString(frag)
			}
		}
		collect(in.Feed(tc.chunk))
		collect(in.Finish())
		joined := args.String()
		if joined == "" {
			t.Errorf("%s: no argument bytes emitted", tc.name)
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(joined), &obj); err != nil {
			t.Errorf("%s: joined arguments not parseable: %v\nargs=%q", tc.name, err, joined)
		}
	}
}

// TestStreamUnbalancedMarkerGetsClosure pins the marker-arrival path: the
// model bails mid-JSON and closes the block with <<<END_TOOL_CALL>>> — the
// streamed fragments plus the closure must reassemble to parseable JSON with
// no marker bytes inside.
func TestStreamUnbalancedMarkerGetsClosure(t *testing.T) {
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"edit_file\",\"arguments\":{\"path\":\"a.go\",\"old\"\n<<<END_TOOL_CALL>>>"
	in := &AgentStreamInterceptor{}
	var args strings.Builder
	collect := func(parsed AgentParsedChunk) {
		for _, call := range parsed.ToolCalls {
			fn, _ := call["function"].(map[string]interface{})
			frag, _ := fn["arguments"].(string)
			args.WriteString(frag)
		}
	}
	collect(in.Feed(stream))
	collect(in.Finish())
	joined := args.String()
	if strings.Contains(joined, "TOOL_CALL") || strings.Contains(joined, "<") {
		t.Errorf("marker bytes leaked into arguments: %q", joined)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(joined), &obj); err != nil {
		t.Errorf("joined arguments not parseable: %v\nargs=%q", err, joined)
	}
	if _, ok := obj["path"]; !ok {
		t.Errorf("complete key lost in closure: %q", joined)
	}
}

// TestStreamTruncatedArgumentsKeepOriginalBytes verifies the closure is
// purely additive: everything the model actually wrote must survive verbatim
// as a prefix of the joined arguments.
func TestStreamTruncatedArgumentsKeepOriginalBytes(t *testing.T) {
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"qwen-proxy/READ"
	in := &AgentStreamInterceptor{}
	var args strings.Builder
	collect := func(parsed AgentParsedChunk) {
		for _, call := range parsed.ToolCalls {
			fn, _ := call["function"].(map[string]interface{})
			frag, _ := fn["arguments"].(string)
			args.WriteString(frag)
		}
	}
	collect(in.Feed(stream))
	fedArgs := args.String()
	collect(in.Finish())
	if !strings.HasPrefix(args.String(), fedArgs) {
		t.Errorf("Finish() rewrote streamed argument bytes: had %q, joined %q", fedArgs, args.String())
	}
	if !strings.HasPrefix(args.String(), `{"path":"qwen-proxy/READ`) {
		t.Errorf("original argument bytes lost: %q", args.String())
	}
}

// TestStreamTruncatedArgumentsSingleClosure verifies Finish() does not
// append the closure twice (feed-after-finish or repeated finish).
func TestStreamTruncatedArgumentsSingleClosure(t *testing.T) {
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"x"
	in := &AgentStreamInterceptor{}
	var args strings.Builder
	collect := func(parsed AgentParsedChunk) {
		for _, call := range parsed.ToolCalls {
			fn, _ := call["function"].(map[string]interface{})
			frag, _ := fn["arguments"].(string)
			args.WriteString(frag)
		}
	}
	collect(in.Feed(stream))
	collect(in.Finish())
	collect(in.Finish()) // second drain: nothing left to emit
	joined := args.String()
	want := `{"path":"x"}`
	if joined != want {
		t.Errorf("joined arguments = %q, want %q", joined, want)
	}
}

// TestAgentSnapToolName covers the glued-name production shapes plus the
// must-not-snap guards.
func TestAgentSnapToolName(t *testing.T) {
	known := []string{"read_file", "edit_file", "write_file", "grep", "find_path", "list_directory", "terminal", "delete_path", "move_path", "copy_path", "create_directory", "spawn_agent", "diagnostics", "skill", "fetch"}
	cases := []struct {
		in   string
		want string
	}{
		// Production shapes from log_stall.md (2026-09-06).
		{"read_filearguments", "read_file"},    // name glued with the next JSON key
		{"read_fileailing", "read_file"},       // name glued with trailing prose ("failing")
		{"grep_arguments_placeholder", "grep"}, // key-name hallucination glued on
		{"code.read_file", "read_file"},        // dotted prefix junk
		// Exact (and case-insensitive exact) passes through as the canonical spelling.
		{"read_file", "read_file"},
		{"READ_FILE", "read_file"},
		// Longest known prefix wins when several match.
		{"find_path_extra", "find_path"},
		// No plausible match: unchanged (the client error is then genuine).
		{"anywhere.", "anywhere."},
		{"", ""},
	}
	for _, tc := range cases {
		if got := agentSnapToolName(tc.in, known); got != tc.want {
			t.Errorf("agentSnapToolName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAgentSnapToolNameNoTools verifies an empty tool list disables snapping
// entirely — nothing to validate against, so names pass through untouched.
func TestAgentSnapToolNameNoTools(t *testing.T) {
	for _, name := range []string{"read_filearguments", "whatever"} {
		if got := agentSnapToolName(name, nil); got != name {
			t.Errorf("agentSnapToolName(%q, nil) = %q, want unchanged", name, got)
		}
	}
}

// TestAgentCollectToolNames verifies the request tool-list extraction used by
// the snapping layer, in both wrapped (function.name) and flat spellings.
func TestAgentCollectToolNames(t *testing.T) {
	tools := json.RawMessage(`[
		{"type":"function","function":{"name":"read_file","description":"read"}},
		{"type":"function","name":"flat_tool"}
	]`)
	got := agentCollectToolNames(tools)
	if len(got) != 2 {
		t.Fatalf("collected %d names, want 2: %v", len(got), got)
	}
	if got[0] != "read_file" || got[1] != "flat_tool" {
		t.Errorf("collected names = %v, want [read_file flat_tool]", got)
	}
	if n := agentCollectToolNames(nil); n != nil {
		t.Errorf("nil tools must yield nil, got %v", n)
	}
}
