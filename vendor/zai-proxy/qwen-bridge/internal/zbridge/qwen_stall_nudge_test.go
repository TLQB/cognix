package zbridge

// Regression for the 2026-09-09 production stall storm: every stall retry
// re-sent a byte-identical request because agentStallNudge was appended only
// to `prompt`, which sendToQwenStream feeds into signature_prompt — a field
// Qwen does but never shows the model. The model answered the identical
// prompt the identical way (silence), attempt 2/2 stalled 7-8s later, and the
// turn ended blank. The nudge must be spliced into ClientMessagesRaw (the
// agent-folded messages the upstream actually reads).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAppendNudgeToClientMessages pins the splice across the message shapes
// the handler can pass through: folded single user message (string content),
// OpenAI multi-part content array, empty content, and malformed input.
func TestAppendNudgeToClientMessages(t *testing.T) {
	const nudge = "\n\n<system_nudge>act now</system_nudge>"

	t.Run("string content gets nudge appended", func(t *testing.T) {
		raw := json.RawMessage(`[{"role":"user","content":"task: fix the bug"}]`)
		out := appendNudgeToClientMessages(raw, nudge)
		var msgs []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(out, &msgs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Role != "user" {
			t.Fatalf("msg count/role changed: %+v", msgs)
		}
		if !strings.HasSuffix(msgs[0].Content, nudge) ||
			!strings.HasPrefix(msgs[0].Content, "task: fix the bug") {
			t.Fatalf("content mangled: %q", msgs[0].Content)
		}
	})

	t.Run("content array gets text part appended", func(t *testing.T) {
		raw := json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"hello"}]}]`)
		out := appendNudgeToClientMessages(raw, nudge)
		var msgs []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(out, &msgs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(msgs) != 1 || len(msgs[0].Content) != 2 {
			t.Fatalf("expected 2 parts, got %+v", msgs)
		}
		if msgs[0].Content[1].Type != "text" || msgs[0].Content[1].Text != nudge {
			t.Fatalf("nudge part wrong: %+v", msgs[0].Content[1])
		}
	})

	t.Run("missing content becomes the nudge", func(t *testing.T) {
		raw := json.RawMessage(`[{"role":"assistant"}]`)
		out := appendNudgeToClientMessages(raw, nudge)
		var msgs []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(out, &msgs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msgs[0].Role != "assistant" || msgs[0].Content != nudge {
			t.Fatalf("expected role preserved + nudge content, got %+v", msgs)
		}
	})

	t.Run("malformed input returned unchanged", func(t *testing.T) {
		raw := json.RawMessage(`{"not":"an array"}`)
		if out := appendNudgeToClientMessages(raw, nudge); string(out) != string(raw) {
			t.Fatalf("malformed input must pass through, got %s", out)
		}
	})

	t.Run("empty nudge / empty raw are no-ops", func(t *testing.T) {
		raw := json.RawMessage(`[{"role":"user","content":"x"}]`)
		if out := appendNudgeToClientMessages(raw, ""); string(out) != string(raw) {
			t.Fatal("empty nudge must be a no-op")
		}
		if out := appendNudgeToClientMessages(nil, nudge); out != nil {
			t.Fatal("nil raw must stay nil")
		}
	})
}

// TestStallRetryNudgeReachesUpstream drives the full streaming handler: the
// upstream answers the first request with a stall shape (announcement-only,
// no tool call), and answers the SECOND request with a tool call — but only
// if it actually received the nudge. The old code never sent it (the retry
// was byte-identical), so the turn ended blank after attempt 2/2. With the
// fix, the retry carries the nudge, the upstream sees it, and the client
// receives the tool call.
func TestStallRetryNudgeReachesUpstream(t *testing.T) {
	cfg := GetConfig()
	oldAgent, oldRetries, oldLevel := cfg.AgentMode, cfg.StallMaxRetries, cfg.Logging.Level
	cfg.AgentMode = true
	cfg.StallMaxRetries = 2
	cfg.Logging.Level = "info"
	t.Cleanup(func() {
		cfg.AgentMode, cfg.StallMaxRetries, cfg.Logging.Level = oldAgent, oldRetries, oldLevel
	})

	var hits int32
	var nudged atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/chats/new":
			writeJSONUpstream(w, 200, map[string]interface{}{"success": true, "data": map[string]string{"id": "chat-mock-1"}})
			return
		case "/api/v2/configs/":
			writeJSONUpstream(w, 200, map[string]string{})
			return
		case "/api/v2/models/":
			writeJSONUpstream(w, 200, map[string]interface{}{"data": []map[string]interface{}{
				{"id": "qwen3.6-plus", "name": "Qwen3.6-Plus"},
			}})
			return
		case "/api/v2/chat/completions":
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONUpstream(w, 400, map[string]string{"error": err.Error()})
				return
			}
			// The Qwen transport folds the conversation into one user
			// message; the nudge rides messages[0].content.
			sawNudge := false
			if msgs, ok := body["messages"].([]interface{}); ok {
				for _, mi := range msgs {
					if m, ok := mi.(map[string]interface{}); ok {
						if c, ok := m["content"].(string); ok && strings.Contains(c, "<system_nudge>") {
							sawNudge = true
						}
					}
				}
			}
			n := atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				// Stall shape: announcement-only, no tool call.
				if sawNudge {
					t.Errorf("first attempt must not carry the nudge")
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"I'll check the file now.\"},\"index\":0}]}\n\n")
			} else {
				// Retry: answer with a tool call ONLY if the nudge arrived.
				if !sawNudge {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"I'll check the file now.\"},\"index\":0}]}\n\n")
				} else {
					nudged.Store(true)
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"<<<TOOL_CALL>>>{\\\"name\\\":\\\"read_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"a.go\\\"}}<<<END_TOOL_CALL>>>\"},\"index\":0}]}\n\n")
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		default:
			writeJSONUpstream(w, 200, map[string]string{})
		}
	}))
	defer upstream.Close()

	oldBase := BASE_URL
	BASE_URL = upstream.URL
	defer func() { BASE_URL = oldBase }()
	defer OverrideSessionState("test-token", "test-user", true)()
	// sessionPool is nil in tests → AcquireStatelessSession mints a local
	// throwaway id; ReleaseStatelessSession gcSessions is a no-op against the
	// mock upstream. No pool reset needed.

	handler := NewHandler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{
			"model": "qwen3.6-plus",
			"stream": true,
			"tools": [{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],
			"messages": [{"role":"user","content":"read a.go and summarize"}]
		}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream never finished: %s", truncateForTest(body, 500))
	}
	if !nudged.Load() {
		t.Fatalf("retry never carried the nudge to the upstream (hits=%d). Body: %s", hits, truncateForTest(body, 500))
	}
	if !strings.Contains(body, "read_file") {
		t.Fatalf("client never received the recovered tool call: %s", truncateForTest(body, 800))
	}
	if hits < 2 {
		t.Fatalf("expected >=2 upstream attempts, got %d", hits)
	}
}

func writeJSONUpstream(w http.ResponseWriter, code int, v interface{}) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func truncateForTest(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
