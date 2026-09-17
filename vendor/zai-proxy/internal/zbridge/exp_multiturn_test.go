package zbridge

// SPIKE #2: multi-turn native upstream.
//
// Question: does Z.AI's /api/v2/chat/completions accept a NATIVE multi-turn
// messages array (user/assistant/tool alternating) instead of the current
// agent shim, which folds the whole conversation into ONE user message via
// buildAgentPrompt + wrapAgentPromptAsMessages?
//
// If native multi-turn works: the prompt gets smaller (no re-serialization
// of full history each turn), tool results arrive as first-class tool-role
// messages, and the model's own assistant turns stay distinct — likely
// fewer stalls ("no tool call and no answer") because the model sees a
// real conversation, not a wall of folded XML.
//
// If it fails (400/broken responses): the fold is mandatory and we close
// the spike with that conclusion.
//
// Run: EXP_MULTITURN=1 go test ./internal/zbridge/ -run TestExpMultiturnNative -v -count=1

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// expMultiturnBody builds the upstream request body with a CONTROLLABLE
// messages array (the whole point of this spike) — everything else matches
// the production payload from sendToZAIStream: model, stream:false for easy
// assertions, features, and a captcha param minted on the live Q.
func expMultiturnBody(messages []map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"model":            "glm-4.7",
		"messages":         messages,
		"signature_prompt": "",
		"stream":           false,
		"features":         map[string]interface{}{"flags": []interface{}{}, "image_generation": false},
	}
}

// expJoinAnswerChunks reconstructs the model's answer text from the SSE
// stream snippet: delta_content pieces plus edit_content rewrites (Z.AI
// streams a partial token then edits it into the full word).
func expJoinAnswerChunks(out string) string {
	var b strings.Builder
	for _, part := range strings.Split(out, "|") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, "\"type\":\"chat:completion\"") {
			continue
		}
		for _, field := range []string{"delta_content", "edit_content"} {
			key := `"` + field + `":"`
			i := strings.Index(part, key)
			if i < 0 {
				continue
			}
			rest := part[i+len(key):]
			j := strings.Index(rest, `"`)
			if j > 0 {
				b.WriteString(rest[:j])
			}
		}
	}
	return b.String()
}

// expTestClient mirrors the utls/HTTP1.1 client from TestExpCaptchaBypass.
func expTestClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext:      dialUTLSAlb,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			ForceAttemptHTTP2:   false,
		},
		Timeout: 90 * time.Second,
	}
}

func TestExpMultiturnNative(t *testing.T) {
	if os.Getenv("EXP_MULTITURN") == "" {
		t.Skip("set EXP_MULTITURN=1 to run this experimental live probe")
	}
	verbose = true
	dbPath = "../../tokens.sqlite"
	if err := initDB(); err != nil {
		t.Fatalf("initDB: %v", err)
	}
	if config.ZaiToken == "" {
		t.Fatal("ZAI_TOKEN not set")
	}

	// expCaptchaChat is reused wholesale (utls dial, XFF rotation, WAF 405
	// retry) — it only needs a body map with a `messages` field we control
	// and the signature_prompt string. Each probe below swaps in a
	// different messages array shape.

	// Captcha params are SINGLE-USE (probe A consumes its param; B and C
	// failed with FRONTEND_CAPTCHA_REQUIRED when reusing it). Mint a fresh
	// one per probe.
	freshParam := func(t *testing.T) string {
		t.Helper()
		p, err := getCaptchaVerifyParam()
		if err != nil {
			t.Fatalf("captcha param: %v", err)
		}
		return p
	}

	// ── Probe A: minimal native multi-turn (user → assistant → user) ────
	// The current shim folds this into one user message. If Z.AI
	// accepts the alternating roles, we get a normal 200 stream.
	t.Run("A_minimal_multiturn", func(t *testing.T) {
		body := expMultiturnBody([]map[string]interface{}{
			{"role": "user", "content": "Say exactly: ALPHA"},
			{"role": "assistant", "content": "ALPHA"},
			{"role": "user", "content": "Now say exactly: BRAVO"},
		})
		body["captcha_verify_param"] = freshParam(t)
		code, out := expCaptchaChat(t, expTestClient(), body, "Now say exactly: BRAVO")
		t.Logf("A: HTTP %d | %s", code, out)
		if code != 200 {
			t.Errorf("A: expected 200, got %d — native multi-turn rejected?", code)
		}
		// Z.AI streams answer text via delta_content and may rewrite a chunk
		// via edit_content (delta "B" + edit "RAVO" = "BRAVO"), and the body
		// is truncated at 600 chars — so assert on the join of both fields
		// instead of one contiguous string in `out`.
		joined := expJoinAnswerChunks(out)
		if !strings.Contains(joined, "BRAVO") {
			t.Errorf("A: joined answer missing BRAVO: %q", joined)
		}
	})

	// ── Probe B: multi-turn WITH a tool role message ─────────────────────
	// The decisive probe: agent mode needs tool results as separate
	// messages. Z.AI's own web client never sends tool-role messages (it
	// folds them too), so this is the shape we actually want upstream to
	// accept for the shim to be removable.
	t.Run("B_tool_role", func(t *testing.T) {
		body := expMultiturnBody([]map[string]interface{}{
			{"role": "user", "content": "What is 2+2? Use the calculator tool."},
			{"role": "assistant", "content": "", "tool_calls": []map[string]interface{}{
				{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "calculator",
						"arguments": "{\"expr\": \"2+2\"}",
					},
				},
			}},
			{"role": "tool", "content": "4", "tool_call_id": "call_1"},
		})
		body["captcha_verify_param"] = freshParam(t)
		code, out := expCaptchaChat(t, expTestClient(), body, "What is 2+2? The calculator returned 4. State the answer.")
		t.Logf("B: HTTP %d | %s", code, out)
		if code != 200 {
			t.Errorf("B: expected 200, got %d — tool role rejected?", code)
		}
	})

	// ── Probe C: control — single user message (current shim shape) ──────
	// Must succeed; if it fails the infra is broken and A/B are void.
	t.Run("C_control_single", func(t *testing.T) {
		body := expMultiturnBody([]map[string]interface{}{
			{"role": "user", "content": "Say exactly: CHARLIE"},
		})
		body["captcha_verify_param"] = freshParam(t)
		code, out := expCaptchaChat(t, expTestClient(), body, "Say exactly: CHARLIE")
		t.Logf("C: HTTP %d | %s", code, out)
		if code != 200 {
			t.Errorf("C: control failed with %d — infra broken, A/B void", code)
		}
		if !strings.Contains(expJoinAnswerChunks(out), "CHARLIE") {
			t.Errorf("C: joined answer missing CHARLIE: %q", expJoinAnswerChunks(out))
		}
	})
}
