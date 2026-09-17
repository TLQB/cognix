package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// MODEL_CONCURRENCY_LIMIT arrives as an inline SSE error in an HTTP 200 body
// ("Model is currently at capacity", observed live on glm-5.3/5.2 bursts,
// 2026-09-05). These tests lock in the typed error + extraction shapes so
// sendToZAIStream can wait-and-retry instead of surfacing a hard failure.

func TestExtractModelCapacityError(t *testing.T) {
	// data.error shape (observed live)
	j := parseJSONOrFatal(t, `{"data":{"error":{"code":"MODEL_CONCURRENCY_LIMIT","detail":"Model is currently at capacity. Please try again later or switch to another model."},"done":true}}`)
	e := extractModelCapacityError(j)
	if e == nil {
		t.Fatal("data.error: expected modelCapacityError")
	}
	if !strings.Contains(e.detail, "at capacity") {
		t.Errorf("detail = %q", e.detail)
	}

	// data.data.error shape (nested variant)
	j = parseJSONOrFatal(t, `{"data":{"data":{"error":{"error_code":"MODEL_CONCURRENCY_LIMIT","detail":"at capacity"}}}}`)
	e = extractModelCapacityError(j)
	if e == nil {
		t.Fatal("data.data.error: expected modelCapacityError")
	}

	// Top-level error shape (defensive)
	j = parseJSONOrFatal(t, `{"error":{"code":"MODEL_CONCURRENCY_LIMIT","detail":"x"}}`)
	if e := extractModelCapacityError(j); e == nil {
		t.Fatal("top-level error: expected modelCapacityError")
	}

	// Other errors must NOT match
	j = parseJSONOrFatal(t, `{"data":{"error":{"code":"FRONTEND_CAPTCHA_REQUIRED","detail":"captcha"}}}`)
	if e := extractModelCapacityError(j); e != nil {
		t.Errorf("captcha error must not be modelCapacityError, got %+v", e)
	}
	j = parseJSONOrFatal(t, `{"data":{"error":{"code":"SOME_OTHER_CODE","detail":"x"}}}`)
	if e := extractModelCapacityError(j); e != nil {
		t.Errorf("unrelated code must not match, got %+v", e)
	}
}

func TestModelCapacityErrorMessage(t *testing.T) {
	e := &modelCapacityError{detail: "Model is currently at capacity."}
	msg := e.Error()
	if !strings.Contains(msg, "MODEL_CONCURRENCY_LIMIT") || !strings.Contains(msg, "at capacity") {
		t.Errorf("message lost detail/code: %q", msg)
	}
}

func TestStreamSSEResponseModelCapacityGate(t *testing.T) {
	// An upstream 200 whose SSE body carries the capacity error (before any
	// content) must surface as the typed error with emitted=false, the
	// condition sendToZAIStream uses to decide a retry is safe.
	sse := "data: {\"data\":{\"error\":{\"code\":\"MODEL_CONCURRENCY_LIMIT\",\"detail\":\"Model is currently at capacity. Please try again later or switch to another model.\"},\"done\":true}}\n\n" +
		"data: [DONE]\n\n"

	ch := make(chan ZAIResult, 16)
	err := streamSSEResponse(strings.NewReader(sse), ch, "test-req-id")
	close(ch)
	mErr, ok := err.(*modelCapacityError)
	if !ok {
		t.Fatalf("expected *modelCapacityError, got %T: %v", err, err)
	}
	if mErr.emitted {
		t.Error("gate fired before content: emitted must be false")
	}
	for r := range ch {
		if r.Err != nil || r.Chunk != "" {
			t.Errorf("no result should have been emitted: %+v", r)
		}
	}
}

// TestStreamModelCapacityMidStreamGuard: if content was already streamed when
// the gate fires, emitted=true — sendToZAIStream must NOT retry (it would
// duplicate content on the client).
func TestStreamModelCapacityMidStreamGuard(t *testing.T) {
	sse := "data: {\"data\":{\"delta_content\":\"partial answer\"}}\n\n" +
		"data: {\"data\":{\"error\":{\"code\":\"MODEL_CONCURRENCY_LIMIT\",\"detail\":\"at capacity\"},\"done\":true}}\n\n" +
		"data: [DONE]\n\n"

	ch := make(chan ZAIResult, 16)
	err := streamSSEResponse(strings.NewReader(sse), ch, "test-req-id")
	close(ch)
	mErr, ok := err.(*modelCapacityError)
	if !ok {
		t.Fatalf("expected *modelCapacityError, got %T: %v", err, err)
	}
	if !mErr.emitted {
		t.Error("content was streamed before the gate: emitted must be true")
	}
}

func parseJSONOrFatal(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	j := map[string]interface{}{}
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	return j
}
