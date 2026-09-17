package zbridge

// Incremental tool-call streaming tests (port of commit 12dfa58 from the
// GLM-Free-API fork): the interceptor must emit tool-call deltas WHILE the
// model is still writing the block — an id/name header as soon as
// {"name": ..., "arguments": { has arrived, then live argument fragments —
// not one buffered blob after <<<END_TOOL_CALL>>>. Deltas mirror the OpenAI
// chunk contract: header deltas carry index/id/type/name, fragment deltas
// carry index and a function.arguments piece, and an SDK reassembles
// arguments by index.

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// splitRunes cuts s into pieces of n bytes (ASCII input assumed).
func splitRunes(s string, n int) []string {
	var out []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// feedChunks replays chunks through the streaming interceptor. It counts
// LOGICAL tool calls (deltas carrying an id open a call; id-less deltas are
// argument fragments of the open call) and returns the arguments accumulated
// across all fragments — mirroring how an OpenAI SDK reassembles the stream.
func feedChunks(t *testing.T, chunks []string, step int) (content string, toolCalls int, args string) {
	t.Helper()
	in := &AgentStreamInterceptor{}
	var b, acc strings.Builder
	collect := func(parsed AgentParsedChunk) {
		b.WriteString(parsed.Content)
		for _, call := range parsed.ToolCalls {
			if id, _ := call["id"].(string); id != "" {
				toolCalls++
			}
			fn := call["function"].(map[string]interface{})
			frag, _ := fn["arguments"].(string)
			acc.WriteString(frag)
		}
	}
	for i := 0; i < len(chunks); i += step {
		piece := ""
		for j := i; j < i+step && j < len(chunks); j++ {
			piece += chunks[j]
		}
		collect(in.Feed(piece))
	}
	collect(in.Finish())
	return b.String(), toolCalls, acc.String()
}

func TestStreamHeaderEmittedBeforeBlockCloses(t *testing.T) {
	in := &AgentStreamInterceptor{}
	// Opening marker + a name + an arguments object still in flight: the
	// client must already see the tool-call header SSE.
	parsed := in.Feed("Sure.\n<<<TOOL_CALL>>>\n{\"name\":\"calculate\",\"arguments\":{\"operation\":\"mult")
	if len(parsed.ToolCalls) == 0 {
		t.Fatalf("no tool-call header streamed before block close: %+v", parsed)
	}
	header := parsed.ToolCalls[0]
	if id, _ := header["id"].(string); id == "" || !strings.HasPrefix(id, "call_") {
		t.Errorf("header id = %v, want call_*", header["id"])
	}
	if header["type"] != "function" {
		t.Errorf("header type = %v, want function", header["type"])
	}
	fn, _ := header["function"].(map[string]interface{})
	if fn["name"] != "calculate" {
		t.Errorf("header name = %v, want calculate", fn["name"])
	}
	if header["index"] != 0 {
		t.Errorf("header index = %v, want 0", header["index"])
	}
	// Prose before the marker is released WITH the header delta (minus a
	// potential pseudo-call hold) — OpenAI-contract clients drop content
	// that arrives after tool-call deltas, so ordering is part of the
	// contract (issue #24). Block bytes must still never leak as content.
	if !strings.Contains(parsed.Content, "Sure.") {
		t.Errorf("pre-marker prose not released with the header: %q", parsed.Content)
	}
	if strings.Contains(parsed.Content, "name") || strings.Contains(parsed.Content, "TOOL_CALL") {
		t.Errorf("block body leaked as content: %q", parsed.Content)
	}
	// The fragment that already arrived rides the header delta (OpenAI does
	// the same when one chunk covers the name and the first argument bytes).
	headerArgs, _ := fn["arguments"].(string)
	if headerArgs != `{"operation":"mult` {
		t.Errorf("header arguments = %q, want the first streamed fragment", headerArgs)
	}

	// Rest of the arguments stream as id-less fragments referencing the
	// header's index, then the block closes.
	parsed = in.Feed("iply\",\"a\": 234, \"b\": 567}}\n<<<END_TOOL_CALL>>>")
	var args strings.Builder
	args.WriteString(headerArgs)
	for _, tc := range parsed.ToolCalls {
		if id, _ := tc["id"].(string); id != "" {
			t.Fatalf("unexpected second header mid-call: %+v", tc)
		}
		if tc["index"] != 0 {
			t.Errorf("fragment index = %v, want 0", tc["index"])
		}
		fn, _ := tc["function"].(map[string]interface{})
		args.WriteString(fn["arguments"].(string))
	}
	if args.String() != `{"operation":"multiply","a": 234, "b": 567}` {
		t.Errorf("reassembled arguments = %q", args.String())
	}
	if parsed.Content != "" {
		t.Errorf("content leaked: %q", parsed.Content)
	}

	// End of stream: nothing remains — the pre-marker prose already went out
	// with the header delta, and the trailing '>' run of the end marker can no
	// longer grow, so the block closed on the previous feed.
	fin := in.Finish()
	if fin.Content != "" {
		t.Errorf("finish left residue: %q", fin.Content)
	}
	if len(fin.ToolCalls) != 0 {
		t.Errorf("finish left residue: %+v", fin)
	}
	if strings.Contains(fin.Content, "TOOL_CALL") || strings.Contains(fin.Content, "calculate") {
		t.Errorf("block bytes leaked at finish: %q", fin.Content)
	}
}

func TestStreamArgumentsByteByByte(t *testing.T) {
	// Worst-case upstream: 1-byte chunks, including the markers and JSON
	// split at every byte boundary. The reassembled arguments must be exact
	// and every fragment must ride a consistent index.
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"search\",\"arguments\":{\"query\":\"what is 234*567\",\"limit\":3}}\n<<<END_TOOL_CALL>>>"
	in := &AgentStreamInterceptor{}
	argsByIndex := map[int]string{}
	headers := 0
	var name string
	accumulate := func(tcs []map[string]interface{}) {
		for _, tc := range tcs {
			idx, _ := tc["index"].(int)
			fn, _ := tc["function"].(map[string]interface{})
			frag, _ := fn["arguments"].(string)
			argsByIndex[idx] += frag
			if id, _ := tc["id"].(string); id != "" {
				headers++
				name, _ = fn["name"].(string)
			}
		}
	}
	for _, piece := range splitRunes(stream, 1) {
		accumulate(in.Feed(piece).ToolCalls)
	}
	accumulate(in.Finish().ToolCalls)
	if headers != 1 || name != "search" {
		t.Fatalf("headers = %d (%s), want 1 (search)", headers, name)
	}
	want := `{"query":"what is 234*567","limit":3}`
	if argsByIndex[0] != want {
		t.Errorf("reassembled arguments = %q, want %q", argsByIndex[0], want)
	}
}

// TestStreamContentPrecedesToolCallHeader asserts the OpenAI delta ordering
// contract (issue #24): every content delta emitted for a turn must reach the
// client BEFORE the first tool-call delta of that turn. OpenAI-contract
// clients (Vercel AI SDK) drop content that arrives after tool_call deltas,
// which truncated every prose tail at its last keep-window emission before
// the canonical block. The pre-marker prose must be released WITH the header
// chunk — never after it.
func TestStreamContentPrecedesToolCallHeader(t *testing.T) {
	cases := []struct {
		name string
		step int
		// prose ends in a tag-style pseudo-call that must stay held (stripped
		// at block close), while everything before it releases with the header.
		stream string
	}{
		{
			name:   "prose-tail",
			step:   3,
			stream: "I will inspect the tunnel now.\n<<<TOOL_CALL>>>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"live_ptz.py\"}}\n<<<END_TOOL_CALL>>>",
		},
		{
			name:   "pseudo-call-tail",
			step:   2,
			stream: "Checking the config.<read_file>{\"path\":\"main.go\"}</read_file>\n<<<TOOL_CALL>>>\n{\"name\":\"list_dir\",\"arguments\":{\"path\":\"/tmp\"}}\n<<<END_TOOL_CALL>>>",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			in := &AgentStreamInterceptor{}
			var content strings.Builder
			var contentAfterToolCall bool
			var sawToolCall bool
			note := func(parsed AgentParsedChunk) {
				if parsed.Content != "" {
					if sawToolCall {
						contentAfterToolCall = true
					}
					content.WriteString(parsed.Content)
				}
				if len(parsed.ToolCalls) > 0 {
					sawToolCall = true
				}
			}
			for _, piece := range splitRunes(tc.stream, tc.step) {
				note(in.Feed(piece))
			}
			note(in.Finish())
			if contentAfterToolCall {
				t.Errorf("content delta emitted after a tool-call delta — clients drop it (ordering contract)")
			}
			if !strings.Contains(content.String(), "I will inspect") && !strings.Contains(content.String(), "Checking the config") {
				t.Errorf("pre-marker prose lost: %q", content.String())
			}
		})
	}
}

func TestStreamArgumentFragmentsAreValidUTF8(t *testing.T) {
	// Argument string values carrying multi-byte runes, byte-cut mid-rune
	// by the chunker (issue #23): no fragment may surface half a rune.
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"translate\",\"arguments\":{\"text\":\"你好，世界 🙂 安心\",\"to\":\"en\"}}\n<<<END_TOOL_CALL>>>"
	in := &AgentStreamInterceptor{}
	var args strings.Builder
	for _, piece := range splitRunes(stream, 3) { // lands inside 3/4-byte runes
		parsed := in.Feed(piece)
		for _, tc := range parsed.ToolCalls {
			fn, _ := tc["function"].(map[string]interface{})
			frag, _ := fn["arguments"].(string)
			if !utf8.ValidString(frag) {
				t.Fatalf("argument fragment is invalid UTF-8 (client garble): %q", frag)
			}
			args.WriteString(frag)
		}
	}
	for _, tc := range in.Finish().ToolCalls {
		fn, _ := tc["function"].(map[string]interface{})
		args.WriteString(fn["arguments"].(string))
	}
	want := `{"text":"你好，世界 🙂 安心","to":"en"}`
	if args.String() != want {
		t.Errorf("reassembled arguments = %q, want %q", args.String(), want)
	}
}

func TestStreamMarkerTextInsideStringArgument(t *testing.T) {
	// A string argument whose VALUE contains the closing-marker words: the
	// scanner tracks JSON strings, so the block still streams and closes at
	// the real marker instead of truncating at the quoted one.
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"write\",\"arguments\":{\"content\":\"template: <<<END_TOOL_CALL>>> done\",\"path\":\"/tmp/x\"}}\n<<<END_TOOL_CALL>>>"
	content, calls, args := feedChunks(t, splitRunes(stream, 4), 1)
	if calls != 1 {
		t.Fatalf("%d tool calls, want 1 (content=%q)", calls, content)
	}
	if !strings.Contains(args, "<<<END_TOOL_CALL>>> done") {
		t.Errorf("arguments truncated at quoted marker: %q", args)
	}
	if !strings.Contains(args, "/tmp/x") {
		t.Errorf("arguments missing keys after quoted marker: %q", args)
	}
}

func TestStreamStringArgumentsFallsBackToCompleteCall(t *testing.T) {
	// arguments as a JSON-encoded STRING is not streamable: the interceptor
	// must buffer the block and emit one complete, unescaped call.
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"uname -a\\\"}\"}\n<<<END_TOOL_CALL>>>"
	content, calls, args := feedChunks(t, splitRunes(stream, 5), 1)
	if calls != 1 {
		t.Fatalf("%d tool calls, want 1 (content=%q)", calls, content)
	}
	if args != `{"command":"uname -a"}` {
		t.Errorf("arguments = %q, want unwrapped JSON object", args)
	}
}

func TestStreamFlatPayloadFallsBackToCompleteCall(t *testing.T) {
	// Flat payload ({"tool": ..., <parameters>}): no arguments key is ever
	// found, so the buffered path folds the stray keys into one complete
	// call — same result as the finished-text parser.
	stream := "<<<TOOL_CALL>>>\n{\"tool\":\"bash\",\"command\":\"uname -a\",\"timeout\":10}\n<<<END_TOOL_CALL>>>"
	content, calls, args := feedChunks(t, splitRunes(stream, 3), 1)
	if calls != 1 {
		t.Fatalf("%d tool calls, want 1 (content=%q)", calls, content)
	}
	if !strings.Contains(args, `"command":"uname -a"`) || !strings.Contains(args, `"timeout":10`) {
		t.Errorf("arguments = %q, want flat keys folded in", args)
	}
}

func TestStreamAlternateKeySpellingsStillStream(t *testing.T) {
	// Tolerated spellings stream too: the name resolves from "tool" and the
	// arguments object from "parameters".
	stream := "<<TOOL_CALL>>>\n{\"tool\":\"lookup\",\"parameters\":{\"city\":\"Berlin\"}}\n<<<END_TOOL_CALL>>>"
	content, calls, args := feedChunks(t, splitRunes(stream, 3), 1)
	if calls != 1 {
		t.Fatalf("%d tool calls, want 1 (content=%q)", calls, content)
	}
	if args != `{"city":"Berlin"}` {
		t.Errorf("arguments = %q, want streamed parameters object", args)
	}
	if strings.Contains(content, "TOOL_CALL") || strings.TrimSpace(content) != "" {
		t.Errorf("markers or junk leaked as content: %q", content)
	}
}

func TestStreamUnbalancedJSONClosesAtMarker(t *testing.T) {
	// The model bails mid-JSON and closes the block anyway: the streamed
	// call must close at the marker (no marker bytes inside the arguments)
	// and no marker text may leak as content.
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"bash\",\"arguments\":{\"command\":\"echo\"\n<<<END_TOOL_CALL>>>"
	content, calls, args := feedChunks(t, splitRunes(stream, 4), 1)
	if calls != 1 {
		t.Fatalf("%d tool calls, want 1 (content=%q)", calls, content)
	}
	if strings.Contains(args, "TOOL_CALL") || strings.Contains(args, "<") {
		t.Errorf("end-marker bytes leaked into streamed arguments: %q", args)
	}
	if strings.Contains(content, "TOOL_CALL") {
		t.Errorf("marker leaked as content: %q", content)
	}
}

func TestStreamTwoCallsWithProseBetween(t *testing.T) {
	stream := "<<<TOOL_CALL>>>\n{\"name\":\"a\",\"arguments\":{\"x\":1}}\n<<<END_TOOL_CALL>>>\nfirst done\n<<<TOOL_CALL>>>\n{\"name\":\"b\",\"arguments\":{\"y\":2}}\n<<<END_TOOL_CALL>>>\ntrailing"
	in := &AgentStreamInterceptor{}
	var content strings.Builder
	argsByIndex := map[int]string{}
	headers := 0
	var names []string
	collect := func(parsed AgentParsedChunk) {
		content.WriteString(parsed.Content)
		for _, tc := range parsed.ToolCalls {
			idx, _ := tc["index"].(int)
			fn, _ := tc["function"].(map[string]interface{})
			argsByIndex[idx] += fn["arguments"].(string)
			if id, _ := tc["id"].(string); id != "" {
				headers++
				names = append(names, fn["name"].(string))
			}
		}
	}
	for _, piece := range splitRunes(stream, 6) {
		collect(in.Feed(piece))
	}
	collect(in.Finish())

	if headers != 2 || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("headers = %d names = %v, want 2 [a b]", headers, names)
	}
	if argsByIndex[0] != `{"x":1}` || argsByIndex[1] != `{"y":2}` {
		t.Errorf("arguments by index = %v", argsByIndex)
	}
	if !strings.Contains(content.String(), "first done") || !strings.Contains(content.String(), "trailing") {
		t.Errorf("prose between/after calls lost: %q", content.String())
	}
	if strings.Contains(content.String(), "TOOL_CALL") || strings.Contains(content.String(), `"x"`) {
		t.Errorf("block bytes leaked as content: %q", content.String())
	}
}

// TestAgentEndMarkerPrefixMatrix pins the partial-marker classifier used by
// the streaming arguments scanner: "maybe" verdicts hold bytes back (a
// marker split across upstream chunks), "complete" closes the call, "no"
// streams the byte as ordinary (malformed) JSON. Tolerance is 1..6 brackets
// per side in this codebase (the fork base allowed 2..4): 5-7 leading
// brackets are Maybe/No accordingly.
func TestAgentEndMarkerPrefixMatrix(t *testing.T) {
	cases := []struct {
		in    string
		state int
		mLen  int
	}{
		{"<<<END_TOOL_CALL>>>", agentPrefixComplete, 19}, // canonical
		{"<<<<END_TOOL_CALL>>>>", agentPrefixComplete, 21},
		{"<<END_TOOL_CALL>>", agentPrefixComplete, 17},  // short tolerated
		{"<<<END_TOOL_CALL>>", agentPrefixComplete, 18}, // asymmetric 3/2 — tolerated
		{"<END_TOOL_CALL>", agentPrefixComplete, 15},    // single bracket — tolerated
		{"<<<END_TOOL_CALL>", agentPrefixComplete, 17},  // 1 trailing bracket — within 1..6
		{"<<<END_TOOL_CAL", agentPrefixMaybe, 0},        // word still arriving
		{"<<<END_TOOL", agentPrefixMaybe, 0},
		{"<<<END_", agentPrefixMaybe, 0},
		{"<<<", agentPrefixMaybe, 0}, // bare bracket run, word may still start
		{"<", agentPrefixMaybe, 0},
		{"<<<<", agentPrefixMaybe, 0},                      // 4 brackets so far — within 1..6
		{"<<<<<<", agentPrefixMaybe, 0},                    // 6 brackets — max tolerated
		{"<<<<<<<", agentPrefixNo, 0},                      // 7 brackets: never tolerated
		{"<<<<<END_TOOL_CALL>>>", agentPrefixComplete, 21}, // 5 lead/3 trail — within 1..6
		{"<abc", agentPrefixNo, 0},
		{"<<x", agentPrefixNo, 0},
		{"<<<END_TOOL_CALL>x", agentPrefixComplete, 17}, // marker completes; 'x' trails unconsumed
		{"<<<NOT_TOOL_CALL>>>", agentPrefixNo, 0},
		{"<<<END_TOOL_CALLL>>>", agentPrefixNo, 0},
	}
	for _, c := range cases {
		st, ml := agentEndMarkerPrefix(c.in)
		if st != c.state || ml != c.mLen {
			t.Errorf("agentEndMarkerPrefix(%q) = (%d,%d), want (%d,%d)", c.in, st, ml, c.state, c.mLen)
		}
	}
}

// TestStreamChunkSweepInvariants sweeps EVERY chunk size and randomized
// split patterns through the interceptor and asserts the streaming
// invariants hold at every boundary:
//  1. exactly one header delta per block, with a stable id and name;
//  2. fragments reassemble, by index, into the exact arguments JSON;
//  3. every fragment is valid UTF-8 (issue #23);
//  4. no marker/JSON bytes leak as content.
func TestStreamChunkSweepInvariants(t *testing.T) {
	streams := []string{
		"<<<TOOL_CALL>>>\n{\"name\":\"calculate\",\"arguments\":{\"a\": 234, \"b\": 567, \"operation\": \"multiply\"}}\n<<<END_TOOL_CALL>>>",
		"Reasoning first.\n<<<TOOL_CALL>>>\n{\"name\":\"translate\",\"arguments\":{\"text\":\"你好，世界\",\"to\":\"en\"}}\n<<<END_TOOL_CALL>>>\nafter",
		"<<TOOL_CALL>>>\n{\"tool\":\"bash\",\"parameters\":{\"command\":\"uname -m\",\"timeout\":10}}\n<<<END_TOOL_CALL>>>",
		"<<<TOOL_CALL>>>\n{\"name\":\"echo\",\"arguments\":{}}\n<<<END_TOOL_CALL>>>",
	}
	wantArgs := []string{
		`{"a": 234, "b": 567, "operation": "multiply"}`,
		`{"text":"你好，世界","to":"en"}`,
		`{"command":"uname -m","timeout":10}`,
		`{}`,
	}

	for si, stream := range streams {
		for size := 1; size <= len(stream); size++ {
			chunks := splitRunes(stream, size)
			in := &AgentStreamInterceptor{}
			var content strings.Builder
			argsByIndex := map[int]string{}
			headers := 0
			var name, id string
			collect := func(tcs []map[string]interface{}) {
				for _, tc := range tcs {
					idx, _ := tc["index"].(int)
					fn, _ := tc["function"].(map[string]interface{})
					frag, _ := fn["arguments"].(string)
					if !utf8.ValidString(frag) {
						t.Fatalf("stream %d size %d: invalid UTF-8 fragment %q", si, size, frag)
					}
					argsByIndex[idx] += frag
					if tcid, _ := tc["id"].(string); tcid != "" {
						headers++
						id = tcid
						name, _ = fn["name"].(string)
					}
				}
			}
			for _, piece := range chunks {
				parsed := in.Feed(piece)
				content.WriteString(parsed.Content)
				collect(parsed.ToolCalls)
			}
			f := in.Finish()
			content.WriteString(f.Content)
			collect(f.ToolCalls)

			if headers != 1 {
				t.Fatalf("stream %d size %d: %d header deltas, want 1", si, size, headers)
			}
			if name == "" || id == "" {
				t.Fatalf("stream %d size %d: header lost name/id", si, size)
			}
			if got := argsByIndex[0]; got != wantArgs[si] {
				t.Fatalf("stream %d size %d: reassembled %q, want %q", si, size, got, wantArgs[si])
			}
			out := content.String()
			if strings.Contains(out, "TOOL_CALL") {
				t.Fatalf("stream %d size %d: marker leaked: %q", si, size, out)
			}
		}
	}

	// Randomized chunk boundaries (deterministic seed).
	rnd := rand.New(rand.NewSource(234567))
	for trial := 0; trial < 200; trial++ {
		stream := streams[trial%len(streams)]
		want := wantArgs[trial%len(wantArgs)]
		in := &AgentStreamInterceptor{}
		args := ""
		headers := 0
		for pos := 0; pos < len(stream); {
			n := 1 + rnd.Intn(12)
			if pos+n > len(stream) {
				n = len(stream) - pos
			}
			parsed := in.Feed(stream[pos : pos+n])
			for _, tc := range parsed.ToolCalls {
				fn, _ := tc["function"].(map[string]interface{})
				frag, _ := fn["arguments"].(string)
				if !utf8.ValidString(frag) {
					t.Fatalf("trial %d: invalid UTF-8 fragment %q", trial, frag)
				}
				args += frag
				if id, _ := tc["id"].(string); id != "" {
					headers++
				}
			}
			pos += n
		}
		f := in.Finish()
		for _, tc := range f.ToolCalls {
			fn, _ := tc["function"].(map[string]interface{})
			args += fn["arguments"].(string)
			if id, _ := tc["id"].(string); id != "" {
				headers++
			}
		}
		if headers != 1 {
			t.Fatalf("trial %d: %d headers, want 1", trial, headers)
		}
		if args != want {
			t.Fatalf("trial %d: reassembled %q, want %q", trial, args, want)
		}
	}
}

// TestRearmAgentInterceptorPreservesCallIndex pins the rewind contract: a
// re-armed interceptor continues the client-visible index sequence so a
// re-streamed call after an edit_content rewind cannot collide inside SDK
// accumulation (issue #23).
func TestRearmAgentInterceptorPreservesCallIndex(t *testing.T) {
	withAgentModern(t)
	old := newAgentInterceptor(nil).(*modernAgentInterceptor)
	// Stream one full call through the old interceptor.
	_, calls := old.feed("<<<TOOL_CALL>>>\n{\"name\":\"a\",\"arguments\":{\"x\":1}}\n<<<END_TOOL_CALL>>>")
	if len(calls) == 0 {
		t.Fatal("no call streamed")
	}
	fresh := rearmAgentInterceptor(old).(*modernAgentInterceptor)
	if fresh.in.callIndex != old.in.callIndex {
		t.Errorf("callIndex after rearm = %d, want %d", fresh.in.callIndex, old.in.callIndex)
	}
	_, replay := fresh.feed("<<<TOOL_CALL>>>\n{\"name\":\"a\",\"arguments\":{\"x\":1}}\n<<<END_TOOL_CALL>>>")
	if len(replay) == 0 {
		t.Fatal("no replay call streamed")
	}
	for _, tc := range replay {
		if idx, _ := tc["index"].(int); idx != old.in.callIndex {
			t.Errorf("replayed call index = %v, want %d (no collision)", tc["index"], old.in.callIndex)
		}
	}
}
