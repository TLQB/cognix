package zbridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for the 2026-09-05 e2e outage: the Aliyun WAF blocklisted the
// five addresses at the head of the XFF pool (1.0.0.1 … 149.112.112.112),
// and the old single-405-rotation budget meant every request that started on
// a blocked head address failed client-visibly — the good addresses further
// down the pool were never reached. sendToZAIStream must now sweep the whole
// pool on consecutive 405s instead of giving up after one rotation.

// TestWAF405SweepsPoolUntilPass pins the sweep: an upstream that 405s the
// first N addresses must still succeed once rotation reaches a good one,
// within a single sendToZAIStream call, and each 405 must quarantine the
// address so the NEXT request starts on the good address directly.
func TestWAF405SweepsPoolUntilPass(t *testing.T) {
	resetXffStateForTest(t)
	cfg := GetConfig()
	oldAgent := cfg.AgentMode
	cfg.AgentMode = true
	t.Cleanup(func() { cfg.AgentMode = oldAgent })

	// Fresh pool of 6 addresses; the upstream 405s (WAF page) while the
	// request's XFF is one of the first 3, then streams a normal answer.
	pool := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"}
	setXffPoolForTest(t, pool)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/chat/completions" {
			w.WriteHeader(200)
			fmt.Fprint(w, "true")
			return
		}
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" && (xff == pool[0] || xff == pool[1] || xff == pool[2]) {
			w.WriteHeader(405)
			fmt.Fprint(w, "<html>waf block</html>")
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"data\":{\"delta_content\":\"ok\"}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	oldBase := BASE_URL
	BASE_URL = upstream.URL
	defer func() { BASE_URL = oldBase }()
	defer OverrideSessionState("test-token", "test-user", true)()
	// One captcha param per upstream attempt (params are single-use):
	// 3 blocked rotations + 1 success + slack for the second request.
	for i := 0; i < 8; i++ {
		SeedCaptchaParam("test-captcha-param")
	}

	ch := make(chan ZAIResult, 64)
	err := sendToZAIStream("test prompt", struct {
		Model, ChatID     string
		FeaturesMap       map[string]interface{}
		Messages          []Message
		ClientMessagesRaw json.RawMessage
		RequestID         string
	}{Model: "glm-4.7", ChatID: "test-chat"}, ch)
	close(ch) // sendToZAIStream only closes via sendToZAI's goroutine wrapper
	if err != nil {
		t.Fatalf("sendToZAIStream: %v", err)
	}
	var content string
	for r := range ch {
		if r.Err != nil {
			t.Fatalf("stream result error: %v", r.Err)
		}
		content += r.Chunk
	}
	if content != "ok" {
		t.Fatalf("content = %q, want ok", content)
	}
	if hits != 1 {
		t.Fatalf("upstream good-address hits = %d, want 1", hits)
	}

	// The three 405-ed addresses must now be quarantined: a fresh request
	// starts directly on a good address with zero upstream 405s.
	var blockedSeen int32
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/chat/completions" {
			w.WriteHeader(200)
			fmt.Fprint(w, "true")
			return
		}
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" && (xff == pool[0] || xff == pool[1] || xff == pool[2]) {
			atomic.AddInt32(&blockedSeen, 1)
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"data\":{\"delta_content\":\"ok2\"}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream2.Close()
	BASE_URL = upstream2.URL

	ch2 := make(chan ZAIResult, 64)
	err = sendToZAIStream("test prompt 2", struct {
		Model, ChatID     string
		FeaturesMap       map[string]interface{}
		Messages          []Message
		ClientMessagesRaw json.RawMessage
		RequestID         string
	}{Model: "glm-4.7", ChatID: "test-chat"}, ch2)
	close(ch2)
	if err != nil {
		t.Fatalf("second sendToZAIStream: %v", err)
	}
	content2 := ""
	for r := range ch2 {
		if r.Err != nil {
			t.Fatalf("second stream error: %v", r.Err)
		}
		content2 += r.Chunk
	}
	if content2 != "ok2" {
		t.Fatalf("second content = %q, want ok2", content2)
	}
	if blockedSeen != 0 {
		t.Fatalf("quarantine failed: second request hit a blocked address %d times", blockedSeen)
	}
}

// TestWAF405ExhaustedPoolSurfacesError: when every address in the pool 405s,
// the sweep terminates (bounded) and surfaces a clear error instead of
// looping forever.
func TestWAF405ExhaustedPoolSurfacesError(t *testing.T) {
	resetXffStateForTest(t)
	cfg := GetConfig()
	oldAgent := cfg.AgentMode
	cfg.AgentMode = true
	t.Cleanup(func() { cfg.AgentMode = oldAgent })

	pool := []string{"10.1.0.1", "10.1.0.2", "10.1.0.3"}
	setXffPoolForTest(t, pool)

	var requests int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/chat/completions" {
			w.WriteHeader(200)
			fmt.Fprint(w, "true")
			return
		}
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(405)
		fmt.Fprint(w, "<html>waf block</html>")
	}))
	defer upstream.Close()

	oldBase := BASE_URL
	BASE_URL = upstream.URL
	defer func() { BASE_URL = oldBase }()
	defer OverrideSessionState("test-token", "test-user", true)()
	// One captcha param per pool address (params are single-use) + slack.
	for i := 0; i < len(pool)+2; i++ {
		SeedCaptchaParam("test-captcha-param")
	}

	ch2 := make(chan ZAIResult, 64)
	err := sendToZAIStream("test prompt", struct {
		Model, ChatID     string
		FeaturesMap       map[string]interface{}
		Messages          []Message
		ClientMessagesRaw json.RawMessage
		RequestID         string
	}{Model: "glm-4.7", ChatID: "test-chat"}, ch2)
	if err == nil {
		t.Fatal("expected an error when the whole pool is WAF-blocked")
	}
	if !strings.Contains(err.Error(), "405") {
		t.Fatalf("error should mention 405, got: %v", err)
	}
	// Bounded: the sweep makes exactly one request per pool address on the
	// initial pass, then (because markXffIPBad clears the bad-set once the
	// whole pool is quarantined) one final re-try of the first address
	// before giving up — never an infinite loop.
	want := int32(len(pool)) + 1
	if got := atomic.LoadInt32(&requests); got != want {
		t.Fatalf("upstream requests = %d, want %d (bounded sweep)", got, want)
	}
}

// resetXffStateForTest clears the rotation/pool/bad state so a test is
// independent of whatever earlier tests or the live server left behind.
// It forces the POOL strategy for the test's duration — the sweep and
// quarantine machinery under test only exists there; the default random
// strategy (see defaultXffCIDRBanks) has no rotation state at all.
func resetXffStateForTest(t *testing.T) {
	t.Helper()
	t.Setenv("ZAI_XFF_STRATEGY", "pool")
	t.Setenv("ZAI_XFF_IP", "")
	t.Setenv("ZAI_XFF_IPS", "")
	clearXff := func() {
		xffBadMu.Lock()
		xffBad = map[string]time.Time{}
		xffBadMu.Unlock()
	}
	xffMu.Lock()
	xffIndex = 0
	xffMu.Unlock()
	clearXff()
	// Restore a clean default pool + bad-set after the test so WAF-sweep
	// quarantine does not leak into other tests (e.g. TestXffRotation).
	t.Cleanup(func() {
		clearXff()
		xffMu.Lock()
		xffPoolVal = defaultXffIPPool
		xffIndex = 0
		xffMu.Unlock()
	})
}

// setXffPoolForTest installs a deterministic pool (as ZAI_XFF_IPS would).
func setXffPoolForTest(t *testing.T, pool []string) {
	t.Helper()
	xffPoolOnce.Do(func() {}) // pin the once so later loads cannot overwrite
	xffMu.Lock()
	xffPoolVal = pool
	xffMu.Unlock()
}
