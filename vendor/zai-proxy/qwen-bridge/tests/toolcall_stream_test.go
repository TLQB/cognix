// Blackbox end-to-end regression test for STREAMED TOOL CALLS through the
// REAL HTTP stack, driven via the exported zbridge.NewHandler() surface:
//
//   NewHandler -> authMiddleware -> chatCompletionsHandler -> sendToQwen ->
//   sendToQwenStream -> streamSSEResponse -> AgentStreamInterceptor ->
//   OpenAI tool_calls SSE chunks on the wire.
//
// The upstream SSE is chunked so the tool-call block arrives split across
// many events (worst case), and the test asserts the OpenAI streaming
// contract:
//
//   - the first tool_calls delta carries index/id/type/name (and may carry
//     the first arguments bytes, like OpenAI does when one chunk covers
//     both);
//   - subsequent deltas carry function.arguments fragments that reassemble
//     by index into the exact JSON object;
//   - the streamed call surfaces as MULTIPLE tool_calls chunks (header +
//     fragments), never one buffered blob emitted after the block closed;
//   - the stream closes with finish_reason "tool_calls" then [DONE];
//   - no marker/JSON bytes leak as content.

package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"qwen-proxy/internal/zbridge"
)

// mockToolCallUpstream streams the given assistant text as delta_content
// events split into `every`-byte pieces, one SSE event per piece.
func mockToolCallUpstream(text string, every int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			// Qwen SSE shape: choices[0].delta.content carries the answer
			// text; [DONE] terminates the stream.
			for i := 0; i < len(text); i += every {
				end := i + every
				if end > len(text) {
					end = len(text)
				}
				piece := text[i:end]
				esc, _ := json.Marshal(piece)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s},\"index\":0}]}\n\n", string(esc))
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		case "/api/v2/models/":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"qwen3.6-plus","name":"Qwen3.6-Plus"}]}`)
			return
		case "/api/v2/chats/new":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"success":true,"data":{"id":"chat-mock-1"}}`)
			return
		default:
			// /api/v1/chats/{id} (session GC delete), /api/v2/configs/ (token
			// ping), homepage scrape — answer generically.
			w.WriteHeader(200)
			fmt.Fprint(w, "true")
			return
		}
	}))
}

// openAIToolCallStream drives /v1/chat/completions with stream+tools against
// a mock upstream and returns every SSE data chunk the client received.
func openAIToolCallStream(t *testing.T, upstreamText string, every int) (chunks []string) {
	t.Helper()

	upstream := mockToolCallUpstream(upstreamText, every)
	defer upstream.Close()

	oldBase := zbridge.BASE_URL
	zbridge.BASE_URL = upstream.URL
	defer func() { zbridge.BASE_URL = oldBase }()

	defer zbridge.OverrideSessionState("test-token", "test-user", true)()

	cfg := zbridge.GetConfig()
	oldAgentMode := cfg.AgentMode
	cfg.AgentMode = true
	defer func() { cfg.AgentMode = oldAgentMode }()

	tools := `"tools":[{"type":"function","function":{"name":"calculate","description":"Perform arithmetic","parameters":{"type":"object","properties":{"operation":{"type":"string","enum":["add","subtract","multiply","divide"]},"a":{"type":"number"},"b":{"type":"number"}},"required":["operation","a","b"]}}}],"tool_choice":"auto"`
	body := fmt.Sprintf(`{"model":"qwen3.6-plus","stream":true,"messages":[{"role":"user","content":"What is 234 * 567? Calculate this using arithmetic."}],%s}`, tools)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	rec := httptest.NewRecorder()
	zbridge.NewHandler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		chunks = append(chunks, strings.TrimPrefix(line, "data: "))
	}
	return chunks
}

// parseToolCallChunk extracts the delta and finish_reason from one
// chat.completion.chunk JSON string. ok=false for chunks with no choices
// (e.g. the usage chunk).
func parseToolCallChunk(t *testing.T, data string) (delta map[string]interface{}, finishReason interface{}, ok bool) {
	t.Helper()
	var chunk struct {
		Choices []struct {
			Delta struct {
				Role      string                   `json:"role"`
				Content   interface{}              `json:"content"`
				ToolCalls []map[string]interface{} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason interface{} `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("chunk %q is not valid chat.completion.chunk JSON: %v", data, err)
	}
	if len(chunk.Choices) == 0 {
		return nil, nil, false // usage chunk
	}
	d := map[string]interface{}{}
	for _, tc := range chunk.Choices[0].Delta.ToolCalls {
		fn, _ := tc["function"].(map[string]interface{})
		d["toolCall"] = tc
		d["fn"] = fn
	}
	if chunk.Choices[0].Delta.ToolCalls == nil {
		d["content"] = chunk.Choices[0].Delta.Content
	}
	return d, chunk.Choices[0].FinishReason, true
}

func TestHTTPEndToEndStreamedToolCalls(t *testing.T) {
	// The goal's exact scenario: the model writes the tool-call block
	// incrementally; upstream events are tiny (worst-case chunking).
	upstreamText := "<<<TOOL_CALL>>>\n" +
		`{"name":"calculate","arguments": {"operation": "multiply", "a": 234, "b": 567}}` +
		"\n<<<END_TOOL_CALL>>>"

	for _, every := range []int{1, 7, 40} {
		chunks := openAIToolCallStream(t, upstreamText, every)
		if len(chunks) == 0 {
			t.Fatalf("every=%d: no SSE chunks received", every)
		}

		sawDone := false
		toolChunksTotal := 0 // tool_calls chunks on the wire
		argsByIndex := map[int]string{}
		names := map[int]string{}
		ids := map[int]string{}
		var finishReason string
		var contentLeak string

		for _, data := range chunks {
			if data == "[DONE]" {
				sawDone = true
				continue
			}
			delta, finish, ok := parseToolCallChunk(t, data)
			if !ok {
				continue
			}
			if tc, _ := delta["toolCall"].(map[string]interface{}); tc != nil {
				toolChunksTotal++
				idx, _ := tc["index"].(int)
				fn, _ := tc["function"].(map[string]interface{})
				if id, _ := tc["id"].(string); id != "" {
					ids[idx] = id
					names[idx], _ = fn["name"].(string)
				}
				if frag, _ := fn["arguments"].(string); frag != "" {
					if !utf8.ValidString(frag) {
						t.Fatalf("every=%d: invalid UTF-8 fragment %q", every, frag)
					}
					argsByIndex[idx] += frag
				}
			} else if c, _ := delta["content"].(string); c != "" {
				contentLeak += c
			}
			if f, _ := finish.(string); f != "" {
				finishReason = f
			}
		}

		// The streamed call must surface as MULTIPLE incremental tool_calls
		// chunks (header + argument fragments), never one buffered blob.
		if toolChunksTotal < 2 {
			t.Errorf("every=%d: only %d tool_calls chunks on the wire — buffered behaviour regressed", every, toolChunksTotal)
		}
		if names[0] != "calculate" {
			t.Errorf("every=%d: header name = %q, want calculate", every, names[0])
		}
		if !strings.HasPrefix(ids[0], "call_") {
			t.Errorf("every=%d: header id = %q, want call_*", every, ids[0])
		}
		wantArgs := `{"operation": "multiply", "a": 234, "b": 567}`
		if argsByIndex[0] != wantArgs {
			t.Errorf("every=%d: reassembled arguments = %q, want %q", every, argsByIndex[0], wantArgs)
		}
		if strings.Contains(contentLeak, "TOOL_CALL") || strings.Contains(contentLeak, "calculate") {
			t.Errorf("every=%d: block leaked as content: %q", every, contentLeak)
		}
		if finishReason != "tool_calls" {
			t.Errorf("every=%d: finish_reason = %q, want tool_calls", every, finishReason)
		}
		if !sawDone {
			t.Errorf("every=%d: stream missing [DONE]", every)
		}
	}
}

// TestHTTPEndToEndStreamedToolCallsLegacyVariant runs the same streamed
// tool-call scenario through the LEGACY shim (opt-in) to keep both variants
// on the OpenAI streaming contract: header delta + argument fragments.
func TestHTTPEndToEndStreamedToolCallsLegacyVariant(t *testing.T) {
	upstreamText := "<<<TOOL_CALL>>>\n" +
		`{"name":"calculate","arguments": {"operation": "multiply", "a": 234, "b": 567}}` +
		"\n<<<END_TOOL_CALL>>>"

	upstream := mockToolCallUpstream(upstreamText, 9)
	defer upstream.Close()

	oldBase := zbridge.BASE_URL
	zbridge.BASE_URL = upstream.URL
	defer func() { zbridge.BASE_URL = oldBase }()

	defer zbridge.OverrideSessionState("test-token", "test-user", true)()

	cfg := zbridge.GetConfig()
	oldAgentMode, oldVariant := cfg.AgentMode, cfg.AgentModeVariant
	cfg.AgentMode = true
	cfg.AgentModeVariant = "legacy"
	defer func() { cfg.AgentMode, cfg.AgentModeVariant = oldAgentMode, oldVariant }()

	tools := `"tools":[{"type":"function","function":{"name":"calculate","parameters":{"type":"object","properties":{"operation":{"type":"string"},"a":{"type":"number"},"b":{"type":"number"}}}}}]`
	body := fmt.Sprintf(`{"model":"qwen3.6-plus","stream":true,"messages":[{"role":"user","content":"234*567?"}],%s}`, tools)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	rec := httptest.NewRecorder()
	zbridge.NewHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	args := ""
	name := ""
	toolChunks := 0
	var finishReason string
	var contentLeak string
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		if !strings.Contains(data, `"tool_calls":[`) {
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content interface{} `json:"content"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
				if c, ok := chunk.Choices[0].Delta.Content.(string); ok {
					contentLeak += c
				}
				if chunk.Choices[0].FinishReason != "" {
					finishReason = chunk.Choices[0].FinishReason
				}
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		for _, c := range chunk.Choices[0].Delta.ToolCalls {
			toolChunks++
			if c.ID != "" && c.Function.Name != "" {
				name = c.Function.Name
			}
			args += c.Function.Arguments
		}
	}
	if toolChunks < 2 {
		t.Errorf("legacy variant: only %d tool_calls chunks — buffered behaviour regressed", toolChunks)
	}
	if name != "calculate" {
		t.Errorf("legacy variant: name = %q", name)
	}
	if args != `{"operation": "multiply", "a": 234, "b": 567}` {
		t.Errorf("legacy variant: reassembled arguments = %q", args)
	}
	if finishReason != "tool_calls" {
		t.Errorf("legacy variant: finish_reason = %q, want tool_calls", finishReason)
	}
	if strings.Contains(contentLeak, "TOOL_CALL") || strings.Contains(contentLeak, "calculate") {
		t.Errorf("legacy variant: block leaked as content: %q", contentLeak)
	}
}
